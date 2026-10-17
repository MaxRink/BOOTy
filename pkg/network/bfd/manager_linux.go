//go:build linux

package bfd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// UDP ports of RFC 5881 (single-hop) and RFC 5883 (multihop), and the
// source port range of RFC 5881 section 4.
const (
	portSingleHop = 3784
	portMultihop  = 4784
	srcPortMin    = 49152
	srcPortMax    = 65535
	// minMultihopTTL is the FRR bfdd default "minimum-ttl 254".
	minMultihopTTL = 254
)

// ErrNoSession is returned by Replace for a name without a session.
var ErrNoSession = errors.New("no session")

// Peer is one BFD session to create.
type Peer struct {
	// Name identifies the session to the caller, for example the BGP
	// neighbor address.
	Name string
	// Addr is the peer address. A link-local address carries its zone.
	Addr netip.Addr
	// Local is the source address of a multihop session.
	Local netip.Addr
	// Device is the interface of a single-hop session or the VRF of a
	// multihop session; empty means no device binding.
	Device   string
	Multihop bool
	Timers   Timers
}

// ChangeFunc is called on every state change of a session.
type ChangeFunc func(p *Peer, from, to State, diag Diag)

// Manager runs BFD sessions.
type Manager struct {
	log      *slog.Logger
	onChange ChangeFunc

	mu        sync.Mutex
	runs      map[uint32]*run // by local discriminator
	listeners map[listenerKey]*listener
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type listenerKey struct {
	device string
	v6     bool
	port   int
}

type listener struct {
	conn net.PacketConn
	// The x/net wrappers keep the control message flags, so the TTL is
	// read with the same wrapper that enabled it.
	p4   *ipv4.PacketConn
	p6   *ipv6.PacketConn
	runs []*run
}

type run struct {
	peer Peer
	sess *Session
	tx   net.PacketConn
	dst  *net.UDPAddr
	rx   chan Packet
	stop chan struct{} // closed by Replace
	done chan struct{} // closed when the loop has ended
}

// NewManager returns a manager that reports state changes to onChange.
func NewManager(log *slog.Logger, onChange ChangeFunc) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{log: log, onChange: onChange, runs: map[uint32]*run{}, listeners: map[listenerKey]*listener{}, ctx: ctx, cancel: cancel}
}

