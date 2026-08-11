//go:build linux

package gobgp

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

// mockOverlayNetlinkOps records overlay netlink operations for assertion in unit tests.
type mockOverlayNetlinkOps struct {
	linkName          string
	linkErr           error
	linkErrName       string
	sets              []*netlink.Neigh
	appends           []*netlink.Neigh
	dels              []*netlink.Neigh
	routeReplaces     []*netlink.Route
	routeDels         []*netlink.Route
	setErr            error
	setErrOnCall      int
	appendErr         error
	delErr            error
	delErrOnCall      int
	routeReplaceErr   error
	routeDelErr       error
	routeDelErrOnCall int
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
	if m.routeDelErr != nil && (m.routeDelErrOnCall == 0 || m.routeDelErrOnCall == len(m.routeDels)) {
		return m.routeDelErr
	}
	return nil
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

	t.Run("withdraw without next-hop uses stored gateway ref", func(t *testing.T) {
		directNLRI, err := anypb.New(&apipb.EVPNIPPrefixRoute{
			IpPrefix:    "10.100.0.0",
			IpPrefixLen: 24,
			GwAddress:   type5DirectGateway,
		})
		if err != nil {
			t.Fatalf("marshal direct type-5 NLRI: %v", err)
		}

		mock := &mockOverlayNetlinkOps{}
		overlay := newOverlay(mock)
		installPath := &apipb.Path{Nlri: directNLRI, Pattrs: []*anypb.Any{mp, extComm}}
		unreach, err := anypb.New(&apipb.MpUnreachNLRIAttribute{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
			Nlris:  []*anypb.Any{directNLRI},
		})
		if err != nil {
			t.Fatalf("marshal MP_UNREACH: %v", err)
		}
		// MP_UNREACH withdrawals do not carry MP_REACH next-hop information.
		withdrawPath := &apipb.Path{
			Pattrs: []*anypb.Any{unreach},
		}

		overlay.processRouteUpdate(installPath)
		overlay.processRouteUpdate(withdrawPath)

		if len(mock.routeDels) != 1 {
			t.Fatalf("expected 1 route delete, got %d", len(mock.routeDels))
		}
		if got := mock.routeDels[0].Gw.String(); got != "192.168.4.1" {
			t.Fatalf("withdraw route delete gateway = %s, want 192.168.4.1", got)
		}
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

func TestProcessRouteUpdateType5SetupFailureThenMPUnreachWithdrawal(t *testing.T) {
	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "10.100.0.42",
		IpPrefixLen: 32,
		GwAddress:   type5DirectGateway,
	}
	nlri, err := anypb.New(route)
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}
	mpReach, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{"192.168.4.1"},
	})
	if err != nil {
		t.Fatalf("marshal MP_REACH: %v", err)
	}
	communities, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{
			mustRT2(t, 65000, 4000),
			mustRouterMAC(t, "62:db:b8:c1:80:52"),
		},
	})
	if err != nil {
		t.Fatalf("marshal communities: %v", err)
	}
	unreach, err := anypb.New(&apipb.MpUnreachNLRIAttribute{
		Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		Nlris:  []*anypb.Any{nlri},
	})
	if err != nil {
		t.Fatalf("marshal MP_UNREACH: %v", err)
	}

	mock := &mockOverlayNetlinkOps{
		setErr:            errors.New("neighbor setup failed"),
		routeDelErr:       errors.New("route rollback failed"),
		routeDelErrOnCall: 1,
	}
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

	overlay.processRouteUpdate(&apipb.Path{Nlri: nlri, Pattrs: []*anypb.Any{mpReach, communities}})

	key := type5RouteRefKey(route, nil)
	if _, ok := overlay.loadType5GatewayRef(key); !ok {
		t.Fatal("failed route rollback should retain Type-5 cleanup state")
	}
	if len(mock.routeDels) != 1 {
		t.Fatalf("route rollback attempts = %d, want 1", len(mock.routeDels))
	}

	overlay.processRouteUpdate(&apipb.Path{Pattrs: []*anypb.Any{unreach}})

	if len(mock.routeDels) != 2 {
		t.Fatalf("route deletes after withdrawal = %d, want 2", len(mock.routeDels))
	}
	if len(mock.dels) != 2 {
		t.Fatalf("gateway cleanup deletes = %d, want 2", len(mock.dels))
	}
	if _, ok := overlay.loadType5GatewayRef(key); ok {
		t.Fatal("successful MP_UNREACH cleanup should remove retained state")
	}
}

