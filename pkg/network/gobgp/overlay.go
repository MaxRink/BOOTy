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
	"sync"
	"syscall"
	"time"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// vxlanOverhead is the VXLAN encapsulation overhead in bytes:
	// 8 (outer UDP) + 8 (VXLAN header) + 20 (outer IPv4) + 14 (outer Ethernet).
	vxlanOverhead = 50

	// vxlanPort is the IANA-assigned VXLAN UDP port.
	vxlanPort = 4789

	// defaultMTU is the fallback inner MTU when cfg.MTU is too low.
	defaultMTU = 1500
)

// overlayNetlinkOps abstracts overlay netlink operations for testability.
type overlayNetlinkOps interface {
	LinkByName(name string) (netlink.Link, error)
	NeighSet(neigh *netlink.Neigh) error
	NeighAppend(neigh *netlink.Neigh) error
	NeighDel(neigh *netlink.Neigh) error
	RouteReplace(route *netlink.Route) error
	RouteDel(route *netlink.Route) error
}

// netlinkOverlayOps is the production implementation using real netlink calls.
type netlinkOverlayOps struct{}

func (netlinkOverlayOps) LinkByName(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("link by name %s: %w", name, err)
	}
	return link, nil
}

func (netlinkOverlayOps) NeighSet(n *netlink.Neigh) error {
	if err := netlink.NeighSet(n); err != nil {
		return fmt.Errorf("neigh set: %w", err)
	}
	return nil
}

func (netlinkOverlayOps) NeighAppend(n *netlink.Neigh) error {
	if err := netlink.NeighAppend(n); err != nil {
		return fmt.Errorf("neigh append: %w", err)
	}
	return nil
}

func (netlinkOverlayOps) NeighDel(n *netlink.Neigh) error {
	if err := netlink.NeighDel(n); err != nil {
		return fmt.Errorf("neigh del: %w", err)
	}
	return nil
}

func (netlinkOverlayOps) RouteReplace(route *netlink.Route) error {
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("route replace: %w", err)
	}
	return nil
}

func (netlinkOverlayOps) RouteDel(route *netlink.Route) error {
	if err := netlink.RouteDel(route); err != nil {
		return fmt.Errorf("route delete: %w", err)
	}
	return nil
}

// OverlayTier manages EVPN Type-5 routes and VXLAN encapsulation.
// When EnableL2 is set, it also handles Type-2 (MAC/IP) and Type-3
// (Inclusive Multicast) routes for L2 overlay use cases.
type OverlayTier struct {
	bgp        *server.BgpServer
	cfg        *Config
	log        *slog.Logger
	cancel     context.CancelFunc
	netlinkOps overlayNetlinkOps
	hooks      *overlaySetupHooks

	// Track resources created by us for clean teardown.
	createdVRFs     map[string]bool
	createdBridge   bool
	createdVXLAN    bool
	addedLoopbackIP *netlink.Addr
	gatewayFDB      *netlink.Neigh

	// macVTEP tracks MAC → VTEP mappings learned from Type-2 routes
	// so withdrawals (which lack next-hop) can still delete the right
	// FDB entry. WatchEvent callbacks may run concurrently.
	macVTEP sync.Map
	// type5GatewayMAC tracks Type-5 gateway IP → router MAC mappings so
	// withdrawals without extended communities can still delete the neighbor.
	// type5GatewayMu serializes gateway MAC/reference updates and the refcount
	// scan used before deleting shared gateway neighbors.
	type5GatewayMu  sync.Mutex
	type5GatewayMAC sync.Map
	// type5GatewayRefs tracks Type-5 prefix → gateway references so one
	// prefix withdrawal cannot delete a shared gateway neighbor.
	type5GatewayRefs sync.Map

	importRTOnce sync.Once
	importRTASN  uint32
	importRTVNI  uint32
	importRTErr  error
}

type type5GatewayRef struct {
	gateway   string
	vtep      string
	routerMAC net.HardwareAddr
}

type overlaySetupHooks struct {
	createVXLANAndBridge func() error
	addProvisionIP       func() error
	teardown             func(context.Context) error
}

// NewOverlayTier creates a new overlay tier.
func NewOverlayTier(cfg *Config) *OverlayTier {
	return &OverlayTier{
		cfg:         cfg,
		log:         slog.With("tier", "overlay"),
		netlinkOps:  netlinkOverlayOps{},
		createdVRFs: map[string]bool{},
	}
}

// SetBgpServer sets the shared BGP server from the underlay tier.
func (o *OverlayTier) SetBgpServer(s *server.BgpServer) {
	o.bgp = s
}

// Setup creates VXLAN, bridge, and advertises the provision subnet as an
// EVPN Type-5 (IP Prefix) route so the fabric can route to this node.
// Incoming Type-5 routes from the fabric are installed as kernel routes
// by watchRoutes. VRF creation is handled by the stack before setup.
func (o *OverlayTier) Setup(ctx context.Context) error {
	switch OverlayType(o.cfg.OverlayType) {
	case OverlayNone:
		o.log.Info("overlay type is none, skipping overlay setup")
		return nil
	case OverlayL3VPN:
		return fmt.Errorf("overlay type %q is not yet implemented", o.cfg.OverlayType)
	case OverlayEVPNVXLAN:
		// default — continue with EVPN-VXLAN setup below
	default:
		return fmt.Errorf("unknown overlay type %q", o.cfg.OverlayType)
	}

	if o.bgp == nil {
		return fmt.Errorf("BGP server not set: call SetBgpServer before Setup")
	}

	if err := o.setupCreateVXLANAndBridge(); err != nil {
		return o.cleanupSetupFailure(ctx, "create VXLAN/bridge", err)
	}

	if err := o.setupAddProvisionIP(); err != nil {
		return o.cleanupSetupFailure(ctx, "add provision IP", err)
	}

	if err := o.addOverlayLoopback(); err != nil {
		return o.cleanupSetupFailure(ctx, "add overlay loopback", err)
	}

	if err := o.advertiseType5(ctx); err != nil {
		return o.cleanupSetupFailure(ctx, "advertise EVPN Type-5", err)
	}

	if o.cfg.EnableL2 {
		if err := o.advertiseType3(ctx); err != nil {
			return o.cleanupSetupFailure(ctx, "advertise EVPN Type-3", err)
		}
		if err := o.advertiseType2(ctx); err != nil {
			return o.cleanupSetupFailure(ctx, "advertise EVPN Type-2", err)
		}
	}

	watchCtx, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	go o.watchRoutes(watchCtx)

	return nil
}

func (o *OverlayTier) cleanupSetupFailure(ctx context.Context, step string, setupErr error) error {
	if teardownErr := o.setupTeardown(ctx); teardownErr != nil {
		o.log.Warn("failed to tear down overlay after setup failure", "step", step, "error", teardownErr)
	}
	return fmt.Errorf("%s: %w", step, setupErr)
}

func (o *OverlayTier) setupCreateVXLANAndBridge() error {
	if o.hooks != nil && o.hooks.createVXLANAndBridge != nil {
		return o.hooks.createVXLANAndBridge()
	}
	return o.createVXLANAndBridge()
}

func (o *OverlayTier) setupAddProvisionIP() error {
	if o.hooks != nil && o.hooks.addProvisionIP != nil {
		return o.hooks.addProvisionIP()
	}
	return o.addProvisionIP()
}

func (o *OverlayTier) setupTeardown(ctx context.Context) error {
	if o.hooks != nil && o.hooks.teardown != nil {
		return o.hooks.teardown(ctx)
	}
	return o.Teardown(ctx)
}

