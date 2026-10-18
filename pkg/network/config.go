// Package network provides network mode abstractions for provisioning.
package network

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var resolvConfPath = "/etc/resolv.conf"

// DefaultBridgeName is the provision bridge name when the config sets none.
const DefaultBridgeName = "br.provision"

// Mode configures network connectivity in the ramdisk.
type Mode interface {
	// Setup configures all networking (interfaces, routing, etc.)
	Setup(ctx context.Context, cfg *Config) error
	// WaitForConnectivity blocks until network is reachable.
	WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error
	// Teardown cleans up network configuration.
	Teardown(ctx context.Context) error
}

// Config holds all parameters needed for network setup.
type Config struct {
	// DHCP mode fields.
	Interfaces []string // NICs to configure (default: auto-detect)

	// LegacyFRRRecovery restarts FRR after every failed 20 second
	// connectivity round unless a BGP session is established, like
	// wait_for_network and restart_frr of the CAPRF legacy common.sh.
	LegacyFRRRecovery bool

	// FRR/EVPN mode fields (from kernel cmdline or /deploy/vars).
	UnderlaySubnet   string // e.g. "192.168.4.0/24"
	UnderlayIP       string // Direct underlay IP (if no subnet)
	OverlaySubnet    string // e.g. "2a01:598:40a:5481::/64"
	IPMISubnet       string // e.g. "172.30.0.0/24"
	IPMIMAC          string // IPMI MAC for IP derivation
	IPMIIP           string // IPMI IP for offset calculation
	ASN              uint32 // BGP ASN for underlay
	ProvisionVNI     uint32 // VXLAN VNI for provision network
	ProvisionIP      string // IP/mask to assign to provision bridge (e.g. "10.100.0.20/24")
	ProvisionGateway string // Gateway VTEP IP for VXLAN BUM flooding
	DNSResolvers     string // Comma-separated DNS servers

	// Optional FRR onefabric mode fields.
	DCGWIPs          string // Data Center Gateway IPs (comma-sep)
	LeafASN          uint32 // Leaf switch AS
	LocalASN         uint32 // Local AS for leaf connections
	OverlayAggregate string // Route aggregate for overlay
	VPNRT            string // VPN route target for EVPN

	// Static networking fields.
	StaticIP      string // IP/mask to assign (e.g. "10.0.0.5/24")
	StaticGateway string // Default gateway IP
	StaticIface   string // Interface name (default: auto-detect first physical NIC)
	// StaticMatch selects the static NIC like a netplan match block when
	// StaticIface is empty. Without a single match, the first NIC is used.
	StaticMatch *LinkMatch

	// LinkRenames renames NICs before the network starts, like netplan
	// set-name.
	LinkRenames []LinkMatch
	// NICWait is the longest wait for configured NICs that do not exist
	// yet (NIC_WAIT_SECONDS).
	NICWait time.Duration

	// InterfaceNames are the interface names that the netplan files
	// (ethernet IDs without a match block) and frr.conf ("neighbor X
	// interface") name. ApplyPredictableNames renames NICs to them.
	InterfaceNames []string

	// LACP bonding fields.
	BondInterfaces string // Comma-separated NICs to bond (e.g. "eth0,eth1")
	BondMode       string // Bonding mode (default: "802.3ad" for LACP)

	// BGP/BFD tuning fields.
	VRFTableID        uint32 // Routing table ID for the underlay VRF (default: 1000)
	OverlayVRFTableID uint32 // Routing table ID for the overlay bridge VRF (default: VRFTableID)
	BGPKeepalive      uint32 // BGP keepalive interval in seconds (0 = FRR default)
	BGPHold           uint32 // BGP hold timer in seconds (0 = FRR default)
	BFDTransmitMS     uint32 // BFD transmit interval in ms of the native peers (0 = no BFD)
	BFDReceiveMS      uint32 // FRR-only BFD receive interval in ms (0 = disabled)
	BFDDetectMult     uint32 // BFD detect multiplier (0 = 3)

	// BGP peering mode (GoBGP).
	BGPPeerMode     PeerMode // Unnumbered (default), dual, or numbered
	BGPInterfaces   string   // Comma-separated interfaces for unnumbered/dual peers
	BGPNeighbors    string   // Comma-separated numbered peer IPs
	BGPRemoteASN    uint32   // Remote ASN for numbered peers (0 = iBGP)
	BGPUnderlayAF   string   // Underlay address family: ipv4 only; non-ipv4 values are rejected in config validation
	BGPOverlayType  string   // Overlay encapsulation: evpn-vxlan, l3vpn, none (default: evpn-vxlan)
	BGPAuthPassword string   // Optional TCP-MD5 password for all BGP peers (empty = no auth)
	BGPMinPeers     int      // Minimum established BGP peers for underlay readiness (default: 1)
	// BGPSessions are the peers of the provisioner FRR config. When set,
	// GoBGP uses them instead of BGPPeerMode, BGPInterfaces and
	// BGPNeighbors.
	BGPSessions []BGPSession
	// BGPMultipathRelax is "bgp bestpath as-path multipath-relax".
	BGPMultipathRelax bool
	// BGPCapabilities are the optional capabilities of the OPEN message.
	BGPCapabilities BGPCapabilities
	// BGPDefaults is the "frr defaults" profile, "traditional" or
	// "datacenter": BGP_DEFAULTS, else frr.conf, else datacenter (the
	// profile of the legacy daemons file). It selects the FRR 10.3
	// defaults that depend on it (bgpd/bgp_vty.c FRR_CFG_DEFAULT_*).
	BGPDefaults string
	// BGPHostname is BGP_HOSTNAME: when set, the ramdisk host name before
	// BGP starts, because GoBGP sends the system host name as the FQDN
	// capability.
	BGPHostname string
	// BGPRouteMaps and BGPCommunityLists define the route-maps that the
	// sessions name; BGPRouteMapIn and BGPRouteMapOut apply to the native
	// peers in every address family.
	BGPRouteMaps      []RouteMap
	BGPCommunityLists []CommunityList
	BGPRouteMapIn     string
	BGPRouteMapOut    string
	// BGPEBGPRequiresPolicy applies "bgp ebgp-requires-policy" to the
	// native peers of BGP_PEER_MODE (off by default).
	BGPEBGPRequiresPolicy bool
	// BGPLocalAS is the local-as of the native unnumbered peers.
	BGPLocalAS uint32
	// BGPNeighborVRF binds the native numbered peers to a VRF device.
	BGPNeighborVRF string
	// BGPConnectRetry is the connect retry timer of the native peers in
	// seconds; 0 keeps the GoBGP default.
	BGPConnectRetry uint32
	// BGPAllowASIn is the allowas-in count of the native peers.
	BGPAllowASIn uint32
	// BGPInstallRoutes installs the learned unicast routes into the kernel.
	BGPInstallRoutes bool
	// BGPFastExternalFailover is the native fast-external-failover
	// setting; nil keeps the session values (FRR default on).
	BGPFastExternalFailover *bool

	// Common fields.
	BridgeName        string // Default: "br.provision"
	BridgeMAC         string // Provision bridge MAC from netplan; empty derives a per-host MAC
	VRFName           string // Underlay VRF name; empty means default namespace
	OverlayVRFName    string // Overlay bridge VRF name; empty means default namespace
	OverlayVRFSet     bool   // OverlayVRFName was explicitly derived from structured config
	UnderlayDummyName string // Netplan underlay dummy name; empty uses "dummy.underlay"
	VXLANLink         string // Netplan VXLAN underlay link; empty leaves kernel selection
	VXLANName         string // Netplan VXLAN device name; empty uses "vx<VNI>"
	MTU               int    // Default: 9000
	NetworkMode       string // "gobgp" to use in-process GoBGP instead of FRR

	// EVPN L2 overlay (Type-2/3 route processing) — disabled by default.
	EVPNL2Enabled bool // Enable L2 overlay route handling
	// EVPNType5Only selects direct Type-5 routing without an L2 gateway.
	EVPNType5Only bool
	// EVPNType5OnlySet is true when EVPN_TYPE5_ONLY was given explicitly.
	EVPNType5OnlySet bool
	// EVPNL3VNIs lists the VNIs that the provisioner FRR config maps to a VRF.
	// Set from netplan input only; an L3VNI provision VNI keeps Type-5 only.
	EVPNL3VNIs []uint32

	// LoopbackAddrs are the netplan addresses of "lo", such as the IPv6
	// /128 provisioning address of the bm4x templates.
	LoopbackAddrs []string
	// BlackholeRoutes are the FRR "ip route" and "ipv6 route" blackhole
	// prefixes of the default VRF.
	BlackholeRoutes []string
	// EVPNImportRTs and EVPNExportRTs are the route targets of the
	// provision L3VNI ("route-target import|export"); empty lists use
	// vpn_rt or the derived route target.
	EVPNImportRTs []string
	EVPNExportRTs []string
	// Type5Networks are the FRR "network" prefixes of the provision L3VNI
	// VRF that "advertise ipv4|ipv6 unicast" originates as EVPN Type-5.
	Type5Networks []string

	// OverlayDevices are the device settings of the provision VXLAN device
	// and bridge, from netplan or the native options.
	OverlayDevices OverlayDevices
	// NoAcceptRA lists the NICs with netplan "accept-ra: false".
	NoAcceptRA []string
	// EmitLLDP lists the NICs with netplan "emit-lldp: true".
	EmitLLDP []string
	// LLDPProfile is the identity of the sent LLDP frames (lldp.Profile*).
	// LLDPEmitAll sends on every physical NIC; the caller starts that
	// sender, so the network stacks then send nothing themselves.
	LLDPProfile      string
	LLDPPortIDFormat string
	LLDPEmitAll      bool
	// IPForwarding and IPv6Forwarding set the forwarding sysctls; nil keeps
	// the GoBGP default: IPv4 on, IPv6 off (the kernel default the legacy
	// ramdisk keeps). frr.conf "[no] ip[v6] forwarding" sets them.
	IPForwarding   *bool
	IPv6Forwarding *bool

	// VLAN configuration.
	VLANs []VLANConfig // 802.1Q VLAN interfaces to create before network mode setup
}

