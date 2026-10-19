//go:build linux

package gobgp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/telekom/BOOTy/pkg/network"
)

// collisionLeaf plays a leaf by hand on two TCP connections, like FRR when
// BFD comes up after a carrier loss: GoBGP has connected to it (a), and it
// connects to GoBGP (b).
type collisionLeaf struct {
	t    *testing.T
	a, b net.Conn
}

func (l *collisionLeaf) write(c net.Conn, m *bgp.BGPMessage) {
	l.t.Helper()
	b, err := m.Serialize()
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := c.Write(b); err != nil {
		l.t.Fatal(err)
	}
}

func (l *collisionLeaf) read(c net.Conn) *bgp.BGPMessage {
	l.t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		l.t.Fatal(err)
	}
	hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
	if _, err := io.ReadFull(c, hdr); err != nil {
		l.t.Fatal(err)
	}
	body := make([]byte, int(binary.BigEndian.Uint16(hdr[16:18]))-bgp.BGP_HEADER_LENGTH)
	if _, err := io.ReadFull(c, body); err != nil {
		l.t.Fatal(err)
	}
	m, err := bgp.ParseBGPMessage(append(hdr, body...))
	if err != nil {
		l.t.Fatal(err)
	}
	return m
}

func (l *collisionLeaf) expect(c net.Conn, typ uint8) *bgp.BGPMessage {
	l.t.Helper()
	m := l.read(c)
	if m.Header.Type != typ {
		l.t.Fatalf("got message type %d (%v), want %d", m.Header.Type, m.Body, typ)
	}
	return m
}

func (l *collisionLeaf) expectNotification(c net.Conn, code, subcode uint8) {
	l.t.Helper()
	n := l.expect(c, bgp.BGP_MSG_NOTIFICATION).Body.(*bgp.BGPNotification)
	if n.ErrorCode != code || n.ErrorSubcode != subcode {
		l.t.Fatalf("got NOTIFICATION %d/%d, want %d/%d", n.ErrorCode, n.ErrorSubcode, code, subcode)
	}
}

// open is the leaf's OPEN: a lower BGP Identifier than BOOTy's, so BOOTy
// keeps the connection it opened (RFC 4271 Section 6.8), with graceful
// restart like the production leaves.
func (l *collisionLeaf) open() *bgp.BGPMessage {
	l.t.Helper()
	m, err := bgp.NewBGPOpenMessage(65500, 90, netip.MustParseAddr("10.0.0.1"), []bgp.OptionParameterInterface{
		bgp.NewOptionParameterCapability([]bgp.ParameterCapabilityInterface{
			bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC),
			bgp.NewCapFourOctetASNumber(65500),
			bgp.NewCapGracefulRestart(false, false, 120, []*bgp.CapGracefulRestartTuple{
				bgp.NewCapGracefulRestartTuple(bgp.RF_IPv4_UC, false),
			}),
		}),
	})
	if err != nil {
		l.t.Fatal(err)
	}
	return m
}

// startCollision starts a GoBGP server with a peer as BOOTy builds it with
// BGP_CONNECT_AT_ONCE (ConnectAtOnce, graceful restart helper) to a leaf on
// a local port and
// brings both connections to the point where GoBGP sent its OPEN on each.
func startCollision(t *testing.T) (*server.BgpServer, *collisionLeaf) {
	t.Helper()
	ctx := t.Context()
	var lc net.ListenConfig
	leafL, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leafL.Close() })
	port := freePort(t)
	s := startServerAt(t, 65000, "192.0.2.17", "127.0.0.1", port)
	cfg := &Config{ASN: 65000, ConnectAtOnce: true, Capabilities: network.BGPCapabilities{GracefulRestart: true, GracefulRestartTime: 120}}
	p := buildSessionPeer(cfg, &network.BGPSession{Name: "127.0.0.1", RemoteASN: 65500, IPv4Unicast: true}, "127.0.0.1")
	p.Transport.RemotePort = uint32(leafL.Addr().(*net.TCPAddr).Port) //nolint:gosec // test port
	if err := s.AddPeer(ctx, &apipb.AddPeerRequest{Peer: p}); err != nil {
		t.Fatal(err)
	}
	l := &collisionLeaf{t: t}
	if l.a, err = leafL.Accept(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.a.Close() })
	l.expect(l.a, bgp.BGP_MSG_OPEN)
	if l.b, err = (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.b.Close() })
	l.expect(l.b, bgp.BGP_MSG_OPEN)
	return s, l
}

