//go:build linux

package gobgp

import (
	"context"
	"log/slog"
	"testing"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"

	"github.com/telekom/BOOTy/pkg/network"
)

// pathMED returns the MED of a path.
func pathMED(t *testing.T, p *apipb.Path) (uint32, bool) {
	t.Helper()
	for _, a := range p.GetPattrs() {
		if m := a.GetMultiExitDisc(); m != nil {
			return m.GetMed(), true
		}
	}
	return 0, false
}

// rdString returns an RD as "admin:assigned".
func rdString(t *testing.T, a *apipb.RouteDistinguisher) string {
	t.Helper()
	rd, err := apiutil.UnmarshalRD(a)
	if err != nil {
		t.Fatal(err)
	}
	return rd.String()
}

// The underlay /32 carries MED 0, like an FRR "network" route.
func TestAnnounceUnderlayRouteMED(t *testing.T) {
	ctx := context.Background()
	s := startServer(t, 4210000017, "192.0.2.17", -1)
	u := &UnderlayTier{bgp: s, cfg: &Config{RouterID: "192.0.2.17"}, log: slog.Default()}
	if err := u.announceUnderlayRoute(ctx); err != nil {
		t.Fatal(err)
	}
	var paths []*apipb.Path
	for _, ps := range listPaths(t, s, ipv4Families()[0].GetConfig().GetFamily()) {
		paths = append(paths, ps...)
	}
	if len(paths) != 1 {
		t.Fatalf("%d paths, want the underlay /32", len(paths))
	}
	if med, ok := pathMED(t, paths[0]); !ok || med != 0 {
		t.Fatalf("MED = %d (%v), want 0", med, ok)
	}
}

// The RDs follow FRR: EVPN_RD or the frr.conf rd wins; else
// "<router-id>:<vrf_rd_id>" for the L3VNI routes and the next id after
// the instances for the L2VNI routes. Without frr.conf the native peers
// stand for the template: the neighbor VRF instance first.
func TestEVPNRDs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cfg    network.Config
		l3, l2 string
	}{
		{"explicit", network.Config{EVPNRD: "65000:7", EVPNRDIndex: 2, BGPInstances: 2}, "65000:7", "65000:7"},
		{"frr.conf", network.Config{EVPNRDIndex: 2, BGPInstances: 2}, "192.0.2.17:2", "192.0.2.17:3"},
		{"frr.conf L3VNI first", network.Config{EVPNRDIndex: 1, BGPInstances: 2}, "192.0.2.17:1", "192.0.2.17:3"},
		{"native neighbor VRF", network.Config{BGPNeighborVRF: "Vrf_underlay"}, "192.0.2.17:2", "192.0.2.17:3"},
		{"native VRF_NAME, default overlay", network.Config{VRFName: "Vrf_underlay", OverlayVRFSet: true}, "192.0.2.17:2", "192.0.2.17:3"},
		{"native one VRF", network.Config{VRFName: "Vrf_underlay"}, "192.0.2.17:1", "192.0.2.17:2"},
		{"native default VRF", network.Config{}, "192.0.2.17:1", "192.0.2.17:2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l3, l2 := evpnRDs(&tt.cfg, "192.0.2.17")
			if l3 != tt.l3 || l2 != tt.l2 {
				t.Fatalf("RDs = %s, %s; want %s, %s", l3, l2, tt.l3, tt.l2)
			}
		})
	}
}

// The Type-5 routes carry the IPv4 RD of the router ID, an invalid
// EVPN_RD is rejected.
func TestRouteDistinguisher(t *testing.T) {
	o := &OverlayTier{cfg: &Config{RouterID: "192.0.2.17", RD: "192.0.2.17:2"}}
	a, err := o.routeDistinguisher(o.cfg.RD)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := apiutil.UnmarshalRD(a)
	if err != nil || rd.String() != "192.0.2.17:2" {
		t.Fatalf("RD = %v (%v)", rd, err)
	}
	if a, err = o.routeDistinguisher(""); err != nil {
		t.Fatal(err)
	}
	if rd, _ = apiutil.UnmarshalRD(a); rd.String() != "192.0.2.17:1" {
		t.Fatalf("empty RD = %v, want 192.0.2.17:1", rd)
	}
	for rd, ok := range map[string]bool{
		"nope": false, "65000:7": true, "192.0.2.17:2": true, "4210000017:9": true,
		"4210000017:70000": false, "1.2.3.4:70000": false, "65000:5000000000": false,
	} {
		c := &Config{ASN: 65000, RouterID: "10.0.0.1", PeerMode: network.PeerModeUnnumbered, ProvisionVNI: 100,
			ProvisionGateway: testProvisionGateway, MinEstablishedPeers: 1, RD: rd}
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("Validate with RD %s = %v", rd, err)
		}
		c.RD, c.L2RD = "", rd
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("Validate with L2 RD %s = %v", rd, err)
		}
	}
}
