package gobgp

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// testPath builds a learned IPv4 path with an RFC 5549 next hop. An empty
// neighbor is a path of BOOTy itself.
func testPath(t *testing.T, neighbor, nexthop string, asPath []uint32, localPref uint32, best bool) *apiutil.Path {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("198.51.100.1/32"))
	if err != nil {
		t.Fatal(err)
	}
	mpReach, err := bgp.NewPathAttributeMpReachNLRI(bgp.RF_IPv4_UC, []bgp.PathNLRI{{NLRI: nlri}}, netip.MustParseAddr(nexthop))
	if err != nil {
		t.Fatal(err)
	}
	p := &apiutil.Path{
		Family: bgp.RF_IPv4_UC,
		Nlri:   nlri,
		Attrs: []bgp.PathAttributeInterface{
			bgp.NewPathAttributeOrigin(0),
			bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, asPath)}),
			mpReach,
			bgp.NewPathAttributeLocalPref(localPref),
		},
		Best:    best,
		PeerASN: asPath[0],
	}
	if neighbor != "" {
		p.PeerAddress = netip.MustParseAddr(neighbor)
	}
	return p
}

// dest is the ListPath result of one destination.
func dest(t *testing.T, prefix string, paths ...*apiutil.Path) fibDest {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	return fibDest{prefix: nlri, paths: paths}
}

func devOf(m map[string]string) func(string) string {
	return func(n string) string { return m[n] }
}

// testSelector selects for one table with the router AS localASN.
func testSelector(localASN uint32, relax bool, devs func(string) string) *fibSelector {
	return &fibSelector{
		relax: relax, peerDev: devs,
		localAS: func(string) uint32 { return localASN },
		table:   func(string) int { return 10 },
	}
}

