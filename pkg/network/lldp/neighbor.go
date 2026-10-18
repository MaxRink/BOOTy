package lldp

import (
	"strings"
	"time"
)

// Neighbor represents a discovered LLDP neighbor (switch).
type Neighbor struct {
	ChassisID string `json:"chassisID"`
	// ChassisIDSubtype and PortIDSubtype name the ID form: mac, ifname,
	// local, network-address, interface-alias, port-component,
	// chassis-component or agent-circuit-id. Junos sends the ifIndex as
	// "local" port ID by default; its port description then names the
	// port.
	ChassisIDSubtype string `json:"chassisIDSubtype,omitempty"`
	PortID           string `json:"portID"`
	PortIDSubtype    string `json:"portIDSubtype,omitempty"`
	PortDescription  string `json:"portDescription,omitempty"`
	SystemName       string `json:"systemName,omitempty"`
	Description      string `json:"description,omitempty"`
	// Capabilities and EnabledCapabilities: bridge, router, station, ...
	Capabilities        []string `json:"capabilities,omitempty"`
	EnabledCapabilities []string `json:"enabledCapabilities,omitempty"`
	// MgmtAddresses holds the management addresses (IPv4, IPv6, MAC).
	MgmtAddresses []string `json:"mgmtAddresses,omitempty"`
	// VLAN is the 802.1 port VLAN ID, 0 when not sent.
	VLAN      uint16   `json:"vlan,omitempty"`
	VLANNames []string `json:"vlanNames,omitempty"` // "id:name"
	// LinkAggregation is the 802.1 or 802.3 link aggregation TLV.
	LinkAggregation *LinkAggregation `json:"linkAggregation,omitempty"`
	// MACPHY is the 802.3 MAC/PHY configuration/status TLV.
	MACPHY *MACPHY `json:"macPhy,omitempty"`
	// MaxFrameSize is the 802.3 maximum frame size, 0 when not sent.
	MaxFrameSize uint16 `json:"maxFrameSize,omitempty"`
	// Inventory holds the LLDP-MED inventory TLVs.
	Inventory *Inventory `json:"inventory,omitempty"`
	Interface string     `json:"interface"` // local interface where LLDP was received
	TTL       uint16     `json:"ttl"`
	// FirstSeen is when this neighbor (chassis and port ID) appeared on the
	// interface, LastSeen its latest frame. Changes counts the times the
	// neighbor of the interface changed to another chassis, port or name.
	FirstSeen time.Time `json:"firstSeen,omitzero"`
	LastSeen  time.Time `json:"lastSeen,omitzero"`
	Changes   int       `json:"changes,omitempty"`
}

// LinkAggregation is the link aggregation TLV of a neighbor port.
type LinkAggregation struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	PortID    uint32 `json:"portID,omitempty"`
}

// MACPHY is the 802.3 MAC/PHY configuration/status TLV: autonegotiation
// and the operational MAU type (RFC 4836), which gives speed and duplex.
type MACPHY struct {
	AutonegSupported bool   `json:"autonegSupported"`
	AutonegEnabled   bool   `json:"autonegEnabled"`
	MAUType          uint16 `json:"mauType"`
}

// Inventory is the LLDP-MED inventory of a neighbor.
type Inventory struct {
	HardwareRevision string `json:"hardwareRevision,omitempty"`
	FirmwareRevision string `json:"firmwareRevision,omitempty"`
	SoftwareRevision string `json:"softwareRevision,omitempty"`
	SerialNumber     string `json:"serialNumber,omitempty"`
	Manufacturer     string `json:"manufacturer,omitempty"`
	Model            string `json:"model,omitempty"`
	AssetID          string `json:"assetID,omitempty"`
}

// sameNeighbor reports whether two frames come from the same neighbor.
func sameNeighbor(a, b *Neighbor) bool {
	return a.ChassisID == b.ChassisID && a.PortID == b.PortID && a.SystemName == b.SystemName
}

// SwitchPort returns the name of the neighbor's port: the port
// description when the port ID is a number in a "local" port ID (the SNMP
// ifIndex that Junos sends by default), else the port ID.
func (n *Neighbor) SwitchPort() string {
	ifIndex := n.PortIDSubtype == "local" && n.PortID != "" && strings.Trim(n.PortID, "0123456789") == ""
	if ifIndex && n.PortDescription != "" {
		return n.PortDescription
	}
	return n.PortID
}
