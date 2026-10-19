// Package dhcp4 is BOOTy's DHCPv4 client. It follows RFC 2131 and the
// client of systemd-networkd (systemd 255), the client of the legacy CAPRF
// ramdisk: its options and their order, its retransmission backoff (2, 4,
// ... 64 s), RENEWING with a unicast DHCPREQUEST, REBINDING with a
// broadcast one, INIT-REBOOT, and rapid commit (RFC 4039).
package dhcp4

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/gopacket/layers"
)

// Callback receives a lease.
type Callback func(*Lease)

// Client is the DHCPv4 client of one NIC. Set the fields, then Start it;
// Stop ends it. A Client is not restarted after Stop.
type Client struct {
	Iface    *net.Interface
	Hostname string // option 12; none when empty
	ClientID []byte // option 61; none when empty
	Params   []layers.DHCPOpt
	// OnBound runs for every lease the server acknowledges: the first one,
	// renewals, rebinds and the lease after an INIT-REBOOT.
	OnBound Callback
	// OnExpire runs when the lease ends: it expired before a renewal or a
	// rebind, or the server sent a DHCPNAK.
	OnExpire Callback
	// SendRelease sends a DHCPRELEASE for a bound lease at Stop, like
	// networkd (SendRelease=yes).
	SendRelease bool
	Log         *slog.Logger

	// Seams for tests.
	newTransport func(*net.Interface) (transport, error)
	now          func() time.Time
	jitter       func() time.Duration
	after        func(time.Duration) <-chan time.Time

	mu     sync.Mutex
	stop   chan struct{}
	done   chan struct{}
	reboot chan struct{}
	// rebootReq is set by Reboot until the client starts the INIT-REBOOT;
	// it ends any wait or exchange of the client.
	rebootReq atomic.Bool
	tr        transport // the open transport of the running exchange
	lease     *Lease
	// lastAddr is the address of the lease that ended last.
	lastAddr net.IP
	started  time.Time
}

// errStopped ends a wait because Stop was called; errReboot because
// Reboot was called.
var (
	errStopped = errors.New("stopped")
	errReboot  = errors.New("reboot requested")
)

// interrupted tells whether err ends the current state: Stop or Reboot.
func interrupted(err error) bool {
	return errors.Is(err, errStopped) || errors.Is(err, errReboot)
}

// timeout of the n-th retransmission (0 based), like networkd and RFC 2131
// section 4.1: 2, 4, 8, 16, 32, then 64 s, each +-1 s.
func (c *Client) timeout(n int) time.Duration {
	if n > 5 {
		n = 5
	}
	return time.Duration(2<<n)*time.Second + c.jitter()
}

