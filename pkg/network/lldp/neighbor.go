package lldp

// Neighbor represents a discovered LLDP neighbor (switch).
type Neighbor struct {
	ChassisID   string `json:"chassisID"`
	PortID      string `json:"portID"`
	SystemName  string `json:"systemName,omitempty"`
	Description string `json:"description,omitempty"`
	Interface   string `json:"interface"` // local interface where LLDP was received
	TTL         uint16 `json:"ttl"`
}
