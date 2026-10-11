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

	// LACP bonding fields.
	BondInterfaces string // Comma-separated NICs to bond (e.g. "eth0,eth1")
	BondMode       string // Bonding mode (default: "802.3ad" for LACP)

	// BGP/BFD tuning fields.
	VRFTableID        uint32 // Routing table ID for the underlay VRF (default: 1000)
	OverlayVRFTableID uint32 // Routing table ID for the overlay bridge VRF (default: VRFTableID)
	BGPKeepalive      uint32 // BGP keepalive interval in seconds (0 = FRR default)
	BGPHold           uint32 // BGP hold timer in seconds (0 = FRR default)
	BFDTransmitMS     uint32 // FRR-only BFD transmit interval in ms (0 = disabled)
	BFDReceiveMS      uint32 // FRR-only BFD receive interval in ms (0 = disabled)

	// BGP peering mode (GoBGP).
	BGPPeerMode     PeerMode // Unnumbered (default), dual, or numbered
	BGPInterfaces   string   // Comma-separated interfaces for unnumbered/dual peers
	BGPNeighbors    string   // Comma-separated numbered peer IPs
	BGPRemoteASN    uint32   // Remote ASN for numbered peers (0 = iBGP)
	BGPUnderlayAF   string   // Underlay address family: ipv4 only; non-ipv4 values are rejected in config validation
	BGPOverlayType  string   // Overlay encapsulation: evpn-vxlan, l3vpn, none (default: evpn-vxlan)
	BGPAuthPassword string   // Optional TCP-MD5 password for all BGP peers (empty = no auth)
	BGPMinPeers     int      // Minimum established BGP peers for underlay readiness (default: 1)

	// Common fields.
	BridgeName        string // Default: "br.provision"
	BridgeMAC         string // Provision bridge MAC from netplan; empty derives a per-host MAC
	VRFName           string // Underlay VRF name; empty means default namespace
	OverlayVRFName    string // Overlay bridge VRF name; empty means default namespace
	OverlayVRFSet     bool   // OverlayVRFName was explicitly derived from structured config
	UnderlayDummyName string // Netplan underlay dummy name; empty uses "dummy.underlay"
	VXLANLink         string // Netplan VXLAN underlay link; empty leaves kernel selection
	MTU               int    // Default: 9000
	NetworkMode       string // "gobgp" to use in-process GoBGP instead of FRR

	// EVPN L2 overlay (Type-2/3 route processing) — disabled by default.
	EVPNL2Enabled bool // Enable L2 overlay route handling
	// EVPNType5Only selects direct Type-5 routing without an L2 gateway.
	EVPNType5Only bool
	// EVPNL3VNIs lists the VNIs that the provisioner FRR config maps to a VRF.
	// Set from netplan input only; an L3VNI provision VNI keeps Type-5 only.
	EVPNL3VNIs []uint32

	// VLAN configuration.
	VLANs []VLANConfig // 802.1Q VLAN interfaces to create before network mode setup
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