// Ready waits until the overlay is operational by checking EVPN route state.
func (o *OverlayTier) Ready(_ context.Context, _ time.Duration) error {
	// Overlay is ready once Setup completes (routes are advertised synchronously).
	return nil
}

// Teardown removes the overlay network resources we created (bridge, vxlan)
// and removes the overlay loopback IP from lo.
// VRF is cleaned up separately by Stack.Teardown after underlay detaches.
func (o *OverlayTier) Teardown(_ context.Context) error {
	if o.cancel != nil {
		o.cancel()
	}

	if o.gatewayFDB != nil {
		if err := o.netlinkOps.NeighDel(o.gatewayFDB); err != nil {
			o.log.Debug("failed to remove gateway BUM FDB entry", "vtep", o.gatewayFDB.IP, "error", err)
		} else {
			o.log.Info("removed gateway BUM FDB entry", "vtep", o.gatewayFDB.IP)
		}
		o.gatewayFDB = nil
	}

	// Remove overlay loopback IP from lo.
	if o.addedLoopbackIP != nil {
		lo, err := netlink.LinkByName("lo")
		if err == nil {
			if err := netlink.AddrDel(lo, o.addedLoopbackIP); err != nil {
				o.log.Warn("failed to remove overlay loopback IP", "ip", o.addedLoopbackIP, "error", err)
			}
		}
	}

	vxlanName := o.vxlanName()

	type owned struct {
		name    string
		created bool
	}
	for _, res := range []owned{
		{o.cfg.BridgeName, o.createdBridge},
		{vxlanName, o.createdVXLAN},
	} {
		if res.name == "" || !res.created {
			continue
		}
		link, err := netlink.LinkByName(res.name)
		if err != nil {
			continue
		}
		if err := netlink.LinkDel(link); err != nil {
			o.log.Warn("failed to remove interface", "name", res.name, "error", err)
		}
	}

	return nil
}

// CreateVRF creates configured VRF interfaces.
// Called by Stack before underlay setup so that dummy/NICs can be assigned.
func (o *OverlayTier) CreateVRF() error {
	if err := o.createVRF(o.cfg.VRFName, o.cfg.VRFTableID); err != nil {
		return err
	}
	if o.cfg.OverlayVRFName != o.cfg.VRFName {
		return o.createVRF(o.cfg.OverlayVRFName, o.cfg.OverlayVRFTableID)
	}
	return nil
}

func (o *OverlayTier) createVRF(name string, tableID uint32) error {
	if name == "" {
		return nil
	}

	vrf := &netlink.Vrf{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Table:     tableID,
	}
	if err := netlink.LinkAdd(vrf); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("add VRF %s: %w", name, err)
		}
	} else {
		if o.createdVRFs == nil {
			o.createdVRFs = map[string]bool{}
		}
		o.createdVRFs[name] = true
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find VRF %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up VRF %s: %w", name, err)
	}

	o.log.Info("vrf ready", "name", name, "table", tableID)
	return nil
}

func (o *OverlayTier) overlayRouteTable() int {
	if o.cfg.OverlayVRFName == "" {
		return 0
	}
	return int(o.cfg.OverlayVRFTableID)
}

// vxlanName returns the VXLAN interface name derived from the provision VNI.
func (o *OverlayTier) vxlanName() string {
	return fmt.Sprintf("vx%d", o.cfg.ProvisionVNI)
}

func (o *OverlayTier) createVXLANAndBridge() error {
	vxlanName := o.vxlanName()

	vxLink, err := o.createVXLAN(vxlanName)
	if err != nil {
		return err
	}

	brLink, err := o.createBridge()
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMasterByIndex(vxLink, brLink.Attrs().Index); err != nil {
		return fmt.Errorf("attach VXLAN to bridge: %w", err)
	}

	// Assign bridge to the configured overlay VRF. Production-like netplan
	// often keeps the L3VNI bridge in the default VRF while only the
	// underlay NIC and VTEP source live in Vrf_underlay.
	if o.cfg.OverlayVRFName != "" {
		vrfLink, err := netlink.LinkByName(o.cfg.OverlayVRFName)
		if err != nil {
			return fmt.Errorf("find overlay VRF %s: %w", o.cfg.OverlayVRFName, err)
		}
		if err := netlink.LinkSetMasterByIndex(brLink, vrfLink.Attrs().Index); err != nil {
			return fmt.Errorf("assign bridge to overlay VRF: %w", err)
		}
	}

	if err := netlink.LinkSetUp(brLink); err != nil {
		return fmt.Errorf("bring up bridge: %w", err)
	}
	if err := netlink.LinkSetUp(vxLink); err != nil {
		return fmt.Errorf("bring up VXLAN: %w", err)
	}

	// Install a BUM FDB entry so broadcast/unknown/multicast traffic
	// (e.g. ARP for the gateway) is flooded to the gateway VTEP.
	// Without this, the VXLAN FDB is empty and BUM frames are dropped.
	if o.cfg.ProvisionGateway != "" {
		if err := o.addGatewayFDB(vxLink); err != nil {
			o.log.Warn("gateway BUM FDB entry failed (non-fatal)", "error", err)
		}
	}

	o.log.Info("created VXLAN and bridge",
		"vxlan", vxlanName, "vni", o.cfg.ProvisionVNI,
		"bridge", o.cfg.BridgeName,
	)
	return nil
}

func (o *OverlayTier) createVXLAN(name string) (netlink.Link, error) {
	vxlanMTU := o.cfg.MTU - vxlanOverhead
	if vxlanMTU <= 0 {
		vxlanMTU = defaultMTU
	}

	srcAddr := net.ParseIP(o.cfg.RouterID)
	vxlan := &netlink.Vxlan{
		LinkAttrs:    netlink.LinkAttrs{Name: name},
		VxlanId:      o.cfg.ProvisionVNI,
		SrcAddr:      srcAddr,
		Port:         vxlanPort,
		Learning:     false,
		VtepDevIndex: 0,
	}

	if err := netlink.LinkAdd(vxlan); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			return nil, fmt.Errorf("add VXLAN %s: %w", name, err)
		}
	} else {
		o.createdVXLAN = true
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("find VXLAN: %w", err)
	}
	if err := netlink.LinkSetMTU(link, vxlanMTU); err != nil {
		return nil, fmt.Errorf("set VXLAN MTU: %w", err)
	}
	return link, nil
}

func (o *OverlayTier) createBridge() (netlink.Link, error) {
	hwAddr, err := net.ParseMAC(o.cfg.BridgeMAC)
	if err != nil {
		return nil, fmt.Errorf("parse bridge MAC %s: %w", o.cfg.BridgeMAC, err)
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name:         o.cfg.BridgeName,
			HardwareAddr: hwAddr,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			return nil, fmt.Errorf("add bridge %s: %w", o.cfg.BridgeName, err)
		}
	} else {
		o.createdBridge = true
	}

	link, err := netlink.LinkByName(o.cfg.BridgeName)
	if err != nil {
		return nil, fmt.Errorf("find bridge: %w", err)
	}
	return link, nil
}

