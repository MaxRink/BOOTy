// Package gobgp implements a three-tier BGP network stack using the GoBGP
// library as a pure-Go replacement for FRR.
//
// Architecture:
//   - Tier 1 (Underlay): eBGP peering with leaf switches for VXLAN reachability
//   - Tier 2 (Overlay): EVPN Type-5 routes with VXLAN encapsulation
//   - Tier 3 (IPMI): Optional L3 path to the BMC (not yet implemented)
//
// Supported BGP session modes (PeerMode):
//   - Unnumbered: link-local interface peers (IPv4 + L2VPN-EVPN)
//   - Dual: unnumbered underlay (IPv4) + numbered peers (L2VPN-EVPN)
//   - Numbered: explicit neighbor IPs only (IPv4 + L2VPN-EVPN), requires
//     DHCP or static underlay for initial connectivity
package gobgp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frr"
)

// Tier represents a single concern in the network stack.
type Tier interface {
	// Setup configures the tier's networking resources.
	Setup(ctx context.Context) error
	// Ready waits for the tier to become operational.
	Ready(ctx context.Context, timeout time.Duration) error
	// Teardown removes the tier's networking resources.
	Teardown(ctx context.Context) error
}

// Config holds GoBGP three-tier stack configuration.
type Config struct {
	ASN               uint32 // Local BGP autonomous system number
	RouterID          string // BGP router ID (underlay IP)
	ListenPort        int32  // BGP listen port (default: 179)
	ProvisionVNI      int    // VXLAN VNI for provisioning network
	ProvisionIP       string // IP/mask for provision bridge
	ProvisionGateway  string // Gateway VTEP IP for VXLAN BUM flooding
	VPNRT             string // Optional explicit EVPN route target (ASN:value)
	importRTASN       uint32
	importRTVNI       uint32
	importRTReady     bool
	importRTErr       error
	DNSResolvers      string           // Comma-separated DNS servers
	BridgeName        string           // Bridge device name (default: "br.provision")
	VRFName           string           // Underlay VRF name (default: empty, same as FRR)
	OverlayVRFName    string           // Overlay bridge VRF name (default: follows VRFName)
	VRFTableID        uint32           // Routing table ID for the underlay VRF (default: 1000)
	OverlayVRFTableID uint32           // Routing table ID for the overlay bridge VRF (default: VRFTableID)
	UnderlayDummyName string           // Underlay dummy device name (default: "dummy.underlay")
	VXLANLink         string           // VXLAN underlay link from netplan (optional)
	VXLANName         string           // VXLAN device name from netplan (default: "vx<VNI>")
	MTU               int              // Physical interface MTU (default: 9000)
	KeepaliveInterval uint64           // BGP keepalive seconds (default: 3)
	HoldTime          uint64           // BGP hold timer seconds (default: 9)
	ConnectRetry      uint64           // BGP connect retry seconds (default: 5)
	OverlayIP         string           // Overlay loopback IP (derived or same as RouterID)
	BridgeMAC         string           // Derived MAC for provision bridge
	IPMIMAC           string           // IPMI MAC for bridge MAC derivation
	PeerMode          network.PeerMode // BGP session establishment mode
	Interfaces        []string         // Explicit interfaces for unnumbered peers
	NeighborAddrs     []string         // Explicit numbered peer IPs (dual/numbered modes)
	RemoteASN         uint32           // Remote ASN for numbered peers (0 = same ASN → iBGP)
	EnableL2          bool             // Enable L2 EVPN overlay (gate Type-2/3 route handling)
	Type5Only         bool             // Use direct Type-5 routing without an L2 gateway
	AuthPassword      string           // Optional TCP-MD5 password for BGP peers (empty = no auth)
	// UnderlayAF selects the BGP address family for underlay sessions.
	// Machine config accepts only ipv4; other parsed values are unsupported.
	UnderlayAF string
	// OverlayType selects the BGP overlay encapsulation (evpn-vxlan/l3vpn/none).
	OverlayType string
	// MinEstablishedPeers is the minimum number of BGP peers that must reach
	// ESTABLISHED state before Ready() reports the underlay as ready.
	// In dual-homed EVPN fabrics, set this to 2 to ensure full-fabric
	// connectivity before provisioning proceeds.  Default: 1.
	MinEstablishedPeers int
	// Sessions are the peers of the provisioner FRR config. When set they
	// replace PeerMode, Interfaces, NeighborAddrs, RemoteASN and the timers.
	Sessions []network.BGPSession
	// MultipathRelax is "bgp bestpath as-path multipath-relax": equal-cost
	// paths may have different AS paths of the same length.
	MultipathRelax bool
	// FastExternalFailover allows the carrier watch of the unnumbered
	// sessions ("bgp fast-external-failover"); default true like FRR.
	FastExternalFailover bool
	// LoopbackAddrs are added to lo instead of OverlayIP.
	LoopbackAddrs []string
	// BlackholeRoutes are installed in the main table, like FRR static
	// blackhole routes. A received Type-5 route for one of them is not
	// installed: the static route wins, as in zebra.
	BlackholeRoutes []string
	// Type5Networks are originated as EVPN Type-5 routes when a local
	// address or blackhole route covers them (FRR "network" import check).
	Type5Networks []string
	// Policy configures BGP import/export communities and route attributes.
	Policy *PolicyConfig
	// GracefulRestart configures BGP graceful restart behavior.
	GracefulRestart *GracefulRestartConfig
}

