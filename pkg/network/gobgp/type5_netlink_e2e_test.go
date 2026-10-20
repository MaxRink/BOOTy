//go:build linux && linux_e2e

package gobgp

import (
	"log/slog"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"testing"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const type5E2ETable = 4242

// TestType5NetlinkFailoverKeepsRoute runs the Type-5 route handling against
// the real kernel. Two gateways advertise 0/0 with different RDs. When the
// active one withdraws, the kernel route and the gateway neighbor and FDB
// entries must move to the other gateway. When both are gone, the route
// must be gone.
func TestType5NetlinkFailoverKeepsRoute(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	cfg := &Config{
		RouterID:          "192.0.2.10",
		ProvisionVNI:      4242,
		BridgeName:        "br-t5e2e",
		OverlayVRFName:    "Vrf_t5e2e", // routes go to the test table, never main
		OverlayVRFTableID: type5E2ETable,
	}
	bridge, vxlan := setupType5E2ELinks(t, cfg)
	overlay := NewOverlayTier(cfg)
	overlay.log = slog.Default()

	routeA, routeB := type5E2ERoute(t, 1), type5E2ERoute(t, 2)
	macA, macB := mustParseMAC(t, "02:00:00:00:00:a1"), mustParseMAC(t, "02:00:00:00:00:b2")

	overlay.handleType5Route(routeA, "192.0.2.1", macA, false)
	overlay.handleType5Route(routeB, "192.0.2.2", macB, false)
	requireType5E2EDefaultVia(t, "192.0.2.2")

	overlay.handleType5Route(routeB, "192.0.2.2", nil, true)
	requireType5E2EDefaultVia(t, "192.0.2.1")
	requireType5E2ENeighbor(t, bridge, "192.0.2.1", macA)
	requireType5E2EFDB(t, vxlan, "192.0.2.1", macA)

	overlay.handleType5Route(routeA, "192.0.2.1", nil, true)
	if routes := type5E2EDefaultRoutes(t); len(routes) != 0 {
		t.Fatalf("default route left after the last withdraw: %v", routes)
	}
}

// One writer owns the default route in the kernel: while the FIB writer
// has it in the table, the overlay keeps its Type-5 path in the shadow
// and leaves the writer's route alone, also on a withdraw. When the writer
// removes its route, the Type-5 route is installed as the backup.
func TestType5NetlinkFIBOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	cfg := &Config{
		RouterID: "192.0.2.10", ProvisionVNI: 4242, BridgeName: "br-t5e2e",
		OverlayVRFName: "Vrf_t5e2e", OverlayVRFTableID: type5E2ETable,
	}
	setupType5E2ELinks(t, cfg)
	dum := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dum-fib"}}
	if err := netlink.LinkAdd(dum); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	if err := netlink.LinkSetUp(dum); err != nil {
		t.Fatalf("set dummy up: %v", err)
	}
	overlay := NewOverlayTier(cfg)
	overlay.log = slog.Default()
	w := &fibWriter{nl: &netlink.Handle{}, log: slog.Default(), installed: map[string]*netlink.Route{}, onRemove: overlay.fibRemoved}
	overlay.fibOwns = w.owns
	_, all, _ := net.ParseCIDR("0.0.0.0/0")
	fib := map[string]*netlink.Route{
		routeKey(type5E2ETable, netip.MustParsePrefix("0.0.0.0/0")): {
			Dst: all, LinkIndex: dum.Attrs().Index, Table: type5E2ETable, Protocol: unix.RTPROT_BGP, Priority: fibMetric,
		},
	}
	viaFIB := func() {
		t.Helper()
		routes := type5E2EDefaultRoutes(t)
		if len(routes) != 1 || routes[0].LinkIndex != dum.Attrs().Index {
			t.Fatalf("default routes in table %d = %v, want the FIB writer's on dum-fib", type5E2ETable, routes)
		}
	}
	route, mac := type5E2ERoute(t, 1), mustParseMAC(t, "02:00:00:00:00:a1")

	w.apply(fib, false)
	overlay.handleType5Route(route, "192.0.2.1", mac, false)
	viaFIB()
	w.apply(nil, false) // the underlay loses the default route
	requireType5E2EDefaultVia(t, "192.0.2.1")

	// The other direction: the writer replaces the Type-5 route. A withdraw
	// then leaves the writer's route alone.
	w.apply(fib, false)
	viaFIB()
	overlay.handleType5Route(route, "192.0.2.1", nil, true)
	viaFIB()
	w.apply(nil, false)
	if routes := type5E2EDefaultRoutes(t); len(routes) != 0 {
		t.Fatalf("default routes after the withdraw and the FIB removal = %v, want none", routes)
	}
}

