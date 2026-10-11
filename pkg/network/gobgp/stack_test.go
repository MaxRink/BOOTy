//go:build linux

package gobgp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/telekom/BOOTy/pkg/network"
)

func TestStackLogPollingTargetRedactsSensitiveURLParts(t *testing.T) {
	var logs bytes.Buffer
	stack := &Stack{
		log: slog.New(slog.NewTextHandler(&logs, nil)),
	}

	stack.logPollingTarget("https://user:super-secret@example.telekom.de/images/provisioner.iso?token=abc123#frag")

	got := logs.String()
	for _, leaked := range []string{"user", "super-secret", "token=abc123", "frag"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("logs leaked %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, `target=https://example.telekom.de/images/provisioner.iso`) {
		t.Fatalf("logs = %q, want redacted target URL", got)
	}
}

func TestInstallGatewayRouteNumberedUsesNeighborEgress(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 7}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}

	if err := stack.installGatewayRoute(); err != nil {
		t.Fatalf("installGatewayRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if got := route.Dst.String(); got != "10.0.0.1/32" {
		t.Fatalf("route Dst = %s, want 10.0.0.1/32", got)
	}
	if route.LinkIndex != 7 {
		t.Fatalf("route LinkIndex = %d, want 7", route.LinkIndex)
	}
	if !route.Gw.Equal(net.ParseIP("10.0.2.1")) {
		t.Fatalf("route Gw = %v, want 10.0.2.1", route.Gw)
	}
	if stack.gateway != route {
		t.Fatal("stack did not track installed gateway route")
	}
}

func TestGatewayRouteSetupDefersNumberedMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		mode network.PeerMode
		want bool
	}{
		{name: "unnumbered", mode: network.PeerModeUnnumbered, want: true},
		{name: "dual", mode: network.PeerModeDual, want: true},
		{name: "numbered", mode: network.PeerModeNumbered, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stack := newGatewayRouteTestStack(tt.mode, &mockGatewayRoutes{})

			got := stack.shouldInstallGatewayRouteDuringSetup()

			if got != tt.want {
				t.Fatalf("shouldInstallGatewayRouteDuringSetup() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstallGatewayRouteNumberedSetsVRFTable(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 7}},
		},
		requiredRouteGetVRFName: "Vrf_underlay",
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}
	stack.cfg.VRFName = "Vrf_underlay"
	stack.cfg.VRFTableID = 1000

	if err := stack.installGatewayRoute(); err != nil {
		t.Fatalf("installGatewayRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if route.Table != 1000 {
		t.Fatalf("route Table = %d, want 1000", route.Table)
	}
	if got := strings.Join(routes.routeGetVRFNames, ","); got != "Vrf_underlay" {
		t.Fatalf("RouteGet VRFs = %q, want Vrf_underlay", got)
	}
}

func TestInstallGatewayRouteNumberedMirrorsEgressGateway(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 7, Gw: net.ParseIP("10.0.3.1"), Table: 200}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}
	stack.cfg.VRFName = "Vrf_underlay"
	stack.cfg.VRFTableID = 1000

	if err := stack.installGatewayRoute(); err != nil {
		t.Fatalf("installGatewayRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if !route.Gw.Equal(net.ParseIP("10.0.3.1")) {
		t.Fatalf("route Gw = %v, want egress gateway 10.0.3.1", route.Gw)
	}
	if route.Table != 200 {
		t.Fatalf("route Table = %d, want egress table 200", route.Table)
	}
}

func TestInstallGatewayRouteNumberedUsesLinkScopeWhenNeighborIsGateway(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.0.1": {{LinkIndex: 8}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.0.1"}

	if err := stack.installGatewayRoute(); err != nil {
		t.Fatalf("installGatewayRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if route.LinkIndex != 8 {
		t.Fatalf("route LinkIndex = %d, want 8", route.LinkIndex)
	}
	if route.Gw != nil {
		t.Fatalf("route Gw = %v, want nil for on-link neighbor", route.Gw)
	}
	if route.Scope != netlink.SCOPE_LINK {
		t.Fatalf("route Scope = %d, want SCOPE_LINK", route.Scope)
	}
}

func TestInstallGatewayRouteNumberedSkipsAddressFamilyMismatch(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 9}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"2001:db8::1", "10.0.2.1"}

	if err := stack.installGatewayRoute(); err != nil {
		t.Fatalf("installGatewayRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if !route.Gw.Equal(net.ParseIP("10.0.2.1")) {
		t.Fatalf("route Gw = %v, want 10.0.2.1", route.Gw)
	}
	if got := strings.Join(routes.routeGetCalls, ","); got != "10.0.2.1" {
		t.Fatalf("RouteGet calls = %q, want only IPv4 neighbor", got)
	}
}

