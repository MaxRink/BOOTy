//go:build linux

package bfd

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
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
	t.Cleanup(m.Close)
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
