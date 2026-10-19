package dhcp4

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

func reply4(typ dhcpv4.MessageType, yiaddr net.IP, opts ...dhcpv4.Option) []byte {
	d := &dhcpv4.DHCPv4{
		OpCode: dhcpv4.OpcodeBootReply, HWType: iana.HWTypeEthernet, TransactionID: dhcpv4.TransactionID{0, 0, 0, 1},
		ClientHWAddr: testMAC, YourIPAddr: yiaddr,
		Options: dhcpv4.OptionsFromList(append([]dhcpv4.Option{dhcpv4.OptMessageType(typ)}, opts...)...),
	}
	return d.ToBytes()
}

func TestDecodeReplyChecks(t *testing.T) {
	sid := dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, testServer)
	lt := dhcpv4.OptGeneric(dhcpv4.OptionIPAddressLeaseTime, []byte{0, 0, 0, 120})
	good := reply4(dhcpv4.MessageTypeAck, testAddr, sid, lt)
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
		{"OFFER without yiaddr", reply4(dhcpv4.MessageTypeOffer, net.IPv4zero, sid, lt), false},
		{"ACK without server ID", reply4(dhcpv4.MessageTypeAck, testAddr, lt), false},
		{"ACK with server ID 0.0.0.0", reply4(dhcpv4.MessageTypeAck, testAddr, lt, dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, net.IPv4zero.To4())), false},
		{"OFFER with server ID 0.0.0.0", reply4(dhcpv4.MessageTypeOffer, testAddr, lt, dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, net.IPv4zero.To4())), false},
		{"ACK without lease time", reply4(dhcpv4.MessageTypeAck, testAddr, sid), false},
		{"NAK without server ID", reply4(dhcpv4.MessageTypeNak, net.IPv4zero), false},
		{"NAK", reply4(dhcpv4.MessageTypeNak, net.IPv4zero, sid), true},
		{"ACK with lease time 0", reply4(dhcpv4.MessageTypeAck, testAddr, sid, dhcpv4.OptGeneric(dhcpv4.OptionIPAddressLeaseTime, []byte{0, 0, 0, 0})), false},
		{"ACK with option overload", reply4(dhcpv4.MessageTypeAck, testAddr, sid, lt, dhcpv4.OptGeneric(dhcpv4.OptionOptionOverload, []byte{3})), false},
	} {
		if _, ok := decodeReply(tt.b, time.Now()); ok != tt.ok {
			t.Errorf("%s: ok = %v, want %v", tt.name, ok, tt.ok)
		}
	}
}

func FuzzDecodeReply(f *testing.F) {
	f.Add(reply4(dhcpv4.MessageTypeAck, testAddr, dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, testServer)))
	f.Add(make([]byte, 240))
	crash := make([]byte, 300) // hlen 255
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
		leaseTimes{lease: tt.lease, t1: tt.t1, t2: tt.t2}.set(&l, now)
		if got1, got2 := uint32(l.Renew.Sub(now)/time.Second), uint32(l.Rebind.Sub(now)/time.Second); got1 != tt.wantT1 || got2 != tt.wantT2 {
			t.Errorf("lease %d T1 %d T2 %d: got T1 %d T2 %d, want %d %d", tt.lease, tt.t1, tt.t2, got1, got2, tt.wantT1, tt.wantT2)
		}
	}
}

// frame4 builds Ethernet/IPv4/UDP to port 68 with valid checksums,
// padded to 60 bytes like a short Ethernet frame.
func frame4(payload []byte) []byte {
	b := udpFrame(testMAC, testServer, testAddr, 67, 68, 0, payload)
	return append(b, make([]byte, max(60-len(b), 0))...)
}

// The broadcast frames are those gopacket built before (testdata/frames.txt).
func TestBroadcastFrames(t *testing.T) {
	c := newFakeNet().client()
	frames := map[string][]byte{
		"discover":          udpFrame(testMAC, net.IPv4zero, net.IPv4bcast, 68, 67, 0xc0, c.discover(0x01020304).encode()),
		"request/selecting": udpFrame(testMAC, net.IPv4zero, net.IPv4bcast, 68, 67, 0xc0, c.request(0x01020304, nil, testServer, testAddr).encode()),
		"request/rebinding": udpFrame(testMAC, testAddr, net.IPv4bcast, 68, 67, 0xc0, c.request(0xfffefdfc, testAddr, nil, nil).encode()),
		"short":             udpFrame(testMAC, net.IPv4zero, net.IPv4bcast, 68, 67, 0xc0, (&message{typ: 1, xid: 7, mac: testMAC}).encode()),
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "frames.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(golden)), "\n") {
		name, want, _ := strings.Cut(line, " ")
		if got := hex.EncodeToString(frames[name]); got != want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
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
	sid := dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, testServer)
	lt := dhcpv4.OptGeneric(dhcpv4.OptionIPAddressLeaseTime, []byte{0, 0, 0, 120})
	for _, tt := range []struct {
		opts []dhcpv4.Option
		want uint16
	}{
		{nil, 0},
		{[]dhcpv4.Option{dhcpv4.OptGeneric(dhcpv4.OptionInterfaceMTU, []byte{0, 0})}, 576},
		{[]dhcpv4.Option{dhcpv4.OptGeneric(dhcpv4.OptionInterfaceMTU, []byte{0x23, 0x28})}, 9000},
	} {
		r, ok := decodeReply(reply4(dhcpv4.MessageTypeAck, testAddr, append([]dhcpv4.Option{sid, lt}, tt.opts...)...), time.Now())
		if !ok || r.lease.MTU != tt.want {
			t.Errorf("MTU = %d (ok %v), want %d", r.lease.MTU, ok, tt.want)
		}
	}
}

