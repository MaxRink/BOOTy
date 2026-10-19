package network

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// dnsAnswer answers query q: rcode, an A record of ip unless nil, TC when
// truncated. The EDNS record of the query is dropped.
func dnsAnswer(q []byte, rcode byte, ip net.IP, truncated bool) []byte {
	end := 12 // the question: the name, type and class
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	resp := append([]byte(nil), q[:end+5]...)
	resp[2], resp[3] = 0x81, 0x80|rcode // response, recursion
	if truncated {
		resp[2] |= 0x02
	}
	resp[6], resp[7], resp[10], resp[11] = 0, 0, 0, 0
	if ip != nil {
		resp[7] = 1
		resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
		resp = append(resp, ip.To4()...)
	}
	return resp
}

// udpDNS serves on host:port; answer returns the datagrams to send back
// for a query, after delay.
func udpDNS(t *testing.T, host, port string, delay time.Duration, queries *atomic.Int32, answer func(q []byte) [][]byte) {
	t.Helper()
	pc, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", net.JoinHostPort(host, port))
	if err != nil {
		t.Skipf("listen on %s: %v", host, err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			queries.Add(1)
			q := append([]byte(nil), buf[:n]...)
			go func() {
				time.Sleep(delay)
				for _, r := range answer(q) {
					_, _ = pc.WriteTo(r, from)
				}
			}()
		}
	}()
}

// freePort returns a UDP port that is free on 127.0.0.1.
func freePort(t *testing.T) string {
	t.Helper()
	probe, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(probe.LocalAddr().String())
	_ = probe.Close()
	return port
}

func fanoutTest(t *testing.T, links [][]string) (*fanout, *net.Resolver, string) {
	t.Helper()
	port := freePort(t)
	origPort, origLinger := dnsPort, fanoutLinger
	t.Cleanup(func() { dnsPort, fanoutLinger = origPort, origLinger })
	dnsPort, fanoutLinger = port, 100*time.Millisecond
	f := &fanout{}
	f.set(links)
	return f, &net.Resolver{PreferGo: true, Dial: f.dial}, port
}

func lookup(t *testing.T, r *net.Resolver, timeout time.Duration) ([]net.IP, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.LookupIP(ctx, "ip4", "image.l2-parity.test.") //nolint:wrapcheck // test
}

// A lookup asks every link at once and takes the first answer: the first
// server never answers, the second does.
func TestDNSFanout(t *testing.T) {
	_, r, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var silent, answering atomic.Int32
	udpDNS(t, "127.0.0.1", port, 0, &silent, func([]byte) [][]byte { return nil })
	udpDNS(t, "127.0.0.2", port, 0, &answering, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 0, net.IPv4(192, 0, 2, 7), false)} })
	start := time.Now()
	ips, err := lookup(t, r, 3*time.Second)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(192, 0, 2, 7)) {
		t.Fatalf("LookupIP = %v, %v", ips, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the lookup took %s: it waited for the silent first server", took)
	}
	for deadline := time.Now().Add(time.Second); silent.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond) // the silent server counts concurrently
	}
	if silent.Load() == 0 || answering.Load() == 0 {
		t.Fatalf("queries: silent %d, answering %d; want both asked", silent.Load(), answering.Load())
	}
}

// A fast NXDOMAIN of one link does not beat a later answer of another,
// like resolved (dns_query_ready).
func TestDNSFanoutPrefersSuccess(t *testing.T) {
	_, r, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var a, b atomic.Int32
	udpDNS(t, "127.0.0.1", port, 0, &a, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 3, nil, false)} })
	udpDNS(t, "127.0.0.2", port, 100*time.Millisecond, &b, func(q []byte) [][]byte {
		return [][]byte{dnsAnswer(q, 0, net.IPv4(192, 0, 2, 7), false)}
	})
	ips, err := lookup(t, r, 3*time.Second)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(192, 0, 2, 7)) {
		t.Fatalf("LookupIP = %v, %v; want the answer of the slower link", ips, err)
	}
}

// One link refuses (no server), the other sends one datagram of another
// query ID and then nothing: the lookup ends at its deadline instead of
// hanging.
func TestDNSFanoutDeadline(t *testing.T) {
	_, r, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var b atomic.Int32
	udpDNS(t, "127.0.0.2", port, 0, &b, func(q []byte) [][]byte {
		bad := dnsAnswer(q, 0, net.IPv4(192, 0, 2, 7), false)
		binary.BigEndian.PutUint16(bad, binary.BigEndian.Uint16(bad)+1)
		return [][]byte{bad}
	})
	done := make(chan error, 1)
	go func() {
		_, err := lookup(t, r, time.Second)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a lookup without an answer succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup hangs past its deadline")
	}
}

