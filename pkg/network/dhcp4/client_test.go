package dhcp4

import (
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
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
}

func newFakeNet() *fakeNet {
	return &fakeNet{now: time.Unix(1_800_000_000, 0), tickCh: make(chan struct{}), stopped: make(chan struct{})}
}

func (f *fakeNet) client() *Client {
	c := &Client{
		Iface:    &net.Interface{Name: "eth9", Index: 99999, MTU: 1500, HardwareAddr: testMAC},
		Hostname: "e2e-test-rfm-0",
		ClientID: append([]byte{255, 1, 2, 3, 4, 0, 2}, make([]byte, 12)...),
		Params:   []layers.DHCPOpt{1, 3, 6, 12, 15, 26, 33, 42, 114, 119, 120, 121},
	}
	c.newTransport = func(*net.Interface) (transport, error) { return &fakeTransport{f: f}, nil }
	c.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	c.jitter = func() time.Duration { return 0 }
	c.after = func(d time.Duration) <-chan time.Time {
		f.mu.Lock()
		f.waits = append(f.waits, d)
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

type fakeTransport struct{ f *fakeNet }

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
	return t.send(sent{kind: "unicast", src: src, dst: dst, m: m})
}

func (t *fakeTransport) receive(timeout time.Duration) ([]byte, error) {
	t.f.mu.Lock()
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

func (t *fakeTransport) close() error { return nil }

// serverReply builds a reply of the server to m.
func serverReply(m *message, typ layers.DHCPMsgType, extra ...layers.DHCPOption) []byte {
	lease := make([]byte, 4)
	binary.BigEndian.PutUint32(lease, 120)
	d := &layers.DHCPv4{
		Operation: layers.DHCPOpReply, HardwareType: layers.LinkTypeEthernet, HardwareLen: 6,
		Xid: m.xid, ClientHWAddr: m.mac, YourClientIP: testAddr, ClientIP: net.IPv4zero,
		NextServerIP: net.IPv4zero, RelayAgentIP: net.IPv4zero,
		Options: append([]layers.DHCPOption{
			layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(typ)}),
			layers.NewDHCPOption(layers.DHCPOptServerID, testServer),
			layers.NewDHCPOption(layers.DHCPOptLeaseTime, lease),
			layers.NewDHCPOption(layers.DHCPOptSubnetMask, []byte{255, 255, 255, 0}),
			layers.NewDHCPOption(layers.DHCPOptRouter, testServer),
		}, extra...),
	}
	buf := gopacket.NewSerializeBuffer()
	_ = d.SerializeTo(buf, gopacket.SerializeOptions{})
	return buf.Bytes()
}

// optionTypes returns the option codes of m in order, without padding.
func optionTypes(m *message) []int {
	out := []int{int(layers.DHCPOptMessageType)}
	for _, o := range m.options {
		if o.Type != layers.DHCPOptPad {
			out = append(out, int(o.Type))
		}
	}
	return out
}

func option(m *message, t layers.DHCPOpt) []byte {
	for _, o := range m.options {
		if o.Type == t {
			return o.Data
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
		case layers.DHCPMsgTypeDiscover:
			if discovers++; discovers > 3 {
				return serverReply(s.m, layers.DHCPMsgTypeOffer)
			}
		case layers.DHCPMsgTypeRequest:
			return serverReply(s.m, layers.DHCPMsgTypeAck)
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
	if got := option(msgs[0].m, layers.DHCPOptMaxMessageSize); !slices.Equal(got, []byte{0x05, 0xc0}) {
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
		case s.m.typ == layers.DHCPMsgTypeDiscover:
			return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil))
		case s.kind == "broadcast" && s.m.ciaddr != nil:
			return serverReply(s.m, layers.DHCPMsgTypeAck)
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
	if msgs[0].m.typ != layers.DHCPMsgTypeDiscover || len(renew) == 0 || len(rebind) != 1 {
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
			case s.m.typ == layers.DHCPMsgTypeDiscover:
				return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil))
			case nak && s.kind == "unicast":
				return serverReply(s.m, layers.DHCPMsgTypeNak)
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
			if s.m.typ == layers.DHCPMsgTypeDiscover {
				discovers++
			}
		}
		if discovers != 2 {
			t.Errorf("nak=%v: %d DISCOVERs, want a second one after the end of the lease", nak, discovers)
		}
		// networkd asks for the address of the ended lease (option 50).
		var last sent
		for _, s := range msgs {
			if s.m.typ == layers.DHCPMsgTypeDiscover {
				last = s
			}
		}
		if got := optionTypes(last.m); !slices.Equal(got, []int{53, 61, 55, 57, 50, 80, 12}) || !net.IP(option(last.m, layers.DHCPOptRequestIP)).Equal(testAddr) {
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
		if s.m.typ == layers.DHCPMsgTypeDiscover || s.m.typ == layers.DHCPMsgTypeRequest {
			return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.SendRelease = true
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	c.Reboot()
	<-bound
	close(f.stopped)
	c.Stop()

	msgs := f.messages()
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want DISCOVER, REQUEST, RELEASE", len(msgs))
	}
	r := msgs[1]
	if r.m.typ != layers.DHCPMsgTypeRequest || r.kind != "broadcast" || r.m.ciaddr != nil ||
		!slices.Equal(optionTypes(r.m), []int{53, 61, 55, 57, 50, 12}) || !net.IP(option(r.m, layers.DHCPOptRequestIP)).Equal(testAddr) {
		t.Errorf("INIT-REBOOT REQUEST = %s ciaddr %s options %v", r.kind, r.m.ciaddr, optionTypes(r.m))
	}
	rel := msgs[2]
	if rel.m.typ != layers.DHCPMsgTypeRelease || rel.kind != "unicast" || !rel.dst.Equal(testServer) || !rel.m.ciaddr.Equal(testAddr) ||
		!slices.Equal(optionTypes(rel.m), []int{53, 61}) {
		t.Errorf("RELEASE = %v %s to %s ciaddr %s", rel.m.typ, rel.kind, rel.dst, rel.m.ciaddr)
	}
}