// Options are read like networkd (parse_options): without End, a later
// valid copy of an option replaces an earlier one, a copy with the wrong
// length is ignored, the routes of valid copies of 33 and 121 add up.
func TestDecodeReplyRepeatedOptions(t *testing.T) {
	// raw is a reply whose options are opts, without End.
	raw := func(opts ...byte) []byte {
		b := reply4(dhcpv4.MessageTypeAck, testAddr)
		return append(b[:bootpLen:bootpLen], opts...)
	}
	// ACK, server ID 192.168.10.1, lease time 120 s.
	ack := []byte{53, 1, 5, 54, 4, 192, 168, 10, 1, 51, 4, 0, 0, 0, 120}
	for _, tt := range []struct {
		name  string
		b     []byte
		ok    bool
		check func(reply) bool
	}{
		{"54 twice", raw(append(append(ack, 54, 4, 192, 168, 10, 2), 0xff)...), true,
			func(r reply) bool { return r.lease.ServerID.Equal(net.IPv4(192, 168, 10, 2)) }},
		{"51 twice", raw(append(append(ack, 51, 4, 0, 0, 1, 44), 0xff)...), true,
			func(r reply) bool { return r.lease.Expire.Sub(r.lease.Bound) == 300*time.Second }},
		{"3 twice", raw(append(append(ack, 3, 4, 10, 0, 0, 1, 3, 4, 10, 0, 0, 254), 0xff)...), true,
			func(r reply) bool {
				return len(r.lease.Router) == 1 && r.lease.Router[0].Equal(net.IPv4(10, 0, 0, 254))
			}},
		{"53 twice, OFFER then ACK", raw(append(append([]byte{53, 1, 2}, ack...), 0xff)...), true,
			func(r reply) bool { return r.typ == dhcpv4.MessageTypeAck }},
		{"121 twice", raw(append(append(ack, 121, 5, 0, 10, 0, 0, 1, 121, 8, 24, 10, 1, 0, 10, 0, 0, 2), 0xff)...), true,
			func(r reply) bool { return len(r.lease.Options.Get(dhcpv4.OptionClasslessStaticRoute)) == 13 }},
		{"54, then 54 of 3 bytes", raw(append(append(ack, 54, 3, 192, 168, 10), 0xff)...), true,
			func(r reply) bool { return r.lease.ServerID.Equal(testServer) }},
		{"3, then 3 of 5 bytes", raw(append(append(ack, 3, 4, 10, 0, 0, 1, 3, 5, 10, 0, 0, 2, 9), 0xff)...), true,
			func(r reply) bool { return len(r.lease.Router) == 1 && r.lease.Router[0].Equal(net.IPv4(10, 0, 0, 1)) }},
		{"51 of 2 bytes, then 51", raw(append([]byte{53, 1, 5, 54, 4, 192, 168, 10, 1, 51, 2, 0, 9, 51, 4, 0, 0, 0, 60}, 0xff)...), true,
			func(r reply) bool { return r.lease.Expire.Sub(r.lease.Bound) == time.Minute }},
		{"121, then a malformed 121", raw(append(append(ack, 121, 5, 0, 10, 0, 0, 1, 121, 2, 33, 1), 0xff)...), true,
			func(r reply) bool { return len(r.lease.Options.Get(dhcpv4.OptionClasslessStaticRoute)) == 5 }},
		{"a 121 without a whole route stays present, empty", raw(append(append(ack, 121, 2, 33, 1), 0xff)...), true,
			func(r reply) bool {
				return r.lease.Options.Has(dhcpv4.OptionClasslessStaticRoute) && len(r.lease.Options.Get(dhcpv4.OptionClasslessStaticRoute)) == 0
			}},
		{"no End", raw(ack...), true, func(reply) bool { return true }},
		{"truncated", raw(append(ack, 6, 4, 10, 0)...), false, nil},
	} {
		r, ok := decodeReply(tt.b, time.Now())
		if ok != tt.ok || ok && !tt.check(r) {
			t.Errorf("%s: ok %v, lease %+v", tt.name, ok, r.lease)
		}
	}
}

