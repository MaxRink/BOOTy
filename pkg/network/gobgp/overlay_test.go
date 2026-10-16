//go:build linux

package gobgp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

// mockOverlayNetlinkOps records overlay netlink operations for assertion in unit tests.
type mockOverlayNetlinkOps struct {
	linkName        string
	linkErr         error
	linkErrName     string
	sets            []*netlink.Neigh
	appends         []*netlink.Neigh
	dels            []*netlink.Neigh
	routeReplaces   []*netlink.Route
	routeDels       []*netlink.Route
	setErr          error
	setErrOnCall    int
	appendErr       error
	delErr          error
	delErrOnCall    int
	routeReplaceErr error
	routeDelErr     error
}

func (m *mockOverlayNetlinkOps) LinkByName(name string) (netlink.Link, error) {
	m.linkName = name
	if m.linkErr != nil && (m.linkErrName == "" || m.linkErrName == name) {
		return nil, m.linkErr
	}
	index := 42
	if strings.HasPrefix(name, "vx") {
		index = 43
	}
	return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: index, Name: name}}, nil
}

func (m *mockOverlayNetlinkOps) NeighSet(n *netlink.Neigh) error {
	m.sets = append(m.sets, n)
	if m.setErr != nil && (m.setErrOnCall == 0 || m.setErrOnCall == len(m.sets)) {
		return m.setErr
	}
	return nil
}

func (m *mockOverlayNetlinkOps) NeighAppend(n *netlink.Neigh) error {
	m.appends = append(m.appends, n)
	return m.appendErr
}

func (m *mockOverlayNetlinkOps) NeighDel(n *netlink.Neigh) error {
	m.dels = append(m.dels, n)
	if m.delErr != nil && (m.delErrOnCall == 0 || m.delErrOnCall == len(m.dels)) {
		return m.delErr
	}
	return nil
}

func (m *mockOverlayNetlinkOps) RouteReplace(route *netlink.Route) error {
	m.routeReplaces = append(m.routeReplaces, route)
	return m.routeReplaceErr
}

func (m *mockOverlayNetlinkOps) RouteDel(route *netlink.Route) error {
	m.routeDels = append(m.routeDels, route)
	return m.routeDelErr
}

func TestNewVXLANUsesConfiguredNetplanLink(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:     "192.168.4.10",
			ProvisionVNI: 1000,
			VXLANLink:    "dum.underlay",
		},
		netlinkOps: mock,
	}

	vxlan, err := overlay.newVXLAN("vx1000")
	if err != nil {
		t.Fatalf("newVXLAN: %v", err)
	}
	if vxlan.VtepDevIndex != 42 {
		t.Errorf("VtepDevIndex = %d, want 42", vxlan.VtepDevIndex)
	}
	if mock.linkName != "dum.underlay" {
		t.Errorf("LinkByName called with %q, want dum.underlay", mock.linkName)
	}
}

func TestNewVXLANLeavesDeviceUnboundWithoutNetplanLink(t *testing.T) {
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "192.168.4.10", ProvisionVNI: 1000},
		netlinkOps: &mockOverlayNetlinkOps{},
	}

	vxlan, err := overlay.newVXLAN("vx1000")
	if err != nil {
		t.Fatalf("newVXLAN: %v", err)
	}
	if vxlan.VtepDevIndex != 0 {
		t.Errorf("VtepDevIndex = %d, want 0", vxlan.VtepDevIndex)
	}
}

func TestNewVXLANUsesUnderlayDummyForConfiguredVRF(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID: "192.168.4.10",
			VRFName:  "Vrf_underlay",
		},
		netlinkOps: mock,
	}

	vxlan, err := overlay.newVXLAN("vx1000")
	if err != nil {
		t.Fatalf("newVXLAN: %v", err)
	}
	if vxlan.VtepDevIndex != 42 {
		t.Errorf("VtepDevIndex = %d, want 42", vxlan.VtepDevIndex)
	}
	if mock.linkName != defaultUnderlayDummyName {
		t.Errorf("LinkByName called with %q, want %q", mock.linkName, defaultUnderlayDummyName)
	}
}

func TestNewVXLANRejectsMissingConfiguredNetplanLink(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:  "192.168.4.10",
			VXLANLink: "dum.underlay",
		},
		netlinkOps: &mockOverlayNetlinkOps{
			linkErr: errors.New("not found"),
		},
	}

	_, err := overlay.newVXLAN("vx1000")
	if err == nil || !strings.Contains(err.Error(), "find VXLAN underlay link dum.underlay") {
		t.Fatalf("newVXLAN error = %v, want missing underlay link context", err)
	}
}

func TestShouldInstallGatewayFDB(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "legacy gateway", cfg: Config{ProvisionGateway: testProvisionGateway}, want: true},
		{name: "missing gateway", cfg: Config{}, want: false},
		{name: "explicit Type-5-only", cfg: Config{ProvisionGateway: testProvisionGateway, Type5Only: true}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay := NewOverlayTier(&tt.cfg)
			if got := overlay.shouldInstallGatewayFDB(); got != tt.want {
				t.Fatalf("shouldInstallGatewayFDB() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureType5VTEPRoute(t *testing.T) {
	overlay := NewOverlayTier(&Config{})
	if _, ok := overlay.ensureType5VTEP("192.168.4.11", false); !ok {
		t.Fatal("nil VTEP route installer must succeed")
	}

	overlay.ensureVTEPRoute = func(net.IP) error { return nil }
	if _, ok := overlay.ensureType5VTEP("192.168.4.11", false); !ok {
		t.Fatal("successful VTEP route installer must succeed")
	}

	overlay.ensureVTEPRoute = func(net.IP) error { return errors.New("unreachable") }
	if _, ok := overlay.ensureType5VTEP("192.168.4.11", false); ok {
		t.Fatal("failed VTEP route installer must fail")
	}
	if vtep, ok := overlay.ensureType5VTEP("invalid", true); !ok || vtep != nil {
		t.Fatal("withdraw must permit a missing VTEP")
	}
}

func TestHandleType5RouteStopsWhenVTEPRouteCannotBeInstalled(t *testing.T) {
	ops := &mockOverlayNetlinkOps{}
	overlay := NewOverlayTier(&Config{RouterID: "192.168.4.10", BridgeName: "br.provision"})
	overlay.netlinkOps = ops
	overlay.ensureVTEPRoute = func(net.IP) error { return errors.New("unreachable") }

	overlay.handleType5Route(
		&apipb.EVPNIPPrefixRoute{IpPrefix: "10.200.0.11", IpPrefixLen: 32},
		"192.168.4.11",
		nil,
		false,
	)
	if len(ops.routeReplaces) != 0 {
		t.Fatalf("installed %d Type-5 routes despite missing underlay VTEP reachability", len(ops.routeReplaces))
	}
}

func TestBuildRouteDistinguisher(t *testing.T) {
	tests := []struct {
		name    string
		asn     uint32
		vni     uint32
		wantErr bool
		check   func(t *testing.T, a *apipb.RouteDistinguisherTwoOctetASN, b *apipb.RouteDistinguisherFourOctetASN)
	}{
		{
			name: "2-byte ASN",
			asn:  65000,
			vni:  4000,
			check: func(t *testing.T, two *apipb.RouteDistinguisherTwoOctetASN, _ *apipb.RouteDistinguisherFourOctetASN) {
				t.Helper()
				if two == nil {
					t.Fatal("expected 2-octet RD")
				}
				if two.Admin != 65000 {
					t.Errorf("Admin = %d, want 65000", two.Admin)
				}
				if two.Assigned != 4000 {
					t.Errorf("Assigned = %d, want 4000", two.Assigned)
				}
			},
		},
		{
			name: "4-byte ASN",
			asn:  70000,
			vni:  5000,
			check: func(t *testing.T, _ *apipb.RouteDistinguisherTwoOctetASN, four *apipb.RouteDistinguisherFourOctetASN) {
				t.Helper()
				if four == nil {
					t.Fatal("expected 4-octet RD")
				}
				if four.Admin != 70000 {
					t.Errorf("Admin = %d, want 70000", four.Admin)
				}
				// VNI is masked to 16 bits for 4-octet format.
				if four.Assigned != 5000 {
					t.Errorf("Assigned = %d, want 5000", four.Assigned)
				}
			},
		},
		{
			name: "4-byte ASN truncates large VNI",
			asn:  100000,
			vni:  70000,
			check: func(t *testing.T, _ *apipb.RouteDistinguisherTwoOctetASN, four *apipb.RouteDistinguisherFourOctetASN) {
				t.Helper()
				if four == nil {
					t.Fatal("expected 4-octet RD")
				}
				if four.Assigned != 70000&0xFFFF {
					t.Errorf("Assigned = %d, want %d (truncated)", four.Assigned, 70000&0xFFFF)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rd, err := buildRouteDistinguisher(tt.asn, tt.vni)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			msg, err := rd.UnmarshalNew()
			if err != nil {
				t.Fatalf("unmarshal RD: %v", err)
			}

			var gotTwo *apipb.RouteDistinguisherTwoOctetASN
			var gotFour *apipb.RouteDistinguisherFourOctetASN
			switch v := msg.(type) {
			case *apipb.RouteDistinguisherTwoOctetASN:
				gotTwo = v
			case *apipb.RouteDistinguisherFourOctetASN:
				gotFour = v
			default:
				t.Fatalf("unexpected RD type: %T", msg)
			}
			tt.check(t, gotTwo, gotFour)
		})
	}
}

func TestBuildRouteTarget(t *testing.T) {
	tests := []struct {
		name    string
		asn     uint32
		vni     uint32
		wantErr bool
	}{
		{name: "2-byte ASN", asn: 65000, vni: 4000},
		{name: "4-byte ASN", asn: 70000, vni: 5000},
		{name: "4-byte ASN rejects unencodable local admin", asn: 70000, vni: 65536, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, err := buildRouteTarget(tt.asn, tt.vni)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildRouteTarget() err = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rt == nil {
				t.Fatal("expected non-nil route target")
			}
		})
	}
}

func TestBuildEVPNType5NLRI(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	// Passing a /32 host route (as advertiseType5 now does).
	nlri, err := buildEVPNType5NLRI(rd, "10.100.0.20/32", "10.0.0.1", 4000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nlri == nil {
		t.Fatal("expected non-nil NLRI")
	}

	// Unmarshal and verify fields.
	msg, err := nlri.UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal NLRI: %v", err)
	}
	route, ok := msg.(*apipb.EVPNIPPrefixRoute)
	if !ok {
		t.Fatalf("expected EVPNIPPrefixRoute, got %T", msg)
	}
	if route.IpPrefix != "10.100.0.20" {
		t.Errorf("IpPrefix = %s, want 10.100.0.20", route.IpPrefix)
	}
	if route.IpPrefixLen != 32 {
		t.Errorf("IpPrefixLen = %d, want 32", route.IpPrefixLen)
	}
	if route.GwAddress != "10.0.0.1" {
		t.Errorf("GwAddress = %s, want 10.0.0.1", route.GwAddress)
	}
}

func TestBuildType5PathAttrs(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType5NLRI(rd, "10.100.0.20/32", "10.0.0.1", 4000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType5PathAttrs(nlri, "10.0.0.1", 65000, 4000, "", "62:db:b8:c1:80:52")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect 3 attributes: origin, mp-reach, ext-communities.
	if len(pattrs) != 3 {
		t.Errorf("got %d path attrs, want 3", len(pattrs))
	}
	path := &apipb.Path{Pattrs: pattrs}
	if got := mustFindEncapTunnelType(t, path); got != vxlanTunnelType {
		t.Fatalf("encapsulation tunnel type = %d, want %d", got, vxlanTunnelType)
	}
	if !matchesLocalRT(path, 65000, 4000) {
		t.Fatal("expected exported Type-5 attributes to carry route target 65000:4000")
	}
	if got := mustFindRouterMAC(t, path); got != "62:db:b8:c1:80:52" {
		t.Fatalf("router MAC = %s, want 62:db:b8:c1:80:52", got)
	}
	if got := countExtendedCommunities(t, path); got != 3 {
		t.Fatalf("extended community count = %d, want 3 (RT, ET, RMAC)", got)
	}
}

func TestBuildType5PathAttrsUsesExplicitVPNRT(t *testing.T) {
	rd, err := buildRouteDistinguisher(65100, 1000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}
	nlri, err := buildEVPNType5NLRI(rd, "10.200.0.10/32", "192.168.4.10", 1000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType5PathAttrs(nlri, "192.168.4.10", 65100, 1000, "65000:1000", "62:db:b8:c1:80:52")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	path := &apipb.Path{Pattrs: pattrs}
	if !matchesLocalRT(path, 65000, 1000) {
		t.Fatal("expected exported Type-5 attributes to carry configured VPNRT 65000:1000")
	}
	if matchesLocalRT(path, 65100, 1000) {
		t.Fatal("did not expect exported Type-5 attributes to carry fallback local RT")
	}
	if got := mustFindEncapTunnelType(t, path); got != vxlanTunnelType {
		t.Fatalf("encapsulation tunnel type = %d, want %d", got, vxlanTunnelType)
	}
	if got := mustFindRouterMAC(t, path); got != "62:db:b8:c1:80:52" {
		t.Fatalf("router MAC = %s, want 62:db:b8:c1:80:52", got)
	}
	if got := countExtendedCommunities(t, path); got != 3 {
		t.Fatalf("extended community count = %d, want 3 (RT, ET, RMAC)", got)
	}
}

func TestBuildType5PathAttrsOmitsRouterMACWhenUnset(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}
	nlri, err := buildEVPNType5NLRI(rd, "10.100.0.20/32", "10.0.0.1", 4000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType5PathAttrs(nlri, "10.0.0.1", 65000, 4000, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	path := &apipb.Path{Pattrs: pattrs}
	if got, err := extractRouterMAC(path); err != nil || got != nil {
		t.Fatalf("router MAC = %v, err = %v; want nil, nil", got, err)
	}
	if got := mustFindEncapTunnelType(t, path); got != vxlanTunnelType {
		t.Fatalf("encapsulation tunnel type = %d, want %d", got, vxlanTunnelType)
	}
	if got := countExtendedCommunities(t, path); got != 2 {
		t.Fatalf("extended community count = %d, want 2 (RT, ET)", got)
	}
}

func TestBuildType5PathAttrsRejectsInvalidRouterMAC(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}
	nlri, err := buildEVPNType5NLRI(rd, "10.100.0.20/32", "10.0.0.1", 4000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	_, err = buildType5PathAttrs(nlri, "10.0.0.1", 65000, 4000, "", "not-a-mac")
	if err == nil {
		t.Fatal("expected invalid router MAC error")
	}
}

func TestBuildType5PathAttrsRejectsEUI64RouterMAC(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}
	nlri, err := buildEVPNType5NLRI(rd, "10.100.0.20/32", "10.0.0.1", 4000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	_, err = buildType5PathAttrs(nlri, "10.0.0.1", 65000, 4000, "", "02:00:00:ff:fe:00:00:10")
	if err == nil {
		t.Fatal("expected EUI-64 router MAC error")
	}
}

func TestBuildType5PathAttrsRejectsInvalidVPNRT(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}
	nlri, err := buildEVPNType5NLRI(rd, "10.100.0.20/32", "10.0.0.1", 4000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	_, err = buildType5PathAttrs(nlri, "10.0.0.1", 65000, 4000, "invalid", "62:db:b8:c1:80:52")
	if err == nil {
		t.Fatal("expected invalid VPNRT error")
	}
}

func TestAdvertiseType5RejectsInvalidRouterMAC(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{
			ASN:          65000,
			RouterID:     "192.168.4.10",
			ProvisionVNI: 4000,
			ProvisionIP:  "10.100.0.20/24",
			BridgeMAC:    "not-a-mac",
		},
		log: slog.Default(),
		bgp: server.NewBgpServer(),
	}

	err := overlay.advertiseType5(context.Background())
	if err == nil {
		t.Fatal("expected invalid router MAC error")
	}
}

func TestAdvertiseType5PublishesRouterMAC(t *testing.T) {
	bgp := server.NewBgpServer()
	go bgp.Serve()
	t.Cleanup(bgp.Stop)
	ctx := context.Background()
	if err := bgp.StartBgp(ctx, &apipb.StartBgpRequest{
		Global: &apipb.Global{
			Asn:        65100,
			RouterId:   "192.168.4.10",
			ListenPort: -1,
		},
	}); err != nil {
		t.Fatalf("start BGP: %v", err)
	}

	overlay := &OverlayTier{
		cfg: &Config{
			ASN:          65100,
			RouterID:     "192.168.4.10",
			ProvisionVNI: 1000,
			ProvisionIP:  "10.200.0.10/32",
			BridgeMAC:    "62:db:b8:c1:80:52",
			VPNRT:        "65000:1000",
		},
		log: slog.Default(),
		bgp: bgp,
	}

	if err := overlay.advertiseType5(ctx); err != nil {
		t.Fatalf("advertise Type-5: %v", err)
	}

	var paths []*apipb.Path
	err := bgp.ListPath(ctx, &apipb.ListPathRequest{
		TableType: apipb.TableType_GLOBAL,
		Family:    &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
	}, func(dst *apipb.Destination) {
		paths = append(paths, dst.GetPaths()...)
	})
	if err != nil {
		t.Fatalf("list paths: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 advertised Type-5 path, got %d", len(paths))
	}
	route := mustFindType5Route(t, paths[0])
	if route.GetIpPrefix() != "10.200.0.10" {
		t.Fatalf("type-5 prefix = %s, want 10.200.0.10", route.GetIpPrefix())
	}
	if route.GetIpPrefixLen() != 32 {
		t.Fatalf("type-5 prefix length = %d, want 32", route.GetIpPrefixLen())
	}
	if route.GetGwAddress() != type5DirectGateway {
		t.Fatalf("type-5 gateway IP = %s, want %s", route.GetGwAddress(), type5DirectGateway)
	}
	if route.GetLabel() != 1000 {
		t.Fatalf("type-5 label = %d, want 1000", route.GetLabel())
	}
	if got := extractNextHop(paths[0]); got != "192.168.4.10" {
		t.Fatalf("type-5 next-hop = %s, want 192.168.4.10", got)
	}
	if !matchesLocalRT(paths[0], 65000, 1000) {
		t.Fatal("expected advertised Type-5 path to carry configured VPNRT 65000:1000")
	}
	if matchesLocalRT(paths[0], 65100, 1000) {
		t.Fatal("did not expect advertised Type-5 path to carry fallback local RT")
	}
	if got := mustFindRouterMAC(t, paths[0]); got != "62:db:b8:c1:80:52" {
		t.Fatalf("router MAC = %s, want 62:db:b8:c1:80:52", got)
	}
	if got := mustFindEncapTunnelType(t, paths[0]); got != vxlanTunnelType {
		t.Fatalf("encapsulation tunnel type = %d, want %d", got, vxlanTunnelType)
	}
	if got := countExtendedCommunities(t, paths[0]); got != 3 {
		t.Fatalf("extended community count = %d, want 3 (RT, ET, RMAC)", got)
	}
}

func TestAdvertiseType5SkipsWithoutProvisionIP(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{
			ASN:          65000,
			RouterID:     "192.168.4.10",
			ProvisionVNI: 4000,
			BridgeMAC:    "not-a-mac",
		},
		log: slog.Default(),
	}

	if err := overlay.advertiseType5(context.Background()); err != nil {
		t.Fatalf("advertise Type-5 without provision IP: %v", err)
	}
}

func TestAdvertiseType5RejectsInvalidProvisionIP(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{
			ASN:          65000,
			RouterID:     "192.168.4.10",
			ProvisionVNI: 4000,
			ProvisionIP:  "not-a-cidr",
			BridgeMAC:    "62:db:b8:c1:80:52",
		},
		log: slog.Default(),
		bgp: server.NewBgpServer(),
	}

	if err := overlay.advertiseType5(context.Background()); err == nil {
		t.Fatal("expected invalid provision IP error")
	}
}

func TestBuildEVPNType5NLRIInvalidIP(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	_, err = buildEVPNType5NLRI(rd, "not-a-cidr", "10.0.0.1", 4000)
	if err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

// mustMarshalPath is a test helper that marshals an NLRI and optional next-hop
// into an apipb.Path for dispatch testing.
func mustMarshalPath(t *testing.T, nlriMsg interface{ ProtoReflect() protoreflect.Message }, nextHop string, withdraw bool) *apipb.Path {
	t.Helper()
	nlri, err := anypb.New(nlriMsg)
	if err != nil {
		t.Fatalf("marshal NLRI: %v", err)
	}
	p := &apipb.Path{
		Nlri:       nlri,
		IsWithdraw: withdraw,
	}
	if nextHop != "" {
		mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
			Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
			NextHops: []string{nextHop},
		})
		if err != nil {
			t.Fatalf("marshal MpReachNLRI: %v", err)
		}
		p.Pattrs = []*anypb.Any{mp}
	}
	return p
}

func mustAddRouteTarget(t *testing.T, p *apipb.Path) *apipb.Path {
	t.Helper()
	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{mustRT2(t, 65000, 4000)},
	})
	if err != nil {
		t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
	}
	p.Pattrs = append(p.Pattrs, extComm)
	return p
}

