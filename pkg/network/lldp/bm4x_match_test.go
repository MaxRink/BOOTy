//go:build linux

package lldp

import (
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

// This file ports the compute matching of the bm4x connection check to Go,
// so a test shows which BOOTy frames bm4x matches. Source: bm4x-api
// (gitlab t-dci-cloud/infracloud-api/bm4x-api, commit 1d3600863,
// 2026-01-16), apps/bm4x-app/src/main/java/de/telekom/dci/infracloud/bm4x/
// admin/service/connectioncheck/:
//
//   - Connection.java:13-15 MAC_ADDRESS_PATTERN, NODE_NAME "bare-minimal",
//     UNKNOWN_DEVICE; :24-29 isToMacAddress.
//   - NetworkConnectionProvider.java:97-102: per switch port, deviceB =
//     NSO lldp-remote-system-name, interfaceB = NSO lldp-remote-port-id.
//     network-app LldpNeighborInformationMapper passes both strings from
//     NSO unchanged.
//   - NetworkConnectionProvider.java:168-193: a connection to a MAC is
//     resolved through macToInterface (lower-case MAC) to the NetBox
//     interface and the device hostname, and added in both directions.
//   - ConnectionCheckUtil.java:86-92: macToInterface is keyed by the
//     lower-case NetBox interface mac_address.

// bm4xMACPattern is Connection.MAC_ADDRESS_PATTERN.
var bm4xMACPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}([0-9A-Fa-f]{2})$`)

// bm4xConnection is Connection: device A is the switch, B its neighbor.
type bm4xConnection struct {
	deviceA, interfaceA string
	deviceB, interfaceB string
	deviceBNull         bool // NSO sent no remote system name (Java null)
}

// isToMacAddress is Connection.isToMacAddress.
func (c bm4xConnection) isToMacAddress() bool {
	return (c.deviceBNull || c.deviceB == "bare-minimal" || c.deviceB == "UNKNOWN_DEVICE") &&
		bm4xMACPattern.MatchString(c.interfaceB)
}

type bm4xInterface struct{ hostname, name string }

// resolve is the isToMacAddress branch of processActualConnections: it
// returns the connection and its reverse, with the NetBox names of the
// compute, or nothing when the MAC is not a compute MAC.
func (c bm4xConnection) resolve(macToInterface []struct {
	mac string
	bm4xInterface
}) []bm4xConnection {
	if !c.isToMacAddress() {
		return []bm4xConnection{c}
	}
	n := c
	n.deviceB, n.deviceBNull = "UNKNOWN_DEVICE", false
	for _, e := range macToInterface {
		if strings.EqualFold(e.mac, c.interfaceB) {
			n.deviceB, n.interfaceB = e.hostname, e.name
		}
	}
	return []bm4xConnection{n, {deviceA: n.deviceB, interfaceA: n.interfaceB, deviceB: n.deviceA, interfaceB: n.interfaceA}}
}

// switchView is what a switch reports to NSO for a received frame: the
// system name and the port ID, a MAC port ID (subtype 3) in the colon form
// that lldpd and Junos show. NX-OS shows aabb.ccdd.eeff instead; that form
// does not match MAC_ADDRESS_PATTERN unless NSO converts it.
func switchView(t *testing.T, frame []byte) (sysName, portID string) {
	t.Helper()
	return switchViewAs(t, frame, false)
}

// switchViewAs is switchView of a switch that shows a MAC port ID in the
// colon form (lldpd, Junos) or, with dotted, in the NX-OS form. Both show
// a locally assigned or interface name port ID unchanged.
func switchViewAs(t *testing.T, frame []byte, dotted bool) (sysName, portID string) {
	t.Helper()
	pkt := gopacket.NewPacket(frame, layers.LayerTypeEthernet, gopacket.Default)
	l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	info, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if l == nil || info == nil {
		t.Fatalf("frame does not decode: %v", pkt.ErrorLayer())
	}
	portID = string(l.PortID.ID)
	if l.PortID.Subtype == layers.LLDPPortIDSubtypeMACAddr {
		portID = net.HardwareAddr(l.PortID.ID).String()
		if dotted {
			portID = dottedMAC(l.PortID.ID)
		}
	}
	return info.SysName, portID
}

// With the bm4x-cablecheck profile bm4x matches the BOOTy port to the
// compute interface in NetBox, in both directions. With the networkd
// default it does not: the port ID is the interface name and the system
// name the hostname.
func TestBM4XMatchesCableCheckFrames(t *testing.T) {
	mac, _ := net.ParseMAC("3C:FD:FE:A1:B2:C3")
	netbox := []struct {
		mac string
		bm4xInterface
	}{{"3C:FD:FE:A1:B2:C3", bm4xInterface{"site1com5k01", "ens1f0np0"}}}

	frame, err := Frame("ens1f0np0", mac, &Info{MachineID: "0123456789abcdef0123456789abcdef", SystemName: "booty", Profile: ProfileBM4XCableCheck})
	if err != nil {
		t.Fatal(err)
	}
	name, port := switchView(t, frame)
	c := bm4xConnection{deviceA: "site1lsw5k10", interfaceA: "et-0/0/3", deviceB: name, interfaceB: port}
	if !c.isToMacAddress() {
		t.Fatalf("bm4x does not match the frame: system name %q, port ID %q", name, port)
	}
	got := c.resolve(netbox)
	want := []bm4xConnection{
		{deviceA: "site1lsw5k10", interfaceA: "et-0/0/3", deviceB: "site1com5k01", interfaceB: "ens1f0np0"},
		{deviceA: "site1com5k01", interfaceA: "ens1f0np0", deviceB: "site1lsw5k10", interfaceB: "et-0/0/3"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}

	frame, err = Frame("ens1f0np0", mac, &Info{MachineID: "0123456789abcdef0123456789abcdef", SystemName: "booty"})
	if err != nil {
		t.Fatal(err)
	}
	name, port = switchView(t, frame)
	if c := (bm4xConnection{deviceB: name, interfaceB: port}); c.isToMacAddress() {
		t.Fatalf("bm4x matches the networkd frame (%q, %q)", name, port)
	}
}

// The rules of isToMacAddress, as in Connection.java.
func TestBM4XIsToMacAddress(t *testing.T) {
	for _, tt := range []struct {
		c    bm4xConnection
		want bool
	}{
		{bm4xConnection{deviceB: "bare-minimal", interfaceB: "3c:fd:fe:a1:b2:c3"}, true},
		{bm4xConnection{deviceB: "UNKNOWN_DEVICE", interfaceB: "3C:FD:FE:A1:B2:C3"}, true},
		{bm4xConnection{deviceBNull: true, interfaceB: "3c:fd:fe:a1:b2:c3"}, true},
		{bm4xConnection{deviceB: "booty", interfaceB: "3c:fd:fe:a1:b2:c3"}, false},
		{bm4xConnection{deviceB: "bare-minimal", interfaceB: "ens1f0np0"}, false},
		{bm4xConnection{deviceB: "bare-minimal", interfaceB: "3cfd.fea1.b2c3"}, false}, // NX-OS form
	} {
		if got := tt.c.isToMacAddress(); got != tt.want {
			t.Errorf("%+v: isToMacAddress = %v, want %v", tt.c, got, tt.want)
		}
	}
}

// Which port ID format bm4x matches, by how the switch shows it to NSO
// (NSO and network-app pass the string through unchanged): the MAC port
// ID only where the switch shows the colon form, the colon string
// everywhere, the dotted string and the interface name nowhere.
func TestBM4XMatchesPortIDFormats(t *testing.T) {
	mac, _ := net.ParseMAC("3C:FD:FE:A1:B2:C3")
	for _, tt := range []struct {
		format            string
		colonSwitch, nxos bool
	}{
		{"", true, false},
		{PortIDMAC, true, false},
		{PortIDMACStringColon, true, true},
		{PortIDMACStringDotted, false, false},
		{PortIDIfname, false, false},
	} {
		frame, err := Frame("ens1f0np0", mac, &Info{SystemName: "booty", Profile: ProfileBM4XCableCheck, PortIDFormat: tt.format})
		if err != nil {
			t.Fatal(err)
		}
		for _, sw := range []struct {
			name   string
			dotted bool
			want   bool
		}{{"lldpd/Junos", false, tt.colonSwitch}, {"NX-OS", true, tt.nxos}} {
			name, port := switchViewAs(t, frame, sw.dotted)
			c := bm4xConnection{deviceA: "leaf", interfaceA: "Ethernet1/1", deviceB: name, interfaceB: port}
			if got := c.isToMacAddress(); got != sw.want {
				t.Errorf("format %q on %s (port ID %q): bm4x match = %v, want %v", tt.format, sw.name, port, got, sw.want)
			}
		}
	}
}
