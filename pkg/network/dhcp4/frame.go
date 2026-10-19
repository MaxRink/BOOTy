package dhcp4

import (
	"encoding/binary"
	"net"
)

// udpFrame returns an Ethernet broadcast frame from mac that carries an
// IPv4 UDP datagram (TTL 64, ID 0, no flags) with tos and both checksums.
func udpFrame(mac net.HardwareAddr, src, dst net.IP, sport, dport uint16, tos byte, payload []byte) []byte {
	const ethLen, ipLen, udpLen = 14, 20, 8
	b := make([]byte, ethLen+ipLen+udpLen+len(payload))
	copy(b, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(b[6:], mac)
	binary.BigEndian.PutUint16(b[12:], 0x0800)
	ip := b[ethLen:]
	ip[0], ip[1], ip[8], ip[9] = 0x45, tos, 64, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip))) //nolint:gosec // one DHCP message
	copy(ip[12:], src.To4())
	copy(ip[16:], dst.To4())
	binary.BigEndian.PutUint16(ip[10:], checksum(0, ip[:ipLen]))
	udp := ip[ipLen:]
	binary.BigEndian.PutUint16(udp, sport)
	binary.BigEndian.PutUint16(udp[2:], dport)
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp))) //nolint:gosec // one DHCP message
	copy(udp[udpLen:], payload)
	pseudo := append(append([]byte(nil), ip[12:20]...), 0, 17, udp[4], udp[5])
	binary.BigEndian.PutUint16(udp[6:], checksum(sum(0, pseudo), udp))
	return b
}

// udpPayload returns the payload of an Ethernet frame that carries an
// unfragmented IPv4 UDP datagram to port 68. It checks the IP header
// checksum and, with verifyUDP, a UDP checksum that is not 0, like
// networkd's dhcp_packet_verify_headers; a frame whose checksum offload has
// not run yet comes without verifyUDP (the IP header checksum is always
// filled in).
func udpPayload(frame []byte, verifyUDP bool) ([]byte, bool) {
	const ethLen = 14
	if len(frame) < ethLen+20 || binary.BigEndian.Uint16(frame[12:]) != 0x0800 {
		return nil, false
	}
	ip, ihl, ok := ipv4UDP(frame[ethLen:])
	if !ok {
		return nil, false
	}
	udp := ip[ihl:]
	ulen := int(binary.BigEndian.Uint16(udp[4:]))
	if binary.BigEndian.Uint16(udp[2:]) != 68 || ulen < 8 || ulen > len(udp) {
		return nil, false
	}
	udp = udp[:ulen]
	if verifyUDP && !udpChecksumOK(ip, udp) {
		return nil, false
	}
	return append([]byte(nil), udp[8:]...), true
}

// ipv4UDP returns an unfragmented IPv4 UDP packet cut to its total length
// and its header length, with a right header checksum.
func ipv4UDP(ip []byte) (packet []byte, ihl int, ok bool) {
	ihl = int(ip[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(ip[2:]))
	if ip[0]>>4 != 4 || ihl < 20 || total < ihl+8 || total > len(ip) || ip[9] != 17 ||
		binary.BigEndian.Uint16(ip[6:])&0x3fff != 0 { // MF set or an offset: a fragment
		return nil, 0, false
	}
	if checksum(0, ip[:ihl]) != 0 {
		return nil, 0, false
	}
	return ip[:total], ihl, true
}

// udpChecksumOK checks a UDP checksum with the IPv4 pseudo header; 0 means
// the sender sent none.
func udpChecksumOK(ip, udp []byte) bool {
	if binary.BigEndian.Uint16(udp[6:]) == 0 {
		return true
	}
	pseudo := make([]byte, 0, 12)
	pseudo = append(pseudo, ip[12:20]...)
	pseudo = append(pseudo, 0, 17, byte(len(udp)>>8), byte(len(udp)))
	return checksum(sum(0, pseudo), udp) == 0
}

// sum adds b as 16-bit big-endian words (RFC 1071).
func sum(acc uint32, b []byte) uint32 {
	for ; len(b) >= 2; b = b[2:] {
		acc += uint32(binary.BigEndian.Uint16(b))
	}
	if len(b) == 1 {
		acc += uint32(b[0]) << 8
	}
	return acc
}

// checksum folds acc plus b; 0 means the data carries a valid checksum.
func checksum(acc uint32, b []byte) uint16 {
	acc = sum(acc, b)
	for acc>>16 != 0 {
		acc = acc&0xffff + acc>>16
	}
	return ^uint16(acc)
}
