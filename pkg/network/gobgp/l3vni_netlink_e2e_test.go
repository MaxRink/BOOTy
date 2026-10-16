//go:build linux && linux_e2e

package gobgp

import (
	"log/slog"
	"net"
	"os"
	"slices"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const l3vniE2ETable = 4243

// TestL3VNINetlinkState runs the L3VNI host state against the kernel (root
// only, CI job Linux E2E): the IPv6 route over an IPv4 VTEP with the
// 16-byte IPv4-mapped gateway, protocol bgp and metric 20 like zebra, its
// gateway neighbor as NOARP extern_learn, the static blackhole route with
// protocol 196 and metric 20, and the lo addresses. The kernel rejects an
// IPv6 route or neighbor with a 4-byte gateway, so this fails when the
// mapped address is sent as IPv4.
func TestL3VNINetlinkState(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	cfg := &Config{
		RouterID:        "192.0.2.10",
		OverlayIP:       "2001:db8:ff::10",
		ProvisionVNI:    4242,
		BridgeName:      "br-l3e2e",
		LoopbackAddrs:   []string{"2001:db8:40a:5481::17/128"},
		BlackholeRoutes: []string{"2001:db8:40a:5481::/64"},
	}
	bridge, _ := setupType5E2ELinks(t, cfg)
	overlay := NewOverlayTier(cfg)
	overlay.log = slog.Default()

	// The netplan lo addresses replace the overlay IP.
	if err := overlay.addOverlayLoopback(); err != nil {
		t.Fatal(err)
	}
	if err := overlay.installBlackholes(); err != nil {
		t.Fatal(err)
	}
	mac := mustParseMAC(t, "02:00:00:00:00:c3")
	_, dst, _ := net.ParseCIDR("2001:db8:99::/64")
	gw := type5Gateway(dst, net.ParseIP("192.0.2.9"))
	neigh := buildType5GatewayNeighbor(bridge, gw, mac)
	if err := overlay.netlinkOps.NeighSet(neigh); err != nil {
		t.Fatalf("mapped neighbor: %v", err)
	}
	route := buildType5KernelRoute(bridge, dst, gw, l3vniE2ETable)
	if err := overlay.netlinkOps.RouteReplace(route); err != nil {
		t.Fatalf("mapped route: %v", err)
	}

	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: l3vniE2ETable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || len(routes[0].Gw) != net.IPv6len || !routes[0].Gw.Equal(net.ParseIP("::ffff:192.0.2.9")) ||
		routes[0].Protocol != unix.RTPROT_BGP || routes[0].Priority != zebraRouteMetric {
		t.Fatalf("IPv6 routes in table %d = %+v, want 2001:db8:99::/64 via ::ffff:192.0.2.9 proto bgp metric 20", l3vniE2ETable, routes)
	}

	neighs, err := netlink.NeighList(bridge.Attrs().Index, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(neighs, func(n netlink.Neigh) bool {
		return n.IP.Equal(net.ParseIP("::ffff:192.0.2.9")) && n.State == netlink.NUD_NOARP &&
			n.Flags&netlink.NTF_EXT_LEARNED != 0 && n.HardwareAddr.String() == mac.String()
	}) {
		t.Fatalf("IPv6 neighbors = %+v, want ::ffff:192.0.2.9 NOARP extern_learn %s", neighs, mac)
	}

	holes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: unix.RT_TABLE_MAIN, Type: unix.RTN_BLACKHOLE}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(holes, func(r netlink.Route) bool {
		return r.Dst != nil && r.Dst.String() == "2001:db8:40a:5481::/64" && r.Protocol == rtprotZebraStatic && r.Priority == zebraRouteMetric
	}) {
		t.Fatalf("blackhole routes = %+v, want 2001:db8:40a:5481::/64 proto 196 metric 20", holes)
	}

	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := netlink.AddrList(lo, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	var global []string
	for _, a := range addrs {
		if !a.IP.IsLoopback() && !a.IP.IsLinkLocalUnicast() {
			global = append(global, a.IPNet.String())
		}
	}
	if !slices.Equal(global, []string{"2001:db8:40a:5481::17/128"}) {
		t.Fatalf("lo addresses = %v, want only the netplan 2001:db8:40a:5481::17/128, not the overlay IP", global)
	}

	if err := overlay.netlinkOps.RouteDel(route); err != nil {
		t.Fatalf("delete mapped route: %v", err)
	}
	if err := overlay.netlinkOps.NeighDel(neigh); err != nil {
		t.Fatalf("delete mapped neighbor: %v", err)
	}
	overlay.removeLocalState()
	if holes, _ = netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{Table: unix.RT_TABLE_MAIN, Type: unix.RTN_BLACKHOLE}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_TYPE); len(holes) != 0 {
		t.Fatalf("blackhole routes after cleanup = %+v", holes)
	}
}