func TestHandleType5RouteReplacementPreservesActiveAndPendingRefs(t *testing.T) {
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:     "192.168.4.10",
			ProvisionVNI: 1000,
			BridgeName:   "br.provision",
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
	oldMAC := mustParseMAC(t, "62:db:b8:c1:80:52")
	newMAC := mustParseMAC(t, "62:db:b8:c1:80:53")

	overlay.handleType5Route(oldRoute, "192.168.4.1", oldMAC, false)
	mock.delErr = errors.New("old gateway cleanup failed")
	overlay.handleType5Route(newRoute, "192.168.4.2", newMAC, false)

	key := type5RouteRefKey(newRoute, nil)
	active, ok := overlay.loadType5GatewayRef(key)
	if !ok || active.gateway != "192.168.4.2" {
		t.Fatalf("active replacement ref = %+v (present=%t), want new gateway", active, ok)
	}
	pending := overlay.loadType5GatewayPendingRefs(key)
	if len(pending) != 1 || pending[0].gateway != "192.168.4.1" {
		t.Fatalf("pending replacement refs = %+v, want old gateway", pending)
	}

	mock.delErr = nil
	overlay.handleType5Route(newRoute, "192.168.4.2", newMAC, false)

	if pending = overlay.loadType5GatewayPendingRefs(key); len(pending) != 0 {
		t.Fatalf("replacement retry left pending refs: %+v", pending)
	}
	active, ok = overlay.loadType5GatewayRef(key)
	if !ok || active.gateway != "192.168.4.2" {
		t.Fatalf("replacement retry active ref = %+v (present=%t), want new gateway", active, ok)
	}
}

func TestHandleType5RouteKeepsDistinctRouteDistinguisherRefs(t *testing.T) {
	rdA, err := buildRouteDistinguisher(65000, 1000)
	if err != nil {
		t.Fatalf("build first RD: %v", err)
	}
	rdB, err := buildRouteDistinguisher(65000, 2000)
	if err != nil {
		t.Fatalf("build second RD: %v", err)
	}
	routeA := &apipb.EVPNIPPrefixRoute{
		Rd:          rdA,
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.1",
	}
	routeB := &apipb.EVPNIPPrefixRoute{
		Rd:          rdB,
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
		GwAddress:   "192.168.4.2",
	}
	mock := &mockOverlayNetlinkOps{}
	overlay := &OverlayTier{
		cfg: &Config{
			RouterID:     "192.168.4.10",
			ProvisionVNI: 1000,
			BridgeName:   "br.provision",
		},
		log:        slog.Default(),
		netlinkOps: mock,
	}
	macA := mustParseMAC(t, "62:db:b8:c1:80:52")
	macB := mustParseMAC(t, "62:db:b8:c1:80:53")

	overlay.handleType5Route(routeA, "192.168.4.1", macA, false)
	overlay.handleType5Route(routeB, "192.168.4.2", macB, false)

	keyA := type5RouteRefKey(routeA, nil)
	keyB := type5RouteRefKey(routeB, nil)
	if keyA == keyB {
		t.Fatal("distinct Type-5 RDs must produce distinct cleanup keys")
	}
	if _, ok := overlay.loadType5GatewayRef(keyA); !ok {
		t.Fatal("first RD cleanup ref missing")
	}
	if _, ok := overlay.loadType5GatewayRef(keyB); !ok {
		t.Fatal("second RD cleanup ref missing")
	}

	overlay.handleType5Route(routeA, "", nil, true)

	if _, ok := overlay.loadType5GatewayRef(keyA); ok {
		t.Fatal("withdrawal of first RD should remove only its cleanup ref")
	}
	if ref, ok := overlay.loadType5GatewayRef(keyB); !ok || ref.gateway != "192.168.4.2" {
		t.Fatalf("second RD cleanup ref = %+v (present=%t), want gateway 192.168.4.2", ref, ok)
	}
}

