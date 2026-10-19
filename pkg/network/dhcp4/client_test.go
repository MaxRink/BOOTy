package dhcp4

import (
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

var (
	testMAC    = net.HardwareAddr{0x52, 0x54, 0x00, 0x44, 0x44, 0x00}
	testServer = net.IPv4(192, 168, 10, 1).To4()
	testAddr   = net.IPv4(192, 168, 10, 10).To4()
)

// sent is one message the client sent.
type sent struct {
	kind     string // broadcast or unicast
	src, dst net.IP
	m        *message
}

// fakeNet is the network and DHCP server of a test, with a fake clock:
// a receive without a reply advances the clock by its timeout; a wait in
// the bound state blocks until the test calls tick.
type fakeNet struct {
	mu      sync.Mutex
	now     time.Time
	sent    []sent
	waits   []time.Duration // receive timeouts and bound waits, in order
	queue   [][]byte
	answer  func(s sent) []byte // the server; nil answers nothing
	tickCh  chan struct{}
	stopped chan struct{}
	// noAddr fails unicasts, like a NIC whose lease address is gone.
	noAddr bool
	// onOpen runs when the client opens a transport; block makes a receive
	// without a reply wait until the transport closes.
	onOpen func()
	block  bool
	// afters counts the waits of the client (c.after): bound waits and
	// pauses; boundAt is afters when OnBound of f.bound last ran.
	afters, boundAt int
}

func newFakeNet() *fakeNet {
	return &fakeNet{now: time.Unix(1_800_000_000, 0), tickCh: make(chan struct{}), stopped: make(chan struct{})}
}

func (f *fakeNet) client() *Client {
	c := &Client{
		Iface:    &net.Interface{Name: "eth9", Index: 99999, MTU: 1500, HardwareAddr: testMAC},
		Hostname: "e2e-test-rfm-0",
		ClientID: append([]byte{255, 1, 2, 3, 4, 0, 2}, make([]byte, 12)...),
		Params:   codes(1, 3, 6, 12, 15, 26, 33, 42, 114, 119, 120, 121),
	}
	c.newTransport = func(*net.Interface) (transport, error) {
		f.mu.Lock()
		hook := f.onOpen
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		return &fakeTransport{f: f, closed: make(chan struct{})}, nil
	}
	c.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	c.jitter = func() time.Duration { return 0 }
	c.after = func(d time.Duration) <-chan time.Time {
		f.mu.Lock()
		f.waits = append(f.waits, d)
		f.afters++
		f.mu.Unlock()
		ch := make(chan time.Time, 1)
		go func() {
			select {
			case <-f.tickCh:
				f.mu.Lock()
				f.now = f.now.Add(d)
				f.mu.Unlock()
				ch <- time.Time{}
			case <-f.stopped:
			}
		}()
		return ch
	}
	return c
}

// boundWait returns once the client waits in BOUND after the lease that
// <-bound just returned: OnBound runs inside bind, and a Reboot before bind
// returns belongs to that lease (bind drops it).
func (f *fakeNet) boundWait(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	n := f.boundAt
	f.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		got := f.afters
		f.mu.Unlock()
		if got > n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the client does not wait in BOUND")
		}
	}
}

// bound is an OnBound that sends the lease to ch, for boundWait.
func (f *fakeNet) bound(ch chan<- *Lease) Callback {
	return func(l *Lease) {
		f.mu.Lock()
		f.boundAt = f.afters
		f.mu.Unlock()
		ch <- l
	}
}

// tick ends the current bound wait.
func (f *fakeNet) tick(t *testing.T) {
	t.Helper()
	select {
	case f.tickCh <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the client does not wait")
	}
}

func (f *fakeNet) messages() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

type fakeTransport struct {
	f      *fakeNet
	closed chan struct{}
	once   sync.Once
}

func (t *fakeTransport) send(s sent) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.sent = append(t.f.sent, s)
	if t.f.answer != nil {
		if r := t.f.answer(s); r != nil {
			t.f.queue = append(t.f.queue, r)
		}
	}
	return nil
}

