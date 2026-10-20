//go:build linux

package gobgp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
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
		{peerEvent("fe80::1", apipb.PeerState_SESSION_STATE_ACTIVE, apipb.PeerState_ADMIN_STATE_UP), nil},
		{peerEvent("fe80::1", apipb.PeerState_SESSION_STATE_ESTABLISHED, apipb.PeerState_ADMIN_STATE_UP), nil},
		{peerEvent("fe80::1", apipb.PeerState_SESSION_STATE_IDLE, apipb.PeerState_ADMIN_STATE_DOWN), nil},
		{peerEvent("fe80::1", apipb.PeerState_SESSION_STATE_IDLE, apipb.PeerState_ADMIN_STATE_UP), both},
	}
	for i, s := range steps {
		r.handle(ctx, s.event)
		r.wg.Wait()
		if !slices.Equal(asked, s.want) {
			t.Fatalf("step %d: asked %v, want %v", i, asked, s.want)
		}
	}
	r.stop()
	r.handle(ctx, peerEvent("fe80::1", apipb.PeerState_SESSION_STATE_IDLE, apipb.PeerState_ADMIN_STATE_UP))
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
	open, err := bgp.NewBGPOpenMessage(65500, 90, netip.MustParseAddr("198.51.100.1"), []bgp.OptionParameterInterface{bgp.NewOptionParameterCapability(caps)})
	if err != nil {
		t.Fatal(err)
	}
	send(open)
	readMsg() // GoBGP's KEEPALIVE, sent after it took our OPEN
	send(bgp.NewBGPKeepAliveMessage())
	go func() { _, _ = io.Copy(io.Discard, conn) }()
	waitPeer(t, u, func(p *apipb.Peer) bool {
		return p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED
	})
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
	return p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_IDLE && p.GetTimers().GetState().GetUptime() != nil
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
			return p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_IDLE && p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_DOWN
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
// Idle (FRR: 1 s after a TCP close, 2 s after a NOTIFICATION). It returns
// how long BOOTy took to be Established again, and false when that took
// longer than limit.
func reestablishAfterLeafReset(t *testing.T, restarter bool, limit time.Duration) (time.Duration, bool) {
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
	took := time.Since(start)
	t.Logf("restarter %v: Established again after %v: %v", restarter, took, ok)
	return took, ok
}