func TestProcessRouteUpdateDispatch(t *testing.T) {
	// processRouteUpdate should not panic on any NLRI type.
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	tests := []struct {
		name string
		path *apipb.Path
	}{
		{
			name: "nil NLRI is ignored",
			path: &apipb.Path{},
		},
		{
			name: "type-2 route ignored when EnableL2 false",
			path: mustMarshalPath(t, &apipb.EVPNMACIPAdvertisementRoute{
				MacAddress:  "aa:bb:cc:dd:ee:ff",
				IpAddress:   "10.100.0.50",
				EthernetTag: 0,
			}, "10.0.0.1", false),
		},
		{
			name: "type-3 route ignored when EnableL2 false",
			path: mustMarshalPath(t, &apipb.EVPNInclusiveMulticastEthernetTagRoute{
				IpAddress:   "10.0.0.1",
				EthernetTag: 0,
			}, "10.0.0.1", false),
		},
		{
			name: "type-5 route dispatches to handler",
			path: mustMarshalPath(t, &apipb.EVPNIPPrefixRoute{
				IpPrefix:    "10.100.0.0",
				IpPrefixLen: 24,
				GwAddress:   "10.0.0.1",
			}, "10.0.0.1", false),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic.
			overlay.processRouteUpdate(tt.path)
		})
	}
}

func TestProcessRouteUpdateGuardAndL2Branches(t *testing.T) {
	newOverlay := func() (*OverlayTier, *mockOverlayNetlinkOps) {
		mock := &mockOverlayNetlinkOps{}
		return &OverlayTier{
			cfg: &Config{
				RouterID:     "10.0.0.99",
				ASN:          65000,
				ProvisionVNI: 4000,
				BridgeName:   "br.provision",
				EnableL2:     true,
			},
			log:        slog.Default(),
			netlinkOps: mock,
		}, mock
	}
	type5Path := mustAddRouteTarget(t, mustMarshalPath(t, &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "10.0.0.1",
	}, "10.0.0.1", false))

	t.Run("invalid import route target skips dispatch", func(t *testing.T) {
		overlay, mock := newOverlay()
		overlay.cfg.VPNRT = "not-a-route-target"

		overlay.processRouteUpdate(type5Path)

		if mock.linkName != "" {
			t.Fatalf("invalid import RT should skip dispatch, got LinkByName(%q)", mock.linkName)
		}
	})

	t.Run("invalid NLRI skips dispatch", func(t *testing.T) {
		overlay, mock := newOverlay()
		path := mustAddRouteTarget(t, &apipb.Path{
			Nlri: &anypb.Any{TypeUrl: "type.googleapis.com/invalid.Route", Value: []byte{0xff}},
		})

		overlay.processRouteUpdate(path)

		if mock.linkName != "" {
			t.Fatalf("invalid NLRI should skip dispatch, got LinkByName(%q)", mock.linkName)
		}
	})

	t.Run("type-2 dispatch runs when L2 is enabled", func(t *testing.T) {
		overlay, mock := newOverlay()
		path := mustAddRouteTarget(t, mustMarshalPath(t, &apipb.EVPNMACIPAdvertisementRoute{
			MacAddress: "aa:bb:cc:dd:ee:ff",
		}, "10.0.0.1", false))

		overlay.processRouteUpdate(path)

		if len(mock.sets) != 1 {
			t.Fatalf("expected 1 Type-2 neighbor set, got %d", len(mock.sets))
		}
	})

	t.Run("type-3 dispatch runs when L2 is enabled", func(t *testing.T) {
		overlay, mock := newOverlay()
		path := mustAddRouteTarget(t, mustMarshalPath(t, &apipb.EVPNInclusiveMulticastEthernetTagRoute{
			IpAddress: "10.0.0.1",
		}, "10.0.0.1", false))

		overlay.processRouteUpdate(path)

		if len(mock.appends) != 1 {
			t.Fatalf("expected 1 Type-3 neighbor append, got %d", len(mock.appends))
		}
	})

	t.Run("unhandled EVPN route is ignored", func(t *testing.T) {
		overlay, mock := newOverlay()
		path := mustAddRouteTarget(t, mustMarshalPath(t, &apipb.EVPNEthernetAutoDiscoveryRoute{}, "10.0.0.1", false))

		overlay.processRouteUpdate(path)

		if mock.linkName != "" {
			t.Fatalf("unhandled EVPN route should not touch netlink, got LinkByName(%q)", mock.linkName)
		}
	})
}

func TestProcessRouteUpdateType5RouterMACNeighbor(t *testing.T) {
	nlri, err := anypb.New(&apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	})
	if err != nil {
		t.Fatalf("marshal NLRI: %v", err)
	}
	mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{"192.168.4.1"},
	})
	if err != nil {
		t.Fatalf("marshal MpReachNLRI: %v", err)
	}
	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{
			mustRT2(t, 65000, 4000),
			mustRouterMAC(t, "62:db:b8:c1:80:52"),
		},
	})
	if err != nil {
		t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
	}

	newOverlay := func(mock *mockOverlayNetlinkOps) *OverlayTier {
		return &OverlayTier{
			cfg: &Config{
				RouterID:     "192.168.4.10",
				ASN:          65000,
				ProvisionVNI: 4000,
				BridgeName:   "br.provision",
			},
			log:        slog.Default(),
			netlinkOps: mock,
		}
	}

	t.Run("install programs permanent neighbor", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		path := &apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mp, extComm}}

		newOverlay(mock).processRouteUpdate(path)

		if len(mock.sets) != 2 {
			t.Fatalf("expected gateway neighbor and FDB set, got %d sets", len(mock.sets))
		}
		assertType5GatewaySet(t, mock.sets, 0, "192.168.4.1", "62:db:b8:c1:80:52")
	})

	t.Run("withdraw deletes permanent neighbor", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		path := &apipb.Path{
			IsWithdraw: true,
			Nlri:       nlri,
			Pattrs:     []*anypb.Any{mp, extComm},
		}

		newOverlay(mock).processRouteUpdate(path)

		if len(mock.dels) != 2 {
			t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
		}
		assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
	})

	t.Run("withdraw uses stored router MAC when communities are omitted", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		installPath := &apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mp, extComm}}
		withdrawPath := &apipb.Path{
			IsWithdraw: true,
			Nlri:       nlri,
			Pattrs:     []*anypb.Any{mp},
		}

		overlay.processRouteUpdate(installPath)
		overlay.processRouteUpdate(withdrawPath)

		if len(mock.dels) != 2 {
			t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
		}
		assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
	})

	t.Run("invalid router MAC still installs route without neighbor", func(t *testing.T) {
		badExtComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{
				mustRT2(t, 65000, 4000),
				mustRouterMAC(t, "not-a-mac"),
			},
		})
		if err != nil {
			t.Fatalf("marshal bad ExtendedCommunitiesAttribute: %v", err)
		}
		mock := &mockOverlayNetlinkOps{}

		newOverlay(mock).processRouteUpdate(&apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mp, badExtComm}})

		if len(mock.routeReplaces) != 1 {
			t.Fatalf("expected 1 route replace, got %d", len(mock.routeReplaces))
		}
		if len(mock.sets) != 0 {
			t.Fatalf("expected no neighbor set for invalid router MAC, got %d", len(mock.sets))
		}
	})

	t.Run("invalid router MAC preserves existing neighbor", func(t *testing.T) {
		badExtComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{
				mustRT2(t, 65000, 4000),
				mustRouterMAC(t, "not-a-mac"),
			},
		})
		if err != nil {
			t.Fatalf("marshal bad ExtendedCommunitiesAttribute: %v", err)
		}
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)

		overlay.processRouteUpdate(&apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mp, extComm}})
		overlay.processRouteUpdate(&apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mp, badExtComm}})

		if len(mock.routeReplaces) != 2 {
			t.Fatalf("expected 2 route replaces, got %d", len(mock.routeReplaces))
		}
		if len(mock.sets) != 2 {
			t.Fatalf("expected only initial gateway neighbor and FDB set, got %d sets", len(mock.sets))
		}
		if len(mock.dels) != 0 {
			t.Fatalf("invalid router MAC should preserve existing neighbor, got %d deletes", len(mock.dels))
		}
		ref, ok := overlay.loadType5GatewayRef("10.100.0.0/24")
		if !ok {
			t.Fatal("invalid router MAC should preserve stored Type-5 gateway ref")
		}
		if ref.gateway != "192.168.4.1" || ref.routerMAC.String() != "62:db:b8:c1:80:52" {
			t.Fatalf("stored gateway ref = %+v, want gateway 192.168.4.1 and MAC 62:db:b8:c1:80:52", ref)
		}
	})
}