func (t *fakeTransport) broadcast(src net.IP, m *message) error {
	return t.send(sent{kind: "broadcast", src: src, dst: net.IPv4bcast, m: m})
}

func (t *fakeTransport) unicast(src, dst net.IP, m *message) error {
	t.f.mu.Lock()
	noAddr := t.f.noAddr
	t.f.mu.Unlock()
	if noAddr {
		return errors.New("bind: cannot assign requested address")
	}
	return t.send(sent{kind: "unicast", src: src, dst: dst, m: m})
}

func (t *fakeTransport) receive(timeout time.Duration) ([]byte, error) {
	t.f.mu.Lock()
	if len(t.f.queue) == 0 && t.f.block {
		t.f.mu.Unlock()
		<-t.closed
		return nil, errors.New("use of closed transport")
	}
	defer t.f.mu.Unlock()
	if len(t.f.queue) > 0 {
		r := t.f.queue[0]
		t.f.queue = t.f.queue[1:]
		return r, nil
	}
	t.f.waits = append(t.f.waits, timeout)
	t.f.now = t.f.now.Add(timeout)
	return nil, errTimeout
}

func (t *fakeTransport) close() error {
	if t.closed != nil {
		t.once.Do(func() { close(t.closed) })
	}
	return nil
}

// serverReply builds a reply of the server to m.
func serverReply(m *message, typ dhcpv4.MessageType, extra ...dhcpv4.Option) []byte {
	d := &dhcpv4.DHCPv4{
		OpCode: dhcpv4.OpcodeBootReply, HWType: iana.HWTypeEthernet, ClientHWAddr: m.mac, YourIPAddr: testAddr,
		Options: dhcpv4.OptionsFromList(append([]dhcpv4.Option{
			dhcpv4.OptMessageType(typ),
			dhcpv4.OptServerIdentifier(testServer),
			dhcpv4.OptIPAddressLeaseTime(120 * time.Second),
			dhcpv4.OptSubnetMask(net.IPv4Mask(255, 255, 255, 0)),
			dhcpv4.OptRouter(testServer),
		}, extra...)...),
	}
	binary.BigEndian.PutUint32(d.TransactionID[:], m.xid)
	return d.ToBytes()
}

func codes(c ...uint8) []dhcpv4.OptionCode {
	out := make([]dhcpv4.OptionCode, 0, len(c))
	for _, x := range c {
		out = append(out, dhcpv4.GenericOptionCode(x))
	}
	return out
}

// optionTypes returns the option codes of m in order.
func optionTypes(m *message) []int {
	out := make([]int, 1, 1+len(m.options))
	out[0] = int(dhcpv4.OptionDHCPMessageType)
	for _, o := range m.options {
		out = append(out, int(o.Code.Code()))
	}
	return out
}

func option(m *message, c dhcpv4.OptionCode) []byte {
	for _, o := range m.options {
		if o.Code.Code() == c.Code() {
			return o.Value.ToBytes()
		}
	}
	return nil
}