// Start starts the client in INIT.
func (c *Client) Start() {
	if c.Log == nil {
		c.Log = slog.Default()
	}
	c.Log = c.Log.With("component", "dhcp", "iface", c.Iface.Name)
	if c.newTransport == nil {
		c.newTransport = openTransport
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.jitter == nil {
		c.jitter = func() time.Duration { return time.Duration(rand.Int64N(int64(2*time.Second))) - time.Second } //nolint:gosec // timing jitter
	}
	if c.after == nil {
		c.after = time.After
	}
	c.stop = make(chan struct{})
	c.done = make(chan struct{})
	c.reboot = make(chan struct{}, 1)
	go c.run()
}

// Stop ends the client and waits for it. With SendRelease it releases a
// bound lease.
func (c *Client) Stop() {
	c.mu.Lock()
	select {
	case <-c.stop:
		c.mu.Unlock()
		<-c.done
		return
	default:
	}
	close(c.stop)
	if c.tr != nil {
		_ = c.tr.close() // ends a receive in progress
	}
	c.mu.Unlock()
	<-c.done
	if c.SendRelease {
		c.release()
	}
}

// Reboot asks the client to confirm its address (INIT-REBOOT) at once,
// like networkd restarts its client when the carrier comes back: it ends
// any wait or exchange in progress (a renewal that waits for an answer or
// after a failed send, the INIT backoff).
//
// The flag decides, the token only wakes a wait: the flag is set (under
// c.mu, which open checks) before the token, so a wake always finds it,
// and the run loop starts one INIT-REBOOT per request.
func (c *Client) Reboot() {
	c.mu.Lock()
	c.rebootReq.Store(true)
	if c.tr != nil {
		_ = c.tr.close() // ends a receive in progress
	}
	c.mu.Unlock()
	select {
	case c.reboot <- struct{}{}:
	default:
	}
}

func (c *Client) stopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// state is the RFC 2131 client state.
type state int

const (
	stateInit state = iota
	stateRebooting
	stateBound
	stateRenewing
	stateRebinding
)

func (c *Client) run() {
	defer close(c.done)
	st := stateInit
	for !c.stopped() {
		if c.rebootReq.Swap(false) {
			select {
			case <-c.reboot:
			default:
			}
			st = stateRebooting
		}
		switch st {
		case stateInit:
			st = c.initState()
		case stateRebooting:
			st = c.rebootState()
		case stateBound:
			st = c.boundState()
		case stateRenewing:
			st = c.renewState()
		case stateRebinding:
			st = c.rebindState()
		}
	}
}

// initState runs DISCOVER (with rapid commit), OFFER, REQUEST, ACK.
func (c *Client) initState() state {
	c.started = c.now()
	xid := rand.Uint32() //nolint:gosec // a transaction ID
	for n := 0; ; n++ {
		// A lease kept after failed INIT-REBOOTs (the carrier came back but
		// the server does not answer) still ends at its expiry.
		timeout := c.timeout(n)
		if left, ok := c.leaseLeft(); ok {
			if left <= 0 {
				c.expire()
			} else {
				timeout = min(timeout, left)
			}
		}
		r, err := c.exchange(func(tr transport, m *message) error { return tr.broadcast(nil, m) },
			c.discover(xid), timeout, nil, layers.DHCPMsgTypeOffer, layers.DHCPMsgTypeAck)
		switch {
		case interrupted(err):
			return stateInit
		case err != nil:
			continue
		case r.typ == layers.DHCPMsgTypeAck && r.rapidCommit:
			return c.bind(&r.lease)
		case r.typ == layers.DHCPMsgTypeOffer:
			return c.selecting(xid, &r.lease)
		}
	}
}

// selecting requests an offered lease; without an answer after five
// attempts, or with a DHCPNAK, the client starts over.
func (c *Client) selecting(xid uint32, offer *Lease) state {
	for n := 0; n < 5; n++ {
		r, err := c.exchange(func(tr transport, m *message) error { return tr.broadcast(nil, m) },
			c.request(xid, nil, offer.ServerID, offer.FixedAddress), c.timeout(n), offer.ServerID, layers.DHCPMsgTypeAck, layers.DHCPMsgTypeNak)
		switch {
		case interrupted(err):
			return stateInit
		case err != nil:
			continue
		case r.typ == layers.DHCPMsgTypeAck:
			return c.bind(&r.lease)
		default:
			c.Log.Info("DHCPNAK, starting over")
			return stateInit
		}
	}
	return stateInit
}

// rebootState confirms the address of the last lease (INIT-REBOOT). Like
// networkd it sends once and goes to INIT on the first timeout. DHCPMode
// calls Reboot when the carrier returns (wired in #793).
func (c *Client) rebootState() state {
	c.mu.Lock()
	last := c.lease
	c.mu.Unlock()
	if last == nil {
		return stateInit
	}
	c.started = c.now()
	xid := rand.Uint32() //nolint:gosec // a transaction ID
	for n := 0; n < 1; n++ {
		r, err := c.exchange(func(tr transport, m *message) error { return tr.broadcast(nil, m) },
			c.request(xid, nil, nil, last.FixedAddress), c.timeout(n), last.ServerID, layers.DHCPMsgTypeAck, layers.DHCPMsgTypeNak)
		switch {
		case interrupted(err):
			return stateInit
		case err != nil:
			continue
		case r.typ == layers.DHCPMsgTypeAck:
			return c.bind(&r.lease)
		default:
			return c.expire()
		}
	}
	return stateInit
}

// boundState waits for T1 or a reboot request.
func (c *Client) boundState() state {
	c.mu.Lock()
	l := c.lease
	c.mu.Unlock()
	if l == nil || l.Renew.IsZero() {
		// An infinite lease: only a reboot request or Stop ends the wait.
		select {
		case <-c.stop:
		case <-c.reboot: // the run loop sees the flag
		}
		return stateBound
	}
	select {
	case <-c.stop:
		return stateBound
	case <-c.reboot: // the run loop sees the flag
		return stateBound
	case <-c.after(l.Renew.Sub(c.now())):
		return stateRenewing
	}
}

// renewState sends unicast DHCPREQUESTs to the server of the lease until
// T2 (RFC 2131 section 4.4.5): the wait is half the time left, at least
// 60 s.
func (c *Client) renewState() state {
	return c.extend(stateRebinding, func(l *Lease) time.Time { return l.Rebind },
		func(tr transport, m *message, l *Lease) error { return tr.unicast(l.FixedAddress, l.ServerID, m) })
}

// rebindState broadcasts DHCPREQUESTs until the lease expires. Like
// every networkd broadcast, they have the IP source 0.0.0.0; the lease
// address is in ciaddr.
func (c *Client) rebindState() state {
	return c.extend(stateInit, func(l *Lease) time.Time { return l.Expire },
		func(tr transport, m *message, _ *Lease) error { return tr.broadcast(nil, m) })
}

func (c *Client) extend(next state, until func(*Lease) time.Time, send func(transport, *message, *Lease) error) state {
	c.mu.Lock()
	l := c.lease
	c.mu.Unlock()
	if l == nil {
		return stateInit
	}
	c.started = c.now()
	xid := rand.Uint32() //nolint:gosec // a transaction ID
	for !c.stopped() {
		left := until(l).Sub(c.now())
		if left <= 0 {
			if next == stateInit {
				return c.expire()
			}
			return next
		}
		wait := max(left/2, 60*time.Second)
		wait = min(wait, left)
		r, err := c.exchange(func(tr transport, m *message) error { return send(tr, m, l) },
			c.request(xid, l.FixedAddress, nil, nil), wait, l.ServerID, layers.DHCPMsgTypeAck, layers.DHCPMsgTypeNak)
		switch {
		case interrupted(err):
			return stateBound
		case err != nil:
			continue
		case r.typ == layers.DHCPMsgTypeAck:
			return c.bind(&r.lease)
		default:
			return c.expire()
		}
	}
	return stateBound
}

// bind keeps a lease and reports it.
func (c *Client) bind(l *Lease) state {
	c.mu.Lock()
	c.lease = l
	c.mu.Unlock()
	c.Log.Debug("DHCP lease bound", "addr", l.FixedAddress, "server", l.ServerID, "expire", l.Expire)
	if c.OnBound != nil {
		c.OnBound(l)
	}
	// A fresh lease answers a reboot request that came before it was
	// applied (the carrier came up around the ACK, DHCP mode had no lease
	// to confirm yet): no INIT-REBOOT for the lease just bound. A request
	// that comes while OnBound returns is dropped too; the ACK has just
	// confirmed the lease.
	c.rebootReq.Store(false)
	select {
	case <-c.reboot:
	default:
	}
	return stateBound
}

// leaseLeft returns the time to the expiry of the lease the client keeps;
// ok is false without a lease or for a lease without an end.
func (c *Client) leaseLeft() (time.Duration, bool) {
	c.mu.Lock()
	l := c.lease
	c.mu.Unlock()
	if l == nil || l.Expire.IsZero() {
		return 0, false
	}
	return l.Expire.Sub(c.now()), true
}

// expire ends the lease and starts over in INIT.
func (c *Client) expire() state {
	c.mu.Lock()
	l := c.lease
	c.lease = nil
	if l != nil {
		c.lastAddr = l.FixedAddress
	}
	c.mu.Unlock()
	if l != nil && c.OnExpire != nil {
		c.OnExpire(l)
	}
	return stateInit
}

// exchange sends m with send and waits up to timeout for a reply of one of
// the types with the same transaction ID and our MAC.
// A DHCPNAK counts only from server (the server of the offer or lease),
// like networkd ("NAK from unexpected server, ignoring").
func (c *Client) exchange(send func(transport, *message) error, m *message, timeout time.Duration, server net.IP, types ...layers.DHCPMsgType) (reply, error) {
	tr, err := c.open()
	if interrupted(err) {
		return reply{}, err
	}
	if err != nil {
		c.Log.Debug("DHCP transport", "error", err)
		return reply{}, c.pause(timeout)
	}
	defer c.closeTransport(tr)
	m.secs = c.secs()
	c.Log.Debug("DHCP send", "type", m.typ, "ciaddr", m.ciaddr, "wait", timeout)
	if err := send(tr, m); err != nil {
		c.Log.Debug("DHCP send", "type", m.typ, "error", err)
		return reply{}, c.pause(timeout)
	}
	deadline := c.now().Add(timeout)
	for {
		left := deadline.Sub(c.now())
		if left <= 0 {
			return reply{}, errTimeout
		}
		payload, err := tr.receive(left)
		if c.stopped() {
			return reply{}, errStopped
		}
		if c.rebootReq.Load() {
			return reply{}, errReboot
		}
		if err != nil {
			if errors.Is(err, errTimeout) {
				return reply{}, errTimeout
			}
			c.Log.Debug("DHCP receive", "error", err)
			return reply{}, c.pause(left)
		}
		r, ok := decodeReply(payload, c.now())
		if ok && c.wanted(&r, m, server, types) {
			return r, nil
		}
	}
}

// wanted tells whether r answers m with one of the types; a NAK counts only
// from server.
func (c *Client) wanted(r *reply, m *message, server net.IP, types []layers.DHCPMsgType) bool {
	if r.xid != m.xid || r.mac.String() != c.Iface.HardwareAddr.String() || !slices.Contains(types, r.typ) {
		return false
	}
	if r.typ == layers.DHCPMsgTypeNak && server != nil && !r.lease.ServerID.Equal(server) {
		c.Log.Debug("DHCPNAK from an unexpected server, ignoring", "server", r.lease.ServerID)
		return false
	}
	return true
}

// pause waits d after a transport error, so a failing socket does not spin.
func (c *Client) pause(d time.Duration) error {
	timeout := c.after(d)
	for {
		if c.rebootReq.Load() {
			return errReboot
		}
		select {
		case <-c.stop:
			return errStopped
		case <-c.reboot: // a stale token without the flag keeps waiting
		case <-timeout:
			return errTimeout
		}
	}
}

func (c *Client) open() (transport, error) {
	tr, err := c.newTransport(c.Iface)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped() {
		_ = tr.close()
		return nil, errStopped
	}
	if c.rebootReq.Load() {
		// Reboot came while no transport was open: it closed nothing.
		_ = tr.close()
		return nil, errReboot
	}
	c.tr = tr
	return tr, nil
}

func (c *Client) closeTransport(tr transport) {
	c.mu.Lock()
	if c.tr == tr {
		c.tr = nil
	}
	c.mu.Unlock()
	_ = tr.close()
}

// secs is the time since the client began the exchange (RFC 2131).
func (c *Client) secs() uint16 {
	s := c.now().Sub(c.started) / time.Second
	return uint16(min(max(s, 0), 0xffff)) //nolint:gosec // bounded
}

// release sends a DHCPRELEASE for a bound lease.
func (c *Client) release() {
	c.mu.Lock()
	l := c.lease
	c.mu.Unlock()
	if l == nil || l.ServerID == nil {
		return
	}
	tr, err := c.newTransport(c.Iface)
	if err != nil {
		return
	}
	defer func() { _ = tr.close() }()
	// networkd sends the client identifier only (captured in kindmetal:
	// options 53, 61), unicast from the lease address to the server.
	var opts []layers.DHCPOption
	if len(c.ClientID) > 0 {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptClientID, c.ClientID))
	}
	m := &message{typ: layers.DHCPMsgTypeRelease, xid: rand.Uint32(), ciaddr: l.FixedAddress, mac: c.Iface.HardwareAddr, options: opts} //nolint:gosec // a transaction ID
	if err := tr.unicast(l.FixedAddress, l.ServerID, m); err != nil {
		c.Log.Debug("DHCPRELEASE", "error", err)
	}
}

