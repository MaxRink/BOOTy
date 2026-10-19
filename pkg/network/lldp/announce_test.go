//go:build linux

package lldp

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

func fakePort(t *testing.T, p port) {
	t.Helper()
	orig := portFacts
	t.Cleanup(func() { portFacts = orig })
	portFacts = func(string) port { return p }
}

var testSystem = System{
	Version: "v0.3.0", Mode: "check", Vendor: "HPE", Product: "ProLiant DL380 Gen11",
	UUID: "36383150-3530-4d32-3233-303630353538", BoardVersion: "A", BIOSVersion: "U58 v1.48", Serial: "CZ12345678", Tag: "asset-7",
}

// The standard level: a BOOTy system description, the port description
// with driver and PCI address, the management addresses (IPs and MAC), the
// 802.3 MAC/PHY status and the maximum frame size (MTU + 18).
func TestFrameStandard(t *testing.T) {
	on := true
	fakePort(t, port{Driver: "ice", PCI: "0000:3b:00.0", MTU: 9000, Index: 4, Autoneg: &on,
		Addrs: []net.IP{net.ParseIP("fe80::1"), net.ParseIP("192.0.2.10")}})
	mac, _ := net.ParseMAC("3c:fd:fe:a1:b2:c3")
	b, err := Frame("ens1f0np0", mac, &Info{MachineID: "m", SystemName: "host1", PrettyHostname: "Host 1", Announce: AnnounceStandard, System: testSystem})
	if err != nil {
		t.Fatal(err)
	}
	n := parseLLDP(b[14:], "x")
	want := Neighbor{
		ChassisID: "m", ChassisIDSubtype: "local", PortID: "ens1f0np0", PortIDSubtype: "ifname",
		PortDescription: "ens1f0np0 ice 0000:3b:00.0", SystemName: "host1", Description: "BOOTy v0.3.0 mode=check",
		Capabilities: []string{"bridge", "router", "station"}, EnabledCapabilities: []string{"station"},
		MgmtAddresses: []string{"fe80::1", "192.0.2.10", "3c:fd:fe:a1:b2:c3"},
		MACPHY:        &MACPHY{AutonegSupported: true, AutonegEnabled: true}, MaxFrameSize: 9018,
		Interface: "x", TTL: 120,
	}
	if !reflect.DeepEqual(*n, want) {
		t.Fatalf("standard frame\n got  %+v\n want %+v", *n, want)
	}
}

// The full level adds the host identity to the system description and
// the LLDP-MED inventory; with the bm4x profile the identity stays.
func TestFrameFull(t *testing.T) {
	fakePort(t, port{Driver: "mlx5_core", MTU: 1500})
	mac, _ := net.ParseMAC("3c:fd:fe:a1:b2:c3")
	b, err := Frame("ens1f0np0", mac, &Info{MachineID: "m", SystemName: "host1", Profile: ProfileBM4XCableCheck, Announce: AnnounceFull, System: testSystem})
	if err != nil {
		t.Fatal(err)
	}
	n := parseLLDP(b[14:], "x")
	if n.SystemName != "bare-minimal" || n.PortID != "3c:fd:fe:a1:b2:c3" || n.PortDescription != "ens1f0np0 mlx5_core" {
		t.Errorf("identity = %+v", n)
	}
	if n.Description != "BOOTy v0.3.0 mode=check product=HPE_ProLiant_DL380_Gen11 uuid=36383150-3530-4d32-3233-303630353538" {
		t.Errorf("system description %q", n.Description)
	}
	want := Inventory{HardwareRevision: "A", FirmwareRevision: "U58 v1.48", SoftwareRevision: "BOOTy v0.3.0",
		SerialNumber: "CZ12345678", Manufacturer: "HPE", Model: "ProLiant DL380 Gen11", AssetID: "asset-7"}
	if n.Inventory == nil || *n.Inventory != want || n.MACPHY == nil || n.MACPHY.AutonegSupported {
		t.Errorf("inventory %+v, MAC/PHY %+v", n.Inventory, n.MACPHY)
	}
}

// The frame stays within one LLDPDU with every string at its limit, a
// machine ID of 32 digits and an interface name of 15 bytes (IFNAMSIZ);
// at most four IP management addresses are sent.
func TestFrameBudget(t *testing.T) {
	long := strings.Repeat("x", 300)
	id, ifname := strings.Repeat("a", 32), strings.Repeat("e", 15)
	addrs := make([]net.IP, 0, 20)
	for i := range 10 {
		addrs = append(addrs, net.IPv6loopback, net.IPv4(192, 0, 2, byte(i)))
	}
	fakePort(t, port{Driver: long, MTU: 9000, Addrs: addrs})
	sys := testSystem
	sys.Vendor, sys.Product, sys.UUID = long, long, long
	mac, _ := net.ParseMAC("3c:fd:fe:a1:b2:c3")
	sys.BoardVersion, sys.BIOSVersion, sys.Serial, sys.Tag = long, long, long, long
	b, err := Frame(ifname, mac, &Info{MachineID: id, SystemName: long, Announce: AnnounceFull, System: sys})
	if err != nil {
		t.Fatal(err)
	}
	if len(b)-14 > maxLLDPDU {
		t.Fatalf("LLDPDU of %d bytes, more than %d", len(b)-14, maxLLDPDU)
	}
	n := parseLLDP(b[14:], "x")
	if len(n.MgmtAddresses) != maxMgmtIPs+1 || n.Inventory == nil || len(n.Inventory.AssetID) != 32 {
		t.Fatalf("mgmt %v, inventory %+v: want 4 IPs and the MAC, and 32-byte inventory strings", n.MgmtAddresses, n.Inventory)
	}
	t.Logf("largest LLDPDU: %d bytes", len(b)-14)
}

// The LLDP-MED capabilities TLV announces exactly "LLDP-MED capabilities"
// and "inventory" (bits 0 and 5) and endpoint class I.
func TestFrameMEDCapabilities(t *testing.T) {
	fakePort(t, port{Driver: "ice", MTU: 1500})
	mac, _ := net.ParseMAC("3c:fd:fe:a1:b2:c3")
	b, err := Frame("ens1f0np0", mac, &Info{MachineID: "m", SystemName: "host1", Announce: AnnounceFull, System: testSystem})
	if err != nil {
		t.Fatal(err)
	}
	pkt := gopacket.NewPacket(b, layers.LayerTypeEthernet, gopacket.Default)
	info, ok := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if !ok {
		t.Fatal("no LLDP info")
	}
	med, err := info.DecodeMedia()
	if err != nil {
		t.Fatal(err)
	}
	want := layers.LLDPMediaCapabilities{Capabilities: true, Inventory: true, Class: layers.LLDPMediaClassEndpointI}
	if med.MediaCapabilities != want {
		t.Fatalf("MED capabilities %+v, want %+v", med.MediaCapabilities, want)
	}
}
