//go:build linux

package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/vishvananda/netlink"
)

// Stack composes underlay and overlay tiers into a network.Mode implementation.
type Stack struct {
	underlay   *UnderlayTier
	overlay    *OverlayTier
	cfg        *Config
	log        *slog.Logger
	gateway    *netlink.Route
	routes     gatewayRouteOps
	vtepMu     sync.Mutex
	vtepRoutes map[string]*netlink.Route
}

type gatewayRouteOps interface {
	LinkByName(name string) (netlink.Link, error)
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	RouteGet(destination net.IP, options *netlink.RouteGetOptions) ([]netlink.Route, error)
	RouteReplace(route *netlink.Route) error
	RouteDel(route *netlink.Route) error
}

type netlinkGatewayRouteOps struct{}

func (netlinkGatewayRouteOps) LinkByName(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("link by name %s: %w", name, err)
	}
	return link, nil
}

func (netlinkGatewayRouteOps) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	addrs, err := network.RetryDump(func() ([]netlink.Addr, error) { return netlink.AddrList(link, family) })
	if err != nil {
		return nil, fmt.Errorf("address list for %s: %w", link.Attrs().Name, err)
	}
	return addrs, nil
}

func (netlinkGatewayRouteOps) RouteGet(destination net.IP, options *netlink.RouteGetOptions) ([]netlink.Route, error) {
	routes, err := netlink.RouteGetWithOptions(destination, options)
	if err != nil {
		return nil, fmt.Errorf("route get %s: %w", destination.String(), err)
	}
	return routes, nil
}

func (netlinkGatewayRouteOps) RouteReplace(route *netlink.Route) error {
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("route replace: %w", err)
	}
	return nil
}

func (netlinkGatewayRouteOps) RouteDel(route *netlink.Route) error {
	if err := netlink.RouteDel(route); err != nil {
		return fmt.Errorf("route delete: %w", err)
	}
	return nil
}

// NewStack creates a GoBGP stack from the given configuration.
func NewStack(cfg *Config) *Stack {
	underlay := NewUnderlayTier(cfg)
	overlay := NewOverlayTier(cfg)

	return &Stack{
		underlay:   underlay,
		overlay:    overlay,
		cfg:        cfg,
		log:        slog.With("mode", "gobgp"),
		routes:     netlinkGatewayRouteOps{},
		vtepRoutes: map[string]*netlink.Route{},
	}
}

// Setup initializes the underlay and overlay tiers sequentially.
// The cfg parameter satisfies the network.Mode interface; the stack uses
// its own Config parsed at construction time.
func (s *Stack) Setup(ctx context.Context, _ *network.Config) error {
	s.log.Info("Setting up GoBGP network stack",
		"asn", s.cfg.ASN,
		"routerID", s.cfg.RouterID,
		"vni", s.cfg.ProvisionVNI,
	)

	if s.cfg.Policy != nil {
		s.log.Warn("policy config (communities, local-pref, MED) is configured but not yet applied to BGP sessions")
	}

	// Create VRF first so underlay can assign dummy/NICs to it.
	if err := s.overlay.CreateVRF(); err != nil {
		return fmt.Errorf("create VRF: %w", err)
	}

	if err := s.underlay.Setup(ctx); err != nil {
		s.cleanupVRF()
		return fmt.Errorf("underlay setup: %w", err)
	}

	// Share the BGP server with the overlay tier.
	s.overlay.SetBgpServer(s.underlay.BgpServer())
	s.wireVTEPRoutes()

	if err := s.overlay.Setup(ctx); err != nil {
		// Clean up the underlay that was already started.
		if teardownErr := s.underlay.Teardown(ctx); teardownErr != nil {
			s.log.Warn("Failed to tear down underlay after overlay failure", "error", teardownErr)
		}
		// Clean up the VRF created earlier.
		s.cleanupVRF()
		return fmt.Errorf("overlay setup: %w", err)
	}

	// Install a kernel route to the gateway VTEP so VXLAN outer packets
	// can reach the spine switch. GoBGP does not install received BGP
	// routes into the kernel FIB, so this explicit route is required.
	if s.shouldInstallGatewayRouteDuringSetup() {
		if err := s.installGatewayRoute(); err != nil {
			s.log.Warn("failed to install gateway route", "error", err)
		}
	}

	s.log.Info("GoBGP network stack ready")
	return nil
}

