package network

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// dnsFanout makes the DNS lookups of the process ask the current DNS
// server of every link at once, like systemd-resolved in the legacy
// ramdisk: the first positive answer counts, a negative one (NXDOMAIN,
// SERVFAIL, REFUSED) only when no link answers better. The Go resolver
// alone asks the first resolv.conf server and the next only after a
// timeout. DHCP mode sets the links whenever it writes resolv.conf.
//
// It is the process resolver from the start (set before any lookup, no
// data race); without at least two links it dials like the Go resolver.
var dnsFanout fanout

func init() {
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: dnsFanout.dial}
}

// dnsPort is the server port; tests change it.
var dnsPort = "53"

// fanoutLinger keeps the sockets of a lookup open after the answer, so the
// answers of the other servers find a socket (no ICMP port unreachable);
// systemd-resolved keeps its transactions open as well.
var fanoutLinger = 2 * time.Second

type fanout struct {
	mu    sync.Mutex
	links [][]string // per link its servers in order
	cur   []int      // per link the server asked now; a failure moves on
	// tcp is per query ID the server that truncated its answer; the
	// resolver asks it again over TCP with the same ID.
	tcp  map[uint16]string
	port string // dnsPort at set
	// linger is fanoutLinger at set.
	linger time.Duration
}

// set replaces the links (each a list of servers). Nil goes back to the
// plain Go resolver (Teardown).
func (f *fanout) set(links [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links, f.cur, f.tcp, f.port, f.linger = links, make([]int, len(links)), nil, dnsPort, fanoutLinger
}

// current returns the server asked now on every link.
func (f *fanout) current() (servers []string, links []int, port string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	port = f.port
	for i, l := range f.links {
		if len(l) > 0 {
			servers = append(servers, l[f.cur[i]%len(l)])
			links = append(links, i)
		}
	}
	return servers, links, port
}

// failed moves a link to its next server for the next lookup.
func (f *fanout) failed(link int, server string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if link < len(f.links) && len(f.links[link]) > 0 && f.links[link][f.cur[link]%len(f.links[link])] == server {
		f.cur[link]++
	}
}

func (f *fanout) truncated(id uint16, server string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tcp == nil || len(f.tcp) >= 256 { // ponytail: lookups that never asked over TCP are dropped in bulk
		f.tcp = map[uint16]string{}
	}
	f.tcp[id] = server
}

// tcpServer returns (and forgets) the server that truncated the answer to
// the query with id.
func (f *fanout) tcpServer(id uint16) (server, port string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	server, ok = f.tcp[id]
	delete(f.tcp, id)
	return server, f.port, ok
}

// dial gives the Go resolver one connection for a query to address. Over
// UDP with two links or more it is a fan-out to the current server of each;
// TCP (after a truncated answer) goes to the server that truncated the
// answer to that query.
func (f *fanout) dial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	if !strings.HasPrefix(network, "udp") {
		return &tcpConn{ctx: ctx, f: f, network: network, address: address}, nil
	}
	servers, links, port := f.current()
	if len(servers) < 2 {
		return d.DialContext(ctx, network, address) //nolint:wrapcheck // the resolver reports it
	}
	f.mu.Lock()
	linger := f.linger
	f.mu.Unlock()
	c := &fanoutConn{f: f, linger: linger, replies: make(chan fanoutReply, 4*len(servers)), done: make(chan struct{})}
	for i, s := range servers {
		conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(s, port))
		if err != nil {
			f.failed(links[i], s)
			continue
		}
		c.conns = append(c.conns, fanoutLink{conn: conn, link: links[i], server: s})
	}
	if len(c.conns) == 0 {
		return d.DialContext(ctx, network, address) //nolint:wrapcheck // the resolver reports it
	}
	return c, nil
}

type fanoutLink struct {
	conn   net.Conn
	link   int
	server string
}

type fanoutReply struct {
	b   []byte
	err error
	i   int // the index in conns
}

// fanoutConn writes a query to the server of every link and reads the
// best answer. It is a net.PacketConn, so the resolver frames it as UDP.
// The state of the answers lives on the conn: a resolver that reads again
// (an answer of another query ID) keeps the count of what came already.
type fanoutConn struct {
	f       *fanout
	linger  time.Duration
	conns   []fanoutLink
	start   sync.Once
	replies chan fanoutReply
	done    chan struct{}

	mu        sync.Mutex
	id        uint16 // the query ID
	deadline  time.Time
	answered  []bool   // per conn: answered or failed
	negatives [][]byte // negative answers, held until every link answered
}

// rcodeOK tells a response that counts as an answer: NOERROR. NXDOMAIN,
// SERVFAIL and REFUSED wait for the other links (resolved dns_query_ready).
func rcodeOK(b []byte) bool { return len(b) >= 4 && b[3]&0x0f == 0 }

func (c *fanoutConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if len(b) >= 2 {
		c.id = binary.BigEndian.Uint16(b)
	}
	c.answered = make([]bool, len(c.conns))
	c.negatives = nil
	c.mu.Unlock()
	var sent bool
	var last error
	for i, l := range c.conns {
		if _, err := l.conn.Write(b); err != nil {
			last = err
			c.mu.Lock()
			c.answered[i] = true // no answer to wait for
			c.mu.Unlock()
			c.f.failed(l.link, l.server)
			continue
		}
		sent = true
	}
	if !sent {
		return 0, last
	}
	return len(b), nil
}