// waitEstablished waits until the session to addr is up since the Unix
// time since or later, and returns when it came up; false when ctx ends.
func waitEstablished(ctx context.Context, s *server.BgpServer, addr string, since int64) (int64, bool) {
	for {
		var up int64
		_ = s.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
			if p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED {
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
// connection in Idle for its idle hold time: 2 s after the NOTIFICATION
// since osrg/gobgp 4e70e768, a fixed 5 s before (osrg/gobgp#3735, 19 to
// 30 s seen until both sides connected). With the restarter BOOTy accepts
// it at once, like FRR (1.6 to 1.8 s seen, 2.6 to 3.0 s without).
func TestRestarterAcceptsLeafAfterReset(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 10 s")
	}
	const limit = 4 * time.Second
	without, _ := reestablishAfterLeafReset(t, false, 10*time.Second)
	with, ok := reestablishAfterLeafReset(t, true, limit)
	if !ok {
		t.Errorf("with the restarter BOOTy was not Established again within %v", limit)
	}
	if with >= without {
		t.Errorf("with the restarter BOOTy was Established again after %v, not sooner than without it (%v)", with, without)
	}
}

// A GoBGP v4 peer event of a link-local peer has the zone in its address;
// the restarter keys peers without it, as the v3 events did. The API
// session state is the FSM state plus one.
func TestPeerStateEventDropsZone(t *testing.T) {
	p := peerStateEvent(&apiutil.PeerState{
		NeighborAddress: netip.MustParseAddr("fe80::1%eth1"),
		SessionState:    bgp.BGP_FSM_ESTABLISHED,
		AdminState:      apipb.PeerState_ADMIN_STATE_UP,
	})
	s := p.GetState()
	if s.GetNeighborAddress() != "fe80::1" || s.GetSessionState() != apipb.PeerState_SESSION_STATE_ESTABLISHED ||
		s.GetAdminState() != apipb.PeerState_ADMIN_STATE_UP {
		t.Fatalf("peerStateEvent() = %v, want fe80::1 established up", s)
	}
}

// The kindmetal EVPN parity run with both leaves down: on one uplink BFD
// fails before the carrier (BFD reset, the restarter adds the peer again,
// then the carrier loss stops it), on the other after it. When the
// carrier returns, the leaf connects at once (FRR starts the session on
// interface up). With EnablePeer alone GoBGP 4.10.0 held the first peer
// 5 s in Idle (holdtimeIdle) and closed the leaf's connection meanwhile;
// now both wait 1 s and connect 1.5 to 2 s later. Since the fork tag
// v4.10.0-booty.17 the administrative shutdown leaves 1 s as well, so
// this test passes with and without the reset in startStoppedPeer.
func TestCarrierReturnWaitsIdleHoldAfterReset(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 3 s")
	}
	ctx := context.Background()
	bootyPort := freePort(t)
	u := &UnderlayTier{bgp: startServerAt(t, 65001, "10.0.0.1", "127.0.0.1", bootyPort), log: slog.Default()}
	timers := func(atOnce bool) *apipb.Timers {
		return &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 10, HoldTime: 9, KeepaliveInterval: 3, IdleHoldTimeAfterReset: idleHoldAfterReset, ConnectAtOnce: atOnce}}
	}
	type uplink struct {
		iface, addr string
		leaf        *server.BgpServer
	}
	links := []*uplink{{iface: "eth0", addr: "127.0.0.2"}, {iface: "eth1", addr: "127.0.0.3"}}
	leafPeer := func(l *uplink) *apipb.Peer {
		return &apipb.Peer{
			Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65001},
			Transport: &apipb.Transport{LocalAddress: l.addr, RemotePort: uint32(bootyPort)}, //nolint:gosec // test port
			Timers:    timers(true), AfiSafis: ipv4Families(),
		}
	}
	ifaces := map[string]string{}
	for i, l := range links {
		port := freePort(t)
		l.leaf = startServerAt(t, uint32(65002+i), l.addr, l.addr, port) //nolint:gosec // small
		if err := l.leaf.AddPeer(ctx, &apipb.AddPeerRequest{Peer: leafPeer(l)}); err != nil {
			t.Fatal(err)
		}
		if err := u.addPeer(ctx, &apipb.Peer{
			Conf:      &apipb.PeerConf{NeighborAddress: l.addr, PeerAsn: uint32(65002 + i)},  //nolint:gosec // small
			Transport: &apipb.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(port)}, //nolint:gosec // test port
			Timers:    timers(false), AfiSafis: ipv4Families(),
		}); err != nil {
			t.Fatal(err)
		}
		u.recordInterfacePeer(l.iface, l.addr)
		ifaces[l.iface] = l.addr
	}
	if err := u.startRestarter(); err != nil {
		t.Fatal(err)
	}
	defer u.stopRestart()
	w := u.newCarrierWatch(ifaces)

	waitFor := func(addr string, ok func(*apipb.Peer) bool) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			done := false
			_ = u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) { done = ok(p) })
			if done {
				return
			}
		}
		t.Fatalf("peer %s: state not reached", addr)
	}
	established := func(p *apipb.Peer) bool {
		return p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED
	}
	bfdDown := func(addr string) {
		t.Helper()
		if err := u.bgp.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: addr, BfdDown: true}); err != nil {
			t.Fatal(err)
		}
	}
	carrierLoss := func(l *uplink) {
		t.Helper()
		w.handle(l.iface, false)
		if err := l.leaf.DisablePeer(ctx, &apipb.DisablePeerRequest{Address: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range links {
		waitFor(l.addr, established)
	}

	// eth0: BFD Down, the restarter adds the peer again, then the carrier goes.
	bfdDown(links[0].addr)
	waitFor(links[0].addr, func(p *apipb.Peer) bool { return p.GetTimers().GetState().GetUptime() == nil })
	carrierLoss(links[0])
	// eth1: the carrier goes, then BFD Down.
	carrierLoss(links[1])
	bfdDown(links[1].addr)
	for _, l := range links {
		waitFor(l.addr, func(p *apipb.Peer) bool {
			return p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_DOWN && p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_IDLE
		})
	}

	start := time.Now()
	for _, l := range links {
		w.handle(l.iface, true)
		// The leaf starts the session at once on interface up, like FRR.
		if err := l.leaf.DeletePeer(ctx, &apipb.DeletePeerRequest{Address: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
		if err := l.leaf.AddPeer(ctx, &apipb.AddPeerRequest{Peer: leafPeer(l)}); err != nil {
			t.Fatal(err)
		}
	}
	limited, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, l := range links {
		if _, ok := waitEstablished(limited, u.bgp, l.addr, 0); !ok {
			t.Errorf("%s: not Established within 3 s after the carrier returned", l.iface)
			continue
		}
		t.Logf("%s: Established %v after the carrier returned", l.iface, time.Since(start))
	}
}

// carrierTier is a tier with the GoBGP peer 127.0.0.1 of a fake speaker
// on eth0, and the carrier watch of that interface.
func carrierTier(t *testing.T, gr bool) (*UnderlayTier, *carrierWatch, net.Conn) {
	t.Helper()
	u := &UnderlayTier{log: slog.Default(), bgp: startServer(t, 65000, "192.0.2.17", -1)}
	conn := fakeSpeaker(t, u, gr)
	u.recordInterfacePeer("eth0", "127.0.0.1")
	w := u.newCarrierWatch(map[string]string{"eth0": "127.0.0.1"})
	return u, w, conn
}

// A leaf that restarts with graceful restart (its session ended without
// a NOTIFICATION) and then loses carrier keeps its stale routes in GoBGP
// until the carrier is back and it advertises again, like FRR: bgp_stop
// keeps the stale paths of a peer in NSF wait and the restart timer runs
// on (bgp_fsm.c), and the start on interface up (bgp_start_interface_nbrs)
// does not clear them. Deleting the GoBGP peer would drop them.
func TestCarrierReturnKeepsGracefulRestartRoutes(t *testing.T) {
	u, w, conn := carrierTier(t, true)
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("10.9.9.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	update := bgp.NewBGPUpdateMessage(nil, []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{65500})}),
		nh,
	}, []bgp.PathNLRI{{NLRI: nlri}})
	b, err := update.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
	family := ipv4Families()[0].GetConfig().GetFamily()
	hasRoute := func() bool { return len(listPaths(t, u.bgp, family)["10.9.9.0/24"]) > 0 }
	for deadline := time.Now().Add(5 * time.Second); !hasRoute(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("route not received")
		}
	}
	_ = conn.Close()
	waitPeer(t, u, func(p *apipb.Peer) bool { return idleAfterUp(p) && keepsGracefulRestartRoutes(p) })

	w.handle("eth0", false)
	waitPeer(t, u, func(p *apipb.Peer) bool {
		return p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_DOWN
	})
	if !hasRoute() {
		t.Fatal("the carrier loss dropped the stale route")
	}
	w.handle("eth0", true)
	waitPeer(t, u, func(p *apipb.Peer) bool {
		return p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_UP
	})
	if !hasRoute() {
		t.Fatal("the carrier return dropped the stale route of the restarting leaf")
	}
}

// A session that is still Established when the carrier returns (the
// carrier loss did not stop it, for example a lost DisablePeer) is left
// alone.
func TestCarrierReturnLeavesEstablishedPeer(t *testing.T) {
	u, w, _ := carrierTier(t, false)
	var up int64
	_ = u.bgp.ListPeer(context.Background(), &apipb.ListPeerRequest{Address: "127.0.0.1"}, func(p *apipb.Peer) {
		up = p.GetTimers().GetState().GetUptime().GetSeconds()
	})
	w.disable = func(context.Context, string) error { return errors.New("lost") }
	w.handle("eth0", false)
	w.handle("eth0", true)
	time.Sleep(200 * time.Millisecond)
	waitPeer(t, u, func(p *apipb.Peer) bool {
		return p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED &&
			p.GetTimers().GetState().GetUptime().GetSeconds() == up && !p.GetConf().GetAdminDown()
	})
}

// The carrier watch stops and starts a peer under peerOps, so the peer
// restarter and the relearner cannot delete and add it at the same time.
func TestCarrierOpsHoldPeerOps(t *testing.T) {
	u := &UnderlayTier{log: slog.Default(), bgp: startServer(t, 65000, "192.0.2.17", -1)}
	w := u.newCarrierWatch(map[string]string{"eth0": "127.0.0.1"})
	for name, op := range map[string]func(context.Context, string) error{"disable": w.disable, "enable": w.enable} {
		u.peerOps.Lock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = op(context.Background(), "127.0.0.1")
		}()
		select {
		case <-done:
			u.peerOps.Unlock()
			t.Fatalf("%s ran without peerOps", name)
		case <-time.After(200 * time.Millisecond):
		}
		u.peerOps.Unlock()
		<-done
	}
}

// A carrier loss and return handled back to back leave the peer enabled:
// DisablePeer only queues the stop, so without stopPeer's wait the start
// read the peer as still enabled and skipped it, and the stop then left
// the peer down for good (reviewer probe: 27 of 30 runs).
func TestCarrierFlapBackToBack(t *testing.T) {
	u, w, _ := carrierTier(t, false)
	w.handle("eth0", false)
	w.handle("eth0", true)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		admin := apipb.PeerState_ADMIN_STATE_UNSPECIFIED
		_ = u.bgp.ListPeer(context.Background(), &apipb.ListPeerRequest{Address: "127.0.0.1"}, func(p *apipb.Peer) { admin = p.GetState().GetAdminState() })
		if admin == apipb.PeerState_ADMIN_STATE_DOWN && time.Until(deadline) < time.Second {
			t.Fatal("peer stuck administratively down after a fast carrier flap")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A peer that is enabled and waits in Idle (its session ended and the
// restarter does not run) when the carrier returns without a stop before
// is not reset, enabled or added again.
func TestCarrierReturnLeavesIdlePeer(t *testing.T) {
	u, w, conn := carrierTier(t, false)
	_ = conn.Close()
	waitPeer(t, u, idleAfterUp)
	w.disable = func(context.Context, string) error { return errors.New("lost") }
	w.handle("eth0", false)
	reset := false
	w.enable = func(ctx context.Context, addr string) error {
		err := u.startStoppedPeer(ctx, addr)
		_ = u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{Address: addr}, func(p *apipb.Peer) {
			reset = p.GetState().GetAdminState() != apipb.PeerState_ADMIN_STATE_UP || p.GetTimers().GetState().GetUptime() == nil
		})
		return err
	}
	w.handle("eth0", true)
	if reset {
		t.Fatal("the Idle peer was stopped or added again")
	}
	waitPeer(t, u, func(p *apipb.Peer) bool {
		return p.GetState().GetAdminState() == apipb.PeerState_ADMIN_STATE_UP && p.GetTimers().GetState().GetUptime() != nil
	})
}

func adminState(u *UnderlayTier) apipb.PeerState_AdminState {
	a := apipb.PeerState_ADMIN_STATE_UNSPECIFIED
	_ = u.bgp.ListPeer(context.Background(), &apipb.ListPeerRequest{Address: "127.0.0.1"}, func(p *apipb.Peer) { a = p.GetState().GetAdminState() })
	return a
}

// Carrier down, up and down back to back: the second stop met the start
// still queued, GoBGP dropped it and the peer stayed enabled on a link
// without carrier (reviewer probe: 24 of 30 runs). startStoppedPeer now
// waits until the start is applied.
func TestCarrierFlapDownUpDown(t *testing.T) {
	u, w, _ := carrierTier(t, false)
	w.handle("eth0", false)
	w.handle("eth0", true)
	w.handle("eth0", false)
	time.Sleep(300 * time.Millisecond)
	if a := adminState(u); a != apipb.PeerState_ADMIN_STATE_DOWN {
		t.Fatalf("admin state %v after carrier down, up, down; want down", a)
	}
	w.handle("eth0", true)
	time.Sleep(300 * time.Millisecond)
	if a := adminState(u); a != apipb.PeerState_ADMIN_STATE_UP {
		t.Fatalf("admin state %v after the carrier returned; want up", a)
	}
}

// Fast carrier flaps of a peer that already waits in Idle (its session
// ended, the restarter does not run): the stop must be waited for by its
// admin state, the session state is Idle before the stop is applied.
func TestCarrierFlapIdlePeer(t *testing.T) {
	u, w, conn := carrierTier(t, false)
	_ = conn.Close()
	waitPeer(t, u, idleAfterUp)
	for i := range 10 {
		w.handle("eth0", false)
		w.handle("eth0", true)
		time.Sleep(50 * time.Millisecond)
		if a := adminState(u); a != apipb.PeerState_ADMIN_STATE_UP {
			t.Fatalf("flap %d: admin state %v after the carrier returned; want up", i, a)
		}
	}
}