func TestProcessRouteUpdateType5FDBUsesNextHopNotGateway(t *testing.T) {
	nlri, err := anypb.New(&apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "10.255.0.1",
	})
	if err != nil {
		t.Fatalf("marshal NLRI: %v", err)
	}
	mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{"192.168.4.1"},
	})
	if err != nil {
		t.Fatalf("marshal MpReachNLRI: %v", err)
	}
	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{
			mustRT2(t, 65000, 4000),
			mustRouterMAC(t, "62:db:b8:c1:80:52"),
		},
	})
	if err != nil {
		t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
	}
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:     "192.168.4.10",
			ASN:          65000,
			ProvisionVNI: 4000,
			BridgeName:   "br.provision",
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	overlay.processRouteUpdate(&apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mp, extComm}})

	if len(mock.sets) != 2 {
		t.Fatalf("expected gateway neighbor and FDB set, got %d sets", len(mock.sets))
	}
	assertType5GatewayNeighbor(t, mock.sets[0], "10.255.0.1", "62:db:b8:c1:80:52")
	assertType5GatewayFDB(t, mock.sets[1], "192.168.4.1", "62:db:b8:c1:80:52")

	overlay.processRouteUpdate(&apipb.Path{IsWithdraw: true, Nlri: nlri, Pattrs: []*anypb.Any{mp}})

	if len(mock.dels) != 2 {
		t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[0], "10.255.0.1", "62:db:b8:c1:80:52")
	assertType5GatewayFDB(t, mock.dels[1], "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestExtractNextHop(t *testing.T) {
	tests := []struct {
		name      string
		buildPath func(t *testing.T) *apipb.Path
		want      string
	}{
		{
			name: "with MpReach next-hop",
			buildPath: func(t *testing.T) *apipb.Path {
				t.Helper()
				mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
					Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
					NextHops: []string{"10.0.0.1"},
				})
				if err != nil {
					t.Fatalf("anypb.New MpReachNLRIAttribute: %v", err)
				}
				return &apipb.Path{Pattrs: []*anypb.Any{mp}}
			},
			want: "10.0.0.1",
		},
		{
			name: "no MpReach",
			buildPath: func(t *testing.T) *apipb.Path {
				t.Helper()
				return &apipb.Path{}
			},
			want: "",
		},
		{
			name: "MpReach with empty next-hops",
			buildPath: func(t *testing.T) *apipb.Path {
				t.Helper()
				mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
					Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
				})
				if err != nil {
					t.Fatalf("anypb.New MpReachNLRIAttribute: %v", err)
				}
				return &apipb.Path{Pattrs: []*anypb.Any{mp}}
			},
			want: "",
		},
		{
			name: "multiple next-hops returns first",
			buildPath: func(t *testing.T) *apipb.Path {
				t.Helper()
				mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
					Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
					NextHops: []string{"10.0.0.2", "10.0.0.3"},
				})
				if err != nil {
					t.Fatalf("anypb.New MpReachNLRIAttribute: %v", err)
				}
				return &apipb.Path{Pattrs: []*anypb.Any{mp}}
			},
			want: "10.0.0.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.buildPath(t)
			got := extractNextHop(path)
			if got != tt.want {
				t.Errorf("extractNextHop() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractNextHopMixedAttrs(t *testing.T) {
	// MpReach buried among other attributes.
	origin, err := anypb.New(&apipb.OriginAttribute{Origin: 0})
	if err != nil {
		t.Fatalf("marshal origin attribute: %v", err)
	}
	mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{"10.0.0.5"},
	})
	if err != nil {
		t.Fatalf("marshal mp reach attribute: %v", err)
	}
	path := &apipb.Path{Pattrs: []*anypb.Any{origin, mp}}

	got := extractNextHop(path)
	if got != "10.0.0.5" {
		t.Errorf("extractNextHop() = %q, want 10.0.0.5", got)
	}
}

func TestAddGatewayFDB(t *testing.T) {
	tests := []struct {
		name      string
		gateway   string
		appendErr error
		wantErr   bool
		wantMAC   net.HardwareAddr
	}{
		{
			name:    "success",
			gateway: "10.0.0.1",
			wantMAC: net.HardwareAddr{0, 0, 0, 0, 0, 0},
		},
		{
			name:      "neigh append error",
			gateway:   "10.0.0.1",
			appendErr: errors.New("operation not supported"),
			wantErr:   true,
		},
		{
			name:    "invalid gateway IP",
			gateway: "not-an-ip",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockOverlayNetlinkOps{}
			mock.appendErr = tt.appendErr
			overlay := &OverlayTier{
				cfg:        &Config{ProvisionGateway: tt.gateway, ProvisionVNI: 100},
				log:        slog.Default(),
				netlinkOps: mock,
			}
			vxLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "vxlan100"}}
			err := overlay.addGatewayFDB(vxLink)
			if (err != nil) != tt.wantErr {
				t.Fatalf("addGatewayFDB() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(mock.appends) != 1 {
				t.Fatalf("expected 1 NeighAppend call, got %d", len(mock.appends))
			}
			neigh := mock.appends[0]
			if neigh.HardwareAddr.String() != tt.wantMAC.String() {
				t.Errorf("BUM FDB MAC = %s, want %s", neigh.HardwareAddr, tt.wantMAC)
			}
			if !neigh.IP.Equal(net.ParseIP(tt.gateway)) {
				t.Errorf("BUM FDB VTEP IP = %s, want %s", neigh.IP, tt.gateway)
			}
			if neigh.LinkIndex != vxLink.Index {
				t.Errorf("BUM FDB LinkIndex = %d, want %d", neigh.LinkIndex, vxLink.Index)
			}
		})
	}
}

func TestParsePrefixRoute(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		prefixLen uint32
		wantStr   string
		wantErr   bool
	}{
		{"default route", "0.0.0.0", 0, "0.0.0.0/0", false},
		{"subnet", "10.100.0.0", 24, "10.100.0.0/24", false},
		{"host route", "10.0.0.1", 32, "10.0.0.1/32", false},
		{"invalid IP", "not-an-ip", 0, "", true},
		{"IPv4 prefix length too large", "10.0.0.0", 33, "", true},
		{"IPv6 prefix length too large", "2001:db8::", 129, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePrefixRoute(tt.prefix, tt.prefixLen)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePrefixRoute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got.String() != tt.wantStr {
				t.Errorf("parsePrefixRoute() = %s, want %s", got, tt.wantStr)
			}
		})
	}
}

func TestHandleType5RouteInvalidPrefix(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision"},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "not-an-ip",
		IpPrefixLen: 24,
		GwAddress:   "10.0.0.1",
	}
	routerMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}

	// Valid VTEP, gateway and router MAC: only the prefix check stops the install.
	overlay.handleType5Route(route, "10.0.0.1", routerMAC, false)

	assertNoType5NetlinkChanges(t, mock)
	if mock.linkName != "" {
		t.Fatalf("bridge lookup %q ran for an invalid prefix", mock.linkName)
	}
}

func TestHandleType5RouteSelfRoute(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision"},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "10.0.0.99",
	}

	// Self-originated route (vtep == RouterID) — should be silently skipped.
	overlay.handleType5Route(route, "10.0.0.99", nil, false)
	// No route operations should have been attempted.
	if len(mock.appends) != 0 || len(mock.sets) != 0 || len(mock.dels) != 0 ||
		len(mock.routeReplaces) != 0 || len(mock.routeDels) != 0 {
		t.Error("self-originated type-5 route should be skipped")
	}
}

func TestHandleType5RouteNoGateway(t *testing.T) {
	routerMAC := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	tests := []struct {
		name     string
		gw       string
		vtep     string
		withdraw bool
	}{
		// Install with no gateway and no next-hop: the VTEP check stops it.
		{name: "install without gateway or next-hop", gw: "", vtep: ""},
		// Withdrawals skip the VTEP check, so only the gateway check stops it.
		{name: "withdraw without gateway or next-hop", gw: "", vtep: "", withdraw: true},
		// Valid VTEP but an unparsable gateway: only the gateway check stops it.
		{name: "install with unparsable gateway", gw: "not-an-ip", vtep: "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockOverlayNetlinkOps{}
			overlay := &OverlayTier{
				cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision"},
				log:        slog.Default(),
				netlinkOps: mock,
			}
			route := &apipb.EVPNIPPrefixRoute{
				IpPrefix:    "10.50.0.0",
				IpPrefixLen: 24,
				GwAddress:   tt.gw,
			}

			overlay.handleType5Route(route, tt.vtep, routerMAC, tt.withdraw)

			assertNoType5NetlinkChanges(t, mock)
		})
	}
}

func assertNoType5NetlinkChanges(t *testing.T, mock *mockOverlayNetlinkOps) {
	t.Helper()
	if len(mock.routeReplaces) != 0 || len(mock.routeDels) != 0 {
		t.Fatalf("route changes: replaces=%v dels=%v, want none", mock.routeReplaces, mock.routeDels)
	}
	if len(mock.sets) != 0 || len(mock.appends) != 0 || len(mock.dels) != 0 {
		t.Fatalf("neighbor changes: sets=%v appends=%v dels=%v, want none", mock.sets, mock.appends, mock.dels)
	}
}

func TestType5RouteStateGateway(t *testing.T) {
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision"},
		log:        slog.Default(),
		netlinkOps: &mockOverlayNetlinkOps{},
	}
	for _, tt := range []struct {
		prefix, gw, want string
	}{
		{"10.50.0.0", "", "10.0.0.1"},
		{"10.50.0.0", "0.0.0.0", "10.0.0.1"},
		{"10.50.0.0", "10.0.0.7", "10.0.0.7"},
		{"2001:db8::", "::", "::ffff:10.0.0.1"},
		{"10.50.0.0", "not-an-ip", ""},
	} {
		route := &apipb.EVPNIPPrefixRoute{IpPrefix: tt.prefix, IpPrefixLen: 24, GwAddress: tt.gw}
		state, ok := overlay.type5RouteState(route, "10.0.0.1", net.ParseIP("10.0.0.1"))
		if tt.want == "" {
			if ok {
				t.Errorf("gateway %q: state %+v, want none", tt.gw, state)
			}
			continue
		}
		// An IPv6 route needs the 16-byte IPv4-mapped form.
		if !ok || !state.gw.Equal(net.ParseIP(tt.want)) || isMappedGateway(state.gw) != strings.Contains(tt.prefix, ":") {
			t.Errorf("gateway %q of %s: got %v (%v), want %s", tt.gw, tt.prefix, state.gw, ok, tt.want)
		}
	}
}