// BGPSession is one BGP peer with every FRR attribute resolved (peer,
// peer-group, profile default).
type BGPSession struct {
	Name      string // peer address, or the interface for an unnumbered peer
	Interface bool
	VRF       string // "" is the default VRF
	Internal  bool   // iBGP
	RemoteASN uint32 // 0 with Internal false: any external AS
	LocalASN  uint32 // "local-as N no-prepend replace-as", 0 when unset
	// Timers in seconds.
	KeepaliveTime uint32
	HoldTime      uint32
	ConnectRetry  uint32
	// ConnectRetrySet is true when the session sets its connect timer.
	ConnectRetrySet bool
	// Activated address families.
	IPv4Unicast bool
	IPv6Unicast bool
	L2VPNEVPN   bool
	// AllowASIn is the "allowas-in" count, 0 when unset.
	AllowASIn int
	// FastExternalFailover stops an unnumbered session as soon as its
	// interface loses carrier (FRR "bgp fast-external-failover").
	FastExternalFailover bool
	BFD                  bool
	BFDProfile           string
	// BFD timers in ms and the detect multiplier, set when BFD is on.
	BFDTransmitMS uint32
	BFDReceiveMS  uint32
	BFDDetectMult uint32
	// RouteMaps are the route-maps per address family ("ipv4-unicast",
	// "ipv6-unicast", "l2vpn-evpn").
	RouteMaps []SessionRouteMaps
	// EBGPRequiresPolicy rejects the routes of an eBGP session in each
	// family and direction without a route-map, like FRR "bgp
	// ebgp-requires-policy".
	EBGPRequiresPolicy bool
}