// A truncated answer: the resolver asks again over TCP, and the fan-out
// sends it to the server that truncated.
func TestDNSFanoutTruncatedGoesToItsServer(t *testing.T) {
	_, r, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var a, b atomic.Int32
	udpDNS(t, "127.0.0.1", port, 0, &a, func([]byte) [][]byte { return nil })
	udpDNS(t, "127.0.0.2", port, 0, &b, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 0, nil, true)} })
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Skipf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var n [2]byte
				if _, err := io.ReadFull(c, n[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(n[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp := dnsAnswer(q, 0, net.IPv4(192, 0, 2, 9), false)
				_, _ = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(resp))), resp...)) //nolint:gosec // short
			}()
		}
	}()
	ips, err := lookup(t, r, 3*time.Second)
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(192, 0, 2, 9)) {
		t.Fatalf("LookupIP = %v, %v; want the TCP answer of the truncating server", ips, err)
	}
}

// A link moves to its next server after a failure.
func TestDNSFanoutFailover(t *testing.T) {
	f := &fanout{}
	f.set([][]string{{"10.0.0.1", "10.0.0.2"}, {"10.0.1.1"}})
	f.failed(0, "10.0.0.1")
	f.failed(0, "10.0.0.1") // a second report of the same failure changes nothing
	if got, _, _ := f.current(); !slices.Equal(got, []string{"10.0.0.2", "10.0.1.1"}) {
		t.Fatalf("current = %v", got)
	}
	f.set(nil)
	if got, _, _ := f.current(); got != nil {
		t.Fatalf("current after reset = %v", got)
	}
}

// countingDial counts the Reads of the fan-out conns of r.
func countingDial(f *fanout, reads *atomic.Int64) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, nw, a string) (net.Conn, error) {
		c, err := f.dial(ctx, nw, a)
		if fc, ok := c.(*fanoutConn); ok {
			return countConn{fc, reads}, err
		}
		return c, err
	}}
}

type countConn struct {
	*fanoutConn
	reads *atomic.Int64
}

func (c countConn) Read(b []byte) (int, error) {
	c.reads.Add(1)
	return c.fanoutConn.Read(b) //nolint:wrapcheck // test
}

func (c countConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := c.Read(b)
	return n, nil, err
}

// One link answers FORMERR without a question (EDNS intolerant), the other
// NXDOMAIN 50 ms later: the resolver rejects the FORMERR and reads again,
// and gets the NXDOMAIN. Each negative answer comes once.
func TestDNSFanoutRejectedNegativeThenNext(t *testing.T) {
	f, _, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var a, b atomic.Int32
	udpDNS(t, "127.0.0.1", port, 0, &a, func(q []byte) [][]byte {
		h := append([]byte(nil), q[:12]...)
		h[2], h[3] = 0x81, 0x81 // response, FORMERR
		clear(h[4:12])
		return [][]byte{h}
	})
	udpDNS(t, "127.0.0.2", port, 50*time.Millisecond, &b, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 3, nil, false)} })
	var reads atomic.Int64
	r := countingDial(f, &reads)
	done := make(chan error, 1)
	go func() {
		_, err := r.LookupIP(context.Background(), "ip4", "image.l2-parity.test.")
		done <- err
	}()
	select {
	case err := <-done:
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound {
			t.Fatalf("lookup = %v, want not found", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("the lookup never ends; %d Reads", reads.Load())
	}
}

// Junk of three bytes with the query ID and a refusing link: the lookup
// ends at its deadline and the resolver does not read in a loop.
func TestDNSFanoutJunkDoesNotSpin(t *testing.T) {
	f, _, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var n atomic.Int32
	udpDNS(t, "127.0.0.2", port, 0, &n, func(q []byte) [][]byte { return [][]byte{{q[0], q[1], 0x81}} })
	var reads atomic.Int64
	r := countingDial(f, &reads)
	done := make(chan error, 1)
	go func() {
		_, err := lookup(t, r, time.Second)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a lookup with junk succeeded")
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("the lookup hangs; %d Reads", reads.Load())
	}
	if got := reads.Load(); got > 100 {
		t.Fatalf("%d Reads for one lookup: Read spins", got)
	}
}

// fanoutConn on its own: Read ends at the deadline, Reads after it do not
// return a held answer again, and Close closes the sockets after the
// linger time.
func TestFanoutConnDeadlineAndClose(t *testing.T) {
	f, _, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var n atomic.Int32
	udpDNS(t, "127.0.0.2", port, 0, &n, func(q []byte) [][]byte { return [][]byte{{q[0], q[1], 0x81}} })
	c, err := f.dial(context.Background(), "udp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	fc := c.(*fanoutConn)
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1}
	start := time.Now()
	_ = c.SetDeadline(start.Add(300 * time.Millisecond))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	var errs int
	for range 5 {
		if _, err := c.Read(buf); err != nil {
			errs++
		}
	}
	if took := time.Since(start); took > 800*time.Millisecond {
		t.Fatalf("Reads took %s past the 300 ms deadline", took)
	}
	if errs < 4 {
		t.Fatalf("%d of 5 Reads failed: a held answer comes again", errs)
	}
	_ = c.Close()
	time.Sleep(300 * time.Millisecond) // linger 100 ms
	for _, l := range fc.conns {
		if _, err := l.conn.Write(q); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("socket of %s open after Close: %v", l.server, err)
		}
	}
}

