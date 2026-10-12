package frr

import (
	"fmt"
	"net"
	"strings"
	"text/template"
	"unicode"

	"github.com/telekom/BOOTy/pkg/network"
)

// DeriveAddresses computes underlay IP, overlay IP, and bridge MAC from
// IPMI information and network configuration.
func DeriveAddresses(cfg *network.Config) (underlayIP, overlayIP, bridgeMAC string, err error) {
	switch {
	case cfg.UnderlayIP != "":
		underlayIP = cfg.UnderlayIP
	case cfg.UnderlaySubnet != "" && cfg.IPMIIP != "" && cfg.IPMISubnet != "":
		underlayIP, err = DeriveIPFromOffset(cfg.IPMIIP, cfg.IPMISubnet, cfg.UnderlaySubnet)
		if err != nil {
			return "", "", "", fmt.Errorf("derive underlay IP: %w", err)
		}
	default:
		return "", "", "", fmt.Errorf("underlay IP or (underlay_subnet + ipmi_ip + ipmi_subnet) required")
	}

	if cfg.OverlaySubnet != "" && cfg.IPMIIP != "" && cfg.IPMISubnet != "" {
		overlayIP, err = DeriveIPFromOffset(cfg.IPMIIP, cfg.IPMISubnet, cfg.OverlaySubnet)
		if err != nil {
			return "", "", "", fmt.Errorf("derive overlay IP: %w", err)
		}
	} else {
		overlayIP = underlayIP
	}

	bridgeMAC, err = deriveBridgeMAC(cfg, underlayIP)
	if err != nil {
		return "", "", "", err
	}

	return underlayIP, overlayIP, bridgeMAC, nil
}

// DeriveIPFromOffset calculates a target IP by applying the host offset
// from sourceIP within sourceSubnet to targetSubnet.
func DeriveIPFromOffset(sourceIP, sourceSubnet, targetSubnet string) (string, error) {
	src := net.ParseIP(sourceIP)
	if src == nil {
		return "", fmt.Errorf("invalid source IP: %s", sourceIP)
	}

	_, srcNet, err := net.ParseCIDR(sourceSubnet)
	if err != nil {
		return "", fmt.Errorf("invalid source subnet: %w", err)
	}

	_, tgtNet, err := net.ParseCIDR(targetSubnet)
	if err != nil {
		return "", fmt.Errorf("invalid target subnet: %w", err)
	}
	if !srcNet.Contains(src) {
		return "", fmt.Errorf("source IP %s is outside source subnet %s", sourceIP, sourceSubnet)
	}

	var result string
	src4 := src.To4()
	if src4 != nil {
		// Cross-family: IPv4 source + IPv6 target — compute offset from IPv4, apply to IPv6.
		if tgtNet.IP.To4() == nil {
			srcBase := srcNet.IP.To4()
			if srcBase == nil {
				return "", fmt.Errorf("invalid IPv4 source subnet")
			}
			offset := ipToUint32(src4) - ipToUint32(srcBase)
			resultIP := make(net.IP, 16)
			copy(resultIP, tgtNet.IP.To16())
			tgtLast4 := ipToUint32(tgtNet.IP.To16()[12:16]) + offset
			resultIP[12] = byte(tgtLast4 >> 24) //nolint:gosec // intentional truncation for IP byte extraction
			resultIP[13] = byte(tgtLast4 >> 16) //nolint:gosec // intentional truncation
			resultIP[14] = byte(tgtLast4 >> 8)  //nolint:gosec // intentional truncation
			resultIP[15] = byte(tgtLast4)       //nolint:gosec // intentional truncation
			return validateDerivedIP(resultIP.String(), tgtNet, targetSubnet)
		}
		result, err = deriveIPv4Offset(src4, srcNet, tgtNet)
	} else {
		result, err = deriveIPv6Offset(src.To16(), srcNet, tgtNet)
	}
	if err != nil {
		return "", err
	}
	return validateDerivedIP(result, tgtNet, targetSubnet)
}

func validateDerivedIP(result string, tgtNet *net.IPNet, targetSubnet string) (string, error) {
	resultIP := net.ParseIP(result)
	if resultIP == nil {
		return "", fmt.Errorf("derived target IP %s is invalid", result)
	}
	if !tgtNet.Contains(resultIP) {
		return "", fmt.Errorf("derived target IP %s is outside target subnet %s", result, targetSubnet)
	}
	if err := validateIPv4HostAddress(resultIP, tgtNet, targetSubnet); err != nil {
		return "", err
	}
	return result, nil
}

func validateIPv4HostAddress(ip net.IP, subnet *net.IPNet, targetSubnet string) error {
	ip4 := ip.To4()
	net4 := subnet.IP.To4()
	if ip4 == nil || net4 == nil {
		return nil
	}
	ones, bits := subnet.Mask.Size()
	if bits != 32 || ones >= 31 {
		return nil
	}
	ipValue := ipToUint32(ip4)
	networkValue := ipToUint32(net4)
	broadcastValue := networkValue | ^ipToUint32(net.IP(subnet.Mask))
	switch ipValue {
	case networkValue, broadcastValue:
		return fmt.Errorf("derived target IP %s is not a usable host in target subnet %s", ip.String(), targetSubnet)
	default:
		return nil
	}
}