func TestWaitForConnectivityInstallsNumberedGatewayRouteAfterUnderlayReady(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 9}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}
	stack.cfg.MinEstablishedPeers = 1
	stack.underlay.peerCountFn = func(context.Context) int { return 1 }
	stack.underlay.pollInterval = time.Millisecond

	if err := stack.WaitForConnectivity(context.Background(), "", time.Second); err != nil {
		t.Fatalf("WaitForConnectivity() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if !route.Gw.Equal(net.ParseIP("10.0.2.1")) {
		t.Fatalf("route Gw = %v, want 10.0.2.1", route.Gw)
	}
}

func TestWaitForConnectivityKeepsUnnumberedGatewayRouteBestEffort(t *testing.T) {
	routes := &mockGatewayRoutes{
		links: map[string]netlink.Link{
			"eth1": testLink("eth1", 9, 0),
		},
		replaceErr: errors.New("netlink busy"),
	}
	stack := newGatewayRouteTestStack(network.PeerModeUnnumbered, routes)
	stack.underlay.nics = []string{"eth1"}
	stack.cfg.MinEstablishedPeers = 1
	stack.underlay.peerCountFn = func(context.Context) int { return 1 }
	stack.underlay.pollInterval = time.Millisecond

	if err := stack.WaitForConnectivity(context.Background(), "", time.Second); err != nil {
		t.Fatalf("WaitForConnectivity() error = %v, want non-numbered gateway route failure to stay best-effort", err)
	}
	if stack.gateway != nil {
		t.Fatalf("stack gateway = %+v, want nil after failed best-effort install", stack.gateway)
	}
}

func TestWaitForConnectivityFailsNumberedGatewayRouteAfterUnderlayReady(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGetErr: map[string]error{
			"10.0.2.1": errors.New("unreachable"),
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}
	stack.cfg.MinEstablishedPeers = 1
	stack.underlay.peerCountFn = func(context.Context) int { return 1 }
	stack.underlay.pollInterval = time.Millisecond

	err := stack.WaitForConnectivity(context.Background(), "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "gateway route") {
		t.Fatalf("WaitForConnectivity() error = %v, want numbered gateway route failure", err)
	}
	if len(routes.replaced) != 0 {
		t.Fatalf("installed %d routes, want 0", len(routes.replaced))
	}
}

func TestInstallGatewayRouteNumberedFailsWhenNeighborHasNoKernelRoute(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGetErr: map[string]error{
			"10.0.2.1": errors.New("unreachable"),
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}

	err := stack.installGatewayRoute()
	if err == nil || !strings.Contains(err.Error(), "resolve numbered gateway egress") {
		t.Fatalf("installGatewayRoute() error = %v, want numbered egress error", err)
	}
	if len(routes.replaced) != 0 {
		t.Fatalf("installed %d routes, want 0", len(routes.replaced))
	}
}

func TestInstallGatewayRouteNumberedReportsEgressGatewayFamilyMismatch(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 9, Gw: net.ParseIP("2001:db8::1")}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}

	err := stack.installGatewayRoute()
	if err == nil || !strings.Contains(err.Error(), "egress gateway 2001:db8::1 address family does not match gateway 10.0.0.1") {
		t.Fatalf("installGatewayRoute() error = %v, want egress gateway family mismatch", err)
	}
	if len(routes.replaced) != 0 {
		t.Fatalf("installed %d routes, want 0", len(routes.replaced))
	}
}

