//go:build linux

package gobgp

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/bfd"
)

// A peer is moved only after relearnGrace without a session while its
// interface has carrier, and only to an address that movedPeer found. A
// session or a carrier loss restarts the grace time.
func TestRelearnerCheck(t *testing.T) {
	var (
		up, carrier = false, true
		next        string
		moved       int
		replaced    []string
	)
	r := &relearner{
		peers:       func() map[string]string { return map[string]string{"eth1": "fe80::1%eth1"} },
		established: func(context.Context, string) bool { return up },
		carrier:     func(string) bool { return carrier },
		moved:       func(context.Context, string, string) string { moved++; return next },
		replace: func(_ context.Context, iface, old, addr string) error {
			replaced = append(replaced, iface+" "+old+" "+addr)
			return nil
		},
		log: slog.Default(),
	}
	t0 := time.Unix(1000, 0)
	at := func(d time.Duration) { r.check(context.Background(), t0.Add(d)) }
	at(0)
	at(2 * time.Second)
	if moved != 0 {
		t.Fatalf("looked for a new address %d times within the grace time", moved)
	}
	at(3 * time.Second) // the old address still answers
	if moved != 1 || len(replaced) != 0 {
		t.Fatalf("moved %d, replaced %v; want one look and no move", moved, replaced)
	}
	up = true
	at(4 * time.Second)
	up = false
	at(5 * time.Second)
	carrier = false
	at(8 * time.Second)
	carrier = true
	at(9 * time.Second)
	next = "fe80::2%eth1"
	at(11 * time.Second)
	if moved != 1 || len(replaced) != 0 {
		t.Fatalf("a session or carrier loss did not restart the grace time: moved %d, replaced %v", moved, replaced)
	}
	at(12 * time.Second)
	if want := []string{"eth1 fe80::1%eth1 fe80::2%eth1"}; !slices.Equal(replaced, want) {
		t.Fatalf("replaced %v, want %v", replaced, want)
	}
	at(13 * time.Second)
	if len(replaced) != 1 {
		t.Fatalf("a moved peer starts a new grace time, replaced %v", replaced)
	}
}

