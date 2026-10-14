// Package netplan parses netplan YAML and FRR configuration files from
// provisioner file-system overlays, enabling BOOTy to auto-detect network
// settings supplied by upstream orchestrators (e.g. t-co).
package netplan

// Config is the top-level netplan YAML structure.
type Config struct {
	Network NetworkSection `yaml:"network"`
}

// NetworkSection is the "network:" block inside a netplan YAML file.
type NetworkSection struct {
	Version      int                       `yaml:"version,omitempty"`
	Renderer     string                    `yaml:"renderer,omitempty"`
	Ethernets    map[string]EthernetConfig `yaml:"ethernets,omitempty"`
	Bonds        map[string]BondConfig     `yaml:"bonds,omitempty"`
	Tunnels      map[string]TunnelConfig   `yaml:"tunnels,omitempty"`
	Bridges      map[string]BridgeConfig   `yaml:"bridges,omitempty"`
	VLANs        map[string]VLANConfig     `yaml:"vlans,omitempty"`
	DummyDevices map[string]DummyConfig    `yaml:"dummy-devices,omitempty"`
	VRFs         map[string]VRFConfig      `yaml:"vrfs,omitempty"`
}

// EthernetConfig describes an ethernet interface.
type EthernetConfig struct {
	Match                       *MatchConfig  `yaml:"match,omitempty"`
	DHCP4                       *bool         `yaml:"dhcp4,omitempty"`
	DHCP6                       *bool         `yaml:"dhcp6,omitempty"`
	Addresses                   []string      `yaml:"addresses,omitempty"`
	Nameservers                 *DNSConfig    `yaml:"nameservers,omitempty"`
	MTU                         int           `yaml:"mtu,omitempty"`
	LinkLocal                   []string      `yaml:"link-local,omitempty"`
	AcceptRA                    *bool         `yaml:"accept-ra,omitempty"`
	EmitLLDP                    *bool         `yaml:"emit-lldp,omitempty"`
	IgnoreCarrier               *bool         `yaml:"ignore-carrier,omitempty"`
	Routes                      []RouteConfig `yaml:"routes,omitempty"`
	VirtualFunctionCount        int           `yaml:"virtual-function-count,omitempty"`
	EmbeddedSwitchMode          string        `yaml:"embedded-switch-mode,omitempty"`
	DelayVirtualFunctionsRebind *bool         `yaml:"delay-virtual-functions-rebind,omitempty"`
	IPv6AddressGeneration       string        `yaml:"ipv6-address-generation,omitempty"`
	SetName                     string        `yaml:"set-name,omitempty"`
	// Optional is accepted and ignored: it only changes whether
	// systemd-networkd-wait-online waits for the device.
	Optional *bool `yaml:"optional,omitempty"`
}

// MatchConfig matches physical devices by name or MAC.
type MatchConfig struct {
	Name string `yaml:"name,omitempty"`
	MAC  string `yaml:"macaddress,omitempty"`
}

// DNSConfig holds nameserver configuration.
type DNSConfig struct {
	Addresses []string `yaml:"addresses,omitempty"`
	Search    []string `yaml:"search,omitempty"`
}

// RouteConfig describes a static route.
type RouteConfig struct {
	To     string `yaml:"to"`
	Via    string `yaml:"via,omitempty"`
	From   string `yaml:"from,omitempty"`
	Metric int    `yaml:"metric,omitempty"`
	Scope  string `yaml:"scope,omitempty"`
	Table  int    `yaml:"table,omitempty"`
}

// BondConfig describes a network bond.
type BondConfig struct {
	Interfaces    []string        `yaml:"interfaces,omitempty"`
	MTU           int             `yaml:"mtu,omitempty"`
	AcceptRA      *bool           `yaml:"accept-ra,omitempty"`
	IgnoreCarrier *bool           `yaml:"ignore-carrier,omitempty"`
	Addresses     []string        `yaml:"addresses,omitempty"`
	Parameters    *BondParameters `yaml:"parameters,omitempty"`
	// Optional is accepted and ignored: it only changes whether
	// systemd-networkd-wait-online waits for the device.
	Optional *bool `yaml:"optional,omitempty"`
}

