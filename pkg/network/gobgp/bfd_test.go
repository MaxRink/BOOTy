//go:build linux

package gobgp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/bfd"
)

// The BFD sessions of the BGP peers, like FRR 10.3 bgpd: only sessions
// with "bfd", multiplier 3 by default, and multihop only for an iBGP peer
// that is not on a connected network of its VRF (bgp_bfd.h
// PEER_IS_MULTIHOP).
func TestBFDPeers(t *testing.T) {
	// 10.0.0.1 is connected in Vrf_underlay only, 10.0.0.9 nowhere.
	connected := func(a netip.Addr, vrf string) bool {
		return a == netip.MustParseAddr("10.0.0.1") && vrf == "Vrf_underlay"
	}
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	fabric := bfd.Timers{DesiredMinTx: ms(150), RequiredMinRx: ms(150), DetectMult: 3}
	single := func(name, dev string) bfd.Peer {
		return bfd.Peer{Name: name, Addr: netip.MustParseAddr(name), Device: dev, Timers: fabric}
	}
	multi := func(name, dev string) bfd.Peer {
		p := single(name, dev)
		p.Multihop, p.Local = true, netip.MustParseAddr("192.0.2.17")
		return p
	}
	session := func(name string, internal bool, vrf string) network.BGPSession {
		return network.BGPSession{Name: name, Internal: internal, VRF: vrf, BFD: true, BFDTransmitMS: 150, BFDReceiveMS: 150}
	}
	ifPeers := map[string]string{"eth1": "fe80::2%eth1", "eth2": "fe80::3%eth2"}
	for _, tt := range []struct {
		name string
		cfg  Config
		want []bfd.Peer
	}{
		{"sessions", Config{RouterID: "192.0.2.17", Sessions: []network.BGPSession{
			{Name: "eth1", Interface: true, BFD: true, BFDTransmitMS: 150, BFDReceiveMS: 150},
			{Name: "eth2", Interface: true},            // no bfd
			session("10.0.0.1", true, "Vrf_underlay"),  // iBGP, connected: single-hop
			session("10.0.0.1", true, "default"),       // connected only in another VRF
			session("10.0.0.9", true, "Vrf_underlay"),  // iBGP, not connected: multihop
			session("10.0.0.9", false, "Vrf_underlay"), // eBGP: single-hop
		}}, []bfd.Peer{
			single("fe80::2%eth1", "eth1"),
			single("10.0.0.1", "Vrf_underlay"),
			multi("10.0.0.1", ""),
			multi("10.0.0.9", "Vrf_underlay"),
			single("10.0.0.9", "Vrf_underlay"),
		}},
		{"native peers without BFD timers", Config{NeighborAddrs: []string{"10.0.0.9"}}, nil},
		{"native iBGP peers", Config{RouterID: "192.0.2.17", ASN: 65000, BFDTransmitMS: 150, BFDReceiveMS: 150,
			NeighborAddrs: []string{"10.0.0.1", "10.0.0.9"}, NeighborVRF: "Vrf_underlay"}, []bfd.Peer{
			single("fe80::2%eth1", "eth1"),
			single("fe80::3%eth2", "eth2"),
			single("10.0.0.1", "Vrf_underlay"),
			multi("10.0.0.9", "Vrf_underlay"),
		}},
		{"native eBGP peers", Config{RouterID: "192.0.2.17", ASN: 65000, RemoteASN: 65100, BFDTransmitMS: 150,
			BFDReceiveMS: 150, NeighborAddrs: []string{"10.0.0.9"}, NeighborVRF: "Vrf_underlay"}, []bfd.Peer{
			single("fe80::2%eth1", "eth1"),
			single("fe80::3%eth2", "eth2"),
			single("10.0.0.9", "Vrf_underlay"),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := &UnderlayTier{cfg: &tt.cfg, log: slog.Default(), ifacePeers: ifPeers, connected: connected}
			got := u.bfdPeers()
			// The interface peers come from a map.
			if len(got) > 1 && got[0].Device == "eth2" {
				got[0], got[1] = got[1], got[0]
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("bfdPeers =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// Only BFD Up to Down resets the BGP session (FRR bgp_bfd.c); a session
// coming Up or a session that was never Up does not.
func TestBFDChanged(t *testing.T) {
	var resets []string
	u := &UnderlayTier{log: slog.Default(), resetBGP: func(_ context.Context, addr, reason string) {
		resets = append(resets, addr+" "+reason)
	}, bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
		return apipb.PeerState_SESSION_STATE_ESTABLISHED, time.Now().Add(-time.Minute)
	}}
	p := &bfd.Peer{Name: "fe80::2%eth1"}
	for _, tr := range [][2]bfd.State{
		{bfd.StateDown, bfd.StateInit}, {bfd.StateInit, bfd.StateUp}, {bfd.StateDown, bfd.StateUp},
		{bfd.StateInit, bfd.StateDown}, {bfd.StateUp, bfd.StateAdminDown},
	} {
		u.bfdChanged(p, tr[0], tr[1], bfd.DiagNone)
	}
	if len(resets) != 0 {
		t.Fatalf("resets without BFD Up to Down: %v", resets)
	}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagTimeExpired)
	if want := []string{"fe80::2%eth1 BFD down"}; !reflect.DeepEqual(resets, want) {
		t.Fatalf("resets = %v, want %v", resets, want)
	}
}

// Like FRR bgpd (last_event > peer->uptime, in seconds), a BFD Down in
// the second the BGP session came up does not reset it; GoBGP's uptime is
// in whole seconds, so neither does one in the next second. Nor does one
// in any state from Active to Established without an uptime: ListPeer
// lags behind the FSM, which sent the KEEPALIVE that brings the RR up.
// A Down for an unknown peer (BOOTy restarted it) is ignored. An earlier
// uptime, an uptime in the future (the clock stepped back) and Idle reset
// it. These are Downs with diag 3, the RR's re-key.
func TestBFDChangedAtBGPStart(t *testing.T) {
	now := time.Unix(1_000_000, 500_000_000)
	for _, tt := range []struct {
		name  string
		at    time.Time
		state apipb.PeerState_SessionState
		reset bool
	}{
		{"same second", now.Add(-400 * time.Millisecond), apipb.PeerState_SESSION_STATE_ESTABLISHED, false},
		// GoBGP truncates the uptime: up at 999_999.999 s, Down 1 ms later.
		{"next second", time.Unix(999_999, 0), apipb.PeerState_SESSION_STATE_ESTABLISHED, false},
		{"up for 2 s", now.Add(-2 * time.Second), apipb.PeerState_SESSION_STATE_ESTABLISHED, true},
		{"uptime in the future (clock stepped back)", now.Add(time.Hour), apipb.PeerState_SESSION_STATE_ESTABLISHED, true},
		{"OpenConfirm", time.Time{}, apipb.PeerState_SESSION_STATE_OPENCONFIRM, false},
		{"established, uptime not set yet", time.Time{}, apipb.PeerState_SESSION_STATE_ESTABLISHED, false},
		{"OpenSent", time.Time{}, apipb.PeerState_SESSION_STATE_OPENSENT, false},
		{"Active", time.Time{}, apipb.PeerState_SESSION_STATE_ACTIVE, false},
		{"Connect", time.Time{}, apipb.PeerState_SESSION_STATE_CONNECT, false},
		{"unknown peer", time.Time{}, apipb.PeerState_SESSION_STATE_UNSPECIFIED, false},
		{"Idle", time.Time{}, apipb.PeerState_SESSION_STATE_IDLE, true},
		{"Idle with an uptime in the same second", now.Add(-400 * time.Millisecond), apipb.PeerState_SESSION_STATE_IDLE, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resets := 0
			u := &UnderlayTier{log: slog.Default(), now: func() time.Time { return now },
				resetBGP:   func(context.Context, string, string) { resets++ },
				bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) { return tt.state, tt.at },
			}
			p := &bfd.Peer{Name: "198.51.100.2"}
			u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
			if (resets == 1) != tt.reset {
				t.Fatalf("resets = %d, want a reset %v", resets, tt.reset)
			}
			u.bfdChanged(p, bfd.StateInit, bfd.StateUp, bfd.DiagNone) // ends the recheck
		})
	}
}

// A path failure (diag 1, the detection time expired) gets only the
// tolerance of main and bgpd: OpenConfirm, no uptime yet, or the same
// second. In the next second, before OpenConfirm or for an unknown peer
// it resets the session at once.
func TestBFDPathFailureAtBGPStart(t *testing.T) {
	now := time.Unix(1_000_000, 500_000_000)
	for _, tt := range []struct {
		name  string
		at    time.Time
		state apipb.PeerState_SessionState
		reset bool
	}{
		{"same second", now.Add(-400 * time.Millisecond), apipb.PeerState_SESSION_STATE_ESTABLISHED, false},
		{"next second", time.Unix(999_999, 0), apipb.PeerState_SESSION_STATE_ESTABLISHED, true},
		{"OpenConfirm", time.Time{}, apipb.PeerState_SESSION_STATE_OPENCONFIRM, false},
		{"established, uptime not set yet", time.Time{}, apipb.PeerState_SESSION_STATE_ESTABLISHED, false},
		{"OpenSent", time.Time{}, apipb.PeerState_SESSION_STATE_OPENSENT, true},
		{"Active", time.Time{}, apipb.PeerState_SESSION_STATE_ACTIVE, true},
		{"unknown peer", time.Time{}, apipb.PeerState_SESSION_STATE_UNSPECIFIED, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resets := 0
			u := &UnderlayTier{log: slog.Default(), now: func() time.Time { return now },
				resetBGP:   func(context.Context, string, string) { resets++ },
				bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) { return tt.state, tt.at },
			}
			p := &bfd.Peer{Name: "198.51.100.2"}
			u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagTimeExpired)
			if (resets == 1) != tt.reset {
				t.Fatalf("resets = %d, want a reset %v", resets, tt.reset)
			}
			u.bfdChanged(p, bfd.StateInit, bfd.StateUp, bfd.DiagNone) // ends the recheck
			u.stopRechecks()
		})
	}
}