func (s *Stack) shouldInstallGatewayRouteDuringSetup() bool {
	return s.cfg.ProvisionGateway != "" && s.cfg.PeerMode != network.PeerModeNumbered
}

// WaitForConnectivity waits for BGP to establish and then polls the target
// URL until reachable, consistent with other network modes.
func (s *Stack) WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error {
	s.log.Info("Waiting for BGP peer connectivity", "timeout", timeout)

	if err := s.underlay.Ready(ctx, timeout); err != nil {
		return fmt.Errorf("underlay connectivity: %w", err)
	}

	if err := s.ensureGatewayRoute(); err != nil {
		return fmt.Errorf("gateway route: %w", err)
	}

	if target != "" {
		s.logPollingTarget(target)
		if err := network.WaitForHTTP(ctx, target, timeout); err != nil {
			return fmt.Errorf("target connectivity: %w", err)
		}
	}

	return nil
}

func (s *Stack) ensureGatewayRoute() error {
	if s.cfg.ProvisionGateway == "" || s.gateway != nil {
		return nil
	}
	if s.cfg.PeerMode != network.PeerModeNumbered {
		if err := s.installGatewayRoute(); err != nil {
			s.log.Warn("failed to install gateway route", "error", err)
		}
		return nil
	}
	return s.installGatewayRoute()
}

func (s *Stack) logPollingTarget(target string) {
	s.log.Info("BGP established, polling target URL", "target", network.RedactHTTPURLForLog(target))
}

// installGatewayRoute adds a host route to the gateway VTEP. Numbered mode
// resolves the egress link through an already configured neighbor route; other
// modes install a link-scope route via a fabric NIC in the underlay VRF. On
// the point-to-point link between the VM and the leaf/spine switch, the kernel
// will ARP for the destination directly on the interface and the switch will
// respond (proxy-arp or arp_ignore defaults to 0).
//
// When a VRF is configured, the route is installed in the VRF's routing
// table using a NIC that is enslaved to that VRF, so VXLAN outer packets
// (sourced from the VRF) can reach the remote VTEP.
func (s *Stack) installGatewayRoute() error {
	gwIP := net.ParseIP(s.cfg.ProvisionGateway)
	if gwIP == nil {
		return fmt.Errorf("invalid gateway IP %q", s.cfg.ProvisionGateway)
	}

	maskBits := 32
	if gwIP.To4() == nil {
		maskBits = 128
	}

	if s.cfg.PeerMode == network.PeerModeNumbered {
		return s.installNumberedGatewayRoute(gwIP, maskBits)
	}

	if len(s.underlay.nics) == 0 {
		return fmt.Errorf("no underlay NICs available")
	}

	nic := s.selectGatewayNIC()
	link, err := s.gatewayRoutes().LinkByName(nic)
	if err != nil {
		return fmt.Errorf("find NIC %s: %w", nic, err)
	}

	route := &netlink.Route{
		Dst:       &net.IPNet{IP: gwIP, Mask: net.CIDRMask(maskBits, maskBits)},
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
	}

	// When a VRF is active, install in the VRF's routing table so the
	// route is visible to VXLAN outer packet forwarding.
	if s.overlay.cfg.VRFName != "" {
		route.Table = int(s.overlay.cfg.VRFTableID)
	}

	if err := s.gatewayRoutes().RouteReplace(route); err != nil {
		return fmt.Errorf("replace route to %s via %s: %w", s.cfg.ProvisionGateway, nic, err)
	}
	s.gateway = route

	s.log.Info("installed gateway VTEP route", "gateway", s.cfg.ProvisionGateway, "nic", nic)
	return nil
}

func (s *Stack) installNumberedGatewayRoute(gwIP net.IP, maskBits int) error {
	gatewayRoute, addr, err := s.numberedRoute(gwIP, maskBits)
	if err != nil {
		return err
	}
	if err := s.gatewayRoutes().RouteReplace(gatewayRoute); err != nil {
		return fmt.Errorf("replace numbered route to %s via %s: %w", s.cfg.ProvisionGateway, addr, err)
	}
	s.gateway = gatewayRoute
	s.log.Info("installed numbered gateway VTEP route",
		"gateway", s.cfg.ProvisionGateway,
		"neighbor", addr,
		"linkIndex", gatewayRoute.LinkIndex,
	)
	return nil
}

