//go:build linux

package gobgp

import (
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
	"golang.org/x/net/ipv6"
	"google.golang.org/protobuf/types/known/anypb"

	exec "github.com/telekom/BOOTy/pkg/executil"
	"github.com/telekom/BOOTy/pkg/network"
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
	stopRA       chan struct{}                 // signals the periodic RA goroutine to stop
	raDone       chan struct{}                 // closed when the periodic RA goroutine exits
	peerCountFn  func(ctx context.Context) int // overridable in tests; nil = use countEstablishedPeers
	pollInterval time.Duration                 // overridable in tests; zero = use default (1s)
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
	if u.cfg.PeerMode != network.PeerModeNumbered {
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
	} else {
		u.setNICMTU()
	}

	if err := enableForwarding(u.log); err != nil {
		return fmt.Errorf("enable forwarding: %w", err)
	}

	if err := u.startBgpServer(ctx); err != nil {
		return fmt.Errorf("start BGP server: %w", err)
	}

	if err := u.addPeers(ctx); err != nil {
		return fmt.Errorf("add BGP peers: %w", err)
	}

	if err := u.announceUnderlayRoute(ctx); err != nil {
		return fmt.Errorf("announce underlay route: %w", err)
	}

	// Start periodic Router Advertisements so FRR's zebra continuously
	// knows our link-local addresses for BGP unnumbered peering.
	if u.cfg.PeerMode != network.PeerModeNumbered && len(u.nics) > 0 {
		u.startPeriodicRA()
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
	if len(u.cfg.Interfaces) > 0 {
		return u.waitForConfiguredNICs(ctx)
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

func (u *UnderlayTier) waitForConfiguredNICs(ctx context.Context) ([]string, error) {
	var missing []string
	for range nicRetryCount {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for configured BGP interfaces: %w", err)
		}
		missing = missing[:0]
		for _, name := range u.cfg.Interfaces {
			if _, err := netlink.LinkByName(name); err != nil {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return append([]string(nil), u.cfg.Interfaces...), nil
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
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
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
	u.bgp = server.NewBgpServer()
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
	var added int
	var lastErr error

	switch u.cfg.PeerMode {
	case network.PeerModeUnnumbered:
		added, lastErr = u.addInterfacePeers(ctx, allFamilies())
	case network.PeerModeDual:
		added, lastErr = u.addInterfacePeers(ctx, ipv4Families())
		n, err := u.addNumberedPeers(ctx, allFamilies())
		added += n
		if err != nil {
			lastErr = err
		}
	case network.PeerModeNumbered:
		added, lastErr = u.addNumberedPeers(ctx, allFamilies())
	default:
		return fmt.Errorf("unsupported peer mode: %q", u.cfg.PeerMode)
	}

	if added == 0 && lastErr != nil {
		return fmt.Errorf("no BGP peers added: %w", lastErr)
	}
	return nil
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
	addr, err := discoverLinkLocalPeer(ctx, iface)
	if err != nil {
		return fmt.Errorf("discover link-local peer on %s: %w", iface, err)
	}

	peer := buildInterfacePeer(u.cfg, iface, addr, families)

	if u.cfg.AuthPassword != "" {
		u.log.Info("tcp-md5 authentication configured for BGP peer", "interface", iface)
	}

	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: peer}); err != nil {
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

	if err := u.bgp.AddPeer(ctx, &apipb.AddPeerRequest{Peer: peer}); err != nil {
		return fmt.Errorf("add %s peer %s: %w", sessionType, addr, err)
	}

	u.log.Info("added numbered BGP peer", "address", addr, "type", sessionType, "remoteASN", peer.Conf.PeerAsn)
	return nil
}

func (u *UnderlayTier) countEstablishedPeers(ctx context.Context) int {
	var count int

	fn := func(p *apipb.Peer) {
		if p.GetState().GetSessionState() == apipb.PeerState_ESTABLISHED {
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

	nlri, err := anypb.New(&apipb.IPAddressPrefix{
		PrefixLen: 32,
		Prefix:    u.cfg.RouterID,
	})
	if err != nil {
		return fmt.Errorf("build NLRI: %w", err)
	}

	origin, err := anypb.New(&apipb.OriginAttribute{Origin: 0}) // IGP
	if err != nil {
		return fmt.Errorf("build origin attr: %w", err)
	}

	// Use 0.0.0.0 as next-hop so GoBGP replaces it with the session's
	// local address for each peer. For numbered peers this resolves to the
	// interface IPv4 address; for unnumbered peers GoBGP detects the IPv6
	// link-local local address and uses RFC 5549 extended next-hop encoding
	// (MP_REACH_NLRI with IPv6 next-hop for IPv4 unicast).
	nexthop, err := anypb.New(&apipb.NextHopAttribute{NextHop: "0.0.0.0"})
	if err != nil {
		return fmt.Errorf("build nexthop attr: %w", err)
	}

	aspath, err := anypb.New(&apipb.AsPathAttribute{
		Segments: []*apipb.AsSegment{},
	})
	if err != nil {
		return fmt.Errorf("build as-path attr: %w", err)
	}

	_, err = u.bgp.AddPath(ctx, &apipb.AddPathRequest{
		Path: &apipb.Path{
			Family: &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: []*anypb.Any{origin, nexthop, aspath},
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
	if len(addrs) == 0 {
		return ""
	}

	probeCtx, cancel := context.WithTimeout(ctx, bgpProbeTimeout)
	defer cancel()

	var wg sync.WaitGroup
	result := make(chan string, 1)
	done := make(chan struct{})

	for _, addr := range addrs {
		addr := addr
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !bgpPortReachable(probeCtx, addr, port) {
				return
			}
			select {
			case result <- addr:
			default:
			}
		}()
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case addr := <-result:
		cancel()
		return addr
	case <-done:
		return ""
	case <-probeCtx.Done():
		return ""
	}
}

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
	neighs, err := netlink.NeighList(ifi.Index, netlink.FAMILY_V6)
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