// TestDiscoverBackoffAndOptions: networkd's options and order (captured in
// kindmetal) and its backoff of 2, 4, 8 s between DISCOVERs; then
// SELECTING with server ID and requested address.
func TestDiscoverBackoffAndOptions(t *testing.T) {
	f := newFakeNet()
	discovers := 0
	f.answer = func(s sent) []byte {
		switch s.m.typ {
		case dhcpv4.MessageTypeDiscover:
			if discovers++; discovers > 3 {
				return serverReply(s.m, dhcpv4.MessageTypeOffer)
			}
		case dhcpv4.MessageTypeRequest:
			return serverReply(s.m, dhcpv4.MessageTypeAck)
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	l := <-bound
	close(f.stopped)
	c.Stop()

	if !l.FixedAddress.Equal(testAddr) || l.Expire.Sub(l.Bound) != 120*time.Second || l.Renew.Sub(l.Bound) != 60*time.Second {
		t.Fatalf("lease = %+v", l)
	}
	msgs := f.messages()
	if len(msgs) != 5 {
		t.Fatalf("sent %d messages, want 4 DISCOVERs and a REQUEST", len(msgs))
	}
	if got := optionTypes(msgs[0].m); !slices.Equal(got, []int{53, 61, 55, 57, 80, 12}) {
		t.Errorf("DISCOVER options = %v, want networkd's 53,61,55,57,80,12", got)
	}
	if got := option(msgs[0].m, dhcpv4.OptionMaximumDHCPMessageSize); !slices.Equal(got, []byte{0x05, 0xc0}) {
		t.Errorf("max message size = %x, want 1472", got)
	}
	req := msgs[4]
	if got := optionTypes(req.m); req.kind != "broadcast" || req.m.ciaddr != nil || !slices.Equal(got, []int{53, 61, 55, 57, 54, 50, 12}) {
		t.Errorf("SELECTING REQUEST = %s %v, want a broadcast with 53,61,55,57,54,50,12", req.kind, got)
	}
	if req.m.xid != msgs[0].m.xid {
		t.Error("the REQUEST has another transaction ID than the DISCOVERs")
	}
	f.mu.Lock()
	waits := slices.Clone(f.waits)
	f.mu.Unlock()
	if want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(waits[:3], want) {
		t.Errorf("waits = %v, want the backoff %v", waits, want)
	}
}

// TestRenewUnicastRebindBroadcast: at T1 a unicast REQUEST from the lease
// address with ciaddr and without 50/54 (RENEWING); unanswered until T2,
// then a broadcast one (REBINDING), which the server answers.
func TestRenewUnicastRebindBroadcast(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		switch {
		case s.m.typ == dhcpv4.MessageTypeDiscover:
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		case s.kind == "broadcast" && s.m.ciaddr != nil:
			return serverReply(s.m, dhcpv4.MessageTypeAck)
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound // rapid commit: no REQUEST
	f.tick(t)
	<-bound
	close(f.stopped)
	c.Stop()

	msgs := f.messages()
	var renew, rebind []sent
	for _, s := range msgs[1:] {
		if s.kind == "unicast" {
			renew = append(renew, s)
		} else {
			rebind = append(rebind, s)
		}
	}
	if msgs[0].m.typ != dhcpv4.MessageTypeDiscover || len(renew) == 0 || len(rebind) != 1 {
		t.Fatalf("messages = %d DISCOVER/ACK, %d renew, %d rebind", len(msgs), len(renew), len(rebind))
	}
	r := renew[0]
	if !r.src.Equal(testAddr) || !r.dst.Equal(testServer) || !r.m.ciaddr.Equal(testAddr) || !slices.Equal(optionTypes(r.m), []int{53, 61, 55, 57, 12}) {
		t.Errorf("RENEWING REQUEST = %s from %s to %s ciaddr %s options %v", r.kind, r.src, r.dst, r.m.ciaddr, optionTypes(r.m))
	}
	b := rebind[0]
	if b.src != nil || !b.m.ciaddr.Equal(testAddr) || !slices.Equal(optionTypes(b.m), []int{53, 61, 55, 57, 12}) {
		t.Errorf("REBINDING REQUEST = %s from %s ciaddr %s options %v", b.kind, b.src, b.m.ciaddr, optionTypes(b.m))
	}
	f.mu.Lock()
	waits := slices.Clone(f.waits)
	f.mu.Unlock()
	// Bound until T1 (60 s), then one renew attempt with the 45 s left
	// until T2 (min(max(22.5 s, 60 s), 45 s)).
	if want := []time.Duration{60 * time.Second, 45 * time.Second}; !slices.Equal(waits[:2], want) {
		t.Errorf("waits = %v, want %v first", waits, want)
	}
}

// TestExpiryAndNak: without answers the lease expires (OnExpire) and the
// client starts over with a DISCOVER; a DHCPNAK to a renewal ends the lease
// at once.
func TestExpiryAndNak(t *testing.T) {
	for _, nak := range []bool{false, true} {
		f := newFakeNet()
		f.answer = func(s sent) []byte {
			switch {
			case s.m.typ == dhcpv4.MessageTypeDiscover:
				return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
			case nak && s.kind == "unicast":
				return serverReply(s.m, dhcpv4.MessageTypeNak)
			}
			return nil
		}
		bound, expired := make(chan *Lease, 4), make(chan *Lease, 1)
		c := f.client()
		c.OnBound = func(l *Lease) { bound <- l }
		c.OnExpire = func(l *Lease) { expired <- l }
		c.Start()
		<-bound
		f.tick(t)
		<-expired
		<-bound // a new lease after the DISCOVER
		close(f.stopped)
		c.Stop()
		msgs := f.messages()
		discovers := 0
		for _, s := range msgs {
			if s.m.typ == dhcpv4.MessageTypeDiscover {
				discovers++
			}
		}
		if discovers != 2 {
			t.Errorf("nak=%v: %d DISCOVERs, want a second one after the end of the lease", nak, discovers)
		}
		// networkd asks for the address of the ended lease (option 50).
		var last sent
		for _, s := range msgs {
			if s.m.typ == dhcpv4.MessageTypeDiscover {
				last = s
			}
		}
		if got := optionTypes(last.m); !slices.Equal(got, []int{53, 61, 55, 57, 50, 80, 12}) || !net.IP(option(last.m, dhcpv4.OptionRequestedIPAddress)).Equal(testAddr) {
			t.Errorf("nak=%v: DISCOVER after the lease = %v, want 53,61,55,57,50,80,12 with the old address", nak, got)
		}
	}
}

// TestRebootAndRelease: Reboot confirms the address with an INIT-REBOOT
// REQUEST (50, no 54, ciaddr 0); Stop with SendRelease sends a DHCPRELEASE
// to the server.
func TestRebootAndRelease(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover || s.m.typ == dhcpv4.MessageTypeRequest {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.SendRelease = true
	c.OnBound = f.bound(bound)
	c.Start()
	<-bound
	f.boundWait(t)
	c.Reboot()
	<-bound
	close(f.stopped)
	c.Stop()

	msgs := f.messages()
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want DISCOVER, REQUEST, RELEASE", len(msgs))
	}
	r := msgs[1]
	if r.m.typ != dhcpv4.MessageTypeRequest || r.kind != "broadcast" || r.m.ciaddr != nil ||
		!slices.Equal(optionTypes(r.m), []int{53, 61, 55, 57, 50, 12}) || !net.IP(option(r.m, dhcpv4.OptionRequestedIPAddress)).Equal(testAddr) {
		t.Errorf("INIT-REBOOT REQUEST = %s ciaddr %s options %v", r.kind, r.m.ciaddr, optionTypes(r.m))
	}
	rel := msgs[2]
	if rel.m.typ != dhcpv4.MessageTypeRelease || rel.kind != "unicast" || !rel.dst.Equal(testServer) || !rel.m.ciaddr.Equal(testAddr) ||
		!slices.Equal(optionTypes(rel.m), []int{53, 61}) {
		t.Errorf("RELEASE = %v %s to %s ciaddr %s", rel.m.typ, rel.kind, rel.dst, rel.m.ciaddr)
	}
}

// TestRebootEndsRenewal: the carrier comes back while the client waits
// in RENEWING after a failed unicast (the lease address was removed with
// the carrier). Reboot sends the INIT-REBOOT at once instead of after the
// wait, like networkd, whose client restarts with the carrier.
func TestRebootEndsRenewal(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover || s.kind == "broadcast" && s.m.ciaddr == nil {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	f.mu.Lock()
	f.noAddr = true
	f.mu.Unlock()
	f.tick(t) // T1: the renewal fails to send and waits
	waits := func() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.waits) }
	for deadline := time.Now().Add(5 * time.Second); waits() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the renewal does not wait")
		}
	}
	c.Reboot()
	select {
	case <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("no INIT-REBOOT before the renewal wait ended")
	}
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	last := msgs[len(msgs)-1]
	if last.kind != "broadcast" || last.m.typ != dhcpv4.MessageTypeRequest || last.m.ciaddr != nil {
		t.Fatalf("last message = %s %v ciaddr %s, want the INIT-REBOOT REQUEST", last.kind, last.m.typ, last.m.ciaddr)
	}
}

