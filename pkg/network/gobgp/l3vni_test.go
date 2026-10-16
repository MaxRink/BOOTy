//go:build linux

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"testing"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Mirrors FRR 10.x bgp_evpn_rt5/test_bgp_evpn.py::test_router_check_ip: an
// IPv6 Type-5 route over an IPv4 VTEP goes via ::ffff:<VTEP> on the L3VNI
// bridge, with a permanent neighbor entry for that address with the RMAC
// and an FDB entry RMAC -> VTEP.
func TestHandleType5RouteIPv6OverIPv4VTEP(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "192.0.2.17", ProvisionVNI: 3000, BridgeName: "br.provision", VXLANName: "vx.3000"},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{IpPrefix: "::", IpPrefixLen: 0, GwAddress: "::"}
	rmac := mustParseMAC(t, "02:54:c6:33:64:01")

	overlay.handleType5Route(route, "198.51.100.1", rmac, false)

	if len(mock.routeReplaces) != 1 {
		t.Fatalf("route replaces = %d, want 1", len(mock.routeReplaces))
	}
	r := mock.routeReplaces[0]
	if r.Dst.String() != "::/0" || r.LinkIndex != 42 || r.Flags&int(netlink.FLAG_ONLINK) == 0 {
		t.Fatalf("route = %+v, want ::/0 onlink on the bridge", r)
	}
	if r.Protocol != unix.RTPROT_BGP || r.Priority != zebraRouteMetric {
		t.Fatalf("route protocol %d metric %d, want bgp and 20 like zebra", r.Protocol, r.Priority)
	}
	if !isMappedGatewayRoute(r) || r.Gw.String() != "198.51.100.1" {
		t.Fatalf("gateway = %v (%d bytes), want ::ffff:198.51.100.1", r.Gw, len(r.Gw))
	}
	if len(mock.sets) != 2 {
		t.Fatalf("neighbor sets = %d, want neighbor and FDB", len(mock.sets))
	}
	n, fdb := mock.sets[0], mock.sets[1]
	if n.Family != syscall.AF_INET6 || !isMappedGateway(n.IP) || n.State != netlink.NUD_NOARP || n.Flags != netlink.NTF_EXT_LEARNED ||
		n.HardwareAddr.String() != rmac.String() || n.LinkIndex != 42 {
		t.Fatalf("neighbor = %+v, want ::ffff:198.51.100.1 -> RMAC extern_learn NOARP on the bridge", n)
	}
	if fdb.Family != syscall.AF_BRIDGE || fdb.IP.String() != "198.51.100.1" || fdb.LinkIndex != 43 ||
		fdb.HardwareAddr.String() != rmac.String() {
		t.Fatalf("FDB = %+v, want RMAC -> VTEP on vx.3000", fdb)
	}
	if mock.linkName != "vx.3000" {
		t.Fatalf("VXLAN looked up as %q, want the netplan name vx.3000", mock.linkName)
	}

	// The IPv4 gateway of an IPv4 route for the same VTEP is separate state.
	overlay.handleType5Route(&apipb.EVPNIPPrefixRoute{IpPrefix: "0.0.0.0", GwAddress: "0.0.0.0"}, "198.51.100.1", rmac, false)
	if got := mock.sets[2]; got.Family != syscall.AF_INET || len(got.IP) != net.IPv4len {
		t.Fatalf("IPv4 neighbor = %+v, want AF_INET", got)
	}

	overlay.handleType5Route(route, "198.51.100.1", nil, true)
	if len(mock.routeDels) != 1 || !isMappedGatewayRoute(mock.routeDels[0]) {
		t.Fatalf("route deletes = %+v, want the ::/0 route", mock.routeDels)
	}
	if len(mock.dels) != 1 || mock.dels[0].Family != syscall.AF_INET6 {
		t.Fatalf("neighbor deletes = %+v, want only the IPv6 neighbor (the FDB entry is shared)", mock.dels)
	}
}

// zebra prefers a static route (distance 1) over BGP, so a received route
// for a local blackhole prefix is not installed.
func TestHandleType5RouteSkipsBlackholePrefix(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{RouterID: "192.0.2.17", ProvisionVNI: 3000, BridgeName: "br.provision",
			BlackholeRoutes: []string{"2001:db8:40a:5481::/64"}},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	overlay.handleType5Route(&apipb.EVPNIPPrefixRoute{IpPrefix: "2001:db8:40a:5481::", IpPrefixLen: 64, GwAddress: "::"},
		"192.0.2.18", mustParseMAC(t, "02:54:c0:00:02:12"), false)
	if len(mock.routeReplaces) != 0 || len(mock.sets) != 0 {
		t.Fatalf("installed %+v %+v, want nothing", mock.routeReplaces, mock.sets)
	}
}

