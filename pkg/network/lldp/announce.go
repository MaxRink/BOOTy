//go:build linux

package lldp

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/google/gopacket/layers"

	"github.com/telekom/BOOTy/pkg/buildinfo"
	"github.com/telekom/BOOTy/pkg/network/ethtool"
)

// System is the host data of the standard and full announce levels.
type System struct {
	Version, Mode                          string
	Vendor, Product, UUID                  string
	BoardVersion, BIOSVersion, Serial, Tag string
}

// sysDMI and sysClassNet are the sysfs paths; test seams.
var (
	sysDMI      = "/sys/class/dmi/id"
	sysClassNet = "/sys/class/net"
)

// runMode is the operating mode for the system description.
var runMode atomic.Pointer[string]

// SetRunMode sets the operating mode (MODE) that the standard and full
// levels send.
func SetRunMode(mode string) { runMode.Store(&mode) }

func readSystem() System {
	dmi := func(name string) string {
		b, err := os.ReadFile(filepath.Join(sysDMI, name)) //nolint:gosec // sysfs path
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	s := System{
		Version: buildinfo.Get().Version, Vendor: dmi("sys_vendor"), Product: dmi("product_name"),
		UUID: dmi("product_uuid"), BoardVersion: dmi("board_version"), BIOSVersion: dmi("bios_version"),
		Serial: dmi("product_serial"), Tag: dmi("chassis_asset_tag"),
	}
	if m := runMode.Load(); m != nil {
		s.Mode = *m
	}
	return s
}

// port is what the standard level tells about a NIC.
type port struct {
	Driver, PCI string
	MTU         int
	Addrs       []net.IP
	Autoneg     *bool // nil when the driver does not report it
	Index       int
}

// portFacts reads the facts of a NIC; a test seam.
var portFacts = func(ifname string) port {
	p := port{Driver: ethtool.Driver(ifname)}
	if dev, err := os.Readlink(filepath.Join(sysClassNet, ifname, "device")); err == nil {
		p.PCI = filepath.Base(dev)
	}
	if ifi, err := net.InterfaceByName(ifname); err == nil {
		p.MTU, p.Index = ifi.MTU, ifi.Index
		if addrs, err := ifi.Addrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					p.Addrs = append(p.Addrs, n.IP)
				}
			}
		}
	}
	if m, err := ethtool.GetLinkModes(ifname); err == nil {
		p.Autoneg = &m.Autoneg
	}
	return p
}

// maxLLDPDU is the largest LLDPDU that fits one Ethernet frame;
// maxMgmtIPs bounds the management address TLVs (plus the MAC).
const (
	maxLLDPDU  = 1500
	maxMgmtIPs = 4
)

// announce replaces the descriptions of the base TLVs and adds the TLVs of
// the standard and full levels. Every field is bounded (strings 255 bytes,
// MED strings 32, at most maxMgmtIPs addresses), so the LLDPDU stays below
// maxLLDPDU with a 32-digit machine ID and an interface name of at most 15
// bytes (TestFrameBudget).
func announce(base []layers.LinkLayerDiscoveryValue, ifname string, mac net.HardwareAddr, info *Info) []layers.LinkLayerDiscoveryValue {
	p := portFacts(ifname)
	out := []layers.LinkLayerDiscoveryValue{tlv(layers.LLDPTLVPortDescription, strings.Join(nonEmpty(ifname, p.Driver, p.PCI), " "))}
	for _, v := range base {
		switch v.Type {
		case layers.LLDPTLVPortDescription, layers.LLDPTLVSysDescription:
		case layers.LLDPTLVSysCapabilities:
			out = append(out, tlv(layers.LLDPTLVSysDescription, systemDescription(info)), v)
		default:
			out = append(out, v)
		}
	}
	for _, ip := range p.Addrs[:min(len(p.Addrs), maxMgmtIPs)] {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, mgmtTLV(1, v4, p.Index))
		} else {
			out = append(out, mgmtTLV(2, ip.To16(), p.Index))
		}
	}
	out = append(out, mgmtTLV(6, mac, p.Index), macPHY(p.Autoneg))
	if p.MTU > 0 {
		out = append(out, orgTLV(layers.IEEEOUI8023, 4, binary.BigEndian.AppendUint16(nil, uint16(p.MTU+18)))) //nolint:gosec // MTU < 65518
	}
	if info.Announce == AnnounceFull {
		out = append(out, medTLVs(&info.System)...)
	}
	return out
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// systemDescription is "BOOTy <version> mode=<mode>"; the full level adds
// the host identity. Without an OUI that Telekom owns, this data goes
// into the system description instead of organizationally specific TLVs.
func systemDescription(info *Info) string {
	s := info.System
	parts := []string{"BOOTy " + s.Version}
	if s.Mode != "" {
		parts = append(parts, "mode="+s.Mode)
	}
	if info.Announce == AnnounceFull {
		if p := strings.TrimSpace(s.Vendor + " " + s.Product); p != "" {
			parts = append(parts, "product="+strings.ReplaceAll(p, " ", "_"))
		}
		if s.UUID != "" {
			parts = append(parts, "uuid="+s.UUID)
		}
	}
	return strings.Join(parts, " ")
}

