//go:build linux

package bfd

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

type change struct{ from, to State }

// A session to 127.0.0.1 receives its own packets and comes Up. A Down
// packet for it with TTL 254 must be dropped (RFC 5881 5, GTSM); the same
// packet with TTL 255 takes the session Down. The timers are the 1 s of
// the slow start: a session that talks to itself sees its own Poll before
// its own interval changes.
func TestManagerSingleHopTTL(t *testing.T) {
	changes := make(chan change, 64)
	m := NewManager(slog.Default(), func(_ *Peer, from, to State, _ Diag) {
		select {
		case changes <- change{from, to}:
		default:
		}
	})
	t.Cleanup(func() { closeWithin(t, m) })
	timers := Timers{DesiredMinTx: time.Second, RequiredMinRx: time.Second, DetectMult: 3}
	if err := m.Add(&Peer{Name: "self", Addr: netip.MustParseAddr("127.0.0.1"), Timers: timers}); err != nil {
		t.Fatal(err)
	}
	waitChange := func(want change, within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case c := <-changes:
				if c == want {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	if !waitChange(change{StateInit, StateUp}, 10*time.Second) {
		t.Fatal("the session did not come Up")
	}
	m.mu.Lock()
	var discr uint32
	for d := range m.runs {
		discr = d
	}
	m.mu.Unlock()

	send := func(ttl int) {
		t.Helper()
		var lc net.ListenConfig
		conn, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := ipv4.NewPacketConn(conn).SetTTL(ttl); err != nil {
			t.Fatal(err)
		}
		p := Packet{State: StateDown, Diag: DiagTimeExpired, DetectMult: 3, MyDiscriminator: discr,
			YourDiscriminator: discr, DesiredMinTxUS: 1000000, RequiredMinRxUS: 1000000}
		if _, err := conn.WriteTo(p.Marshal(), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: portSingleHop}); err != nil {
			t.Fatal(err)
		}
	}
	send(254)
	if waitChange(change{StateUp, StateDown}, 500*time.Millisecond) {
		t.Fatal("a single-hop packet with TTL 254 took the session Down")
	}
	send(255)
	if !waitChange(change{StateUp, StateDown}, 2*time.Second) {
		t.Fatal("a single-hop packet with TTL 255 did not take the session Down")
	}
}

// A session with fast timers that talks to itself stays Up: on Up it
// advertises 150 ms at once, so its next packet must follow within the
// detection time of 450 ms, not after the 1 s of the slow start. Before,
// it timed out about 450 ms after Up, like FRR bfdd did at the leaves.
func TestManagerFastTxAfterUp(t *testing.T) {
	changes := make(chan change, 64)
	m := NewManager(slog.Default(), func(_ *Peer, from, to State, _ Diag) {
		select {
		case changes <- change{from, to}:
		default:
		}
	})
	t.Cleanup(func() { closeWithin(t, m) })
	timers := Timers{DesiredMinTx: 150 * time.Millisecond, RequiredMinRx: 150 * time.Millisecond, DetectMult: 3}
	if err := m.Add(&Peer{Name: "self", Addr: netip.MustParseAddr("127.0.0.1"), Timers: timers}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for up := false; !up; {
		select {
		case c := <-changes:
			up = c.to == StateUp
		case <-deadline:
			t.Fatal("the session did not come Up")
		}
	}
	select {
	case c := <-changes:
		t.Fatalf("session left Up: %v -> %v", c.from, c.to)
	case <-time.After(3 * time.Second):
	}
}

// Replace moves a session to a new address: the old one stops and the
// new one comes Up with its own packets.
func TestManagerReplace(t *testing.T) {
	changes := make(chan string, 64)
	m := NewManager(slog.Default(), func(p *Peer, _, to State, _ Diag) {
		select {
		case changes <- p.Name + " " + to.String():
		default:
		}
	})
	t.Cleanup(func() { closeWithin(t, m) })
	timers := Timers{DesiredMinTx: time.Second, RequiredMinRx: time.Second, DetectMult: 3}
	// 192.0.2.1 never answers; 127.0.0.1 is the session itself.
	if err := m.Add(&Peer{Name: "old", Addr: netip.MustParseAddr("192.0.2.1"), Timers: timers}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	var old *run
	for _, r := range m.runs {
		old = r
	}
	m.mu.Unlock()
	if err := m.Replace("old", "new", netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop of the replaced session still runs")
	}
	m.mu.Lock()
	for _, l := range m.listeners {
		if slices.Contains(l.runs, old) {
			t.Error("the listener still hands packets to the replaced session")
		}
	}
	m.mu.Unlock()
	if err := m.Replace("old", "x", netip.MustParseAddr("127.0.0.1")); err == nil {
		t.Fatal("Replace of a removed session succeeded")
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case c := <-changes:
			if c == "new Up" {
				m.mu.Lock()
				n := len(m.runs)
				m.mu.Unlock()
				if n != 1 {
					t.Fatalf("%d sessions after Replace, want 1", n)
				}
				return
			}
		case <-deadline:
			t.Fatal("the replaced session did not come Up")
		}
	}
}

// closeWithin closes a manager and fails instead of hanging when a session
// loop does not end.
func closeWithin(t *testing.T, m *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Manager.Close did not return within 5 s")
	}
}