func TestType5RouteRefKeyIncludesPathAndSource(t *testing.T) {
	rd, err := buildRouteDistinguisher(65000, 1000)
	if err != nil {
		t.Fatalf("build RD: %v", err)
	}
	route := &apipb.EVPNIPPrefixRoute{
		Rd:          rd,
		IpPrefix:    "10.100.0.0",
		IpPrefixLen: 24,
	}
	first := type5RouteRefKey(route, &apipb.Path{
		Identifier: 1,
		SourceId:   "192.168.4.1",
	})
	second := type5RouteRefKey(route, &apipb.Path{
		Identifier: 2,
		SourceId:   "192.168.4.2",
	})
	if first == second {
		t.Fatal("different Type-5 path/source identities must not share a cleanup key")
	}
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
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision"},
		log:        slog.Default(),
		netlinkOps: &mockOverlayNetlinkOps{},
	}

	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "not-an-ip",
		IpPrefixLen: 0,
	}

	// Should return early due to invalid prefix — no panic.
	overlay.handleType5Route(route, "10.0.0.1", nil, false)
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
	overlay := &OverlayTier{
		cfg:        &Config{RouterID: "10.0.0.99", ProvisionVNI: 100, BridgeName: "br.provision"},
		log:        slog.Default(),
		netlinkOps: &mockOverlayNetlinkOps{},
	}

	route := &apipb.EVPNIPPrefixRoute{
		IpPrefix:    "0.0.0.0",
		IpPrefixLen: 0,
		GwAddress:   "",
	}

	// No gateway and no next-hop — should return early.
	overlay.handleType5Route(route, "", nil, false)
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

func TestHandleType5RouteTracksRouteWhenUpdateHasNoRouterMAC(t *testing.T) {
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
	ref, ok := overlay.loadType5GatewayRef("10.100.0.0/24")
	if !ok || ref.gateway != "192.168.4.1" || len(ref.routerMAC) != 0 {
		t.Fatalf("no-RMAC route update ref = %+v (present=%t), want route-only cleanup state", ref, ok)
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
		neigh := buildType5GatewayNeighbor(link, net.ParseIP("192.168.4.1"), mustParseMAC(t, "62:db:b8:c1:80:52"))

		assertType5GatewayNeighbor(t, neigh, "192.168.4.1", "62:db:b8:c1:80:52")
	})

	t.Run("skips empty router MAC", func(t *testing.T) {
		if got := buildType5GatewayNeighbor(link, net.ParseIP("192.168.4.1"), nil); got != nil {
			t.Fatalf("neighbor = %#v, want nil", got)
		}
	})

	t.Run("skips EUI-64 router MAC", func(t *testing.T) {
		mac := mustParseMAC(t, "02:00:00:ff:fe:00:00:10")
		if got := buildType5GatewayNeighbor(link, net.ParseIP("192.168.4.1"), mac); got != nil {
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
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
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
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)

	if len(mock.sets) != 2 {
		t.Fatalf("expected gateway neighbor set and failed FDB set, got %d sets", len(mock.sets))
	}
	if len(mock.dels) != 1 {
		t.Fatalf("expected one failed rollback delete, got %d deletes", len(mock.dels))
	}
	assertType5GatewayNeighbor(t, mock.dels[0], "192.168.4.1", "62:db:b8:c1:80:52")
	if _, ok := overlay.loadType5GatewayRef("10.100.0.0/24"); ok {
		t.Fatal("failed rollback should not mark the failed setup active")
	}
	pending := overlay.loadType5GatewayPendingRefs("10.100.0.0/24")
	if len(pending) != 1 || pending[0].gateway != "192.168.4.1" {
		t.Fatalf("failed rollback pending refs = %+v, want gateway 192.168.4.1", pending)
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

	overlay.deleteType5GatewayNeighbor(link, "10.100.0.0/24", net.ParseIP("192.168.4.1"), nil, nil)

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
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
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
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
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
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
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
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
		mustParseMAC(t, "62:db:b8:c1:80:52"),
	)
	overlay.deleteType5GatewayNeighbor(
		link,
		"10.100.0.0/24",
		net.ParseIP("192.168.4.1"),
		net.ParseIP("192.168.4.1"),
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
				net.ParseIP("192.168.4.1"),
				net.ParseIP("192.168.4.1"),
				mustParseMAC(t, "62:db:b8:c1:80:52"),
			)
			overlay.deleteType5GatewayNeighbor(
				link,
				"10.100.0.0/24",
				net.ParseIP("192.168.4.1"),
				net.ParseIP("192.168.4.1"),
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
