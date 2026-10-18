//go:build linux

package gobgp

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"
	"github.com/osrg/gobgp/v3/pkg/server"
)

func peerEvent(addr string, s apipb.PeerState_SessionState, a apipb.PeerState_AdminState) *apipb.Peer {
	return &apipb.Peer{State: &apipb.PeerState{NeighborAddress: addr, SessionState: s, AdminState: a}}
}

// An Idle event of a peer that BOOTy keeps up asks every peer of the host
// for a restart (restartPeer decides); other states, an administratively
// down peer and events after stop do not.
func TestPeerRestarterHandle(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	r := &peerRestarter{
		peers: func(host string) []string { return []string{host + "%eth0", host + "%eth1"} },
		restart: func(_ context.Context, addr string) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, addr)
			return addr == "fe80::1%eth0", nil
		},
		log: slog.Default(),
	}
	ctx := context.Background()
	both := []string{"fe80::1%eth0", "fe80::1%eth1"}
	steps := []struct {
		event *apipb.Peer
		want  []string
	}{
		{peerEvent("fe80::1", apipb.PeerState_ACTIVE, apipb.PeerState_UP), nil},
		{peerEvent("fe80::1", apipb.PeerState_ESTABLISHED, apipb.PeerState_UP), nil},
		{peerEvent("fe80::1", apipb.PeerState_IDLE, apipb.PeerState_DOWN), nil},
		{peerEvent("fe80::1", apipb.PeerState_IDLE, apipb.PeerState_UP), both},
	}
	for i, s := range steps {
		r.handle(ctx, s.event)
		r.wg.Wait()
		if !slices.Equal(asked, s.want) {
			t.Fatalf("step %d: asked %v, want %v", i, asked, s.want)
		}
	}
	r.stop()
	r.handle(ctx, peerEvent("fe80::1", apipb.PeerState_IDLE, apipb.PeerState_UP))
	r.wg.Wait()
	if !slices.Equal(asked, both) {
		t.Fatalf("asked after stop: %v", asked)
	}
}

// fakeSpeaker accepts the session of a GoBGP peer that u adds for
// 127.0.0.1 and brings it up by hand: OPEN (with graceful restart and an
// IPv4 tuple when gr), then KEEPALIVE. Closing the returned connection
// ends the session without a NOTIFICATION.
func fakeSpeaker(t *testing.T, u *UnderlayTier, gr bool) net.Conn {
	t.Helper()
	ctx := context.Background()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	peer := &apipb.Peer{
		Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65500},
		Transport: &apipb.Transport{RemotePort: uint32(ln.Addr().(*net.TCPAddr).Port)}, //nolint:gosec // test port
		Timers:    &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 1}},
		AfiSafis:  ipv4Families(),
	}
	caps := []bgp.ParameterCapabilityInterface{bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC), bgp.NewCapFourOctetASNumber(65500)}
	if gr {
		peer.GracefulRestart = &apipb.GracefulRestart{Enabled: true, RestartTime: 120}
		peer.AfiSafis[0].MpGracefulRestart = &apipb.MpGracefulRestart{Config: &apipb.MpGracefulRestartConfig{Enabled: true}}
		caps = append(caps, bgp.NewCapGracefulRestart(false, false, 120,
			[]*bgp.CapGracefulRestartTuple{bgp.NewCapGracefulRestartTuple(bgp.RF_IPv4_UC, true)}))
	}
	if err := u.addPeer(ctx, peer); err != nil {
		t.Fatal(err)
	}
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// GoBGP v3.37 writes the graceful restart state of the peer config in
	// OpenSent (Advertised in capabilitiesFromConfig, Received and Enabled
	// when it takes our OPEN) under the FSM lock, but ListPeer copies the
	// AfiSafis elements after it released the lock (toConfig): a data race
	// inside GoBGP v3, fixed in v4 (pConfAccess.ReadCopy). So the test
	// calls ListPeer only after those writes, which it sees through the
	// messages GoBGP sends after them (its OPEN, then its KEEPALIVE), and
	// allows no second connection.
	_ = ln.Close()
	readMsg := func() {
		t.Helper()
		hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(hdr[16:18]))-bgp.BGP_HEADER_LENGTH); err != nil {
			t.Fatal(err)
		}
	}
	send := func(m *bgp.BGPMessage) {
		t.Helper()
		b, err := m.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	readMsg() // GoBGP's OPEN
	send(bgp.NewBGPOpenMessage(65500, 90, "198.51.100.1", []bgp.OptionParameterInterface{bgp.NewOptionParameterCapability(caps)}))
	readMsg() // GoBGP's KEEPALIVE, sent after it took our OPEN
	send(bgp.NewBGPKeepAliveMessage())
	go func() { _, _ = io.Copy(io.Discard, conn) }()
	waitPeer(t, u, func(p *apipb.Peer) bool { return p.GetState().GetSessionState() == apipb.PeerState_ESTABLISHED })
	return conn
}