// mgmtTLV is a management address TLV (IEEE 802.1AB 8.5.9) of an IANA
// address family (1 IPv4, 2 IPv6, 6 MAC) with the ifIndex of the NIC and
// no OID.
func mgmtTLV(family byte, addr []byte, ifIndex int) layers.LinkLayerDiscoveryValue {
	v := append([]byte{byte(1 + len(addr)), family}, addr...)
	v = append(v, 2)                                                                                         // ifIndex numbering
	v = binary.BigEndian.AppendUint32(v, uint32(ifIndex))                                                    //nolint:gosec // ifIndex fits
	v = append(v, 0)                                                                                         // no OID
	return layers.LinkLayerDiscoveryValue{Type: layers.LLDPTLVMgmtAddress, Length: uint16(len(v)), Value: v} //nolint:gosec // < 64
}

// macPHY is the 802.3 MAC/PHY configuration/status TLV: autonegotiation
// supported and enabled as the driver reports it. The advertised
// capabilities and the operational MAU type are 0 (unknown).
func macPHY(autoneg *bool) layers.LinkLayerDiscoveryValue {
	var status byte
	if autoneg != nil {
		status = 1 // supported
		if *autoneg {
			status |= 2 // enabled
		}
	}
	return orgTLV(layers.IEEEOUI8023, 1, []byte{status, 0, 0, 0, 0})
}

// medTLVs are the LLDP-MED capabilities (capabilities and inventory,
// endpoint class I) and inventory TLVs (TIA-1057 10.2.6), at most 32
// bytes each. TIA-1057 sends them only after hearing LLDP-MED; BOOTy
// sends them at once (full level), like lldpd -M.
func medTLVs(s *System) []layers.LinkLayerDiscoveryValue {
	out := []layers.LinkLayerDiscoveryValue{orgTLV(layers.IEEEOUIMedia, 1, []byte{0x00, 0x21, 0x01})}
	for _, f := range []struct {
		subtype byte
		value   string
	}{
		{5, s.BoardVersion}, {6, s.BIOSVersion}, {7, "BOOTy " + s.Version}, {8, s.Serial},
		{9, s.Vendor}, {10, s.Product}, {11, s.Tag},
	} {
		if v := sanitizeLLDP(f.value); v != "" {
			out = append(out, orgTLV(layers.IEEEOUIMedia, f.subtype, []byte(v[:min(len(v), 32)])))
		}
	}
	return out
}

// orgTLV is an organizationally specific TLV.
func orgTLV(oui layers.IEEEOUI, subtype byte, data []byte) layers.LinkLayerDiscoveryValue {
	v := append([]byte{byte(oui >> 16), byte(oui >> 8), byte(oui), subtype}, data...)
	return layers.LinkLayerDiscoveryValue{Type: layers.LLDPTLVOrgSpecific, Length: uint16(len(v)), Value: v} //nolint:gosec // < 64
}