// A reply for another client or another transaction is ignored.
func TestIgnoresForeignReplies(t *testing.T) {
	f := newFakeNet()
	f.answer = func(s sent) []byte {
		if s.m.typ != layers.DHCPMsgTypeDiscover {
			return nil
		}
		other := *s.m
		other.mac = net.HardwareAddr{2, 0, 0, 0, 0, 1}
		f.queue = append(f.queue, serverReply(&other, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil)))
		wrong := *s.m
		wrong.xid++
		f.queue = append(f.queue, serverReply(&wrong, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil)))
		return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil))
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
	b, err := (&message{typ: layers.DHCPMsgTypeDiscover, xid: 1, mac: testMAC}).encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < minMessageSize {
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
	f.answer = func(s sent) []byte { return serverReply(s.m, layers.DHCPMsgTypeOffer) }
	bound := make(chan *Lease, 1)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	isDiscover := func(s sent) bool { return s.m.typ == layers.DHCPMsgTypeDiscover }
	f.waitFor(t, "the second DISCOVER", func(m []sent) bool { return count(m, isDiscover) >= 2 })
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	second := slices.IndexFunc(msgs[1:], isDiscover) + 1
	if n := count(msgs[:second], func(s sent) bool { return s.m.typ == layers.DHCPMsgTypeRequest }); n != 5 {
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
		if s.m.typ == layers.DHCPMsgTypeDiscover {
			return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil))
		}
		return nil
	}
	bound := make(chan *Lease, 4)
	c := f.client()
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	c.Reboot()
	<-bound // the DISCOVER after two INIT-REBOOTs
	close(f.stopped)
	c.Stop()
	if n := count(f.messages(), func(s sent) bool { return s.m.typ == layers.DHCPMsgTypeRequest }); n != 1 {
		t.Errorf("%d INIT-REBOOT REQUESTs, want 1", n)
	}
}

// RENEWING waits half the time to T2, at least 60 s (RFC 2131 4.4.5).
func TestRenewWait(t *testing.T) {
	f := newFakeNet()
	lease := binary.BigEndian.AppendUint32(nil, 600)
	f.answer = func(s sent) []byte {
		if s.m.typ == layers.DHCPMsgTypeDiscover {
			return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil), layers.NewDHCPOption(layers.DHCPOptLeaseTime, lease))
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
		case s.m.typ == layers.DHCPMsgTypeDiscover:
			return serverReply(s.m, layers.DHCPMsgTypeAck, layers.NewDHCPOption(optRapidCommit, nil))
		case s.kind == "unicast":
			return serverReply(s.m, layers.DHCPMsgTypeNak, layers.NewDHCPOption(layers.DHCPOptServerID, other))
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
	if n := count(msgs[:first], func(s sent) bool { return s.m.typ == layers.DHCPMsgTypeDiscover }); n != 1 {
		t.Fatalf("%d DISCOVERs: a NAK from another server ended the lease", n)
	}
}
