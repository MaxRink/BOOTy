package config

// MaxNetworkWaitAttempts bounds NETWORK_WAIT_MAX_ATTEMPTS (about 23 days of
// 20 second rounds), so the wait time cannot overflow.
const MaxNetworkWaitAttempts = 100000

// NetworkConfig defines how BOOTy configures network connectivity in the
// ramdisk. It supports multiple network modes: EVPN/BGP (via FRR or GoBGP),
// static IP assignment, LACP bonding, and DHCP fallback.
//
// The network stack is shared across all operating modes since connectivity
// is required before any mode-specific logic runs.
//
// Sub-sections are read based on the active network mode:
//   - BGP + EVPN sections: read when Mode is "gobgp" or "frr"
//   - Static section: read when Mode is "static"
//   - Bond/VLAN/VRF sections: read regardless of mode (layered on top)
type NetworkConfig struct {
	// Mode selects the network stack implementation.
	// Valid values: "gobgp", "frr", "static", "dhcp"
	// Default: "" (auto-detect: GoBGP > FRR > Static > DHCP based on which fields are set)
	Mode string `yaml:"mode" json:"mode"`

	// DNSResolvers is a comma-separated list of DNS server IPs.
	// Applied regardless of network mode.
	// Example: "8.8.8.8,8.8.4.4"
	// Default: ""
	DNSResolvers string `yaml:"dnsResolvers" json:"dnsResolvers"`

	// WaitMaxAttempts is the number of 20 second connectivity rounds in
	// CAPRF legacy compat mode, like NETWORK_WAIT_MAX_ATTEMPTS of the legacy
	// provisioner. Ignored outside legacy compat. Unset (nil) means 15
	// rounds, about 5 minutes. An explicit 0 means no round: the run fails
	// at once, like ${NETWORK_WAIT_MAX_ATTEMPTS:-15} with the value 0.
	WaitMaxAttempts *int `yaml:"waitMaxAttempts,omitempty" json:"waitMaxAttempts,omitempty"`

	// PredictableNames renames ramdisk links to their predictable names
	// (systemd net_id, naming scheme v255: eno<index>, ens<slot>,
	// enp<bus>s<slot>, with f<function> and n<phys_port_name> or d<dev_port>)
	// when the netplan, frr.conf or vars name an interface that does not
	// exist, so a config written for the installed OS (enp1s0, ens1f0np0)
	// finds its links. Configs that only name existing links (eth0) see no
	// rename. false never renames.
	// Default: nil (true)
	PredictableNames *bool `yaml:"predictableNames,omitempty" json:"predictableNames,omitempty"`

	// Sub-sections grouped by network concern.
	BGP    BGPConfig    `yaml:"bgp"    json:"bgp"`
	EVPN   EVPNConfig   `yaml:"evpn"   json:"evpn"`
	Static StaticConfig `yaml:"static" json:"static"`
	Bond   BondConfig   `yaml:"bond"   json:"bond"`
	VLAN   VLANConfig   `yaml:"vlan"   json:"vlan"`
	VRF    VRFConfig    `yaml:"vrf"    json:"vrf"`
	IPMI   IPMIConfig   `yaml:"ipmi"   json:"ipmi"`
}