func TestHandleType5RouteInstallsOnlinkGatewayRoute(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	routerMAC := mustParseMAC(t, "62:db:b8:c1:80:52")
	overlay.handleType5Route(route, "192.168.4.1", routerMAC, false)

	if len(mock.routeReplaces) != 1 {
		t.Fatalf("expected 1 route replace, got %d", len(mock.routeReplaces))
	}
	installed := mock.routeReplaces[0]
	if installed.LinkIndex != 42 {
		t.Errorf("LinkIndex = %d, want 42", installed.LinkIndex)
	}
	if installed.Dst.String() != "10.100.0.0/24" {
		t.Errorf("Dst = %s, want 10.100.0.0/24", installed.Dst)
	}
	if !installed.Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Errorf("Gw = %s, want 192.168.4.1", installed.Gw)
	}
	if installed.Table != 20 {
		t.Errorf("Table = %d, want 20", installed.Table)
	}
	if installed.Flags&int(netlink.FLAG_ONLINK) == 0 {
		t.Errorf("Flags = %v, want FLAG_ONLINK", installed.Flags)
	}
	if len(mock.sets) != 2 {
		t.Fatalf("expected gateway neighbor and FDB set, got %d sets", len(mock.sets))
	}
	assertType5GatewaySet(t, mock.sets, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestHandleType5RouteWithdrawDeletesOnlinkGatewayRoute(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	routerMAC := mustParseMAC(t, "62:db:b8:c1:80:52")
	overlay.handleType5Route(route, "192.168.4.1", routerMAC, true)

	if len(mock.routeDels) != 1 {
		t.Fatalf("expected 1 route delete, got %d", len(mock.routeDels))
	}
	deleted := mock.routeDels[0]
	if deleted.Dst.String() != "10.100.0.0/24" {
		t.Errorf("Dst = %s, want 10.100.0.0/24", deleted.Dst)
	}
	if deleted.Flags&int(netlink.FLAG_ONLINK) == 0 {
		t.Errorf("Flags = %v, want FLAG_ONLINK", deleted.Flags)
	}
	if len(mock.dels) != 2 {
		t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestHandleType5RouteWithdrawUsesStoredRouterMAC(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	overlay.handleType5Route(route, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	overlay.handleType5Route(route, "192.168.4.1", nil, true)

	if len(mock.dels) != 2 {
		t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestHandleType5RouteKeepsSharedGatewayNeighborUntilLastPrefixWithdraw(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeA := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	routeB := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.200.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	routerMAC := mustParseMAC(t, "62:db:b8:c1:80:52")

	overlay.handleType5Route(routeA, "192.168.4.1", routerMAC, false)
	overlay.handleType5Route(routeB, "192.168.4.1", routerMAC, false)
	overlay.handleType5Route(routeA, "192.168.4.1", nil, true)

	if len(mock.dels) != 0 {
		t.Fatalf("shared gateway neighbor was deleted before the last prefix withdrew")
	}

	overlay.handleType5Route(routeB, "192.168.4.1", nil, true)

	if len(mock.dels) != 2 {
		t.Fatalf("expected gateway neighbor and FDB delete after last prefix withdraw, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestHandleType5RouteMovesPrefixToNewGateway(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	oldRoute := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	newRoute := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.2",
	}

	overlay.handleType5Route(oldRoute, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	overlay.handleType5Route(newRoute, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:53"), false)

	if len(mock.dels) != 2 {
		t.Fatalf("expected old gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")

	overlay.handleType5Route(newRoute, "192.168.4.2", nil, true)

	if len(mock.dels) != 4 {
		t.Fatalf("expected new gateway neighbor and FDB delete after withdraw, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 2, "192.168.4.2", "62:db:b8:c1:80:53")
}

func TestHandleType5RouteInvalidRouterMACMovesPrefixWithoutNewNeighbor(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	oldRoute := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	newRoute := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.2",
	}

	overlay.handleType5Route(oldRoute, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	overlay.handleType5RouteWithRouterMACState(newRoute, "192.168.4.22", nil, false, false)

	if len(mock.sets) != 2 {
		t.Fatalf("expected only initial gateway neighbor and FDB set, got %d sets", len(mock.sets))
	}
	if len(mock.dels) != 2 {
		t.Fatalf("expected old gateway neighbor and FDB delete after invalid-RMAC gateway move, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
	ref, ok := overlay.loadType5GatewayRef("10.100.0.0/24")
	if !ok {
		t.Fatal("invalid-RMAC gateway move should keep prefix ref for future cleanup")
	}
	if ref.gateway != "192.168.4.2" || ref.vtep != "192.168.4.22" || len(ref.routerMAC) != 0 {
		t.Fatalf("stored gateway ref = %+v, want gateway 192.168.4.2, vtep 192.168.4.22, and no router MAC", ref)
	}
	if _, ok := overlay.type5GatewayMAC.Load("192.168.4.2"); ok {
		t.Fatal("invalid-RMAC gateway move should not overwrite router MAC cache for new gateway")
	}
}

func TestHandleType5RouteInvalidRouterMACRestoresOldRefOnDeleteFailure(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	oldRoute := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	newRoute := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.2",
	}

	overlay.handleType5Route(oldRoute, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	mock.delErr = errors.New("delete failed")
	overlay.handleType5RouteWithRouterMACState(newRoute, "192.168.4.22", nil, false, false)

	if len(mock.dels) != 2 {
		t.Fatalf("expected attempted old gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	ref, ok := overlay.loadType5GatewayRef("10.100.0.0/24")
	if !ok {
		t.Fatal("failed old gateway cleanup should keep prefix ref for retry")
	}
	if ref.gateway != "192.168.4.1" || ref.vtep != "192.168.4.1" || ref.routerMAC.String() != "62:db:b8:c1:80:52" {
		t.Fatalf("stored gateway ref = %+v, want restored old gateway/VTEP/MAC", ref)
	}
}

func TestHandleType5RouteKeepsOldGatewayWhenAnotherPrefixStillUsesIt(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeA := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	routeAMoved := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.2",
	}
	routeB := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.200.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	oldMAC := mustParseMAC(t, "62:db:b8:c1:80:52")

	overlay.handleType5Route(routeA, "192.168.4.1", oldMAC, false)
	overlay.handleType5Route(routeB, "192.168.4.1", oldMAC, false)
	overlay.handleType5Route(routeAMoved, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:53"), false)

	if len(mock.dels) != 0 {
		t.Fatalf("shared old gateway neighbor was deleted while another prefix still used it")
	}

	overlay.handleType5Route(routeB, "192.168.4.1", nil, true)

	if len(mock.dels) != 2 {
		t.Fatalf("expected old gateway neighbor and FDB delete after remaining old prefix withdrew, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestHandleType5RouteReplacesFDBWhenGatewayReusesDifferentVTEP(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	overlay.handleType5Route(route, "192.168.4.11", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	overlay.handleType5Route(route, "192.168.4.12", mustParseMAC(t, "62:db:b8:c1:80:53"), false)

	if len(mock.dels) != 1 {
		t.Fatalf("expected only the old FDB to be deleted, got %d deletes", len(mock.dels))
	}
	assertType5GatewayFDB(t, mock.dels[0], "192.168.4.11", "62:db:b8:c1:80:52")

	overlay.handleType5Route(route, "192.168.4.12", nil, true)

	if len(mock.dels) != 3 {
		t.Fatalf("expected new gateway neighbor and FDB delete after withdraw, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[1], "192.168.4.1", "62:db:b8:c1:80:53")
	assertType5GatewayFDB(t, mock.dels[2], "192.168.4.12", "62:db:b8:c1:80:53")
}

func TestHandleType5RouteKeepsSharedFDBUntilLastGatewayWithdraws(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeA := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	routeB := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.200.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.2",
	}
	routerMAC := mustParseMAC(t, "62:db:b8:c1:80:52")

	overlay.handleType5Route(routeA, "192.168.4.11", routerMAC, false)
	overlay.handleType5Route(routeB, "192.168.4.11", routerMAC, false)
	overlay.handleType5Route(routeA, "192.168.4.11", nil, true)

	if len(mock.dels) != 1 {
		t.Fatalf("expected only gateway A neighbor delete while FDB is shared, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[0], "192.168.4.1", "62:db:b8:c1:80:52")

	overlay.handleType5Route(routeB, "192.168.4.11", nil, true)

	if len(mock.dels) != 3 {
		t.Fatalf("expected gateway B neighbor and shared FDB delete after last withdraw, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[1], "192.168.4.2", "62:db:b8:c1:80:52")
	assertType5GatewayFDB(t, mock.dels[2], "192.168.4.11", "62:db:b8:c1:80:52")
}

func TestHandleType5RouteClearsStoredNeighborWhenRouteUpdateHasNoRouterMAC(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	overlay.handleType5Route(route, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	overlay.handleType5Route(route, "192.168.4.1", nil, false)

	if len(mock.dels) != 2 {
		t.Fatalf("expected old gateway neighbor and FDB delete after no-RMAC update, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
	if overlay.hasType5GatewayRef("192.168.4.1") {
		t.Fatal("no-RMAC route update should clear stored Type-5 gateway ref")
	}
}

func TestClearType5GatewayNeighborSkipsMissingAndSharedRefs(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayRefs.Store("10.200.0.0/24", type5GatewayRef{
		gateway:   "192.168.4.1",
		vtep:      "192.168.4.1",
		routerMAC: mustParseMAC(t, "62:db:b8:c1:80:52"),
	})
	overlay.type5GatewayRefs.Store("10.100.0.0/24", type5GatewayRef{
		gateway:   "192.168.4.1",
		vtep:      "192.168.4.1",
		routerMAC: mustParseMAC(t, "62:db:b8:c1:80:52"),
	})

	overlay.clearType5GatewayNeighbor(link, "10.0.0.0/24")
	overlay.clearType5GatewayNeighbor(link, "10.100.0.0/24")

	if len(mock.dels) != 0 {
		t.Fatalf("expected no neighbor deletes for missing or still-shared refs, got %d", len(mock.dels))
	}
}

func TestClearType5GatewayNeighborRestoresRefOnDeleteFailure(t *testing.T) {
	mock := &mockOverlayNetlinkOps{delErr: errors.New("delete failed")}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", type5GatewayRef{
		gateway:   "192.168.4.1",
		vtep:      "192.168.4.1",
		routerMAC: mustParseMAC(t, "62:db:b8:c1:80:52"),
	})

	overlay.clearType5GatewayNeighbor(link, "10.100.0.0/24")

	if len(mock.dels) != 2 {
		t.Fatalf("expected attempted gateway neighbor and FDB deletes, got %d deletes", len(mock.dels))
	}
	if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); !ok {
		t.Fatal("failed clear should restore Type-5 gateway ref for retry")
	}
}

func TestClearType5GatewayNeighborRestoresRefWhenFDBBuildFails(t *testing.T) {
	mock := &mockOverlayNetlinkOps{linkErr: errors.New("missing vxlan"), linkErrName: "vx1000"}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", type5GatewayRef{
		gateway:   "192.168.4.1",
		vtep:      "192.168.4.1",
		routerMAC: mustParseMAC(t, "62:db:b8:c1:80:52"),
	})
	overlay.type5GatewayMAC.Store("192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"))

	overlay.clearType5GatewayNeighbor(link, "10.100.0.0/24")

	if len(mock.dels) != 1 {
		t.Fatalf("expected bridge neighbor delete before failed FDB build, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[0], "192.168.4.1", "62:db:b8:c1:80:52")
	if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); !ok {
		t.Fatal("failed FDB build should restore Type-5 gateway ref for retry")
	}
	if _, ok := overlay.type5GatewayMAC.Load("192.168.4.1"); !ok {
		t.Fatal("failed FDB build should keep cached router MAC for retry")
	}
}

func TestClearType5GatewayNeighborRestoresRefWhenNeighborBuildFails(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", type5GatewayRef{
		gateway:   "not-an-ip",
		vtep:      "192.168.4.1",
		routerMAC: mustParseMAC(t, "62:db:b8:c1:80:52"),
	})

	overlay.clearType5GatewayNeighbor(link, "10.100.0.0/24")

	if len(mock.dels) != 1 {
		t.Fatalf("expected FDB delete attempt despite malformed gateway, got %d deletes", len(mock.dels))
	}
	assertType5GatewayFDB(t, mock.dels[0], "192.168.4.1", "62:db:b8:c1:80:52")
	if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); !ok {
		t.Fatal("failed neighbor build should restore Type-5 gateway ref for retry")
	}
}

func TestHandleType5RouteKeepsNeighborWhenWithdrawRouteDeleteFails(t *testing.T) {
	mock := &mockOverlayNetlinkOps{routeDelErr: errors.New("delete failed")}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	overlay.handleType5Route(route, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)
	overlay.handleType5Route(route, "192.168.4.1", nil, true)

	if len(mock.dels) != 0 {
		t.Fatalf("route delete failure should keep gateway neighbor, got %d neighbor deletes", len(mock.dels))
	}

	mock.routeDelErr = nil
	overlay.handleType5Route(route, "192.168.4.1", nil, true)

	if len(mock.dels) != 2 {
		t.Fatalf("expected retained gateway neighbor and FDB to delete after successful retry, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestBuildType5GatewayNeighbor(t *testing.T) {
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	t.Run("builds IPv4 permanent neighbor", func(t *testing.T) {
		neigh := buildType5GatewayNeighbor(link, net.ParseIP("192.168.4.1").To4(), mustParseMAC(t, "62:db:b8:c1:80:52"))

		assertType5GatewayNeighbor(t, neigh, "192.168.4.1", "62:db:b8:c1:80:52")
	})

	t.Run("skips empty router MAC", func(t *testing.T) {
		if got := buildType5GatewayNeighbor(link, net.ParseIP("192.168.4.1").To4(), nil); got != nil {
			t.Fatalf("neighbor = %#v, want nil", got)
		}
	})

	t.Run("skips EUI-64 router MAC", func(t *testing.T) {
		mac := mustParseMAC(t, "02:00:00:ff:fe:00:00:10")
		if got := buildType5GatewayNeighbor(link, net.ParseIP("192.168.4.1").To4(), mac); got != nil {
			t.Fatalf("neighbor = %#v, want nil", got)
		}
	})

	t.Run("skips IPv6 gateway", func(t *testing.T) {
		mac := mustParseMAC(t, "62:db:b8:c1:80:52")
		if got := buildType5GatewayNeighbor(link, net.ParseIP("2001:db8::1"), mac); got != nil {
			t.Fatalf("neighbor = %#v, want nil", got)
		}
	})
}

func TestBuildType5GatewayFDB(t *testing.T) {
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 43, Name: "vx1000"}}

	t.Run("builds IPv4 permanent bridge FDB entry", func(t *testing.T) {
		fdb := buildType5GatewayFDB(link, net.ParseIP("192.168.4.1"), mustParseMAC(t, "62:db:b8:c1:80:52"))

		assertType5GatewayFDB(t, fdb, "192.168.4.1", "62:db:b8:c1:80:52")
	})

	t.Run("skips nil VXLAN link", func(t *testing.T) {
		if got := buildType5GatewayFDB(nil, net.ParseIP("192.168.4.1"), mustParseMAC(t, "62:db:b8:c1:80:52")); got != nil {
			t.Fatalf("FDB = %#v, want nil", got)
		}
	})

	t.Run("skips empty router MAC", func(t *testing.T) {
		if got := buildType5GatewayFDB(link, net.ParseIP("192.168.4.1"), nil); got != nil {
			t.Fatalf("FDB = %#v, want nil", got)
		}
	})

	t.Run("skips EUI-64 router MAC", func(t *testing.T) {
		mac := mustParseMAC(t, "02:00:00:ff:fe:00:00:10")
		if got := buildType5GatewayFDB(link, net.ParseIP("192.168.4.1"), mac); got != nil {
			t.Fatalf("FDB = %#v, want nil", got)
		}
	})

	t.Run("skips IPv6 VTEP", func(t *testing.T) {
		if got := buildType5GatewayFDB(link, net.ParseIP("2001:db8::1"), mustParseMAC(t, "62:db:b8:c1:80:52")); got != nil {
			t.Fatalf("FDB = %#v, want nil", got)
		}
	})
}

func TestOverlayBuildType5GatewayFDBSkipsMissingState(t *testing.T) {
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	routerMAC := mustParseMAC(t, "62:db:b8:c1:80:52")

	overlay := &OverlayTier{netlinkOps: &mockOverlayNetlinkOps{}, log: slog.Default()}
	if got := overlay.buildType5GatewayFDB(net.ParseIP("192.168.4.1"), routerMAC); got != nil {
		t.Fatalf("FDB with nil config = %#v, want nil", got)
	}

	mock := &mockOverlayNetlinkOps{linkErr: errors.New("missing vxlan"), linkErrName: "vx1000"}
	overlay = &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	if got := overlay.buildType5GatewayFDB(net.ParseIP("192.168.4.1"), routerMAC); got != nil {
		t.Fatalf("FDB with missing VXLAN = %#v, want nil", got)
	}

	overlay = &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: &mockOverlayNetlinkOps{},
	}
	fdb := overlay.buildType5GatewayFDB(net.ParseIP("192.168.4.1"), routerMAC)
	assertType5GatewayFDB(t, fdb, "192.168.4.1", "62:db:b8:c1:80:52")
	if link.Attrs().Index != 42 {
		t.Fatalf("bridge test fixture index changed unexpectedly: %d", link.Attrs().Index)
	}
}

func TestSetType5GatewayNeighborSkipsInvalidInputs(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.setType5GatewayNeighbor(link, "10.100.0.0/24", net.ParseIP("192.168.4.1"), net.ParseIP("192.168.4.1"), nil)
	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("2001:db8::1"),
		net.ParseIP("192.168.4.1"),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)
	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1"),
		net.ParseIP("2001:db8::1"),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.sets) != 0 {
		t.Fatalf("expected no neighbor sets for invalid Type-5 gateway inputs, got %d", len(mock.sets))
	}
}

func TestSetType5GatewayNeighborDoesNotTrackFailedSet(t *testing.T) {
	mock := &mockOverlayNetlinkOps{setErr: errors.New("set failed")}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.sets) != 1 {
		t.Fatalf("expected 1 attempted neighbor set, got %d", len(mock.sets))
	}
	if overlay.hasType5GatewayRef("192.168.4.1") {
		t.Fatal("failed neighbor set should not track a Type-5 gateway ref")
	}
}

func TestSetType5GatewayNeighborSkipsWhenVXLANMissing(t *testing.T) {
	mock := &mockOverlayNetlinkOps{linkErr: errors.New("missing vxlan"), linkErrName: "vx1000"}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.sets) != 0 {
		t.Fatalf("missing VXLAN should skip all neighbor/FDB sets, got %d", len(mock.sets))
	}
	if overlay.hasType5GatewayRef("192.168.4.1") {
		t.Fatal("missing VXLAN should not track a Type-5 gateway ref")
	}
}

func TestSetType5GatewayNeighborRollsBackFailedFDBSet(t *testing.T) {
	mock := &mockOverlayNetlinkOps{setErr: errors.New("fdb set failed"), setErrOnCall: 2}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.sets) != 2 {
		t.Fatalf("expected gateway neighbor set and failed FDB set, got %d sets", len(mock.sets))
	}
	assertType5GatewaySet(t, mock.sets, 0, "192.168.4.1", "62:db:b8:c1:80:52")
	if len(mock.dels) != 1 {
		t.Fatalf("expected bridge neighbor rollback, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[0], "192.168.4.1", "62:db:b8:c1:80:52")
	if overlay.hasType5GatewayRef("192.168.4.1") {
		t.Fatal("failed FDB set should not track a Type-5 gateway ref")
	}
	if _, ok := overlay.type5GatewayMAC.Load("192.168.4.1"); ok {
		t.Fatal("failed FDB set should not cache a router MAC")
	}
}

func TestSetType5GatewayNeighborToleratesRollbackDeleteFailure(t *testing.T) {
	mock := &mockOverlayNetlinkOps{
		setErr:       errors.New("fdb set failed"),
		setErrOnCall: 2,
		delErr:       errors.New("rollback failed"),
	}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.sets) != 2 {
		t.Fatalf("expected gateway neighbor set and failed FDB set, got %d sets", len(mock.sets))
	}
	if len(mock.dels) != 1 {
		t.Fatalf("expected one failed rollback delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[0], "192.168.4.1", "62:db:b8:c1:80:52")
	if overlay.hasType5GatewayRef("192.168.4.1") {
		t.Fatal("failed rollback should not track a Type-5 gateway ref")
	}
}

func TestDeleteType5GatewayNeighborSkipsWhenNoRouterMAC(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.deleteType5GatewayNeighbor(link, "10.100.0.0/24", net.ParseIP("192.168.4.1").To4(), nil, nil)

	if len(mock.dels) != 0 {
		t.Fatalf("expected no neighbor delete without a router MAC, got %d", len(mock.dels))
	}
}

func TestDeleteType5GatewayNeighborUsesCachedRouterMAC(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayMAC.Store("192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"))

	overlay.deleteType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		nil,
	)

	if len(mock.dels) != 2 {
		t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestDeleteType5GatewayNeighborIgnoresMalformedStoredRef(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", "not-a-ref")

	overlay.deleteType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.dels) != 2 {
		t.Fatalf("expected malformed stored ref to fall back to supplied gateway/MAC, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
}

func TestDeleteType5GatewayNeighborPrefersExplicitRouterMAC(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", type5GatewayRef{
		gateway:   "192.168.4.1",
		vtep:      "192.168.4.1",
		routerMAC: mustParseMAC(t, "62:db:b8:c1:80:52"),
	})

	overlay.deleteType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		mustParseMAC(t, "62:db:b8:c1:80:53"),
	)

	if len(mock.dels) != 2 {
		t.Fatalf("expected gateway neighbor and FDB delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:53")
}

func TestDeleteType5GatewayNeighborLogsDeleteFailure(t *testing.T) {
	mock := &mockOverlayNetlinkOps{delErr: errors.New("delete failed")}
	overlay := &OverlayTier{
		cfg:        &Config{ProvisionVNI: 1000},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	overlay.setType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)
	overlay.deleteType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1").To4(),
		net.ParseIP("192.168.4.1").To4(),
		nil,
	)

	if len(mock.dels) != 2 {
		t.Fatalf("expected attempted gateway neighbor and FDB deletes, got %d deletes", len(mock.dels))
	}
	if _, ok := overlay.type5GatewayMAC.Load("192.168.4.1"); !ok {
		t.Fatal("failed neighbor delete should keep cached router MAC for retry")
	}
	if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); !ok {
		t.Fatal("failed neighbor delete should restore Type-5 gateway ref for retry")
	}
}

func TestDeleteType5GatewayNeighborRestoresRefOnPartialDeleteFailure(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failCall int
	}{
		{
			name:     "neighbor delete fails",
			failCall: 1,
		},
		{
			name:     "FDB delete fails",
			failCall: 2,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockOverlayNetlinkOps{delErr: errors.New("delete failed"), delErrOnCall: tt.failCall}
			overlay := &OverlayTier{
				cfg:        &Config{ProvisionVNI: 1000},
				log:        slog.Default(),
				netlinkOps: mock,
			}
			link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

			overlay.setType5GatewayNeighbor(
				link,
				"10.100.0.0/24",
				net.ParseIP("192.168.4.1").To4(),
				net.ParseIP("192.168.4.1").To4(),
				mustParseMAC(t, "62:db:b8:c1:80:52"),
			)
			overlay.deleteType5GatewayNeighbor(
				link,
				"10.100.0.0/24",
				net.ParseIP("192.168.4.1").To4(),
				net.ParseIP("192.168.4.1").To4(),
				nil,
			)

			if len(mock.dels) != 2 {
				t.Fatalf("expected attempted gateway neighbor and FDB deletes, got %d deletes", len(mock.dels))
			}
			assertType5GatewayDelete(t, mock.dels, 0, "192.168.4.1", "62:db:b8:c1:80:52")
			if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); !ok {
				t.Fatal("partial delete failure should restore Type-5 gateway ref for retry")
			}
			if _, ok := overlay.type5GatewayMAC.Load("192.168.4.1"); !ok {
				t.Fatal("partial delete failure should keep cached router MAC for retry")
			}
		})
	}
}

func TestLoadType5GatewayRefRejectsMalformedStoredValue(t *testing.T) {
	overlay := &OverlayTier{}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", "not-a-ref")

	if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); ok {
		t.Fatal("expected malformed Type-5 gateway ref to be ignored")
	}
}

func TestHasType5GatewayFDBRefSkipsEmptyRouterMAC(t *testing.T) {
	overlay := &OverlayTier{}
	overlay.type5GatewayRefs.Store("10.100.0.0/24", type5GatewayRef{
		gateway: "192.168.4.1",
		vtep:    "192.168.4.11",
	})

	if overlay.hasType5GatewayFDBRef("192.168.4.11", nil) {
		t.Fatal("empty router MAC should not match a Type-5 gateway FDB ref")
	}
}

func TestRouterMACInCommunitiesIgnoresUnmarshalFailures(t *testing.T) {
	mac, err := routerMACInCommunities([]*anypb.Any{
		{TypeUrl: "type.googleapis.com/invalid", Value: []byte{0xff}},
		mustRouterMAC(t, "62:db:b8:c1:80:52"),
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mac.String() != "62:db:b8:c1:80:52" {
		t.Fatalf("router MAC = %s, want 62:db:b8:c1:80:52", mac)
	}
}

func TestRouterMACInCommunitiesRejectsEUI64(t *testing.T) {
	_, err := routerMACInCommunities([]*anypb.Any{mustRouterMAC(t, "02:00:00:ff:fe:00:00:10")})

	if err == nil {
		t.Fatal("expected EUI-64 router MAC error")
	}
}

func TestExtractRouterMACIgnoresMalformedAndIrrelevantAttributes(t *testing.T) {
	origin, err := anypb.New(&apipb.OriginAttribute{Origin: 0})
	if err != nil {
		t.Fatalf("marshal OriginAttribute: %v", err)
	}
	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{mustRouterMAC(t, "62:db:b8:c1:80:52")},
	})
	if err != nil {
		t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
	}

	mac, err := extractRouterMAC(&apipb.Path{Pattrs: []*anypb.Any{
		{TypeUrl: "type.googleapis.com/invalid.Attribute", Value: []byte{0xff}},
		origin,
		extComm,
	}})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mac.String() != "62:db:b8:c1:80:52" {
		t.Fatalf("router MAC = %s, want 62:db:b8:c1:80:52", mac)
	}
}

func TestHandleType5RouteSkipsWhenBridgeMissing(t *testing.T) {
	mock := &mockOverlayNetlinkOps{linkErr: errors.New("missing bridge")}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:   "192.168.4.10",
			BridgeName: "br.provision",
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	overlay.handleType5Route(route, "192.168.4.1", nil, false)

	if mock.linkName != "br.provision" {
		t.Errorf("LinkByName called with %q, want br.provision", mock.linkName)
	}
	if len(mock.routeReplaces) != 0 || len(mock.routeDels) != 0 {
		t.Fatal("missing bridge should not attempt route operations")
	}
}

func TestHandleType5RouteKeepsFailedInstallRouteForAssertion(t *testing.T) {
	mock := &mockOverlayNetlinkOps{routeReplaceErr: errors.New("replace failed")}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:   "192.168.4.10",
			BridgeName: "br.provision",
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "0.0.0.0",
	}

	overlay.handleType5Route(route, "192.168.4.1", nil, false)

	if len(mock.routeReplaces) != 1 {
		t.Fatalf("expected 1 route replace, got %d", len(mock.routeReplaces))
	}
	installed := mock.routeReplaces[0]
	if installed.Table != 0 {
		t.Errorf("Table = %d, want default table", installed.Table)
	}
	if !installed.Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Errorf("Gw = %s, want fallback VTEP gateway", installed.Gw)
	}
}

func TestHandleType5RouteKeepsFailedWithdrawRouteForAssertion(t *testing.T) {
	mock := &mockOverlayNetlinkOps{routeDelErr: errors.New("delete failed")}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}

	overlay.handleType5Route(route, "192.168.4.1", nil, true)

	if len(mock.routeDels) != 1 {
		t.Fatalf("expected 1 route delete, got %d", len(mock.routeDels))
	}
	deleted := mock.routeDels[0]
	if deleted.Table != 20 {
		t.Errorf("Table = %d, want 20", deleted.Table)
	}
	if deleted.Flags&int(netlink.FLAG_ONLINK) == 0 {
		t.Errorf("Flags = %v, want FLAG_ONLINK", deleted.Flags)
	}
}

func TestOverlayRouteTable(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want int
	}{
		{
			name: "default namespace",
			cfg:  &Config{OverlayVRFName: "", OverlayVRFTableID: 20},
			want: 0,
		},
		{
			name: "overlay VRF",
			cfg:  &Config{OverlayVRFName: "Vrf_overlay", OverlayVRFTableID: 20},
			want: 20,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay := &OverlayTier{cfg: tt.cfg}
			if got := overlay.overlayRouteTable(); got != tt.want {
				t.Errorf("overlayRouteTable() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildType5KernelRouteMarksGatewayOnlink(t *testing.T) {
	dst := &net.IPNet{IP: net.ParseIP("10.100.0.0"), Mask: net.CIDRMask(24, 32)}
	gw := net.ParseIP("192.168.4.1")
	link := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	route := buildType5KernelRoute(link, dst, gw, 20)

	if route.LinkIndex != 42 {
		t.Errorf("LinkIndex = %d, want 42", route.LinkIndex)
	}
	if route.Dst.String() != "10.100.0.0/24" {
		t.Errorf("Dst = %s, want 10.100.0.0/24", route.Dst)
	}
	if !route.Gw.Equal(gw) {
		t.Errorf("Gw = %s, want %s", route.Gw, gw)
	}
	if route.Table != 20 {
		t.Errorf("Table = %d, want 20", route.Table)
	}
	if route.Flags&int(netlink.FLAG_ONLINK) == 0 {
		t.Errorf("Flags = %v, want FLAG_ONLINK", route.Flags)
	}
}

func TestBuildType5KernelRouteOmitsOnlinkWithoutGateway(t *testing.T) {
	dst := &net.IPNet{IP: net.ParseIP("10.100.0.0"), Mask: net.CIDRMask(24, 32)}
	link := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Index: 42, Name: "br.provision"}}

	route := buildType5KernelRoute(link, dst, nil, 0)

	if route.Table != 0 {
		t.Errorf("Table = %d, want 0", route.Table)
	}
	if route.Flags&int(netlink.FLAG_ONLINK) != 0 {
		t.Errorf("Flags = %v, did not expect FLAG_ONLINK", route.Flags)
	}
}

// --- Type-2 handler tests ---------------------------------------------------

func TestHandleType2RouteInstallsFDB(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNMACIPAdvertisementRoute{
		MacAddress: "aa:bb:cc:dd:ee:ff",
	}
	overlay.handleType2Route(route, "10.0.0.1", false)

	if len(mock.sets) != 1 {
		t.Fatalf("expected 1 NeighSet call, got %d", len(mock.sets))
	}
	if mock.sets[0].HardwareAddr.String() != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MAC = %s, want aa:bb:cc:dd:ee:ff", mock.sets[0].HardwareAddr)
	}
	if !mock.sets[0].IP.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("VTEP IP = %s, want 10.0.0.1", mock.sets[0].IP)
	}
	if got, ok := overlay.macVTEP.Load("aa:bb:cc:dd:ee:ff"); !ok || got != "10.0.0.1" {
		t.Errorf("macVTEP not tracked")
	}
}

func TestHandleType2RouteSelfSkipped(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNMACIPAdvertisementRoute{MacAddress: "aa:bb:cc:dd:ee:ff"}
	overlay.handleType2Route(route, "10.0.0.99", false)

	if len(mock.sets) != 0 {
		t.Error("self-originated type-2 route should be skipped")
	}
}

func TestHandleType2RouteWithdraw(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	overlay.macVTEP.Store("aa:bb:cc:dd:ee:ff", "10.0.0.1")

	route := &apipb.EVPNMACIPAdvertisementRoute{MacAddress: "aa:bb:cc:dd:ee:ff"}
	overlay.handleType2Route(route, "", true)

	if len(mock.dels) != 1 {
		t.Fatalf("expected 1 NeighDel call, got %d", len(mock.dels))
	}
	if _, ok := overlay.macVTEP.Load("aa:bb:cc:dd:ee:ff"); ok {
		t.Error("macVTEP entry should be removed on withdraw")
	}
}

func TestHandleType2RouteInvalidMAC(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNMACIPAdvertisementRoute{MacAddress: "not-a-mac"}
	overlay.handleType2Route(route, "10.0.0.1", false)

	if len(mock.sets) != 0 {
		t.Error("invalid MAC should be skipped")
	}
}

// --- Type-3 handler tests ---------------------------------------------------

func TestHandleType3RouteAppendsBUM(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNInclusiveMulticastEthernetTagRoute{IpAddress: "10.0.0.1"}
	overlay.handleType3Route(route, "10.0.0.1", false)

	if len(mock.appends) != 1 {
		t.Fatalf("expected 1 NeighAppend call, got %d", len(mock.appends))
	}
	if mock.appends[0].HardwareAddr.String() != "00:00:00:00:00:00" {
		t.Errorf("BUM MAC = %s, want 00:00:00:00:00:00", mock.appends[0].HardwareAddr)
	}
	if !mock.appends[0].IP.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("VTEP IP = %s, want 10.0.0.1", mock.appends[0].IP)
	}
}

func TestHandleType3RouteSelfSkipped(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNInclusiveMulticastEthernetTagRoute{IpAddress: "10.0.0.99"}
	overlay.handleType3Route(route, "10.0.0.99", false)

	if len(mock.appends) != 0 {
		t.Error("self-originated type-3 route should be skipped")
	}
}

func TestHandleType3RouteWithdraw(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNInclusiveMulticastEthernetTagRoute{IpAddress: "10.0.0.1"}
	overlay.handleType3Route(route, "10.0.0.1", true)

	if len(mock.dels) != 1 {
		t.Fatalf("expected 1 NeighDel call, got %d", len(mock.dels))
	}
}

func TestHandleType3RouteNoVTEP(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision", EnableL2: true},
		log:        slog.Default(),
		netlinkOps: mock,
	}

	route := &apipb.EVPNInclusiveMulticastEthernetTagRoute{IpAddress: ""}
	overlay.handleType3Route(route, "", false)

	if len(mock.appends) != 0 {
		t.Error("type-3 route with no VTEP should be skipped")
	}
}

// --- Type-3 NLRI builder tests -----------------------------------------------

func TestBuildEVPNType3NLRI(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType3NLRI(rd, "10.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg, err := nlri.UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal NLRI: %v", err)
	}
	route, ok := msg.(*apipb.EVPNInclusiveMulticastEthernetTagRoute)
	if !ok {
		t.Fatalf("expected EVPNInclusiveMulticastEthernetTagRoute, got %T", msg)
	}
	if route.IpAddress != "10.0.0.1" {
		t.Errorf("IpAddress = %s, want 10.0.0.1", route.IpAddress)
	}
	if route.EthernetTag != 0 {
		t.Errorf("EthernetTag = %d, want 0", route.EthernetTag)
	}
}

func TestBuildType3PathAttrs(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType3NLRI(rd, "10.0.0.1")
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType3PathAttrs(nlri, "10.0.0.1", 65000, 4000, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect 4 attributes: origin, mp-reach, ext-communities, pmsi-tunnel.
	if len(pattrs) != 4 {
		t.Errorf("got %d path attrs, want 4", len(pattrs))
	}

	// Verify PMSI tunnel attribute is present and correct.
	pmsiFound := false
	for _, attr := range pattrs {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		if pmsi, ok := msg.(*apipb.PmsiTunnelAttribute); ok {
			pmsiFound = true
			if pmsi.Type != pmsiTunnelTypeIngressReplication {
				t.Errorf("PMSI tunnel type = %d, want %d", pmsi.Type, pmsiTunnelTypeIngressReplication)
			}
			if pmsi.Label != 4000 {
				t.Errorf("PMSI label = %d, want 4000", pmsi.Label)
			}
		}
	}
	if !pmsiFound {
		t.Error("PMSI tunnel attribute not found in path attrs")
	}
}

func TestBuildType3PathAttrs4ByteASN(t *testing.T) {
	rd, err := buildRouteDistinguisher(70000, 5000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType3NLRI(rd, "10.0.0.1")
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType3PathAttrs(nlri, "10.0.0.1", 70000, 5000, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pattrs) != 4 {
		t.Errorf("got %d path attrs, want 4", len(pattrs))
	}
}

// --- Type-2 NLRI builder tests -----------------------------------------------

func TestBuildEVPNType2NLRI(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType2NLRI(rd, "aa:bb:cc:dd:ee:ff", "10.100.0.20", 4000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg, err := nlri.UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal NLRI: %v", err)
	}
	route, ok := msg.(*apipb.EVPNMACIPAdvertisementRoute)
	if !ok {
		t.Fatalf("expected EVPNMACIPAdvertisementRoute, got %T", msg)
	}
	if route.MacAddress != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MacAddress = %s, want aa:bb:cc:dd:ee:ff", route.MacAddress)
	}
	if route.IpAddress != "10.100.0.20" {
		t.Errorf("IpAddress = %s, want 10.100.0.20", route.IpAddress)
	}
	if len(route.Labels) != 1 || route.Labels[0] != 4000 {
		t.Errorf("Labels = %v, want [4000]", route.Labels)
	}
	if route.EthernetTag != 0 {
		t.Errorf("EthernetTag = %d, want 0", route.EthernetTag)
	}
}

func TestBuildEVPNType2NLRINoIP(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType2NLRI(rd, "aa:bb:cc:dd:ee:ff", "", 4000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg, err := nlri.UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal NLRI: %v", err)
	}
	route, ok := msg.(*apipb.EVPNMACIPAdvertisementRoute)
	if !ok {
		t.Fatalf("expected EVPNMACIPAdvertisementRoute, got %T", msg)
	}
	if route.IpAddress != "" {
		t.Errorf("IpAddress = %s, want empty", route.IpAddress)
	}
}

func TestBuildType2PathAttrs(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 4000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType2NLRI(rd, "aa:bb:cc:dd:ee:ff", "10.100.0.20", 4000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType2PathAttrs(nlri, "10.0.0.1", 65000, 4000, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect 3 attributes: origin, mp-reach, ext-communities.
	if len(pattrs) != 3 {
		t.Errorf("got %d path attrs, want 3", len(pattrs))
	}

	// Verify MpReach next-hop is RouterID.
	for _, attr := range pattrs {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		if mp, ok := msg.(*apipb.MpReachNLRIAttribute); ok {
			if len(mp.NextHops) != 1 || mp.NextHops[0] != "10.0.0.1" {
				t.Errorf("NextHops = %v, want [10.0.0.1]", mp.NextHops)
			}
		}
	}
}

func TestBuildType2PathAttrs4ByteASN(t *testing.T) {
	rd, err := buildRouteDistinguisher(70000, 5000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}

	nlri, err := buildEVPNType2NLRI(rd, "aa:bb:cc:dd:ee:ff", "10.100.0.20", 5000)
	if err != nil {
		t.Fatalf("build NLRI: %v", err)
	}

	pattrs, err := buildType2PathAttrs(nlri, "10.0.0.1", 70000, 5000, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pattrs) != 3 {
		t.Errorf("got %d path attrs, want 3", len(pattrs))
	}
}

// --- advertiseType2 / advertiseType3 unit tests (skip verification) ----------

func TestAdvertiseType2SkipsWhenBridgeMACEmpty(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{BridgeMAC: "", ProvisionIP: "10.100.0.20/24"},
		log: slog.Default(),
	}
	if err := overlay.advertiseType2(context.TODO()); err != nil {
		t.Errorf("expected nil error when BridgeMAC empty, got: %v", err)
	}
}

func TestAdvertiseType2SkipsWhenProvisionIPEmpty(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{BridgeMAC: "aa:bb:cc:dd:ee:ff", ProvisionIP: ""},
		log: slog.Default(),
	}
	if err := overlay.advertiseType2(context.TODO()); err != nil {
		t.Errorf("expected nil error when ProvisionIP empty, got: %v", err)
	}
}

func TestAdvertiseType2InvalidBridgeMAC(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{BridgeMAC: "not-a-mac", ProvisionIP: "10.100.0.20/24"},
		log: slog.Default(),
	}
	err := overlay.advertiseType2(context.TODO())
	if err == nil {
		t.Fatal("expected error for invalid bridge MAC")
	}
}

func TestAdvertiseType2InvalidProvisionIP(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{BridgeMAC: "aa:bb:cc:dd:ee:ff", ProvisionIP: "not-a-cidr", ASN: 65000, ProvisionVNI: 100},
		log: slog.Default(),
	}
	err := overlay.advertiseType2(context.TODO())
	if err == nil {
		t.Fatal("expected error for invalid provision IP")
	}
}