func deriveIPv4Offset(src net.IP, srcNet, tgtNet *net.IPNet) (string, error) {
	srcBase := srcNet.IP.To4()
	tgtBase := tgtNet.IP.To4()
	if srcBase == nil || tgtBase == nil {
		return "", fmt.Errorf("IPv4/IPv6 mismatch between source and target subnets")
	}

	offset := ipToUint32(src) - ipToUint32(srcBase)
	result := uint32ToIP(ipToUint32(tgtBase) + offset)
	return result.String(), nil
}

func deriveIPv6Offset(src net.IP, srcNet, tgtNet *net.IPNet) (string, error) {
	srcBase := srcNet.IP.To16()
	tgtBase := tgtNet.IP.To16()
	if srcBase == nil || tgtBase == nil {
		return "", fmt.Errorf("invalid IPv6 addresses")
	}

	srcOffset := ipToUint32(src[12:16]) - ipToUint32(srcBase[12:16])
	result := make(net.IP, 16)
	copy(result, tgtBase)
	tgtLast4 := ipToUint32(tgtBase[12:16]) + srcOffset
	result[12] = byte(tgtLast4 >> 24) //nolint:gosec // intentional truncation for IP byte extraction
	result[13] = byte(tgtLast4 >> 16) //nolint:gosec // intentional truncation
	result[14] = byte(tgtLast4 >> 8)  //nolint:gosec // intentional truncation
	result[15] = byte(tgtLast4)       //nolint:gosec // intentional truncation

	return result.String(), nil
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)) //nolint:gosec // intentional truncation for IP byte extraction
}

// DeriveBridgeMAC creates a locally administered unicast MAC from the IPMI MAC.
// It keeps the last 4 bytes of the IPMI MAC behind the 02:54 prefix.
// It returns an empty string when ipmiMAC is not a valid 48-bit MAC.
func DeriveBridgeMAC(ipmiMAC string) string {
	hw, err := net.ParseMAC(ipmiMAC)
	if err != nil || len(hw) != 6 {
		return ""
	}
	return bridgeMACFromBytes(hw[2:])
}

// deriveBridgeMAC picks the provision bridge MAC, which is also the EVPN
// router MAC. An explicit netplan bridge macaddress wins. Otherwise the MAC
// comes from the IPMI MAC, or from the underlay IP when no IPMI MAC is known.
// The underlay IP is also the BGP router ID, so it must be IPv4 in all cases.
func deriveBridgeMAC(cfg *network.Config, underlayIP string) (string, error) {
	underlayMAC, err := deriveBridgeMACFromIP(underlayIP)
	if err != nil {
		return "", err
	}
	if cfg.BridgeMAC != "" {
		hw, err := net.ParseMAC(cfg.BridgeMAC)
		if err != nil {
			return "", fmt.Errorf("invalid bridge MAC %q: %w", cfg.BridgeMAC, err)
		}
		if len(hw) != 6 {
			return "", fmt.Errorf("bridge MAC %q is not a 48-bit MAC", cfg.BridgeMAC)
		}
		return hw.String(), nil
	}
	if mac := DeriveBridgeMAC(cfg.IPMIMAC); mac != "" {
		return mac, nil
	}
	return underlayMAC, nil
}

// deriveBridgeMACFromIP encodes the underlay IPv4 behind the 02:54 prefix,
// like the legacy configure-frr.py ramdisk script. The underlay IP is the
// VTEP address, so the MAC is unique per host. Both the GoBGP and the FRR
// stack use the underlay IP as IPv4 router ID, so other values are an error.
func deriveBridgeMACFromIP(underlayIP string) (string, error) {
	ip4 := net.ParseIP(underlayIP).To4()
	if ip4 == nil {
		return "", fmt.Errorf("underlay IP %q must be an IPv4 address", underlayIP)
	}
	return bridgeMACFromBytes(ip4), nil
}

// bridgeMACFromBytes returns 02:54 followed by 4 host specific bytes.
// 0x02 sets the locally administered bit and clears the multicast bit.
func bridgeMACFromBytes(b []byte) string {
	return net.HardwareAddr{0x02, 0x54, b[0], b[1], b[2], b[3]}.String()
}

// frrConfigData holds template data for FRR configuration rendering.
type frrConfigData struct {
	ASN              uint32
	LocalASN         uint32
	UnderlayIP       string
	NICs             []string
	VRFName          string
	BGPKeepalive     uint32
	BGPHold          uint32
	BGPAuthPassword  string
	BFDProfile       string
	BFDTransmitMS    uint32
	BFDReceiveMS     uint32
	HasIPv6          bool
	IsOnefabric      bool
	DCGWIPs          []string
	OverlayAggregate string
	VPNRT            string
}