// OverlayDevices are netplan settings of the provision VXLAN device and
// bridge, with systemd-networkd semantics. Zero and nil values keep the
// GoBGP defaults: VXLAN MTU = MTU - 50, a derived MAC, no MAC learning,
// kernel defaults for the bridge port and an IPv6 link-local address.
type OverlayDevices struct {
	VXLANMTU int    `json:"vxlanMTU,omitempty" yaml:"vxlanMTU,omitempty"`
	VXLANMAC string `json:"vxlanMAC,omitempty" yaml:"vxlanMAC,omitempty"`
	// VXLANLearning is netplan mac-learning, VXLANARPProxy arp-proxy.
	VXLANLearning *bool `json:"vxlanLearning,omitempty" yaml:"vxlanLearning,omitempty"`
	VXLANARPProxy *bool `json:"vxlanARPProxy,omitempty" yaml:"vxlanARPProxy,omitempty"`
	// PortHairpin and PortLearning are netplan hairpin and
	// port-mac-learning: the VXLAN device as bridge port.
	PortHairpin  *bool `json:"portHairpin,omitempty" yaml:"portHairpin,omitempty"`
	PortLearning *bool `json:"portLearning,omitempty" yaml:"portLearning,omitempty"`
	// VXLANLinkLocal and BridgeLinkLocal false is netplan "link-local: []":
	// no IPv6 link-local address.
	VXLANLinkLocal  *bool `json:"vxlanLinkLocal,omitempty" yaml:"vxlanLinkLocal,omitempty"`
	BridgeLinkLocal *bool `json:"bridgeLinkLocal,omitempty" yaml:"bridgeLinkLocal,omitempty"`
	// VXLANAcceptRA and BridgeAcceptRA are netplan accept-ra.
	VXLANAcceptRA  *bool `json:"vxlanAcceptRA,omitempty" yaml:"vxlanAcceptRA,omitempty"`
	BridgeAcceptRA *bool `json:"bridgeAcceptRA,omitempty" yaml:"bridgeAcceptRA,omitempty"`
	BridgeMTU      int   `json:"bridgeMTU,omitempty" yaml:"bridgeMTU,omitempty"`
	// BridgeSTP is the spanning tree of the provision bridge, like netplan
	// bridge parameters stp: a parameters block without stp means on.
	// nil keeps the kernel default (off).
	BridgeSTP *bool `json:"bridgeSTP,omitempty" yaml:"bridgeSTP,omitempty"`
}