// BGPConfig holds BGP peering and tuning parameters.
// Read when network Mode is "gobgp" or "frr" (or auto-detected via ASN > 0).
type BGPConfig struct {
	// ASN is the BGP Autonomous System Number for the local machine.
	// Required for FRR/GoBGP modes. Setting ASN > 0 activates BGP networking.
	// Default: 0 (disables BGP)
	ASN uint32 `yaml:"asn" json:"asn"`

	// PeerMode controls how BGP neighbor sessions are established.
	// Valid values: "unnumbered" (default), "dual", "numbered"
	//   - unnumbered: link-local interface peers for all address families
	//   - dual: unnumbered for IPv4 unicast + numbered for L2VPN-EVPN
	//   - numbered: explicit neighbor IPs for all sessions
	// Default: "unnumbered"
	PeerMode string `yaml:"peerMode" json:"peerMode"`

	// Interfaces is a comma-separated list of interfaces used for
	// unnumbered BGP peering. When empty, all detected physical NICs are used.
	// Example: "eth1,eth2"
	// Default: ""
	Interfaces string `yaml:"interfaces" json:"interfaces"`

	// Neighbors is a comma-separated list of numbered BGP peer IPs.
	// Required when PeerMode is "dual" or "numbered".
	// Example: "10.0.0.1,10.0.0.2"
	// Default: ""
	Neighbors string `yaml:"neighbors" json:"neighbors"`

	// RemoteASN is the remote ASN for numbered peers.
	// 0 means iBGP (same AS as local).
	// Default: 0 (iBGP)
	RemoteASN uint32 `yaml:"remoteASN" json:"remoteASN"`

	// UnderlayAF is the underlay address family.
	// Valid value: "ipv4". Non-ipv4 values are rejected during config validation.
	// Default: "ipv4"
	UnderlayAF string `yaml:"underlayAF" json:"underlayAF"`

	// OverlayType is the overlay encapsulation type.
	// Valid values: "evpn-vxlan", "l3vpn", "none"
	// Default: "evpn-vxlan"
	OverlayType string `yaml:"overlayType" json:"overlayType"`

	// AuthPassword is the TCP-MD5 authentication password for all BGP peers.
	// Default: "" (no authentication)
	AuthPassword string `yaml:"authPassword" json:"authPassword"`

	// MinPeers is the minimum number of established BGP peers before
	// the underlay is considered ready for provisioning traffic.
	// Default: 1
	MinPeers int `yaml:"minPeers" json:"minPeers"`

	// Keepalive is the BGP keepalive interval in seconds.
	// Default: 0 (use FRR/GoBGP default, typically 60s)
	Keepalive uint32 `yaml:"keepalive" json:"keepalive"`

	// Hold is the BGP hold timer in seconds.
	// Default: 0 (use FRR/GoBGP default, typically 180s)
	Hold uint32 `yaml:"hold" json:"hold"`

	// BFDTransmitMS is the FRR-only BFD transmit interval in milliseconds.
	// BFD timers must be configured as a pair and are rejected for GoBGP mode.
	// Default: 0 (BFD disabled; typical value when enabled: 300)
	BFDTransmitMS uint32 `yaml:"bfdTransmitMS" json:"bfdTransmitMS"`

	// BFDReceiveMS is the FRR-only BFD receive interval in milliseconds.
	// BFD timers must be configured as a pair and are rejected for GoBGP mode.
	// Default: 0 (BFD disabled; typical value when enabled: 300)
	BFDReceiveMS uint32 `yaml:"bfdReceiveMS" json:"bfdReceiveMS"`
}

// EVPNConfig holds EVPN/VXLAN overlay parameters for BGP-based network modes.
// Read when BGP mode is active (ASN > 0).
type EVPNConfig struct {
	// UnderlaySubnet is the IPv4 subnet for underlay loopback IP derivation.
	// The machine's loopback IP is derived from its position in this subnet.
	// Example: "192.168.4.0/24"
	// Default: "" (use UnderlayIP directly instead)
	UnderlaySubnet string `yaml:"underlaySubnet" json:"underlaySubnet"`

	// UnderlayIP is the direct underlay loopback IP (alternative to subnet derivation).
	// If both UnderlaySubnet and UnderlayIP are set, UnderlayIP takes precedence.
	// Example: "192.168.4.10"
	// Default: ""
	UnderlayIP string `yaml:"underlayIP" json:"underlayIP"`

	// OverlaySubnet is the IPv6 subnet for overlay address assignment.
	// Example: "2a01:598:40a:5481::/64"
	// Default: ""
	OverlaySubnet string `yaml:"overlaySubnet" json:"overlaySubnet"`

	// ProvisionVNI is the VXLAN Network Identifier for the provisioning overlay network.
	// Default: 0
	ProvisionVNI uint32 `yaml:"provisionVNI" json:"provisionVNI"`

	// ProvisionIP is the IP address with CIDR mask to assign to the provisioning bridge.
	// Example: "10.100.0.20/24"
	// Default: ""
	ProvisionIP string `yaml:"provisionIP" json:"provisionIP"`

	// ProvisionGateway is the gateway VTEP IP for VXLAN BUM traffic flooding.
	// Default: ""
	ProvisionGateway string `yaml:"provisionGateway" json:"provisionGateway"`

	// L2Enabled enables Type-2 (MAC/IP) and Type-3 (multicast) route processing
	// for L2 overlay connectivity. Disabled by default.
	// Default: false
	L2Enabled bool `yaml:"l2Enabled" json:"l2Enabled"`

	// Type5Only enables direct EVPN Type-5 routing without an L2 gateway.
	// In this mode provisionGateway is optional; remote VTEPs are reached
	// through underlay BGP routes learned from the fabric.
	// Default: false
	Type5Only bool `yaml:"type5Only" json:"type5Only"`

	// DCGWIPs is a comma-separated list of Data Center Gateway IPs.
	// Used in OneFabric topologies for external routing.
	// Default: ""
	DCGWIPs string `yaml:"dcgwIPs" json:"dcgwIPs"`

	// LeafASN is the BGP ASN of the leaf switches in the fabric.
	// Default: 0
	LeafASN uint32 `yaml:"leafASN" json:"leafASN"`

	// LocalASN is the local AS number for leaf-facing connections.
	// Default: 0
	LocalASN uint32 `yaml:"localASN" json:"localASN"`

	// OverlayAggregate is the route aggregate prefix for the overlay network.
	// Advertised as a summary route to reduce routing table size.
	// Default: ""
	OverlayAggregate string `yaml:"overlayAggregate" json:"overlayAggregate"`

	// VPNRT is the VPN route target for EVPN route import/export filtering.
	// Default: ""
	VPNRT string `yaml:"vpnRT" json:"vpnRT"`
}