func sessionState(t *testing.T, s *server.BgpServer) apipb.PeerState_SessionState {
	t.Helper()
	var st apipb.PeerState_SessionState
	if err := s.ListPeer(context.Background(), &apipb.ListPeerRequest{}, func(p *apipb.Peer) {
		st = p.GetState().GetSessionState()
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func waitSessionState(t *testing.T, s *server.BgpServer, want apipb.PeerState_SessionState, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for sessionState(t, s) != want {
		if time.Now().After(deadline) {
			t.Fatalf("session %s, want %s within %s", sessionState(t, s), want, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The two connection collisions of the production template lab after a
// carrier loss (TestProdTplOneDropPerFault, TestProdTplCarrierLossPastBFD)
// with the GoBGP fork tag v4.10.0-booty.10.
func TestCollisionAfterCarrierLoss(t *testing.T) {
	// GoBGP gets the leaf's OPEN on its own connection first and is
	// Established there while the leaf's connection has no OPEN yet. GoBGP
	// must keep the leaf's connection until the leaf's OPEN arrives on it
	// and close it then with Cease/Connection Collision Resolution. Closing
	// it earlier made the leaf connect again, and with graceful restart the
	// new connection ended the session a second time (booty.6: two drops per
	// fault).
	t.Run("losing connection closed after its OPEN", func(t *testing.T) {
		s, l := startCollision(t)
		oa, err := l.open().Serialize()
		if err != nil {
			t.Fatal(err)
		}
		ka, err := bgp.NewBGPKeepAliveMessage().Serialize()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.a.Write(append(oa, ka...)); err != nil {
			t.Fatal(err)
		}
		l.expect(l.a, bgp.BGP_MSG_KEEPALIVE)
		waitSessionState(t, s, apipb.PeerState_SESSION_STATE_ESTABLISHED, 2*time.Second)

		// The leaf's connection is still open and silent.
		if err := l.b.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		var ne net.Error
		if _, err := l.b.Read(make([]byte, 1)); !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("GoBGP did not keep the leaf's connection until its OPEN: %v", err)
		}
		l.write(l.b, l.open())
		l.expectNotification(l.b, bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION)

		// One session, which stays up.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if st := sessionState(t, s); st != apipb.PeerState_SESSION_STATE_ESTABLISHED {
				t.Fatalf("session %s after the collision, want it to stay Established", st)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	// The leaf drops its own connection with a NOTIFICATION before its OPEN.
	// GoBGP answers it with the FSM Error (RFC 4271 Section 8.2.2) and goes
	// on with its own connection, without the 5 s Idle hold (booty.6: the
	// session came up more than 5 s after the carrier).
	t.Run("notification before open on the leaf's connection", func(t *testing.T) {
		s, l := startCollision(t)
		start := time.Now()
		l.write(l.b, bgp.NewBGPNotificationMessage(bgp.BGP_ERROR_CEASE, bgp.BGP_ERROR_SUB_CONNECTION_COLLISION_RESOLUTION, nil))
		l.expectNotification(l.b, bgp.BGP_ERROR_FSM_ERROR, 1)
		l.write(l.a, l.open())
		l.expect(l.a, bgp.BGP_MSG_KEEPALIVE)
		l.write(l.a, bgp.NewBGPKeepAliveMessage())
		waitSessionState(t, s, apipb.PeerState_SESSION_STATE_ESTABLISHED, 2*time.Second)
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("session up %s after the leaf's NOTIFICATION", took)
		}
	})
}