// SessionRouteMaps are the import (In) and export (Out) route-maps of a
// BGP session in one address family.
type SessionRouteMaps struct {
	Family string `json:"family" yaml:"family"`
	In     string `json:"in,omitempty" yaml:"in,omitempty"`
	Out    string `json:"out,omitempty" yaml:"out,omitempty"`
}

// RouteMap is an FRR route-map with the subset of statements BOOTy
// implements, in sequence order.
type RouteMap struct {
	Name    string          `json:"name" yaml:"name"`
	Entries []RouteMapEntry `json:"entries" yaml:"entries"`
}

// RouteMapEntry is one "route-map NAME permit|deny SEQ" entry. A permit
// entry that matches applies its sets and stops; a deny entry that
// matches rejects. A route that no entry matches is rejected.
type RouteMapEntry struct {
	Seq  int  `json:"seq" yaml:"seq"`
	Deny bool `json:"deny,omitempty" yaml:"deny,omitempty"`
	// MatchCommunity names a community-list; empty matches every route.
	MatchCommunity     string   `json:"matchCommunity,omitempty" yaml:"matchCommunity,omitempty"`
	SetLocalPreference *uint32  `json:"setLocalPreference,omitempty" yaml:"setLocalPreference,omitempty"`
	SetCommunities     []string `json:"setCommunities,omitempty" yaml:"setCommunities,omitempty"`
	// SetCommunityAdditive adds SetCommunities instead of replacing.
	SetCommunityAdditive bool `json:"setCommunityAdditive,omitempty" yaml:"setCommunityAdditive,omitempty"`
}

// CommunityList is a standard community-list. An entry matches a route
// that carries all its communities; the first matching entry decides,
// and no match denies.
type CommunityList struct {
	Name    string               `json:"name" yaml:"name"`
	Entries []CommunityListEntry `json:"entries" yaml:"entries"`
}

// CommunityListEntry is one "bgp community-list standard" line.
type CommunityListEntry struct {
	Deny        bool     `json:"deny,omitempty" yaml:"deny,omitempty"`
	Communities []string `json:"communities" yaml:"communities"`
}

