//go:build linux

package gobgp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	exec "github.com/telekom/BOOTy/pkg/executil"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/bfd"
	"github.com/telekom/BOOTy/pkg/network/lldp"
)

// Underlay network constants.
const (
	// nicRetryCount is the number of iterations when waiting for physical NICs.
	nicRetryCount = 20

	// nicRetryInterval is the delay between NIC detection attempts.
	nicRetryInterval = 500 * time.Millisecond

	// bgpProbeTimeout is a best-effort TCP probe timeout used to prefer an
	// actual BGP speaker when a link has multiple IPv6 link-local neighbors.
	bgpProbeTimeout = 250 * time.Millisecond

	// raInterval is the period between Router Advertisement bursts.
	raInterval = 10 * time.Second

	// raPacketSize is the RA packet capacity: 16-byte header + 8-byte SLLAO.
	raPacketSize = 24

	// icmpv6TypeRA is the ICMPv6 Router Advertisement message type (RFC 4861 §4.2).
	icmpv6TypeRA = 134

	// defaultHopLimit is the default IPv6 Cur Hop Limit advertised in RAs.
	defaultHopLimit = 64

	// raLifetimeHi and raLifetimeLo encode the router lifetime of 1800s
	// in big-endian format (0x0708 = 1800).
	raLifetimeHi = 0x07
	raLifetimeLo = 0x08

	// mandatedHopLimit is the hop limit mandated by RFC 4861 §6.1.2.
	// FRR's zebra silently drops RAs with any other value.
	mandatedHopLimit = 255
)

// UnderlayTier manages BGP peering for VXLAN reachability.
// Depending on PeerMode it establishes unnumbered (link-local),
// numbered (explicit IP), or a combination of both session types.
type UnderlayTier struct {
	bgp          *server.BgpServer
	cfg          *Config
	nics         []string
	log          *slog.Logger
	stopRA       chan struct{}                                                           // signals the periodic RA goroutine to stop
	stopFIB      context.CancelFunc                                                      // stops the FIB writer
	fibDone      chan struct{}                                                           // closed when the FIB writer has removed its routes
	peerDevs     map[string]string                                                       // unnumbered peer address -> interface
	discoverPeer func(context.Context, string) (string, error)                           // overridable in tests; nil = discoverLinkLocalPeer
	connected    func(netip.Addr, string) bool                                           // overridable in tests; nil = connectedInVRF
	resetBGP     func(context.Context, string, string)                                   // overridable in tests; nil = resetPeer
	bgpSession   func(context.Context, string) (apipb.PeerState_SessionState, time.Time) // overridable in tests; nil = sessionState
	now          func() time.Time                                                        // overridable in tests; nil = time.Now
	ifacePeers   map[string]string                                                       // interface -> unnumbered peer address as added
	// ifacePeerConfs are the GoBGP peers as added, by interface, to move
	// to a new link-local address of the switch.
	ifacePeerConfs map[string]*apipb.Peer
	// peerMu guards peerDevs, ifacePeers and ifacePeerConfs after Setup,
	// when the relearner changes them.
	peerMu sync.RWMutex
	// peerConfs are all GoBGP peers as added, by GoBGP address, to add
	// a peer again (peerRestarter). peerOps guards it and serializes the
	// peer moves and restarts.
	peerConfs    map[string]*apipb.Peer
	peerOps      sync.Mutex
	stopRestart  func()                        // stops the peer restarter
	stopRelearn  context.CancelFunc            // stops the relearner
	relearnDone  chan struct{}                 // closed when the relearner exits
	stopCarrier  chan struct{}                 // stops the carrier watch
	carrierDone  <-chan struct{}               // closed when the carrier watch ended
	raDone       chan struct{}                 // closed when the periodic RA goroutine exits
	peerCountFn  func(ctx context.Context) int // overridable in tests; nil = use countEstablishedPeers
	pollInterval time.Duration                 // overridable in tests; zero = use default (1s)
	bfd          *bfd.Manager                  // BFD sessions of the peers
	bfdUp        sync.Map                      // BFD peer name -> bool, the session is Up
	bfdUpGrace   time.Duration                 // overridable in tests; zero = bfdRecheckGrace
	// checkCtx, canceled by Teardown, bounds the BFD rechecks of
	// starting sessions (recheck); checkMu guards it and checks.Add.
	checkMu    sync.Mutex
	checkCtx   context.Context //nolint:containedctx // canceled by Teardown, like stopRelearn
	stopChecks context.CancelFunc
	checks     sync.WaitGroup
	stopLLDP   func() // stops the LLDP senders and waits for them
}

const defaultUnderlayDummyName = "dummy.underlay"

// NewUnderlayTier creates a new underlay tier.
func NewUnderlayTier(cfg *Config) *UnderlayTier {
	return &UnderlayTier{
		cfg: cfg,
		log: slog.With("tier", "underlay"),
	}
}

// BgpServer returns the shared BGP server for the overlay tier.
func (u *UnderlayTier) BgpServer() *server.BgpServer {
	return u.bgp
}

// NICs returns the detected physical NICs.
func (u *UnderlayTier) NICs() []string {
	return u.nics
}

