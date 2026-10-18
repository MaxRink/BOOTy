//go:build linux

package lldp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// The frame carries what systemd-networkd sends with EmitLLDP=yes
// (sd-lldp-tx.c): nearest-bridge destination, the machine ID as locally
// assigned chassis ID, the interface name as port ID, TTL 120, the
// hostname, the pretty hostname and the capabilities.
func TestFrameLikeNetworkd(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	b, err := Frame("ens1f0np0", mac, &Info{MachineID: "0123456789abcdef0123456789abcdef", SystemName: "host1", PrettyHostname: "Host 1"})
	if err != nil {
		t.Fatal(err)
	}
	pkt := gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.Default)
	eth, _ := pkt.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
	l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	info, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if eth == nil || l == nil || info == nil {
		t.Fatalf("frame does not decode: %v", pkt.ErrorLayer())
	}
	if eth.DstMAC.String() != "01:80:c2:00:00:0e" || eth.SrcMAC.String() != mac.String() {
		t.Errorf("ethernet %s -> %s", eth.SrcMAC, eth.DstMAC)
	}
	if l.ChassisID.Subtype != layers.LLDPChassisIDSubTypeLocal || string(l.ChassisID.ID) != "0123456789abcdef0123456789abcdef" {
		t.Errorf("chassis = %v %q", l.ChassisID.Subtype, l.ChassisID.ID)
	}
	if l.PortID.Subtype != layers.LLDPPortIDSubtypeIfaceName || string(l.PortID.ID) != "ens1f0np0" || l.TTL != 120 {
		t.Errorf("port = %v %q, TTL %d", l.PortID.Subtype, l.PortID.ID, l.TTL)
	}
	if info.SysName != "host1" || info.SysDescription != "Host 1" {
		t.Errorf("system name %q description %q", info.SysName, info.SysDescription)
	}
	var caps []byte
	for _, v := range l.Values {
		if v.Type == layers.LLDPTLVSysCapabilities {
			caps = v.Value
		}
	}
	if len(caps) != 4 || binary.BigEndian.Uint16(caps) != 0x94 || binary.BigEndian.Uint16(caps[2:]) != 0x80 {
		t.Errorf("capabilities = %x, want supported 0094 enabled 0080", caps)
	}

	// Without a machine ID the NIC MAC is the chassis ID; without a pretty
	// hostname there is no system description.
	b, err = Frame("eth0", mac, &Info{SystemName: "host1"})
	if err != nil {
		t.Fatal(err)
	}
	pkt = gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.Default)
	l, _ = pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	info, _ = pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if l.ChassisID.Subtype != layers.LLDPChassisIDSubTypeMACAddr || net.HardwareAddr(l.ChassisID.ID).String() != mac.String() || info.SysDescription != "" {
		t.Errorf("fallback chassis %v %x, description %q", l.ChassisID.Subtype, l.ChassisID.ID, info.SysDescription)
	}
	// networkd sends no system description TLV at all without a pretty
	// hostname, not an empty one.
	for _, v := range l.Values {
		if v.Type == layers.LLDPTLVSysDescription {
			t.Errorf("fallback frame has a system description TLV %q", v.Value)
		}
	}
}

