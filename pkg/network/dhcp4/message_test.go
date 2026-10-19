package dhcp4

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func reply4(typ layers.DHCPMsgType, yiaddr net.IP, opts ...layers.DHCPOption) []byte {
	d := &layers.DHCPv4{
		Operation: layers.DHCPOpReply, HardwareType: layers.LinkTypeEthernet, HardwareLen: 6, Xid: 1,
		ClientHWAddr: testMAC, YourClientIP: yiaddr, ClientIP: net.IPv4zero, NextServerIP: net.IPv4zero, RelayAgentIP: net.IPv4zero,
		Options: append([]layers.DHCPOption{layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(typ)})}, opts...),
	}
	buf := gopacket.NewSerializeBuffer()
	_ = d.SerializeTo(buf, gopacket.SerializeOptions{})
	return buf.Bytes()
}

func TestDecodeReplyChecks(t *testing.T) {
	sid := layers.NewDHCPOption(layers.DHCPOptServerID, testServer)
	lt := layers.NewDHCPOption(layers.DHCPOptLeaseTime, []byte{0, 0, 0, 120})
	good := reply4(layers.DHCPMsgTypeAck, testAddr, sid, lt)
	badHlen := append([]byte(nil), good...)
	badHlen[2] = 255
	badType := append([]byte(nil), good...)
	badType[1] = 6
	for _, tt := range []struct {
		name string
		b    []byte
		ok   bool
	}{
		{"ACK", good, true},
		{"hlen 255", badHlen, false},
		{"htype 6", badType, false},
		{"short", good[:239], false},
		{"OFFER without yiaddr", reply4(layers.DHCPMsgTypeOffer, net.IPv4zero, sid, lt), false},
		{"ACK without server ID", reply4(layers.DHCPMsgTypeAck, testAddr, lt), false},
		{"ACK with server ID 0.0.0.0", reply4(layers.DHCPMsgTypeAck, testAddr, lt, layers.NewDHCPOption(layers.DHCPOptServerID, net.IPv4zero.To4())), false},
		{"OFFER with server ID 0.0.0.0", reply4(layers.DHCPMsgTypeOffer, testAddr, lt, layers.NewDHCPOption(layers.DHCPOptServerID, net.IPv4zero.To4())), false},
		{"ACK without lease time", reply4(layers.DHCPMsgTypeAck, testAddr, sid), false},
		{"NAK without server ID", reply4(layers.DHCPMsgTypeNak, net.IPv4zero), false},
		{"NAK", reply4(layers.DHCPMsgTypeNak, net.IPv4zero, sid), true},
		{"ACK with lease time 0", reply4(layers.DHCPMsgTypeAck, testAddr, sid, layers.NewDHCPOption(layers.DHCPOptLeaseTime, []byte{0, 0, 0, 0})), false},
		{"ACK with option overload", reply4(layers.DHCPMsgTypeAck, testAddr, sid, lt, layers.NewDHCPOption(optOverload, []byte{3})), false},
	} {
		if _, ok := decodeReply(tt.b, time.Now()); ok != tt.ok {
			t.Errorf("%s: ok = %v, want %v", tt.name, ok, tt.ok)
		}
	}
}

func FuzzDecodeReply(f *testing.F) {
	f.Add(reply4(layers.DHCPMsgTypeAck, testAddr, layers.NewDHCPOption(layers.DHCPOptServerID, testServer)))
	f.Add(make([]byte, 240))
	crash := make([]byte, 300) // hlen 255: gopacket slices chaddr past the end
	crash[0], crash[1], crash[2] = 2, 1, 255
	f.Add(crash)
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = decodeReply(b, time.Now())
	})
}

func TestLeaseTimes(t *testing.T) {
	for _, tt := range []struct{ lease, t1, t2, wantT1, wantT2 uint32 }{
		{120, 0, 0, 60, 105},
		{10, 0, 0, 5, 8},
		{0xffffffff, 0, 0, 0x7fffffff, 3758096383},
		{120, 110, 100, 60, 100},
		{120, 0, 200, 60, 105},
		{120, 50, 40, 60, 105},
		{120, 30, 90, 30, 90},
	} {
		now := time.Unix(0, 0)
		var l Lease
		leaseTimes{lease: tt.lease, t1: tt.t1, t2: tt.t2, has: true}.set(&l, now)
		if got1, got2 := uint32(l.Renew.Sub(now)/time.Second), uint32(l.Rebind.Sub(now)/time.Second); got1 != tt.wantT1 || got2 != tt.wantT2 {
			t.Errorf("lease %d T1 %d T2 %d: got T1 %d T2 %d, want %d %d", tt.lease, tt.t1, tt.t2, got1, got2, tt.wantT1, tt.wantT2)
		}
	}
}