// failWrite is a conn whose Write fails; Read waits for Close.
type failWrite struct{ net.Conn }

func (failWrite) Write([]byte) (int, error) { return 0, errors.New("network unreachable") }

// A link whose Write fails counts as answered: the NXDOMAIN of the other
// comes without waiting for the deadline.
func TestFanoutConnWriteFailureDoesNotWait(t *testing.T) {
	f, _, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var n atomic.Int32
	udpDNS(t, "127.0.0.2", port, 0, &n, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 3, nil, false)} })
	p, _ := net.Pipe()
	udp, err := (&net.Dialer{}).DialContext(context.Background(), "udp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Fatal(err)
	}
	c := &fanoutConn{f: f, linger: 0, replies: make(chan fanoutReply, 8), done: make(chan struct{}),
		conns: []fanoutLink{{conn: failWrite{p}, link: 0, server: "127.0.0.1"}, {conn: udp, link: 1, server: "127.0.0.2"}}}
	defer c.Close()
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1}
	start := time.Now()
	_ = c.SetDeadline(start.Add(3 * time.Second))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	nr, err := c.Read(buf)
	if err != nil || nr < 4 || buf[3]&0x0f != 3 {
		t.Fatalf("Read = %d, %v; want the NXDOMAIN", nr, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the NXDOMAIN came after %s: Read waited for the failed link", took)
	}
}

// The server for the TCP retry is kept per query ID: concurrent truncated
// lookups each go to their own server.
func TestDNSFanoutTCPServerPerQuery(t *testing.T) {
	f := &fanout{}
	f.set([][]string{{"10.0.0.1"}, {"10.0.1.1"}})
	f.truncated(1, "10.0.0.1")
	f.truncated(2, "10.0.1.1")
	for id, want := range map[uint16]string{2: "10.0.1.1", 1: "10.0.0.1"} {
		if got, _, ok := f.tcpServer(id); !ok || got != want {
			t.Errorf("tcpServer(%d) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if _, _, ok := f.tcpServer(1); ok {
		t.Error("a TCP server is used twice")
	}
}

// The fan-out is the process resolver from the start.
func TestDNSFanoutIsDefaultResolver(t *testing.T) {
	if !net.DefaultResolver.PreferGo || net.DefaultResolver.Dial == nil {
		t.Fatal("net.DefaultResolver is not the fan-out")
	}
}

// One link answers NXDOMAIN, the other is a silent listener: at the 300 ms
// deadline the first Read returns the held NXDOMAIN (the silent link counts
// as answered then), and a second Read ends with os.ErrDeadlineExceeded.
func TestFanoutConnDeadlineReturnsHeldNegativeOnce(t *testing.T) {
	f, _, port := fanoutTest(t, [][]string{{"127.0.0.1"}, {"127.0.0.2"}})
	var a, b atomic.Int32
	udpDNS(t, "127.0.0.1", port, 0, &a, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 3, nil, false)} })
	udpDNS(t, "127.0.0.2", port, 0, &b, func([]byte) [][]byte { return nil })
	c, err := f.dial(context.Background(), "udp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1}
	_ = c.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || n < 4 || buf[3]&0x0f != 3 {
		t.Fatalf("first Read = %d, %v; want the NXDOMAIN at the deadline", n, err)
	}
	if _, err := c.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("second Read = %v, want os.ErrDeadlineExceeded", err)
	}
}