func (o *OverlayTier) addProvisionIP() error {
	if o.cfg.ProvisionIP == "" {
		return nil
	}

	link, err := netlink.LinkByName(o.cfg.BridgeName)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", o.cfg.BridgeName, err)
	}

	addr, err := netlink.ParseAddr(o.cfg.ProvisionIP)
	if err != nil {
		return fmt.Errorf("parse provision IP %s: %w", o.cfg.ProvisionIP, err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add provision IP to bridge: %w", err)
	}

	o.log.Info("assigned provision IP", "bridge", o.cfg.BridgeName, "ip", o.cfg.ProvisionIP)
	return nil
}

// addGatewayFDB installs a BUM (broadcast/unknown/multicast) FDB entry on the
// VXLAN interface pointing to the gateway's VTEP. This is equivalent to:
//
//	bridge fdb append 00:00:00:00:00:00 dev vxlanXXX dst <gateway> self permanent
//
// Without this entry the VXLAN has no remote VTEP and drops all BUM frames,
// making ARP resolution impossible.
func (o *OverlayTier) addGatewayFDB(vxLink netlink.Link) error {
	gwIP := net.ParseIP(o.cfg.ProvisionGateway)
	if gwIP == nil {
		return fmt.Errorf("parse gateway VTEP IP %q", o.cfg.ProvisionGateway)
	}

	fdb := &netlink.Neigh{
		LinkIndex:    vxLink.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		HardwareAddr: net.HardwareAddr{0, 0, 0, 0, 0, 0},
		IP:           gwIP,
		Flags:        netlink.NTF_SELF,
		State:        netlink.NUD_PERMANENT,
	}
	if err := o.netlinkOps.NeighAppend(fdb); err != nil {
		return fmt.Errorf("append BUM FDB entry for %s: %w", o.cfg.ProvisionGateway, err)
	}
	o.gatewayFDB = fdb

	o.log.Info("installed gateway BUM FDB entry", "vxlan", vxLink.Attrs().Name, "vtep", o.cfg.ProvisionGateway)
	return nil
}

func (o *OverlayTier) addOverlayLoopback() error {
	if o.cfg.OverlayIP == "" {
		return nil
	}

	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}

	// Try /128 (IPv6) first, fall back to /32 (IPv4).
	addr, err := netlink.ParseAddr(o.cfg.OverlayIP + "/128")
	if err != nil {
		addr, err = netlink.ParseAddr(o.cfg.OverlayIP + "/32")
		if err != nil {
			return fmt.Errorf("parse overlay IP %s: %w", o.cfg.OverlayIP, err)
		}
	}

	if err := netlink.AddrAdd(lo, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add overlay IP to loopback: %w", err)
	}
	o.addedLoopbackIP = addr

	o.log.Info("added overlay loopback", "ip", o.cfg.OverlayIP)
	return nil
}

// advertiseType5 advertises this node's provision host IP as an EVPN Type-5
// (IP Prefix) /32 route so the fabric can route overlay traffic to this VTEP.
// The /32 is required because multiple BOOTy nodes may share the same /24
// provision subnet — only unique host routes allow per-node reachability.
func (o *OverlayTier) advertiseType5(ctx context.Context) error {
	if o.cfg.ProvisionIP == "" {
		o.log.Warn("provision IP not set, skipping EVPN Type-5 advertisement")
		return nil
	}

	rd, err := buildRouteDistinguisher(o.cfg.ASN, uint32(o.cfg.ProvisionVNI))
	if err != nil {
		return fmt.Errorf("build route distinguisher: %w", err)
	}

	// Extract the host IP from the CIDR and advertise it as a /32 host route.
	ip, _, err := net.ParseCIDR(o.cfg.ProvisionIP)
	if err != nil {
		return fmt.Errorf("parse provision IP %s: %w", o.cfg.ProvisionIP, err)
	}
	hostRoute := ip.String() + "/32"

	nlri, err := buildEVPNType5NLRI(rd, hostRoute, type5DirectGateway, uint32(o.cfg.ProvisionVNI))
	if err != nil {
		return fmt.Errorf("build EVPN NLRI: %w", err)
	}

	pattrs, err := buildType5PathAttrs(nlri, o.cfg.RouterID, o.cfg.ASN, uint32(o.cfg.ProvisionVNI), o.cfg.VPNRT, o.cfg.BridgeMAC)
	if err != nil {
		return fmt.Errorf("build path attributes: %w", err)
	}

	_, err = o.bgp.AddPath(ctx, &apipb.AddPathRequest{
		Path: &apipb.Path{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
			Nlri:   nlri,
			Pattrs: pattrs,
		},
	})
	if err != nil {
		return fmt.Errorf("add EVPN Type-5 path: %w", err)
	}

	o.log.Info("advertised EVPN type-5 host route",
		"ip", hostRoute, "vni", o.cfg.ProvisionVNI)
	return nil
}

// advertiseType3 originates an EVPN Type-3 (Inclusive Multicast Ethernet Tag)
// route so that remote VTEPs in the fabric include this node as a BUM flooding
// target. The IMET route carries a PMSI tunnel attribute (ingress replication)
// telling peers to use unicast VXLAN encapsulation for BUM traffic.
func (o *OverlayTier) advertiseType3(ctx context.Context) error {
	rd, err := buildRouteDistinguisher(o.cfg.ASN, uint32(o.cfg.ProvisionVNI))
	if err != nil {
		return fmt.Errorf("build route distinguisher: %w", err)
	}

	nlri, err := buildEVPNType3NLRI(rd, o.cfg.RouterID)
	if err != nil {
		return fmt.Errorf("build EVPN type-3 NLRI: %w", err)
	}

	pattrs, err := buildType3PathAttrs(nlri, o.cfg.RouterID, o.cfg.ASN, uint32(o.cfg.ProvisionVNI), o.cfg.VPNRT)
	if err != nil {
		return fmt.Errorf("build type-3 path attributes: %w", err)
	}

	_, err = o.bgp.AddPath(ctx, &apipb.AddPathRequest{
		Path: &apipb.Path{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
			Nlri:   nlri,
			Pattrs: pattrs,
		},
	})
	if err != nil {
		return fmt.Errorf("add EVPN type-3 path: %w", err)
	}

	o.log.Info("advertised EVPN type-3 IMET route",
		"vtep", o.cfg.RouterID, "vni", o.cfg.ProvisionVNI)
	return nil
}

// advertiseType2 originates an EVPN Type-2 (MAC/IP Advertisement) route for
// the local bridge MAC and provision IP. This lets remote VTEPs install an FDB
// entry via BGP control-plane learning instead of relying on data-plane flooding.
func (o *OverlayTier) advertiseType2(ctx context.Context) error {
	if o.cfg.BridgeMAC == "" || o.cfg.ProvisionIP == "" {
		o.log.Warn("bridge MAC or provision IP not set, skipping type-2 advertisement")
		return nil
	}

	if _, err := net.ParseMAC(o.cfg.BridgeMAC); err != nil {
		return fmt.Errorf("parse bridge MAC %s: %w", o.cfg.BridgeMAC, err)
	}

	ip, _, err := net.ParseCIDR(o.cfg.ProvisionIP)
	if err != nil {
		return fmt.Errorf("parse provision IP %s: %w", o.cfg.ProvisionIP, err)
	}

	rd, err := buildRouteDistinguisher(o.cfg.ASN, uint32(o.cfg.ProvisionVNI))
	if err != nil {
		return fmt.Errorf("build route distinguisher: %w", err)
	}

	nlri, err := buildEVPNType2NLRI(rd, o.cfg.BridgeMAC, ip.String(), uint32(o.cfg.ProvisionVNI))
	if err != nil {
		return fmt.Errorf("build EVPN type-2 NLRI: %w", err)
	}

	pattrs, err := buildType2PathAttrs(nlri, o.cfg.RouterID, o.cfg.ASN, uint32(o.cfg.ProvisionVNI), o.cfg.VPNRT)
	if err != nil {
		return fmt.Errorf("build type-2 path attributes: %w", err)
	}

	_, err = o.bgp.AddPath(ctx, &apipb.AddPathRequest{
		Path: &apipb.Path{
			Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
			Nlri:   nlri,
			Pattrs: pattrs,
		},
	})
	if err != nil {
		return fmt.Errorf("add EVPN type-2 path: %w", err)
	}

	o.log.Info("advertised EVPN type-2 MAC/IP route",
		"mac", o.cfg.BridgeMAC, "ip", ip, "vni", o.cfg.ProvisionVNI)
	return nil
}