func TestOverlaySetupRejectsUnimplementedTypes(t *testing.T) {
	tests := []struct {
		name        string
		overlayType string
		wantErr     string
	}{
		{"l3vpn not implemented", string(OverlayL3VPN), "not yet implemented"},
		{"unknown type", "invalid", "unknown overlay type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay := &OverlayTier{
				cfg: &Config{OverlayType: tt.overlayType},
				log: slog.Default(),
			}
			err := overlay.Setup(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, err) || err.Error() == "" {
				t.Fatal("error should not be empty")
			}
		})
	}
}

func TestOverlaySetupSkipsOverlayNone(t *testing.T) {
	overlay := &OverlayTier{
		cfg: &Config{OverlayType: string(OverlayNone)},
		log: slog.Default(),
	}
	if err := overlay.Setup(context.Background()); err != nil {
		t.Fatalf("overlay none should succeed with no-op: %v", err)
	}
}

func TestOverlaySetupFailureRunsTeardown(t *testing.T) {
	watchCtx, cancel := context.WithCancel(context.Background())
	setupErr := errors.New("forced setup failure")
	teardownCalled := false
	overlay := &OverlayTier{
		cfg:    &Config{OverlayType: string(OverlayEVPNVXLAN), ProvisionVNI: 4000, BridgeName: "br-test"},
		log:    slog.Default(),
		cancel: cancel,
		bgp:    server.NewBgpServer(),
		hooks: &overlaySetupHooks{
			createVXLANAndBridge: func() error { return nil },
			addProvisionIP:       func() error { return setupErr },
			teardown: func(context.Context) error {
				teardownCalled = true
				cancel()
				return nil
			},
		},
	}

	err := overlay.Setup(context.Background())
	if !errors.Is(err, setupErr) {
		t.Fatalf("Setup error = %v, want wrapped setup error", err)
	}
	if !teardownCalled {
		t.Fatal("Setup failure did not run teardown")
	}
	select {
	case <-watchCtx.Done():
	default:
		t.Fatal("cleanupSetupFailure did not cancel overlay watch context")
	}
}

