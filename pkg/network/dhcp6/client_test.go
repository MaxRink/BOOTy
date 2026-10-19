package dhcp6

import (
	"bytes"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
)

var (
	testDUID   = []byte{0, 2, 0, 0, 0xab, 0x11, 1, 2, 3, 4, 5, 6, 7, 8}
	testServer = []byte{0, 1, 0, 1, 9, 9, 9, 9}
	testAddr   = net.ParseIP("fd00:10::1a4")
)

// fakeNet is the server and clock of a test: a receive without a reply
// advances the clock by its timeout; the wait for T1 blocks until tick.
type fakeNet struct {
	mu      sync.Mutex
	now     time.Time
	sent    [][]byte
	waits   []time.Duration
	queue   [][]byte
	answer  func(msg []byte) []byte
	tickCh  chan struct{}
	stopped chan struct{}
}

func newFakeNet() *fakeNet {
	return &fakeNet{now: time.Unix(1_800_000_000, 0), tickCh: make(chan struct{}), stopped: make(chan struct{})}
}

func (f *fakeNet) client(stateful bool) *Client {
	c := &Client{Iface: &net.Interface{Name: "eth9", Index: 99999}, Hostname: "e2e-test-rfm-0", DUID: testDUID, IAID: 7, Stateful: stateful}
	c.newTransport = func(*net.Interface) (transport, error) { return &fakeTransport{f: f}, nil }
	c.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	c.rand = func() float64 { return 0 }
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

func (f *fakeNet) tick(t *testing.T) {
	t.Helper()
	select {
	case f.tickCh <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the client does not wait")
	}
}

func (f *fakeNet) messages() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

type fakeTransport struct{ f *fakeNet }

func (t *fakeTransport) send(msg []byte) error {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	t.f.sent = append(t.f.sent, msg)
	if t.f.answer != nil {
		if r := t.f.answer(msg); r != nil {
			t.f.queue = append(t.f.queue, r)
		}
	}
	return nil
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

func xidOf(msg []byte) uint32 { return uint32(msg[1])<<16 | uint32(msg[2])<<8 | uint32(msg[3]) }

// serverReply answers msg with typ: T1 60 s, T2 96 s, preferred 120 s,
// valid 180 s, one DNS server.
func serverReply(msg []byte, typ dhcpv6.MessageType, rapid bool) []byte {
	clientID, _ := find(msg, optClientID)
	ia := binary.BigEndian.AppendUint32(nil, 7)
	ia = binary.BigEndian.AppendUint32(ia, 60)
	ia = binary.BigEndian.AppendUint32(ia, 96)
	addr := append(append([]byte(nil), testAddr.To16()...), 0, 0, 0, 120, 0, 0, 0, 180)
	ia = binary.BigEndian.AppendUint16(ia, uint16(optIAAddr))
	ia = binary.BigEndian.AppendUint16(ia, uint16(len(addr)))
	ia = append(ia, addr...)
	opts := []dhcpv6.Option{raw(optServerID, testServer), raw(optClientID, clientID), raw(optIANA, ia), raw(optDNSServers, net.ParseIP("fd00:10::1").To16())}
	if rapid {
		opts = append(opts, raw(optRapidCommit, nil))
	}
	return encode(typ, xidOf(msg), opts)
}

func codes(msg []byte) []uint16 {
	out := make([]uint16, 0, 8)
	for _, o := range options(msg) {
		out = append(out, uint16(o.Code()))
	}
	return out
}

// options parses the options of msg.
func options(msg []byte) dhcpv6.Options {
	var opts dhcpv6.Options
	_ = opts.FromBytesWithParser(msg[4:], lenient)
	return opts
}

// find returns the payload of the first option of msg with code.
func find(msg []byte, code dhcpv6.OptionCode) ([]byte, bool) {
	o := options(msg).GetOne(code)
	if o == nil {
		return nil, false
	}
	return o.ToBytes(), true
}

// TestSolicitLikeNetworkd: networkd's SOLICIT and its retransmissions
// (1, 2, 4 s); the REPLY with rapid commit binds the address.
func TestSolicitLikeNetworkd(t *testing.T) {
	f := newFakeNet()
	solicits := 0
	f.answer = func(msg []byte) []byte {
		if dhcpv6.MessageType(msg[0]) == msgSolicit {
			if solicits++; solicits > 3 {
				return serverReply(msg, msgReply, true)
			}
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client(true)
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	l := <-bound
	close(f.stopped)
	c.Stop()

	if !l.Addr.Equal(testAddr) || l.Valid != 180*time.Second || l.Renew.Sub(l.Bound) != 60*time.Second || len(l.DNS) != 1 {
		t.Fatalf("lease = %+v", l)
	}
	msgs := f.messages()
	s := msgs[0]
	if got := codes(s); !slices.Equal(got, []uint16{14, 3, 25, 39, 6, 1, 8}) {
		t.Errorf("SOLICIT options = %v, want networkd's 14,3,25,39,6,1,8", got)
	}
	o, _ := find(s, dhcpv6.OptionORO)
	if !bytes.Equal(o, []byte{0, 23, 0, 31, 0, 56, 0, 82, 0, 103}) {
		t.Errorf("ORO = %x, want 23,31,56,82,103", o)
	}
	fq, _ := find(s, optClientFQDN)
	if !bytes.Equal(fq, append([]byte{1, 14}, "e2e-test-rfm-0"...)) {
		t.Errorf("FQDN = %x, want flags 01 and the partial host name", fq)
	}
	f.mu.Lock()
	waits := slices.Clone(f.waits)
	f.mu.Unlock()
	// The first SOLICIT timeout is strictly greater than IRT (RAND 0 counts as 0.01).
	if want := []time.Duration{1010 * time.Millisecond, 2020 * time.Millisecond, 4040 * time.Millisecond}; !slices.Equal(waits[:3], want) {
		t.Errorf("waits = %v, want %v", waits, want)
	}
}

// TestRenewRebindExpire: RENEW (2,3,39,6,1,8) from T1 to T2, REBIND
// (3,39,6,1,8) to the end of the valid lifetime, then OnExpire and a new
// SOLICIT.
func TestRenewRebindExpire(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		if dhcpv6.MessageType(msg[0]) == msgSolicit {
			return serverReply(msg, msgReply, true)
		}
		return nil
	}
	bound, expired := make(chan *Lease, 4), make(chan *Lease, 1)
	c := f.client(true)
	c.OnBound = func(l *Lease) { bound <- l }
	c.OnExpire = func(l *Lease) { expired <- l }
	c.Start()
	<-bound
	f.tick(t)
	<-expired
	<-bound
	close(f.stopped)
	c.Stop()
	var renew, rebind []byte
	for _, m := range f.messages() {
		switch dhcpv6.MessageType(m[0]) {
		case msgRenew:
			renew = m
		case msgRebind:
			rebind = m
		}
	}
	if renew == nil || !slices.Equal(codes(renew), []uint16{2, 3, 39, 6, 1, 8}) {
		t.Errorf("RENEW options = %v, want 2,3,39,6,1,8", codes(renew))
	}
	if rebind == nil || !slices.Equal(codes(rebind), []uint16{3, 39, 6, 1, 8}) {
		t.Errorf("REBIND options = %v, want 3,39,6,1,8", codes(rebind))
	}
	o, _ := find(renew, dhcpv6.OptionORO)
	if !bytes.Equal(o, []byte{0, 23, 0, 31, 0, 56, 0, 103}) {
		t.Errorf("RENEW ORO = %x, want 23,31,56,103", o)
	}
}

// TestAdvertiseRequestAndRelease: without rapid commit the ADVERTISE leads
// to a REQUEST; Stop releases the address (2,3,1,8) and reads the REPLY.
func TestAdvertiseRequestAndRelease(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		switch dhcpv6.MessageType(msg[0]) {
		case msgSolicit:
			return serverReply(msg, msgAdvertise, false)
		case msgRequest, msgRelease:
			return serverReply(msg, msgReply, false)
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client(true)
	c.SendRelease = true
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	<-bound
	close(f.stopped)
	c.Stop()
	msgs := f.messages()
	if len(msgs) != 3 || dhcpv6.MessageType(msgs[1][0]) != msgRequest || dhcpv6.MessageType(msgs[2][0]) != msgRelease {
		t.Fatalf("messages = %v, want SOLICIT, REQUEST, RELEASE", msgs)
	}
	if got := codes(msgs[2]); !slices.Equal(got, []uint16{2, 3, 1, 8}) {
		t.Errorf("RELEASE options = %v, want networkd's 2,3,1,8", got)
	}
	if sid, _ := find(msgs[1], optServerID); !bytes.Equal(sid, testServer) {
		t.Errorf("REQUEST server ID = %x", sid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) != 0 {
		t.Error("the RELEASE REPLY was not read: the socket closed before it came")
	}
}

// Without the M flag the client sends INFORMATION-REQUESTs (6,1,8) and
// reports the options of the REPLY.
func TestInformationRequest(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		if dhcpv6.MessageType(msg[0]) == msgInformationRequest {
			return serverReply(msg, msgReply, false)
		}
		return nil
	}
	bound := make(chan *Lease, 1)
	c := f.client(false)
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	l := <-bound
	close(f.stopped)
	c.Stop()
	if got := codes(f.messages()[0]); !slices.Equal(got, []uint16{6, 1, 8}) {
		t.Errorf("INFORMATION-REQUEST options = %v", got)
	}
	if len(l.DNS) != 1 {
		t.Errorf("DNS = %v", l.DNS)
	}
}

func TestDomainList(t *testing.T) {
	b := []byte{1, 'a', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 1, 'b', 0}
	if got := domainList(b); !slices.Equal(got, []string{"a.example", "b"}) {
		t.Fatalf("domainList = %v", got)
	}
	// networkd 255 dhcp6_option_parse_domainname_list: a name may end at
	// the end of the option when the last byte is 0; one byte after a
	// name, a label over 63 bytes or past the end, a pointer, or a last
	// byte other than 0 ignore the list.
	for _, tt := range []struct {
		b    []byte
		want []string
	}{
		{[]byte{1, 'a', 0, 2, 'b', 0}, []string{"a", "b\x00"}},
		{[]byte{0, 1, 'a', 0}, []string{"a"}},
		{[]byte{1, 'a', 0, 0}, nil},
		{[]byte{1, 'a'}, nil},
		{[]byte{1, 'a', 0xc0, 0}, nil},
		{[]byte{3, 'a', 0}, nil},
		{append([]byte{64}, make([]byte, 65)...), nil},
	} {
		if got := domainList(tt.b); !slices.Equal(got, tt.want) {
			t.Errorf("domainList(%x) = %q, want %q", tt.b, got, tt.want)
		}
	}
}

// RFC 8415 18.2.1: the first SOLICIT RT is above IRT even with a negative
// RAND; other first RTs use RAND as it is.
func TestFirstSolicitAboveIRT(t *testing.T) {
	c := &Client{rand: func() float64 { return -0.1 }}
	if got := c.next(solicitRT, 0); got != 1100*time.Millisecond {
		t.Errorf("first SOLICIT RT = %s, want 1.1s", got)
	}
	if got := c.next(requestRT, 0); got != 900*time.Millisecond {
		t.Errorf("first REQUEST RT = %s, want 0.9s", got)
	}
}

// replyFrom is serverReply from server with the extra options.
func replyFrom(msg []byte, typ dhcpv6.MessageType, server []byte, extra ...dhcpv6.Option) []byte {
	opts := options(serverReply(msg, typ, false))
	opts.Update(raw(optServerID, server))
	return encode(typ, xidOf(msg), append(opts, extra...))
}

func pref(p byte) dhcpv6.Option { return raw(optPreference, []byte{p}) }

// RFC 8415 18.2.9: the ADVERTISEs of the first RT are collected and the
// one with the highest preference wins; preference 255, or an ADVERTISE
// after the first RT, is taken at once.
func TestAdvertisePreference(t *testing.T) {
	a, b, cs := []byte{0, 1, 0, 1, 'a'}, []byte{0, 1, 0, 1, 'b'}, []byte{0, 1, 0, 1, 'c'}
	for _, tt := range []struct {
		name   string
		advs   func(n int, msg []byte) [][]byte // the ADVERTISEs to SOLICIT n
		server []byte
		after  time.Duration // REQUEST time after the start
	}{
		{"highest preference of the first RT", func(n int, msg []byte) [][]byte {
			return [][]byte{replyFrom(msg, msgAdvertise, a, pref(5)), replyFrom(msg, msgAdvertise, b, pref(10)), replyFrom(msg, msgAdvertise, cs)}
		}, b, 1010 * time.Millisecond},
		{"preference 255 at once", func(n int, msg []byte) [][]byte {
			return [][]byte{replyFrom(msg, msgAdvertise, a, pref(10)), replyFrom(msg, msgAdvertise, b, pref(255)), replyFrom(msg, msgAdvertise, cs, pref(255))}
		}, b, 0},
		{"after the first RT at once", func(n int, msg []byte) [][]byte {
			if n == 1 {
				return nil
			}
			return [][]byte{replyFrom(msg, msgAdvertise, a)}
		}, a, 1010 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeNet()
			start := f.now
			var solicits int
			var reqAt time.Time
			f.answer = func(msg []byte) []byte {
				switch dhcpv6.MessageType(msg[0]) {
				case msgSolicit:
					solicits++
					advs := tt.advs(solicits, msg)
					if len(advs) == 0 {
						return nil
					}
					f.queue = append(f.queue, advs[1:]...)
					return advs[0]
				case msgRequest:
					if reqAt.IsZero() {
						reqAt = f.now
					}
					return serverReply(msg, msgReply, false)
				}
				return nil
			}
			bound := make(chan *Lease, 1)
			c := f.client(true)
			c.OnBound = func(l *Lease) { bound <- l }
			c.Start()
			select {
			case <-bound:
			case <-time.After(5 * time.Second):
				t.Fatal("no lease")
			}
			close(f.stopped)
			c.Stop()
			var req []byte
			for _, m := range f.messages() {
				if dhcpv6.MessageType(m[0]) == msgRequest {
					req = m
					break
				}
			}
			if sid, _ := find(req, optServerID); !bytes.Equal(sid, tt.server) {
				t.Errorf("REQUEST to server %q, want %q", sid, tt.server)
			}
			if got := reqAt.Sub(start); got != tt.after {
				t.Errorf("REQUEST after %s, want %s", got, tt.after)
			}
		})
	}
}

// socketTransport keeps its own replies: a reply that arrives after a
// receive timed out waits on this socket only.
type socketTransport struct {
	f     *fakeNet
	queue [][]byte
	late  []byte
}

func (s *socketTransport) send(msg []byte) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.sent = append(s.f.sent, msg)
	switch {
	case dhcpv6.MessageType(msg[0]) == msgSolicit && len(s.f.sent) == 1:
		s.late = serverReply(msg, msgAdvertise, false) // after the first RT
	case dhcpv6.MessageType(msg[0]) == msgRequest:
		s.queue = append(s.queue, serverReply(msg, msgReply, false))
	}
	return nil
}

func (s *socketTransport) receive(timeout time.Duration) ([]byte, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if len(s.queue) > 0 {
		r := s.queue[0]
		s.queue = s.queue[1:]
		return r, nil
	}
	s.f.now = s.f.now.Add(timeout)
	if s.late != nil {
		s.queue, s.late = append(s.queue, s.late), nil
	}
	return nil, errTimeout
}

func (s *socketTransport) close() error { return nil }

// An ADVERTISE that answers the first SOLICIT after its RT is read on the
// same socket during the retransmission: one socket per transaction.
func TestLateAdvertiseSameSocket(t *testing.T) {
	f := newFakeNet()
	var opens int
	bound := make(chan *Lease, 1)
	c := f.client(true)
	c.newTransport = func(*net.Interface) (transport, error) {
		f.mu.Lock()
		opens++
		f.mu.Unlock()
		return &socketTransport{f: f}, nil
	}
	c.OnBound = func(l *Lease) { bound <- l }
	c.Start()
	select {
	case <-bound:
	case <-time.After(5 * time.Second):
		t.Fatal("the late ADVERTISE was lost")
	}
	close(f.stopped)
	c.Stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	if opens != 1 {
		t.Errorf("sockets opened = %d, want 1 for the transaction", opens)
	}
}

func (f *fakeNet) countType(typ dhcpv6.MessageType) int {
	n := 0
	for _, m := range f.messages() {
		if dhcpv6.MessageType(m[0]) == typ {
			n++
		}
	}
	return n
}

// otherServer is a second DHCPv6 server.
var otherServer = []byte{0, 1, 0, 1, 8, 8, 8, 8}

// noBindingFrom is a REPLY of server whose IA_NA has the status NoBinding.
func noBindingFrom(msg, server []byte) []byte {
	opts := options(noBinding(msg))
	opts.Update(raw(optServerID, server))
	return encode(msgReply, xidOf(msg), opts)
}

// noBinding is a REPLY whose IA_NA has the status NoBinding.
func noBinding(msg []byte) []byte {
	clientID, _ := find(msg, optClientID)
	ia := binary.BigEndian.AppendUint32(nil, 7)
	ia = append(ia, make([]byte, 8)...)
	ia = append(ia, 0, byte(optStatusCode), 0, 2, 0, byte(iana.StatusNoBinding))
	return encode(msgReply, xidOf(msg), []dhcpv6.Option{raw(optServerID, testServer), raw(optClientID, clientID), raw(optIANA, ia)})
}

// RFC 8415 18.2.10.1: NoBinding in the REPLY to a RENEW makes the client
// send a REQUEST for the address; when the server has no binding for that
// either, the lease ends at once (networkd) instead of rebinding.
func TestRenewNoBindingRequests(t *testing.T) {
	for _, answered := range []bool{true, false} {
		f := newFakeNet()
		f.answer = func(msg []byte) []byte {
			switch dhcpv6.MessageType(msg[0]) {
			case msgSolicit:
				return serverReply(msg, msgReply, true)
			case msgRenew:
				return noBindingFrom(msg, otherServer)
			case msgRequest:
				if answered {
					return serverReply(msg, msgReply, false)
				}
				return noBinding(msg)
			}
			return nil
		}
		bound, expired := make(chan *Lease, 4), make(chan *Lease, 1)
		c := f.client(true)
		c.OnBound = func(l *Lease) { bound <- l }
		c.OnExpire = func(l *Lease) { expired <- l }
		c.Start()
		<-bound
		f.tick(t)
		want := bound
		if !answered {
			want = expired // and then a new SOLICIT binds again
		}
		select {
		case <-want:
		case <-time.After(5 * time.Second):
			t.Fatalf("answered %v: no lease or expiry after NoBinding", answered)
		}
		if answered && len(expired) != 0 {
			t.Fatal("the lease expired although the REQUEST was answered")
		}
		close(f.stopped)
		c.Stop()
		var types []dhcpv6.MessageType
		for _, m := range f.messages() {
			if typ := dhcpv6.MessageType(m[0]); len(types) == 0 || types[len(types)-1] != typ {
				types = append(types, typ)
			}
		}
		if len(types) < 3 || types[1] != msgRenew || types[2] != msgRequest || slices.Contains(types, msgRebind) || f.countType(msgRequest) != 1 {
			t.Fatalf("answered %v: message types %v, want SOLICIT, RENEW, one REQUEST and no REBIND", answered, types)
		}
		req := f.messages()[2]
		if ia, _ := find(req, optIANA); !bytes.Contains(ia, testAddr.To16()) {
			t.Errorf("REQUEST IA_NA %x without the address", ia)
		}
		if sid, _ := find(req, optServerID); !bytes.Equal(sid, otherServer) {
			t.Errorf("REQUEST to server %x, want %x that answered NoBinding", sid, otherServer)
		}
	}
}

// A stateless client waits the information refresh time of the REPLY
// (option 32, at least 600 s) before it asks again; a day without it.
func TestInformationWaitsRefreshTime(t *testing.T) {
	for _, tt := range []struct {
		opt  []byte
		want time.Duration
	}{{[]byte{0, 0, 0x0e, 0x10}, time.Hour}, {[]byte{0, 0, 0, 0}, 600 * time.Second}, {nil, 24 * time.Hour}} {
		f := newFakeNet()
		f.answer = func(msg []byte) []byte {
			if dhcpv6.MessageType(msg[0]) != msgInformationRequest {
				return nil
			}
			r := serverReply(msg, msgReply, false)
			if tt.opt != nil {
				r = encode(msgReply, xidOf(msg), append(options(r), raw(optInfoRefresh, tt.opt)))
			}
			return r
		}
		bound := make(chan *Lease, 2)
		c := f.client(false)
		c.OnBound = func(l *Lease) { bound <- l }
		c.Start()
		<-bound
		var waits []time.Duration
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			f.mu.Lock()
			waits = slices.Clone(f.waits)
			f.mu.Unlock()
			if len(waits) > 0 {
				break
			}
		}
		close(f.stopped)
		c.Stop()
		if !slices.Equal(waits, []time.Duration{tt.want}) {
			t.Errorf("option 32 %x: waits %v, want %s", tt.opt, waits, tt.want)
		}
	}
}

// The REQUEST after NoBinding stops at the end of the valid lifetime of
// the address (180 s; NoBinding at T1, 60 s): eight REQUESTs fit, not ten.
func TestNoBindingRequestBoundedByLifetime(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		switch dhcpv6.MessageType(msg[0]) {
		case msgSolicit:
			return serverReply(msg, msgReply, true)
		case msgRenew:
			return noBinding(msg)
		}
		return nil
	}
	bound, expired := make(chan *Lease, 4), make(chan *Lease, 1)
	c := f.client(true)
	c.OnBound = func(l *Lease) { bound <- l }
	c.OnExpire = func(l *Lease) { expired <- l }
	c.Start()
	l := <-bound
	f.tick(t)
	select {
	case <-expired:
	case <-time.After(5 * time.Second):
		t.Fatal("no expiry")
	}
	f.mu.Lock()
	now := f.now
	f.mu.Unlock()
	close(f.stopped)
	c.Stop()
	if n := f.countType(msgRequest); n != 8 {
		t.Errorf("%d REQUESTs after NoBinding, want 8 within the valid lifetime", n)
	}
	if now.After(l.Expire) {
		t.Errorf("the lease ended at %s, after its expiry %s", now, l.Expire)
	}
}

// countTransport counts the sockets that are open.
type countTransport struct {
	fakeTransport
	open *atomic.Int32
}

func (c *countTransport) close() error { c.open.Add(-1); return nil }

// A stateless client closes its socket before the information refresh
// wait.
func TestInformationClosesSocketBeforeWait(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		if dhcpv6.MessageType(msg[0]) == msgInformationRequest {
			return serverReply(msg, msgReply, false)
		}
		return nil
	}
	var open atomic.Int32
	c := f.client(false)
	c.newTransport = func(*net.Interface) (transport, error) {
		open.Add(1)
		return &countTransport{fakeTransport{f: f}, &open}, nil
	}
	openAtWait := make(chan int32, 1)
	after := c.after
	c.after = func(d time.Duration) <-chan time.Time {
		select {
		case openAtWait <- open.Load():
		default:
		}
		return after(d)
	}
	c.Start()
	var got int32
	select {
	case got = <-openAtWait:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh wait")
	}
	close(f.stopped)
	c.Stop()
	if got != 0 {
		t.Fatalf("%d sockets open during the information refresh wait, want 0", got)
	}
}
