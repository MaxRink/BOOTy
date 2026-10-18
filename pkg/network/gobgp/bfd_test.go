//go:build linux

package gobgp

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"

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
// the second the BGP session came up does not reset it; an earlier uptime,
// an uptime in the future (the clock stepped back) and a session that is
// not up reset it.
func TestBFDChangedAtBGPStart(t *testing.T) {
	now := time.Unix(1_000_000, 500_000_000)
	for _, tt := range []struct {
		name  string
		at    time.Time
		up    bool
		reset bool
	}{
		{"same second", now.Add(-400 * time.Millisecond), true, false},
		{"up for 2 s", now.Add(-2 * time.Second), true, true},
		{"uptime in the future (clock stepped back)", now.Add(time.Hour), true, true},
		{"not up", now, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resets := 0
			u := &UnderlayTier{log: slog.Default(), now: func() time.Time { return now },
				resetBGP:  func(context.Context, string, string) { resets++ },
				bgpUptime: func(context.Context, string) (time.Time, bool) { return tt.at, tt.up },
			}
			u.bfdChanged(&bfd.Peer{Name: "198.51.100.2"}, bfd.StateUp, bfd.StateDown, bfd.DiagNeighborSaysDown)
			if (resets == 1) != tt.reset {
				t.Fatalf("resets = %d, want a reset %v", resets, tt.reset)
			}
		})
	}
}

// establishedAt reads the uptime of an established GoBGP peer.
func TestEstablishedAt(t *testing.T) {
	u := &UnderlayTier{}
	if _, ok := u.establishedAt(context.Background(), "192.0.2.1"); ok {
		t.Fatal("established without a BGP server")
	}
	u.bgp = startServer(t, 65000, "192.0.2.17", -1)
	if err := u.bgp.AddPeer(context.Background(), &apipb.AddPeerRequest{Peer: buildNumberedPeer(&Config{ASN: 65000}, "192.0.2.1", ipv4Families())}); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.establishedAt(context.Background(), "192.0.2.1"); ok {
		t.Fatal("a peer that is not up is established")
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
		if at, ok := u.establishedAt(ctx, "127.0.0.1"); ok {
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
// after a reset, not the GoBGP default of 30 s.
func TestPeersIdleHoldAfterReset(t *testing.T) {
	cfg := &Config{ASN: 65000}
	for name, p := range map[string]*apipb.Peer{
		"numbered":  buildNumberedPeer(cfg, "10.0.0.1", ipv4Families()),
		"interface": buildInterfacePeer(cfg, "eth1", "fe80::2%eth1", ipv4Families()),
		"session":   buildSessionPeer(cfg, &network.BGPSession{Name: "10.0.0.1", Internal: true}, "10.0.0.1"),
	} {
		if got := p.GetTimers().GetConfig().GetIdleHoldTimeAfterReset(); got != idleHoldAfterReset {
			t.Errorf("%s peer: IdleHoldTimeAfterReset = %d, want %d", name, got, idleHoldAfterReset)
		}
	}
}

// After a reset (BFD down) the peer leaves Idle within 3 s and the session
// is Established again. With the GoBGP default idle hold time the peer
// stays in Idle for 30 s. The other side is passive, like a peer that only
// accepts: a GoBGP peer that receives the Cease stays in Idle for a fixed
// 5 s, and two such peers connecting at each other can miss each other.
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
	wait("Established", is(apipb.PeerState_ESTABLISHED), 30*time.Second)
	if err := a.srv.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: "127.0.0.1", Communication: "BFD down"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	wait("Idle after the reset", is(apipb.PeerState_IDLE), 3*time.Second)
	wait("leaving Idle", func(st apipb.PeerState_SessionState) bool { return st != apipb.PeerState_IDLE }, 3*time.Second)
	t.Logf("connecting %s after the reset", time.Since(start).Round(100*time.Millisecond))
	wait("Established again", is(apipb.PeerState_ESTABLISHED), 15*time.Second)
	t.Logf("established again %s after the reset", time.Since(start).Round(100*time.Millisecond))
}