const frrConfigTemplate = `frr version 10.3
frr defaults datacenter
!
{{- if .BFDProfile }}
bfd
 profile {{ .BFDProfile }}
  transmit-interval {{ .BFDTransmitMS }}
  receive-interval {{ .BFDReceiveMS }}
 exit
exit
!
{{- end }}
router bgp {{ .ASN }}{{ if .VRFName }} vrf {{ .VRFName }}{{ end }}
 bgp router-id {{ .UnderlayIP }}
 no bgp default ipv4-unicast
 bgp bestpath as-path multipath-relax
{{- if and .BGPKeepalive .BGPHold }}
 timers bgp {{ .BGPKeepalive }} {{ .BGPHold }}
{{- end }}
 neighbor fabric peer-group
 neighbor fabric remote-as external
{{- if .BGPAuthPassword }}
 neighbor fabric password {{ .BGPAuthPassword }}
{{- end }}
{{- if .LocalASN }}
 neighbor fabric local-as {{ .LocalASN }} no-prepend replace-as
{{- end }}
{{- if .BFDProfile }}
 neighbor fabric bfd profile {{ .BFDProfile }}
{{- end }}
{{- range .NICs }}
 neighbor {{ . }} interface peer-group fabric
{{- end }}
 !
 address-family ipv4 unicast
  neighbor fabric activate
  redistribute connected
 exit-address-family
{{- if .HasIPv6 }}
 !
 address-family ipv6 unicast
  neighbor fabric activate
  redistribute connected
 exit-address-family
{{- end }}
 !
 address-family l2vpn evpn
  neighbor fabric activate
  advertise-all-vni
{{- if .IsOnefabric }}
  advertise ipv4 unicast
{{- if .HasIPv6 }}
  advertise ipv6 unicast
{{- end }}
{{- end }}
 exit-address-family
{{- if .IsOnefabric }}
{{- range .DCGWIPs }}
 neighbor {{ . }} remote-as internal
 neighbor {{ . }} update-source {{ $.UnderlayIP }}
{{- if $.BGPAuthPassword }}
 neighbor {{ . }} password {{ $.BGPAuthPassword }}
{{- end }}
{{- if $.BFDProfile }}
 neighbor {{ . }} bfd profile {{ $.BFDProfile }}
{{- end }}
{{- end }}
 !
 address-family l2vpn evpn
{{- range .DCGWIPs }}
  neighbor {{ . }} activate
{{- end }}
{{- if .OverlayAggregate }}
  aggregate-address {{ .OverlayAggregate }}
{{- end }}
{{- if .VPNRT }}
  route-target both {{ .VPNRT }}
{{- end }}
 exit-address-family
{{- end }}
exit
!
line vty
!
`

// RenderConfig generates the FRR configuration file content using a template.
// overlayIP is used to detect IPv6 and conditionally add the IPv6 address-family.
func RenderConfig(cfg *network.Config, underlayIP, overlayIP string, nics []string) (string, error) {
	bgpAuthPassword, err := normalizeBGPAuthPassword(cfg.BGPAuthPassword)
	if err != nil {
		return "", fmt.Errorf("normalize BGP auth password: %w", err)
	}

	data := frrConfigData{
		ASN:             cfg.ASN,
		LocalASN:        cfg.LocalASN,
		UnderlayIP:      underlayIP,
		NICs:            nics,
		VRFName:         cfg.VRFName,
		BGPAuthPassword: bgpAuthPassword,
	}

	if cfg.BGPKeepalive > 0 && cfg.BGPHold > 0 {
		data.BGPKeepalive = cfg.BGPKeepalive
		data.BGPHold = cfg.BGPHold
	}

	if cfg.BFDTransmitMS > 0 && cfg.BFDReceiveMS > 0 {
		data.BFDProfile = "datacenter"
		data.BFDTransmitMS = cfg.BFDTransmitMS
		data.BFDReceiveMS = cfg.BFDReceiveMS
	}

	if isIPv6(overlayIP) {
		data.HasIPv6 = true
	}

	if cfg.DCGWIPs != "" {
		data.IsOnefabric = true
		data.DCGWIPs = strings.Split(cfg.DCGWIPs, ",")
		data.OverlayAggregate = cfg.OverlayAggregate
		data.VPNRT = cfg.VPNRT
	}

	tmpl, err := template.New("frr").Parse(frrConfigTemplate)
	if err != nil {
		return "", fmt.Errorf("parse FRR template: %w", err)
	}

	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("execute FRR template: %w", err)
	}

	return sb.String(), nil
}

func normalizeBGPAuthPassword(password string) (string, error) {
	password = strings.TrimSpace(password)
	if password == "" {
		return "", nil
	}
	if strings.ContainsFunc(password, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return "", fmt.Errorf("bgp auth password must not contain whitespace or control characters")
	}
	return password, nil
}

// isIPv6 returns true if the given IP string is an IPv6 address.
func isIPv6(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() == nil
}