// numberedRoute builds a host route to dst through the egress of the first
// usable numbered BGP neighbor. It returns the route and the neighbor.
func (s *Stack) numberedRoute(dst net.IP, maskBits int) (*netlink.Route, string, error) {
	var lastErr error
	for _, addr := range s.cfg.NeighborAddrs {
		neighbor := net.ParseIP(addr)
		if neighbor == nil {
			continue
		}
		if !sameIPFamily(dst, neighbor) {
			lastErr = fmt.Errorf("neighbor %s address family does not match gateway %s", addr, dst)
			continue
		}
		egress, err := s.gatewayRoutes().RouteGet(neighbor, s.numberedGatewayRouteGetOptions())
		if err != nil {
			lastErr = err
			continue
		}
		foundLink := false
		for i := range egress {
			route, hasLink, err := s.numberedGatewayRoute(dst, maskBits, neighbor, &egress[i])
			if !hasLink {
				continue
			}
			foundLink = true
			if err != nil {
				lastErr = err
				continue
			}
			return route, addr, nil
		}
		if !foundLink {
			lastErr = fmt.Errorf("route to neighbor %s has no link index", addr)
		}
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("resolve numbered gateway egress: %w", lastErr)
	}
	return nil, "", fmt.Errorf("no usable BGP neighbors for numbered gateway route")
}

func (s *Stack) numberedGatewayRouteGetOptions() *netlink.RouteGetOptions {
	if s.overlay.cfg.VRFName == "" {
		return nil
	}
	return &netlink.RouteGetOptions{VrfName: s.overlay.cfg.VRFName}
}

func (s *Stack) numberedGatewayRoute(gwIP net.IP, maskBits int, neighbor net.IP, route *netlink.Route) (*netlink.Route, bool, error) {
	if route.LinkIndex == 0 {
		return nil, false, nil
	}
	gatewayRoute := &netlink.Route{
		Dst:       &net.IPNet{IP: gwIP, Mask: net.CIDRMask(maskBits, maskBits)},
		LinkIndex: route.LinkIndex,
	}
	if route.Table != 0 {
		gatewayRoute.Table = route.Table
	} else if s.overlay.cfg.VRFName != "" {
		gatewayRoute.Table = int(s.overlay.cfg.VRFTableID)
	}
	switch {
	case route.Gw != nil:
		if !sameIPFamily(gwIP, route.Gw) {
			return nil, true, fmt.Errorf("egress gateway %s address family does not match gateway %s", route.Gw.String(), gwIP)
		}
		gatewayRoute.Gw = route.Gw
	case neighbor.Equal(gwIP):
		gatewayRoute.Scope = netlink.SCOPE_LINK
	default:
		gatewayRoute.Gw = neighbor
	}
	return gatewayRoute, true, nil
}

func sameIPFamily(a, b net.IP) bool {
	return (a.To4() == nil) == (b.To4() == nil)
}

// selectGatewayNIC picks the NIC to use for the gateway route. When a VRF
// is configured, it picks the first NIC that is actually enslaved to the
// VRF (skipping management interfaces like eth0 that may not be in the VRF).
// Without VRF, it skips NICs that have an IPv4 address assigned (management
// NICs have Docker-assigned IPs; fabric NICs in unnumbered mode have none).
// Falls back to nics[0] if no better candidate is found.
func (s *Stack) selectGatewayNIC() string {
	if s.overlay.cfg.VRFName == "" {
		for _, nic := range s.underlay.nics {
			link, err := s.gatewayRoutes().LinkByName(nic)
			if err != nil {
				continue
			}
			addrs, err := s.gatewayRoutes().AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				continue
			}
			if len(addrs) == 0 {
				return nic
			}
		}
		return s.underlay.nics[0]
	}

	vrfLink, err := s.gatewayRoutes().LinkByName(s.overlay.cfg.VRFName)
	if err != nil {
		return s.underlay.nics[0]
	}
	vrfIdx := vrfLink.Attrs().Index

	for _, nic := range s.underlay.nics {
		link, err := s.gatewayRoutes().LinkByName(nic)
		if err != nil {
			continue
		}
		if link.Attrs().MasterIndex == vrfIdx {
			return nic
		}
	}

	return s.underlay.nics[0]
}

func (s *Stack) teardownGatewayRoute() {
	if s.gateway == nil {
		return
	}
	if err := s.gatewayRoutes().RouteDel(s.gateway); err != nil {
		s.log.Debug("failed to delete gateway VTEP route", "gateway", s.cfg.ProvisionGateway, "error", err)
	}
	s.gateway = nil
}