// The peer moves to the new address with the same configuration, and so
// do its route-map neighbor sets, its BFD session and the address maps.
// An address that another interface peer shares stays in the sets.
func TestReplaceInterfacePeer(t *testing.T) {
	for _, shared := range []bool{false, true} {
		ctx := context.Background()
		s := startServer(t, 4210000017, "192.0.2.17", -1)
		cfg := &Config{ASN: 4210000017, LocalAS: 65501, AllowASIn: 3}
		u := &UnderlayTier{bgp: s, cfg: cfg, log: slog.Default()}
		u.recordInterfacePeer("lo", "fe80::1%lo")
		if shared {
			u.recordInterfacePeer("eth9", "fe80::1%eth9")
		}
		peer := buildInterfacePeer(cfg, "lo", "fe80::1%lo", ipv4Families())
		u.ifacePeerConfs["lo"] = peer
		if err := s.AddPeer(ctx, &apipb.AddPeerRequest{Peer: peer}); err != nil {
			t.Fatal(err)
		}
		for name, list := range map[string][]string{"rm-in": {"fe80::1/128", "fe80::9/128"}, "other": {"fe80::9/128"}} {
			if err := s.AddDefinedSet(ctx, &apipb.AddDefinedSetRequest{DefinedSet: &apipb.DefinedSet{
				DefinedType: apipb.DefinedType_NEIGHBOR, Name: name, List: list}}); err != nil {
				t.Fatal(err)
			}
		}
		u.bfd = bfd.NewManager(slog.Default(), nil)
		if err := u.bfd.Add(&bfd.Peer{Name: "fe80::1%lo", Addr: netip.MustParseAddr("fe80::1%lo"),
			Timers: bfd.Timers{DesiredMinTx: time.Second, RequiredMinRx: time.Second, DetectMult: 3}}); err != nil {
			t.Fatal(err)
		}

		if err := u.replaceInterfacePeer(ctx, "lo", "fe80::1%lo", "fe80::2%lo"); err != nil {
			t.Fatal(err)
		}

		var peers []*apipb.Peer
		_ = s.ListPeer(ctx, &apipb.ListPeerRequest{}, func(p *apipb.Peer) { peers = append(peers, p) })
		if len(peers) != 1 || peers[0].GetConf().GetNeighborAddress() != "fe80::2%lo" ||
			peers[0].GetConf().GetLocalAsn() != 65501 || peers[0].GetConf().GetAllowOwnAsn() != 3 ||
			peers[0].GetTransport().GetBindInterface() != "lo" || len(peers[0].GetAfiSafis()) != 1 {
			t.Fatalf("shared=%v: peers after the move = %v", shared, peers)
		}
		sets := map[string][]string{}
		_ = s.ListDefinedSet(ctx, &apipb.ListDefinedSetRequest{DefinedType: apipb.DefinedType_NEIGHBOR},
			func(d *apipb.DefinedSet) { sets[d.GetName()] = slices.Sorted(slices.Values(d.GetList())) })
		wantIn := []string{"fe80::2/128", "fe80::9/128"}
		if shared {
			wantIn = []string{"fe80::1/128", "fe80::2/128", "fe80::9/128"}
		}
		if !slices.Equal(sets["rm-in"], wantIn) || !slices.Equal(sets["other"], []string{"fe80::9/128"}) {
			t.Fatalf("shared=%v: neighbor sets = %v, want rm-in %v", shared, sets, wantIn)
		}
		if got := u.bfd.Names(); !slices.Equal(got, []string{"fe80::2%lo"}) {
			t.Fatalf("BFD sessions = %v", got)
		}
		if u.ifacePeer("lo") != "fe80::2%lo" || u.peerDev("fe80::2") != "lo" || u.ifacePeerConfs["lo"].GetConf().GetNeighborAddress() != "fe80::2%lo" {
			t.Fatalf("maps not moved: %v %v", u.ifacePeers, u.peerDevs)
		}
		if old := u.peerDev("fe80::1"); old == "lo" || (old == "") == shared {
			t.Fatalf("shared=%v: old address maps to %q", shared, old)
		}
		u.bfd.Close()
	}
}

// A switch that does not answer at setup gets its peer on the placeholder
// address: an frr.conf interface session always, a native peer when its
// NIC has carrier (lo has); a NIC without carrier gets none.
func TestPlaceholderPeers(t *testing.T) {
	ctx := context.Background()
	s := startServer(t, 4210000017, "192.0.2.17", -1)
	none := func(context.Context, string) (string, error) { return "", errors.New("no neighbor") }
	u := &UnderlayTier{bgp: s, log: slog.Default(), discoverPeer: none, cfg: &Config{ASN: 4210000017,
		Sessions: []network.BGPSession{{Name: "eth9", Interface: true, RemoteASN: 65500, IPv4Unicast: true}}}}
	if err := u.addSessionPeers(ctx); err != nil {
		t.Fatal(err)
	}
	if got := u.ifacePeer("eth9"); got != placeholderPeer("eth9") || u.ifacePeerConfs["eth9"] == nil {
		t.Fatalf("session peer = %q", got)
	}
	u.cfg.Sessions = nil
	if err := u.addInterfacePeer(ctx, "lo", ipv4Families()); err != nil {
		t.Fatal(err)
	}
	if got := u.ifacePeer("lo"); got != placeholderPeer("lo") || u.ifacePeerConfs["lo"].GetConf().GetNeighborAddress() != placeholderPeer("lo") {
		t.Fatalf("native peer on lo = %q, configuration %v", got, u.ifacePeerConfs["lo"])
	}
	if err := u.addInterfacePeer(ctx, "nonexistent0", ipv4Families()); err == nil {
		t.Fatal("a NIC without carrier got a peer")
	}
	var n int
	_ = s.ListPeer(ctx, &apipb.ListPeerRequest{}, func(*apipb.Peer) { n++ })
	if n != 2 {
		t.Fatalf("%d GoBGP peers, want 2", n)
	}
}