// BOOTy creates a machine ID for the boot like systemd (32 hex digits, a
// version 4 UUID), keeps it under /run (never /etc) and uses it again.
// When it cannot write it, there is no machine ID and the frames fall
// back to the MAC.
func TestMachineIDPerBoot(t *testing.T) {
	if !strings.HasPrefix(machineIDPath, "/run/") {
		t.Fatalf("machine ID path %s, want one under /run", machineIDPath)
	}
	orig := machineIDPath
	t.Cleanup(func() { machineIDPath = orig })
	machineIDPath = filepath.Join(t.TempDir(), "booty", "machine-id")
	id := machineID()
	if !regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`).MatchString(id) {
		t.Fatalf("machine ID %q, want 32 hex digits of a version 4 UUID", id)
	}
	if again := machineID(); again != id {
		t.Fatalf("machine ID changed: %q then %q", id, again)
	}
	if b, err := os.ReadFile(machineIDPath); err != nil || string(b) != id+"\n" {
		t.Fatalf("machine-id file = %q (%v)", b, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	machineIDPath = filepath.Join(file, "machine-id") // its directory is a file
	if got := machineID(); got != "" {
		t.Fatalf("machine ID %q although its directory can not be created, want none", got)
	}
	machineIDPath = t.TempDir() // a directory: the write fails, also as root
	if got := machineID(); got != "" {
		t.Fatalf("machine ID %q although it can not be written, want none", got)
	}
}

// The networkd profile (the default) sends the bytes that BOOTy sent
// before the identity profiles existed (#698, e136315e).
func TestFrameNetworkdGolden(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	for _, tt := range []struct {
		ifname string
		info   Info
		want   string
	}{
		{"ens1f0np0", Info{MachineID: "0123456789abcdef0123456789abcdef", SystemName: "host1", PrettyHostname: "Host 1"},
			"0180c200000e02000000000188cc0221073031323334353637383961626364656630313233343536373839616263646566040a05656e733166306e7030060200780a05686f7374310c06486f737420310e04009400800000"},
		{"eth0", Info{SystemName: "host1"},
			"0180c200000e02000000000188cc02070402000000000104050565746830060200780a05686f7374310e040094008000000000000000000000000000"},
	} {
		for _, profile := range []string{"", ProfileNetworkd} {
			info := tt.info
			info.Profile = profile
			b, err := Frame(tt.ifname, mac, &info)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(b); got != tt.want {
				t.Errorf("%s profile %q:\n got  %s\n want %s", tt.ifname, profile, got, tt.want)
			}
		}
	}
}

// The bm4x-cablecheck profile sends the NIC MAC as port ID (subtype 3),
// the interface name as port description and the system name
// "bare-minimal"; chassis, TTL and capabilities stay as networkd sends them.
func TestFrameBM4XCableCheck(t *testing.T) {
	mac, _ := net.ParseMAC("3c:fd:fe:a1:b2:c3")
	b, err := Frame("ens1f0np0", mac, &Info{MachineID: "0123456789abcdef0123456789abcdef", SystemName: "host1", Profile: ProfileBM4XCableCheck})
	if err != nil {
		t.Fatal(err)
	}
	pkt := gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.Default)
	l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	info, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if l == nil || info == nil {
		t.Fatalf("frame does not decode: %v", pkt.ErrorLayer())
	}
	if l.PortID.Subtype != layers.LLDPPortIDSubtypeMACAddr || !bytes.Equal(l.PortID.ID, mac) {
		t.Errorf("port ID = %v %x, want MAC %s", l.PortID.Subtype, l.PortID.ID, mac)
	}
	if info.SysName != "bare-minimal" || info.PortDescription != "ens1f0np0" {
		t.Errorf("system name %q, port description %q", info.SysName, info.PortDescription)
	}
	if l.ChassisID.Subtype != layers.LLDPChassisIDSubTypeLocal || l.TTL != 120 {
		t.Errorf("chassis %v, TTL %d", l.ChassisID.Subtype, l.TTL)
	}
}

// Only the bm4x profile and LLDP_EMIT_ALL skip NICs without carrier; the
// networkd emitter tries every NIC as before.
func TestCarrierOnly(t *testing.T) {
	for _, tt := range []struct {
		info Info
		want bool
	}{
		{Info{}, false},
		{Info{Profile: ProfileNetworkd}, false},
		{Info{Profile: ProfileBM4XCableCheck}, true},
		{Info{CarrierOnly: true}, true},
	} {
		if got := tt.info.carrierOnly(); got != tt.want {
			t.Errorf("%+v: carrierOnly = %v, want %v", tt.info, got, tt.want)
		}
	}
}

// network.lldp.portIDFormat selects the port ID of the bm4x-cablecheck
// profile; "" is "mac" byte for byte.
func TestFrameBM4XPortIDFormats(t *testing.T) {
	mac, _ := net.ParseMAC("3c:fd:fe:a1:b2:c3")
	base := Info{MachineID: "0123456789abcdef0123456789abcdef", SystemName: "host1", Profile: ProfileBM4XCableCheck}
	def, err := Frame("ens1f0np0", mac, &base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		format  string
		subtype layers.LLDPPortIDSubType
		id      []byte
	}{
		{PortIDMAC, layers.LLDPPortIDSubtypeMACAddr, mac},
		{PortIDMACStringColon, layers.LLDPPortIDSubtypeLocal, []byte("3c:fd:fe:a1:b2:c3")},
		{PortIDMACStringDotted, layers.LLDPPortIDSubtypeLocal, []byte("3cfd.fea1.b2c3")},
		{PortIDIfname, layers.LLDPPortIDSubtypeIfaceName, []byte("ens1f0np0")},
	} {
		info := base
		info.PortIDFormat = tt.format
		b, err := Frame("ens1f0np0", mac, &info)
		if err != nil {
			t.Fatal(err)
		}
		pkt := gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.Default)
		l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
		d, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
		if l == nil || d == nil {
			t.Fatalf("%s: frame does not decode: %v", tt.format, pkt.ErrorLayer())
		}
		if l.PortID.Subtype != tt.subtype || !bytes.Equal(l.PortID.ID, tt.id) {
			t.Errorf("%s: port ID = %v %q, want %v %q", tt.format, l.PortID.Subtype, l.PortID.ID, tt.subtype, tt.id)
		}
		if d.SysName != BM4XSystemName || d.PortDescription != "ens1f0np0" {
			t.Errorf("%s: system name %q, port description %q", tt.format, d.SysName, d.PortDescription)
		}
		if tt.format == PortIDMAC && !bytes.Equal(b, def) {
			t.Errorf("format mac differs from the default")
		}
	}
}

func TestDottedMAC(t *testing.T) {
	mac, _ := net.ParseMAC("00:1B:54:C2:00:0A")
	if got := dottedMAC(mac); got != "001b.54c2.000a" {
		t.Fatalf("dottedMAC = %q", got)
	}
}