// A reply for another client or another transaction is ignored.
func TestIgnoresForeignReplies(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ != dhcpv4.MessageTypeDiscover {
			return nil
		}
		other := *s.m
		other.mac = net.HardwareAddr{2, 0, 0, 0, 0, 1}
		f.queue = append(f.queue, serverReply(&other, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil)))
		wrong := *s.m
		wrong.xid++
		f.queue = append(f.queue, serverReply(&wrong, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil)))
		return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
	}
	bound := make(chan *Lease, 1)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	l := <-bound
	close(f.stopped)
	c.Stop()
	if !l.FixedAddress.Equal(testAddr) {
		t.Fatalf("lease = %+v", l)
	}
}

func TestEncodePadsToMinimumSize(t *testing.T) {
	if b := (&message{typ: dhcpv4.MessageTypeDiscover, xid: 1, mac: testMAC}).encode(); len(b) < minMessageSize {
		t.Fatalf("message is %d bytes, want at least %d", len(b), minMessageSize)
	}
}

// waitFor polls the sent messages until ok, or fails.
func (f *fakeNet) waitFor(t *testing.T, what string, ok func([]sent) bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(f.messages()); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func count(msgs []sent, ok func(sent) bool) int {
	n := 0
	for _, s := range msgs {
		if ok(s) {
			n++
		}
	}
	return n
}

// SELECTING ignores a reply of another type (an OFFER to the REQUEST) and
// starts over with a DISCOVER after five REQUESTs.
func TestSelectingIgnoresOtherTypesAndRetriesFiveTimes(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte { return serverReply(s.m, dhcpv4.MessageTypeOffer) }
	bound := make(chan *Lease, 1)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	isDiscover := func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeDiscover }
	f.waitFor(t, "the second DISCOVER", func(m []sent) bool { return count(m, isDiscover) >= 2 })
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	second := slices.IndexFunc(msgs[1:], isDiscover) + 1
	if n := count(msgs[:second], func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeRequest }); n != 5 {
		t.Errorf("%d REQUESTs before the second DISCOVER, want 5", n)
	}
	select {
	case l := <-bound:
		t.Fatalf("an OFFER to a REQUEST bound %+v", l)
	default:
	}
}