// Setup creates the underlay loopback, detects NICs (when needed), starts the
// BGP server, and adds peers according to the configured PeerMode.
func (u *UnderlayTier) Setup(ctx context.Context) error {
	switch UnderlayAF(u.cfg.UnderlayAF) {
	case AFIPv4:
		// default — continue with IPv4 underlay
	case AFIPv6:
		return fmt.Errorf("underlay address family %q is not yet implemented", u.cfg.UnderlayAF)
	case AFDualStack:
		return fmt.Errorf("underlay address family %q is not yet implemented", u.cfg.UnderlayAF)
	}

	if err := u.createUnderlayDummy(); err != nil {
		return fmt.Errorf("create underlay dummy: %w", err)
	}

	// Unnumbered and dual modes need physical NICs for interface-based peering.
	// Numbered mode uses explicit IP addresses so NIC detection and link-up
	// are not required — but MTU must be set on physical NICs in all modes
	// to avoid VXLAN fragmentation (50-byte overhead).
	if err := u.setupNICs(ctx); err != nil {
		return err
	}

	if err := enableForwarding(u.log, u.cfg.IPForwarding, u.cfg.IPv6Forwarding); err != nil {
		return fmt.Errorf("enable forwarding: %w", err)
	}
	u.disableAcceptRA()
	u.startLLDP()
	if err := u.acceptVRFConnections(); err != nil {
		return err
	}

	if err := u.startBgpServer(ctx); err != nil {
		return fmt.Errorf("start BGP server: %w", err)
	}

	if err := u.setupBGP(ctx); err != nil {
		return err
	}

	u.startBFD()
	if u.cfg.InstallRoutes {
		u.startFIBWriter()
	}
	if u.cfg.FastExternalFailover {
		u.startCarrierWatch()
	}
	u.startRelearn()
	if err := u.startRestarter(); err != nil {
		u.log.Warn("a BGP peer waits 5 s in Idle after its session ends", "error", err)
	}

	// Start periodic Router Advertisements so FRR's zebra continuously
	// knows our link-local addresses for BGP unnumbered peering.
	if u.needsNICs() && len(u.nics) > 0 {
		u.startPeriodicRA()
	}

	return nil
}

func (u *UnderlayTier) setupNICs(ctx context.Context) error {
	if !u.needsNICs() {
		u.setNICMTU()
		return nil
	}
	nics, err := u.waitForNICs(ctx)
	if err != nil {
		return fmt.Errorf("detect NICs: %w", err)
	}
	if len(nics) == 0 {
		return fmt.Errorf("no physical NICs detected")
	}
	u.nics = nics
	u.log.Info("Detected physical NICs", "nics", nics)

	if err := u.configureNICs(); err != nil {
		return fmt.Errorf("configure NICs: %w", err)
	}
	return nil
}

// Ready waits until at least MinEstablishedPeers BGP peers reach ESTABLISHED state.
func (u *UnderlayTier) Ready(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	interval := u.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	countPeers := u.countEstablishedPeers
	if u.peerCountFn != nil {
		countPeers = u.peerCountFn
	}

	minPeers := u.cfg.MinEstablishedPeers
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled: %w", ctx.Err())
		case <-timer.C:
			return fmt.Errorf("timed out waiting for %d BGP peer(s) to establish", minPeers)
		case <-ticker.C:
			if n := countPeers(ctx); n >= minPeers {
				u.log.Info("underlay BGP peers established", "count", n, "required", minPeers)
				return nil
			}
		}
	}
}

// Teardown stops the BGP server and removes the underlay dummy interface.
func (u *UnderlayTier) Teardown(_ context.Context) error {
	if u.stopRA != nil {
		close(u.stopRA)
		u.stopRA = nil
	}
	if u.stopRestart != nil {
		u.stopRestart() // no peer restarts after the server stops
		u.stopRestart = nil
	}
	if u.stopRelearn != nil {
		u.stopRelearn()
		<-u.relearnDone // no peer changes after the server stops
		u.stopRelearn = nil
	}
	if u.stopCarrier != nil {
		close(u.stopCarrier)
		<-u.carrierDone // no Disable/EnablePeer after the server stops
		u.stopCarrier = nil
	}
	if u.stopFIB != nil {
		u.stopFIB()
		<-u.fibDone
		u.stopFIB = nil
	}
	if u.bfd != nil {
		u.bfd.Close()
		u.bfd = nil
	}
	u.stopRechecks() // no ResetPeer during the teardown, none waits on the stopped server
	if u.stopLLDP != nil {
		u.stopLLDP()
		u.stopLLDP = nil
	}

	if u.bgp != nil {
		u.bgp.Stop()
		u.log.Info("BGP server stopped")
	}

	// Release NICs from VRF so the VRF itself can be cleaned up.
	for _, nic := range u.nics {
		link, err := netlink.LinkByName(nic)
		if err != nil {
			continue
		}
		if err := netlink.LinkSetNoMaster(link); err != nil {
			u.log.Warn("Failed to release NIC from VRF", "nic", nic, "error", err)
		}
	}

	link, err := netlink.LinkByName(u.underlayDummyName())
	if err == nil {
		if delErr := netlink.LinkDel(link); delErr != nil {
			u.log.Warn("Failed to remove underlay dummy", "error", delErr)
		}
	}

	return nil
}

func (u *UnderlayTier) createUnderlayDummy() error {
	dummyName := u.underlayDummyName()
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: dummyName},
	}
	if err := netlink.LinkAdd(dummy); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add %s: %w", dummyName, err)
	}

	link, err := netlink.LinkByName(dummyName)
	if err != nil {
		return fmt.Errorf("find %s: %w", dummyName, err)
	}

	addr, err := netlink.ParseAddr(u.cfg.RouterID + "/32")
	if err != nil {
		return fmt.Errorf("parse router ID %s: %w", u.cfg.RouterID, err)
	}

	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add addr to %s: %w", dummyName, err)
	}

	// The VXLAN device can bind to the dummy (netplan tunnel "link"); its
	// MTU must fit under the dummy MTU, like the template's "mtu: 9100".
	if u.cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, u.cfg.MTU); err != nil {
			return fmt.Errorf("set %s MTU: %w", dummyName, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", dummyName, err)
	}

	// Assign dummy to VRF for traffic isolation.
	if u.cfg.VRFName != "" {
		vrfLink, err := netlink.LinkByName(u.cfg.VRFName)
		if err != nil {
			return fmt.Errorf("find VRF %s for dummy assignment: %w", u.cfg.VRFName, err)
		}
		if err := netlink.LinkSetMasterByIndex(link, vrfLink.Attrs().Index); err != nil {
			return fmt.Errorf("assign dummy to VRF %s: %w", u.cfg.VRFName, err)
		}
	}

	u.log.Info("Created underlay dummy", "ip", u.cfg.RouterID)
	return nil
}