func waitPeer(t *testing.T, u *UnderlayTier, ok func(*apipb.Peer) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		done := false
		_ = u.bgp.ListPeer(context.Background(), &apipb.ListPeerRequest{Address: "127.0.0.1"}, func(p *apipb.Peer) { done = ok(p) })
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("peer state not reached")
}

func idleAfterUp(p *apipb.Peer) bool {
	return p.GetState().GetSessionState() == apipb.PeerState_IDLE && p.GetTimers().GetState().GetUptime() != nil
}

// restartPeer against a real GoBGP: it restarts a peer whose session
// ended, but not one that never came up, an Established one, one that
// BOOTy stopped (administratively down) or a restarting peer that sent
// family tuples.
func TestRestartPeerReal(t *testing.T) {
	ctx := context.Background()
	newTier := func() *UnderlayTier {
		return &UnderlayTier{log: slog.Default(), bgp: startServer(t, 65000, "192.0.2.17", -1)}
	}
	t.Run("never up", func(t *testing.T) {
		u := newTier()
		if err := u.addPeer(ctx, &apipb.Peer{Conf: &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65500},
			Transport: &apipb.Transport{RemotePort: uint32(freePort(t))}}); err != nil { //nolint:gosec // test port
			t.Fatal(err)
		}
		if ok, err := u.restartPeer(ctx, "127.0.0.1"); ok || err != nil {
			t.Fatalf("restart = %v, %v; want none", ok, err)
		}
	})
	t.Run("session ended", func(t *testing.T) {
		u := newTier()
		_ = fakeSpeaker(t, u, false).Close()
		waitPeer(t, u, idleAfterUp)
		if ok, err := u.restartPeer(ctx, "127.0.0.1"); !ok || err != nil {
			t.Fatalf("restart = %v, %v; want a restart", ok, err)
		}
		// Added again: a new peer without an uptime.
		waitPeer(t, u, func(p *apipb.Peer) bool { return p.GetTimers().GetState().GetUptime() == nil })
	})
	t.Run("administratively down", func(t *testing.T) {
		u := newTier()
		_ = fakeSpeaker(t, u, false)
		if ok, err := u.restartPeer(ctx, "127.0.0.1"); ok || err != nil {
			t.Fatalf("restart of an Established peer = %v, %v; want none", ok, err)
		}
		if err := u.bgp.DisablePeer(ctx, &apipb.DisablePeerRequest{Address: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
		// GoBGP also clears the uptime of a disabled peer.
		waitPeer(t, u, func(p *apipb.Peer) bool {
			return p.GetState().GetSessionState() == apipb.PeerState_IDLE && p.GetState().GetAdminState() == apipb.PeerState_DOWN
		})
		if ok, err := u.restartPeer(ctx, "127.0.0.1"); ok || err != nil {
			t.Fatalf("restart = %v, %v; want none", ok, err)
		}
	})
	t.Run("graceful restart with tuples", func(t *testing.T) {
		u := newTier()
		_ = fakeSpeaker(t, u, true).Close()
		waitPeer(t, u, func(p *apipb.Peer) bool { return idleAfterUp(p) && keepsGracefulRestartRoutes(p) })
		if ok, err := u.restartPeer(ctx, "127.0.0.1"); ok || err != nil {
			t.Fatalf("restart = %v, %v; want none", ok, err)
		}
	})
}

// GoBGP keeps the stale routes of a restarting peer that sent family
// tuples; such a peer is not deleted.
func TestKeepsGracefulRestartRoutes(t *testing.T) {
	tuples := []*apipb.AfiSafi{{MpGracefulRestart: &apipb.MpGracefulRestart{State: &apipb.MpGracefulRestartState{Received: true}}}}
	for _, tt := range []struct {
		name string
		p    *apipb.Peer
		want bool
	}{
		{"restarting with tuples", &apipb.Peer{GracefulRestart: &apipb.GracefulRestart{PeerRestarting: true}, AfiSafis: tuples}, true},
		{"restarting without tuples", &apipb.Peer{GracefulRestart: &apipb.GracefulRestart{PeerRestarting: true}, AfiSafis: ipv4Families()}, false},
		{"not restarting", &apipb.Peer{AfiSafis: tuples}, false},
	} {
		if got := keepsGracefulRestartRoutes(tt.p); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

// reestablishAfterLeafReset peers BOOTy (127.0.0.1) with a leaf
// (127.0.0.2) that resets the session and connects again after 1 s in
// Idle (FRR: 1 s after a TCP close, 2 s after a NOTIFICATION). It reports whether BOOTy is Established again within limit.
func reestablishAfterLeafReset(t *testing.T, restarter bool, limit time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bootyPort, leafPort := freePort(t), freePort(t)
	booty := startServerAt(t, 65001, "10.0.0.1", "127.0.0.1", bootyPort)
	leaf := startServerAt(t, 65002, "10.0.0.2", "127.0.0.2", leafPort)
	timers := func() *apipb.Timers {
		return &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 10, HoldTime: 9, KeepaliveInterval: 3, IdleHoldTimeAfterReset: 1}}
	}
	if err := leaf.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
		Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65001},
		Transport: &apipb.Transport{LocalAddress: "127.0.0.2", RemotePort: uint32(bootyPort)}, //nolint:gosec // test port
		Timers:    timers(), AfiSafis: ipv4Families(),
	}}); err != nil {
		t.Fatal(err)
	}
	u := &UnderlayTier{bgp: booty, log: slog.Default()}
	if err := u.addPeer(ctx, &apipb.Peer{
		Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.2", PeerAsn: 65002},
		Transport: &apipb.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(leafPort)}, //nolint:gosec // test port
		Timers:    timers(), AfiSafis: ipv4Families(),
	}); err != nil {
		t.Fatal(err)
	}
	if restarter {
		if err := u.startRestarter(); err != nil {
			t.Fatal(err)
		}
		defer u.stopRestart()
	}
	up, _ := waitEstablished(ctx, booty, "127.0.0.2", 0)
	if up == 0 {
		t.Fatal("the session did not come up")
	}
	// GoBGP keeps the uptime in seconds: reset in a later second than the
	// session came up, so the next session has a later uptime.
	time.Sleep(time.Until(time.Unix(up+1, 0)))
	start := time.Now()
	if err := leaf.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	limited, stop := context.WithTimeout(ctx, limit)
	defer stop()
	_, ok := waitEstablished(limited, booty, "127.0.0.2", start.Unix())
	t.Logf("restarter %v: Established again after %v: %v", restarter, time.Since(start), ok)
	return ok
}

// waitEstablished waits until the session to addr is up since the Unix
// time since or later, and returns when it came up; false when ctx ends.
func waitEstablished(ctx context.Context, s *server.BgpServer, addr string, since int64) (int64, bool) {
	for {
		var up int64
		_ = s.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
			if p.GetState().GetSessionState() == apipb.PeerState_ESTABLISHED {
				up = p.GetTimers().GetState().GetUptime().GetSeconds()
			}
		})
		if up != 0 && up >= since {
			return up, true
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// When the leaf resets the session, GoBGP alone closes the leaf's
// connection for 5 s in Idle (osrg/gobgp#3735), and the two sides can
// refuse each other for much longer (19 to 30 s seen); with the restarter
// BOOTy accepts it at once, like FRR (1.7 s seen).
func TestRestarterAcceptsLeafAfterReset(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 10 s")
	}
	const limit = 4 * time.Second
	if reestablishAfterLeafReset(t, false, limit) {
		t.Errorf("without the restarter BOOTy was Established again within %v; GoBGP should hold it 5 s in Idle", limit)
	}
	if !reestablishAfterLeafReset(t, true, limit) {
		t.Errorf("with the restarter BOOTy was not Established again within %v", limit)
	}
}