// NewConfig creates a GoBGP Config from network configuration.
// It derives addresses using the shared FRR address-derivation logic
// and applies GoBGP-specific defaults (aggressive hold timers, no BFD).
func NewConfig(netCfg *network.Config) (*Config, error) {
	netCfg.ApplyDefaults()
	if netCfg.BFDTransmitMS > 0 || netCfg.BFDReceiveMS > 0 {
		return nil, fmt.Errorf("gobgp mode does not support BFD; use FRR mode or BGP keepalive/hold timers")
	}

	underlayIP, overlayIP, bridgeMAC, err := frr.DeriveAddresses(netCfg)
	if err != nil {
		return nil, fmt.Errorf("derive addresses: %w", err)
	}

	overlayVRFName := netCfg.OverlayVRFName
	if !netCfg.OverlayVRFSet {
		overlayVRFName = netCfg.VRFName
	}

	cfg := &Config{
		ASN:                  netCfg.ASN,
		RouterID:             underlayIP,
		ProvisionVNI:         int(netCfg.ProvisionVNI),
		ProvisionIP:          netCfg.ProvisionIP,
		ProvisionGateway:     netCfg.ProvisionGateway,
		VPNRT:                netCfg.VPNRT,
		DNSResolvers:         netCfg.DNSResolvers,
		BridgeName:           netCfg.BridgeName,
		VRFName:              netCfg.VRFName,
		OverlayVRFName:       overlayVRFName,
		VRFTableID:           netCfg.VRFTableID,
		OverlayVRFTableID:    netCfg.OverlayVRFTableID,
		UnderlayDummyName:    netCfg.UnderlayDummyName,
		VXLANLink:            netCfg.VXLANLink,
		VXLANName:            netCfg.VXLANName,
		LoopbackAddrs:        netCfg.LoopbackAddrs,
		BlackholeRoutes:      netCfg.BlackholeRoutes,
		Type5Networks:        netCfg.Type5Networks,
		MTU:                  netCfg.MTU,
		KeepaliveInterval:    uint64(netCfg.BGPKeepalive),
		HoldTime:             uint64(netCfg.BGPHold),
		OverlayIP:            overlayIP,
		BridgeMAC:            bridgeMAC,
		IPMIMAC:              netCfg.IPMIMAC,
		PeerMode:             netCfg.BGPPeerMode,
		Interfaces:           parseCSV(netCfg.BGPInterfaces),
		NeighborAddrs:        parseNeighborAddrs(netCfg.BGPNeighbors),
		RemoteASN:            netCfg.BGPRemoteASN,
		EnableL2:             netCfg.EVPNL2Enabled,
		Type5Only:            netCfg.EVPNType5Only,
		AuthPassword:         netCfg.BGPAuthPassword,
		UnderlayAF:           netCfg.BGPUnderlayAF,
		OverlayType:          netCfg.BGPOverlayType,
		MinEstablishedPeers:  netCfg.BGPMinPeers,
		Sessions:             netCfg.BGPSessions,
		MultipathRelax:       netCfg.BGPMultipathRelax,
		FastExternalFailover: netCfg.BGPFastExternalFailover == nil || *netCfg.BGPFastExternalFailover,
	}

	cfg.ApplyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// ApplyDefaults fills in default values for unset fields.
// GoBGP uses aggressive hold timers (3s/9s) instead of BFD for fast failover.
func (c *Config) ApplyDefaults() {
	if c.ListenPort == 0 {
		c.ListenPort = 179
	}
	if c.KeepaliveInterval == 0 {
		c.KeepaliveInterval = 3
	}
	if c.HoldTime == 0 {
		c.HoldTime = 9
	}
	if c.ConnectRetry == 0 {
		c.ConnectRetry = 5
	}
	if c.BridgeName == "" {
		c.BridgeName = network.DefaultBridgeName
	}
	if c.MTU == 0 {
		c.MTU = 9000
	}
	c.applyVRFTableDefaults()
	// Normalize UnderlayAF and OverlayType to their canonical lowercase forms
	// so downstream comparisons don't need case-insensitive matching.
	if af, err := ParseUnderlayAF(c.UnderlayAF); err == nil {
		c.UnderlayAF = string(af)
	}
	if ot, err := ParseOverlayType(c.OverlayType); err == nil {
		c.OverlayType = string(ot)
	}
	if c.MinEstablishedPeers == 0 {
		c.MinEstablishedPeers = 1
	}
	if c.GracefulRestart != nil {
		c.GracefulRestart.ApplyDefaults()
	}
}

func (c *Config) applyVRFTableDefaults() {
	switch c.VRFTableID {
	case 0:
		c.VRFTableID = 1000
	case 1:
		slog.Warn("vrf_table_id=1 conflicts with default routing table, overriding to 1000")
		c.VRFTableID = 1000
	}

	switch c.OverlayVRFTableID {
	case 0:
		c.OverlayVRFTableID = c.VRFTableID
	case 1:
		slog.Warn("overlay_vrf_table_id=1 conflicts with default routing table, using underlay VRF table")
		c.OverlayVRFTableID = c.VRFTableID
	}
}

// Validate checks that required configuration fields are present.
func (c *Config) Validate() error {
	if c.ASN == 0 {
		return fmt.Errorf("ASN is required for GoBGP mode")
	}
	if err := c.validateRouterID(); err != nil {
		return err
	}
	if err := c.validatePeerMode(); err != nil {
		return err
	}
	if err := c.validateTimers(); err != nil {
		return err
	}
	if err := c.validateProvisioning(); err != nil {
		return err
	}

	af, err := ParseUnderlayAF(c.UnderlayAF)
	if err != nil {
		return fmt.Errorf("invalid underlay AF: %w", err)
	}
	if af != AFIPv4 {
		return fmt.Errorf("underlay AF %q is not yet implemented", af)
	}
	ot, err := ParseOverlayType(c.OverlayType)
	if err != nil {
		return fmt.Errorf("invalid overlay type: %w", err)
	}
	if ot == OverlayL3VPN {
		return fmt.Errorf("overlay type %q is not yet implemented", ot)
	}
	if c.MinEstablishedPeers < 1 {
		return fmt.Errorf("BGP_MIN_PEERS %d is invalid (must be >= 1)", c.MinEstablishedPeers)
	}
	if _, _, err := c.importRouteTarget(); err != nil {
		return err
	}
	return c.validatePolicy()
}

func (c *Config) validateProvisioning() error {
	if c.ProvisionVNI == 0 || c.ProvisionVNI > 16777215 {
		return fmt.Errorf("ProvisionVNI %d out of range (must be 1..16777215)", c.ProvisionVNI)
	}
	if err := c.validateOverlayProvisioning(); err != nil {
		return err
	}

	// 4-octet ASN RD/RT format can only encode 16-bit VNI values.
	if c.ASN > 65535 && c.ProvisionVNI > 65535 {
		return fmt.Errorf("4-octet ASN %d with VNI %d > 65535 is unsupported (RD/RT truncation)", c.ASN, c.ProvisionVNI)
	}

	// MTU must leave room for VXLAN overhead (50 bytes) plus minimum
	// useful IP payload (576 bytes per RFC 791).
	const minMTU = 576 + 50 // minIP + vxlanOverhead
	if c.MTU > 0 && c.MTU < minMTU {
		return fmt.Errorf("MTU %d too low (minimum %d = 576 IP + 50 VXLAN overhead)", c.MTU, minMTU)
	}
	return nil
}

func (c *Config) validateOverlayProvisioning() error {
	if c.EnableL2 && c.Type5Only {
		return fmt.Errorf("type5Only and EnableL2 cannot both be enabled")
	}
	if overlayType, err := ParseOverlayType(c.OverlayType); err == nil && overlayType == OverlayEVPNVXLAN {
		if c.ProvisionGateway == "" && !c.Type5Only {
			return fmt.Errorf("provision gateway is required for GoBGP VXLAN data plane")
		}
		gateway := net.ParseIP(c.ProvisionGateway)
		if c.ProvisionGateway != "" && (gateway == nil || gateway.To4() == nil) {
			return fmt.Errorf("provision gateway %q must be a valid IPv4 address", c.ProvisionGateway)
		}
	}
	return nil
}

// usesType5Only reports whether direct Type-5 routing was explicitly selected.
func (c *Config) usesType5Only() bool {
	return c.Type5Only
}

func (c *Config) validatePolicy() error {
	if c.Policy == nil {
		return nil
	}
	if err := ValidateCommunities(&c.Policy.ImportCommunities); err != nil {
		return fmt.Errorf("import communities: %w", err)
	}
	if err := ValidateCommunities(&c.Policy.ExportCommunities); err != nil {
		return fmt.Errorf("export communities: %w", err)
	}
	return nil
}

func (c *Config) validateRouterID() error {
	if c.RouterID == "" {
		return fmt.Errorf("router ID (underlay IP) is required for GoBGP mode")
	}
	if net.ParseIP(c.RouterID) == nil || net.ParseIP(c.RouterID).To4() == nil {
		return fmt.Errorf("router ID %q must be a valid IPv4 address", c.RouterID)
	}
	return nil
}

func (c *Config) validatePeerMode() error {
	if len(c.Sessions) > 0 {
		return validateSessions(c.Sessions)
	}
	switch c.PeerMode {
	case network.PeerModeDual, network.PeerModeNumbered:
		if len(c.NeighborAddrs) == 0 {
			return fmt.Errorf("BGP_NEIGHBORS required for %s peer mode", c.PeerMode)
		}
		for _, addr := range c.NeighborAddrs {
			ip := net.ParseIP(addr)
			if ip == nil {
				return fmt.Errorf("invalid neighbor address %q", addr)
			}
			if ip.To4() == nil {
				return fmt.Errorf("BGP_NEIGHBORS address %q must be IPv4; IPv6 underlay is not implemented", addr)
			}
		}
	case network.PeerModeUnnumbered:
		// No extra validation needed.
	default:
		return fmt.Errorf("unknown peer mode %q", c.PeerMode)
	}
	return nil
}

// validateSessions checks the FRR peers. FRR accepts a hold time of 0 or
// 3-65535 and any keepalive; the negotiated keepalive is at most hold/3.
func validateSessions(sessions []network.BGPSession) error {
	for i := range sessions {
		s := &sessions[i]
		if !s.Interface && net.ParseIP(s.Name) == nil {
			return fmt.Errorf("BGP neighbor %q is not an address", s.Name)
		}
		if s.HoldTime == 0 || s.KeepaliveTime == 0 {
			// FRR "timers 0 0" disables keepalives and the hold timer;
			// GoBGP would replace 0 with 30/90 instead.
			return fmt.Errorf("BGP neighbor %s: timers %d %d: 0 is not supported", s.Name, s.KeepaliveTime, s.HoldTime)
		}
		if s.HoldTime < 3 {
			return fmt.Errorf("BGP neighbor %s: hold time %d must be at least 3", s.Name, s.HoldTime)
		}
		if !s.IPv4Unicast && !s.IPv6Unicast && !s.L2VPNEVPN {
			return fmt.Errorf("BGP neighbor %s has no activated address family", s.Name)
		}
	}
	return nil
}

// sessionVRF returns the VRF device a session is bound to, "" for the
// default VRF.
func sessionVRF(s *network.BGPSession) string {
	if s.VRF == "default" {
		return ""
	}
	return s.VRF
}

// validateTimers checks BGP timer constraints per RFC 4271 §4.2.
func (c *Config) validateTimers() error {
	if c.HoldTime != 0 && c.HoldTime < 3 {
		return fmt.Errorf("HoldTime %d violates RFC 4271 (must be 0 or >= 3)", c.HoldTime)
	}
	if c.HoldTime > 0 && c.KeepaliveInterval > c.HoldTime/3 {
		return fmt.Errorf("KeepaliveInterval %d exceeds HoldTime/3 (%d)", c.KeepaliveInterval, c.HoldTime/3)
	}
	return nil
}

func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	var values []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			values = append(values, a)
		}
	}
	return values
}

// parseNeighborAddrs splits a comma-separated list of IPs into a string slice.
func parseNeighborAddrs(s string) []string {
	return parseCSV(s)
}

func (c *Config) importRouteTarget() (asn, vni uint32, err error) {
	if !c.importRTReady {
		_ = c.cacheImportRouteTarget()
	}
	return c.importRTASN, c.importRTVNI, c.importRTErr
}

func (c *Config) cacheImportRouteTarget() error {
	c.importRTASN, c.importRTVNI, c.importRTErr = deriveImportRouteTarget(c)
	c.importRTReady = true
	return c.importRTErr
}

func deriveImportRouteTarget(c *Config) (asn, vni uint32, err error) {
	asn, vni = c.ASN, uint32(c.ProvisionVNI)
	vpnRT := strings.TrimSpace(c.VPNRT)
	if vpnRT == "" {
		return asn, vni, nil
	}
	parsedASN, parsedVNI, err := ParseRouteTarget(vpnRT)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid VPN route target: %w", err)
	}
	return parsedASN, parsedVNI, nil
}

// IsiBGP returns true when the numbered peers use the same ASN (iBGP).
func (c *Config) IsiBGP() bool {
	return c.RemoteASN == 0 || c.RemoteASN == c.ASN
}