// INIT-REBOOT is sent once, then INIT on the timeout, like networkd.
func TestRebootTriesOnce(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = f.bound(bound)
	c.Start()
	<-bound
	f.boundWait(t)
	c.Reboot()
	<-bound // the DISCOVER after two INIT-REBOOTs
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeRequest }); n != 1 {
		t.Errorf("%d INIT-REBOOT REQUESTs, want 1", n)
	}
}

// RENEWING waits half the time to T2, at least 60 s (RFC 2131 4.4.5).
func TestRenewWait(t *testing.T) {
	f := newFakeNet()
	lease := binary.BigEndian.AppendUint32(nil, 600)
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil), dhcpv4.OptGeneric(dhcpv4.OptionIPAddressLeaseTime, lease))
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	f.tick(t) // T1 at 300 s; T2 at 525 s
	f.waitFor(t, "two RENEWs", func(m []sent) bool {
		return count(m, func(s sent) bool { return s.kind == "unicast" }) >= 2
	})
	close(f.stopped)
	c.Stop()
	f.mu.Lock()
	waits := slices.Clone(f.waits)
	f.mu.Unlock()
	// 225 s to T2: wait 112.5 s; then 112.5 s left: wait 60 s.
	if want := []time.Duration{300 * time.Second, 112500 * time.Millisecond, 60 * time.Second}; !slices.Equal(waits[:3], want) {
		t.Errorf("waits = %v, want %v first", waits, want)
	}
}