// The peer moves only when the old address is silent, and only to a
// candidate that answers on the BGP port.
func TestChooseMoved(t *testing.T) {
	var mu sync.Mutex // probes can outlive chooseMoved
	alive := map[string]bool{}
	set := func(addr string, up bool) { mu.Lock(); alive[addr] = up; mu.Unlock() }
	orig := bgpProbe
	t.Cleanup(func() { bgpProbe = orig })
	bgpProbe = func(_ context.Context, addr, _ string) bool { mu.Lock(); defer mu.Unlock(); return alive[addr] }
	cands := []string{"fe80::2%eth1", "fe80::3%eth1"}
	set("fe80::1%eth1", true)
	set("fe80::3%eth1", true)
	if got := chooseMoved(context.Background(), "fe80::1%eth1", cands); got != "" {
		t.Fatalf("moved to %q while the old address answers", got)
	}
	set("fe80::1%eth1", false)
	if got := chooseMoved(context.Background(), "fe80::1%eth1", cands); got != "fe80::3%eth1" {
		t.Fatalf("moved to %q, want the answering fe80::3%%eth1", got)
	}
	set("fe80::3%eth1", false)
	if got := chooseMoved(context.Background(), "fe80::1%eth1", cands); got != "" {
		t.Fatalf("moved to %q, but no candidate answers", got)
	}
}

// A failed move is tried again in the next round; the carrier watch uses
// the relearner's current address; Setup's relearner starts only with
// unnumbered peers and Teardown stops it.
func TestRelearnerRetryAndWiring(t *testing.T) {
	fail := true
	var tries int
	r := &relearner{
		peers:       func() map[string]string { return map[string]string{"eth1": "fe80::1%eth1"} },
		established: func(context.Context, string) bool { return false },
		carrier:     func(string) bool { return true },
		moved:       func(context.Context, string, string) string { return "fe80::2%eth1" },
		replace: func(context.Context, string, string, string) error {
			tries++
			if fail {
				fail = false
				return errors.New("busy")
			}
			return nil
		},
		log: slog.Default(),
	}
	t0 := time.Unix(1000, 0)
	for i := range 6 {
		r.check(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if tries != 2 {
		t.Fatalf("replace tried %d times, want a retry after the failure", tries)
	}

	u := &UnderlayTier{cfg: &Config{}, log: slog.Default()}
	u.recordInterfacePeer("eth1", "fe80::1%eth1")
	w := u.newCarrierWatch(map[string]string{"eth1": "fe80::1%eth1"})
	u.peerMu.Lock()
	u.ifacePeers["eth1"] = "fe80::2%eth1"
	u.peerMu.Unlock()
	if w.current == nil || w.current("eth1") != "fe80::2%eth1" {
		t.Fatal("the carrier watch does not use the current peer address")
	}

	u.startRelearn()
	if u.stopRelearn == nil {
		t.Fatal("no relearner for an unnumbered peer")
	}
	u.stopRelearn()
	select {
	case <-u.relearnDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the relearner did not stop")
	}
	none := &UnderlayTier{cfg: &Config{}, log: slog.Default()}
	if none.startRelearn(); none.stopRelearn != nil {
		t.Fatal("a relearner without unnumbered peers")
	}
}

// Setup starts the relearner after the peers and the carrier watch, and
// Teardown stops it before the server (Setup needs a full stack, so the
// source is checked).
func TestSetupStartsRelearner(t *testing.T) {
	src, err := os.ReadFile("underlay.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	setup := body[strings.Index(body, "func (u *UnderlayTier) Setup("):]
	setup = setup[:strings.Index(setup, "\n}\n")]
	teardown := body[strings.Index(body, "func (u *UnderlayTier) Teardown("):]
	teardown = teardown[:strings.Index(teardown, "\n}\n")]
	if !strings.Contains(setup, "u.startRelearn()") || strings.Index(setup, "u.startRelearn()") < strings.Index(setup, "u.setupBGP(ctx)") {
		t.Fatal("Setup does not start the relearner after the peers")
	}
	if !strings.Contains(teardown, "u.stopRelearn()") || strings.Index(teardown, "u.stopRelearn()") > strings.Index(teardown, "u.bgp.Stop()") {
		t.Fatal("Teardown does not stop the relearner before the BGP server")
	}
}