func (o *OverlayTier) watchRoutes(ctx context.Context) {
	err := o.bgp.WatchEvent(ctx, &apipb.WatchEventRequest{
		Table: &apipb.WatchEventRequest_Table{
			Filters: []*apipb.WatchEventRequest_Table_Filter{
				{
					Type: apipb.WatchEventRequest_Table_Filter_BEST,
					Init: true,
				},
			},
		},
	}, func(resp *apipb.WatchEventResponse) {
		if t := resp.GetTable(); t != nil {
			for _, p := range t.GetPaths() {
				o.processRouteUpdate(p)
			}
		}
	})
	if err != nil {
		o.log.Warn("route watcher stopped", "error", err)
	}
}

// processRouteUpdate handles a single BGP path update by dispatching to the
// appropriate handler based on NLRI type. Add/update routes must carry a Route
// Target matching the local ASN+VNI. Withdrawals with Route Target communities
// must also match, while withdrawals without Route Targets are allowed so
// MP_UNREACH updates can remove previously imported routes.
func (o *OverlayTier) processRouteUpdate(p *apipb.Path) {
	withdraw := p.GetIsWithdraw()
	action := "add"
	if withdraw {
		action = "withdraw"
	}

	nlri := p.GetNlri()
	if nlri == nil {
		return
	}

	importASN, importVNI, err := o.importRouteTarget()
	if err != nil {
		o.log.Debug("route update skipped: invalid configured import RT", "action", action, "type", nlri.GetTypeUrl(), "error", err)
		return
	}
	if !routeUpdateMatchesImportRT(p, importASN, importVNI) {
		o.log.Debug("route update skipped: RT mismatch", "action", action, "type", nlri.GetTypeUrl())
		return
	}

	msg, err := nlri.UnmarshalNew()
	if err != nil {
		o.log.Debug("route update unmarshal failed", "error", err)
		return
	}

	vtep := extractNextHop(p)

	switch route := msg.(type) {
	case *apipb.EVPNIPPrefixRoute:
		routerMAC, err := extractRouterMAC(p)
		routerMACValid := true
		if err != nil {
			o.log.Debug("type-5 route with invalid router MAC", "error", err)
			routerMACValid = false
		}
		o.handleType5RouteWithRouterMACState(route, vtep, routerMAC, routerMACValid, withdraw)
	case *apipb.EVPNMACIPAdvertisementRoute:
		if o.cfg.EnableL2 {
			o.handleType2Route(route, vtep, withdraw)
		}
	case *apipb.EVPNInclusiveMulticastEthernetTagRoute:
		if o.cfg.EnableL2 {
			o.handleType3Route(route, vtep, withdraw)
		}
	default:
		o.log.Debug("route update", "action", action, "type", nlri.GetTypeUrl())
	}
}

// handleType5Route installs or removes a kernel route for an IP prefix
// received via EVPN Type-5 (IP Prefix) route. This is how BOOTy learns
// the default route (and any other prefixes) from the fabric.
func (o *OverlayTier) handleType5Route(route *apipb.EVPNIPPrefixRoute, vtep string, routerMAC net.HardwareAddr, withdraw bool) {
	o.handleType5RouteWithRouterMACState(route, vtep, routerMAC, true, withdraw)
}

func (o *OverlayTier) handleType5RouteWithRouterMACState(
	route *apipb.EVPNIPPrefixRoute,
	vtep string,
	routerMAC net.HardwareAddr,
	routerMACValid bool,
	withdraw bool,
) {
	// Skip routes originated by this node (e.g., reflected back by the RR).
	// Installing our own route would override the connected route and break
	// provisioning connectivity.
	if vtep == o.cfg.RouterID {
		return
	}

	prefix := route.GetIpPrefix()
	prefixLen := route.GetIpPrefixLen()

	dst, err := parsePrefixRoute(prefix, prefixLen)
	if err != nil {
		o.log.Debug("type-5 route with invalid prefix", "prefix", prefix, "len", prefixLen, "error", err)
		return
	}

	// Resolve the gateway: prefer the NLRI's GwAddress, fall back to next-hop.
	gwStr := route.GetGwAddress()
	if gwStr == "" || gwStr == type5DirectGateway {
		gwStr = vtep
	}
	gw := net.ParseIP(gwStr)
	vtepIP := net.ParseIP(vtep)
	if withdraw {
		gw, vtepIP = o.resolveType5WithdrawGatewayRef(dst.String(), gw, vtepIP)
	}
	if gw == nil {
		o.log.Debug("type-5 route with no valid gateway", "prefix", dst, "gw", gwStr)
		return
	}

	link, err := o.netlinkOps.LinkByName(o.cfg.BridgeName)
	if err != nil {
		o.log.Warn("cannot find bridge for route install", "bridge", o.cfg.BridgeName, "error", err)
		return
	}

	kr := buildType5KernelRoute(link, dst, gw, o.overlayRouteTable())

	if withdraw {
		if err := o.netlinkOps.RouteDel(kr); err != nil {
			o.log.Debug("failed to delete route from type-5 withdraw", "dst", dst, "gw", gw, "error", err)
			return
		}
		o.log.Info("removed route from type-5 withdraw", "dst", dst, "gw", gw)
		o.deleteType5GatewayNeighbor(link, dst.String(), gw, vtepIP, routerMAC)
		return
	}

	if err := o.netlinkOps.RouteReplace(kr); err != nil {
		o.log.Warn("failed to install route from type-5", "dst", dst, "gw", gw, "error", err)
	} else {
		o.log.Info("installed route from type-5", "dst", dst, "gw", gw)
		if !routerMACValid {
			o.updateType5GatewayRefWithoutRouterMAC(link, dst.String(), gw, vtepIP)
			return
		}
		if len(routerMAC) == 0 {
			o.clearType5GatewayNeighbor(link, dst.String())
			return
		}
		o.setType5GatewayNeighbor(link, dst.String(), gw, vtepIP, routerMAC)
	}
}

func (o *OverlayTier) resolveType5WithdrawGatewayRef(prefix string, gw, vtep net.IP) (net.IP, net.IP) {
	if gw != nil && vtep != nil {
		return gw, vtep
	}
	ref, ok := o.loadType5GatewayRef(prefix)
	if !ok {
		return gw, vtep
	}
	if gw == nil {
		gw = net.ParseIP(ref.gateway)
	}
	if vtep == nil {
		vtep = net.ParseIP(ref.vtep)
	}
	return gw, vtep
}