// sessionState reads the state of a GoBGP peer and the uptime of an
// established one.
func TestSessionState(t *testing.T) {
	u := &UnderlayTier{}
	if state, _ := u.sessionState(context.Background(), "192.0.2.1"); state != apipb.PeerState_SESSION_STATE_UNSPECIFIED {
		t.Fatalf("state without a BGP server = %v", state)
	}
	u.bgp = startServer(t, 65000, "192.0.2.17", -1)
	if err := u.bgp.AddPeer(context.Background(), &apipb.AddPeerRequest{Peer: buildNumberedPeer(&Config{ASN: 65000}, "192.0.2.1", ipv4Families())}); err != nil {
		t.Fatal(err)
	}
	if state, at := u.sessionState(context.Background(), "192.0.2.1"); state == apipb.PeerState_SESSION_STATE_ESTABLISHED || !at.IsZero() {
		t.Fatalf("a peer that is not up: %v %v", state, at)
	}

	ctx := context.Background()
	port := freePort(t)
	fabric := startServerAt(t, 65500, "198.51.100.1", "127.0.0.1", port)
	if err := fabric.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
		Conf: &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65000}, Transport: &apipb.Transport{PassiveMode: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
		Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65500},
		Transport: &apipb.Transport{RemotePort: uint32(port)}, //nolint:gosec // test port
		Timers:    &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if state, at := u.sessionState(ctx, "127.0.0.1"); state == apipb.PeerState_SESSION_STATE_ESTABLISHED {
			if d := time.Since(at); d < -time.Second || d > 30*time.Second {
				t.Fatalf("uptime %v is not the establishment", at)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("session not established")
}

// Every peer waits idleHoldAfterReset (1 s, FRR BGP_INIT_START_TIMER)
// after a reset, not the GoBGP default of 30 s. ConnectAtOnce is off unless
// BGP_CONNECT_AT_ONCE sets it (#811: connection collisions after a carrier
// loss).
func TestPeersIdleHoldAfterReset(t *testing.T) {
	for _, atOnce := range []bool{false, true} {
		cfg := &Config{ASN: 65000, ConnectAtOnce: atOnce}
		for name, p := range map[string]*apipb.Peer{
			"numbered":  buildNumberedPeer(cfg, "10.0.0.1", ipv4Families()),
			"interface": buildInterfacePeer(cfg, "eth1", "fe80::2%eth1", ipv4Families()),
			"session":   buildSessionPeer(cfg, &network.BGPSession{Name: "10.0.0.1", Internal: true}, "10.0.0.1"),
		} {
			if got := p.GetTimers().GetConfig().GetIdleHoldTimeAfterReset(); got != idleHoldAfterReset {
				t.Errorf("%s peer: IdleHoldTimeAfterReset = %d, want %d", name, got, idleHoldAfterReset)
			}
			if got := p.GetTimers().GetConfig().GetConnectAtOnce(); got != atOnce {
				t.Errorf("%s peer: ConnectAtOnce = %v, want %v", name, got, atOnce)
			}
		}
	}
}

// After a reset (BFD down) the peer leaves Idle within 3 s and the session
// is Established again. With the GoBGP default idle hold time the peer
// stays in Idle for 30 s. The other side is passive, like a peer that only
// accepts: a GoBGP peer that receives the Cease stays in Idle for 2 s (the
// idle hold time doubles, like v_start of FRR), and two such peers
// connecting at each other can miss each other.
func TestResetPeerReestablishes(t *testing.T) {
	ctx := context.Background()
	freePort := func() int32 {
		var lc net.ListenConfig
		l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return int32(l.Addr().(*net.TCPAddr).Port) //nolint:gosec // a TCP port
	}
	type side struct {
		srv  *server.BgpServer
		asn  uint32
		id   string
		port int32
	}
	a, b := side{asn: 65001, id: "192.0.2.1", port: freePort()}, side{asn: 65002, id: "192.0.2.2", port: freePort()}
	for _, s := range []*side{&a, &b} {
		s.srv = server.NewBgpServer()
		go s.srv.Serve()
		t.Cleanup(s.srv.Stop)
		if err := s.srv.StartBgp(ctx, &apipb.StartBgpRequest{Global: &apipb.Global{
			Asn: s.asn, RouterId: s.id, ListenPort: s.port, ListenAddresses: []string{"127.0.0.1"},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]*side{{&a, &b}, {&b, &a}} {
		local, remote := pair[0], pair[1]
		p := buildSessionPeer(&Config{ASN: local.asn}, &network.BGPSession{
			Name: "127.0.0.1", RemoteASN: remote.asn, ConnectRetry: 1, IPv4Unicast: true,
		}, "127.0.0.1")
		p.Transport.RemotePort = uint32(remote.port) //nolint:gosec // a TCP port
		p.Transport.PassiveMode = local == &b
		if err := local.srv.AddPeer(ctx, &apipb.AddPeerRequest{Peer: p}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() apipb.PeerState_SessionState {
		var st apipb.PeerState_SessionState
		_ = a.srv.ListPeer(ctx, &apipb.ListPeerRequest{}, func(p *apipb.Peer) { st = p.GetState().GetSessionState() })
		return st
	}
	wait := func(what string, ok func(apipb.PeerState_SessionState) bool, within time.Duration) {
		start := time.Now()
		for !ok(state()) {
			if time.Since(start) > within {
				t.Fatalf("%s not reached within %s, state %s", what, within, state())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	is := func(want apipb.PeerState_SessionState) func(apipb.PeerState_SessionState) bool {
		return func(st apipb.PeerState_SessionState) bool { return st == want }
	}
	wait("Established", is(apipb.PeerState_SESSION_STATE_ESTABLISHED), 30*time.Second)
	// Like FRR (bgp_bfd.c), the peer gets Cease/BFD Down (RFC 9384).
	reasons := make(chan string, 16)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := b.srv.WatchEvent(wctx, server.WatchEventMessageCallbacks{
		OnPeerUpdate: func(e *apiutil.WatchEventMessage_PeerEvent, _ time.Time) {
			if m := e.Peer.State.DisconnectMessage; m != "" {
				select {
				case reasons <- m:
				default:
				}
			}
		},
	}, server.WatchPeer()); err != nil {
		t.Fatal(err)
	}
	(&UnderlayTier{bgp: a.srv, log: slog.Default()}).resetPeer(ctx, "127.0.0.1", "BFD down")
	start := time.Now()
	wait("Idle after the reset", is(apipb.PeerState_SESSION_STATE_IDLE), 3*time.Second)
	select {
	case m := <-reasons:
		if !strings.Contains(m, "subcode 10(bfd down)") {
			t.Errorf("the peer saw %q, want Cease/BFD Down", m)
		}
	case <-time.After(3 * time.Second):
		t.Error("the peer reported no disconnect reason")
	}
	wait("leaving Idle", func(st apipb.PeerState_SessionState) bool { return st != apipb.PeerState_SESSION_STATE_IDLE }, 3*time.Second)
	t.Logf("connecting %s after the reset", time.Since(start).Round(100*time.Millisecond))
	wait("Established again", is(apipb.PeerState_SESSION_STATE_ESTABLISHED), 15*time.Second)
	t.Logf("established again %s after the reset", time.Since(start).Round(100*time.Millisecond))

	// The carrier-loss cases (a reset of a peer without a session, a reset
	// that shortens a running idle hold timer) are not tested here: BOOTy
	// sets IdleHoldTimeAfterReset to 1 s, and since the fork tag
	// v4.10.0-booty.17 an administrative shutdown leaves 1 s as well, so
	// BOOTy cannot tell the fix from no fix. The fork tests them with other
	// values: TestResetPeerWhileAdminDownAppliesIdleHoldTimeAfterReset,
	// TestResetPeerShortensRunningIdleHoldTimer and
	// TestResetAndEnableOfStoppedPeerSendNothing (pkg/server).
}

// The start race of the prodtpl lab, reproduced with a real GoBGP: a peer
// (the RR) that sent its OPEN but not yet its KEEPALIVE holds our session
// in OpenConfirm. A BFD Down then (the RR re-keys BFD when its own side
// comes up) does not reset the session, which comes up with the
// KEEPALIVE.
func TestBFDDownInOpenConfirm(t *testing.T) {
	ctx := context.Background()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	resets := 0
	u := &UnderlayTier{log: slog.Default(), bgp: startServer(t, 65000, "192.0.2.17", -1),
		resetBGP: func(context.Context, string, string) { resets++ }}
	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
		Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65500},
		Transport: &apipb.Transport{RemotePort: uint32(ln.Addr().(*net.TCPAddr).Port)}, //nolint:gosec // test port
		Timers:    &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
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
	go func() { _, _ = io.Copy(io.Discard, conn) }() // our OPEN and KEEPALIVEs
	open, err := bgp.NewBGPOpenMessage(65500, 90, netip.MustParseAddr("198.51.100.1"), []bgp.OptionParameterInterface{
		bgp.NewOptionParameterCapability([]bgp.ParameterCapabilityInterface{
			bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC), bgp.NewCapFourOctetASNumber(65500),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	send(open)
	waitState := func(want apipb.PeerState_SessionState) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if state, _ := u.sessionState(ctx, "127.0.0.1"); state == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("session not %v", want)
	}
	waitState(apipb.PeerState_SESSION_STATE_OPENCONFIRM)
	p := &bfd.Peer{Name: "127.0.0.1"}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	if resets != 0 {
		t.Fatal("a BFD Down in OpenConfirm reset the session")
	}
	u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone) // the re-keyed session
	send(bgp.NewBGPKeepAliveMessage())
	waitState(apipb.PeerState_SESSION_STATE_ESTABLISHED)
}

// A BFD Down that did not reset a starting session is checked again when
// the session is Established: it is reset if BFD stays Down for the grace
// time, kept if BFD comes Up again, and left alone if the session does
// not come up at all. ListPeer can still show Active when the Down
// arrives (TestBFDDownWhenPeerGetsOurKeepalive).
func TestBFDDownWhileStartingRecheck(t *testing.T) {
	for _, tt := range []struct {
		name    string
		first   apipb.PeerState_SessionState
		then    apipb.PeerState_SessionState
		bfdUp   bool
		wantRst bool
	}{
		{"BFD stays down", apipb.PeerState_SESSION_STATE_OPENCONFIRM, apipb.PeerState_SESSION_STATE_ESTABLISHED, false, true},
		{"BFD up again", apipb.PeerState_SESSION_STATE_OPENCONFIRM, apipb.PeerState_SESSION_STATE_ESTABLISHED, true, false},
		{"session fails", apipb.PeerState_SESSION_STATE_OPENCONFIRM, apipb.PeerState_SESSION_STATE_ACTIVE, false, false},
		{"Active first, BFD stays down", apipb.PeerState_SESSION_STATE_ACTIVE, apipb.PeerState_SESSION_STATE_ESTABLISHED, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			state, at := tt.first, time.Time{}
			resets := make(chan struct{}, 2)
			u := &UnderlayTier{log: slog.Default(), bfdUpGrace: 200 * time.Millisecond,
				resetBGP: func(context.Context, string, string) { resets <- struct{}{} },
				bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
					mu.Lock()
					defer mu.Unlock()
					return state, at
				},
			}
			p := &bfd.Peer{Name: "198.51.100.1"}
			u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
			time.Sleep(50 * time.Millisecond)
			if len(resets) != 0 {
				t.Fatalf("reset in %v", tt.first)
			}
			if tt.bfdUp {
				u.bfdChanged(p, bfd.StateDown, bfd.StateInit, bfd.DiagNone)
				u.bfdChanged(p, bfd.StateInit, bfd.StateUp, bfd.DiagNone)
			}
			mu.Lock()
			state = tt.then
			if tt.then == apipb.PeerState_SESSION_STATE_ESTABLISHED {
				at = time.Now()
			}
			mu.Unlock()
			select {
			case <-resets:
				if !tt.wantRst {
					t.Fatal("reset, want none")
				}
			case <-time.After(time.Second):
				if tt.wantRst {
					t.Fatal("no reset after the grace time")
				}
			}
		})
	}
}

// A recheck checks the session it was started for. When BOOTy restarts
// the peer (Idle, deleted and added again, or a new uptime) and the new session comes up while BFD
// is still Down, the recheck ends without a reset: the new session has
// its own BFD Down.
func TestBFDRecheckEndsWithItsSession(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name    string
		first   apipb.PeerState_SessionState
		firstT  time.Time
		between []apipb.PeerState_SessionState // states before the new session
	}{
		{"restarted while coming up", apipb.PeerState_SESSION_STATE_ACTIVE, time.Time{}, []apipb.PeerState_SessionState{apipb.PeerState_SESSION_STATE_IDLE}},
		{"restarted after it came up", apipb.PeerState_SESSION_STATE_ESTABLISHED, now, nil},
		{"deleted and added again", apipb.PeerState_SESSION_STATE_ACTIVE, time.Time{}, []apipb.PeerState_SessionState{apipb.PeerState_SESSION_STATE_UNSPECIFIED}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			state, at := tt.first, tt.firstT
			resets := make(chan struct{}, 2)
			u := &UnderlayTier{log: slog.Default(), bfdUpGrace: 200 * time.Millisecond, now: func() time.Time { return now },
				resetBGP: func(context.Context, string, string) { resets <- struct{}{} },
				bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
					mu.Lock()
					defer mu.Unlock()
					return state, at
				},
			}
			p := &bfd.Peer{Name: "198.51.100.1"}
			u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
			time.Sleep(50 * time.Millisecond)
			for _, st := range tt.between {
				mu.Lock()
				state, at = st, time.Time{}
				mu.Unlock()
				time.Sleep(50 * time.Millisecond)
			}
			mu.Lock()
			state, at = apipb.PeerState_SESSION_STATE_ESTABLISHED, now.Add(5*time.Second)
			mu.Unlock()
			select {
			case <-resets:
				t.Fatal("the recheck reset the restarted session")
			case <-time.After(time.Second):
			}
			u.stopRechecks()
		})
	}
}

// A BFD flap while a recheck runs does not start a second one: the
// session is reset once, after the grace time from the last Down.
func TestBFDRecheckOncePerPeer(t *testing.T) {
	var mu sync.Mutex
	state, at := apipb.PeerState_SESSION_STATE_OPENCONFIRM, time.Time{}
	var resets atomic.Int32
	u := &UnderlayTier{log: slog.Default(), bfdUpGrace: 200 * time.Millisecond,
		resetBGP: func(context.Context, string, string) { resets.Add(1) },
		bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
			mu.Lock()
			defer mu.Unlock()
			return state, at
		},
	}
	p := &bfd.Peer{Name: "198.51.100.1"}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone)
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	mu.Lock()
	state, at = apipb.PeerState_SESSION_STATE_ESTABLISHED, time.Now()
	mu.Unlock()
	time.Sleep(time.Second)
	u.stopRechecks()
	if n := resets.Load(); n != 1 {
		t.Fatalf("resets = %d, want 1", n)
	}
}

// A Down for a new session that arrives while the recheck of the old one
// still runs (BOOTy restarted the peer within a tick) is checked: the
// recheck binds to the new session and resets it if BFD stays Down.
func TestBFDRecheckRebindsToNewSession(t *testing.T) {
	var mu sync.Mutex
	u1 := time.Now().Truncate(time.Second)
	state, at := apipb.PeerState_SESSION_STATE_ESTABLISHED, u1
	var resets atomic.Int32
	u := &UnderlayTier{log: slog.Default(), bfdUpGrace: 300 * time.Millisecond,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return at
		},
		resetBGP: func(context.Context, string, string) { resets.Add(1) },
		bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
			mu.Lock()
			defer mu.Unlock()
			return state, at
		},
	}
	p := &bfd.Peer{Name: "198.51.100.1"}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown) // the old session's Down
	// Within one tick: BFD Up, BOOTy restarts the peer, the new session's Down.
	u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone)
	mu.Lock()
	at = u1.Add(5 * time.Second)
	mu.Unlock()
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	time.Sleep(time.Second)
	u.stopRechecks()
	if n := resets.Load(); n != 1 {
		t.Fatalf("resets = %d, want 1: the new session's Down with BFD down for the grace time", n)
	}
}

