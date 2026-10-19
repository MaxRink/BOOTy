//go:build linux

package lldp

import (
	"strings"
	"testing"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

func mustSerializeLLDP(t *testing.T) []byte {
	t.Helper()

	lldpLayer := &layers.LinkLayerDiscovery{
		ChassisID: layers.LLDPChassisID{
			Subtype: layers.LLDPChassisIDSubTypeMACAddr,
			ID:      []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		},
		PortID: layers.LLDPPortID{
			Subtype: layers.LLDPPortIDSubtypeIfaceName,
			ID:      []byte("Ethernet1"),
		},
		TTL: 120,
	}

	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{}, lldpLayer); err != nil {
		t.Fatalf("serialize LLDP payload: %v", err)
	}

	return buf.Bytes()
}

func TestParseLLDPValidPayload(t *testing.T) {
	data := mustSerializeLLDP(t)
	n := parseLLDP(data, "eth0")
	if n == nil {
		t.Fatal("parseLLDP() returned nil for valid payload")
		return
	}

	if n.Interface != "eth0" {
		t.Fatalf("Interface = %q, want %q", n.Interface, "eth0")
	}
	if n.ChassisID != "00:11:22:33:44:55" {
		t.Fatalf("ChassisID = %q, want %q", n.ChassisID, "00:11:22:33:44:55")
	}
	if n.PortID != "Ethernet1" {
		t.Fatalf("PortID = %q, want %q", n.PortID, "Ethernet1")
	}
	if n.SystemName != "" {
		t.Fatalf("SystemName = %q, want empty", n.SystemName)
	}
	if n.Description != "" {
		t.Fatalf("Description = %q, want empty", n.Description)
	}
	if n.TTL != 120 {
		t.Fatalf("TTL = %d, want %d", n.TTL, 120)
	}
}

func TestParseLLDPInvalidPayload(t *testing.T) {
	if got := parseLLDP([]byte{0x01, 0x02, 0x03}, "eth0"); got != nil {
		t.Fatalf("parseLLDP() = %#v, want nil for invalid payload", got)
	}
}

func TestHTONS(t *testing.T) {
	if got := htons(0x88cc); got != 0xcc88 {
		t.Fatalf("htons(0x88cc) = 0x%x, want 0xcc88", got)
	}
}

func TestSanitizeLLDP(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "switch-leaf01", "switch-leaf01"},
		{"control chars", "bad\x00\x1f\x7fname", "bad???name"},
		{"long string", string(make([]byte, 300)), strings.Repeat("?", maxLLDPFieldLen)},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeLLDP(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeLLDP(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// rawTLV encodes one LLDP TLV: 7 bits type, 9 bits length, value.
func rawTLV(typ layers.LLDPTLVType, value ...byte) []byte {
	h := uint16(typ)<<9 | uint16(len(value))
	return append([]byte{byte(h >> 8), byte(h)}, value...)
}

// IEEE 802.1AB: the first three TLVs are Chassis ID, Port ID and TTL in
// this order, else the LLDPDU is dropped; the End of LLDPDU TLV is
// optional (802.1AB-2016).
func TestParseLLDPMandatoryTLVs(t *testing.T) {
	chassis := rawTLV(layers.LLDPTLVChassisID, 4, 0, 0x11, 0x22, 0x33, 0x44, 0x55)
	port := rawTLV(layers.LLDPTLVPortID, 5, 'E', 't', 'h', '1')
	ttl := rawTLV(layers.LLDPTLVTTL, 0, 120)
	end := rawTLV(layers.LLDPTLVEnd)
	join := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}
	tests := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"in order with End", join(chassis, port, ttl, end), true},
		{"in order without End", join(chassis, port, ttl), true},
		// One byte of Ethernet padding after an LLDPDU without End TLV.
		{"in order without End, one padding byte", join(chassis, port, ttl, []byte{0}), true},
		{"port before chassis", join(port, chassis, ttl, end), false},
		{"no TTL", join(chassis, port, end), false},
		{"TTL after an optional TLV", join(chassis, port, rawTLV(layers.LLDPTLVSysName, 'x'), ttl, end), false},
		{"short TTL", join(chassis, port, rawTLV(layers.LLDPTLVTTL, 0), end), false},
		{"truncated chassis", chassis[:4], false},
		{"empty payload", nil, false},
		{"one byte", []byte{0x02}, false},
		{"Chassis ID and Port ID only", join(chassis, port), false},
		{"TTL TLV header only", join(chassis, port, []byte{0x06}), false},
		{"second TTL TLV", join(chassis, port, ttl, rawTLV(layers.LLDPTLVTTL, 0, 0), end), false},
		{"second Chassis ID TLV", join(chassis, port, ttl, chassis, end), false},
		{"second Port ID TLV", join(chassis, port, ttl, port, end), false},
		{"mandatory TLVs after End are ignored", join(chassis, port, ttl, end, chassis, port, ttl), true},
		{"optional TLV after the mandatory ones", join(chassis, port, ttl, rawTLV(layers.LLDPTLVSysName, 'x'), end), true},
	}
	for _, tt := range tests {
		n := parseLLDP(tt.data, "eth0")
		if (n != nil) != tt.ok {
			t.Errorf("%s: neighbor %+v, want accepted=%v", tt.name, n, tt.ok)
			continue
		}
		if n != nil && (n.PortID != "Eth1" || n.TTL != 120) {
			t.Errorf("%s: neighbor %+v", tt.name, n)
		}
	}
}
