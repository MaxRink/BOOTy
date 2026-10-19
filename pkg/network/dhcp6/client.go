// Package dhcp6 is BOOTy's DHCPv6 client (RFC 8415). It follows the client
// of systemd-networkd (systemd 255) in the legacy CAPRF ramdisk, which
// starts it when a router advertisement has the M flag (or O for an
// INFORMATION-REQUEST): SOLICIT with rapid commit, IA_NA and IA_PD, the
// client FQDN and networkd's option request list; RENEW, REBIND and
// RELEASE with networkd's options (as captured in kindmetal).
package dhcp6

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"time"
)

// Callback receives a lease.
type Callback func(*Lease)

// Client is the DHCPv6 client of one NIC. Set the fields, then Start it;
// Stop ends it.
type Client struct {
	Iface    *net.Interface
	Hostname string // option 39; none when empty
	DUID     []byte // client identifier (option 1)
	IAID     uint32
	// Stateful requests an address (IA_NA, M flag); otherwise the client
	// sends INFORMATION-REQUESTs for the other options (O flag).
	Stateful bool
	// OnBound runs for every REPLY that assigns the address (or, without
	// Stateful, carries the options).
	OnBound Callback
	// OnExpire runs when the address reaches the end of its valid
	// lifetime without a renewal or rebind.
	OnExpire Callback
	// SendRelease sends a RELEASE for a bound address at Stop (networkd).
	SendRelease bool
	Log         *slog.Logger

	// Seams for tests.
	newTransport func(*net.Interface) (transport, error)
	now          func() time.Time
	rand         func() float64 // in [-0.1, 0.1]
	after        func(time.Duration) <-chan time.Time

	mu    sync.Mutex
	stop  chan struct{}
	done  chan struct{}
	tr    transport
	lease *Lease
}

var errStopped = errors.New("stopped")

// Retransmission parameters (RFC 8415 section 7.6).
type params struct{ irt, mrt time.Duration }

var (
	solicitRT = params{time.Second, 3600 * time.Second}
	requestRT = params{time.Second, 30 * time.Second}
	renewRT   = params{10 * time.Second, 600 * time.Second}
	infoRT    = params{time.Second, 3600 * time.Second}
)

// next is the retransmission timeout after prev (RFC 8415 section 15). The
// first SOLICIT timeout is strictly greater than IRT (18.2.1).
func (c *Client) next(p params, prev time.Duration) time.Duration {
	if prev == 0 {
		r := c.rand()
		if p == solicitRT {
			r = max(math.Abs(r), 0.01)
		}
		return p.irt + time.Duration(r*float64(p.irt))
	}
	rt := 2*prev + time.Duration(c.rand()*float64(prev))
	if rt > p.mrt {
		rt = p.mrt + time.Duration(c.rand()*float64(p.mrt))
	}
	return rt
}