// A DHCPNAK from another server than the lease's is ignored in RENEWING,
// like networkd: the client goes on to REBINDING instead of starting over.
func TestNakFromOtherServerIgnored(t *testing.T) {
	f := newFakeNet()
	other := net.IPv4(192, 168, 10, 99).To4()
	f.answer = func(s sent) []byte {
		switch {
		case s.m.typ == dhcpv4.MessageTypeDiscover:
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		case s.kind == "unicast":
			return serverReply(s.m, dhcpv4.MessageTypeNak, dhcpv4.OptServerIdentifier(other))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	f.tick(t)
	isRebind := func(s sent) bool { return s.kind == "broadcast" && s.m.ciaddr != nil }
	f.waitFor(t, "a REBIND", func(m []sent) bool { return count(m, isRebind) >= 1 })
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	first := slices.IndexFunc(msgs, isRebind)
	if n := count(msgs[:first], func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeDiscover }); n != 1 {
		t.Fatalf("%d DISCOVERs: a NAK from another server ended the lease", n)
	}
}

// A lease kept after failed INIT-REBOOTs ends exactly at its expiry while
// the client is in INIT.
func TestLeaseExpiresInInitAfterFailedReboot(t *testing.T) {
	f := newFakeNet()
	first := true
	f.answer = func(s sent) []byte {
		if first && s.m.typ == dhcpv4.MessageTypeDiscover {
			first = false
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound, expired := make(chan *Lease, 1), make(chan time.Time, 1)
	c := f.client()
	c.OnBound = f.bound(bound)
	c.OnExpire = func(*Lease) { expired <- c.now() }
	c.Start()
	l := <-bound
	f.boundWait(t)
	c.Reboot()
	var at time.Time
	select {
	case at = <-expired:
	case <-time.After(5 * time.Second):
		t.Fatal("the lease did not expire after the failed INIT-REBOOTs")
	}
	close(f.stopped)
	c.Stop()
	if !at.Equal(l.Expire) {
		t.Fatalf("expired at %s, want at the lease expiry %s", at, l.Expire)
	}
}

// Reboot while the client opens a transport (none to close yet) is not
// lost: the next message is the INIT-REBOOT, no RENEW.
func TestRebootWhileOpening(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover || s.kind == "broadcast" && s.m.ciaddr == nil {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	var once sync.Once
	f.mu.Lock()
	f.onOpen = func() { once.Do(c.Reboot) } // the RENEW's transport
	f.mu.Unlock()
	f.tick(t)
	<-bound
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	if len(msgs) != 2 || msgs[1].kind != "broadcast" || msgs[1].m.ciaddr != nil || count(msgs, func(s sent) bool { return s.kind == "unicast" }) != 0 {
		t.Fatalf("messages = %d, second %s ciaddr %v: want DISCOVER then the INIT-REBOOT", len(msgs), msgs[1].kind, msgs[1].m.ciaddr)
	}
}

// Reboot ends a receive that is waiting (it closes the transport), and
// the INIT-REBOOT follows once.
func TestRebootEndsBlockingReceive(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover || s.kind == "broadcast" && s.m.ciaddr == nil {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	f.mu.Lock()
	f.block = true
	f.mu.Unlock()
	f.tick(t) // T1: the RENEW waits in receive
	f.waitFor(t, "the RENEW", func(m []sent) bool { return count(m, func(s sent) bool { return s.kind == "unicast" }) == 1 })
	time.Sleep(10 * time.Millisecond)
	c.Reboot()
	<-bound
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.kind == "broadcast" && s.m.typ == dhcpv4.MessageTypeRequest }); n != 1 {
		t.Fatalf("%d INIT-REBOOTs, want 1", n)
	}
}

// A reboot request that comes before DHCP mode applied the first lease
// (the carrier came up around the ACK, kindmetal console order) is dropped
// when the lease binds: one REQUEST, no INIT-REBOOT for the lease just
// bound.
func TestBindDropsPendingReboot(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		switch s.m.typ {
		case dhcpv4.MessageTypeDiscover:
			return serverReply(s.m, dhcpv4.MessageTypeOffer)
		case dhcpv4.MessageTypeRequest:
			return serverReply(s.m, dhcpv4.MessageTypeAck)
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	var once sync.Once
	c.OnBound = func(l *Lease) {
		once.Do(c.Reboot) // the carrier event, before the lease is applied
		bound <- l
	}
	c.Start()
	<-bound
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		f.mu.Lock()
		n := len(f.waits)
		f.mu.Unlock()
		if n > 0 { // the wait for T1, or a receive of an INIT-REBOOT
			break
		}
	}
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeRequest }); n != 1 {
		t.Fatalf("%d REQUESTs, want 1: no INIT-REBOOT for the lease just bound", n)
	}
}

// otherServer is a second DHCP server on the segment.
var otherServer = net.IPv4(192, 168, 10, 99).To4()

func nakFromOther(m *message) []byte {
	return serverReply(m, dhcpv4.MessageTypeNak, dhcpv4.OptServerIdentifier(otherServer))
}

// REQUESTING ignores a DHCPNAK from another server than that of the offer
// and takes the ACK after it, like networkd.
func TestNakFromOtherServerIgnoredInRequesting(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		switch s.m.typ {
		case dhcpv4.MessageTypeDiscover:
			return serverReply(s.m, dhcpv4.MessageTypeOffer)
		case dhcpv4.MessageTypeRequest:
			f.queue = append(f.queue, nakFromOther(s.m)) // f.mu is held
			return serverReply(s.m, dhcpv4.MessageTypeAck)
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	select {
	case <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("no lease: the NAK of another server ended the REQUEST")
	}
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeDiscover }); n != 1 {
		t.Fatalf("%d DISCOVERs, want 1", n)
	}
}

