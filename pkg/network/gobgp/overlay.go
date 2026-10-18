//go:build linux

package gobgp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
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
	if n.Family == syscall.AF_INET6 && isMappedGateway(n.IP) {
		return mappedGatewayNeigh(n, false)
	}
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
	if n.Family == syscall.AF_INET6 && isMappedGateway(n.IP) {
		return mappedGatewayNeigh(n, true)
	}
	if err := netlink.NeighDel(n); err != nil {
		return fmt.Errorf("neigh del: %w", err)
	}
	return nil
}

func (netlinkOverlayOps) RouteReplace(route *netlink.Route) error {
	if isMappedGatewayRoute(route) {
		return mappedGatewayRoute(route, false)
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("route replace: %w", err)
	}
	return nil
}

func (netlinkOverlayOps) RouteDel(route *netlink.Route) error {
	if isMappedGatewayRoute(route) {
		return mappedGatewayRoute(route, true)
	}
	if err := netlink.RouteDel(route); err != nil {
		return fmt.Errorf("route delete: %w", err)
	}
	return nil
}

func isMappedGatewayRoute(route *netlink.Route) bool {
	return route.Dst != nil && route.Dst.IP.To4() == nil && isMappedGateway(route.Gw)
}

// OverlayTier manages EVPN Type-5 routes and VXLAN encapsulation.
// When EnableL2 is set, it also handles Type-2 (MAC/IP) and Type-3
// (Inclusive Multicast) routes for L2 overlay use cases.
type OverlayTier struct {
	bgp             *server.BgpServer
	cfg             *Config
	log             *slog.Logger
	cancel          context.CancelFunc
	netlinkOps      overlayNetlinkOps
	hooks           *overlaySetupHooks
	ensureVTEPRoute func(net.IP) error

	// Track resources created by us for clean teardown.
	createdVRFs     map[string]bool
	createdBridge   bool
	createdVXLAN    bool
	addedLoopbackIP *netlink.Addr
	// addedLoopbackAddrs and blackholes are the netplan lo addresses and
	// FRR blackhole routes this tier installed.
	addedLoopbackAddrs []*netlink.Addr
	blackholes         []*netlink.Route
	gatewayFDB         *netlink.Neigh

	// type2Paths holds the live Type-2 paths of each MAC by route key, and
	// type2FDB the VTEP of the FDB entry installed for the MAC. A MAC can
	// have several routes (MAC-only and MAC/IP, or the old and the new PE
	// of a move), so the FDB entry follows the best of them and goes only
	// with the last one. WatchEvent callbacks may run concurrently.
	type2Mu    sync.Mutex
	type2Paths map[string]map[string]type2Path
	type2FDB   map[string]string
	// type2Stopped is set by Teardown, so a Type-2 callback that is still
	// in flight does not install anything afterwards.
	type2Stopped bool
	// type2Neigh is the MAC of the neighbor entry installed on the bridge
	// for each IP of a Type-2 MAC/IP route, like zebra installs remote
	// neighbors on the SVI of an L2VNI.
	type2Neigh map[string]string
	// type2IPMACs indexes the MACs with a live MAC/IP route of each IP, so
	// an update looks only at the routes of its IP.
	type2IPMACs map[string]map[string]struct{}
	// type5GatewayMAC tracks Type-5 gateway IP → router MAC mappings so
	// withdrawals without extended communities can still delete the neighbor.
	// type5GatewayMu serializes gateway MAC/reference updates and the refcount
	// scan used before deleting shared gateway neighbors.
	type5GatewayMu  sync.Mutex
	type5GatewayMAC sync.Map
	// type5GatewayRefs tracks Type-5 prefix → gateway references so one
	// prefix withdrawal cannot delete a shared gateway neighbor.
	type5GatewayRefs sync.Map
	// type5PathsMu serializes Type-5 route handling. type5Paths maps a
	// kernel route prefix to the live Type-5 paths for it, keyed by NLRI.
	// Two gateways can advertise the same prefix with different RDs. The
	// kernel route uses one of them, so a withdraw falls back to another.
	type5PathsMu sync.Mutex
	type5Paths   map[string]map[string]*type5Path
	// type5Active maps a prefix to the key of the path whose install last
	// succeeded, so withdrawing another path does not touch the route.
	type5Active map[string]string
	// type5Stopped is set by Teardown so a route callback that is still in
	// flight does not touch the kernel after teardown.
	type5Stopped bool

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
	o.type5PathsMu.Lock()
	o.type5Stopped = false
	o.type5PathsMu.Unlock()
	o.type2Mu.Lock()
	o.type2Stopped = false
	o.type2Mu.Unlock()

	if err := o.setupCreateVXLANAndBridge(); err != nil {
		return o.cleanupSetupFailure(ctx, "create VXLAN/bridge", err)
	}

	if err := o.setupAddProvisionIP(); err != nil {
		return o.cleanupSetupFailure(ctx, "add provision IP", err)
	}

	if err := o.addOverlayLoopback(); err != nil {
		return o.cleanupSetupFailure(ctx, "add overlay loopback", err)
	}

	if err := o.installBlackholes(); err != nil {
		return o.cleanupSetupFailure(ctx, "install blackhole routes", err)
	}

	if err := o.advertiseType5(ctx); err != nil {
		return o.cleanupSetupFailure(ctx, "advertise EVPN Type-5", err)
	}

	if err := o.advertiseType5Networks(ctx); err != nil {
		return o.cleanupSetupFailure(ctx, "advertise EVPN Type-5 networks", err)
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

	o.type5PathsMu.Lock()
	o.type5Paths = nil
	o.type5Active = nil
	o.type5Stopped = true
	o.type5PathsMu.Unlock()

	// Remove the FDB entries of the Type-2 routes: a VXLAN device that
	// BOOTy did not create stays, and so would they. A new Setup learns
	// the routes again.
	o.type2Mu.Lock()
	o.type2Stopped = true
	o.removeType2FDB()
	o.removeType2Neighbors()
	o.type2Paths, o.type2FDB, o.type2Neigh, o.type2IPMACs = nil, nil, nil, nil
	o.type2Mu.Unlock()

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

	o.removeLocalState()

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
	if o.cfg.VXLANName != "" {
		return o.cfg.VXLANName
	}
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
	if err := o.applyPortSettings(vxLink); err != nil {
		return err
	}
	if err := o.applyBridgeSettings(brLink); err != nil {
		return err
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
	if o.shouldInstallGatewayFDB() {
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
	vxlan, err := o.newVXLAN(name)
	if err != nil {
		return nil, err
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
	if err := netlink.LinkSetMTU(link, o.vxlanDeviceMTU()); err != nil {
		return nil, fmt.Errorf("set VXLAN MTU: %w", err)
	}
	if err := applyLinkSettings(link, o.cfg.OverlayDevices.VXLANLinkLocal, o.cfg.OverlayDevices.VXLANAcceptRA); err != nil {
		return nil, err
	}
	return link, nil
}

func (o *OverlayTier) newVXLAN(name string) (*netlink.Vxlan, error) {
	srcAddr := net.ParseIP(o.cfg.RouterID)
	vtepDevIndex, err := o.vtepDevIndex()
	if err != nil {
		return nil, err
	}
	vxlan := &netlink.Vxlan{
		LinkAttrs:    netlink.LinkAttrs{Name: name},
		VxlanId:      o.cfg.ProvisionVNI,
		SrcAddr:      srcAddr,
		Port:         vxlanPort,
		Learning:     false,
		VtepDevIndex: vtepDevIndex,
	}
	if err := o.applyVXLANSettings(vxlan); err != nil {
		return nil, err
	}
	return vxlan, nil
}

func (o *OverlayTier) vxlanMTU() int {
	mtu := o.cfg.MTU - vxlanOverhead
	if mtu <= 0 {
		return defaultMTU
	}
	return mtu
}

func (o *OverlayTier) vtepDevIndex() (int, error) {
	linkName := strings.TrimSpace(o.cfg.VXLANLink)
	if linkName == "" && o.cfg.VRFName != "" {
		linkName = strings.TrimSpace(o.cfg.UnderlayDummyName)
		if linkName == "" {
			linkName = defaultUnderlayDummyName
		}
	}
	if linkName == "" {
		return 0, nil
	}

	ops := o.netlinkOps
	if ops == nil {
		ops = netlinkOverlayOps{}
	}
	link, err := ops.LinkByName(linkName)
	if err != nil {
		return 0, fmt.Errorf("find VXLAN underlay link %s: %w", linkName, err)
	}
	if link.Attrs() == nil || link.Attrs().Index <= 0 {
		return 0, fmt.Errorf("VXLAN underlay link %s has invalid index", linkName)
	}
	return link.Attrs().Index, nil
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
	if err := applyLinkSettings(link, o.cfg.OverlayDevices.BridgeLinkLocal, o.cfg.OverlayDevices.BridgeAcceptRA); err != nil {
		return nil, err
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

func (o *OverlayTier) shouldInstallGatewayFDB() bool {
	return !o.cfg.Type5Only && o.cfg.ProvisionGateway != ""
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
	if len(o.cfg.LoopbackAddrs) > 0 {
		// The netplan files define the lo addresses.
		return o.addLoopbackAddrs()
	}
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

	rd := buildRouteDistinguisher(o.cfg.ASN, uint32(o.cfg.ProvisionVNI))

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

	pattrs, err := buildType5PathAttrsRTs(nlri, o.cfg.RouterID, o.cfg.ASN, uint32(o.cfg.ProvisionVNI), o.exportRTSpecs(), o.cfg.BridgeMAC)
	if err != nil {
		return fmt.Errorf("build path attributes: %w", err)
	}

	err = addPath(o.bgp, &apipb.Path{
		Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		Nlri:   nlri,
		Pattrs: pattrs,
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
	rd := buildRouteDistinguisher(o.cfg.ASN, uint32(o.cfg.ProvisionVNI))

	nlri := buildEVPNType3NLRI(rd, o.cfg.RouterID)

	pattrs, err := buildType3PathAttrs(nlri, o.cfg.RouterID, o.cfg.ASN, uint32(o.cfg.ProvisionVNI), o.cfg.VPNRT)
	if err != nil {
		return fmt.Errorf("build type-3 path attributes: %w", err)
	}

	err = addPath(o.bgp, &apipb.Path{
		Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		Nlri:   nlri,
		Pattrs: pattrs,
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

	rd := buildRouteDistinguisher(o.cfg.ASN, uint32(o.cfg.ProvisionVNI))

	nlri := buildEVPNType2NLRI(rd, o.cfg.BridgeMAC, ip.String(), uint32(o.cfg.ProvisionVNI))

	pattrs, err := buildType2PathAttrs(nlri, o.cfg.RouterID, o.cfg.ASN, uint32(o.cfg.ProvisionVNI), o.cfg.VPNRT)
	if err != nil {
		return fmt.Errorf("build type-2 path attributes: %w", err)
	}

	err = addPath(o.bgp, &apipb.Path{
		Family: &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		Nlri:   nlri,
		Pattrs: pattrs,
	})
	if err != nil {
		return fmt.Errorf("add EVPN type-2 path: %w", err)
	}

	o.log.Info("advertised EVPN type-2 MAC/IP route",
		"mac", o.cfg.BridgeMAC, "ip", ip, "vni", o.cfg.ProvisionVNI)
	return nil
}

func (o *OverlayTier) watchRoutes(ctx context.Context) {
	err := o.bgp.WatchEvent(ctx, server.WatchEventMessageCallbacks{
		OnBestPath: func(paths []*apiutil.Path, _ time.Time) {
			for _, native := range paths {
				p, err := apiPath(native)
				if err != nil {
					o.log.Debug("route update skipped", "error", err)
					continue
				}
				o.processRouteUpdate(p)
			}
		},
	}, server.WatchBestPath(true))
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

	if ok, err := o.importsRoute(p); err != nil {
		o.log.Debug("route update skipped: invalid configured import RT", "action", action, "type", fmt.Sprintf("%T", nlri.GetNlri()), "error", err)
		return
	} else if !ok {
		o.log.Debug("route update skipped: RT mismatch", "action", action, "type", fmt.Sprintf("%T", nlri.GetNlri()))
		return
	}

	vtep := extractNextHop(p)

	switch {
	case nlri.GetEvpnIpPrefix() != nil:
		route := nlri.GetEvpnIpPrefix()
		routerMAC, err := extractRouterMAC(p)
		routerMACValid := true
		if err != nil {
			o.log.Debug("type-5 route with invalid router MAC", "error", err)
			routerMACValid = false
		}
		o.handleType5RouteWithRouterMACState(route, vtep, routerMAC, routerMACValid, withdraw)
	case nlri.GetEvpnMacadv() != nil:
		if o.cfg.EnableL2 {
			o.handleType2Update(nlri.GetEvpnMacadv(), vtep, macMobilityOf(p), withdraw)
		}
	case nlri.GetEvpnMulticast() != nil:
		if o.cfg.EnableL2 {
			route := nlri.GetEvpnMulticast()
			if !withdraw && !ingressReplication(p) {
				// Like FRR: flooding only for ingress replication.
				o.log.Debug("type-3 route without ingress replication, no BUM entry", "vtep", route.GetIpAddress())
				withdraw = true
			}
			o.handleType3Route(route, vtep, withdraw)
		}
	default:
		o.log.Debug("route update", "action", action, "type", fmt.Sprintf("%T", nlri.GetNlri()))
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

	// Hold the lock for the whole update, so Teardown waits for an update
	// in flight and later updates do not touch the kernel.
	o.type5PathsMu.Lock()
	defer o.type5PathsMu.Unlock()
	if o.type5Stopped {
		return
	}
	dst, err := parsePrefixRoute(route.GetIpPrefix(), route.GetIpPrefixLen())
	if err != nil {
		o.log.Debug("type-5 route with invalid prefix", "prefix", route.GetIpPrefix(), "len", route.GetIpPrefixLen(), "error", err)
		return
	}
	prefix, key := dst.String(), type5PathKey(route)
	if withdraw {
		// Always processed, so no route of ours stays behind.
		o.withdrawType5Path(prefix, key, route, vtep, routerMAC)
		return
	}
	if o.isBlackhole(prefix) {
		o.log.Debug("type-5 route for a static blackhole prefix, static route wins", "prefix", prefix)
		return
	}
	// Remember the path before any kernel change, so a later withdraw of
	// another path can still fall back to it even if this install fails.
	o.addType5Path(prefix, &type5Path{
		key:       key,
		route:     route,
		vtep:      vtep,
		routerMAC: append(net.HardwareAddr(nil), routerMAC...),
		valid:     routerMACValid,
	})
}

// addType5Path records and installs a path. If the install fails, the
// kernel route may have moved already, so the previously installed path,
// or else another live path, is installed again. The caller holds
// type5PathsMu.
func (o *OverlayTier) addType5Path(prefix string, path *type5Path) {
	o.rememberType5Path(prefix, path.key, path)
	ok, routeChanged := o.applyType5PathDetailed(path)
	if ok {
		o.markType5Active(prefix, path.key)
		return
	}
	if !routeChanged {
		// The kernel route was not touched. Another installed path stays
		// installed. If this was an update of the installed path itself,
		// the recorded path no longer matches the kernel, so forget which
		// path is installed and let a later withdraw delete by prefix.
		if o.type5Active[prefix] == path.key {
			delete(o.type5Active, prefix)
		}
		return
	}
	previous := o.type5Active[prefix]
	delete(o.type5Active, prefix)
	if p := o.type5Paths[prefix][previous]; p != nil && previous != path.key && o.applyType5Path(p) {
		o.markType5Active(prefix, previous)
		return
	}
	others := o.sortedType5Paths(prefix)
	for _, p := range others {
		if p.key != path.key && p.key != previous && o.applyType5Path(p) {
			o.markType5Active(prefix, p.key)
			return
		}
	}
	// No path could be installed completely. With several paths the kernel
	// route may now point at one whose gateway state failed, so remove it.
	// A single path keeps the old behavior.
	if len(others) > 1 {
		if state, ok := o.type5RouteState(path.route, path.vtep, net.ParseIP(path.vtep)); ok {
			o.removeType5Prefix(state)
		}
	}
}

// withdrawType5Path removes a path. If it was the installed one, the route
// moves to a remaining path, or is deleted when none can be installed.
// The caller holds type5PathsMu.
func (o *OverlayTier) withdrawType5Path(prefix, key string, route *apipb.EVPNIPPrefixRoute, vtep string, routerMAC net.HardwareAddr) {
	old, tracked := o.type5Paths[prefix][key]
	installedRoute := route
	if tracked {
		installedRoute = old.route
	}
	remaining := o.forgetType5Path(prefix, key)
	active, known := o.type5Active[prefix]
	if known && active != key && len(remaining) > 0 {
		// An inactive path withdrew. The installed route stays.
		return
	}
	delete(o.type5Active, prefix)
	for _, p := range remaining {
		if o.applyType5Path(p) {
			o.markType5Active(prefix, p.key)
			o.log.Info("type-5 path withdrawn, keeping route via remaining path",
				"dst", prefix, "withdrawn_vtep", vtep, "vtep", p.vtep)
			return
		}
	}
	vtepIP, _ := o.ensureType5VTEP(vtep, true)
	state, ok := o.type5RouteState(route, vtep, vtepIP)
	if !ok {
		return
	}
	if len(remaining) > 0 || (tracked && !known) {
		// A failed fallback or update may have left the kernel route on
		// another gateway, so delete the prefix route whatever its gateway.
		o.removeType5Prefix(state)
		return
	}
	if type5OverlayGateway(installedRoute) != nil {
		// Installed without gateway state: the neighbor entry of the
		// gateway IP belongs to its Type-2 route.
		o.deleteType5KernelRoute(state)
		return
	}
	o.withdrawType5Route(state, routerMAC)
}

// markType5Active records the installed path of a prefix. The caller holds
// type5PathsMu.
func (o *OverlayTier) markType5Active(prefix, key string) {
	if o.type5Active == nil {
		o.type5Active = make(map[string]string)
	}
	o.type5Active[prefix] = key
}

// applyType5Path prepares the VTEP route and installs the kernel route and
// gateway state for one path. The caller holds type5PathsMu.
func (o *OverlayTier) applyType5Path(p *type5Path) bool {
	ok, _ := o.applyType5PathDetailed(p)
	return ok
}

// applyType5PathDetailed is applyType5Path that also reports whether the
// kernel route was replaced, even if the gateway state then failed.
func (o *OverlayTier) applyType5PathDetailed(p *type5Path) (ok, routeChanged bool) {
	vtepIP, ok := o.ensureType5VTEP(p.vtep, false)
	if !ok {
		return false, false
	}
	if gw := type5OverlayGateway(p.route); gw != nil {
		return o.applyType5GatewayIPPath(p, gw, vtepIP)
	}
	state, ok := o.type5RouteState(p.route, p.vtep, vtepIP)
	if !ok {
		return false, false
	}
	return o.installType5RouteDetailed(state, p.routerMAC, p.valid)
}

// type5OverlayGateway returns the gateway IP overlay index of a Type-5
// route, or nil for a route without one (gateway 0.0.0.0 or ::).
func type5OverlayGateway(route *apipb.EVPNIPPrefixRoute) net.IP {
	gw := net.ParseIP(route.GetGwAddress())
	if gw == nil || gw.IsUnspecified() {
		return nil
	}
	return gw
}

// applyType5GatewayIPPath installs a Type-5 route whose gateway IP is its
// overlay index, the way FRR does (RFC 9136 section 3.2): only with
// "enable-resolve-overlay-index" (off by default, like FRR) and only while
// a live Type-2 MAC/IP route of the L2 overlay carries the gateway IP
// (bgpd bgp_evpn_is_gateway_ip_resolved, the path is valid only then). The
// kernel route goes through the gateway on the bridge; the router MAC is
// ignored, and the gateway's neighbor entry comes from that Type-2 route.
// The caller holds type5PathsMu.
func (o *OverlayTier) applyType5GatewayIPPath(p *type5Path, gw, vtepIP net.IP) (ok, routeChanged bool) {
	switch {
	case !o.cfg.ResolveOverlayIndex || !o.cfg.EnableL2:
		o.log.Debug("type-5 route with a gateway IP overlay index not installed: enable-resolve-overlay-index is off or no L2 overlay",
			"prefix", p.route.GetIpPrefix(), "gw", gw)
		return false, false
	case !o.type2HasIP(gw):
		o.log.Debug("type-5 gateway IP not resolved by a type-2 route yet", "prefix", p.route.GetIpPrefix(), "gw", gw)
		return false, false
	}
	state, ok := o.type5RouteState(p.route, p.vtep, vtepIP)
	if !ok {
		return false, false
	}
	return o.installType5RouteDetailed(state, nil, true)
}

// type2HasIP reports a live Type-2 MAC/IP route with ip.
func (o *OverlayTier) type2HasIP(ip net.IP) bool {
	o.type2Mu.Lock()
	defer o.type2Mu.Unlock()
	return len(o.type2IPMACs[ip.String()]) > 0
}

// resolveType5Gateway installs or removes the Type-5 routes whose gateway
// IP overlay index is ip, after a Type-2 route with ip came or went, like
// bgpd re-evaluates the paths of the gateway IP next hop.
func (o *OverlayTier) resolveType5Gateway(ip net.IP) {
	o.type5PathsMu.Lock()
	defer o.type5PathsMu.Unlock()
	if o.type5Stopped {
		return
	}
	for _, prefix := range slices.Sorted(maps.Keys(o.type5Paths)) {
		for _, p := range o.type5Paths[prefix] {
			if type5OverlayGateway(p.route).Equal(ip) {
				o.resolveType5Prefix(prefix, ip)
				break
			}
		}
	}
}

// resolveType5Prefix re-evaluates the paths of a prefix with a path via the
// gateway IP ip. The caller holds type5PathsMu.
func (o *OverlayTier) resolveType5Prefix(prefix string, ip net.IP) {
	active, had := o.type5Active[prefix]
	if p := o.type5Paths[prefix][active]; had && p != nil &&
		(!type5OverlayGateway(p.route).Equal(ip) || o.type2HasIP(ip)) {
		return // the installed path does not change
	}
	for _, p := range o.sortedType5Paths(prefix) {
		if o.applyType5Path(p) {
			o.markType5Active(prefix, p.key)
			return
		}
	}
	if !had {
		return
	}
	delete(o.type5Active, prefix)
	if p := o.type5Paths[prefix][active]; p != nil {
		if state, ok := o.type5RouteState(p.route, p.vtep, net.ParseIP(p.vtep)); ok {
			o.removeType5Prefix(state)
		}
	}
}

type type5RouteState struct {
	dst  *net.IPNet
	gw   net.IP
	vtep net.IP
	link netlink.Link
}

// type5Path is one live Type-5 path for a kernel route prefix. It keeps the
// raw route so a fallback can redo the VTEP route and bridge lookup.
type type5Path struct {
	key       string
	route     *apipb.EVPNIPPrefixRoute
	vtep      string
	routerMAC net.HardwareAddr
	valid     bool
}

// type5PathKey identifies a Type-5 route. RFC 9136 section 3.1 makes RD,
// Ethernet tag, IP prefix length and IP prefix the route key; ESI, gateway
// and label are not part of it. GoBGP keys its RIB the same way
// (EVPNIPPrefixRoute.String), so an update that changes the gateway of the
// same key replaces the old path instead of adding a second one.
func type5PathKey(route *apipb.EVPNIPPrefixRoute) string {
	return fmt.Sprintf("%s|%d|%s/%d", rdKey(route.GetRd()), route.GetEthernetTag(), route.GetIpPrefix(), route.GetIpPrefixLen())
}

// rdKey identifies a route distinguisher in a route key: its protobuf
// encoding, which keeps the RD types apart by their field number (the
// GoBGP v3 keys used the type URL and value of the Any).
func rdKey(rd *apipb.RouteDistinguisher) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(rd) //nolint:errcheck // a valid message always encodes
	return fmt.Sprintf("%x", b)
}

// rememberType5Path records a live path. The caller holds type5PathsMu.
func (o *OverlayTier) rememberType5Path(prefix, key string, path *type5Path) {
	if o.type5Paths == nil {
		o.type5Paths = make(map[string]map[string]*type5Path)
	}
	if o.type5Paths[prefix] == nil {
		o.type5Paths[prefix] = make(map[string]*type5Path)
	}
	o.type5Paths[prefix][key] = path
}

// forgetType5Path removes a withdrawn path and returns the remaining paths
// for the same prefix in a stable order. The caller holds type5PathsMu.
func (o *OverlayTier) forgetType5Path(prefix, key string) []*type5Path {
	paths := o.type5Paths[prefix]
	delete(paths, key)
	if len(paths) == 0 {
		delete(o.type5Paths, prefix)
		return nil
	}
	return o.sortedType5Paths(prefix)
}

// sortedType5Paths returns the live paths of a prefix in a stable order.
// The caller holds type5PathsMu.
func (o *OverlayTier) sortedType5Paths(prefix string) []*type5Path {
	paths := o.type5Paths[prefix]
	sorted := make([]*type5Path, 0, len(paths))
	for _, k := range slices.Sorted(maps.Keys(paths)) {
		sorted = append(sorted, paths[k])
	}
	return sorted
}

func (o *OverlayTier) type5RouteState(route *apipb.EVPNIPPrefixRoute, vtep string, vtepIP net.IP) (type5RouteState, bool) {
	prefix := route.GetIpPrefix()
	prefixLen := route.GetIpPrefixLen()
	dst, err := parsePrefixRoute(prefix, prefixLen)
	if err != nil {
		o.log.Debug("type-5 route with invalid prefix", "prefix", prefix, "len", prefixLen, "error", err)
		return type5RouteState{}, false
	}
	// No gateway, 0.0.0.0 or "::" means the VTEP, like zebra. A gateway
	// that does not parse stops the route.
	gwStr := route.GetGwAddress()
	if gwStr == "" || net.ParseIP(gwStr).IsUnspecified() {
		gwStr = vtep
	}
	gw := net.ParseIP(gwStr)
	if gw != nil {
		gw = type5Gateway(dst, gw)
	}
	if gw == nil {
		o.log.Debug("type-5 route with no valid gateway", "prefix", dst, "gw", gwStr)
		return type5RouteState{}, false
	}
	link, err := o.netlinkOps.LinkByName(o.cfg.BridgeName)
	if err != nil {
		o.log.Warn("cannot find bridge for route install", "bridge", o.cfg.BridgeName, "error", err)
		return type5RouteState{}, false
	}
	return type5RouteState{dst: dst, gw: gw, vtep: vtepIP, link: link}, true
}

func (o *OverlayTier) withdrawType5Route(state type5RouteState, routerMAC net.HardwareAddr) {
	if o.deleteType5KernelRoute(state) {
		o.deleteType5GatewayNeighbor(state.link, state.dst.String(), state.gw, state.vtep, routerMAC)
	}
}

// deleteType5KernelRoute deletes the kernel route of a Type-5 withdraw.
func (o *OverlayTier) deleteType5KernelRoute(state type5RouteState) bool {
	route := buildType5KernelRoute(state.link, state.dst, state.gw, o.overlayRouteTable())
	if err := o.netlinkOps.RouteDel(route); err != nil {
		o.log.Debug("failed to delete route from type-5 withdraw", "dst", state.dst, "gw", state.gw, "error", err)
		return false
	}
	o.log.Info("removed route from type-5 withdraw", "dst", state.dst, "gw", state.gw)
	return true
}

// removeType5Prefix deletes the kernel route for a prefix whatever its
// gateway, and the gateway state of that prefix. It is used when no
// remaining path could be installed completely.
func (o *OverlayTier) removeType5Prefix(state type5RouteState) {
	route := buildType5KernelRoute(state.link, state.dst, nil, o.overlayRouteTable())
	if err := o.netlinkOps.RouteDel(route); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Never installed, for example an unresolved gateway IP route.
			o.log.Debug("no type-5 route to delete", "dst", state.dst)
			o.clearType5GatewayNeighbor(state.link, state.dst.String())
			return
		}
		// Keep the gateway state, the kernel route may still need it.
		o.log.Warn("failed to delete type-5 route after failed fallback", "dst", state.dst, "error", err)
		return
	}
	o.log.Warn("no remaining type-5 path could be installed, removed route", "dst", state.dst)
	o.clearType5GatewayNeighbor(state.link, state.dst.String())
}

// installType5RouteDetailed also reports whether the kernel route was
// replaced, so a caller can tell a failed route from failed gateway state.
func (o *OverlayTier) installType5RouteDetailed(state type5RouteState, routerMAC net.HardwareAddr, valid bool) (ok, routeChanged bool) {
	route := buildType5KernelRoute(state.link, state.dst, state.gw, o.overlayRouteTable())
	if err := o.netlinkOps.RouteReplace(route); err != nil {
		o.log.Warn("failed to install route from type-5", "dst", state.dst, "gw", state.gw, "error", err)
		return false, false
	}
	o.log.Info("installed route from type-5", "dst", state.dst, "gw", state.gw)
	switch {
	case !valid:
		o.updateType5GatewayRefWithoutRouterMAC(state.link, state.dst.String(), state.gw, state.vtep)
	case len(routerMAC) == 0:
		o.clearType5GatewayNeighbor(state.link, state.dst.String())
	default:
		return o.setType5GatewayNeighbor(state.link, state.dst.String(), state.gw, state.vtep, routerMAC), true
	}
	return true, true
}

func (o *OverlayTier) ensureType5VTEP(vtep string, withdraw bool) (net.IP, bool) {
	vtepIP := net.ParseIP(vtep)
	if withdraw {
		return vtepIP, true
	}
	if vtepIP == nil {
		o.log.Debug("type-5 route with no valid VTEP", "vtep", vtep)
		return nil, false
	}
	if o.ensureVTEPRoute != nil {
		if err := o.ensureVTEPRoute(vtepIP); err != nil {
			o.log.Warn("cannot install underlay VTEP route for type-5", "vtep", vtep, "error", err)
			return nil, false
		}
	}
	return vtepIP, true
}

func (o *OverlayTier) setType5GatewayNeighbor(
	link netlink.Link,
	prefix string,
	gw net.IP,
	vtep net.IP,
	routerMAC net.HardwareAddr,
) bool {
	o.type5GatewayMu.Lock()
	defer o.type5GatewayMu.Unlock()

	neigh := buildType5GatewayNeighbor(link, gw, routerMAC)
	if neigh == nil {
		return true // no gateway neighbor for this gateway or router MAC
	}
	fdb := o.buildType5GatewayFDB(vtep, routerMAC)
	if fdb == nil {
		return false
	}
	// Lock order: type5PathsMu, type5GatewayMu, then type2Mu. The
	// reference is stored under type2Mu, so a Type-2 withdraw of gw sees it.
	o.type2Mu.Lock()
	shared := o.type2OwnsNeighbor(gw)
	if !shared {
		if err := o.netlinkOps.NeighSet(neigh); err != nil {
			o.type2Mu.Unlock()
			o.log.Warn("failed to install type-5 gateway neighbor", "ip", gw, "mac", routerMAC, "error", err)
			return false
		}
	}
	if err := o.netlinkOps.NeighSet(fdb); err != nil {
		if !shared {
			if delErr := o.netlinkOps.NeighDel(neigh); delErr != nil {
				o.log.Debug("failed to roll back type-5 gateway neighbor", "ip", gw, "mac", routerMAC, "error", delErr)
			}
		}
		o.type2Mu.Unlock()
		o.log.Warn("failed to install type-5 gateway FDB entry", "vtep", vtep, "mac", routerMAC, "error", err)
		return false
	}
	oldRef, oldOK := o.storeType5GatewayRef(prefix, gw, vtep, routerMAC)
	o.type2Mu.Unlock()
	if oldOK && !sameType5GatewayRef(oldRef, newType5GatewayRef(gw, vtep, routerMAC)) {
		o.deleteType5GatewayRefLocked(link, oldRef)
	}
	o.log.Info("installed type-5 gateway neighbor and FDB entry", "ip", gw, "vtep", vtep, "mac", routerMAC)
	return true
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

func (o *OverlayTier) storeType5GatewayRef(prefix string, gw, vtep net.IP, routerMAC net.HardwareAddr) (type5GatewayRef, bool) {
	ref := newType5GatewayRef(gw, vtep, routerMAC)
	oldRef, oldOK := o.loadType5GatewayRef(prefix)
	o.type5GatewayRefs.Store(prefix, ref)
	o.type5GatewayMAC.Store(ref.gateway, append(net.HardwareAddr(nil), routerMAC...))
	return oldRef, oldOK
}

func (o *OverlayTier) deleteType5GatewayRefLocked(link netlink.Link, ref type5GatewayRef) bool {
	if len(ref.routerMAC) != ethernetMACLength {
		if !o.hasType5GatewayRef(ref.gateway) {
			o.type5GatewayMAC.Delete(ref.gateway)
		}
		return true
	}
	gw := parseGatewayKey(ref.gateway)
	vtep := net.ParseIP(ref.vtep)
	deleted := true
	deleteGateway := !o.hasType5GatewayRef(ref.gateway)
	deleteFDB := !o.hasType5GatewayFDBRef(ref.vtep, ref.routerMAC)
	if deleteGateway {
		o.type2Mu.Lock()
		switch neigh := buildType5GatewayNeighbor(link, gw, ref.routerMAC); {
		case neigh == nil:
			deleted = false
		case o.type2OwnsNeighbor(gw):
			// The entry is the one of a Type-2 route now; it stays.
		default:
			if err := o.netlinkOps.NeighDel(neigh); err != nil {
				o.log.Debug("failed to delete type-5 gateway neighbor", "ip", gw, "mac", ref.routerMAC, "error", err)
				deleted = false
			}
		}
		o.type2Mu.Unlock()
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
		gateway = gatewayKey(gw)
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
	// Like zebra: protocol bgp, metric 20. A connected route of the same
	// prefix (metric 0) stays preferred, and the Type-5 route takes over
	// when the address goes away. With metric 0 an IPv4 Type-5 route
	// replaced the kernel's connected route.
	route.Protocol = unix.RTPROT_BGP
	route.Priority = zebraRouteMetric
	return route
}

func buildType5GatewayNeighbor(link netlink.Link, gw net.IP, routerMAC net.HardwareAddr) *netlink.Neigh {
	if len(routerMAC) != ethernetMACLength {
		return nil
	}
	if isMappedGateway(gw) {
		// IPv6 routes use the IPv4-mapped VTEP as gateway. zebra installs
		// its neighbor entry as "extern_learn NOARP".
		return &netlink.Neigh{
			LinkIndex:    link.Attrs().Index,
			Family:       syscall.AF_INET6,
			State:        netlink.NUD_NOARP,
			Flags:        netlink.NTF_EXT_LEARNED,
			IP:           gw,
			HardwareAddr: routerMAC,
		}
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

// type2Path is one live Type-2 route of a MAC: its VTEP and the sequence
// number of its MAC Mobility extended community (0 without one).
type type2Path struct {
	vtep   net.IP
	seq    uint32
	sticky bool   // the static (sticky) flag of the MAC Mobility community
	ip     net.IP // the IP of a MAC/IP route, nil for a MAC-only route
}

// type2Key is the route key of a Type-2 route (RFC 7432 section 7.2): RD,
// Ethernet Tag, MAC and IP address.
func type2Key(route *apipb.EVPNMACIPAdvertisementRoute) string {
	return fmt.Sprintf("%s|%d|%s|%s", rdKey(route.GetRd()), route.GetEthernetTag(),
		strings.ToLower(route.GetMacAddress()), route.GetIpAddress())
}

// better reports whether a is a better location of the MAC than b (RFC
// 7432 section 15): the higher sequence number in serial number arithmetic
// (RFC 1982), so a wrap around stays a move, else the lower VTEP address.
//
// Like FRR (bgpd/bgp_route.c, bgp_path_info_cmp), a route with the sticky
// flag wins over one without it before the sequence numbers count: RFC
// 7432 section 15.2 only asks to alert the operator in that case. FRR
// compares the sequence numbers as plain integers; BOOTy keeps serial
// number arithmetic because section 15 requires wrap around handling.
func (a type2Path) better(b type2Path) bool {
	if a.sticky != b.sticky {
		return a.sticky
	}
	if d := int32(a.seq - b.seq); d != 0 { //nolint:gosec // serial number arithmetic
		return d > 0
	}
	return bytes.Compare(a.vtep.To16(), b.vtep.To16()) < 0
}

// handleType2Route installs, moves or removes the FDB entry of a remote MAC
// learned via EVPN Type-2 (MAC/IP Advertisement) routes. Only active when
// EnableL2 is set. The entry points to the best live route of the MAC, so
// the withdraw of a MAC/IP route keeps the entry of the MAC-only route
// (RFC 7432 section 10) and a stale route of the old PE does not move the
// MAC back (section 15).
func (o *OverlayTier) handleType2Route(route *apipb.EVPNMACIPAdvertisementRoute, vtep string, seq uint32, withdraw bool) {
	o.handleType2Update(route, vtep, macMobility{seq: seq}, withdraw)
}

// removeType2FDB deletes the FDB entries of the Type-2 routes. The caller
// holds type2Mu.
func (o *OverlayTier) removeType2FDB() {
	if len(o.type2FDB) == 0 || o.netlinkOps == nil {
		return
	}
	vxLink, err := o.netlinkOps.LinkByName(o.vxlanName())
	if err != nil {
		return // the device is gone, and its FDB with it
	}
	for macStr, vtep := range o.type2FDB {
		mac, _ := net.ParseMAC(macStr)
		fdb := &netlink.Neigh{LinkIndex: vxLink.Attrs().Index, Family: syscall.AF_BRIDGE, HardwareAddr: mac,
			IP: net.ParseIP(vtep), Flags: netlink.NTF_SELF, State: netlink.NUD_PERMANENT}
		if err := o.netlinkOps.NeighDel(fdb); err != nil {
			o.log.Debug("failed to delete FDB entry at teardown", "mac", macStr, "vtep", vtep, "error", err)
		}
	}
}

// macMobility is the MAC Mobility extended community of a Type-2 route.
type macMobility struct {
	seq    uint32
	sticky bool
}

// handleType2Update is handleType2Route with the whole MAC Mobility
// community.
func (o *OverlayTier) handleType2Update(route *apipb.EVPNMACIPAdvertisementRoute, vtep string, mm macMobility, withdraw bool) {
	mac, err := net.ParseMAC(route.GetMacAddress())
	if err != nil {
		o.log.Debug("type-2 route with invalid MAC", "mac", route.GetMacAddress(), "error", err)
		return
	}
	macStr := mac.String()
	key := type2Key(route)

	remoteIP := net.ParseIP(vtep)
	if !withdraw && remoteIP == nil {
		o.log.Debug("type-2 route with no valid next-hop", "vtep", vtep)
		return
	}
	if !withdraw && vtep == o.cfg.RouterID {
		return
	}

	ip := net.ParseIP(route.GetIpAddress())
	if ip != nil && ip.IsUnspecified() {
		ip = nil
	}

	o.type2Mu.Lock()
	if o.type2Stopped {
		o.type2Mu.Unlock()
		return
	}
	if withdraw {
		o.forgetType2Path(macStr, key, ip)
	} else {
		o.rememberType2Path(macStr, key, type2Path{vtep: remoteIP, seq: mm.seq, sticky: mm.sticky, ip: ip})
	}
	o.syncType2FDB(mac)
	if ip != nil {
		o.syncType2Neighbor(ip)
	}
	o.type2Mu.Unlock()
	if ip != nil && o.cfg.ResolveOverlayIndex {
		// Outside type2Mu: the Type-5 handler takes type5PathsMu first.
		o.resolveType5Gateway(ip)
	}
}

// syncType2Neighbor points the neighbor entry of ip on the bridge at the MAC
// of the best live MAC/IP route with that IP, or removes it when none is
// left. zebra does the same for the remote MAC/IP routes of an L2VNI with an
// SVI (state NOARP, extern_learn), and RFC 9135 sections 5.2 and 6.2 ask an
// asymmetric IRB receiver to install the (IP, MAC) association. The caller
// holds type2Mu.
func (o *OverlayTier) syncType2Neighbor(ip net.IP) {
	ipStr := ip.String()
	bestMAC := o.bestType2MAC(ip)
	installed := o.type2Neigh[ipStr]
	if bestMAC == installed {
		return
	}
	link, err := o.netlinkOps.LinkByName(o.cfg.BridgeName)
	if err != nil {
		o.log.Warn("cannot find bridge for neighbor update", "bridge", o.cfg.BridgeName, "error", err)
		return
	}
	if bestMAC == "" {
		delete(o.type2Neigh, ipStr)
		// The kernel keys the entry by IP only: when a Type-5 route still
		// uses ip as its gateway, its entry comes back instead.
		if rmac := o.type5GatewayNeighborMAC(ip); rmac != nil {
			if err := o.netlinkOps.NeighSet(buildType5GatewayNeighbor(link, ip.To4(), rmac)); err != nil {
				o.log.Warn("failed to restore type-5 gateway neighbor", "ip", ip, "mac", rmac, "error", err)
			}
			return
		}
		mac, _ := net.ParseMAC(installed)
		if err := o.netlinkOps.NeighDel(type2Neighbor(link, ip, mac)); err != nil {
			o.log.Debug("failed to delete type-2 neighbor", "ip", ip, "mac", installed, "error", err)
		}
		return
	}
	mac, _ := net.ParseMAC(bestMAC)
	if err := o.netlinkOps.NeighSet(type2Neighbor(link, ip, mac)); err != nil {
		o.log.Debug("failed to install type-2 neighbor", "ip", ip, "mac", bestMAC, "error", err)
		return
	}
	o.log.Info("installed neighbor from type-2 route", "ip", ip, "mac", bestMAC)
	if o.type2Neigh == nil {
		o.type2Neigh = map[string]string{}
	}
	o.type2Neigh[ipStr] = bestMAC
}

// bestType2MAC returns the MAC of the best live MAC/IP route with ip, ""
// for none. Equal routes (a duplicate IP from one VTEP with the same
// sequence number) take the lowest MAC, so map order does not move the
// entry. The caller holds type2Mu.
func (o *OverlayTier) bestType2MAC(ip net.IP) string {
	var best *type2Path
	var bestMAC string
	for mac := range o.type2IPMACs[ip.String()] {
		for _, p := range o.type2Paths[mac] {
			if p.ip.Equal(ip) && (best == nil || p.better(*best) || (!best.better(p) && mac < bestMAC)) {
				best, bestMAC = &p, mac
			}
		}
	}
	return bestMAC
}

// type2Neighbor is the bridge neighbor entry of a remote MAC/IP route,
// installed like zebra: NOARP and extern_learn.
func type2Neighbor(link netlink.Link, ip net.IP, mac net.HardwareAddr) *netlink.Neigh {
	family := syscall.AF_INET6
	if ip.To4() != nil {
		family, ip = syscall.AF_INET, ip.To4()
	}
	return &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       family,
		State:        netlink.NUD_NOARP,
		Flags:        netlink.NTF_EXT_LEARNED,
		IP:           ip,
		HardwareAddr: mac,
	}
}

// type5GatewayNeighborMAC returns the router MAC of the Type-5 gateway
// neighbor entry for an IPv4 ip, nil when no Type-5 route uses it. The
// caller holds type2Mu; the Type-5 side updates its references under
// type2Mu too.
func (o *OverlayTier) type5GatewayNeighborMAC(ip net.IP) net.HardwareAddr {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	key := gatewayKey(ip4)
	if !o.hasType5GatewayRef(key) {
		return nil
	}
	if v, ok := o.type5GatewayMAC.Load(key); ok {
		if mac, ok := v.(net.HardwareAddr); ok && len(mac) == ethernetMACLength {
			return mac
		}
	}
	return nil
}

// type2OwnsNeighbor reports a Type-2 neighbor entry for the Type-5 gateway
// gw. Both owners use the same kernel entry (bridge and IP, NeighDel
// ignores the MAC), and the Type-2 one, a real IP to MAC binding, wins.
// An IPv4-mapped gateway is an IPv6 entry of its own. The caller holds
// type2Mu.
func (o *OverlayTier) type2OwnsNeighbor(gw net.IP) bool {
	if isMappedGateway(gw) {
		return false
	}
	_, ok := o.type2Neigh[gw.String()]
	return ok
}

// removeType2Neighbors deletes the neighbor entries of the Type-2 routes.
// The caller holds type2Mu.
func (o *OverlayTier) removeType2Neighbors() {
	if len(o.type2Neigh) == 0 || o.netlinkOps == nil {
		return
	}
	link, err := o.netlinkOps.LinkByName(o.cfg.BridgeName)
	if err != nil {
		return
	}
	for ipStr, macStr := range o.type2Neigh {
		mac, _ := net.ParseMAC(macStr)
		if err := o.netlinkOps.NeighDel(type2Neighbor(link, net.ParseIP(ipStr), mac)); err != nil {
			o.log.Debug("failed to delete type-2 neighbor", "ip", ipStr, "error", err)
		}
	}
}

// rememberType2Path records a live Type-2 path. The caller holds type2Mu.
func (o *OverlayTier) rememberType2Path(mac, key string, p type2Path) {
	if o.type2Paths == nil {
		o.type2Paths = map[string]map[string]type2Path{}
	}
	if o.type2Paths[mac] == nil {
		o.type2Paths[mac] = map[string]type2Path{}
	}
	o.type2Paths[mac][key] = p
	if p.ip == nil {
		return
	}
	if o.type2IPMACs == nil {
		o.type2IPMACs = map[string]map[string]struct{}{}
	}
	ipStr := p.ip.String()
	if o.type2IPMACs[ipStr] == nil {
		o.type2IPMACs[ipStr] = map[string]struct{}{}
	}
	o.type2IPMACs[ipStr][mac] = struct{}{}
}

// forgetType2Path removes a withdrawn Type-2 path, and the MAC from the
// index of ip when no other route of the MAC has ip. The caller holds
// type2Mu.
func (o *OverlayTier) forgetType2Path(mac, key string, ip net.IP) {
	delete(o.type2Paths[mac], key)
	if ip == nil {
		return
	}
	for _, p := range o.type2Paths[mac] {
		if p.ip.Equal(ip) {
			return
		}
	}
	ipStr := ip.String()
	delete(o.type2IPMACs[ipStr], mac)
	if len(o.type2IPMACs[ipStr]) == 0 {
		delete(o.type2IPMACs, ipStr)
	}
}

// syncType2FDB points the FDB entry of mac at its best live route, or
// removes it when no route is left. The caller holds type2Mu.
func (o *OverlayTier) syncType2FDB(mac net.HardwareAddr) {
	macStr := mac.String()
	var best *type2Path
	for _, p := range o.type2Paths[macStr] {
		if best == nil || p.better(*best) {
			best = &p
		}
	}
	installed := o.type2FDB[macStr]
	if best == nil {
		delete(o.type2Paths, macStr)
	}
	if (best == nil && installed == "") || (best != nil && best.vtep.String() == installed) {
		return
	}

	vxlanName := o.vxlanName()
	vxLink, err := o.netlinkOps.LinkByName(vxlanName)
	if err != nil {
		o.log.Warn("cannot find VXLAN for FDB update", "vxlan", vxlanName, "error", err)
		return
	}
	fdb := func(ip net.IP) *netlink.Neigh {
		return &netlink.Neigh{
			LinkIndex:    vxLink.Attrs().Index,
			Family:       syscall.AF_BRIDGE,
			HardwareAddr: mac,
			IP:           ip,
			Flags:        netlink.NTF_SELF,
			State:        netlink.NUD_PERMANENT,
		}
	}

	if best == nil {
		if err := o.netlinkOps.NeighDel(fdb(net.ParseIP(installed))); err != nil {
			o.log.Debug("failed to delete FDB entry", "mac", mac, "vtep", installed, "error", err)
		} else {
			o.log.Info("removed FDB entry from type-2 withdraw", "mac", mac, "vtep", installed)
		}
		delete(o.type2FDB, macStr)
		return
	}

	if err := o.netlinkOps.NeighSet(fdb(best.vtep)); err != nil {
		o.log.Debug("failed to add/update FDB entry", "mac", mac, "vtep", best.vtep, "error", err)
		return
	}
	o.log.Info("installed FDB entry from type-2 route", "mac", mac, "vtep", best.vtep, "seq", best.seq)
	if o.type2FDB == nil {
		o.type2FDB = map[string]string{}
	}
	o.type2FDB[macStr] = best.vtep.String()
}

// macMobilityOf returns the MAC Mobility extended community of a path: the
// sequence number (0 without one, RFC 7432 section 15) and the sticky flag.
func macMobilityOf(path *apipb.Path) macMobility {
	for _, attr := range path.GetPattrs() {
		for _, c := range attr.GetExtendedCommunities().GetCommunities() {
			if mm := c.GetMacMobility(); mm != nil {
				return macMobility{seq: mm.GetSequenceNum(), sticky: mm.GetIsSticky()}
			}
		}
	}
	return macMobility{}
}

// ingressReplication reports an IMET route whose PMSI Tunnel attribute has
// tunnel type ingress replication. FRR 10.7.1 (bgp_evpn.c,
// evpn_zebra_install) installs head-end replication only for that type;
// for PIM-SM it sets up multicast flooding, which BOOTy does not support,
// and for a missing attribute or any other type it disables flooding to
// the VTEP (RFC 8365 section 9: the receiver uses the advertised tunnel).
func ingressReplication(path *apipb.Path) bool {
	for _, attr := range path.GetPattrs() {
		if pmsi := attr.GetPmsiTunnel(); pmsi != nil {
			return pmsi.GetType() == pmsiTunnelTypeIngressReplication
		}
	}
	return false
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
		if hops := attr.GetMpReach().GetNextHops(); len(hops) > 0 {
			return hops[0]
		}
	}
	return ""
}

// buildRouteDistinguisher builds an RD, selecting 2-octet or 4-octet ASN type.
func buildRouteDistinguisher(asn, vni uint32) *apipb.RouteDistinguisher {
	if asn <= asnMax2Byte {
		return &apipb.RouteDistinguisher{Rd: &apipb.RouteDistinguisher_TwoOctetAsn{TwoOctetAsn: &apipb.RouteDistinguisherTwoOctetASN{
			Admin:    asn,
			Assigned: vni,
		}}}
	}
	return &apipb.RouteDistinguisher{Rd: &apipb.RouteDistinguisher_FourOctetAsn{FourOctetAsn: &apipb.RouteDistinguisherFourOctetASN{
		Admin:    asn,
		Assigned: vni & 0xFFFF,
	}}}
}

// buildEVPNType5NLRI builds an EVPN IP Prefix (Type-5) NLRI for the given
// IP prefix (typically a /32 host route), so the fabric can route overlay
// traffic to this VTEP.
func buildEVPNType5NLRI(rd *apipb.RouteDistinguisher, provisionIP, gwIP string, label uint32) (*apipb.NLRI, error) {
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
	return &apipb.NLRI{Nlri: &apipb.NLRI_EvpnIpPrefix{EvpnIpPrefix: route}}, nil
}

// buildType5PathAttrs builds BGP path attributes for EVPN Type-5 advertisement.
func buildType5PathAttrs(nlri *apipb.NLRI, nextHop string, asn, vni uint32, vpnRT, routerMAC string) ([]*apipb.Attribute, error) {
	return buildType5PathAttrsRTs(nlri, nextHop, asn, vni, []string{vpnRT}, routerMAC)
}

// buildType5PathAttrsRTs is buildType5PathAttrs with one route target per
// spec, like FRR "route-target export" with several values.
func buildType5PathAttrsRTs(nlri *apipb.NLRI, nextHop string, asn, vni uint32, rtSpecs []string, routerMAC string) ([]*apipb.Attribute, error) {
	var communities []*apipb.ExtendedCommunity
	for _, spec := range rtSpecs {
		rt, err := buildRouteTargetForSpec(spec, asn, vni)
		if err != nil {
			return nil, fmt.Errorf("build route target: %w", err)
		}
		communities = append(communities, rt)
	}

	communities = append(communities, &apipb.ExtendedCommunity{Extcom: &apipb.ExtendedCommunity_Encap{Encap: &apipb.EncapExtended{TunnelType: vxlanTunnelType}}})
	if routerMAC != "" {
		rmac, err := buildRouterMACExtended(routerMAC)
		if err != nil {
			return nil, fmt.Errorf("build router MAC extended community: %w", err)
		}
		communities = append(communities, rmac)
	}

	return []*apipb.Attribute{originIGP(), evpnMpReach(nextHop, nlri), extCommunities(communities...)}, nil
}

// originIGP is the ORIGIN attribute IGP.
func originIGP() *apipb.Attribute {
	return &apipb.Attribute{Attr: &apipb.Attribute_Origin{Origin: &apipb.OriginAttribute{Origin: 0}}}
}

// evpnMpReach is the MP_REACH_NLRI attribute of an EVPN route. GoBGP
// requires it to carry both the next hop and the NLRI for EVPN.
func evpnMpReach(nextHop string, nlri *apipb.NLRI) *apipb.Attribute {
	return &apipb.Attribute{Attr: &apipb.Attribute_MpReach{MpReach: &apipb.MpReachNLRIAttribute{
		Family:   &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN},
		NextHops: []string{nextHop},
		Nlris:    []*apipb.NLRI{nlri},
	}}}
}

// extCommunities is the EXTENDED_COMMUNITIES attribute.
func extCommunities(c ...*apipb.ExtendedCommunity) *apipb.Attribute {
	return &apipb.Attribute{Attr: &apipb.Attribute_ExtendedCommunities{ExtendedCommunities: &apipb.ExtendedCommunitiesAttribute{Communities: c}}}
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

func buildRouterMACExtended(routerMAC string) (*apipb.ExtendedCommunity, error) {
	mac, err := parseRouterMAC(routerMAC)
	if err != nil {
		return nil, err
	}
	return &apipb.ExtendedCommunity{Extcom: &apipb.ExtendedCommunity_RouterMac{RouterMac: &apipb.RouterMacExtended{Mac: mac.String()}}}, nil
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
		extComm := attr.GetExtendedCommunities()
		if extComm == nil {
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
		extComm := attr.GetExtendedCommunities()
		if extComm == nil {
			continue
		}
		routerMAC, err := routerMACInCommunities(extComm.GetCommunities())
		if err != nil || routerMAC != nil {
			return routerMAC, err
		}
	}
	return nil, nil
}

// importsRoute reports whether a route update carries an import route
// target. With explicit import route targets (FRR "route-target import")
// any of them imports the route; a withdraw without route targets is
// accepted, as for the single route target.
func (o *OverlayTier) importsRoute(p *apipb.Path) (bool, error) {
	if o.cfg == nil || len(o.cfg.ImportRTs) == 0 {
		asn, vni, err := o.importRouteTarget()
		if err != nil {
			return false, err
		}
		return routeUpdateMatchesImportRT(p, asn, vni), nil
	}
	seen := false
	for _, spec := range o.cfg.ImportRTs {
		asn, val, err := ParseRouteTarget(spec)
		if err != nil {
			return false, fmt.Errorf("invalid import route target: %w", err)
		}
		matches, present := routeTargetMatchState(p, asn, val)
		if matches {
			return true, nil
		}
		seen = seen || present
	}
	return p.GetIsWithdraw() && !seen, nil
}

// exportRTSpecs returns the export route targets: the explicit list, or
// vpn_rt, or ASN:VNI (an empty spec).
func (o *OverlayTier) exportRTSpecs() []string {
	if len(o.cfg.ExportRTs) > 0 {
		return o.cfg.ExportRTs
	}
	return []string{o.cfg.VPNRT}
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

// rtFoundInCommunities checks a slice of extended communities for a Route
// Target matching localASN and localVNI.
func rtFoundInCommunities(communities []*apipb.ExtendedCommunity, localASN, localVNI uint32) (matches, hasRouteTarget bool) {
	for _, c := range communities {
		if rtCommunityPresent(c) {
			hasRouteTarget = true
		}
		if rtCommunityMatches(c, localASN, localVNI) {
			return true, true
		}
	}
	return false, hasRouteTarget
}

func routerMACInCommunities(communities []*apipb.ExtendedCommunity) (net.HardwareAddr, error) {
	for _, c := range communities {
		rmac := c.GetRouterMac()
		if rmac == nil {
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

// rtCommunityPresent reports a Route Target. IPv6 address specific Route
// Targets travel in their own attribute since GoBGP v4.8.0, which BOOTy
// does not import from.
func rtCommunityPresent(c *apipb.ExtendedCommunity) bool {
	const rtSubType = uint32(0x02)
	switch v := c.GetExtcom().(type) {
	case *apipb.ExtendedCommunity_TwoOctetAsSpecific:
		return v.TwoOctetAsSpecific.GetSubType() == rtSubType
	case *apipb.ExtendedCommunity_Ipv4AddressSpecific:
		return v.Ipv4AddressSpecific.GetSubType() == rtSubType
	case *apipb.ExtendedCommunity_FourOctetAsSpecific:
		return v.FourOctetAsSpecific.GetSubType() == rtSubType
	case *apipb.ExtendedCommunity_Unknown:
		return isRawRouteTargetType(v.Unknown.GetType()) && rawExtendedSubType(v.Unknown.GetValue()) == rtSubType
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

// rtCommunityMatches returns true if the extended community is a Route
// Target (SubType 0x02) matching the given ASN and VNI.
func rtCommunityMatches(c *apipb.ExtendedCommunity, localASN, localVNI uint32) bool {
	const rtSubType = uint32(0x02)
	if v := c.GetTwoOctetAsSpecific(); v != nil {
		return v.GetSubType() == rtSubType &&
			v.GetAsn() == localASN &&
			v.GetLocalAdmin() == localVNI
	}
	if v := c.GetFourOctetAsSpecific(); v != nil {
		return v.GetSubType() == rtSubType &&
			v.GetAsn() == localASN &&
			localVNI <= routeTargetLocalAdminMax &&
			v.GetLocalAdmin() == localVNI
	}
	return false
}

// buildRouteTarget builds a route target extended community (Type 0x02),
// selecting 2-octet or 4-octet format based on ASN size.
func buildRouteTarget(asn, vni uint32) (*apipb.ExtendedCommunity, error) {
	if asn <= asnMax2Byte {
		return &apipb.ExtendedCommunity{Extcom: &apipb.ExtendedCommunity_TwoOctetAsSpecific{TwoOctetAsSpecific: &apipb.TwoOctetAsSpecificExtended{
			IsTransitive: true,
			SubType:      0x02, // Route Target
			Asn:          asn,
			LocalAdmin:   vni,
		}}}, nil
	}
	if vni > routeTargetLocalAdminMax {
		return nil, fmt.Errorf("route target value %d exceeds %d for 4-octet ASN %d", vni, routeTargetLocalAdminMax, asn)
	}
	return &apipb.ExtendedCommunity{Extcom: &apipb.ExtendedCommunity_FourOctetAsSpecific{FourOctetAsSpecific: &apipb.FourOctetAsSpecificExtended{
		IsTransitive: true,
		SubType:      0x02, // Route Target
		Asn:          asn,
		LocalAdmin:   vni,
	}}}, nil
}

func buildRouteTargetForSpec(spec string, fallbackASN, fallbackVNI uint32) (*apipb.ExtendedCommunity, error) {
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
func buildEVPNType3NLRI(rd *apipb.RouteDistinguisher, routerID string) *apipb.NLRI {
	return &apipb.NLRI{Nlri: &apipb.NLRI_EvpnMulticast{EvpnMulticast: &apipb.EVPNInclusiveMulticastEthernetTagRoute{
		Rd:          rd,
		EthernetTag: 0,
		IpAddress:   routerID,
	}}}
}

// buildType3PathAttrs builds BGP path attributes for EVPN Type-3 (IMET)
// advertisement, including Origin, MpReach, Route Target, and PMSI Tunnel.
func buildType3PathAttrs(nlri *apipb.NLRI, nextHop string, asn, vni uint32, vpnRT string) ([]*apipb.Attribute, error) {
	rt, err := buildRouteTargetForSpec(vpnRT, asn, vni)
	if err != nil {
		return nil, fmt.Errorf("build route target: %w", err)
	}

	pmsi := &apipb.Attribute{Attr: &apipb.Attribute_PmsiTunnel{PmsiTunnel: &apipb.PmsiTunnelAttribute{
		Flags: 0,
		Type:  pmsiTunnelTypeIngressReplication,
		Label: vni,
		Id:    net.ParseIP(nextHop).To4(),
	}}}

	return []*apipb.Attribute{originIGP(), evpnMpReach(nextHop, nlri), extCommunities(rt), pmsi}, nil
}

// buildEVPNType2NLRI builds an EVPN MAC/IP Advertisement (Type-2) NLRI for
// the given MAC address and optional IP. Used to announce the local bridge
// MAC so remote VTEPs learn the FDB entry via control-plane.
func buildEVPNType2NLRI(rd *apipb.RouteDistinguisher, mac, ip string, label uint32) *apipb.NLRI {
	return &apipb.NLRI{Nlri: &apipb.NLRI_EvpnMacadv{EvpnMacadv: &apipb.EVPNMACIPAdvertisementRoute{
		Rd: rd,
		Esi: &apipb.EthernetSegmentIdentifier{
			Type:  0,
			Value: make([]byte, 9),
		},
		EthernetTag: 0,
		MacAddress:  mac,
		IpAddress:   ip,
		Labels:      []uint32{label},
	}}}
}

// buildType2PathAttrs builds BGP path attributes for EVPN Type-2 (MAC/IP)
// advertisement, including Origin, MpReach, and Route Target.
func buildType2PathAttrs(nlri *apipb.NLRI, nextHop string, asn, vni uint32, vpnRT string) ([]*apipb.Attribute, error) {
	rt, err := buildRouteTargetForSpec(vpnRT, asn, vni)
	if err != nil {
		return nil, fmt.Errorf("build route target: %w", err)
	}
	return []*apipb.Attribute{originIGP(), evpnMpReach(nextHop, nlri), extCommunities(rt)}, nil
}