// BGPCapabilities are the optional BGP capabilities that BOOTy sends in
// its OPEN message, with the FRR 10 defaults (bgpd/bgp_open.c).
type BGPCapabilities struct {
	// GracefulRestart is the graceful restart capability (RFC 4724) of the
	// FRR default helper mode: no address family tuples, so a peer does
	// not keep BOOTy's routes when the session ends, while BOOTy keeps the
	// routes of a restarting peer. It also sends long-lived graceful
	// restart (RFC 9494); in helper mode GoBGP sends it without family
	// tuples, so a stale time has no effect and is not configurable.
	GracefulRestart bool
	// GracefulRestartTime is the restart time in seconds (FRR 120).
	GracefulRestartTime uint32
	// AddPathReceive is the add-path capability (RFC 7911) with receive in
	// every address family, like FRR.
	AddPathReceive bool
	// SoftwareVersion also sends the software version capability to eBGP
	// peers; iBGP peers always get it, like FRR.
	SoftwareVersion bool
}

// VLANConfig holds configuration for a single 802.1Q VLAN interface.
type VLANConfig struct {
	ID      int    // VLAN identifier (1-4094)
	Parent  string // Physical parent interface (e.g. "eno1")
	Address string // Static IP/CIDR (e.g. "10.200.0.42/24"); empty = DHCP
	Gateway string // Default gateway IP (optional)
}

// BridgeNameOrDefault returns the provision bridge name BOOTy creates.
func (c *Config) BridgeNameOrDefault() string {
	if c.BridgeName == "" {
		return DefaultBridgeName
	}
	return c.BridgeName
}

// ApplyDefaults fills in default values for unset fields.
func (c *Config) ApplyDefaults() {
	if c.BridgeName == "" {
		c.BridgeName = DefaultBridgeName
	}
	// VRFName is intentionally left empty by default — the GoBGP stack
	// creates a VRF only when VRFName is set via the VRF_NAME env var.
	// Standard underlay peering runs in the default namespace.
	// Overlay VXLAN/bridge resources are always created regardless of VRF.
	if c.MTU == 0 {
		c.MTU = 9000
	}
	if c.VRFTableID == 0 {
		c.VRFTableID = 1000
	}
	if c.OverlayVRFTableID == 0 {
		c.OverlayVRFTableID = c.VRFTableID
	}
	if c.BGPMinPeers == 0 {
		c.BGPMinPeers = 1
	}
	// BFD is FRR-only and opt-in; BFD timer validation happens before mode setup.
}

