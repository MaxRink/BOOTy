package dhcp6

import (
	"bytes"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
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
func serverReply(msg []byte, typ byte, rapid bool) []byte {
	clientID, _ := find(parseOptions(msg[4:]), optClientID)
	ia := binary.BigEndian.AppendUint32(nil, 7)
	ia = binary.BigEndian.AppendUint32(ia, 60)
	ia = binary.BigEndian.AppendUint32(ia, 96)
	addr := append(append([]byte(nil), testAddr.To16()...), 0, 0, 0, 120, 0, 0, 0, 180)
	ia = binary.BigEndian.AppendUint16(ia, optIAAddr)
	ia = binary.BigEndian.AppendUint16(ia, uint16(len(addr)))
	ia = append(ia, addr...)
	opts := []option{{optServerID, testServer}, {optClientID, clientID}, {optIANA, ia}, {optDNSServers, net.ParseIP("fd00:10::1").To16()}}
	if rapid {
		opts = append(opts, option{optRapidCommit, nil})
	}
	return encode(typ, xidOf(msg), opts)
}

func codes(msg []byte) []uint16 {
	out := make([]uint16, 0, 8)
	for _, o := range parseOptions(msg[4:]) {
		out = append(out, o.code)
	}
	return out
}

// TestSolicitLikeNetworkd: networkd's SOLICIT and its retransmissions
// (1, 2, 4 s); the REPLY with rapid commit binds the address.
func TestSolicitLikeNetworkd(t *testing.T) {
	f := newFakeNet()
	solicits := 0
	f.answer = func(msg []byte) []byte {
		if msg[0] == msgSolicit {
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
	o, _ := find(parseOptions(s[4:]), optORO)
	if !bytes.Equal(o, []byte{0, 23, 0, 31, 0, 56, 0, 82, 0, 103}) {
		t.Errorf("ORO = %x, want 23,31,56,82,103", o)
	}
	fq, _ := find(parseOptions(s[4:]), optClientFQDN)
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
		if msg[0] == msgSolicit {
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
		switch m[0] {
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
	o, _ := find(parseOptions(renew[4:]), optORO)
	if !bytes.Equal(o, []byte{0, 23, 0, 31, 0, 56, 0, 103}) {
		t.Errorf("RENEW ORO = %x, want 23,31,56,103", o)
	}
}

// TestAdvertiseRequestAndRelease: without rapid commit the ADVERTISE leads
// to a REQUEST; Stop releases the address (2,3,1,8) and reads the REPLY.
func TestAdvertiseRequestAndRelease(t *testing.T) {
	f := newFakeNet()
	f.answer = func(msg []byte) []byte {
		switch msg[0] {
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
	if len(msgs) != 3 || msgs[1][0] != msgRequest || msgs[2][0] != msgRelease {
		t.Fatalf("messages = %v, want SOLICIT, REQUEST, RELEASE", msgs)
	}
	if got := codes(msgs[2]); !slices.Equal(got, []uint16{2, 3, 1, 8}) {
		t.Errorf("RELEASE options = %v, want networkd's 2,3,1,8", got)
	}
	if sid, _ := find(parseOptions(msgs[1][4:]), optServerID); !bytes.Equal(sid, testServer) {
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
		if msg[0] == msgInformationRequest {
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