// StaticConfig holds parameters for static IP network mode.
// Read when network Mode is "static" (or auto-detected when IP is set without BGP).
type StaticConfig struct {
	// IP is the IP address with CIDR mask to assign to the interface.
	// Setting this activates static mode (if BGP is not configured).
	// Example: "10.0.0.5/24"
	// Default: ""
	IP string `yaml:"ip" json:"ip"`

	// Gateway is the default gateway IP address.
	// Default: ""
	Gateway string `yaml:"gateway" json:"gateway"`

	// Iface is the network interface to configure.
	// If empty, the first physical NIC is auto-detected.
	// Default: "" (auto-detect)
	Iface string `yaml:"iface" json:"iface"`
}

// BondConfig holds LACP bonding parameters.
// Bonding is applied before the network mode setup — the bond device becomes
// the interface used by BGP/static/DHCP modes.
type BondConfig struct {
	// Interfaces is a comma-separated list of NICs to bond.
	// Setting this activates LACP bonding.
	// Example: "eth0,eth1"
	// Default: "" (no bonding)
	Interfaces string `yaml:"interfaces" json:"interfaces"`

	// Mode is the Linux bonding mode.
	// Default: "802.3ad" (LACP)
	Mode string `yaml:"mode" json:"mode"`
}

// VLANConfig holds 802.1Q VLAN interface configuration.
// VLANs are created before network mode setup, providing sub-interfaces
// that other modes can use.
type VLANConfig struct {
	// Config is a multi-VLAN specification string.
	// Format: "ID:parent[:address[:gateway]],..."
	// Example: "200:eno1:10.200.0.42/24,300:eno2"
	// Default: "" (no VLANs)
	Config string `yaml:"config" json:"config"`
}

// VRFConfig holds VRF (Virtual Routing and Forwarding) isolation parameters.
// When configured, BGP sessions and provisioning traffic run inside a VRF
// to avoid polluting the default routing table.
type VRFConfig struct {
	// Name is the VRF interface name.
	// Example: "Vrf_underlay"
	// Default: "" (no VRF, use default namespace)
	Name string `yaml:"name" json:"name"`

	// TableID is the routing table ID associated with the VRF.
	// Default: 1000
	TableID uint32 `yaml:"tableID" json:"tableID"`
}

// IPMIConfig holds IPMI/BMC management network parameters.
// Used for auto-detecting the IPMI MAC and IP from the system's BMC,
// which is then used for underlay IP offset calculation.
type IPMIConfig struct {
	// Subnet is the IPMI/BMC management network subnet.
	// Example: "172.30.0.0/24"
	// Default: ""
	Subnet string `yaml:"subnet" json:"subnet"`
}