// A recheck that ended (BFD came Up again) leaves no entry behind: the
// next Down starts a new recheck, which resets the session if BFD stays
// Down.
func TestBFDRecheckAfterAnEndedOne(t *testing.T) {
	var resets atomic.Int32
	u := &UnderlayTier{log: slog.Default(), bfdUpGrace: 200 * time.Millisecond,
		resetBGP: func(context.Context, string, string) { resets.Add(1) },
		bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
			return apipb.PeerState_SESSION_STATE_OPENCONFIRM, time.Time{}
		},
	}
	p := &bfd.Peer{Name: "198.51.100.1"}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		u.checkMu.Lock()
		n := len(u.rechecks)
		u.checkMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the recheck did not end after BFD came Up")
		}
	}
	uptime := time.Now()
	u.bgpSession = func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
		return apipb.PeerState_SESSION_STATE_ESTABLISHED, uptime
	}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	time.Sleep(time.Second)
	u.stopRechecks()
	if n := resets.Load(); n != 1 {
		t.Fatalf("resets = %d, want 1", n)
	}
}

// A session that is Established with an uptime when BFD goes Down (the
// same second) keeps it when BFD is Up again inside the grace time.
func TestBFDDownRecheckWaitsForGrace(t *testing.T) {
	now := time.Now()
	resets := make(chan struct{}, 1)
	u := &UnderlayTier{log: slog.Default(), bfdUpGrace: 200 * time.Millisecond, now: func() time.Time { return now },
		resetBGP: func(context.Context, string, string) { resets <- struct{}{} },
		bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
			return apipb.PeerState_SESSION_STATE_ESTABLISHED, now
		},
	}
	p := &bfd.Peer{Name: "198.51.100.1"}
	u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	time.Sleep(100 * time.Millisecond)
	u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone)
	select {
	case <-resets:
		t.Fatal("reset although BFD came Up within the grace time")
	case <-time.After(500 * time.Millisecond):
	}
	u.stopRechecks()
}