// Add starts a session.
func (m *Manager) Add(p *Peer) error {
	if p.Timers.DetectMult == 0 || p.Timers.DesiredMinTx <= 0 || p.Timers.RequiredMinRx <= 0 {
		return fmt.Errorf("bfd %s: invalid timers %+v", p.Name, p.Timers)
	}
	port := portSingleHop
	if p.Multihop {
		port = portMultihop
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	discr := m.newDiscriminator()
	tx, err := openTx(p)
	if err != nil {
		return fmt.Errorf("bfd %s: %w", p.Name, err)
	}
	r := &run{
		peer: *p,
		sess: NewSession(p.Timers, discr),
		tx:   tx,
		dst:  &net.UDPAddr{IP: p.Addr.AsSlice(), Port: port, Zone: p.Addr.Zone()},
		rx:   make(chan Packet, 16),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	key := listenerKey{device: p.Device, v6: p.Addr.Is6() && !p.Addr.Is4In6(), port: port}
	l, ok := m.listeners[key]
	if !ok {
		l, err = openListener(key)
		if err != nil {
			_ = tx.Close()
			return fmt.Errorf("bfd %s: %w", p.Name, err)
		}
		m.listeners[key] = l
		m.wg.Add(1)
		go m.receive(key, l)
	}
	l.runs = append(l.runs, r)
	m.runs[discr] = r
	m.wg.Add(1)
	go m.loop(r)
	m.log.Info("bfd session added", "peer", p.Name, "address", p.Addr, "device", p.Device, "multihop", p.Multihop,
		"tx", p.Timers.DesiredMinTx, "rx", p.Timers.RequiredMinRx, "multiplier", p.Timers.DetectMult)
	return nil
}

// Replace moves the session named old to a new peer name and address,
// with the same device and timers, as a new session starting Down. It is
// for an unnumbered peer whose link-local address changed: the old
// address no longer answers, so nothing is sent to it.
func (m *Manager) Replace(old, name string, addr netip.Addr) error {
	m.mu.Lock()
	var r *run
	for d, x := range m.runs {
		if x.peer.Name == old {
			r = x
			delete(m.runs, d)
		}
	}
	if r == nil {
		m.mu.Unlock()
		return fmt.Errorf("bfd %s: %w", old, ErrNoSession)
	}
	for _, l := range m.listeners {
		l.runs = slices.DeleteFunc(l.runs, func(x *run) bool { return x == r })
	}
	close(r.stop)
	m.mu.Unlock()
	p := r.peer
	p.Name, p.Addr = name, addr
	return m.Add(&p)
}

// Names returns the names of the sessions.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.runs))
	for _, r := range m.runs {
		names = append(names, r.peer.Name)
	}
	slices.Sort(names)
	return names
}

// Close stops every session. Like bfdd on shutdown, it sends AdminDown so
// the peer does not wait for the detection time.
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	for _, l := range m.listeners {
		_ = l.conn.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) newDiscriminator() uint32 {
	for {
		d := rand.Uint32() //nolint:gosec // unique, not secret (RFC 5880 6.8.1)
		if _, used := m.runs[d]; d != 0 && !used {
			return d
		}
	}
}

// loop sends the periodic packets, hands received packets to the state
// machine and runs the detection timer of one session.
func (m *Manager) loop(r *run) {
	defer m.wg.Done()
	if r.done != nil {
		defer close(r.done)
	}
	defer r.tx.Close() //nolint:errcheck // shutdown
	txTimer := time.NewTimer(0)
	defer txTimer.Stop()
	nextTx := time.Now()
	arm := func(d time.Duration) {
		txTimer.Reset(d)
		nextTx = time.Now().Add(d)
	}
	// rearm sends the next periodic packet sooner when the transmit
	// interval dropped below the time left (RFC 5880 6.8.3: a lower
	// interval applies at once, a higher one waits for the timer). FRR
	// bfdd advertises its slow Required Min RX (1 s) until its own Poll
	// completes and then detects in 3 x 150 ms; a packet still due on the
	// old 1 s timer comes too late (kindmetal: Down with diag 3 about
	// 450 ms after every Up).
	rearm := func() {
		if iv := r.sess.TxInterval(); iv > 0 && time.Until(nextTx) > iv {
			arm(m.nextTx(r))
		}
	}
	detect := time.NewTimer(time.Hour)
	defer detect.Stop()
	for {
		select {
		case <-m.ctx.Done():
			p := r.sess.Packet()
			p.State, p.Diag = StateAdminDown, DiagAdminDown
			m.send(r, &p)
			return
		case <-r.stop:
			return
		case p := <-r.rx:
			m.received(r, &p)
			rearm()
			detect.Reset(r.sess.DetectionTime())
		case <-detect.C:
			from := r.sess.State()
			if r.sess.CheckTimeout(time.Now()) {
				m.changed(r, from)
				m.sendNow(r)
			}
			detect.Reset(time.Hour)
		case <-txTimer.C:
			m.periodic(r)
			arm(m.nextTx(r))
		}
	}
}

// received hands a packet to the state machine. A state change or a Poll
// is answered at once.
func (m *Manager) received(r *run, p *Packet) {
	from := r.sess.State()
	changed := r.sess.Receive(p, time.Now())
	if changed {
		m.changed(r, from)
	}
	if changed || r.sess.NeedFinal() {
		m.sendNow(r)
	}
}

// periodic sends the periodic packet, unless the peer asked for none.
func (m *Manager) periodic(r *run) {
	if r.sess.TxInterval() > 0 {
		m.sendNow(r)
	}
}

// nextTx is the time to the next periodic packet.
func (m *Manager) nextTx(r *run) time.Duration {
	iv := r.sess.TxInterval()
	if iv <= 0 {
		return slowTx
	}
	return jitter(iv, r.peer.Timers.DetectMult)
}

func (m *Manager) sendNow(r *run) {
	p := r.sess.Packet()
	m.send(r, &p)
}

func (m *Manager) send(r *run, p *Packet) {
	if _, err := r.tx.WriteTo(p.Marshal(), r.dst); err != nil {
		m.log.Debug("bfd send failed", "peer", r.peer.Name, "error", err)
	}
}

func (m *Manager) changed(r *run, from State) {
	to := r.sess.State()
	m.log.Info("bfd session state changed", "peer", r.peer.Name, "from", from, "to", to, "diag", r.sess.Diag(),
		"detection_time", r.sess.DetectionTime())
	if m.onChange != nil {
		m.onChange(&r.peer, from, to, r.sess.Diag())
	}
}

// receive reads the packets of one listener and hands each to its session.
func (m *Manager) receive(key listenerKey, l *listener) {
	defer m.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, src, ttl, err := l.readWithTTL(buf)
		if err != nil {
			if m.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			m.log.Debug("bfd read failed", "device", key.device, "error", err)
			continue
		}
		// RFC 5881 5: single-hop packets must have TTL 255 (GTSM).
		if (key.port == portSingleHop && ttl != 255) || (key.port == portMultihop && ttl < minMultihopTTL) {
			m.log.Debug("bfd packet with wrong TTL dropped", "source", src, "ttl", ttl)
			continue
		}
		p, err := Parse(buf[:n])
		if err != nil {
			m.log.Debug("bfd packet dropped", "source", src, "error", err)
			continue
		}
		if r := m.lookup(l, &p, src); r != nil {
			select {
			case r.rx <- p:
			default:
			}
		}
	}
}