func (o *OverlayTier) setType5GatewayNeighbor(
	link netlink.Link,
	prefix string,
	gw net.IP,
	vtep net.IP,
	routerMAC net.HardwareAddr,
) {
	o.type5GatewayMu.Lock()
	defer o.type5GatewayMu.Unlock()

	neigh := buildType5GatewayNeighbor(link, gw, routerMAC)
	if neigh == nil {
		return
	}
	fdb := o.buildType5GatewayFDB(vtep, routerMAC)
	if fdb == nil {
		return
	}
	if err := o.netlinkOps.NeighSet(neigh); err != nil {
		o.log.Warn("failed to install type-5 gateway neighbor", "ip", gw, "mac", routerMAC, "error", err)
		return
	}
	if err := o.netlinkOps.NeighSet(fdb); err != nil {
		o.log.Warn("failed to install type-5 gateway FDB entry", "vtep", vtep, "mac", routerMAC, "error", err)
		if delErr := o.netlinkOps.NeighDel(neigh); delErr != nil {
			o.log.Debug("failed to roll back type-5 gateway neighbor", "ip", gw, "mac", routerMAC, "error", delErr)
		}
		return
	}
	o.replaceType5GatewayRef(link, prefix, gw, vtep, routerMAC)
	o.log.Info("installed type-5 gateway neighbor and FDB entry", "ip", gw, "vtep", vtep, "mac", routerMAC)
}

func (o *OverlayTier) updateType5GatewayRefWithoutRouterMAC(link netlink.Link, prefix string, gw, vtep net.IP) {
	o.type5GatewayMu.Lock()
	defer o.type5GatewayMu.Unlock()

	ref := newType5GatewayRef(gw, vtep, nil)
	oldRef, oldOK := o.loadType5GatewayRef(prefix)
	if oldOK && oldRef.gateway == ref.gateway && oldRef.vtep == ref.vtep {
		return
	}
	o.type5GatewayRefs.Store(prefix, ref)
	if oldOK {
		if !o.deleteType5GatewayRefLocked(link, oldRef) {
			o.type5GatewayRefs.Store(prefix, oldRef)
		}
	}
}

func (o *OverlayTier) deleteType5GatewayNeighbor(
	link netlink.Link,
	prefix string,
	gw net.IP,
	vtep net.IP,
	routerMAC net.HardwareAddr,
) {
	o.type5GatewayMu.Lock()
	defer o.type5GatewayMu.Unlock()

	ref := newType5GatewayRef(gw, vtep, routerMAC)
	restoreRef := false
	if stored, ok := o.loadType5GatewayRef(prefix); ok {
		ref = stored
		restoreRef = true
		if len(routerMAC) != 0 {
			ref.routerMAC = append(net.HardwareAddr(nil), routerMAC...)
		}
		if vtep != nil {
			ref.vtep = vtep.String()
		}
	}
	o.type5GatewayRefs.Delete(prefix)

	if len(ref.routerMAC) == 0 {
		if stored, ok := o.type5GatewayMAC.Load(ref.gateway); ok {
			if mac, ok := stored.(net.HardwareAddr); ok {
				ref.routerMAC = mac
			}
		}
	}

	if !o.deleteType5GatewayRefLocked(link, ref) && restoreRef {
		o.type5GatewayRefs.Store(prefix, ref)
	}
}

func (o *OverlayTier) clearType5GatewayNeighbor(link netlink.Link, prefix string) {
	o.type5GatewayMu.Lock()
	defer o.type5GatewayMu.Unlock()

	ref, ok := o.loadType5GatewayRef(prefix)
	if !ok {
		return
	}
	o.type5GatewayRefs.Delete(prefix)
	if !o.deleteType5GatewayRefLocked(link, ref) {
		o.type5GatewayRefs.Store(prefix, ref)
	}
}

func (o *OverlayTier) replaceType5GatewayRef(link netlink.Link, prefix string, gw, vtep net.IP, routerMAC net.HardwareAddr) {
	ref := newType5GatewayRef(gw, vtep, routerMAC)
	oldRef, oldOK := o.loadType5GatewayRef(prefix)
	o.type5GatewayRefs.Store(prefix, ref)
	o.type5GatewayMAC.Store(ref.gateway, append(net.HardwareAddr(nil), routerMAC...))
	if oldOK && !sameType5GatewayRef(oldRef, ref) {
		o.deleteType5GatewayRefLocked(link, oldRef)
	}
}

func (o *OverlayTier) deleteType5GatewayRefLocked(link netlink.Link, ref type5GatewayRef) bool {
	if len(ref.routerMAC) != ethernetMACLength {
		if !o.hasType5GatewayRef(ref.gateway) {
			o.type5GatewayMAC.Delete(ref.gateway)
		}
		return true
	}
	gw := net.ParseIP(ref.gateway)
	vtep := net.ParseIP(ref.vtep)
	deleted := true
	deleteGateway := !o.hasType5GatewayRef(ref.gateway)
	deleteFDB := !o.hasType5GatewayFDBRef(ref.vtep, ref.routerMAC)
	if deleteGateway {
		if neigh := buildType5GatewayNeighbor(link, gw, ref.routerMAC); neigh == nil {
			deleted = false
		} else if err := o.netlinkOps.NeighDel(neigh); err != nil {
			o.log.Debug("failed to delete type-5 gateway neighbor", "ip", gw, "mac", ref.routerMAC, "error", err)
			deleted = false
		}
	}
	if deleteFDB {
		if fdb := o.buildType5GatewayFDB(vtep, ref.routerMAC); fdb == nil {
			deleted = false
		} else if err := o.netlinkOps.NeighDel(fdb); err != nil {
			o.log.Debug("failed to delete type-5 gateway FDB entry", "vtep", vtep, "mac", ref.routerMAC, "error", err)
			deleted = false
		}
	}
	if !deleted {
		return false
	}
	if deleteGateway || deleteFDB {
		o.log.Info("deleted type-5 gateway state",
			"ip", gw, "vtep", vtep, "mac", ref.routerMAC, "neighbor", deleteGateway, "fdb", deleteFDB)
	}
	if deleteGateway {
		o.type5GatewayMAC.Delete(ref.gateway)
	}
	return true
}

func (o *OverlayTier) loadType5GatewayRef(prefix string) (type5GatewayRef, bool) {
	stored, ok := o.type5GatewayRefs.Load(prefix)
	if !ok {
		return type5GatewayRef{}, false
	}
	ref, ok := stored.(type5GatewayRef)
	if !ok {
		return type5GatewayRef{}, false
	}
	ref.routerMAC = append(net.HardwareAddr(nil), ref.routerMAC...)
	return ref, true
}

func newType5GatewayRef(gw, vtep net.IP, routerMAC net.HardwareAddr) type5GatewayRef {
	gateway := ""
	if gw != nil {
		gateway = gw.String()
	}
	vtepStr := ""
	if vtep != nil {
		vtepStr = vtep.String()
	}
	return type5GatewayRef{
		gateway:   gateway,
		vtep:      vtepStr,
		routerMAC: append(net.HardwareAddr(nil), routerMAC...),
	}
}

func (o *OverlayTier) hasType5GatewayRef(gateway string) bool {
	found := false
	o.type5GatewayRefs.Range(func(_, value any) bool {
		ref, ok := value.(type5GatewayRef)
		found = ok && ref.gateway == gateway
		return !found
	})
	return found
}

func (o *OverlayTier) hasType5GatewayFDBRef(vtep string, routerMAC net.HardwareAddr) bool {
	if len(routerMAC) == 0 {
		return false
	}
	found := false
	o.type5GatewayRefs.Range(func(_, value any) bool {
		ref, ok := value.(type5GatewayRef)
		found = ok && ref.vtep == vtep && bytes.Equal(ref.routerMAC, routerMAC)
		return !found
	})
	return found
}