// Start starts the client.
func (c *Client) Start() {
	if c.Log == nil {
		c.Log = slog.Default()
	}
	c.Log = c.Log.With("component", "dhcp6", "iface", c.Iface.Name)
	if c.newTransport == nil {
		c.newTransport = openTransport
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.rand == nil {
		c.rand = func() float64 { return rand.Float64()*0.2 - 0.1 } //nolint:gosec // timing jitter
	}
	if c.after == nil {
		c.after = time.After
	}
	c.stop, c.done = make(chan struct{}), make(chan struct{})
	go c.run()
}

// Stop ends the client and waits for it; with SendRelease it releases a
// bound address.
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
		_ = c.tr.close()
	}
	c.mu.Unlock()
	<-c.done
	if c.SendRelease {
		c.release()
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

func (c *Client) run() {
	defer close(c.done)
	for !c.stopped() {
		c.mu.Lock()
		l := c.lease
		c.mu.Unlock()
		switch {
		case !c.Stateful:
			c.information()
		case l == nil:
			c.solicit()
		default:
			c.extend(l)
		}
		c.endTransaction()
	}
}

// solicit runs SOLICIT (rapid commit) and, after an ADVERTISE, REQUEST.
// During the first retransmission timeout it collects the ADVERTISEs and
// requests from the one with the highest preference; preference 255 or an
// ADVERTISE after the first timeout ends the wait at once (RFC 8415
// 18.2.9, like sd-dhcp6-client client_receive_advertise). A REPLY with
// rapid commit binds at once.
func (c *Client) solicit() {
	start, xid := c.now(), rand.Uint32()&0xffffff //nolint:gosec // a transaction ID
	var rt time.Duration
	var best *reply
	for n := 1; !c.stopped(); n++ {
		rt = c.next(solicitRT, rt)
		var bound *Lease
		_ = c.exchange(func() []byte { return c.solicitMsg(xid, start) }, xid, rt, func(r reply) bool {
			switch {
			case r.typ == msgReply && r.rapidCommit && r.hasAddr:
				bound = &r.lease
				return true
			case r.typ == msgAdvertise && r.hasAddr && r.status == 0:
				if best == nil || r.pref > best.pref {
					best = &r
				}
				return r.pref == 255 || n > 1
			}
			return false
		}, msgAdvertise, msgReply)
		switch {
		case c.stopped():
			return
		case bound != nil:
			c.bind(bound)
			return
		case best != nil:
			c.request(&best.lease, time.Time{})
			return
		}
	}
}

// request asks for an advertised (or, after NoBinding, the current)
// address, up to ten times, until a REPLY with NoBinding or until until
// (when set: the end of the valid lifetime of the current address); it
// tells whether a REPLY bound it.
func (c *Client) request(adv *Lease, until time.Time) bool {
	start, xid := c.now(), rand.Uint32()&0xffffff //nolint:gosec // a transaction ID
	var rt time.Duration
	for n := 0; n < 10 && !c.stopped(); n++ {
		rt = c.next(requestRT, rt)
		if !until.IsZero() {
			left := until.Sub(c.now())
			if left <= 0 {
				return false
			}
			rt = min(rt, left)
		}
		r, err := c.exchangeOne(func() []byte {
			return encode(msgRequest, xid, c.withTail(start, option{optServerID, adv.ServerID}, iaOption(optIANA, c.IAID, adv)))
		}, xid, rt, msgReply)
		switch {
		case err != nil:
		case r.iaStatus == statusNoBinding:
			return false // like networkd: start over
		case r.hasAddr:
			c.bind(&r.lease)
			return true
		}
	}
	return false
}

// extend waits for T1, renews until T2, rebinds until the valid lifetime
// ends, then expires the lease.
func (c *Client) extend(l *Lease) {
	select {
	case <-c.stop:
		return
	case <-c.after(l.Renew.Sub(c.now())):
	}
	for _, phase := range []struct {
		typ   byte
		until time.Time
	}{{msgRenew, l.Rebind}, {msgRebind, l.Expire}} {
		bound, noBinding := c.phase(l, phase.typ, phase.until)
		if bound || c.stopped() {
			return
		}
		if noBinding {
			break
		}
	}
	c.mu.Lock()
	c.lease = nil
	c.mu.Unlock()
	if c.OnExpire != nil {
		c.OnExpire(l)
	}
}

// phase sends RENEW or REBIND until a REPLY binds the address or until is
// reached; it tells whether the address is bound again. A REPLY with
// NoBinding for the IA makes the client send a REQUEST to that server
// (RFC 8415 18.2.10.1); noBinding then tells that the REQUEST failed too,
// and the lease ends at once, like networkd ends it on NoBinding.
func (c *Client) phase(l *Lease, typ byte, until time.Time) (bound, noBinding bool) {
	start, xid := c.now(), rand.Uint32()&0xffffff //nolint:gosec // a transaction ID
	var rt time.Duration
	for !c.stopped() {
		left := until.Sub(c.now())
		if left <= 0 {
			return false, false
		}
		rt = min(c.next(renewRT, rt), left)
		r, err := c.exchangeOne(func() []byte {
			opts := []option{iaOption(optIANA, c.IAID, l)}
			if typ == msgRenew {
				opts = append([]option{{optServerID, l.ServerID}}, opts...)
			}
			if c.Hostname != "" {
				opts = append(opts, option{optClientFQDN, fqdn(c.Hostname)})
			}
			opts = append(opts, oro(optDNSServers, optSNTPServers, optNTPServer, optCaptivePortal), option{optClientID, c.DUID}, elapsed(c.now().Sub(start)))
			return encode(typ, xid, opts)
		}, xid, rt, msgReply)
		switch {
		case err != nil:
		case r.iaStatus == statusNoBinding:
			req := *l
			req.ServerID = r.lease.ServerID
			return c.request(&req, l.Expire), true
		case r.hasAddr:
			c.bind(&r.lease)
			return true, false
		}
	}
	return false, false
}

// infoRefresh is the wait before the next INFORMATION-REQUEST: option 32,
// at least IRT_MINIMUM (600 s), else IRT_DEFAULT (86400 s), RFC 8415
// 18.2.6 and 21.23.
func infoRefresh(v time.Duration) time.Duration {
	if v == 0 {
		return 24 * time.Hour
	}
	return max(v, 600*time.Second)
}

// information sends INFORMATION-REQUESTs until a REPLY, then waits for the
// refresh time (the default of 86400 s).
func (c *Client) information() {
	start, xid := c.now(), rand.Uint32()&0xffffff //nolint:gosec // a transaction ID
	var rt time.Duration
	for !c.stopped() {
		rt = c.next(infoRT, rt)
		r, err := c.exchangeOne(func() []byte {
			return encode(msgInformationRequest, xid, []option{
				oro(optDNSServers, optSNTPServers, optNTPServer, optCaptivePortal), {optClientID, c.DUID}, elapsed(c.now().Sub(start)),
			})
		}, xid, rt, msgReply)
		if err != nil {
			continue
		}
		if c.OnBound != nil {
			c.OnBound(&r.lease)
		}
		c.endTransaction()
		select {
		case <-c.stop:
		case <-c.after(infoRefresh(r.lease.InfoRefresh)):
		}
		return
	}
}

func (c *Client) bind(l *Lease) {
	c.mu.Lock()
	c.lease = l
	c.mu.Unlock()
	c.Log.Debug("DHCPv6 address bound", "addr", l.Addr, "valid", l.Valid)
	if c.OnBound != nil {
		c.OnBound(l)
	}
}

// solicitMsg is networkd's SOLICIT: rapid commit, IA_NA, IA_PD, FQDN, ORO
// (DNS, SNTP, NTP, SOL_MAX_RT, captive portal), client ID, elapsed time.
func (c *Client) solicitMsg(xid uint32, start time.Time) []byte {
	opts := []option{{optRapidCommit, nil}, iaOption(optIANA, c.IAID, nil), iaOption(optIAPD, c.IAID, nil)}
	if c.Hostname != "" {
		opts = append(opts, option{optClientFQDN, fqdn(c.Hostname)})
	}
	opts = append(opts, oro(optDNSServers, optSNTPServers, optNTPServer, optSolMaxRT, optCaptivePortal),
		option{optClientID, c.DUID}, elapsed(c.now().Sub(start)))
	return encode(msgSolicit, xid, opts)
}

// withTail appends FQDN, ORO, client ID and elapsed time to opts.
func (c *Client) withTail(start time.Time, opts ...option) []option {
	if c.Hostname != "" {
		opts = append(opts, option{optClientFQDN, fqdn(c.Hostname)})
	}
	return append(opts, oro(optDNSServers, optSNTPServers, optNTPServer, optCaptivePortal),
		option{optClientID, c.DUID}, elapsed(c.now().Sub(start)))
}

// release sends networkd's RELEASE (server ID, IA_NA, client ID, elapsed).
func (c *Client) release() {
	c.mu.Lock()
	l := c.lease
	c.mu.Unlock()
	if l == nil || !c.Stateful {
		return
	}
	tr, err := c.newTransport(c.Iface)
	if err != nil {
		return
	}
	defer func() { _ = tr.close() }()
	xid := rand.Uint32() & 0xffffff //nolint:gosec // a transaction ID
	msg := encode(msgRelease, xid, []option{
		{optServerID, l.ServerID}, iaOption(optIANA, c.IAID, l), {optClientID, c.DUID}, elapsed(0),
	})
	if err := tr.send(msg); err != nil {
		c.Log.Debug("DHCPv6 RELEASE", "error", err)
		return
	}
	// Wait for the server's REPLY (REL_TIMEOUT, RFC 8415 section 7.6), so
	// it finds the socket open: networkd keeps its socket, and a closed one
	// makes the kernel answer with ICMPv6 port unreachable.
	deadline := c.now().Add(time.Second)
	for left := time.Second; left > 0; left = deadline.Sub(c.now()) {
		b, err := tr.receive(left)
		if err != nil {
			return
		}
		if r, err := decode(b, c.IAID, c.now()); err == nil && r.typ == msgReply && r.xid == xid {
			return
		}
	}
}

// exchangeOne returns the first reply of one of the types.
func (c *Client) exchangeOne(build func() []byte, xid uint32, timeout time.Duration, types ...byte) (reply, error) {
	var r reply
	err := c.exchange(build, xid, timeout, func(x reply) bool { r = x; return true }, types...)
	return r, err
}

// exchange sends the message of build and passes every reply of one of the
// types with the transaction ID and our client ID to take, until take
// returns true (nil) or timeout ends. The socket stays open for the whole
// transaction (endTransaction closes it), so a late reply to an earlier
// transmission is still read (RFC 8415 section 15).
func (c *Client) exchange(build func() []byte, xid uint32, timeout time.Duration, take func(reply) bool, types ...byte) error {
	tr, err := c.open()
	if err != nil {
		return c.pause(timeout)
	}
	msg := build()
	c.Log.Debug("DHCPv6 send", "type", msg[0], "wait", timeout)
	if err := tr.send(msg); err != nil {
		c.Log.Debug("DHCPv6 send", "error", err)
		c.closeTransport(tr)
		return c.pause(timeout)
	}
	deadline := c.now().Add(timeout)
	for {
		left := deadline.Sub(c.now())
		if left <= 0 {
			return errTimeout
		}
		b, err := tr.receive(left)
		if c.stopped() {
			return errStopped
		}
		if err != nil {
			if errors.Is(err, errTimeout) {
				return err
			}
			c.closeTransport(tr)
			return c.pause(left)
		}
		r, err := decode(b, c.IAID, c.now())
		if err != nil || r.xid != xid || !bytes.Equal(r.clientID, c.DUID) || !slices.Contains(types, r.typ) {
			continue
		}
		if take(r) {
			return nil
		}
	}
}

func (c *Client) pause(d time.Duration) error {
	select {
	case <-c.stop:
		return errStopped
	case <-c.after(d):
		return errTimeout
	}
}

// open returns the socket of the transaction, opening one if needed.
func (c *Client) open() (transport, error) {
	c.mu.Lock()
	tr := c.tr
	c.mu.Unlock()
	if tr != nil {
		return tr, nil
	}
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
	c.tr = tr
	return tr, nil
}

// endTransaction closes the socket of the transaction.
func (c *Client) endTransaction() {
	c.mu.Lock()
	tr := c.tr
	c.mu.Unlock()
	if tr != nil {
		c.closeTransport(tr)
	}
}

func (c *Client) closeTransport(tr transport) {
	c.mu.Lock()
	if c.tr == tr {
		c.tr = nil
	}
	c.mu.Unlock()
	_ = tr.close()
}