// frame4 builds Ethernet/IPv4/UDP to port 68 with valid checksums.
func frame4(payload []byte) []byte {
	eth := &layers.Ethernet{SrcMAC: testMAC, DstMAC: layers.EthernetBroadcast, EthernetType: layers.EthernetTypeIPv4}
	ip := &layers.IPv4{Version: 4, TTL: 64, Protocol: layers.IPProtocolUDP, SrcIP: testServer, DstIP: testAddr}
	udp := &layers.UDP{SrcPort: 67, DstPort: 68}
	_ = udp.SetNetworkLayerForChecksum(ip)
	buf := gopacket.NewSerializeBuffer()
	_ = gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, eth, ip, udp, gopacket.Payload(payload))
	return buf.Bytes()
}

func TestUDPPayloadChecksums(t *testing.T) {
	good := frame4([]byte("hello"))
	if p, ok := udpPayload(good, true); !ok || string(p) != "hello" {
		t.Fatalf("valid frame: %q %v", p, ok)
	}
	badUDP := append([]byte(nil), good...)
	badUDP[14+20+8] ^= 0xff // the first payload byte (the frame is padded to 60 bytes)
	if _, ok := udpPayload(badUDP, true); ok {
		t.Error("a bad UDP checksum passed")
	}
	if _, ok := udpPayload(badUDP, false); !ok {
		t.Error("a frame without a filled-in checksum (offload) was dropped")
	}
	noSum := append([]byte(nil), badUDP...)
	binary.BigEndian.PutUint16(noSum[14+20+6:], 0)
	if _, ok := udpPayload(noSum, true); !ok {
		t.Error("UDP checksum 0 (none) was dropped")
	}
	badIP := append([]byte(nil), good...)
	badIP[14+10] ^= 0xff
	if _, ok := udpPayload(badIP, true); ok {
		t.Error("a bad IP header checksum passed")
	}
	if _, ok := udpPayload(badIP, false); ok {
		t.Error("a bad IP header checksum passed without the UDP check (offload marks only the UDP checksum)")
	}
	frag := append([]byte(nil), good...)
	frag[14+6] |= 0x20 // more fragments
	if _, ok := udpPayload(frag, false); ok {
		t.Error("a fragment passed")
	}
	other := frame4([]byte("x"))
	binary.BigEndian.PutUint16(other[14+20+2:], 69)
	if _, ok := udpPayload(other, false); ok {
		t.Error("a datagram to port 69 passed")
	}
}

func FuzzUDPPayload(f *testing.F) {
	f.Add(frame4([]byte("hello")))
	f.Fuzz(func(_ *testing.T, b []byte) { _, _ = udpPayload(b, true) })
}

// Option 26 below 576, also 0, becomes 576 like networkd; without the
// option MTU stays 0.
func TestLeaseMTUMinimum(t *testing.T) {
	sid := layers.NewDHCPOption(layers.DHCPOptServerID, testServer)
	lt := layers.NewDHCPOption(layers.DHCPOptLeaseTime, []byte{0, 0, 0, 120})
	for _, tt := range []struct {
		opts []layers.DHCPOption
		want uint16
	}{
		{nil, 0},
		{[]layers.DHCPOption{layers.NewDHCPOption(layers.DHCPOptInterfaceMTU, []byte{0, 0})}, 576},
		{[]layers.DHCPOption{layers.NewDHCPOption(layers.DHCPOptInterfaceMTU, []byte{0x23, 0x28})}, 9000},
	} {
		r, ok := decodeReply(reply4(layers.DHCPMsgTypeAck, testAddr, append([]layers.DHCPOption{sid, lt}, tt.opts...)...), time.Now())
		if !ok || r.lease.MTU != tt.want {
			t.Errorf("MTU = %d (ok %v), want %d", r.lease.MTU, ok, tt.want)
		}
	}
}