func sameType5GatewayRef(a, b type5GatewayRef) bool {
	return a.gateway == b.gateway && a.vtep == b.vtep && bytes.Equal(a.routerMAC, b.routerMAC)
}

func buildType5KernelRoute(link netlink.Link, dst *net.IPNet, gw net.IP, tableID int) *netlink.Route {
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Gw:        gw,
	}
	if gw != nil {
		// EVPN Type-5 next-hops are remote VTEPs reached through the L3VNI
		// bridge. The bridge often only has a /32 provision IP, so Linux must
		// treat the Type-5 gateway as reachable on this link.
		route.Flags |= int(netlink.FLAG_ONLINK)
	}
	if tableID != 0 {
		route.Table = tableID
	}
	return route
}

func buildType5GatewayNeighbor(link netlink.Link, gw net.IP, routerMAC net.HardwareAddr) *netlink.Neigh {
	if len(routerMAC) != ethernetMACLength {
		return nil
	}
	ip := gw.To4()
	if ip == nil {
		return nil
	}
	return &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       syscall.AF_INET,
		State:        netlink.NUD_PERMANENT,
		IP:           ip,
		HardwareAddr: routerMAC,
	}
}

func (o *OverlayTier) buildType5GatewayFDB(vtep net.IP, routerMAC net.HardwareAddr) *netlink.Neigh {
	if o.cfg == nil {
		return nil
	}
	vxLink, err := o.netlinkOps.LinkByName(o.vxlanName())
	if err != nil {
		o.log.Warn("cannot find VXLAN for type-5 gateway FDB update", "vxlan", o.vxlanName(), "error", err)
		return nil
	}
	return buildType5GatewayFDB(vxLink, vtep, routerMAC)
}

func buildType5GatewayFDB(vxLink netlink.Link, vtep net.IP, routerMAC net.HardwareAddr) *netlink.Neigh {
	if vxLink == nil || len(routerMAC) != ethernetMACLength {
		return nil
	}
	ip := vtep.To4()
	if ip == nil {
		return nil
	}
	return &netlink.Neigh{
		LinkIndex:    vxLink.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		State:        netlink.NUD_PERMANENT,
		Flags:        netlink.NTF_SELF,
		IP:           ip,
		HardwareAddr: routerMAC,
	}
}

// handleType2Route installs or removes an FDB entry for a remote MAC learned
// via EVPN Type-2 (MAC/IP Advertisement) route. Only active when EnableL2 is set.
func (o *OverlayTier) handleType2Route(route *apipb.EVPNMACIPAdvertisementRoute, vtep string, withdraw bool) {
	mac, err := net.ParseMAC(route.GetMacAddress())
	if err != nil {
		o.log.Debug("type-2 route with invalid MAC", "mac", route.GetMacAddress(), "error", err)
		return
	}

	macStr := mac.String()

	if withdraw && vtep == "" {
		if stored, ok := o.macVTEP.Load(macStr); ok {
			if storedVTEP, ok := stored.(string); ok {
				vtep = storedVTEP
			}
		} else {
			o.log.Debug("type-2 withdraw with no tracked VTEP", "mac", macStr)
			return
		}
	}

	remoteIP := net.ParseIP(vtep)
	if remoteIP == nil {
		o.log.Debug("type-2 route with no valid next-hop", "vtep", vtep)
		return
	}

	if vtep == o.cfg.RouterID {
		return
	}

	vxlanName := o.vxlanName()
	vxLink, err := o.netlinkOps.LinkByName(vxlanName)
	if err != nil {
		o.log.Warn("cannot find VXLAN for FDB update", "vxlan", vxlanName, "error", err)
		return
	}

	fdb := &netlink.Neigh{
		LinkIndex:    vxLink.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		HardwareAddr: mac,
		IP:           remoteIP,
		Flags:        netlink.NTF_SELF,
		State:        netlink.NUD_PERMANENT,
	}

	if withdraw {
		if err := o.netlinkOps.NeighDel(fdb); err != nil {
			o.log.Debug("failed to delete FDB entry", "mac", mac, "vtep", vtep, "error", err)
		} else {
			o.log.Info("removed FDB entry from type-2 withdraw", "mac", mac, "vtep", vtep)
		}
		o.macVTEP.Delete(macStr)
		return
	}

	if err := o.netlinkOps.NeighSet(fdb); err != nil {
		o.log.Debug("failed to add/update FDB entry", "mac", mac, "vtep", vtep, "error", err)
	} else {
		o.log.Info("installed FDB entry from type-2 route", "mac", mac, "vtep", vtep)
	}
	o.macVTEP.Store(macStr, vtep)
}

// handleType3Route installs or removes a BUM FDB entry for a remote VTEP
// learned via EVPN Type-3 (Inclusive Multicast Ethernet Tag) route.
// Only active when EnableL2 is set.
func (o *OverlayTier) handleType3Route(route *apipb.EVPNInclusiveMulticastEthernetTagRoute, vtep string, withdraw bool) {
	remoteIP := net.ParseIP(route.GetIpAddress())
	if remoteIP == nil {
		remoteIP = net.ParseIP(vtep)
	}
	if remoteIP == nil {
		o.log.Debug("type-3 route with no valid VTEP IP")
		return
	}

	if remoteIP.String() == o.cfg.RouterID {
		return
	}

	vxlanName := o.vxlanName()
	vxLink, err := o.netlinkOps.LinkByName(vxlanName)
	if err != nil {
		o.log.Warn("cannot find VXLAN for BUM update", "vxlan", vxlanName, "error", err)
		return
	}

	fdb := &netlink.Neigh{
		LinkIndex:    vxLink.Attrs().Index,
		Family:       syscall.AF_BRIDGE,
		HardwareAddr: net.HardwareAddr{0, 0, 0, 0, 0, 0},
		IP:           remoteIP,
		Flags:        netlink.NTF_SELF,
		State:        netlink.NUD_PERMANENT,
	}

	if withdraw {
		if err := o.netlinkOps.NeighDel(fdb); err != nil {
			o.log.Debug("failed to delete BUM FDB entry", "vtep", remoteIP, "error", err)
		} else {
			o.log.Info("removed BUM FDB entry from type-3 withdraw", "vtep", remoteIP)
		}
		return
	}

	if err := o.netlinkOps.NeighAppend(fdb); err != nil {
		o.log.Debug("failed to append BUM FDB entry", "vtep", remoteIP, "error", err)
	} else {
		o.log.Info("installed BUM FDB entry from type-3 route", "vtep", remoteIP)
	}
}

// parsePrefixRoute parses a prefix string and length into a *net.IPNet.
func parsePrefixRoute(prefix string, prefixLen uint32) (*net.IPNet, error) {
	ip := net.ParseIP(prefix)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP %q", prefix)
	}

	var mask net.IPMask
	if ip.To4() != nil {
		if prefixLen > 32 {
			return nil, fmt.Errorf("invalid IPv4 prefix length %d", prefixLen)
		}
		mask = net.CIDRMask(int(prefixLen), 32)
	} else {
		if prefixLen > 128 {
			return nil, fmt.Errorf("invalid IPv6 prefix length %d", prefixLen)
		}
		mask = net.CIDRMask(int(prefixLen), 128)
	}

	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
}