// Per code: a valid copy, then a copy that networkd rejects (wrong length,
// a malformed search list, a 121 without a whole route): the valid copy
// stays. A malformed copy first is replaced by a valid one.
func TestOptionsKeepLastValidCopy(t *testing.T) {
	tlv := func(code byte, data ...byte) []byte { return append([]byte{code, byte(len(data))}, data...) }
	for _, tt := range []struct {
		code      byte
		good, bad []byte
	}{
		{1, []byte{255, 255, 255, 0}, []byte{255, 255, 255}},
		{28, []byte{10, 0, 0, 255}, []byte{10, 0, 0, 255, 0}},
		{51, []byte{0, 0, 0, 120}, []byte{0, 120}},
		{54, []byte{10, 0, 0, 1}, []byte{10, 0, 1}},
		{58, []byte{0, 0, 0, 60}, []byte{0, 0, 60}},
		{59, []byte{0, 0, 0, 90}, []byte{0, 0, 0, 0, 90}},
		{26, []byte{5, 220}, []byte{5}},
		{3, []byte{10, 0, 0, 1}, []byte{10, 0, 0, 2, 9}},
		{4, []byte{10, 0, 0, 3}, []byte{10, 0, 3}},
		{6, []byte{10, 0, 0, 53}, []byte{10, 0, 0, 53, 10}},
		{42, []byte{10, 0, 0, 123}, []byte{10, 0, 123}},
		{33, []byte{10, 1, 0, 0, 10, 0, 0, 1}, []byte{10, 2, 0, 0, 10, 0, 0}},
		{121, []byte{8, 10, 10, 0, 0, 1}, []byte{33, 1, 2, 3, 4, 5}},
		{119, []byte{1, 'a', 0}, []byte{1, 'b'}},
	} {
		if opts, ok := options(append(tlv(tt.code, tt.good...), tlv(tt.code, tt.bad...)...)); !ok || !slices.Equal(opts[tt.code], tt.good) {
			t.Errorf("option %d, valid then malformed: %x (ok %v), want %x", tt.code, opts[tt.code], ok, tt.good)
		}
		if opts, ok := options(append(tlv(tt.code, tt.bad...), tlv(tt.code, tt.good...)...)); !ok || !slices.Equal(opts[tt.code], tt.good) {
			t.Errorf("option %d, malformed then valid: %x (ok %v), want %x", tt.code, opts[tt.code], ok, tt.good)
		}
	}
	// 121: the routes before an error stay, like networkd.
	if opts, _ := options(tlv(121, 8, 10, 10, 0, 0, 1, 40, 1)); !slices.Equal(opts[121], []byte{8, 10, 10, 0, 0, 1}) {
		t.Errorf("121 with a bad second route: %x", opts[121])
	}
	// A route of width 33 ends the routes of 121, even with 5 bytes for
	// its destination.
	if opts, _ := options(tlv(121, 8, 10, 10, 0, 0, 1, 33, 1, 2, 3, 4, 5, 6, 7, 8, 9)); !slices.Equal(opts[121], []byte{8, 10, 10, 0, 0, 1}) {
		t.Errorf("121 with a route of width 33: %x", opts[121])
	}
	// Option 56 (message) like networkd parse_options: empty, a NUL but
	// at the end, or a byte over 127 drops the message.
	for _, tt := range []struct {
		msg []byte
		ok  bool
	}{
		{[]byte("no address"), true},
		{[]byte("no address\x00"), true},
		{nil, false},
		{[]byte("no\x00address"), false},
		{[]byte("a\x00\x00"), false},
		{[]byte("a\x80"), false},
		{[]byte("a\x7f"), true},
		{[]byte("adresse \xe9puis\xe9e"), false},
	} {
		if _, ok := options(tlv(56, tt.msg...)); ok != tt.ok {
			t.Errorf("option 56 %q: ok %v, want %v", tt.msg, ok, tt.ok)
		}
	}
	// A message type or overload option of another length drops the
	// message, whichever copy it is (networkd parse_options -EINVAL).
	for _, b := range [][]byte{
		append(tlv(53, 5), tlv(53, 5, 5)...),
		append(tlv(53, 5, 5), tlv(53, 5)...),
		tlv(52, 1, 1),
	} {
		if _, ok := options(b); ok {
			t.Errorf("options(%x) accepted", b)
		}
	}
}