// The grace time is the detection time at the slow transmit interval,
// longer than the 1 s the RR's re-keyed session needs to come Up again.
func TestBFDRecheckGrace(t *testing.T) {
	for mult, want := range map[uint8]time.Duration{0: 3 * time.Second, 2: 2 * time.Second, 3: 3 * time.Second, 5: 5 * time.Second} {
		got := bfdRecheckGrace(&bfd.Peer{Timers: bfd.Timers{DetectMult: mult}})
		if got != want || got <= bfd.SlowTx {
			t.Errorf("multiplier %d: grace %v, want %v (more than %v)", mult, got, want, bfd.SlowTx)
		}
	}
}

// Teardown stops a recheck that still waits for the session, so it
// neither leaks nor resets a peer during the teardown.
func TestTeardownStopsBFDRecheck(t *testing.T) {
	resets := 0
	var calls atomic.Int32
	u := &UnderlayTier{cfg: &Config{}, log: slog.Default(), bfdUpGrace: 50 * time.Millisecond,
		resetBGP: func(context.Context, string, string) { resets++ },
		bgpSession: func(context.Context, string) (apipb.PeerState_SessionState, time.Time) {
			calls.Add(1)
			return apipb.PeerState_SESSION_STATE_OPENCONFIRM, time.Time{}
		},
	}
	u.bfdChanged(&bfd.Peer{Name: "198.51.100.1"}, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		_ = u.Teardown(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Teardown waits for the recheck")
	}
	n := calls.Load()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != n || resets != 0 {
		t.Fatalf("recheck still running after Teardown: %d more calls, %d resets", calls.Load()-n, resets)
	}
}

// The start race of the prodtpl lab, as the route reflector sees it: it
// is passive, takes our connection, and comes up when our KEEPALIVE
// arrives; then it re-keys BFD (update-source lo), so our BFD session goes
// Down at once. GoBGP writes that KEEPALIVE before its server loop moves
// the peer out of Active (outgoing connection) or OpenSent (incoming), so
// ListPeer can still show either state. The BFD Down must not reset the
// session in any of them.
func TestBFDDownWhenPeerGetsOurKeepalive(t *testing.T) {
	ctx := context.Background()
	readMsg := func(t *testing.T, conn net.Conn) uint8 {
		t.Helper()
		hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(hdr[16])<<8|int(hdr[17])-bgp.BGP_HEADER_LENGTH)
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatal(err)
		}
		return hdr[18]
	}
	open, err := bgp.NewBGPOpenMessage(65000, 90, netip.MustParseAddr("198.51.100.2"), []bgp.OptionParameterInterface{
		bgp.NewOptionParameterCapability([]bgp.ParameterCapabilityInterface{
			bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC), bgp.NewCapFourOctetASNumber(65000),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	openB, _ := open.Serialize()
	states := map[apipb.PeerState_SessionState]int{}
	for range 20 {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		resets := 0
		u := &UnderlayTier{log: slog.Default(), bgp: startServer(t, 65000, "192.0.2.17", -1),
			resetBGP: func(context.Context, string, string) { resets++ }}
		if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
			Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65000},
			Transport: &apipb.Transport{RemotePort: uint32(ln.Addr().(*net.TCPAddr).Port)}, //nolint:gosec // test port
			Timers:    &apipb.Timers{Config: &apipb.TimersConfig{ConnectRetry: 1, ConnectAtOnce: true}},
		}}); err != nil {
			t.Fatal(err)
		}
		conn, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if typ := readMsg(t, conn); typ != bgp.BGP_MSG_OPEN {
			t.Fatalf("message %d, want our OPEN", typ)
		}
		if _, err := conn.Write(openB); err != nil {
			t.Fatal(err)
		}
		if typ := readMsg(t, conn); typ != bgp.BGP_MSG_KEEPALIVE {
			t.Fatalf("message %d, want our KEEPALIVE", typ)
		}
		state, _ := u.sessionState(ctx, "127.0.0.1")
		states[state]++
		p := &bfd.Peer{Name: "127.0.0.1"}
		u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown) // the RR re-keys BFD
		if resets != 0 {
			t.Fatalf("the BFD Down reset the session in %v (states so far %v)", state, states)
		}
		u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone) // the re-keyed session
		u.stopRechecks()
		_ = conn.Close()
		_ = ln.Close()
	}
	t.Logf("ListPeer states when the peer got our KEEPALIVE: %v", states)
}

