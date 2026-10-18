//go:build linux

// Package lldp provides LLDP frame listening for switch topology discovery.
package lldp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/sys/unix"
)

// Listen captures LLDP frames on the given interface until a frame is received
// or the context expires. Uses raw AF_PACKET sockets (no libpcap needed).
func Listen(ctx context.Context, iface string, timeout time.Duration) (*Neighbor, error) {
	fd, err := openRX(iface)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	slog.Info("listening for LLDP frames", "interface", iface, "timeout", timeout)

	buf := make([]byte, 1600)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("LLDP timeout on %s: %w", iface, ctx.Err())
		default:
		}

		neighbor := readNeighbor(fd, buf, iface)
		if neighbor != nil {
			slog.Info("LLDP neighbor discovered",
				"interface", iface,
				"chassisID", neighbor.ChassisID,
				"portID", neighbor.PortID,
				"systemName", neighbor.SystemName,
			)
			return neighbor, nil
		}
	}
}

// lldpGroups are the LLDP destination MACs: nearest bridge, nearest
// non-TPMR bridge and nearest customer bridge (IEEE 802.1AB).
var lldpGroups = []net.HardwareAddr{
	{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e},
	{0x01, 0x80, 0xc2, 0x00, 0x00, 0x03},
	{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00},
}

// openRX opens a raw socket that receives the LLDP frames of iface, with
// a 1 s read timeout. It joins the LLDP multicast groups like networkd
// (lldp-network.c) and lldpd: a real NIC that is not promiscuous drops
// frames to groups that nobody joined (a veth in the lab passes them).
func openRX(iface string) (int, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return -1, fmt.Errorf("interface %s: %w", iface, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(0x88cc)))
	if err != nil {
		return -1, fmt.Errorf("raw socket: %w", err)
	}
	for _, mac := range lldpGroups {
		mreq := &unix.PacketMreq{Ifindex: int32(ifi.Index), Type: unix.PACKET_MR_MULTICAST, Alen: uint16(len(mac))} //nolint:gosec // ifindex fits
		copy(mreq.Address[:], mac)
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, mreq); err != nil {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("join LLDP group %s on %s: %w", mac, iface, err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(0x88cc), Ifindex: ifi.Index}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind to %s: %w", iface, err)
	}
	tv := unix.Timeval{Sec: 1}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("setting read timeout: %w", err)
	}
	return fd, nil
}

// readNeighbor reads one frame and returns its neighbor, or nil after the
// read timeout, for a short frame, and for BOOTy's own frames: a bridge can
// reflect them, so a frame from our MAC or with our chassis ID is ours. An
// ETH_P_ALL socket would also get our sent frames (PACKET_OUTGOING); the
// LLDP socket of openRX does not, the check guards other callers.
func readNeighbor(fd int, buf []byte, iface string) *Neighbor {
	n, from, err := unix.Recvfrom(fd, buf, 0)
	if err != nil || n < 14 {
		return nil
	}
	if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
		return nil
	}
	nb := parseLLDP(buf[14:n], iface)
	if nb == nil || ownFrame(net.HardwareAddr(buf[6:12]), nb.ChassisID) {
		return nil
	}
	return nb
}

// ownFrame reports a frame from one of our NICs or with our chassis ID
// (the machine ID of this boot, or a NIC MAC; see tx.go).
func ownFrame(src net.HardwareAddr, chassis string) bool {
	if id := ownMachineID(); id != "" && chassis == id {
		return true
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifi := range ifaces {
		if len(ifi.HardwareAddr) == 0 {
			continue
		}
		if bytes.Equal(ifi.HardwareAddr, src) || ifi.HardwareAddr.String() == chassis {
			return true
		}
	}
	return false
}

// ownMachineID is the chassis ID that tx.go sends, when it exists.
var ownMachineID = func() string {
	b, err := os.ReadFile(machineIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// DiscoverAll listens for LLDP on all provided interfaces concurrently.
func DiscoverAll(ctx context.Context, ifaces []string, timeout time.Duration) []Neighbor {
	type result struct {
		neighbor *Neighbor
	}

	results := make(chan result, len(ifaces))
	for _, iface := range ifaces {
		go func(ifName string) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in LLDP listener goroutine", "interface", ifName, "panic", r)
					results <- result{nil}
				}
			}()

			n, err := Listen(ctx, ifName, timeout)
			if err != nil {
				slog.Debug("LLDP listen failed", "interface", ifName, "error", err)
				results <- result{nil}
				return
			}
			results <- result{n}
		}(iface)
	}

	var neighbors []Neighbor
	for range ifaces {
		r := <-results
		if r.neighbor != nil {
			neighbors = append(neighbors, *r.neighbor)
		}
	}
	return neighbors
}

// parseLLDP extracts LLDP neighbor information from raw LLDP payload. It
// drops an LLDPDU whose first three TLVs are not Chassis ID, Port ID and
// TTL in this order (IEEE 802.1AB). The End of LLDPDU TLV is optional
// since 802.1AB-2016; the decoder needs one, so a payload without it gets
// one appended (a present End TLV ends the decoding before it).
func parseLLDP(data []byte, iface string) *Neighbor {
	if !mandatoryTLVsFirst(data) {
		return nil
	}
	data = append(data[:len(data):len(data)], 0, 0)
	pkt := gopacket.NewPacket(data, layers.LayerTypeLinkLayerDiscovery, gopacket.NoCopy)

	lldpLayer := pkt.Layer(layers.LayerTypeLinkLayerDiscovery)
	if lldpLayer == nil {
		return nil
	}
	lldp, ok := lldpLayer.(*layers.LinkLayerDiscovery)
	if !ok {
		return nil
	}

	n := &Neighbor{
		Interface: iface,
		TTL:       lldp.TTL,
	}

	switch lldp.ChassisID.Subtype {
	case layers.LLDPChassisIDSubTypeMACAddr:
		if len(lldp.ChassisID.ID) >= 6 {
			n.ChassisID = net.HardwareAddr(lldp.ChassisID.ID).String()
		}
	default:
		n.ChassisID = sanitizeLLDP(string(lldp.ChassisID.ID))
	}

	n.PortID = sanitizeLLDP(string(lldp.PortID.ID))

	infoLayer := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo)
	if infoLayer != nil {
		if info, ok := infoLayer.(*layers.LinkLayerDiscoveryInfo); ok {
			n.SystemName = sanitizeLLDP(info.SysName)
			n.Description = sanitizeLLDP(info.SysDescription)
		}
	}

	return n
}

// mandatoryTLVsFirst reports whether the LLDPDU starts with the Chassis ID,
// Port ID and TTL TLVs, in this order and each within the data.
func mandatoryTLVsFirst(data []byte) bool {
	for _, want := range []layers.LLDPTLVType{layers.LLDPTLVChassisID, layers.LLDPTLVPortID, layers.LLDPTLVTTL} {
		if len(data) < 2 {
			return false
		}
		typ := layers.LLDPTLVType(data[0] >> 1)
		n := int(data[0]&1)<<8 | int(data[1])
		if typ != want || len(data) < 2+n || (typ == layers.LLDPTLVTTL && n < 2) {
			return false
		}
		data = data[2+n:]
	}
	return true
}

const maxLLDPFieldLen = 255

// sanitizeLLDP replaces control characters in untrusted LLDP TLV strings
// to prevent log injection and terminal escape sequence attacks.
func sanitizeLLDP(s string) string {
	if len(s) > maxLLDPFieldLen {
		s = s[:maxLLDPFieldLen]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}