// extractNextHop returns the first next-hop IP from a path's MpReachNLRI
// attribute. Returns empty string if not found.
func extractNextHop(p *apipb.Path) string {
	for _, attr := range p.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		if mpReach, ok := msg.(*apipb.MpReachNLRIAttribute); ok {
			if hops := mpReach.GetNextHops(); len(hops) > 0 {
				return hops[0]
			}
		}
	}
	return ""
}

// buildRouteDistinguisher builds an RD, selecting 2-octet or 4-octet ASN type.
func buildRouteDistinguisher(asn, vni uint32) (*anypb.Any, error) {
	var a *anypb.Any
	var err error
	if asn <= asnMax2Byte {
		a, err = anypb.New(&apipb.RouteDistinguisherTwoOctetASN{
			Admin:    asn,
			Assigned: vni,
		})
	} else {
		a, err = anypb.New(&apipb.RouteDistinguisherFourOctetASN{
			Admin:    asn,
			Assigned: vni & 0xFFFF,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("marshal route distinguisher: %w", err)
	}
	return a, nil
}

// buildEVPNType5NLRI builds an EVPN IP Prefix (Type-5) NLRI for the given
// IP prefix (typically a /32 host route), so the fabric can route overlay
// traffic to this VTEP.
func buildEVPNType5NLRI(rd *anypb.Any, provisionIP, gwIP string, label uint32) (*anypb.Any, error) {
	_, ipNet, err := net.ParseCIDR(provisionIP)
	if err != nil {
		return nil, fmt.Errorf("parse provision IP %s: %w", provisionIP, err)
	}

	ones, _ := ipNet.Mask.Size()

	route := &apipb.EVPNIPPrefixRoute{
		Rd: rd,
		Esi: &apipb.EthernetSegmentIdentifier{
			Type:  0,
			Value: make([]byte, 9),
		},
		EthernetTag: 0,
		IpPrefixLen: uint32(ones),
		IpPrefix:    ipNet.IP.String(),
		GwAddress:   gwIP,
		Label:       label,
	}
	a, err := anypb.New(route)
	if err != nil {
		return nil, fmt.Errorf("marshal EVPN type-5 NLRI: %w", err)
	}
	return a, nil
}

// buildType5PathAttrs builds BGP path attributes for EVPN Type-5 advertisement.
func buildType5PathAttrs(nlri *anypb.Any, nextHop string, asn, vni uint32, vpnRT, routerMAC string) ([]*anypb.Any, error) {
	origin, err := anypb.New(&apipb.OriginAttribute{Origin: 0}) // IGP
	if err != nil {
		return nil, fmt.Errorf("marshal origin: %w", err)
	}

	// GoBGP requires MpReachNLRIAttribute to carry both the next-hop and the
	// NLRI list for EVPN address families.
	mpReach, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{nextHop},
		Nlris:    []*anypb.Any{nlri},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal mp-reach: %w", err)
	}

	rt, err := buildRouteTargetForSpec(vpnRT, asn, vni)
	if err != nil {
		return nil, fmt.Errorf("build route target: %w", err)
	}

	encap, err := anypb.New(&apipb.EncapExtended{TunnelType: vxlanTunnelType})
	if err != nil {
		return nil, fmt.Errorf("marshal vxlan encapsulation extended community: %w", err)
	}

	communities := []*anypb.Any{rt, encap}
	if routerMAC != "" {
		rmac, err := buildRouterMACExtended(routerMAC)
		if err != nil {
			return nil, fmt.Errorf("build router MAC extended community: %w", err)
		}
		communities = append(communities, rmac)
	}

	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: communities,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ext-communities: %w", err)
	}

	return []*anypb.Any{origin, mpReach, extComm}, nil
}

const ethernetMACLength = 6

func parseRouterMAC(routerMAC string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(routerMAC)
	if err != nil {
		return nil, fmt.Errorf("parse router MAC %s: %w", routerMAC, err)
	}
	if len(mac) != ethernetMACLength {
		return nil, fmt.Errorf("parse router MAC %s: expected 48-bit MAC, got %d bytes", routerMAC, len(mac))
	}
	return mac, nil
}

func buildRouterMACExtended(routerMAC string) (*anypb.Any, error) {
	mac, err := parseRouterMAC(routerMAC)
	if err != nil {
		return nil, err
	}
	rmac, err := anypb.New(&apipb.RouterMacExtended{Mac: mac.String()})
	if err != nil {
		return nil, fmt.Errorf("marshal router MAC: %w", err)
	}
	return rmac, nil
}

// matchesLocalRT reports whether path carries an extended community Route Target
// matching the given localASN and localVNI. It iterates path attributes looking
// for an ExtendedCommunitiesAttribute and checks each community entry against
// the expected RT value (same logic as buildRouteTarget).
func matchesLocalRT(path *apipb.Path, localASN, localVNI uint32) bool {
	matches, _ := routeTargetMatchState(path, localASN, localVNI)
	return matches
}

func routeUpdateMatchesImportRT(path *apipb.Path, localASN, localVNI uint32) bool {
	matches, hasRouteTarget := routeTargetMatchState(path, localASN, localVNI)
	if path.GetIsWithdraw() && !hasRouteTarget {
		return true
	}
	return matches
}

func routeTargetMatchState(path *apipb.Path, localASN, localVNI uint32) (matches, hasRouteTarget bool) {
	for _, attr := range path.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		extComm, ok := msg.(*apipb.ExtendedCommunitiesAttribute)
		if !ok {
			continue
		}
		rtMatches, rtPresent := rtFoundInCommunities(extComm.GetCommunities(), localASN, localVNI)
		hasRouteTarget = hasRouteTarget || rtPresent
		if rtMatches {
			return true, true
		}
	}
	return false, hasRouteTarget
}

func extractRouterMAC(path *apipb.Path) (net.HardwareAddr, error) {
	for _, attr := range path.GetPattrs() {
		msg, err := attr.UnmarshalNew()
		if err != nil {
			continue
		}
		extComm, ok := msg.(*apipb.ExtendedCommunitiesAttribute)
		if !ok {
			continue
		}
		routerMAC, err := routerMACInCommunities(extComm.GetCommunities())
		if err != nil || routerMAC != nil {
			return routerMAC, err
		}
	}
	return nil, nil
}

func (o *OverlayTier) importRouteTarget() (asn, vni uint32, err error) {
	o.importRTOnce.Do(func() {
		if o.cfg == nil {
			o.importRTErr = fmt.Errorf("missing GoBGP config")
			return
		}
		o.importRTASN, o.importRTVNI, o.importRTErr = o.cfg.importRouteTarget()
	})
	return o.importRTASN, o.importRTVNI, o.importRTErr
}

// rtFoundInCommunities checks a slice of extended community Any values for a
// Route Target matching localASN and localVNI.
func rtFoundInCommunities(communities []*anypb.Any, localASN, localVNI uint32) (matches, hasRouteTarget bool) {
	for _, c := range communities {
		msg, err := c.UnmarshalNew()
		if err != nil {
			continue
		}
		if rtCommunityPresent(msg) {
			hasRouteTarget = true
		}
		if rtCommunityMatches(msg, localASN, localVNI) {
			return true, true
		}
	}
	return false, hasRouteTarget
}

func routerMACInCommunities(communities []*anypb.Any) (net.HardwareAddr, error) {
	for _, c := range communities {
		msg, err := c.UnmarshalNew()
		if err != nil {
			continue
		}
		rmac, ok := msg.(*apipb.RouterMacExtended)
		if !ok {
			continue
		}
		mac, err := parseRouterMAC(rmac.GetMac())
		if err != nil {
			return nil, err
		}
		return mac, nil
	}
	return nil, nil
}