func TestInstallGatewayRouteUnnumberedStillSelectsFabricNIC(t *testing.T) {
	routes := &mockGatewayRoutes{
		links: map[string]netlink.Link{
			"eth0": testLink("eth0", 1, 0),
			"eth1": testLink("eth1", 2, 0),
		},
		addrs: map[string][]netlink.Addr{
			"eth0": {testAddr(t, "172.17.0.2/16")},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeUnnumbered, routes)
	stack.underlay.nics = []string{"eth0", "eth1"}

	if err := stack.installGatewayRoute(); err != nil {
		t.Fatalf("installGatewayRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if route.LinkIndex != 2 {
		t.Fatalf("route LinkIndex = %d, want eth1 index 2", route.LinkIndex)
	}
	if route.Scope != netlink.SCOPE_LINK {
		t.Fatalf("route Scope = %d, want SCOPE_LINK", route.Scope)
	}
}

type mockGatewayRoutes struct {
	links                   map[string]netlink.Link
	addrs                   map[string][]netlink.Addr
	routeGet                map[string][]netlink.Route
	routeGetErr             map[string]error
	routeGetCalls           []string
	routeGetVRFNames        []string
	requiredRouteGetVRFName string
	replaced                []*netlink.Route
	deleted                 []*netlink.Route
	replaceErr              error
}

func (m *mockGatewayRoutes) LinkByName(name string) (netlink.Link, error) {
	link, ok := m.links[name]
	if !ok {
		return nil, fmt.Errorf("missing link %s", name)
	}
	return link, nil
}

func (m *mockGatewayRoutes) AddrList(link netlink.Link, _ int) ([]netlink.Addr, error) {
	return m.addrs[link.Attrs().Name], nil
}

func (m *mockGatewayRoutes) RouteGet(destination net.IP, options *netlink.RouteGetOptions) ([]netlink.Route, error) {
	key := destination.String()
	m.routeGetCalls = append(m.routeGetCalls, key)
	vrfName := ""
	if options != nil {
		vrfName = options.VrfName
	}
	m.routeGetVRFNames = append(m.routeGetVRFNames, vrfName)
	if m.requiredRouteGetVRFName != "" && vrfName != m.requiredRouteGetVRFName {
		return nil, fmt.Errorf("route get VRF = %q, want %q", vrfName, m.requiredRouteGetVRFName)
	}
	if err := m.routeGetErr[key]; err != nil {
		return nil, err
	}
	return m.routeGet[key], nil
}

func (m *mockGatewayRoutes) RouteReplace(route *netlink.Route) error {
	if m.replaceErr != nil {
		return m.replaceErr
	}
	m.replaced = append(m.replaced, route)
	return nil
}

func (m *mockGatewayRoutes) RouteDel(route *netlink.Route) error {
	m.deleted = append(m.deleted, route)
	return nil
}

func newGatewayRouteTestStack(mode network.PeerMode, routes *mockGatewayRoutes) *Stack {
	cfg := &Config{
		PeerMode:         mode,
		ProvisionGateway: "10.0.0.1",
	}
	return &Stack{
		cfg:      cfg,
		underlay: NewUnderlayTier(cfg),
		overlay:  &OverlayTier{cfg: cfg},
		log:      slog.Default(),
		routes:   routes,
	}
}

func requireSingleGatewayRoute(t *testing.T, routes *mockGatewayRoutes) *netlink.Route {
	t.Helper()
	if len(routes.replaced) != 1 {
		t.Fatalf("installed %d routes, want 1", len(routes.replaced))
	}
	return routes.replaced[0]
}

func testLink(name string, index, masterIndex int) netlink.Link {
	return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{
		Name:        name,
		Index:       index,
		MasterIndex: masterIndex,
	}}
}

func testAddr(t *testing.T, cidr string) netlink.Addr {
	t.Helper()
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatalf("ParseAddr(%q) error = %v", cidr, err)
	}
	return *addr
}

func TestEnsureVTEPRouteNumberedUsesNeighborEgress(t *testing.T) {
	routes := &mockGatewayRoutes{
		routeGet: map[string][]netlink.Route{
			"10.0.2.1": {{LinkIndex: 7}},
		},
	}
	stack := newGatewayRouteTestStack(network.PeerModeNumbered, routes)
	stack.cfg.ProvisionGateway = ""
	stack.cfg.NeighborAddrs = []string{"10.0.2.1"}
	stack.vtepRoutes = map[string]*netlink.Route{}

	if err := stack.ensureVTEPRoute(net.ParseIP("10.0.0.9")); err != nil {
		t.Fatalf("ensureVTEPRoute() error = %v", err)
	}

	route := requireSingleGatewayRoute(t, routes)
	if got := route.Dst.String(); got != "10.0.0.9/32" {
		t.Fatalf("route Dst = %s, want 10.0.0.9/32", got)
	}
	if route.LinkIndex != 7 || !route.Gw.Equal(net.ParseIP("10.0.2.1")) {
		t.Fatalf("route = %+v, want via 10.0.2.1 on link 7", route)
	}
}
