//go:build linux

package gobgp

import (
	"context"
	"log/slog"
	"testing"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/network"
)

func testVRFTable(vrf string) int {
	return map[string]int{"Vrf_underlay": 10, "Vrf_peers": 20}[vrf]
}

// Like FRR, the BGP_PEER_MODE peers on NICs of VRF_NAME use that VRF's
// table, and the numbered peers that of BGP_NEIGHBOR_VRF.
func TestNativeTables(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		cfg                  Config
		unnumbered, numbered int
		vrfs                 map[int]string
	}{
		{"no VRF", Config{}, unix.RT_TABLE_MAIN, unix.RT_TABLE_MAIN, map[int]string{}},
		{"VRF_NAME only", Config{VRFName: "Vrf_underlay"}, 10, unix.RT_TABLE_MAIN, map[int]string{10: "Vrf_underlay"}},
		{"both", Config{VRFName: "Vrf_underlay", NeighborVRF: "Vrf_peers"}, 10, 20, map[int]string{10: "Vrf_underlay", 20: "Vrf_peers"}},
	} {
		un, num, vrfs := nativeTables(&tt.cfg, testVRFTable)
		if un != tt.unnumbered || num != tt.numbered || len(vrfs) != len(tt.vrfs) {
			t.Errorf("%s: tables %d %d vrfs %v, want %d %d %v", tt.name, un, num, vrfs, tt.unnumbered, tt.numbered, tt.vrfs)
		}
		for k, v := range tt.vrfs {
			if vrfs[k] != v {
				t.Errorf("%s: vrfs %v, want %v", tt.name, vrfs, tt.vrfs)
			}
		}
	}
}

// fast-external-failover: explicit wins, else on with sessions and off for
// the BGP_PEER_MODE peers.
func TestFastExternalFailoverDefault(t *testing.T) {
	on, off := true, false
	for _, tt := range []struct {
		name string
		cfg  network.Config
		want bool
	}{
		{"peer mode, unset", network.Config{}, false},
		{"sessions, unset", network.Config{BGPSessions: []network.BGPSession{{Name: "eth0"}}}, true},
		{"peer mode, on", network.Config{BGPFastExternalFailover: &on}, true},
		{"sessions, off", network.Config{BGPSessions: []network.BGPSession{{Name: "eth0"}}, BGPFastExternalFailover: &off}, false},
	} {
		if got := fastExternalFailover(&tt.cfg); got != tt.want {
			t.Errorf("%s: fastExternalFailover = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A native unnumbered peer is recorded for the FIB writer and the carrier
// watch; the selector gives it BGP_LOCAL_AS and the VRF_NAME table, and a
// numbered peer the router AS and the BGP_NEIGHBOR_VRF table.
func TestNativeUnnumberedPeerSelector(t *testing.T) {
	ctx := context.Background()
	bgp := server.NewBgpServer()
	go bgp.Serve()
	t.Cleanup(bgp.Stop)
	if err := bgp.StartBgp(ctx, &apipb.StartBgpRequest{Global: &apipb.Global{Asn: 4210000017, RouterId: "192.0.2.19", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	u := &UnderlayTier{
		bgp: bgp,
		cfg: &Config{ASN: 4210000017, LocalAS: 65501, VRFName: "Vrf_underlay", NeighborVRF: "Vrf_peers"},
		log: slog.Default(),
		discoverPeer: func(context.Context, string) (string, error) {
			return "fe80::2%eth9", nil
		},
	}
	if err := u.addInterfacePeer(ctx, "eth9", ipv4Families()); err != nil {
		t.Fatal(err)
	}
	if u.peerDevs["fe80::2"] != "eth9" || u.ifacePeers["eth9"] != "fe80::2%eth9" {
		t.Fatalf("peerDevs %v ifacePeers %v, want fe80::2 on eth9", u.peerDevs, u.ifacePeers)
	}
	sel, vrfs := u.fibSelector(testVRFTable)
	if got := sel.localAS("fe80::2"); got != 65501 {
		t.Errorf("local AS of the unnumbered peer = %d, want BGP_LOCAL_AS 65501", got)
	}
	if got := sel.table("fe80::2"); got != 10 {
		t.Errorf("table of the unnumbered peer = %d, want the VRF_NAME table 10", got)
	}
	if got := sel.localAS("198.51.100.1"); got != 4210000017 {
		t.Errorf("local AS of a numbered peer = %d, want the router AS", got)
	}
	if got := sel.table("198.51.100.1"); got != 20 || vrfs[20] != "Vrf_peers" || vrfs[10] != "Vrf_underlay" {
		t.Errorf("table of a numbered peer = %d, vrfs %v; want 20 and both VRFs", got, vrfs)
	}

	// frr.conf sessions: an interface session in Vrf_underlay gets its
	// VRF table and its local-as, a session in the default VRF the main
	// table and the router AS.
	u = &UnderlayTier{cfg: &Config{ASN: 4210000017, Sessions: []network.BGPSession{
		{Name: "eth9", Interface: true, VRF: "Vrf_underlay", LocalASN: 65501},
		{Name: "198.51.100.1", VRF: "default"},
	}}}
	u.recordInterfacePeer("eth9", "fe80::2%eth9")
	sel, vrfs = u.fibSelector(testVRFTable)
	if got := sel.table("fe80::2"); got != 10 || vrfs[10] != "Vrf_underlay" {
		t.Errorf("table of the Vrf_underlay session = %d, vrfs %v; want 10", got, vrfs)
	}
	if got := sel.localAS("fe80::2"); got != 65501 {
		t.Errorf("local AS of the Vrf_underlay session = %d, want its local-as 65501", got)
	}
	if got := sel.table("198.51.100.1"); got != unix.RT_TABLE_MAIN {
		t.Errorf("table of the default VRF session = %d, want main", got)
	}
	if got := sel.localAS("198.51.100.1"); got != 4210000017 {
		t.Errorf("local AS of the default VRF session = %d, want the router AS", got)
	}
}