func (c *fanoutConn) readers() {
	for i, l := range c.conns {
		go func() {
			buf := make([]byte, 65535)
			for {
				n, err := l.conn.Read(buf)
				r := fanoutReply{i: i, err: err}
				if err == nil {
					r.b = append([]byte(nil), buf[:n]...)
				}
				select {
				case c.replies <- r:
				case <-c.done:
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

// Read returns the first positive answer; a negative one only when every
// link answered, and each negative once (the resolver reads again after
// one it rejects).
func (c *fanoutConn) Read(b []byte) (int, error) {
	c.start.Do(c.readers)
	for {
		c.mu.Lock()
		deadline := c.deadline
		n, ok := c.nextNegative(b)
		c.mu.Unlock()
		if ok {
			return n, nil
		}
		var timeout <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case r := <-c.replies:
			if timer != nil {
				timer.Stop()
			}
			if n, ok, err := c.take(r, b); ok {
				return n, err
			}
		case <-timeout:
			c.mu.Lock()
			for i, done := range c.answered {
				if !done {
					c.answered[i] = true
					c.f.failed(c.conns[i].link, c.conns[i].server)
				}
			}
			n, ok := c.nextNegative(b)
			c.mu.Unlock()
			if ok {
				return n, nil
			}
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// take handles one reply; ok tells Read to return n and err.
func (c *fanoutConn) take(r fanoutReply, b []byte) (n int, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.err == nil && (len(r.b) < 2 || binary.BigEndian.Uint16(r.b) != c.id) {
		return 0, false, nil // another query ID: not counted
	}
	c.answered[r.i] = true
	switch {
	case r.err != nil:
		c.f.failed(c.conns[r.i].link, c.conns[r.i].server)
	case rcodeOK(r.b):
		if r.b[2]&0x02 != 0 {
			c.f.truncated(c.id, c.conns[r.i].server) // TC: the resolver asks this server over TCP
		}
		return copy(b, r.b), true, nil
	default:
		c.negatives = append(c.negatives, r.b)
	}
	if n, ok := c.nextNegative(b); ok {
		return n, true, nil // every link answered, none better
	}
	if r.err != nil && c.allAnswered() {
		return 0, true, r.err // every link failed
	}
	return 0, false, nil
}

func (c *fanoutConn) allAnswered() bool {
	for _, done := range c.answered {
		if !done {
			return false
		}
	}
	return true
}

// nextNegative returns the next held negative answer once every link
// answered; c.mu is held.
func (c *fanoutConn) nextNegative(b []byte) (int, bool) {
	if len(c.negatives) == 0 || !c.allAnswered() {
		return 0, false
	}
	n := copy(b, c.negatives[0])
	c.negatives = c.negatives[1:]
	return n, true
}

func (c *fanoutConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := c.Read(b)
	return n, c.RemoteAddr(), err
}

func (c *fanoutConn) WriteTo(b []byte, _ net.Addr) (int, error) { return c.Write(b) }

// Close keeps the sockets fanoutLinger longer for late answers.
func (c *fanoutConn) Close() error {
	time.AfterFunc(c.linger, func() {
		close(c.done)
		for _, l := range c.conns {
			_ = l.conn.Close()
		}
	})
	return nil
}

func (c *fanoutConn) LocalAddr() net.Addr  { return c.conns[0].conn.LocalAddr() }
func (c *fanoutConn) RemoteAddr() net.Addr { return c.conns[0].conn.RemoteAddr() }

// SetDeadline and SetReadDeadline bound Read; the sockets get no deadline,
// so they stay open for late answers until Close.
func (c *fanoutConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *fanoutConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *fanoutConn) SetWriteDeadline(time.Time) error { return nil }

// tcpConn dials at the first Write: the query ID picks the server that
// truncated the UDP answer, else the resolver's address.
type tcpConn struct {
	net.Conn
	ctx      context.Context //nolint:containedctx // the resolver's dial context, used at the first Write
	f        *fanout
	network  string
	address  string
	deadline time.Time
}

func (c *tcpConn) Write(b []byte) (int, error) {
	if c.Conn == nil {
		if err := c.connect(b); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(b) //nolint:wrapcheck // the resolver reports it
}

// connect dials the server for the query in b (2 bytes length, then the
// message with its ID).
func (c *tcpConn) connect(b []byte) error {
	address := c.address
	if len(b) >= 4 {
		if s, port, ok := c.f.tcpServer(binary.BigEndian.Uint16(b[2:])); ok {
			address = net.JoinHostPort(s, port)
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(c.ctx, c.network, address)
	if err != nil {
		return err //nolint:wrapcheck // the resolver reports it
	}
	if !c.deadline.IsZero() {
		_ = conn.SetDeadline(c.deadline)
	}
	c.Conn = conn
	return nil
}

func (c *tcpConn) Read(b []byte) (int, error) {
	if c.Conn == nil {
		return 0, net.ErrClosed
	}
	return c.Conn.Read(b) //nolint:wrapcheck // the resolver reports it
}

func (c *tcpConn) Close() error {
	if c.Conn == nil {
		return nil
	}
	return c.Conn.Close() //nolint:wrapcheck // nothing to add
}

func (c *tcpConn) SetDeadline(t time.Time) error {
	c.deadline = t
	if c.Conn == nil {
		return nil
	}
	return c.Conn.SetDeadline(t) //nolint:wrapcheck // nothing to add
}