func TestUnderlaySetupRejectsUnimplementedAF(t *testing.T) {
	tests := []struct {
		name string
		af   string
	}{
		{"ipv6", string(AFIPv6)},
		{"dual-stack", string(AFDualStack)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier := NewUnderlayTier(&Config{UnderlayAF: tt.af})
			err := tier.Setup(context.Background())
			if err == nil {
				t.Fatalf("expected error for underlay AF %q", tt.af)
			}
			if !strings.Contains(err.Error(), "not yet implemented") {
				t.Fatalf("expected 'not yet implemented' error, got: %v", err)
			}
		})
	}
}

func mustPathWithRT(t *testing.T, rts ...*anypb.Any) *apipb.Path {
	t.Helper()
	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: rts,
	})
	if err != nil {
		t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
	}
	return &apipb.Path{Pattrs: []*anypb.Any{extComm}}
}

func mustRouterMAC(t *testing.T, mac string) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&apipb.RouterMacExtended{Mac: mac})
	if err != nil {
		t.Fatalf("marshal RouterMacExtended: %v", err)
	}
	return a
}

func mustParseMAC(t *testing.T, mac string) net.HardwareAddr {
	t.Helper()
	hwAddr, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("parse MAC %s: %v", mac, err)
	}
	return hwAddr
}

func assertType5GatewayNeighbor(t *testing.T, neigh *netlink.Neigh, ip, mac string) {
	t.Helper()
	if neigh == nil {
		t.Fatal("neighbor is nil")
	}
	wantIP := net.ParseIP(ip).To4()
	if wantIP == nil {
		t.Fatalf("invalid IPv4 %s", ip)
	}
	if neigh.LinkIndex != 42 {
		t.Errorf("neighbor LinkIndex = %d, want 42", neigh.LinkIndex)
	}
	if neigh.Family != syscall.AF_INET {
		t.Errorf("neighbor Family = %d, want AF_INET", neigh.Family)
	}
	if neigh.State != netlink.NUD_PERMANENT {
		t.Errorf("neighbor State = %d, want NUD_PERMANENT", neigh.State)
	}
	if !neigh.IP.Equal(wantIP) {
		t.Errorf("neighbor IP = %s, want %s", neigh.IP, wantIP)
	}
	if neigh.HardwareAddr.String() != mac {
		t.Errorf("neighbor HardwareAddr = %s, want %s", neigh.HardwareAddr, mac)
	}
}

func assertType5GatewayFDB(t *testing.T, neigh *netlink.Neigh, vtep, mac string) {
	t.Helper()
	if neigh == nil {
		t.Fatal("FDB entry is nil")
	}
	wantIP := net.ParseIP(vtep).To4()
	if wantIP == nil {
		t.Fatalf("invalid IPv4 %s", vtep)
	}
	if neigh.LinkIndex != 43 {
		t.Errorf("FDB LinkIndex = %d, want 43", neigh.LinkIndex)
	}
	if neigh.Family != syscall.AF_BRIDGE {
		t.Errorf("FDB Family = %d, want AF_BRIDGE", neigh.Family)
	}
	if neigh.State != netlink.NUD_PERMANENT {
		t.Errorf("FDB State = %d, want NUD_PERMANENT", neigh.State)
	}
	if neigh.Flags != netlink.NTF_SELF {
		t.Errorf("FDB Flags = %d, want NTF_SELF", neigh.Flags)
	}
	if !neigh.IP.Equal(wantIP) {
		t.Errorf("FDB IP = %s, want %s", neigh.IP, wantIP)
	}
	if neigh.HardwareAddr.String() != mac {
		t.Errorf("FDB HardwareAddr = %s, want %s", neigh.HardwareAddr, mac)
	}
}

func assertType5GatewaySet(t *testing.T, sets []*netlink.Neigh, index int, gw, mac string) {
	t.Helper()
	if len(sets) <= index+1 {
		t.Fatalf("expected gateway neighbor and FDB set at indexes %d/%d, got %d sets", index, index+1, len(sets))
	}
	assertType5GatewayNeighbor(t, sets[index], gw, mac)
	assertType5GatewayFDB(t, sets[index+1], gw, mac)
}

func assertType5GatewayDelete(t *testing.T, dels []*netlink.Neigh, index int, gw, mac string) {
	t.Helper()
	if len(dels) <= index+1 {
		t.Fatalf("expected gateway neighbor and FDB delete at indexes %d/%d, got %d deletes", index, index+1, len(dels))
	}
	assertType5GatewayNeighbor(t, dels[index], gw, mac)
	assertType5GatewayFDB(t, dels[index+1], gw, mac)
}

func mustFindRouterMAC(t *testing.T, path *apipb.Path) string {
	t.Helper()
	for _, attr := range path.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		extComm, ok := msg.(*apipb.ExtendedCommunitiesAttribute)
		if !ok {
			continue
		}
		for _, community := range extComm.GetCommunities() {
			msg, err := community.UnmarshalNew()
			if err != nil {
				continue
			}
			if rmac, ok := msg.(*apipb.RouterMacExtended); ok {
				return rmac.GetMac()
			}
		}
	}
	t.Fatal("router MAC extended community not found")
	return ""
}

func mustFindType5Route(t *testing.T, path *apipb.Path) *apipb.EVPNIPPrefixRoute {
	t.Helper()
	nlri := path.GetNlri()
	if nlri == nil {
		t.Fatal("Type-5 NLRI not found")
	}
	msg, err := nlri.UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal Type-5 NLRI: %v", err)
	}
	route, ok := msg.(*apipb.EVPNIPPrefixRoute)
	if !ok {
		t.Fatalf("expected EVPNIPPrefixRoute, got %T", msg)
	}
	return route
}

func mustFindEncapTunnelType(t *testing.T, path *apipb.Path) uint32 {
	t.Helper()
	for _, attr := range path.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		extComm, ok := msg.(*apipb.ExtendedCommunitiesAttribute)
		if !ok {
			continue
		}
		for _, community := range extComm.GetCommunities() {
			msg, err := community.UnmarshalNew()
			if err != nil {
				continue
			}
			if encap, ok := msg.(*apipb.EncapExtended); ok {
				return encap.GetTunnelType()
			}
		}
	}
	t.Fatal("encapsulation extended community not found")
	return 0
}

func countExtendedCommunities(t *testing.T, path *apipb.Path) int {
	t.Helper()
	for _, attr := range path.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		extComm, ok := msg.(*apipb.ExtendedCommunitiesAttribute)
		if !ok {
			continue
		}
		return len(extComm.GetCommunities())
	}
	t.Fatal("extended communities attribute not found")
	return 0
}

func mustRT2(t *testing.T, asn, vni uint32) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&apipb.TwoOctetAsSpecificExtended{
		IsTransitive: true,
		SubType:      0x02,
		Asn:          asn,
		LocalAdmin:   vni,
	})
	if err != nil {
		t.Fatalf("marshal TwoOctetAsSpecificExtended: %v", err)
	}
	return a
}

func mustRT4(t *testing.T, asn, vni uint32) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&apipb.FourOctetAsSpecificExtended{
		IsTransitive: true,
		SubType:      0x02,
		Asn:          asn,
		LocalAdmin:   vni,
	})
	if err != nil {
		t.Fatalf("marshal FourOctetAsSpecificExtended: %v", err)
	}
	return a
}

func mustRTIPv4(t *testing.T, address string, localAdmin uint32) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&apipb.IPv4AddressSpecificExtended{
		IsTransitive: true,
		SubType:      0x02,
		Address:      address,
		LocalAdmin:   localAdmin,
	})
	if err != nil {
		t.Fatalf("marshal IPv4AddressSpecificExtended: %v", err)
	}
	return a
}

func mustRawRT(t *testing.T, typ uint32) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&apipb.UnknownExtended{
		Type:  typ,
		Value: []byte{0x02, 0, 0, 0, 0, 0, 0},
	})
	if err != nil {
		t.Fatalf("marshal UnknownExtended: %v", err)
	}
	return a
}

