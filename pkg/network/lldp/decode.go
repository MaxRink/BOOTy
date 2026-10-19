//go:build linux

package lldp

import (
	"fmt"
	"net"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

// Names of the chassis ID subtypes (IEEE 802.1AB 8.5.2.2).
var chassisSubtypes = []string{1: "chassis-component", 2: "interface-alias", 3: "port-component",
	4: "mac", 5: "network-address", 6: "ifname", 7: "local"}

// Names of the port ID subtypes (IEEE 802.1AB 8.5.3.2).
var portSubtypes = []string{1: "interface-alias", 2: "port-component", 3: "mac",
	4: "network-address", 5: "ifname", 6: "agent-circuit-id", 7: "local"}

func subtypeName(names []string, s byte) string {
	if int(s) < len(names) && names[s] != "" {
		return names[s]
	}
	return fmt.Sprintf("%d", s)
}

// idString renders a chassis or port ID: a MAC as aa:bb:cc:dd:ee:ff, a
// network address (IANA family 1 or 2) as an IP, anything else as text.
func idString(subtypeName string, id []byte) string {
	switch subtypeName {
	case "mac":
		if len(id) == 6 {
			return net.HardwareAddr(id).String()
		}
	case "network-address":
		if len(id) == 5 && id[0] == 1 || len(id) == 17 && id[0] == 2 {
			return net.IP(id[1:]).String()
		}
	}
	return sanitizeLLDP(string(id))
}

// parseLLDP extracts the LLDP neighbor information from a raw LLDP
// payload. All strings are sanitized. The gopacket fork
// (docs/dependency-forks.md) drops an LLDPDU whose first three TLVs are
// not Chassis ID, Port ID and TTL in this order, each once (IEEE
// 802.1AB), and accepts one without End TLV (optional since
// 802.1AB-2016). The frame comes from the wire: a decoder panic keeps what
// was decoded so far instead of ending BOOTy.
func parseLLDP(data []byte, iface string) (n *Neighbor) {
	defer func() { _ = recover() }() // n keeps the partial neighbor
	pkt := gopacket.NewPacket(data, layers.LayerTypeLinkLayerDiscovery, gopacket.NoCopy)
	l, ok := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	if !ok {
		return nil
	}
	n = &Neighbor{
		Interface:        iface,
		TTL:              l.TTL,
		ChassisIDSubtype: subtypeName(chassisSubtypes, byte(l.ChassisID.Subtype)),
		PortIDSubtype:    subtypeName(portSubtypes, byte(l.PortID.Subtype)),
	}
	n.ChassisID = idString(n.ChassisIDSubtype, l.ChassisID.ID)
	n.PortID = idString(n.PortIDSubtype, l.PortID.ID)
	n.MgmtAddresses = mgmtAddresses(l.Values)

	info, ok := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if !ok {
		return n
	}
	n.SystemName = sanitizeLLDP(info.SysName)
	n.Description = sanitizeLLDP(info.SysDescription)
	n.PortDescription = sanitizeLLDP(info.PortDescription)
	if hasTLV(l.Values, layers.LLDPTLVSysCapabilities) {
		n.Capabilities = capabilityNames(info.SysCapabilities.SystemCap)
		n.EnabledCapabilities = capabilityNames(info.SysCapabilities.EnabledCap)
	}
	decodeOrg(info, n)
	return n
}

// decodeOrg reads the 802.1, 802.3 and LLDP-MED TLVs. A malformed TLV
// makes its decoder return an error (the gopacket fork,
// docs/dependency-forks.md), so the fields of that group stay empty.
func decodeOrg(info *layers.LinkLayerDiscoveryInfo, n *Neighbor) {
	if d, err := info.Decode8021(); err == nil {
		n.VLAN = d.PVID
		for _, v := range d.VLANNames {
			n.VLANNames = append(n.VLANNames, fmt.Sprintf("%d:%s", v.ID, sanitizeLLDP(v.Name)))
		}
		if hasOrg(info, layers.IEEEOUI8021, layers.LLDP8021SubtypeLinkAggregation) {
			n.LinkAggregation = &LinkAggregation{d.LinkAggregation.Supported, d.LinkAggregation.Enabled, d.LinkAggregation.PortID}
		}
	}
	if d, err := info.Decode8023(); err == nil {
		n.MaxFrameSize = d.MTU
		if hasOrg(info, layers.IEEEOUI8023, layers.LLDP8023SubtypeMACPHY) {
			m := d.MACPHYConfigStatus
			n.MACPHY = &MACPHY{AutonegSupported: m.AutoNegSupported, AutonegEnabled: m.AutoNegEnabled, MAUType: m.MAUType}
		}
		if hasOrg(info, layers.IEEEOUI8023, layers.LLDP8023SubtypeLinkAggregation) {
			n.LinkAggregation = &LinkAggregation{d.LinkAggregation.Supported, d.LinkAggregation.Enabled, d.LinkAggregation.PortID}
		}
	}
	if d, err := info.DecodeMedia(); err == nil {
		inv := Inventory{
			HardwareRevision: sanitizeLLDP(d.HardwareRevision), FirmwareRevision: sanitizeLLDP(d.FirmwareRevision),
			SoftwareRevision: sanitizeLLDP(d.SoftwareRevision), SerialNumber: sanitizeLLDP(d.SerialNumber),
			Manufacturer: sanitizeLLDP(d.Manufacturer), Model: sanitizeLLDP(d.Model), AssetID: sanitizeLLDP(d.AssetID),
		}
		if inv != (Inventory{}) {
			n.Inventory = &inv
		}
	}
}

func hasTLV(values []layers.LinkLayerDiscoveryValue, t layers.LLDPTLVType) bool {
	for _, v := range values {
		if v.Type == t {
			return true
		}
	}
	return false
}

func hasOrg(info *layers.LinkLayerDiscoveryInfo, oui layers.IEEEOUI, subtype uint8) bool {
	for _, o := range info.OrgTLVs {
		if o.OUI == oui && o.SubType == subtype {
			return true
		}
	}
	return false
}

// mgmtAddresses reads every management address TLV: gopacket keeps only
// the last one. Layout: length (subtype + address), subtype, address,
// interface numbering subtype, interface number, OID length, OID.
func mgmtAddresses(values []layers.LinkLayerDiscoveryValue) []string {
	var out []string
	for _, v := range values {
		if v.Type != layers.LLDPTLVMgmtAddress || len(v.Value) < 2 {
			continue
		}
		n := int(v.Value[0])
		if n < 2 || len(v.Value) < 1+n {
			continue
		}
		family, addr := v.Value[1], v.Value[2:1+n]
		switch {
		case family == 1 && len(addr) == 4, family == 2 && len(addr) == 16:
			out = append(out, net.IP(addr).String())
		case family == 6 && len(addr) == 6:
			out = append(out, net.HardwareAddr(addr).String())
		default:
			out = append(out, fmt.Sprintf("family-%d:%x", family, addr))
		}
	}
	return out
}

// capabilityNames lists the set capabilities.
func capabilityNames(c layers.LLDPCapabilities) []string {
	var out []string
	for _, f := range []struct {
		on   bool
		name string
	}{
		{c.Other, "other"}, {c.Repeater, "repeater"}, {c.Bridge, "bridge"}, {c.WLANAP, "wlan-ap"},
		{c.Router, "router"}, {c.Phone, "phone"}, {c.DocSis, "docsis"}, {c.StationOnly, "station"},
		{c.CVLAN, "c-vlan"}, {c.SVLAN, "s-vlan"}, {c.TMPR, "tpmr"},
	} {
		if f.on {
			out = append(out, f.name)
		}
	}
	return out
}