// REBOOTING ignores a DHCPNAK from another server than that of the lease.
func TestNakFromOtherServerIgnoredInRebooting(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		switch {
		case s.m.typ == dhcpv4.MessageTypeDiscover:
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		case s.m.typ == dhcpv4.MessageTypeRequest && s.kind == "broadcast":
			f.queue = append(f.queue, nakFromOther(s.m))
			return serverReply(s.m, dhcpv4.MessageTypeAck)
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = f.bound(bound)
	c.Start()
	<-bound
	f.boundWait(t)
	c.Reboot()
	<-bound
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeDiscover }); n != 1 {
		t.Fatalf("%d DISCOVERs: the NAK of another server ended the INIT-REBOOT", n)
	}
}

// A wake-up token without the reboot flag (the run loop took the flag
// before Reboot sent the token) does not start a second INIT-REBOOT: the
// bound client waits again.
func TestStaleRebootTokenNoInitReboot(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ == dhcpv4.MessageTypeDiscover {
			return serverReply(s.m, dhcpv4.MessageTypeAck, dhcpv4.OptGeneric(dhcpv4.OptionRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	waitLen := func(n int) {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			f.mu.Lock()
			got := len(f.waits)
			f.mu.Unlock()
			if got >= n {
				return
			}
		}
	}
	waitLen(1) // the wait for T1
	waits := 1
	c.reboot <- struct{}{} // the stale token
	waitLen(2)             // the next wait: for T1 again, or of the INIT-REBOOT
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.m.typ == dhcpv4.MessageTypeRequest }); n != 0 {
		t.Fatalf("%d REQUESTs after a stale token, want none", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.waits) <= waits {
		t.Fatal("the bound client does not wait again after the stale token")
	}
}