// lookup selects the session of a packet: by Your Discriminator, or by
// the source address when it is zero (RFC 5880 6.8.6).
func (m *Manager) lookup(l *listener, p *Packet, src netip.Addr) *run {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.YourDiscriminator != 0 {
		return m.runs[p.YourDiscriminator]
	}
	for _, r := range l.runs {
		if r.peer.Addr.WithZone("") == src.WithZone("") {
			return r
		}
	}
	return nil
}

func (l *listener) readWithTTL(buf []byte) (n int, src netip.Addr, ttl int, err error) {
	ttl = -1
	var from net.Addr
	if l.p6 != nil {
		var cm *ipv6.ControlMessage
		n, cm, from, err = l.p6.ReadFrom(buf)
		if cm != nil {
			ttl = cm.HopLimit
		}
	} else {
		var cm *ipv4.ControlMessage
		n, cm, from, err = l.p4.ReadFrom(buf)
		if cm != nil {
			ttl = cm.TTL
		}
	}
	if err != nil {
		return 0, netip.Addr{}, 0, err //nolint:wrapcheck // the caller logs it
	}
	return n, addrOf(from), ttl, nil
}

func addrOf(a net.Addr) netip.Addr {
	if u, ok := a.(*net.UDPAddr); ok {
		ip, _ := netip.AddrFromSlice(u.IP)
		return ip.Unmap().WithZone(u.Zone)
	}
	return netip.Addr{}
}

// openListener opens the receive socket of a port, bound to the device.
func openListener(key listenerKey) (*listener, error) {
	network, addr := "udp4", "0.0.0.0:"+strconv.Itoa(key.port)
	if key.v6 {
		network, addr = "udp6", "[::]:"+strconv.Itoa(key.port)
	}
	lc := net.ListenConfig{Control: sockopts(key.device)}
	conn, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s %s on %q: %w", network, addr, key.device, err)
	}
	l := &listener{conn: conn}
	if key.v6 {
		l.p6 = ipv6.NewPacketConn(conn)
		err = l.p6.SetControlMessage(ipv6.FlagHopLimit, true)
	} else {
		l.p4 = ipv4.NewPacketConn(conn)
		err = l.p4.SetControlMessage(ipv4.FlagTTL, true)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable TTL control messages: %w", err)
	}
	return l, nil
}

// openTx opens the send socket of a session: a source port of 49152-65535
// and TTL 255 (RFC 5881 4 and 5, RFC 5883 2).
func openTx(p *Peer) (net.PacketConn, error) {
	v6 := p.Addr.Is6() && !p.Addr.Is4In6()
	network, host := "udp4", "0.0.0.0"
	if v6 {
		network, host = "udp6", "::"
	}
	if p.Local.IsValid() {
		host = p.Local.String()
	}
	lc := net.ListenConfig{Control: sockopts(p.Device)}
	start := srcPortMin + rand.IntN(srcPortMax-srcPortMin+1) //nolint:gosec // any free port of the range
	for i := 0; i <= srcPortMax-srcPortMin; i++ {
		port := srcPortMin + (start-srcPortMin+i)%(srcPortMax-srcPortMin+1)
		conn, err := lc.ListenPacket(context.Background(), network, net.JoinHostPort(host, strconv.Itoa(port)))
		if errors.Is(err, syscall.EADDRINUSE) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open send socket: %w", err)
		}
		if v6 {
			err = ipv6.NewPacketConn(conn).SetHopLimit(255)
		} else {
			err = ipv4.NewPacketConn(conn).SetTTL(255)
		}
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("set TTL 255: %w", err)
		}
		return conn, nil
	}
	return nil, errors.New("no free source port in 49152-65535")
}

// sockopts binds a socket to a device (an interface or a VRF).
func sockopts(device string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if device != "" {
				serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
			}
		})
		if err != nil {
			return fmt.Errorf("socket control: %w", err)
		}
		if serr != nil {
			return fmt.Errorf("bind to device %s: %w", device, serr)
		}
		return nil
	}
}