// wireVTEPRoutes lets the overlay install a route to each Type-5 VTEP
// when no provision gateway is set. With the FIB writer (BGP_INSTALL_ROUTES,
// on with FRR config sessions) the routes learned from the fabric reach
// the VTEPs, like zebra.
func (s *Stack) wireVTEPRoutes() {
	if s.cfg.ProvisionGateway == "" && !s.cfg.InstallRoutes {
		s.overlay.ensureVTEPRoute = s.ensureVTEPRoute
	}
}

func (s *Stack) ensureVTEPRoute(vtep net.IP) error {
	vtep = vtep.To4()
	if vtep == nil {
		return fmt.Errorf("VTEP must be an IPv4 address")
	}
	key := vtep.String()
	s.vtepMu.Lock()
	defer s.vtepMu.Unlock()
	if _, ok := s.vtepRoutes[key]; ok {
		return nil
	}
	if s.cfg.PeerMode == network.PeerModeNumbered {
		// Numbered mode has no fabric NIC list. The VTEP is reached
		// through the numbered neighbor, like the provision gateway.
		route, addr, err := s.numberedRoute(vtep, 32)
		if err != nil {
			return fmt.Errorf("route to VTEP %s: %w", key, err)
		}
		if err := s.gatewayRoutes().RouteReplace(route); err != nil {
			return fmt.Errorf("replace numbered route to VTEP %s via %s: %w", key, addr, err)
		}
		s.vtepRoutes[key] = route
		return nil
	}
	if len(s.underlay.nics) == 0 {
		return fmt.Errorf("no underlay NICs available")
	}
	nic := s.selectGatewayNIC()
	link, err := s.gatewayRoutes().LinkByName(nic)
	if err != nil {
		return fmt.Errorf("find underlay NIC %s: %w", nic, err)
	}
	route := &netlink.Route{
		Dst:       &net.IPNet{IP: vtep, Mask: net.CIDRMask(32, 32)},
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
	}
	if s.overlay.cfg.VRFName != "" {
		route.Table = int(s.overlay.cfg.VRFTableID)
	}
	if err := s.gatewayRoutes().RouteReplace(route); err != nil {
		return fmt.Errorf("replace underlay route to VTEP %s via %s: %w", key, nic, err)
	}
	s.vtepRoutes[key] = route
	return nil
}

func (s *Stack) teardownVTEPRoutes() {
	s.vtepMu.Lock()
	defer s.vtepMu.Unlock()
	for vtep, route := range s.vtepRoutes {
		if err := s.gatewayRoutes().RouteDel(route); err != nil {
			s.log.Debug("failed to delete Type-5 VTEP route", "vtep", vtep, "error", err)
		}
	}
	clear(s.vtepRoutes)
}

func (s *Stack) gatewayRoutes() gatewayRouteOps {
	if s.routes != nil {
		return s.routes
	}
	return netlinkGatewayRouteOps{}
}

// cleanupVRF removes VRF links created by the overlay tier.
func (s *Stack) cleanupVRF() {
	for name, created := range s.overlay.createdVRFs {
		if name == "" || !created {
			continue
		}
		link, err := netlink.LinkByName(name)
		if err != nil {
			continue
		}
		if err := netlink.LinkDel(link); err != nil {
			s.log.Warn("failed to delete VRF", "name", name, "error", err)
		}
		delete(s.overlay.createdVRFs, name)
	}
}

// Teardown tears down the overlay and underlay tiers in reverse order.
// VRF is deleted last since both tiers may have interfaces enslaved to it.
func (s *Stack) Teardown(ctx context.Context) error {
	s.log.Info("Tearing down GoBGP network stack")

	var firstErr error

	s.teardownGatewayRoute()

	// Stop the overlay first. It waits for a Type-5 update in flight, so
	// no VTEP route is added after the cleanup below.
	if err := s.overlay.Teardown(ctx); err != nil {
		s.log.Warn("Overlay teardown error", "error", err)
		firstErr = err
	}
	s.teardownVTEPRoutes()

	if err := s.underlay.Teardown(ctx); err != nil {
		s.log.Warn("Underlay teardown error", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	// Delete VRF after both tiers have detached their interfaces.
	s.cleanupVRF()

	return firstErr
}