// enterTestNetns runs the rest of the test in a new network namespace, so
// the host routing and neighbor tables are never touched.
func enterTestNetns(t *testing.T) {
	t.Helper()
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("get netns: %v", err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatalf("new netns: %v", err)
	}
	t.Cleanup(func() {
		defer func() { _ = orig.Close() }()
		_ = ns.Close()
		if err := netns.Set(orig); err != nil {
			// Keep the thread locked: the runtime then retires it instead
			// of reusing a thread that is still in the test namespace.
			t.Errorf("restore network namespace: %v", err)
			return
		}
		runtime.UnlockOSThread()
	})
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatalf("find lo: %v", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatalf("set lo up: %v", err)
	}
}

func setupType5E2ELinks(t *testing.T, cfg *Config) (bridge, vxlan netlink.Link) {
	t.Helper()
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: cfg.BridgeName}}
	if err := netlink.LinkAdd(br); err != nil {
		t.Fatalf("add bridge: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(br) })
	vx := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: "vx4242"},
		VxlanId:   4242,
		Port:      4789,
		Learning:  false,
		SrcAddr:   net.ParseIP(cfg.RouterID),
	}
	if err := netlink.LinkAdd(vx); err != nil {
		t.Fatalf("add vxlan: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(vx) })
	for _, link := range []netlink.Link{br, vx} {
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("set %s up: %v", link.Attrs().Name, err)
		}
	}
	bridge, err := netlink.LinkByName(cfg.BridgeName)
	if err != nil {
		t.Fatalf("find bridge: %v", err)
	}
	vxlan, err = netlink.LinkByName("vx4242")
	if err != nil {
		t.Fatalf("find vxlan: %v", err)
	}
	return bridge, vxlan
}

// type5E2ERoute is a default route without a gateway IP: the VTEP is the
// gateway, like the routes FRR sends.
func type5E2ERoute(t *testing.T, rdAssigned uint32) *apipb.EVPNIPPrefixRoute {
	t.Helper()
	rd := &apipb.RouteDistinguisher{Rd: &apipb.RouteDistinguisher_TwoOctetAsn{TwoOctetAsn: &apipb.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: rdAssigned}}}
	return &apipb.EVPNIPPrefixRoute{Rd: rd, IpPrefix: "0.0.0.0", IpPrefixLen: 0, GwAddress: "0.0.0.0"}
}

func type5E2EDefaultRoutes(t *testing.T) []netlink.Route {
	t.Helper()
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: type5E2ETable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	var defaults []netlink.Route
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			defaults = append(defaults, r)
		}
	}
	return defaults
}

func requireType5E2EDefaultVia(t *testing.T, gw string) {
	t.Helper()
	routes := type5E2EDefaultRoutes(t)
	if len(routes) != 1 || !routes[0].Gw.Equal(net.ParseIP(gw)) {
		t.Fatalf("default routes in table %d = %v, want one via %s", type5E2ETable, routes, gw)
	}
}

func requireType5E2ENeighbor(t *testing.T, bridge netlink.Link, ip string, mac net.HardwareAddr) {
	t.Helper()
	neighs, err := netlink.NeighList(bridge.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("list neighbors: %v", err)
	}
	for _, n := range neighs {
		if n.IP.Equal(net.ParseIP(ip)) && n.HardwareAddr.String() == mac.String() {
			return
		}
	}
	t.Fatalf("no neighbor %s lladdr %s on %s: %v", ip, mac, bridge.Attrs().Name, neighs)
}

func requireType5E2EFDB(t *testing.T, vxlan netlink.Link, vtep string, mac net.HardwareAddr) {
	t.Helper()
	fdb, err := netlink.NeighList(vxlan.Attrs().Index, syscall.AF_BRIDGE)
	if err != nil {
		t.Fatalf("list fdb: %v", err)
	}
	for _, n := range fdb {
		if n.IP.Equal(net.ParseIP(vtep)) && n.HardwareAddr.String() == mac.String() {
			return
		}
	}
	t.Fatalf("no FDB entry %s dst %s on %s: %v", mac, vtep, vxlan.Attrs().Name, fdb)
}
