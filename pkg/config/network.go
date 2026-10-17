package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/netplan"
)

// MaxNetworkWaitAttempts bounds NETWORK_WAIT_MAX_ATTEMPTS (about 23 days of
// 20 second rounds), so the wait time cannot overflow.
const MaxNetworkWaitAttempts = 100000

// Values of NetworkConfig.WaitMode.
const (
	NetworkWaitDefault = "default"
	NetworkWaitRounds  = "rounds"
)

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

	// FRRStrict and NetplanStrict make a provisioner frr.conf statement or
	// netplan key that BOOTy does not implement fail the network setup.
	// false logs each one with its line and skips it.
	// Default: nil (true)
	FRRStrict     *bool `yaml:"frrStrict,omitempty" json:"frrStrict,omitempty"`
	NetplanStrict *bool `yaml:"netplanStrict,omitempty" json:"netplanStrict,omitempty"`

	// WaitMode selects the network connectivity wait.
	// "default": up to 3 network setups, each waiting up to 5 minutes.
	// "rounds": like wait_for_network of the CAPRF legacy provisioner, one
	// network setup and WaitMaxAttempts rounds of 20 seconds; in FRR mode
	// FRR restarts after each failed round unless a BGP session is
	// established.
	// Valid values: "default", "rounds". Preset by legacyCompat: "rounds".
	// Default: "" (default)
	WaitMode string `yaml:"waitMode" json:"waitMode"`

	// WaitMaxAttempts is the number of 20 second connectivity rounds with
	// WaitMode "rounds", like NETWORK_WAIT_MAX_ATTEMPTS of the legacy
	// provisioner. Ignored with the default wait. Unset (nil) means 15
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

	// NICWaitSeconds is the longest wait for the NICs that the netplan,
	// frr.conf or vars name and that do not exist yet (a driver that brings
	// up its ports late). After it the setup goes on with the NICs it has.
	// -1 does not wait.
	// Default: 0 (30 seconds)
	NICWaitSeconds int `yaml:"nicWaitSeconds,omitempty" json:"nicWaitSeconds,omitempty"`

	// Overlay holds the device settings of the provision VXLAN device and
	// bridge (GoBGP), like the netplan tunnel and bridge keys. The netplan
	// values win.
	Overlay network.OverlayDevices `yaml:"overlay,omitempty" json:"overlay,omitempty"`

	// Devices is the native form of the provisioner netplan: the same keys
	// as its "network:" section (ethernets with match, set-name, addresses,
	// mtu, accept-ra, link-local, routes and nameservers; vrfs,
	// dummy-devices, tunnels, bridges, bonds, vlans). Set, it replaces the
	// provisioner netplan files as a whole and goes through the same
	// parser, so both forms give the same network config. It is always
	// strict, also when the rest of the config is loaded leniently.
	Devices *NetworkDevices `yaml:"devices,omitempty" json:"devices,omitempty"`

	// NoAcceptRA is a comma-separated list of NICs that ignore IPv6 router
	// advertisements, like netplan "accept-ra: false".
	NoAcceptRA string `yaml:"noAcceptRA" json:"noAcceptRA"`

	// EmitLLDP is a comma-separated list of NICs that send LLDP frames like
	// systemd-networkd with netplan "emit-lldp: true" (GoBGP).
	EmitLLDP string `yaml:"emitLLDP" json:"emitLLDP"`

	// IPForwarding and IPv6Forwarding set the forwarding sysctls (GoBGP).
	// Unset (nil) means the frr.conf "[no] ip[v6] forwarding", else IPv4 on
	// and IPv6 off, like the legacy ramdisk.
	IPForwarding   *bool `yaml:"ipForwarding,omitempty" json:"ipForwarding,omitempty"`
	IPv6Forwarding *bool `yaml:"ipv6Forwarding,omitempty" json:"ipv6Forwarding,omitempty"`

	// MTU is the MTU of the fabric NICs and the underlay dummy (GoBGP);
	// the VXLAN MTU is 50 less. The largest netplan "mtu" wins.
	// Default: 0 (9000)
	MTU int `yaml:"mtu" json:"mtu"`

	// LoopbackAddresses is a comma-separated list of CIDR addresses that
	// GoBGP adds to lo, like netplan "lo: addresses".
	// Example: "2001:db8::17/128"
	// Default: ""
	LoopbackAddresses string `yaml:"loopbackAddresses" json:"loopbackAddresses"`

	// BlackholeRoutes is a comma-separated list of prefixes that GoBGP
	// installs as blackhole routes in the main table, like FRR
	// "ipv6 route P blackhole".
	// Default: ""
	BlackholeRoutes string `yaml:"blackholeRoutes" json:"blackholeRoutes"`

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

	// Sessions are the BGP sessions of GoBGP mode, the native form of the
	// frr.conf neighbors; PeerGroups are their peer-groups. Defaults is the
	// timer profile, "traditional" or "datacenter" (the default), like
	// "frr defaults". A non-empty Sessions list replaces the frr.conf
	// sessions as a whole.
	Sessions   []BGPSessionConfig `yaml:"sessions,omitempty" json:"sessions,omitempty"`
	PeerGroups []BGPSessionConfig `yaml:"peerGroups,omitempty" json:"peerGroups,omitempty"`
	Defaults   string             `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	// BFDProfiles are the BFD profiles of the sessions' bfdProfile, like
	// the frr.conf "bfd / profile" blocks. A non-empty list replaces the
	// frr.conf profiles as a whole.
	BFDProfiles []BFDProfileConfig `yaml:"bfdProfiles,omitempty" json:"bfdProfiles,omitempty"`

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

	// ConnectRetry is the BGP connect retry timer in seconds (GoBGP). An
	// explicit value also replaces the connect timer of frr.conf peers.
	// Default: 0 (5s, or the frr.conf value)
	ConnectRetry uint32 `yaml:"connectRetry" json:"connectRetry"`

	// LocalAS is the local AS that GoBGP sends to the unnumbered (eBGP)
	// peers, like FRR "neighbor X local-as N no-prepend replace-as". An
	// explicit value also replaces the local-as of frr.conf eBGP peers.
	// Default: 0 (the router ASN, or the frr.conf value)
	LocalAS uint32 `yaml:"localAS" json:"localAS"`

	// NeighborVRF binds the numbered peers to a VRF device (GoBGP), like
	// FRR "router bgp N vrf NAME". An explicit value also replaces the VRF
	// of frr.conf numbered peers.
	// Default: "" (no VRF, or the frr.conf value)
	NeighborVRF string `yaml:"neighborVRF" json:"neighborVRF"`

	// AllowASIn is how often the own ASN may be in a received AS path, like
	// FRR "allowas-in N" (1 to 10, 0 turns it off). Unset (nil) keeps the
	// frr.conf value, or off without frr.conf.
	AllowASIn *uint32 `yaml:"allowASIn,omitempty" json:"allowASIn,omitempty"`

	// MultipathRelax is FRR "bgp bestpath as-path multipath-relax": paths
	// with different AS paths of the same length are equal cost. Unset
	// (nil) keeps the frr.conf value, or off without frr.conf.
	MultipathRelax *bool `yaml:"multipathRelax,omitempty" json:"multipathRelax,omitempty"`

	// GracefulRestart sends the graceful restart and long-lived graceful
	// restart capabilities like the FRR default helper mode (GoBGP). Unset
	// (nil) means on.
	GracefulRestart *bool `yaml:"gracefulRestart,omitempty" json:"gracefulRestart,omitempty"`
	// GracefulRestartTime is the graceful restart time in seconds, 1 to
	// 4095. 0 means 120, like FRR.
	GracefulRestartTime uint32 `yaml:"gracefulRestartTime,omitempty" json:"gracefulRestartTime,omitempty"`
	// AddPathReceive sends the add-path capability with receive in every
	// address family, like FRR (GoBGP). Unset (nil) means on.
	AddPathReceive *bool `yaml:"addPathReceive,omitempty" json:"addPathReceive,omitempty"`
	// SoftwareVersion also sends the software version capability to eBGP
	// peers, like FRR "neighbor X capability software-version" (GoBGP).
	// iBGP peers always get it. Unset (nil) follows the profile like FRR
	// 10.3: on with datacenter, off with traditional.
	SoftwareVersion *bool `yaml:"softwareVersion,omitempty" json:"softwareVersion,omitempty"`
	// Hostname is the host name in the FQDN capability (GoBGP). GoBGP
	// sends the system host name, so BOOTy sets it as the ramdisk host name
	// before BGP starts. Empty keeps the system host name, like FRR.
	Hostname string `yaml:"hostname,omitempty" json:"hostname,omitempty"`

	// InstallRoutes installs the learned IPv4/IPv6 unicast routes into the
	// kernel, like zebra (GoBGP). Unset (nil) means on with frr.conf
	// sessions and off without them.
	InstallRoutes *bool `yaml:"installRoutes,omitempty" json:"installRoutes,omitempty"`

	// FastExternalFailover stops an unnumbered session when its interface
	// loses carrier, like FRR "bgp fast-external-failover" (GoBGP). Unset
	// (nil) means on with frr.conf sessions and off without them.
	FastExternalFailover *bool `yaml:"fastExternalFailover,omitempty" json:"fastExternalFailover,omitempty"`

	// BFDTransmitMS is the BFD transmit interval in milliseconds. Set
	// together with BFDReceiveMS it turns BFD on for the FRR stack and for
	// the native GoBGP peers, and replaces the interval of frr.conf BFD
	// sessions.
	// Default: 0 (BFD off; FRR's default is 300)
	BFDTransmitMS uint32 `yaml:"bfdTransmitMS" json:"bfdTransmitMS"`

	// BFDReceiveMS is the BFD receive interval in milliseconds, see
	// BFDTransmitMS.
	// Default: 0 (BFD off; FRR's default is 300)
	BFDReceiveMS uint32 `yaml:"bfdReceiveMS" json:"bfdReceiveMS"`

	// RouteMaps and CommunityLists define route-maps like FRR, with the
	// subset BOOTy implements (permit/deny entries in sequence order,
	// "match community", "set local-preference", "set community
	// [additive]", implicit deny). A non-empty list replaces the
	// provisioner frr.conf list as a whole.
	RouteMaps      []network.RouteMap      `yaml:"routeMaps,omitempty" json:"routeMaps,omitempty"`
	CommunityLists []network.CommunityList `yaml:"communityLists,omitempty" json:"communityLists,omitempty"`

	// RouteMapIn and RouteMapOut apply to every peer in every address
	// family, like "neighbor X route-map M in|out". An explicit value also
	// replaces the frr.conf route-maps of the sessions.
	RouteMapIn  string `yaml:"routeMapIn" json:"routeMapIn"`
	RouteMapOut string `yaml:"routeMapOut" json:"routeMapOut"`

	// EBGPRequiresPolicy is FRR "bgp ebgp-requires-policy": an eBGP peer
	// without a route-map in a family and direction exchanges no routes
	// there. Unset means on for frr.conf and network.bgp.sessions
	// sessions (unless frr.conf has "no bgp ebgp-requires-policy") and off
	// for the BGP_PEER_MODE peers. A set value applies to all of them.
	EBGPRequiresPolicy *bool `yaml:"ebgpRequiresPolicy,omitempty" json:"ebgpRequiresPolicy,omitempty"`

	// BFDDetectMultiplier is the BFD detect multiplier (GoBGP), 2 to 255.
	// An explicit value replaces the multiplier of frr.conf BFD sessions.
	// Default: 0 (3, or the frr.conf profile value)
	BFDDetectMultiplier uint32 `yaml:"bfdDetectMultiplier" json:"bfdDetectMultiplier"`
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
	// Unset (nil) means Type-5 only when the provisioner frr.conf maps the
	// provision VNI to a VRF (L3VNI), like FRR; an explicit value wins.
	// Default: unset (false without an L3VNI)
	Type5Only *bool `yaml:"type5Only,omitempty" json:"type5Only,omitempty"`

	// ImportRouteTargets and ExportRouteTargets are comma-separated EVPN
	// route targets ("ASN:value"), like FRR "route-target import|export".
	// They win over vpnRT and over the provisioner frr.conf.
	// Default: "" (the frr.conf value, else vpnRT, else ASN:VNI)
	ImportRouteTargets string `yaml:"importRouteTargets" json:"importRouteTargets"`
	ExportRouteTargets string `yaml:"exportRouteTargets" json:"exportRouteTargets"`

	// Type5Networks is a comma-separated list of prefixes that BOOTy
	// originates as EVPN Type-5 routes, like FRR "network" lines with
	// "advertise ipv4|ipv6 unicast". Only a prefix of a loopback address,
	// of provisionIP or of a blackhole route is advertised (import check).
	// Example: "2001:db8::17/128,2001:db8::/64"
	// Default: ""
	Type5Networks string `yaml:"type5Networks" json:"type5Networks"`

	// BridgeName is the provision bridge device name.
	// Default: "" ("br.provision")
	BridgeName string `yaml:"bridgeName" json:"bridgeName"`

	// VXLANName is the provision VXLAN device name.
	// Default: "" ("vx<VNI>")
	VXLANName string `yaml:"vxlanName" json:"vxlanName"`

	// BridgeMAC is the provision bridge MAC, the EVPN router MAC.
	// Default: "" (derived per host)
	BridgeMAC string `yaml:"bridgeMAC" json:"bridgeMAC"`

	// VXLANLink is the device that the VXLAN device sends from, like netplan
	// tunnel "link".
	// Default: "" (the underlay dummy with a VRF, else the kernel choice)
	VXLANLink string `yaml:"vxlanLink" json:"vxlanLink"`

	// UnderlayDummyName is the underlay dummy device name.
	// Default: "" ("dummy.underlay")
	UnderlayDummyName string `yaml:"underlayDummyName" json:"underlayDummyName"`

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

	// OverlayName is the VRF of the provision bridge (GoBGP). "default"
	// keeps the bridge in the default VRF, like the bm4x netplan.
	// Default: "" (the underlay VRF Name)
	OverlayName string `yaml:"overlayName" json:"overlayName"`

	// OverlayTableID is the routing table of OverlayName.
	// Default: 0 (TableID)
	OverlayTableID uint32 `yaml:"overlayTableID" json:"overlayTableID"`
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

// NetworkDevices is network.devices: the "network:" section of a netplan
// file. Decoding it runs the strict netplan parser, like NETWORK_DEVICES
// and the provisioner files, so an unknown key fails even when Load is
// not strict.
type NetworkDevices netplan.NetworkSection

// UnmarshalYAML parses the node as a strict netplan "network:" section.
func (d *NetworkDevices) UnmarshalYAML(node *yaml.Node) error {
	b, err := yaml.Marshal(map[string]*yaml.Node{"network": node})
	if err != nil {
		return fmt.Errorf("network.devices: %w", err)
	}
	return d.parse(b)
}

// UnmarshalJSON parses a JSON object as a strict netplan "network:"
// section (JSON is YAML).
func (d *NetworkDevices) UnmarshalJSON(b []byte) error {
	return d.parse(append([]byte("network: "), b...))
}

func (d *NetworkDevices) parse(b []byte) error {
	np, err := netplan.ParseBytesWith("network.devices", b, true)
	if err != nil {
		return err //nolint:wrapcheck // the netplan error names network.devices
	}
	*d = NetworkDevices(np.Network)
	return nil
}

// Section returns the devices as netplan section, nil when unset.
func (d *NetworkDevices) Section() *netplan.NetworkSection {
	return (*netplan.NetworkSection)(d)
}