func (u *UnderlayTier) underlayDummyName() string {
	if name := strings.TrimSpace(u.cfg.UnderlayDummyName); name != "" {
		return name
	}
	return defaultUnderlayDummyName
}

// waitForNICs detects physical NICs, retrying briefly when only one NIC is
// found. In containerlab environments, veth links are created after the
// container starts, so data-plane NICs may not be immediately visible.
func (u *UnderlayTier) waitForNICs(ctx context.Context) ([]string, error) {
	if names := sessionInterfaces(u.cfg.Sessions); len(names) > 0 {
		return u.waitForNamedNICs(ctx, names)
	}
	if len(u.cfg.Interfaces) > 0 {
		return u.waitForNamedNICs(ctx, u.cfg.Interfaces)
	}

	for range nicRetryCount {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for physical NICs: %w", err)
		}
		nics, err := network.DetectPhysicalNICsContext(ctx)
		if err != nil {
			return nil, err
		}
		if len(nics) > 1 {
			return nics, nil
		}
		if err := sleepNICRetry(ctx); err != nil {
			return nil, err
		}
	}
	// Return whatever was found on the last scan (may be only 1 NIC).
	return network.DetectPhysicalNICsContext(ctx)
}

func (u *UnderlayTier) waitForNamedNICs(ctx context.Context, names []string) ([]string, error) {
	var missing []string
	for range nicRetryCount {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for configured BGP interfaces: %w", err)
		}
		missing = missing[:0]
		for _, name := range names {
			if _, err := netlink.LinkByName(name); err != nil {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return append([]string(nil), names...), nil
		}
		if err := sleepNICRetry(ctx); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("configured BGP interface(s) not found: %s", strings.Join(missing, ","))
}

func sleepNICRetry(ctx context.Context) error {
	timer := time.NewTimer(nicRetryInterval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for BGP interface retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (u *UnderlayTier) configureNICs() error {
	for _, nic := range u.nics {
		link, err := netlink.LinkByName(nic)
		if err != nil {
			u.log.Warn("Failed to find NIC", "nic", nic, "error", err)
			continue
		}

		if err := netlink.LinkSetMTU(link, u.cfg.MTU); err != nil {
			u.log.Warn("Failed to set MTU", "nic", nic, "error", err)
		}

		if err := netlink.LinkSetUp(link); err != nil {
			u.log.Warn("Failed to bring up NIC", "nic", nic, "error", err)
		}

		// Assign NIC to VRF for traffic isolation. Skip NICs that carry a
		// default route — those are management interfaces whose connectivity
		// must not be disrupted.
		if u.cfg.VRFName != "" {
			if hasDefaultRoute(link.Attrs().Index) {
				u.log.Info("Skipping VRF assignment for management NIC", "nic", nic)
				continue
			}
			vrfLink, err := netlink.LinkByName(u.cfg.VRFName)
			if err != nil {
				u.log.Warn("Failed to find VRF for NIC assignment", "nic", nic, "vrf", u.cfg.VRFName, "error", err)
			} else if err := netlink.LinkSetMasterByIndex(link, vrfLink.Attrs().Index); err != nil {
				u.log.Warn("Failed to assign NIC to VRF", "nic", nic, "error", err)
			}
		}
	}
	return nil
}

func hasDefaultRoute(linkIndex int) bool {
	routes, err := network.RetryDump(func() ([]netlink.Route, error) { return netlink.RouteList(nil, netlink.FAMILY_V4) })
	if err != nil {
		return true // conservative: treat as management NIC on error
	}
	for i := range routes {
		if routes[i].Dst == nil && routes[i].LinkIndex == linkIndex {
			return true
		}
	}
	return false
}

// setNICMTU sets the MTU on all detected physical NICs without performing
// link-up or VRF assignment. Used in PeerModeNumbered where the OS manages
// interface state but MTU must still account for VXLAN overhead.
func (u *UnderlayTier) setNICMTU() {
	nics, err := network.DetectPhysicalNICs()
	if err != nil {
		u.log.Warn("failed to detect physical NICs for MTU setup", "error", err)
		return
	}
	for _, nic := range nics {
		link, err := netlink.LinkByName(nic)
		if err != nil {
			u.log.Warn("failed to find NIC for MTU", "nic", nic, "error", err)
			continue
		}
		if err := netlink.LinkSetMTU(link, u.cfg.MTU); err != nil {
			u.log.Warn("failed to set MTU", "nic", nic, "mtu", u.cfg.MTU, "error", err)
		}
	}
}

func (u *UnderlayTier) startBgpServer(ctx context.Context) error {
	// GoBGP v4 logs nothing without a logger (v3 logged at info to
	// stderr), so it logs through BOOTy's handler.
	u.bgp = server.NewBgpServer(server.LoggerOption(u.log.With("component", "gobgp"), nil))
	go u.bgp.Serve()

	global := &apipb.Global{
		Asn:        u.cfg.ASN,
		RouterId:   u.cfg.RouterID,
		ListenPort: u.cfg.ListenPort,
	}
	if gr := u.cfg.GracefulRestart; gr != nil && gr.Enabled {
		global.GracefulRestart = &apipb.GracefulRestart{
			Enabled:     true,
			RestartTime: gr.RestartTime,
		}
	}

	if err := u.bgp.StartBgp(ctx, &apipb.StartBgpRequest{
		Global: global,
	}); err != nil {
		u.bgp.Stop()
		u.bgp = nil
		return fmt.Errorf("start BGP: %w", err)
	}

	u.log.Info("BGP server started",
		"asn", u.cfg.ASN,
		"routerID", u.cfg.RouterID,
		"port", u.cfg.ListenPort,
	)

	return nil
}

func (u *UnderlayTier) addPeers(ctx context.Context) error {
	if len(u.cfg.Sessions) > 0 {
		return u.addSessionPeers(ctx)
	}
	var added int
	var lastErr error

	iface, numbered, err := peerModeFamilies(u.cfg.PeerMode)
	if err != nil {
		return err
	}
	if iface != nil {
		added, lastErr = u.addInterfacePeers(ctx, iface())
	}
	if numbered != nil {
		n, err := u.addNumberedPeers(ctx, numbered())
		added += n
		if err != nil {
			lastErr = err
		}
	}

	if added == 0 && lastErr != nil {
		return fmt.Errorf("no BGP peers added: %w", lastErr)
	}
	return nil
}

// setupBGP adds the peers, installs the route policies and announces the
// underlay route.
func (u *UnderlayTier) setupBGP(ctx context.Context) error {
	if err := u.addPeers(ctx); err != nil {
		return fmt.Errorf("add BGP peers: %w", err)
	}
	if err := u.installPolicies(ctx); err != nil {
		return fmt.Errorf("install BGP route policies: %w", err)
	}
	if err := u.announceUnderlayRoute(ctx); err != nil {
		return fmt.Errorf("announce underlay route: %w", err)
	}
	return nil
}

// needsNICs reports whether the underlay peers over physical NICs.
func (u *UnderlayTier) needsNICs() bool {
	if len(u.cfg.Sessions) > 0 {
		return len(sessionInterfaces(u.cfg.Sessions)) > 0
	}
	return u.cfg.PeerMode != network.PeerModeNumbered
}

// startFIBWriter installs the learned unicast routes into the session VRF
// table (the main table without a VRF), like zebra does for bgpd.
// Each session's routes go to the table of its VRF, like FRR keeps one RIB
// per "router bgp N vrf X".
func (u *UnderlayTier) startFIBWriter() {
	// Like zebra with "linkdown" next hops: the kernel skips an ECMP member
	// at once when its NIC loses carrier.
	for _, err := range ignoreLinkdownRoutes("/proc/sys/net", sessionInterfaces(u.cfg.Sessions)) {
		u.log.Warn("set ignore_routes_with_linkdown", "error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.stopFIB = cancel
	u.fibDone = make(chan struct{})
	w := u.buildFIBWriter(u.vrfTable)
	go func() {
		defer close(u.fibDone)
		w.run(ctx)
	}()
}

// buildFIBWriter returns the FIB writer of the sessions, with the zebra
// "ip nht resolve-via-default" of the profile.
func (u *UnderlayTier) buildFIBWriter(vrfTable func(string) int) *fibWriter {
	sel, vrfs := u.fibSelector(vrfTable)
	w := newFIBWriter(u.bgp, sel, vrfs)
	w.resolveViaDefault = u.cfg.ResolveViaDefault
	return w
}

// nativeTables returns the routing tables of the BGP_PEER_MODE peers and
// the VRF devices of those tables. Like FRR, a peer on an interface of a
// VRF belongs to that VRF's "router bgp": the unnumbered peers use the
// table of VRF_NAME, which holds their NICs, and the numbered peers that
// of BGP_NEIGHBOR_VRF. Without a VRF it is the main table.
func nativeTables(cfg *Config, vrfTable func(string) int) (unnumbered, numbered int, vrfs map[int]string) {
	vrfs = map[int]string{}
	unnumbered, numbered = unix.RT_TABLE_MAIN, unix.RT_TABLE_MAIN
	if cfg.VRFName != "" {
		unnumbered = vrfTable(cfg.VRFName)
		vrfs[unnumbered] = cfg.VRFName
	}
	if cfg.NeighborVRF != "" {
		numbered = vrfTable(cfg.NeighborVRF)
		vrfs[numbered] = cfg.NeighborVRF
	}
	return unnumbered, numbered, vrfs
}

// fibSelector builds the route selection of the FIB writer: the table and
// the effective local AS of each neighbor.
func (u *UnderlayTier) fibSelector(vrfTable func(string) int) (sel *fibSelector, vrfs map[int]string) {
	vrfs = map[int]string{}
	tables := map[string]int{"": unix.RT_TABLE_MAIN}
	for i := range u.cfg.Sessions {
		vrf := sessionVRF(&u.cfg.Sessions[i])
		if _, ok := tables[vrf]; !ok {
			t := vrfTable(vrf)
			tables[vrf], vrfs[t] = t, vrf
		}
	}
	unnumbered, numbered := unix.RT_TABLE_MAIN, unix.RT_TABLE_MAIN
	if len(u.cfg.Sessions) == 0 {
		var native map[int]string
		unnumbered, numbered, native = nativeTables(u.cfg, vrfTable)
		maps.Copy(vrfs, native)
	}
	sessionOf := func(neighbor string) *network.BGPSession {
		host, _, _ := strings.Cut(neighbor, "%")
		dev := u.peerDev(neighbor)
		for i := range u.cfg.Sessions {
			s := &u.cfg.Sessions[i]
			if s.Interface && s.Name == dev || !s.Interface && s.Name == host {
				return s
			}
		}
		return nil
	}
	sel = &fibSelector{
		relax:   u.cfg.MultipathRelax,
		peerDev: u.peerDev,
		localAS: func(neighbor string) uint32 {
			if s := sessionOf(neighbor); s != nil {
				return cmp.Or(s.LocalASN, u.cfg.ASN)
			}
			// BGP_LOCAL_AS applies to the native unnumbered peers.
			if u.peerDev(neighbor) != "" && u.cfg.LocalAS != 0 {
				return u.cfg.LocalAS
			}
			return u.cfg.ASN
		},
		table: func(neighbor string) int {
			if s := sessionOf(neighbor); s != nil {
				return tables[sessionVRF(s)]
			}
			if u.peerDev(neighbor) != "" {
				return unnumbered
			}
			return numbered
		},
	}
	return sel, vrfs
}

// ignoreLinkdownRoutes sets ignore_routes_with_linkdown=1 for IPv4 and
// IPv6 on the NICs under the sysctl root (/proc/sys/net).
func ignoreLinkdownRoutes(root string, nics []string) []error {
	var errs []error
	for _, nic := range nics {
		for _, fam := range []string{"ipv4", "ipv6"} {
			path := filepath.Join(root, fam, "conf", nic, "ignore_routes_with_linkdown")
			if err := os.WriteFile(path, []byte("1"), 0o644); err != nil { //nolint:gosec // sysctl path of a session NIC
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// vrfTable returns the routing table of a VRF device; the configured
// underlay table when the device is not there yet.
func (u *UnderlayTier) vrfTable(vrf string) int {
	if link, err := netlink.LinkByName(vrf); err == nil {
		if v, ok := link.(*netlink.Vrf); ok {
			return int(v.Table)
		}
	}
	return int(u.cfg.VRFTableID)
}

// carrierPeers returns the unnumbered peers the carrier watch acts on:
// none with fast-external-failover off globally, else the interface
// sessions with it on. Numbered sessions have no own interface.
func carrierPeers(cfg *Config, ifacePeers map[string]string) map[string]string {
	peers := map[string]string{}
	if !cfg.FastExternalFailover {
		return peers
	}
	if len(cfg.Sessions) == 0 {
		// The unnumbered BGP_PEER_MODE peers.
		maps.Copy(peers, ifacePeers)
		return peers
	}
	for i := range cfg.Sessions {
		s := &cfg.Sessions[i]
		if addr, ok := ifacePeers[s.Name]; ok && s.Interface && s.FastExternalFailover {
			peers[s.Name] = addr
		}
	}
	return peers
}

// startCarrierWatch stops and starts unnumbered sessions with the carrier
// of their interface, like FRR fast-external-failover (on by default; off
// with "no bgp fast-external-failover" or fastExternalFailover: false).
func (u *UnderlayTier) startCarrierWatch() {
	peers := carrierPeers(u.cfg, u.ifacePeers)
	if len(peers) == 0 {
		return
	}
	w := u.newCarrierWatch(peers)
	u.stopCarrier = make(chan struct{})
	done, err := w.start(u.stopCarrier)
	if err != nil {
		u.log.Warn("watch uplink carrier; sessions end only after the hold time", "error", err)
		close(u.stopCarrier)
		u.stopCarrier = nil
		return
	}
	u.carrierDone = done
}

// newCarrierWatch returns the carrier watch of the peers; it acts on the
// current address of each peer, which the relearner may change.
func (u *UnderlayTier) newCarrierWatch(peers map[string]string) *carrierWatch {
	w := newCarrierWatch(u.bgp, peers, u.log)
	w.current = u.ifacePeer
	// Under peerOps, so the peer restarter cannot delete and add a peer
	// between the stop and its own check, which would leave it up on a
	// link without carrier.
	for _, f := range []*func(context.Context, string) error{&w.disable, &w.enable} {
		op := *f
		*f = func(ctx context.Context, addr string) error {
			u.peerOps.Lock()
			defer u.peerOps.Unlock()
			return op(ctx, addr)
		}
	}
	return w
}

// peerDev returns the interface of an unnumbered peer address, with or
// without a zone.
func (u *UnderlayTier) peerDev(neighbor string) string {
	host, zone, _ := strings.Cut(neighbor, "%")
	if zone != "" {
		return zone
	}
	u.peerMu.RLock()
	defer u.peerMu.RUnlock()
	return u.peerDevs[host]
}

// acceptVRFConnections lets peers in a VRF open sessions. The GoBGP
// listener is not bound to a VRF; with tcp_l3mdev_accept the kernel hands
// connections that arrive in a VRF to it (FRR bgpd listens per VRF).
func (u *UnderlayTier) acceptVRFConnections() error {
	if !u.hasVRFSession() {
		return nil
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/tcp_l3mdev_accept", []byte("1"), 0o644); err != nil { //nolint:gosec // sysctl path
		return fmt.Errorf("set tcp_l3mdev_accept: %w", err)
	}
	return nil
}

// hasVRFSession reports a BGP session in a VRF: an frr.conf or native
// session with a VRF, BGP_NEIGHBOR_VRF, or native unnumbered peers on the
// VRF_NAME NICs (FRR puts them into the "router bgp" of that VRF).
func (u *UnderlayTier) hasVRFSession() bool {
	if len(u.cfg.Sessions) == 0 {
		return u.cfg.NeighborVRF != "" || u.cfg.VRFName != "" && u.cfg.PeerMode != network.PeerModeNumbered
	}
	for i := range u.cfg.Sessions {
		if sessionVRF(&u.cfg.Sessions[i]) != "" {
			return true
		}
	}
	return false
}

func sessionInterfaces(sessions []network.BGPSession) []string {
	var names []string
	for i := range sessions {
		if sessions[i].Interface {
			names = append(names, sessions[i].Name)
		}
	}
	return names
}

// addSessionPeers adds every FRR session. Like FRR, a peer that can not
// be added yet does not stop the others; the run fails only when none
// was added.
func (u *UnderlayTier) addSessionPeers(ctx context.Context) error {
	var added int
	var lastErr error
	discover := discoverLinkLocalPeer
	if u.discoverPeer != nil {
		discover = u.discoverPeer
	}
	for i := range u.cfg.Sessions {
		s := &u.cfg.Sessions[i]
		addr := s.Name
		if s.Interface {
			var err error
			if addr, err = discover(ctx, s.Name); err != nil {
				// Like FRR "neighbor PORT interface": the session exists
				// before the switch answers.
				addr = placeholderPeer(s.Name)
				u.log.Warn("no link-local neighbor yet; the peer moves to the switch address when it answers",
					"interface", s.Name, "error", err)
			}
		}
		peer := buildSessionPeer(u.cfg, s, addr)
		if s.Interface {
			u.recordInterfacePeer(s.Name, addr)
			u.ifacePeerConfs[s.Name] = peer
		}
		if err := u.addPeer(ctx, peer); err != nil {
			u.log.Warn("Failed to add BGP peer", "neighbor", s.Name, "error", err)
			lastErr = err
			continue
		}
		added++
		u.log.Info("added BGP peer", "neighbor", s.Name, "address", addr, "vrf", s.VRF,
			"internal", s.Internal, "remoteASN", peer.Conf.PeerAsn, "localASN", s.LocalASN,
			"keepalive", s.KeepaliveTime, "hold", s.HoldTime, "families", len(peer.AfiSafis))
	}
	if added == 0 && lastErr != nil {
		return fmt.Errorf("no BGP peers added: %w", lastErr)
	}
	return nil
}

// addPeer adds a peer to GoBGP and remembers it for the peer restarter.
func (u *UnderlayTier) addPeer(ctx context.Context, peer *apipb.Peer) error {
	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: peer}); err != nil {
		return err //nolint:wrapcheck // the callers name the peer
	}
	u.peerOps.Lock()
	defer u.peerOps.Unlock()
	if u.peerConfs == nil {
		u.peerConfs = map[string]*apipb.Peer{}
	}
	u.peerConfs[peer.GetConf().GetNeighborAddress()] = peer
	return nil
}

// recordInterfacePeer remembers an unnumbered peer for the FIB writer
// (next hop device) and the carrier watch.
func (u *UnderlayTier) recordInterfacePeer(iface, addr string) {
	if u.peerDevs == nil {
		u.peerDevs = map[string]string{}
	}
	host, _, _ := strings.Cut(addr, "%")
	u.peerDevs[host] = iface
	if u.ifacePeers == nil {
		u.ifacePeers = map[string]string{}
	}
	if u.ifacePeerConfs == nil {
		u.ifacePeerConfs = map[string]*apipb.Peer{}
	}
	u.ifacePeers[iface] = addr
}

func (u *UnderlayTier) addInterfacePeers(ctx context.Context, families []*apipb.AfiSafi) (int, error) {
	var added int
	var lastErr error
	for _, nic := range u.nics {
		if err := u.addInterfacePeer(ctx, nic, families); err != nil {
			u.log.Warn("Failed to add unnumbered peer", "nic", nic, "error", err)
			lastErr = err
		} else {
			added++
		}
	}
	return added, lastErr
}

func (u *UnderlayTier) addNumberedPeers(ctx context.Context, families []*apipb.AfiSafi) (int, error) {
	var added int
	var lastErr error
	for _, addr := range u.cfg.NeighborAddrs {
		if err := u.addNumberedPeer(ctx, addr, families); err != nil {
			u.log.Warn("Failed to add numbered peer", "addr", addr, "error", err)
			lastErr = err
		} else {
			added++
		}
	}
	return added, lastErr
}

func (u *UnderlayTier) addInterfacePeer(ctx context.Context, iface string, families []*apipb.AfiSafi) error {
	// GoBGP's AddPeer has a bug where ExtractNeighborAddress is called before
	// SetDefaultNeighborConfigValues, so NeighborInterface is never resolved.
	// Work around by resolving the link-local address ourselves.
	discover := discoverLinkLocalPeer
	if u.discoverPeer != nil {
		discover = u.discoverPeer
	}
	addr, err := discover(ctx, iface)
	if err != nil {
		// A NIC with carrier is cabled to a switch that is not up yet.
		if !hasCarrier(iface) {
			return fmt.Errorf("discover link-local peer on %s: %w", iface, err)
		}
		addr = placeholderPeer(iface)
		u.log.Warn("no link-local neighbor yet; the peer moves to the switch address when it answers",
			"interface", iface, "error", err)
	}

	peer := buildInterfacePeer(u.cfg, iface, addr, families)
	u.recordInterfacePeer(iface, addr)
	u.ifacePeerConfs[iface] = peer

	if u.cfg.AuthPassword != "" {
		u.log.Info("tcp-md5 authentication configured for BGP peer", "interface", iface)
	}

	if err := u.addPeer(ctx, peer); err != nil {
		return fmt.Errorf("add peer on %s: %w", iface, err)
	}

	u.log.Info("added unnumbered BGP peer", "interface", iface, "address", addr)
	return nil
}

func (u *UnderlayTier) addNumberedPeer(ctx context.Context, addr string, families []*apipb.AfiSafi) error {
	peer := buildNumberedPeer(u.cfg, addr, families)

	if u.cfg.AuthPassword != "" {
		u.log.Info("tcp-md5 authentication configured for BGP peer", "address", addr)
	}

	sessionType := "iBGP"
	if peer.Conf.PeerAsn != u.cfg.ASN {
		sessionType = "eBGP"
	}

	if err := u.addPeer(ctx, peer); err != nil {
		return fmt.Errorf("add %s peer %s: %w", sessionType, addr, err)
	}

	u.log.Info("added numbered BGP peer", "address", addr, "type", sessionType, "remoteASN", peer.Conf.PeerAsn)
	return nil
}

func (u *UnderlayTier) countEstablishedPeers(ctx context.Context) int {
	var count int

	fn := func(p *apipb.Peer) {
		if p.GetState().GetSessionState() == apipb.PeerState_SESSION_STATE_ESTABLISHED {
			count++
		}
	}

	if err := u.bgp.ListPeer(ctx, &apipb.ListPeerRequest{}, fn); err != nil {
		u.log.Debug("failed to list peers", "error", err)
		return 0
	}

	return count
}

// announceUnderlayRoute advertises the RouterID /32 via IPv4 unicast so that
// remote peers (e.g. FRR spine) learn the underlay loopback as a BGP route.
func (u *UnderlayTier) announceUnderlayRoute(ctx context.Context) error {
	ip := net.ParseIP(u.cfg.RouterID).To4()
	if ip == nil {
		return fmt.Errorf("invalid router ID %q", u.cfg.RouterID)
	}

	// Use 0.0.0.0 as next-hop so GoBGP replaces it with the session's
	// local address for each peer. For numbered peers this resolves to the
	// interface IPv4 address; for unnumbered peers GoBGP detects the IPv6
	// link-local local address and uses RFC 5549 extended next-hop encoding
	// (MP_REACH_NLRI with IPv6 next-hop for IPv4 unicast).
	err := addPath(u.bgp, &apipb.Path{
		Family: &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST},
		Nlri:   &apipb.NLRI{Nlri: &apipb.NLRI_Prefix{Prefix: &apipb.IPAddressPrefix{PrefixLen: 32, Prefix: u.cfg.RouterID}}},
		Pattrs: []*apipb.Attribute{
			{Attr: &apipb.Attribute_Origin{Origin: &apipb.OriginAttribute{Origin: 0}}}, // IGP
			{Attr: &apipb.Attribute_NextHop{NextHop: &apipb.NextHopAttribute{NextHop: "0.0.0.0"}}},
			{Attr: &apipb.Attribute_AsPath{AsPath: &apipb.AsPathAttribute{}}},
		},
	})
	if err != nil {
		return fmt.Errorf("add underlay route: %w", err)
	}

	u.log.Info("Announced underlay route", "prefix", u.cfg.RouterID+"/32")
	return nil
}

// sendRouterAdvertisement sends an ICMPv6 Router Advertisement (type 134) on
// the interface so that the adjacent FRR switch's zebra learns our link-local
// address and registers it as the BGP unnumbered peer for the interface.
// FRR's BGP unnumbered peering requires zebra to receive an RA from the peer.
// RFC 4861 §6.1.2 mandates Hop Limit = 255; receivers discard RAs with any
// other value.
func sendRouterAdvertisement(iface string) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return
	}

	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(context.Background(), "ip6:ipv6-icmp", "::"+"%"+iface)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort RA

	// Set Hop Limit to 255 as required by RFC 4861 §6.1.2.
	// FRR's zebra silently drops RAs with any other hop limit.
	pc := ipv6.NewPacketConn(conn)
	if err := pc.SetMulticastHopLimit(mandatedHopLimit); err != nil {
		slog.Warn("failed to set multicast hop limit", "iface", iface, "error", err)
		return
	}
	if err := pc.SetHopLimit(mandatedHopLimit); err != nil {
		slog.Warn("failed to set hop limit", "iface", iface, "error", err)
		return
	}

	mac := ifi.HardwareAddr

	// ICMPv6 Router Advertisement (RFC 4861 §4.2):
	//   Type:           134
	//   Code:           0
	//   Checksum:       0 (kernel fills)
	//   Cur Hop Limit:  64
	//   Flags (M|O):    0
	//   Router Lifetime: 1800s (big-endian: 0x0708)
	//   Reachable Time: 0
	//   Retrans Timer:  0
	// Followed by Source Link-Layer Address option (RFC 4861 §4.6.1):
	//   Type:   1 (Source Link-Layer Address)
	//   Length: 1 (in units of 8 octets = 8 bytes total)
	//   Link-Layer Address: 6 bytes MAC
	ra := make([]byte, 0, raPacketSize)
	ra = append(ra,
		icmpv6TypeRA, 0, 0, 0, // type, code, checksum (kernel fills)
		defaultHopLimit,            // cur hop limit
		0,                          // flags
		raLifetimeHi, raLifetimeLo, // router lifetime = 1800s
		0, 0, 0, 0, // reachable time
		0, 0, 0, 0, // retrans timer
		// Source Link-Layer Address option
		1, 1, // type=1, length=1 (8 bytes)
	)
	ra = append(ra, mac...)

	dst := &net.IPAddr{IP: net.ParseIP("ff02::1"), Zone: iface} // all-nodes multicast
	_, _ = conn.WriteTo(ra, dst)
}

// startPeriodicRA starts the RA goroutine. The goroutine gets the stop
// channel as an argument, because Teardown clears the field after closing it.
func (u *UnderlayTier) startPeriodicRA() {
	u.stopRA = make(chan struct{})
	u.raDone = make(chan struct{})
	go func(stop <-chan struct{}, done chan<- struct{}) {
		defer close(done)
		u.sendPeriodicRA(stop)
	}(u.stopRA, u.raDone)
}

// sendPeriodicRA sends Router Advertisements on all NICs every 10 seconds
// until the stop channel is closed. This keeps FRR's zebra informed about
// our link-local addresses so BGP unnumbered sessions can establish and
// remain active.
func (u *UnderlayTier) sendPeriodicRA(stop <-chan struct{}) {
	// Send an initial burst so FRR learns us quickly.
	for _, nic := range u.nics {
		sendRouterAdvertisement(nic)
	}

	ticker := time.NewTicker(raInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			for _, nic := range u.nics {
				sendRouterAdvertisement(nic)
			}
		}
	}
}

// discoverLinkLocalPeer finds the remote peer's link-local IPv6 address on the
// given interface by polling the NDP neighbor table. An ICMPv6 ping to the
// all-nodes multicast group is sent first to trigger neighbor discovery.
func discoverLinkLocalPeer(ctx context.Context, iface string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("discover link-local peer canceled: %w", err)
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", iface, err)
	}

	discoveryTimeout := nicRetryCount * nicRetryInterval
	discoveryCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	// Trigger NDP by pinging the all-nodes multicast address.
	go triggerNDP(discoveryCtx, iface)

	var fallback string
	ticker := time.NewTicker(nicRetryInterval)
	defer ticker.Stop()

	for {
		addrs := findLinkLocalNeighbors(ifi, iface)
		if len(addrs) == 1 {
			return addrs[0], nil
		}
		for _, addr := range addrs {
			if fallback == "" {
				fallback = addr
			}
		}
		if addr := firstReachableBGPPeer(discoveryCtx, addrs); addr != "" {
			return addr, nil
		}

		select {
		case <-discoveryCtx.Done():
			if err := ctx.Err(); err != nil {
				return "", fmt.Errorf("discover link-local peer canceled: %w", err)
			}
			if fallback != "" {
				return fallback, nil
			}
			return "", fmt.Errorf("no IPv6 link-local neighbor found on %s after %s", iface, discoveryTimeout)
		case <-ticker.C:
		}
	}
}

// triggerNDP sends ICMPv6 packets on the interface to populate the NDP
// neighbor table and to announce our presence to adjacent FRR switches.
// It sends both a Router Advertisement (ff02::1) and an echo (ff02::1).
func triggerNDP(ctx context.Context, iface string) {
	// Send RA so FRR's zebra learns our link-local for BGP unnumbered.
	sendRouterAdvertisement(iface)

	// Try raw socket first (requires CAP_NET_RAW).
	lc := net.ListenConfig{}
	conn, err := lc.ListenPacket(ctx, "ip6:ipv6-icmp", "::"+"%"+iface)
	if err != nil {
		// Fallback: use ping6 to send an ICMPv6 echo to all-nodes multicast.
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = exec.CommandContext(pingCtx, "ping", "-6", "-c1", "-W1", "-I", iface, "ff02::1").Run() //nolint:gosec // constant args
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort NDP solicitation

	dst := &net.IPAddr{IP: net.ParseIP("ff02::1"), Zone: iface}
	// ICMPv6 echo request: type=128, code=0, checksum=0 (kernel fills), id=0, seq=1
	msg := []byte{128, 0, 0, 0, 0, 0, 0, 1}
	_, _ = conn.WriteTo(msg, dst)
}

func firstReachableBGPPeer(ctx context.Context, addrs []string) string {
	return firstReachableBGPPeerOnPort(ctx, addrs, "179")
}

func firstReachableBGPPeerOnPort(ctx context.Context, addrs []string, port string) string {
	probeCtx, cancel := context.WithTimeout(ctx, bgpProbeTimeout)
	defer cancel()

	// Every probe sends one answer, so no answer can be lost to a race
	// between "one probe succeeded" and "all probes finished".
	results := make(chan string, len(addrs))
	probe := bgpProbe // probes can outlive this call
	for _, addr := range addrs {
		go func() {
			if probe(probeCtx, addr, port) {
				results <- addr
				return
			}
			results <- ""
		}()
	}
	for range addrs {
		select {
		case addr := <-results:
			if addr != "" {
				return addr
			}
		case <-probeCtx.Done():
			return ""
		}
	}
	return ""
}

// bgpProbe is bgpPortReachable; tests replace it.
var bgpProbe = bgpPortReachable

func bgpPortReachable(ctx context.Context, addr, port string) bool {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp6", net.JoinHostPort(addr, port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// findLinkLocalNeighbors returns non-local link-local IPv6 neighbors on the
// given interface.
func findLinkLocalNeighbors(ifi *net.Interface, iface string) []string {
	neighs, err := network.RetryDump(func() ([]netlink.Neigh, error) { return netlink.NeighList(ifi.Index, netlink.FAMILY_V6) })
	if err != nil {
		return nil
	}

	addrs := make([]string, 0, len(neighs))
	for i := range neighs {
		n := &neighs[i]
		if n.State&netlink.NUD_FAILED != 0 {
			continue
		}
		if !n.IP.IsLinkLocalUnicast() {
			continue
		}
		if isOwnAddress(ifi, n.IP) {
			continue
		}
		addrs = append(addrs, fmt.Sprintf("%s%%%s", n.IP, iface))
	}
	return addrs
}

// isOwnAddress checks if the given IP belongs to the interface.
func isOwnAddress(ifi *net.Interface, ip net.IP) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if parsed, _, _ := net.ParseCIDR(a.String()); ip.Equal(parsed) {
			return true
		}
	}
	return false
}

// disableAcceptRA applies netplan "accept-ra: false" to the NICs.
func (u *UnderlayTier) disableAcceptRA() {
	for _, nic := range u.cfg.NoAcceptRA {
		if err := setAcceptRA(nic, false); err != nil {
			u.log.Warn("accept-ra: false not applied", "nic", nic, "error", err)
		}
	}
}

// startLLDP sends LLDP frames on the NICs with netplan "emit-lldp: true",
// like systemd-networkd.
func (u *UnderlayTier) startLLDP() {
	if len(u.cfg.EmitLLDP) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	info := lldp.SystemInfo()
	info.Profile, info.PortIDFormat, info.Announce = u.cfg.LLDPProfile, u.cfg.LLDPPortIDFormat, u.cfg.LLDPAnnounce
	wait := lldp.Emit(ctx, u.cfg.EmitLLDP, &info, u.log.With("component", "lldp"))
	u.stopLLDP = func() {
		cancel()
		wait()
	}
}