func TestBuildType5GatewayNeighborMappedGateway(t *testing.T) {
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42}}
	mac := mustParseMAC(t, "02:54:c6:33:64:01")
	gw := type5Gateway(&net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}, net.ParseIP("198.51.100.1"))
	n := buildType5GatewayNeighbor(link, gw, mac)
	if n == nil || n.Family != syscall.AF_INET6 || len(n.IP) != net.IPv6len {
		t.Fatalf("neighbor = %+v, want AF_INET6 with 16 bytes", n)
	}
	if got := gatewayKey(gw); got != "::ffff:198.51.100.1" {
		t.Fatalf("gateway key = %q", got)
	}
	if got := parseGatewayKey(gatewayKey(gw)); !isMappedGateway(got) {
		t.Fatalf("parsed key = %v, want the mapped gateway", got)
	}
	if got := parseGatewayKey("198.51.100.1"); len(got) != net.IPv4len {
		t.Fatalf("parsed IPv4 key has %d bytes", len(got))
	}
}

// Mirrors FRR bgp_basic_functionality_topo1
// test_advertise_network_using_network_command: a network is advertised
// only when it is in the RIB (import check). The /128 is on lo, the /64 is
// a blackhole route, the third prefix has no route.
func TestAdvertiseType5NetworksImportCheck(t *testing.T) {
	bgp := server.NewBgpServer()
	go bgp.Serve()
	t.Cleanup(bgp.Stop)
	ctx := context.Background()
	if err := bgp.StartBgp(ctx, &apipb.StartBgpRequest{Global: &apipb.Global{Asn: 4210000017, RouterId: "192.0.2.17", ListenPort: -1}}); err != nil {
		t.Fatalf("start BGP: %v", err)
	}
	overlay := &OverlayTier{
		cfg: &Config{
			ASN: 4210000017, RouterID: "192.0.2.17", ProvisionVNI: 3000,
			BridgeMAC: "02:54:c0:00:02:11", VPNRT: "65169:3000",
			LoopbackAddrs:   []string{"2001:db8:40a:5481::17/128"},
			BlackholeRoutes: []string{"2001:db8:40a:5481::/64"},
			Type5Networks:   []string{"2001:db8:40a:5481::17/128", "2001:db8:40a:5481::/64", "2001:db8:ffff::/48"},
		},
		log: slog.Default(),
		bgp: bgp,
	}
	if err := overlay.advertiseType5Networks(ctx); err != nil {
		t.Fatalf("advertise: %v", err)
	}
	var got []string
	err := bgp.ListPath(ctx, &apipb.ListPathRequest{
		TableType: apipb.TableType_GLOBAL,
		Family:    &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
	}, func(dst *apipb.Destination) {
		for _, p := range dst.GetPaths() {
			r := mustFindType5Route(t, p)
			if r.GetGwAddress() != "::" || r.GetLabel() != 3000 || extractNextHop(p) != "192.0.2.17" ||
				!matchesLocalRT(p, 65169, 3000) || mustFindRouterMAC(t, p) != "02:54:c0:00:02:11" {
				t.Errorf("path %v: want gateway ::, label 3000, next hop 192.0.2.17, RT 65169:3000, RMAC", r)
			}
			got = append(got, fmt.Sprintf("%s/%d", r.GetIpPrefix(), r.GetIpPrefixLen()))
		}
	})
	if err != nil {
		t.Fatalf("list paths: %v", err)
	}
	slices.Sort(got)
	want := []string{"2001:db8:40a:5481::/64", "2001:db8:40a:5481::17/128"}
	if !slices.Equal(got, want) {
		t.Fatalf("advertised %v, want %v", got, want)
	}
}

// The import check accepts a covering local route, like FRR's default
// "bgp network import-check" (exact match only with "import-check exact").
func TestLocalPrefixCovering(t *testing.T) {
	o := &OverlayTier{cfg: &Config{
		LoopbackAddrs:   []string{"2001:db8:40a:5481::17/128"},
		BlackholeRoutes: []string{"2001:db8:40a:5481::/64"},
		ProvisionIP:     "10.100.0.20/24",
	}}
	for _, tt := range []struct {
		prefix string
		want   bool
	}{
		{"2001:db8:40a:5481::17/128", true},
		{"2001:db8:40a:5481::/64", true},
		{"2001:db8:40a:5481:1::/80", true},
		{"2001:db8:40a::/48", false},
		{"10.100.0.0/24", true},
		{"10.100.0.128/25", true},
		{"10.100.0.0/16", false},
		{"192.0.2.0/24", false},
	} {
		if got := o.localPrefix(netip.MustParsePrefix(tt.prefix)); got != tt.want {
			t.Errorf("localPrefix(%s) = %v, want %v", tt.prefix, got, tt.want)
		}
	}
}