func TestMatchesLocalRT(t *testing.T) {
	tests := []struct {
		name     string
		path     *apipb.Path
		localASN uint32
		localVNI uint32
		want     bool
	}{
		{
			name:     "matching 2-byte RT returns true",
			path:     mustPathWithRT(t, mustRT2(t, 65000, 4000)),
			localASN: 65000,
			localVNI: 4000,
			want:     true,
		},
		{
			name:     "non-matching ASN returns false",
			path:     mustPathWithRT(t, mustRT2(t, 65001, 4000)),
			localASN: 65000,
			localVNI: 4000,
			want:     false,
		},
		{
			name:     "non-matching VNI returns false",
			path:     mustPathWithRT(t, mustRT2(t, 65000, 9999)),
			localASN: 65000,
			localVNI: 4000,
			want:     false,
		},
		{
			name:     "path with no extended communities returns false",
			path:     &apipb.Path{},
			localASN: 65000,
			localVNI: 4000,
			want:     false,
		},
		{
			name:     "matching 4-byte RT returns true",
			path:     mustPathWithRT(t, mustRT4(t, 70000, 5000)),
			localASN: 70000,
			localVNI: 5000,
			want:     true,
		},
		{
			name:     "4-byte RT does not mask oversized VNI",
			path:     mustPathWithRT(t, mustRT4(t, 70000, 1)),
			localASN: 70000,
			localVNI: 65537,
			want:     false,
		},
		{
			name:     "multiple communities one matching returns true",
			path:     mustPathWithRT(t, mustRT2(t, 65001, 4000), mustRT2(t, 65000, 4000)),
			localASN: 65000,
			localVNI: 4000,
			want:     true,
		},
		{
			name:     "multiple communities none matching returns false",
			path:     mustPathWithRT(t, mustRT2(t, 65001, 4000), mustRT2(t, 65002, 4000)),
			localASN: 65000,
			localVNI: 4000,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesLocalRT(tt.path, tt.localASN, tt.localVNI)
			if got != tt.want {
				t.Errorf("matchesLocalRT() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRouteTargetMatchStateDetectsForeignRouteTargets(t *testing.T) {
	tests := []struct {
		name string
		path *apipb.Path
	}{
		{
			name: "ipv4 address-specific RT",
			path: mustPathWithRT(t, mustRTIPv4(t, "192.0.2.10", 4000)),
		},
		{
			name: "raw unknown RT",
			path: mustPathWithRT(t, mustRawRT(t, 0x01)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches, hasRT := routeTargetMatchState(tt.path, 65000, 4000)
			if matches {
				t.Fatal("foreign route target matched local RT")
			}
			if !hasRT {
				t.Fatal("foreign route target was treated as absent")
			}
		})
	}
}

func TestConfigImportRouteTarget(t *testing.T) {
	path := mustPathWithRT(t, mustRT2(t, 65000, 1000))

	cfg := &Config{ASN: 65100, ProvisionVNI: 1000, VPNRT: "65000:1000"}
	asn, vni, err := cfg.importRouteTarget()
	if err != nil {
		t.Fatalf("importRouteTarget(): %v", err)
	}
	if !matchesLocalRT(path, asn, vni) {
		t.Fatal("expected configured VPNRT to match imported route")
	}

	cfg.VPNRT = "bad"
	cachedASN, cachedVNI, err := cfg.importRouteTarget()
	if err != nil {
		t.Fatalf("cached importRouteTarget(): %v", err)
	}
	if cachedASN != asn || cachedVNI != vni {
		t.Fatalf("cached import RT = %d:%d, want %d:%d", cachedASN, cachedVNI, asn, vni)
	}

	fallback := &Config{ASN: 65100, ProvisionVNI: 1000}
	fallbackASN, fallbackVNI, err := fallback.importRouteTarget()
	if err != nil {
		t.Fatalf("fallback importRouteTarget(): %v", err)
	}
	if matchesLocalRT(path, fallbackASN, fallbackVNI) {
		t.Fatal("expected local fallback RT to reject route from different RT")
	}

	invalid := &Config{ASN: 65100, ProvisionVNI: 1000, VPNRT: "bad"}
	if _, _, err := invalid.importRouteTarget(); err == nil {
		t.Fatal("expected invalid configured VPNRT to reject route")
	}
}

func TestProcessRouteUpdateRTFilter(t *testing.T) {
	nlri, err := anypb.New(&apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("marshal NLRI: %v", err)
	}

	mp, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{"10.0.0.1"},
	})
	if err != nil {
		t.Fatalf("marshal MpReachNLRI: %v", err)
	}

	newOverlay := func(mock *mockOverlayNetlinkOps) *OverlayTier {
		return &OverlayTier{
			cfg: &Config{
				RouterID:     "10.0.0.99",
				ASN:          65000,
				ProvisionVNI: 4000,
				BridgeName:   "br-test",
			},
			log:        slog.Default(),
			netlinkOps: mock,
		}
	}

	t.Run("route with matching RT is dispatched to FDB", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{mustRT2(t, 65000, 4000)},
		})
		if err != nil {
			t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
		}
		path := &apipb.Path{
			Nlri:   nlri,
			Pattrs: []*anypb.Any{mp, extComm},
		}
		overlay.processRouteUpdate(path)
		// Matching RT must reach FDB dispatch (LinkByName is called for Type-5 installs).
		if mock.linkName == "" {
			t.Error("matching RT: expected FDB dispatch (LinkByName called), but it was not")
		}
	})

	t.Run("route with configured VPNRT is dispatched to FDB", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		overlay.cfg.ASN = 65100
		overlay.cfg.ProvisionVNI = 1000
		overlay.cfg.VPNRT = "65000:1000"
		extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{mustRT2(t, 65000, 1000)},
		})
		if err != nil {
			t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
		}
		path := &apipb.Path{
			Nlri:   nlri,
			Pattrs: []*anypb.Any{mp, extComm},
		}
		overlay.processRouteUpdate(path)
		if mock.linkName == "" {
			t.Error("configured VPNRT: expected FDB dispatch (LinkByName called), but it was not")
		}
	})

	t.Run("configured VPNRT is cached for route updates", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		overlay.cfg.ASN = 65100
		overlay.cfg.ProvisionVNI = 1000
		overlay.cfg.VPNRT = "65000:1000"
		extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{mustRT2(t, 65000, 1000)},
		})
		if err != nil {
			t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
		}
		path := &apipb.Path{
			Nlri:   nlri,
			Pattrs: []*anypb.Any{mp, extComm},
		}

		overlay.processRouteUpdate(path)
		if mock.linkName == "" {
			t.Fatal("configured VPNRT: expected first update to dispatch")
		}

		mock.linkName = ""
		overlay.cfg.VPNRT = "bad"
		overlay.processRouteUpdate(path)
		if mock.linkName == "" {
			t.Error("configured VPNRT cache: expected cached RT to dispatch second update")
		}
	})

	t.Run("route with non-matching RT is skipped", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{mustRT2(t, 65001, 1111)},
		})
		if err != nil {
			t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
		}
		path := &apipb.Path{
			Nlri:   nlri,
			Pattrs: []*anypb.Any{extComm},
		}
		overlay.processRouteUpdate(path)
		// Non-matching RT must not reach FDB dispatch.
		if mock.linkName != "" {
			t.Errorf("non-matching RT: unexpected FDB dispatch via LinkByName(%q)", mock.linkName)
		}
	})

	t.Run("withdrawal with non-matching RT is skipped", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
			Communities: []*anypb.Any{mustRT2(t, 65001, 1111)},
		})
		if err != nil {
			t.Fatalf("marshal ExtendedCommunitiesAttribute: %v", err)
		}
		// RT-bearing withdrawals from foreign overlays must not remove local FDB state.
		path := &apipb.Path{
			IsWithdraw: true,
			Nlri:       nlri,
			Pattrs:     []*anypb.Any{extComm},
		}
		overlay.processRouteUpdate(path)
		if mock.linkName != "" {
			t.Errorf("withdrawal with non-matching RT: unexpected FDB dispatch via LinkByName(%q)", mock.linkName)
		}
	})

	t.Run("withdrawal without RT is not skipped", func(t *testing.T) {
		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		path := &apipb.Path{
			IsWithdraw: true,
			Nlri:       nlri,
		}
		overlay.processRouteUpdate(path)
		if mock.linkName == "" {
			t.Error("withdrawal without RT: expected FDB dispatch (LinkByName called), but it was not")
		}
	})
}

func TestHandleType5RouteKeepsRouteWhileAnotherGatewayAdvertisesIt(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeVia := func(rdAdmin uint32, gw string) *apipb.EVPNIPPrefixRoute {
		t.Helper()
		rd, err := anypb.New(&apipb.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: rdAdmin})
		if err != nil {
			t.Fatalf("anypb.New: %v", err)
		}
		return &apipb.EVPNIPPrefixRoute{Rd: rd, IpPrefix: "0.0.0.0", IpPrefixLen: 0, GwAddress: gw}
	}
	routeA, routeB := routeVia(1, "192.168.4.1"), routeVia(2, "192.168.4.2")
	macA, macB := mustParseMAC(t, "62:db:b8:c1:80:01"), mustParseMAC(t, "62:db:b8:c1:80:02")

	overlay.handleType5Route(routeA, "192.168.4.1", macA, false)
	overlay.handleType5Route(routeB, "192.168.4.2", macB, false)
	overlay.handleType5Route(routeB, "192.168.4.2", nil, true)

	if len(mock.routeDels) != 0 {
		t.Fatalf("route deleted while gateway A still advertises it: %v", mock.routeDels)
	}
	last := mock.routeReplaces[len(mock.routeReplaces)-1]
	if !last.Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Fatalf("route gw after B withdraw = %s, want 192.168.4.1", last.Gw)
	}
	assertType5GatewayNeighbor(t, mock.sets[len(mock.sets)-2], "192.168.4.1", "62:db:b8:c1:80:01")

	overlay.handleType5Route(routeA, "192.168.4.1", nil, true)

	if len(mock.routeDels) != 1 || !mock.routeDels[0].Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Fatalf("expected route delete via 192.168.4.1 after last withdraw, got %v", mock.routeDels)
	}
}

func TestHandleType5RouteDeletesWithdrawnRouteWhenFallbackFails(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeVia := func(rdAdmin uint32, gw string) *apipb.EVPNIPPrefixRoute {
		t.Helper()
		rd, err := anypb.New(&apipb.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: rdAdmin})
		if err != nil {
			t.Fatalf("anypb.New: %v", err)
		}
		return &apipb.EVPNIPPrefixRoute{Rd: rd, IpPrefix: "0.0.0.0", IpPrefixLen: 0, GwAddress: gw}
	}
	routeA, routeB := routeVia(1, "192.168.4.1"), routeVia(2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	mock.routeReplaceErr = errors.New("replace failed")
	overlay.handleType5Route(routeB, "192.168.4.2", nil, true)

	if len(mock.routeDels) != 1 || mock.routeDels[0].Dst.String() != "0.0.0.0/0" {
		t.Fatalf("expected the 0/0 route to be deleted after the failed fallback, got %v", mock.routeDels)
	}
}

func TestHandleType5RouteIgnoredAfterTeardown(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	if err := overlay.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	route := &apipb.EVPNIPPrefixRoute{IpPrefix: "10.100.0.0", IpPrefixLen: 24, GwAddress: "192.168.4.1"}
	overlay.handleType5Route(route, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:52"), false)

	if len(mock.routeReplaces) != 0 || len(mock.sets) != 0 {
		t.Fatalf("route callback changed the kernel after teardown: routes=%d neighbors=%d", len(mock.routeReplaces), len(mock.sets))
	}
}

func TestHandleType5RouteFallsBackToPathWhoseInstallFailed(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeVia := func(rdAdmin uint32, gw string) *apipb.EVPNIPPrefixRoute {
		t.Helper()
		rd, err := anypb.New(&apipb.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: rdAdmin})
		if err != nil {
			t.Fatalf("anypb.New: %v", err)
		}
		return &apipb.EVPNIPPrefixRoute{Rd: rd, IpPrefix: "0.0.0.0", IpPrefixLen: 0, GwAddress: gw}
	}
	routeA, routeB := routeVia(1, "192.168.4.1"), routeVia(2, "192.168.4.2")

	mock.routeReplaceErr = errors.New("transient")
	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	mock.routeReplaceErr = nil
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", nil, true)

	if len(mock.routeDels) != 0 {
		t.Fatalf("route deleted although gateway A still advertises it: %v", mock.routeDels)
	}
	if last := mock.routeReplaces[len(mock.routeReplaces)-1]; !last.Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Fatalf("route gw after B withdraw = %s, want 192.168.4.1", last.Gw)
	}
}

func TestHandleType5RouteTriesEveryRemainingPath(t *testing.T) {
	mock := &failingGatewayRouteOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failGw: "192.168.4.1"}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	routeVia := func(rdAdmin uint32, gw string) *apipb.EVPNIPPrefixRoute {
		t.Helper()
		rd, err := anypb.New(&apipb.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: rdAdmin})
		if err != nil {
			t.Fatalf("anypb.New: %v", err)
		}
		return &apipb.EVPNIPPrefixRoute{Rd: rd, IpPrefix: "0.0.0.0", IpPrefixLen: 0, GwAddress: gw}
	}
	routeA, routeB, routeC := routeVia(1, "192.168.4.1"), routeVia(2, "192.168.4.2"), routeVia(3, "192.168.4.3")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeC, "192.168.4.3", mustParseMAC(t, "62:db:b8:c1:80:03"), false)
	overlay.handleType5Route(routeC, "192.168.4.3", nil, true)

	if len(mock.routeDels) != 0 {
		t.Fatalf("route deleted although gateway B is installable: %v", mock.routeDels)
	}
	if last := mock.routeReplaces[len(mock.routeReplaces)-1]; !last.Gw.Equal(net.ParseIP("192.168.4.2")) {
		t.Fatalf("route gw after C withdraw = %s, want 192.168.4.2", last.Gw)
	}
}

// failingGatewayRouteOps fails RouteReplace for one gateway.
type failingGatewayRouteOps struct {
	*mockOverlayNetlinkOps
	failGw string
}

func (m *failingGatewayRouteOps) RouteReplace(route *netlink.Route) error {
	err := m.mockOverlayNetlinkOps.RouteReplace(route)
	if route.Gw.Equal(net.ParseIP(m.failGw)) {
		return errors.New("replace failed")
	}
	return err
}

func TestHandleType5RouteRetriesVTEPRouteForFallbackPath(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := newType5FailoverTestOverlay(mock)
	vtepFails := true
	overlay.ensureVTEPRoute = func(vtep net.IP) error {
		if vtep.Equal(net.ParseIP("192.168.4.1")) && vtepFails {
			return errors.New("transient underlay error")
		}
		return nil
	}
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	vtepFails = false
	overlay.handleType5Route(routeB, "192.168.4.2", nil, true)

	if len(mock.routeDels) != 0 {
		t.Fatalf("route deleted although gateway A still advertises it: %v", mock.routeDels)
	}
	if last := mock.routeReplaces[len(mock.routeReplaces)-1]; !last.Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Fatalf("route gw after B withdraw = %s, want 192.168.4.1", last.Gw)
	}
}

func TestHandleType5RouteSkipsFallbackWithoutGatewayState(t *testing.T) {
	mock := &failingNeighborOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failIP: "192.168.4.1"}
	overlay := newType5FailoverTestOverlay(mock)
	routeA := type5FailoverRoute(t, 1, "192.168.4.1")
	routeB := type5FailoverRoute(t, 2, "192.168.4.2")
	routeC := type5FailoverRoute(t, 3, "192.168.4.3")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeC, "192.168.4.3", mustParseMAC(t, "62:db:b8:c1:80:03"), false)
	overlay.handleType5Route(routeC, "192.168.4.3", nil, true)

	if last := mock.routeReplaces[len(mock.routeReplaces)-1]; !last.Gw.Equal(net.ParseIP("192.168.4.2")) {
		t.Fatalf("route gw after C withdraw = %s, want 192.168.4.2", last.Gw)
	}
}

func newType5FailoverTestOverlay(ops overlayNetlinkOps) *OverlayTier {
	return &OverlayTier{
		cfg: &Config{
			RouterID:          "192.168.4.10",
			ProvisionVNI:      1000,
			BridgeName:        "br.provision",
			OverlayVRFName:    "Vrf_overlay",
			OverlayVRFTableID: 20,
		},
		log:        slog.Default(),
		netlinkOps: ops,
	}
}

func type5FailoverRoute(t *testing.T, rdAssigned uint32, gw string) *apipb.EVPNIPPrefixRoute {
	t.Helper()
	rd, err := anypb.New(&apipb.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: rdAssigned})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	return &apipb.EVPNIPPrefixRoute{Rd: rd, IpPrefix: "0.0.0.0", IpPrefixLen: 0, GwAddress: gw}
}