// Two uplinks to the same leaf: ECMP over both, with the device of each
// link-local next hop (FRR bgp_ecmp_topo1).
func TestSelectRoutesECMPOverUplinks(t *testing.T) {
	d := dest(t, "198.51.100.1/32",
		testPath(t, "fe80::1", "fe80::1", []uint32{65500, 4210000017}, 100, true),
		testPath(t, "fe80::2", "fe80::2", []uint32{65500, 4210000017}, 100, false),
	)
	devs := devOf(map[string]string{"fe80::1": "ens1f0np0", "fe80::2": "ens1f1np1"})
	got, err := testSelector(4210000017, false, devs).selectRoutes([]fibDest{d})
	if err != nil {
		t.Fatal(err)
	}
	want := []fibRoute{{Table: 10, Prefix: netip.MustParsePrefix("198.51.100.1/32"), Nexthops: []fibNexthop{
		{Addr: netip.MustParseAddr("fe80::1"), Dev: "ens1f0np0"},
		{Addr: netip.MustParseAddr("fe80::2"), Dev: "ens1f1np1"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectRoutes() = %+v, want %+v", got, want)
	}
}

// Different AS paths of the same length are ECMP only with
// multipath-relax; a lower local preference never is.
func TestSelectRoutesMultipathRules(t *testing.T) {
	paths := func() []*apiutil.Path {
		return []*apiutil.Path{
			testPath(t, "fe80::1", "fe80::1", []uint32{65500, 65100}, 100, true),
			testPath(t, "fe80::2", "fe80::2", []uint32{65501, 65100}, 100, false),
			testPath(t, "fe80::3", "fe80::3", []uint32{65500, 65100}, 90, false),
		}
	}
	devs := devOf(map[string]string{"fe80::1": "a", "fe80::2": "b", "fe80::3": "c"})
	for _, tt := range []struct {
		relax bool
		want  int
	}{{false, 1}, {true, 2}} {
		got, err := testSelector(4210000017, tt.relax, devs).selectRoutes([]fibDest{dest(t, "198.51.100.1/32", paths()...)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || len(got[0].Nexthops) != tt.want {
			t.Errorf("relax=%v: routes = %+v, want %d next hops", tt.relax, got, tt.want)
		}
	}
}

// Own paths, filtered paths and destinations without a best path are not
// installed. A link-local next hop without an interface is an error.
func TestSelectRoutesSkips(t *testing.T) {
	own := testPath(t, "", "fe80::1", []uint32{65500}, 100, true)
	filtered := testPath(t, "fe80::1", "fe80::1", []uint32{65500}, 100, true)
	filtered.Filtered = true
	withdrawn := testPath(t, "fe80::1", "fe80::1", []uint32{65500}, 100, true)
	withdrawn.Withdrawal = true
	got, err := testSelector(4210000017, false, devOf(nil)).selectRoutes([]fibDest{
		dest(t, "192.0.2.17/32", own),
		dest(t, "198.51.100.1/32", filtered),
		dest(t, "198.51.100.2/32", withdrawn),
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("selectRoutes() = %+v, %v; want no routes", got, err)
	}
	// The own prefix, reflected back by a peer, gets no kernel route.
	reflected := testPath(t, "fe80::1", "fe80::1", []uint32{65500, 4210000017}, 100, false)
	ownBest := testPath(t, "", "fe80::1", []uint32{65500}, 100, true)
	got, err = testSelector(4210000017, false, devOf(map[string]string{"fe80::1": "a"})).selectRoutes([]fibDest{
		dest(t, "192.0.2.17/32", ownBest, reflected),
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("selectRoutes() = %+v, %v; want no route for the own prefix", got, err)
	}
	unknown := testPath(t, "fe80::9", "fe80::9", []uint32{65500}, 100, true)
	if _, err := testSelector(1, false, devOf(nil)).selectRoutes([]fibDest{dest(t, "198.51.100.1/32", unknown)}); err == nil {
		t.Fatal("selectRoutes() = nil error, want an error for a link-local next hop without interface")
	}
}

// FRR 10.3 multipath without relax: eBGP paths from the same neighbor AS
// are equal even with different AS paths; iBGP paths need the same AS
// path; an iBGP and an eBGP path never are. MED counts only for the same
// leftmost AS.
func TestEqualCostFRR(t *testing.T) {
	ebgp := func(peer uint32, path []uint32, med uint32) *pathInfo {
		return &pathInfo{localPref: 100, asPath: path, asLen: len(path), leftAS: path[0], peerAS: peer, med: med}
	}
	ibgp := func(path []uint32) *pathInfo {
		p := ebgp(65000, path, 0)
		p.internal = true
		return p
	}
	for _, tt := range []struct {
		name  string
		a, b  *pathInfo
		relax bool
		want  bool
	}{
		{"eBGP same neighbor AS, other path", ebgp(65500, []uint32{65500, 1}, 0), ebgp(65500, []uint32{65500, 2}, 0), false, true},
		{"eBGP other neighbor AS", ebgp(65500, []uint32{65500, 1}, 0), ebgp(65501, []uint32{65501, 1}, 0), false, false},
		{"eBGP other neighbor AS, relax", ebgp(65500, []uint32{65500, 1}, 0), ebgp(65501, []uint32{65501, 1}, 0), true, true},
		{"iBGP same path", ibgp([]uint32{1, 2}), ibgp([]uint32{1, 2}), false, true},
		{"iBGP other path", ibgp([]uint32{1, 2}), ibgp([]uint32{1, 3}), false, false},
		{"iBGP and eBGP", ibgp([]uint32{65500, 1}), ebgp(65500, []uint32{65500, 1}, 0), true, false},
		{"MED differs, same leftmost AS", ebgp(65500, []uint32{65500, 1}, 10), ebgp(65500, []uint32{65500, 1}, 20), false, false},
		{"MED differs, other leftmost AS", ebgp(65500, []uint32{65500, 1}, 10), ebgp(65501, []uint32{65501, 1}, 20), true, true},
	} {
		if got := equalCost(tt.a, tt.b, tt.relax); got != tt.want {
			t.Errorf("%s: equalCost = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A mixed iBGP and eBGP destination: only the paths of the best path's
// type are ECMP. iBGP is decided by the session's effective local AS:
// with local-as 65501, a peer in 65501 is iBGP although the router AS
// differs.
func TestSelectRoutesMixedIBGPEBGP(t *testing.T) {
	ib := testPath(t, "fe80::1", "fe80::1", []uint32{65501, 65100}, 100, true)
	ib.PeerASN = 65501
	eb := testPath(t, "fe80::2", "fe80::2", []uint32{65502, 65100}, 100, false)
	eb.PeerASN = 65502
	sel := testSelector(4210000017, true, devOf(map[string]string{"fe80::1": "a", "fe80::2": "b"}))
	sel.localAS = func(string) uint32 { return 65501 }
	got, err := sel.selectRoutes([]fibDest{dest(t, "198.51.100.1/32", ib, eb)})
	if err != nil || len(got) != 1 || len(got[0].Nexthops) != 1 || got[0].Nexthops[0].Dev != "a" {
		t.Fatalf("routes = %+v, %v; want only the iBGP best path (a)", got, err)
	}
	// With the router AS as local AS both are eBGP: ECMP with relax.
	sel.localAS = func(string) uint32 { return 4210000017 }
	got, err = sel.selectRoutes([]fibDest{dest(t, "198.51.100.1/32", ib, eb)})
	if err != nil || len(got) != 1 || len(got[0].Nexthops) != 2 {
		t.Fatalf("routes = %+v, %v; want ECMP over both eBGP paths", got, err)
	}
}

// Sessions in two VRFs: each table gets its own route; a table without
// GoBGP's best path picks its own best path.
func TestSelectRoutesPerVRFTable(t *testing.T) {
	a := testPath(t, "fe80::1", "fe80::1", []uint32{65500, 65100}, 100, true)
	b := testPath(t, "fe80::2", "fe80::2", []uint32{65500, 65100, 65101}, 100, false)
	c := testPath(t, "fe80::3", "fe80::3", []uint32{65500, 65100}, 100, false)
	sel := testSelector(4210000017, false, devOf(map[string]string{"fe80::1": "a", "fe80::2": "b", "fe80::3": "c"}))
	sel.table = func(n string) int {
		if n == "fe80::1" {
			return 10
		}
		return 20
	}
	got, err := sel.selectRoutes([]fibDest{dest(t, "198.51.100.1/32", a, b, c)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Table != 10 || got[0].Nexthops[0].Dev != "a" || got[1].Table != 20 ||
		len(got[1].Nexthops) != 1 || got[1].Nexthops[0].Dev != "c" {
		t.Fatalf("routes = %+v, want table 10 via a and table 20 via c (shorter AS path)", got)
	}
}

// A path with a global and a link-local next hop (RFC 2545) goes over the
// link-local one on the peer's interface, like zebra for a directly
// connected peer.
func TestSelectRoutesPrefersLinkLocalNexthop(t *testing.T) {
	p := testPath(t, "fe80::1", "2001:db8::1", []uint32{65500}, 100, true)
	for _, a := range p.Attrs {
		if mp, ok := a.(*bgp.PathAttributeMpReachNLRI); ok {
			mp.LinkLocalNexthop = netip.MustParseAddr("fe80::1")
		}
	}
	got, err := testSelector(4210000017, false, devOf(map[string]string{"fe80::1": "ens1f0np0"})).selectRoutes([]fibDest{dest(t, "198.51.100.1/32", p)})
	want := []fibNexthop{{Addr: netip.MustParseAddr("fe80::1"), Dev: "ens1f0np0"}}
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0].Nexthops, want) {
		t.Fatalf("selectRoutes() = %+v, %v; want next hop %v", got, err, want)
	}
}

// An IPv4 path with the classic NEXT_HOP attribute (RFC 4271) and no
// MP_REACH_NLRI is installed via that next hop.
func TestSelectRoutesClassicNextHop(t *testing.T) {
	p := testPath(t, "192.0.2.1", "192.0.2.1", []uint32{65500}, 100, true)
	nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr("192.0.2.7"))
	if err != nil {
		t.Fatal(err)
	}
	p.Attrs = []bgp.PathAttributeInterface{p.Attrs[0], p.Attrs[1], nh, p.Attrs[3]}
	got, err := testSelector(4210000017, false, devOf(nil)).selectRoutes([]fibDest{dest(t, "198.51.100.1/32", p)})
	want := []fibNexthop{{Addr: netip.MustParseAddr("192.0.2.7")}}
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0].Nexthops, want) {
		t.Fatalf("selectRoutes() = %+v, %v; want next hop %v", got, err, want)
	}
}