// ConfigureResolvers writes DNS resolvers for the active initramfs network.
func ConfigureResolvers(resolvers string) error {
	lines, err := ResolverLines(resolvers)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}

	dir := filepath.Dir(resolvConfPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create resolver directory %s: %w", dir, err)
	}

	if info, err := os.Lstat(resolvConfPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(resolvConfPath); err != nil {
			return fmt.Errorf("replace resolver symlink %s: %w", resolvConfPath, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat resolver file %s: %w", resolvConfPath, err)
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(resolvConfPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write resolver file %s: %w", resolvConfPath, err)
	}
	return nil
}

// ResolverLines renders validated resolv.conf nameserver lines.
func ResolverLines(resolvers string) ([]string, error) {
	fields := strings.Split(resolvers, ",")
	lines := make([]string, 0, len(fields))
	for _, resolver := range fields {
		resolver = strings.TrimSpace(resolver)
		if resolver == "" {
			continue
		}
		if _, err := netip.ParseAddr(resolver); err != nil {
			return nil, fmt.Errorf("invalid DNS resolver %q: must be an IPv4 or IPv6 address", resolver)
		}
		lines = append(lines, "nameserver "+resolver)
	}
	return lines, nil
}

// IsFRRMode returns true if the config has enough parameters for FRR/EVPN.
func (c *Config) IsFRRMode() bool {
	return (c.UnderlaySubnet != "" || c.UnderlayIP != "") && c.ASN != 0
}

// IsStaticMode returns true if static IP configuration is provided.
func (c *Config) IsStaticMode() bool {
	return c.StaticIP != ""
}

// IsBondMode returns true if LACP bonding interfaces are configured.
func (c *Config) IsBondMode() bool {
	return c.BondInterfaces != ""
}

// IsVLANMode returns true if any VLAN interfaces are configured.
func (c *Config) IsVLANMode() bool {
	return len(c.VLANs) > 0
}

// ParseVLANs parses a comma-separated VLAN specification string into
// a slice of VLANConfig. Each entry has the format:
//
//	ID:parent[:address[:gateway]]
//
// Examples:
//
//	"200:eno1:10.200.0.42/24"
//	"200:eno1:10.200.0.42/24,300:eno2"
//	"200:eno1:10.200.0.42/24:10.200.0.1"
//	"200:eno1:[2001:db8::42/64]:[2001:db8::1]"
//
// IPv6 address or gateway fields must be bracketed because ':' separates
// fields in the compact VLAN specification.
func ParseVLANs(spec string) ([]VLANConfig, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	var configs []VLANConfig
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts, err := splitVLANEntry(entry)
		if err != nil {
			return nil, err
		}
		if len(parts) < 2 {
			return nil, fmt.Errorf("invalid VLAN entry %q: need at least ID:parent", entry)
		}
		if len(parts) > 4 {
			return nil, fmt.Errorf("invalid VLAN entry %q: too many fields; wrap IPv6 address or gateway fields in brackets", entry)
		}

		id, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid VLAN ID %q: %w", parts[0], err)
		}
		if id < 1 || id > 4094 {
			return nil, fmt.Errorf("VLAN ID %d out of range (1-4094)", id)
		}

		parent := strings.TrimSpace(parts[1])
		if parent == "" {
			return nil, fmt.Errorf("invalid VLAN entry %q: parent interface is required", entry)
		}

		cfg := VLANConfig{
			ID:     id,
			Parent: parent,
		}
		if len(parts) >= 3 {
			cfg.Address = strings.TrimSpace(parts[2])
		}
		if len(parts) >= 4 {
			cfg.Gateway = strings.TrimSpace(parts[3])
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

func splitVLANEntry(entry string) ([]string, error) {
	var fields []string
	var field strings.Builder
	inBracket := false
	for _, r := range entry {
		switch r {
		case '[':
			if inBracket {
				return nil, fmt.Errorf("invalid VLAN entry %q: nested '['", entry)
			}
			inBracket = true
		case ']':
			if !inBracket {
				return nil, fmt.Errorf("invalid VLAN entry %q: unmatched ']'", entry)
			}
			inBracket = false
		case ':':
			if !inBracket {
				fields = append(fields, strings.TrimSpace(field.String()))
				field.Reset()
				continue
			}
			field.WriteRune(r)
		default:
			field.WriteRune(r)
		}
	}
	if inBracket {
		return nil, fmt.Errorf("invalid VLAN entry %q: unterminated '['", entry)
	}
	fields = append(fields, strings.TrimSpace(field.String()))
	return fields, nil
}

// IsGoBGPMode returns true if GoBGP mode is explicitly requested.
func (c *Config) IsGoBGPMode() bool {
	return strings.EqualFold(c.NetworkMode, "gobgp")
}

// PeerMode controls how BGP neighbor sessions are established.
type PeerMode string

const (
	// PeerModeUnnumbered uses link-local interface peers for both IPv4 unicast
	// and L2VPN-EVPN address families.  This is the default for leaf-peered
	// datacenter fabrics running BGP unnumbered.
	PeerModeUnnumbered PeerMode = "unnumbered"

	// PeerModeDual combines unnumbered interface peers (IPv4 unicast only) with
	// numbered peers for L2VPN-EVPN.  Typical use case: fabric underlay via
	// unnumbered, EVPN via iBGP to route reflectors.
	PeerModeDual PeerMode = "dual"

	// PeerModeNumbered uses explicit neighbor IPs for all BGP sessions.  The
	// machine must already have underlay reachability (e.g. via DHCP or static
	// IP).  All configured address families are negotiated over the numbered
	// sessions.
	PeerModeNumbered PeerMode = "numbered"
)

// ParsePeerMode converts a string to a PeerMode, defaulting to unnumbered.
func ParsePeerMode(s string) PeerMode {
	switch PeerMode(strings.ToLower(s)) {
	case PeerModeDual:
		return PeerModeDual
	case PeerModeNumbered:
		return PeerModeNumbered
	default:
		return PeerModeUnnumbered
	}
}