// failingNeighborOps fails NeighSet for one gateway neighbor.
type failingNeighborOps struct {
	*mockOverlayNetlinkOps
	failIP string
}

func (m *failingNeighborOps) NeighSet(n *netlink.Neigh) error {
	err := m.mockOverlayNetlinkOps.NeighSet(n)
	if n.IP.Equal(net.ParseIP(m.failIP)) && n.Family == syscall.AF_INET {
		return errors.New("neighbor set failed")
	}
	return err
}

func TestHandleType5RouteRemovesPrefixWhenEveryFallbackFails(t *testing.T) {
	mock := &failingNeighborOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failIP: "192.168.4.1"}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", nil, true)

	if len(mock.routeDels) != 1 || mock.routeDels[0].Gw != nil || mock.routeDels[0].Dst.String() != "0.0.0.0/0" {
		t.Fatalf("expected the 0/0 route to be deleted whatever its gateway, got %v", mock.routeDels)
	}
	if overlay.hasType5GatewayRef("192.168.4.2") {
		t.Fatal("gateway state of the withdrawn path was not cleared")
	}
}

func TestHandleType5RouteInactiveWithdrawKeepsActiveRoute(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	replaces := len(mock.routeReplaces)
	mock.routeReplaceErr = errors.New("transient")
	overlay.handleType5Route(routeA, "192.168.4.1", nil, true)

	if len(mock.routeDels) != 0 || len(mock.routeReplaces) != replaces {
		t.Fatalf("inactive withdraw touched the active route: dels=%v replaces=%d", mock.routeDels, len(mock.routeReplaces)-replaces)
	}
}

func TestHandleType5RouteKeepsGatewayStateWhenPrefixDeleteFails(t *testing.T) {
	mock := &failingNeighborOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failIP: "192.168.4.1"}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	mock.routeDelErr = errors.New("delete failed")
	overlay.handleType5Route(routeB, "192.168.4.2", nil, true)

	if !overlay.hasType5GatewayRef("192.168.4.2") {
		t.Fatal("gateway state was cleared although the route delete failed")
	}
}

func TestHandleType5RouteRestoresActivePathWhenNewPathFails(t *testing.T) {
	mock := &failingNeighborOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failIP: "192.168.4.1"}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)

	if last := mock.routeReplaces[len(mock.routeReplaces)-1]; !last.Gw.Equal(net.ParseIP("192.168.4.2")) {
		t.Fatalf("route gw after failed A install = %s, want 192.168.4.2 restored", last.Gw)
	}

	overlay.handleType5Route(routeA, "192.168.4.1", nil, true)
	if len(mock.routeDels) != 0 {
		t.Fatalf("withdrawing the failed path A deleted the working route: %v", mock.routeDels)
	}
}

func TestHandleType5RouteFallsBackWhenSameNLRIUpdateFails(t *testing.T) {
	mock := &failingNeighborOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failIP: "192.168.4.9"}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")
	routeBMoved := type5FailoverRoute(t, 2, "192.168.4.9")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeBMoved, "192.168.4.9", mustParseMAC(t, "62:db:b8:c1:80:09"), false)

	if last := mock.routeReplaces[len(mock.routeReplaces)-1]; !last.Gw.Equal(net.ParseIP("192.168.4.1")) {
		t.Fatalf("route gw after failed same-NLRI update = %s, want fallback 192.168.4.1", last.Gw)
	}
}

func TestHandleType5RouteWithdrawAfterFailedUpdateRemovesPrefix(t *testing.T) {
	mock := &failingGatewayRouteOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failGw: "192.168.4.9"}
	overlay := newType5FailoverTestOverlay(mock)
	routeB, routeBMoved := type5FailoverRoute(t, 2, "192.168.4.2"), type5FailoverRoute(t, 2, "192.168.4.9")

	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	overlay.handleType5Route(routeBMoved, "192.168.4.9", mustParseMAC(t, "62:db:b8:c1:80:09"), false)
	overlay.handleType5Route(routeBMoved, "192.168.4.9", nil, true)

	if len(mock.routeDels) != 1 || mock.routeDels[0].Gw != nil || mock.routeDels[0].Dst.String() != "0.0.0.0/0" {
		t.Fatalf("expected the 0/0 route to be deleted whatever its gateway, got %v", mock.routeDels)
	}
}

func TestHandleType5RouteRemovesPrefixWhenNoPathInstalls(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	mock.setErr = errors.New("neighbor set failed")
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)

	if len(mock.routeDels) != 1 || mock.routeDels[0].Gw != nil {
		t.Fatalf("expected the prefix route to be removed when no path installs, got %v", mock.routeDels)
	}
}

// blockingRouteOps blocks the first RouteReplace until release is closed.
type blockingRouteOps struct {
	*mockOverlayNetlinkOps
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRouteOps) RouteReplace(route *netlink.Route) error {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.mockOverlayNetlinkOps.RouteReplace(route)
}

func TestOverlayTeardownWaitsForInFlightType5Update(t *testing.T) {
	ops := &blockingRouteOps{
		mockOverlayNetlinkOps: &mockOverlayNetlinkOps{},
		entered:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	overlay := newType5FailoverTestOverlay(ops)
	var vtepRoutes atomic.Int32
	overlay.ensureVTEPRoute = func(net.IP) error {
		vtepRoutes.Add(1)
		return nil
	}
	route := type5FailoverRoute(t, 1, "192.168.4.1")

	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		overlay.handleType5Route(route, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	}()
	<-ops.entered

	teardownDone := make(chan struct{})
	go func() {
		defer close(teardownDone)
		_ = overlay.Teardown(context.Background())
	}()
	select {
	case <-teardownDone:
		t.Fatal("Teardown returned while a Type-5 update was still changing the kernel")
	case <-time.After(100 * time.Millisecond):
	}

	close(ops.release)
	<-updateDone
	select {
	case <-teardownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Teardown did not finish after the update returned")
	}

	before := vtepRoutes.Load()
	overlay.handleType5Route(type5FailoverRoute(t, 2, "192.168.4.2"), "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)
	if vtepRoutes.Load() != before {
		t.Fatal("an update after Teardown added a VTEP route")
	}
}

func TestHandleType5RouteKeepsActiveRouteWhenNewRouteReplaceFails(t *testing.T) {
	mock := &failingGatewayRouteOps{mockOverlayNetlinkOps: &mockOverlayNetlinkOps{}, failGw: "192.168.4.2"}
	overlay := newType5FailoverTestOverlay(mock)
	routeA, routeB := type5FailoverRoute(t, 1, "192.168.4.1"), type5FailoverRoute(t, 2, "192.168.4.2")

	overlay.handleType5Route(routeA, "192.168.4.1", mustParseMAC(t, "62:db:b8:c1:80:01"), false)
	overlay.handleType5Route(routeB, "192.168.4.2", mustParseMAC(t, "62:db:b8:c1:80:02"), false)

	if len(mock.routeDels) != 0 {
		t.Fatalf("working route via A was deleted after B failed to install: %v", mock.routeDels)
	}
	if got := overlay.type5Active["0.0.0.0/0"]; got != type5PathKey(routeA) {
		t.Fatalf("active path = %q, want A", got)
	}
}

// startLocalTestBGP starts an in-process GoBGP server with no peers and no
// listener, so locally originated paths can be read back with ListPath.
func startLocalTestBGP(t *testing.T, asn uint32, routerID string) *server.BgpServer {
	t.Helper()
	bgp := server.NewBgpServer()
	go bgp.Serve()
	t.Cleanup(bgp.Stop)
	if err := bgp.StartBgp(context.Background(), &apipb.StartBgpRequest{
		Global: &apipb.Global{Asn: asn, RouterId: routerID, ListenPort: -1},
	}); err != nil {
		t.Fatalf("start BGP: %v", err)
	}
	return bgp
}

// listLocalEVPNPaths returns the EVPN paths in the global RIB keyed by NLRI type.
func listLocalEVPNPaths(t *testing.T, bgp *server.BgpServer) (type2, type3, type5 []*apipb.Path) {
	t.Helper()
	err := bgp.ListPath(context.Background(), &apipb.ListPathRequest{
		TableType: apipb.TableType_GLOBAL,
		Family:    &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
	}, func(dst *apipb.Destination) {
		for _, p := range dst.GetPaths() {
			msg, err := p.GetNlri().UnmarshalNew()
			if err != nil {
				t.Errorf("unmarshal NLRI: %v", err)
				continue
			}
			switch msg.(type) {
			case *apipb.EVPNMACIPAdvertisementRoute:
				type2 = append(type2, p)
			case *apipb.EVPNInclusiveMulticastEthernetTagRoute:
				type3 = append(type3, p)
			case *apipb.EVPNIPPrefixRoute:
				type5 = append(type5, p)
			default:
				t.Errorf("unexpected EVPN NLRI %T", msg)
			}
		}
	})
	if err != nil {
		t.Fatalf("list paths: %v", err)
	}
	return type2, type3, type5
}

func mustFindPMSITunnel(t *testing.T, path *apipb.Path) *apipb.PmsiTunnelAttribute {
	t.Helper()
	for _, attr := range path.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			t.Fatalf("unmarshal path attribute: %v", err)
		}
		if pmsi, ok := msg.(*apipb.PmsiTunnelAttribute); ok {
			return pmsi
		}
	}
	t.Fatal("path has no PMSI tunnel attribute")
	return nil
}

func l2AdvertiseTestConfig(enableL2 bool) *Config {
	return &Config{
		OverlayType:  string(OverlayEVPNVXLAN),
		ASN:          65100,
		RouterID:     "192.168.4.10",
		ProvisionVNI: 1000,
		ProvisionIP:  "10.200.0.10/24",
		BridgeMAC:    "62:db:b8:c1:80:52",
		VPNRT:        "65000:1000",
		EnableL2:     enableL2,
	}
}

func TestAdvertiseType3PublishesIMETWithIngressReplication(t *testing.T) {
	bgp := startLocalTestBGP(t, 65100, "192.168.4.10")
	overlay := &OverlayTier{cfg: l2AdvertiseTestConfig(true), log: slog.Default(), bgp: bgp}

	if err := overlay.advertiseType3(context.Background()); err != nil {
		t.Fatalf("advertise Type-3: %v", err)
	}

	type2, type3, type5 := listLocalEVPNPaths(t, bgp)
	if len(type3) != 1 || len(type2) != 0 || len(type5) != 0 {
		t.Fatalf("paths: type-2=%d type-3=%d type-5=%d, want only one type-3", len(type2), len(type3), len(type5))
	}
	path := type3[0]
	msg, err := path.GetNlri().UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal NLRI: %v", err)
	}
	route := msg.(*apipb.EVPNInclusiveMulticastEthernetTagRoute)
	if route.GetIpAddress() != "192.168.4.10" || route.GetEthernetTag() != 0 {
		t.Fatalf("IMET route = ip %s tag %d, want ip 192.168.4.10 tag 0", route.GetIpAddress(), route.GetEthernetTag())
	}
	if got := extractNextHop(path); got != "192.168.4.10" {
		t.Fatalf("type-3 next-hop = %s, want 192.168.4.10", got)
	}

	pmsi := mustFindPMSITunnel(t, path)
	if pmsi.GetType() != pmsiTunnelTypeIngressReplication {
		t.Fatalf("PMSI tunnel type = %d, want %d (ingress replication)", pmsi.GetType(), pmsiTunnelTypeIngressReplication)
	}
	if pmsi.GetLabel() != 1000 {
		t.Fatalf("PMSI label = %d, want VNI 1000", pmsi.GetLabel())
	}
	if got := net.IP(pmsi.GetId()); !got.Equal(net.ParseIP("192.168.4.10")) {
		t.Fatalf("PMSI tunnel ID = %v, want router ID 192.168.4.10", got)
	}

	if !matchesLocalRT(path, 65000, 1000) {
		t.Fatal("type-3 path does not carry configured VPNRT 65000:1000")
	}
	if matchesLocalRT(path, 65100, 1000) {
		t.Fatal("type-3 path carries the fallback local RT instead of the configured VPNRT")
	}
}

func TestAdvertiseType2PublishesBridgeMACAndIP(t *testing.T) {
	bgp := startLocalTestBGP(t, 65100, "192.168.4.10")
	overlay := &OverlayTier{cfg: l2AdvertiseTestConfig(true), log: slog.Default(), bgp: bgp}

	if err := overlay.advertiseType2(context.Background()); err != nil {
		t.Fatalf("advertise Type-2: %v", err)
	}

	type2, type3, type5 := listLocalEVPNPaths(t, bgp)
	if len(type2) != 1 || len(type3) != 0 || len(type5) != 0 {
		t.Fatalf("paths: type-2=%d type-3=%d type-5=%d, want only one type-2", len(type2), len(type3), len(type5))
	}
	path := type2[0]
	msg, err := path.GetNlri().UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshal NLRI: %v", err)
	}
	route := msg.(*apipb.EVPNMACIPAdvertisementRoute)
	if route.GetMacAddress() != "62:db:b8:c1:80:52" {
		t.Fatalf("type-2 MAC = %s, want bridge MAC 62:db:b8:c1:80:52", route.GetMacAddress())
	}
	if route.GetIpAddress() != "10.200.0.10" {
		t.Fatalf("type-2 IP = %s, want provision host IP 10.200.0.10", route.GetIpAddress())
	}
	if labels := route.GetLabels(); len(labels) != 1 || labels[0] != 1000 {
		t.Fatalf("type-2 labels = %v, want [1000]", labels)
	}
	if got := extractNextHop(path); got != "192.168.4.10" {
		t.Fatalf("type-2 next-hop = %s, want 192.168.4.10", got)
	}
	if !matchesLocalRT(path, 65000, 1000) {
		t.Fatal("type-2 path does not carry configured VPNRT 65000:1000")
	}
	if matchesLocalRT(path, 65100, 1000) {
		t.Fatal("type-2 path carries the fallback local RT instead of the configured VPNRT")
	}
}

func TestOverlaySetupAdvertisesType2And3OnlyWithL2(t *testing.T) {
	for _, tt := range []struct {
		name      string
		enableL2  bool
		wantType2 int
		wantType3 int
	}{
		{"type-5 only", false, 0, 0},
		{"l2 enabled", true, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bgp := startLocalTestBGP(t, 65100, "192.168.4.10")
			overlay := &OverlayTier{
				cfg: l2AdvertiseTestConfig(tt.enableL2),
				log: slog.Default(),
				bgp: bgp,
				// The route watcher sees our own paths; fail link lookups so
				// it never touches the host.
				netlinkOps: &mockOverlayNetlinkOps{linkErr: errors.New("no links in unit test")},
				hooks: &overlaySetupHooks{
					createVXLANAndBridge: func() error { return nil },
					addProvisionIP:       func() error { return nil },
					// A failed Setup must not run the real host teardown.
					teardown: func(context.Context) error { return nil },
				},
			}
			if err := overlay.Setup(context.Background()); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			t.Cleanup(overlay.cancel)

			type2, type3, type5 := listLocalEVPNPaths(t, bgp)
			if len(type5) != 1 {
				t.Fatalf("type-5 paths = %d, want 1", len(type5))
			}
			if len(type2) != tt.wantType2 || len(type3) != tt.wantType3 {
				t.Fatalf("EnableL2=%v: type-2=%d type-3=%d, want type-2=%d type-3=%d",
					tt.enableL2, len(type2), len(type3), tt.wantType2, tt.wantType3)
			}
		})
	}
}