// BondParameters holds bond tuning parameters.
type BondParameters struct {
	Mode               string `yaml:"mode,omitempty"`
	LACPRate           string `yaml:"lacp-rate,omitempty"`
	TransmitHashPolicy string `yaml:"transmit-hash-policy,omitempty"`
	MIIMonitorInterval int    `yaml:"mii-monitor-interval,omitempty"`
}

// TunnelConfig describes a tunnel (VXLAN, GRE, etc.).
type TunnelConfig struct {
	Mode            string   `yaml:"mode,omitempty"`
	ID              int      `yaml:"id,omitempty"`
	Local           string   `yaml:"local,omitempty"`
	Remote          string   `yaml:"remote,omitempty"`
	Port            int      `yaml:"port,omitempty"`
	Link            string   `yaml:"link,omitempty"`
	MACAddress      string   `yaml:"macaddress,omitempty"`
	MTU             int      `yaml:"mtu,omitempty"`
	LinkLocal       []string `yaml:"link-local,omitempty"`
	Hairpin         *bool    `yaml:"hairpin,omitempty"`
	MACLearning     *bool    `yaml:"mac-learning,omitempty"`
	ARPProxy        *bool    `yaml:"arp-proxy,omitempty"`
	PortMACLearning *bool    `yaml:"port-mac-learning,omitempty"`
	AcceptRA        *bool    `yaml:"accept-ra,omitempty"`
	IgnoreCarrier   *bool    `yaml:"ignore-carrier,omitempty"`
	// Optional is accepted and ignored: it only changes whether
	// systemd-networkd-wait-online waits for the device.
	Optional *bool `yaml:"optional,omitempty"`
}

// BridgeConfig describes a network bridge.
type BridgeConfig struct {
	Interfaces    []string      `yaml:"interfaces,omitempty"`
	Addresses     []string      `yaml:"addresses,omitempty"`
	MTU           int           `yaml:"mtu,omitempty"`
	LinkLocal     []string      `yaml:"link-local,omitempty"`
	Parameters    *BridgeParams `yaml:"parameters,omitempty"`
	MACAddress    string        `yaml:"macaddress,omitempty"`
	AcceptRA      *bool         `yaml:"accept-ra,omitempty"`
	IgnoreCarrier *bool         `yaml:"ignore-carrier,omitempty"`
	// Optional is accepted and ignored: it only changes whether
	// systemd-networkd-wait-online waits for the device.
	Optional *bool `yaml:"optional,omitempty"`
}

// BridgeParams holds bridge-specific parameters.
type BridgeParams struct {
	STP *bool `yaml:"stp,omitempty"`
}

// VLANConfig describes a VLAN sub-interface.
type VLANConfig struct {
	ID          int           `yaml:"id,omitempty"`
	Link        string        `yaml:"link,omitempty"`
	DHCP4       *bool         `yaml:"dhcp4,omitempty"`
	Addresses   []string      `yaml:"addresses,omitempty"`
	MTU         int           `yaml:"mtu,omitempty"`
	Nameservers *DNSConfig    `yaml:"nameservers,omitempty"`
	Routes      []RouteConfig `yaml:"routes,omitempty"`
	// Optional is accepted and ignored: it only changes whether
	// systemd-networkd-wait-online waits for the device.
	Optional *bool `yaml:"optional,omitempty"`
}

// DummyConfig describes a dummy (virtual loopback) device.
type DummyConfig struct {
	Addresses []string `yaml:"addresses,omitempty"`
	MTU       int      `yaml:"mtu,omitempty"`
}

// VRFConfig describes a Virtual Routing and Forwarding instance.
type VRFConfig struct {
	Table      int      `yaml:"table,omitempty"`
	Interfaces []string `yaml:"interfaces,omitempty"`
}