func rtCommunityPresent(msg interface{}) bool {
	const rtSubType = uint32(0x02)
	switch v := msg.(type) {
	case *apipb.TwoOctetAsSpecificExtended:
		return v.GetSubType() == rtSubType
	case *apipb.IPv4AddressSpecificExtended:
		return v.GetSubType() == rtSubType
	case *apipb.IPv6AddressSpecificExtended:
		return v.GetSubType() == rtSubType
	case *apipb.FourOctetAsSpecificExtended:
		return v.GetSubType() == rtSubType
	case *apipb.UnknownExtended:
		return isRawRouteTargetType(v.GetType()) && rawExtendedSubType(v.GetValue()) == rtSubType
	}
	return false
}

func isRawRouteTargetType(t uint32) bool {
	switch t {
	case 0x00, 0x01, 0x02, 0x40, 0x41, 0x42:
		return true
	}
	return false
}

func rawExtendedSubType(value []byte) uint32 {
	if len(value) == 0 {
		return 0
	}
	return uint32(value[0])
}

// rtCommunityMatches returns true if the proto message represents a Route
// Target extended community (SubType 0x02) matching the given ASN and VNI.
func rtCommunityMatches(msg interface{}, localASN, localVNI uint32) bool {
	const rtSubType = uint32(0x02)
	switch v := msg.(type) {
	case *apipb.TwoOctetAsSpecificExtended:
		return v.GetSubType() == rtSubType &&
			v.GetAsn() == localASN &&
			v.GetLocalAdmin() == localVNI
	case *apipb.FourOctetAsSpecificExtended:
		return v.GetSubType() == rtSubType &&
			v.GetAsn() == localASN &&
			localVNI <= routeTargetLocalAdminMax &&
			v.GetLocalAdmin() == localVNI
	}
	return false
}

// buildRouteTarget builds a route target extended community (Type 0x02),
// selecting 2-octet or 4-octet format based on ASN size.
func buildRouteTarget(asn, vni uint32) (*anypb.Any, error) {
	var a *anypb.Any
	var err error
	if asn <= asnMax2Byte {
		a, err = anypb.New(&apipb.TwoOctetAsSpecificExtended{
			IsTransitive: true,
			SubType:      0x02, // Route Target
			Asn:          asn,
			LocalAdmin:   vni,
		})
	} else {
		if vni > routeTargetLocalAdminMax {
			return nil, fmt.Errorf("route target value %d exceeds %d for 4-octet ASN %d", vni, routeTargetLocalAdminMax, asn)
		}
		a, err = anypb.New(&apipb.FourOctetAsSpecificExtended{
			IsTransitive: true,
			SubType:      0x02, // Route Target
			Asn:          asn,
			LocalAdmin:   vni,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("marshal route target: %w", err)
	}
	return a, nil
}

func buildRouteTargetForSpec(spec string, fallbackASN, fallbackVNI uint32) (*anypb.Any, error) {
	if strings.TrimSpace(spec) == "" {
		return buildRouteTarget(fallbackASN, fallbackVNI)
	}
	asn, vni, err := ParseRouteTarget(spec)
	if err != nil {
		return nil, err
	}
	return buildRouteTarget(asn, vni)
}

// vxlanTunnelType is the BGP encapsulation extended-community tunnel type for VXLAN.
const vxlanTunnelType = 8

const type5DirectGateway = "0.0.0.0"

// pmsiTunnelTypeIngressReplication is the PMSI tunnel type for ingress
// replication (RFC 6514 §5). Remote VTEPs replicate BUM frames to this VTEP.
const pmsiTunnelTypeIngressReplication = 6

// buildEVPNType3NLRI builds an EVPN Inclusive Multicast Ethernet Tag (Type-3)
// NLRI. The IMET route tells the fabric to send BUM traffic to this VTEP.
func buildEVPNType3NLRI(rd *anypb.Any, routerID string) (*anypb.Any, error) {
	route := &apipb.EVPNInclusiveMulticastEthernetTagRoute{
		Rd:          rd,
		EthernetTag: 0,
		IpAddress:   routerID,
	}
	a, err := anypb.New(route)
	if err != nil {
		return nil, fmt.Errorf("marshal EVPN type-3 NLRI: %w", err)
	}
	return a, nil
}

// buildType3PathAttrs builds BGP path attributes for EVPN Type-3 (IMET)
// advertisement, including Origin, MpReach, Route Target, and PMSI Tunnel.
func buildType3PathAttrs(nlri *anypb.Any, nextHop string, asn, vni uint32, vpnRT string) ([]*anypb.Any, error) {
	origin, err := anypb.New(&apipb.OriginAttribute{Origin: 0})
	if err != nil {
		return nil, fmt.Errorf("marshal origin: %w", err)
	}

	mpReach, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{nextHop},
		Nlris:    []*anypb.Any{nlri},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal mp-reach: %w", err)
	}

	rt, err := buildRouteTargetForSpec(vpnRT, asn, vni)
	if err != nil {
		return nil, fmt.Errorf("build route target: %w", err)
	}

	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{rt},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ext-communities: %w", err)
	}

	pmsi, err := anypb.New(&apipb.PmsiTunnelAttribute{
		Flags: 0,
		Type:  pmsiTunnelTypeIngressReplication,
		Label: vni,
		Id:    net.ParseIP(nextHop).To4(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal pmsi tunnel: %w", err)
	}

	return []*anypb.Any{origin, mpReach, extComm, pmsi}, nil
}

// buildEVPNType2NLRI builds an EVPN MAC/IP Advertisement (Type-2) NLRI for
// the given MAC address and optional IP. Used to announce the local bridge
// MAC so remote VTEPs learn the FDB entry via control-plane.
func buildEVPNType2NLRI(rd *anypb.Any, mac, ip string, label uint32) (*anypb.Any, error) {
	route := &apipb.EVPNMACIPAdvertisementRoute{
		Rd: rd,
		Esi: &apipb.EthernetSegmentIdentifier{
			Type:  0,
			Value: make([]byte, 9),
		},
		EthernetTag: 0,
		MacAddress:  mac,
		IpAddress:   ip,
		Labels:      []uint32{label},
	}
	a, err := anypb.New(route)
	if err != nil {
		return nil, fmt.Errorf("marshal EVPN type-2 NLRI: %w", err)
	}
	return a, nil
}

// buildType2PathAttrs builds BGP path attributes for EVPN Type-2 (MAC/IP)
// advertisement, including Origin, MpReach, and Route Target.
func buildType2PathAttrs(nlri *anypb.Any, nextHop string, asn, vni uint32, vpnRT string) ([]*anypb.Any, error) {
	origin, err := anypb.New(&apipb.OriginAttribute{Origin: 0})
	if err != nil {
		return nil, fmt.Errorf("marshal origin: %w", err)
	}

	mpReach, err := anypb.New(&apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{nextHop},
		Nlris:    []*anypb.Any{nlri},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal mp-reach: %w", err)
	}

	rt, err := buildRouteTargetForSpec(vpnRT, asn, vni)
	if err != nil {
		return nil, fmt.Errorf("build route target: %w", err)
	}

	extComm, err := anypb.New(&apipb.ExtendedCommunitiesAttribute{
		Communities: []*anypb.Any{rt},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ext-communities: %w", err)
	}

	return []*anypb.Any{origin, mpReach, extComm}, nil
}