// discover is a DHCPDISCOVER with networkd's options: 61, 55, 57, 80, 12.
func (c *Client) discover(xid uint32) *message {
	opts := c.common()
	// Like networkd (sd-dhcp-client last_addr), a DISCOVER after a lease
	// asks for its address again (option 50).
	c.mu.Lock()
	last := c.lastAddr
	if c.lease != nil {
		last = c.lease.FixedAddress
	}
	c.mu.Unlock()
	if ip := last.To4(); ip != nil {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptRequestIP, ip))
	}
	opts = append(opts, layers.NewDHCPOption(optRapidCommit, nil))
	return &message{typ: layers.DHCPMsgTypeDiscover, xid: xid, mac: c.Iface.HardwareAddr, options: c.withHostname(opts)}
}

// request is a DHCPREQUEST: with server ID and address when SELECTING
// (54, 50), with only the address for INIT-REBOOT (50), with ciaddr and
// neither when RENEWING or REBINDING.
func (c *Client) request(xid uint32, ciaddr, server, addr net.IP) *message {
	opts := c.common()
	if server != nil {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptServerID, server.To4()))
	}
	if addr != nil {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptRequestIP, addr.To4()))
	}
	return &message{typ: layers.DHCPMsgTypeRequest, xid: xid, ciaddr: ciaddr, mac: c.Iface.HardwareAddr, options: c.withHostname(opts)}
}

// common returns 61, 55 and 57 (the MTU of the NIC now, less 28).
func (c *Client) common() []layers.DHCPOption {
	var opts []layers.DHCPOption
	if len(c.ClientID) > 0 {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptClientID, c.ClientID))
	}
	if len(c.Params) > 0 {
		prl := make([]byte, 0, len(c.Params))
		for _, p := range c.Params {
			prl = append(prl, byte(p))
		}
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptParamsRequest, prl))
	}
	mtu := c.Iface.MTU
	if ifi, err := net.InterfaceByIndex(c.Iface.Index); err == nil {
		mtu = ifi.MTU
	}
	if mtu >= 576 {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptMaxMessageSize, []byte{byte((mtu - 28) >> 8), byte(mtu - 28)}))
	}
	return opts
}

func (c *Client) withHostname(opts []layers.DHCPOption) []layers.DHCPOption {
	if c.Hostname != "" {
		opts = append(opts, layers.NewDHCPOption(layers.DHCPOptHostname, []byte(c.Hostname)))
	}
	return opts
}