// The same race on an incoming connection (dcgw01 is not passive): GoBGP
// writes its KEEPALIVE in OpenSent and moves the peer to OpenConfirm
// later, so ListPeer can still show OpenSent when the peer's BFD Down
// arrives. It does not reset the session either.
func TestBFDDownWhenPeerGetsOurKeepaliveIncoming(t *testing.T) {
	ctx := context.Background()
	readMsg := func(t *testing.T, conn net.Conn) uint8 {
		t.Helper()
		hdr := make([]byte, bgp.BGP_HEADER_LENGTH)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(hdr[16])<<8|int(hdr[17])-bgp.BGP_HEADER_LENGTH)
		if _, err := io.ReadFull(conn, body); err != nil {
			t.Fatal(err)
		}
		return hdr[18]
	}
	open, err := bgp.NewBGPOpenMessage(65000, 90, netip.MustParseAddr("198.51.100.1"), []bgp.OptionParameterInterface{
		bgp.NewOptionParameterCapability([]bgp.ParameterCapabilityInterface{
			bgp.NewCapMultiProtocol(bgp.RF_IPv4_UC), bgp.NewCapFourOctetASNumber(65000),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	openB, _ := open.Serialize()
	states := map[apipb.PeerState_SessionState]int{}
	for range 20 {
		port := freePort(t)
		resets := 0
		u := &UnderlayTier{log: slog.Default(), bgp: startServer(t, 65000, "192.0.2.17", port),
			resetBGP: func(context.Context, string, string) { resets++ }}
		if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: &apipb.Peer{
			Conf:      &apipb.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65000},
			Transport: &apipb.Transport{PassiveMode: true},
		}}); err != nil {
			t.Fatal(err)
		}
		// GoBGP closes a connection that arrives while the peer is Idle.
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if state, _ := u.sessionState(ctx, "127.0.0.1"); state == apipb.PeerState_SESSION_STATE_ACTIVE {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("peer not Active")
			}
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(openB); err != nil {
			t.Fatal(err)
		}
		for typ := readMsg(t, conn); typ != bgp.BGP_MSG_KEEPALIVE; typ = readMsg(t, conn) {
			if typ != bgp.BGP_MSG_OPEN {
				t.Fatalf("message %d, want our OPEN or KEEPALIVE", typ)
			}
		}
		state, _ := u.sessionState(ctx, "127.0.0.1")
		states[state]++
		p := &bfd.Peer{Name: "127.0.0.1"}
		u.bfdChanged(p, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown) // the RR re-keys BFD
		if resets != 0 {
			t.Fatalf("the BFD Down reset the session in %v (states so far %v)", state, states)
		}
		u.bfdChanged(p, bfd.StateDown, bfd.StateUp, bfd.DiagNone)
		u.stopRechecks()
		_ = conn.Close()
	}
	t.Logf("ListPeer states when the peer got our KEEPALIVE: %v", states)
}
