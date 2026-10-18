//go:build linux

package lldp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/sys/unix"
)

// Transmit timing of systemd-networkd (sd-lldp-tx.c): 4 fast frames 1 s
// apart, then one frame every 30 s, TTL 120 s (4 intervals).
const (
	txFastInit     = 4
	txFastInterval = time.Second
	txInterval     = 30 * time.Second
	txTTL          = 120
)

// nearestBridge is the destination of EmitLLDP=yes (nearest-bridge).
var nearestBridge = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}

// System capabilities as networkd sets them: station, bridge and router
// supported; station enabled without IPForward= in the .network file,
// which netplan does not write.
const (
	capStation   = 0x0080
	capBridge    = 0x0004
	capRouter    = 0x0010
	capSupported = capStation | capBridge | capRouter
	capEnabled   = capStation
)

// Info is the system part of the frames.
type Info struct {
	MachineID      string // chassis ID (locally assigned), like networkd
	SystemName     string // hostname
	PrettyHostname string // system description; omitted when empty
	Profile        string // identity profile; "" is ProfileNetworkd
	PortIDFormat   string // port ID of the bm4x-cablecheck profile; "" is PortIDMAC
	// CarrierOnly skips NICs without carrier instead of trying to send
	// (LLDP_EMIT_ALL). The bm4x-cablecheck profile always does.
	CarrierOnly bool
	// Announce is the announce level; "" is AnnounceParity. System is the
	// host data that the standard and full levels send.
	Announce string
	System   System
}

func (i *Info) carrierOnly() bool {
	return i.CarrierOnly || i.Profile == ProfileBM4XCableCheck
}

// SystemInfo reads the machine ID of the boot, the hostname and the pretty
// hostname like systemd. Without a machine ID (it could not be written)
// the frames use the NIC MAC as chassis ID, a BOOTy extension.
func SystemInfo() Info {
	info := Info{MachineID: machineID(), System: readSystem()}
	info.SystemName, _ = os.Hostname()
	if b, err := os.ReadFile("/etc/machine-info"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "PRETTY_HOSTNAME="); ok {
				info.PrettyHostname = strings.Trim(v, `"'`)
			}
		}
	}
	return info
}

// machineIDPath holds the machine ID of this boot. It is not
// /etc/machine-id: the ramdisk has none, a value there could come with the
// image (the same on every host) or from the provisioned OS, and BOOTy must
// neither reuse it nor write one into the OS. /run starts empty at every
// boot.
var machineIDPath = "/run/booty/machine-id"

// machineID returns the machine ID of this boot, the chassis ID of the
// LLDP frames. Like systemd when it boots the legacy ramdisk with an empty
// /etc/machine-id, BOOTy creates a random one (32 hex digits) at first use;
// a restart of BOOTy in the same boot keeps it. It returns "" only when the
// ID cannot be created or written.
func machineID() string {
	if b, err := os.ReadFile(machineIDPath); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	// A version 4 UUID, like sd_id128_randomize.
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(machineIDPath), 0o755); err != nil { //nolint:gosec // world-readable like /etc/machine-id
		return ""
	}
	if err := os.WriteFile(machineIDPath, []byte(id+"\n"), 0o444); err != nil { //nolint:gosec // world-readable like /etc/machine-id
		return ""
	}
	return id
}

// Frame returns the Ethernet frame that networkd sends on a NIC. The
// bm4x-cablecheck profile sends the port ID of info.PortIDFormat (default
// the NIC MAC, subtype 3), the interface name as port description, like
// lldpd, and the system name "bare-minimal"; the rest stays as networkd
// sends it.
func Frame(ifname string, mac net.HardwareAddr, info *Info) ([]byte, error) {
	caps := make([]byte, 4)
	binary.BigEndian.PutUint16(caps[0:], capSupported)
	binary.BigEndian.PutUint16(caps[2:], capEnabled)
	var values []layers.LinkLayerDiscoveryValue
	sysName := info.SystemName
	if info.Profile == ProfileBM4XCableCheck {
		values = append(values, tlv(layers.LLDPTLVPortDescription, ifname))
		sysName = BM4XSystemName
	}
	values = append(values, tlv(layers.LLDPTLVSysName, sysName))
	if info.PrettyHostname != "" {
		values = append(values, tlv(layers.LLDPTLVSysDescription, info.PrettyHostname))
	}
	values = append(values, layers.LinkLayerDiscoveryValue{Type: layers.LLDPTLVSysCapabilities, Length: 4, Value: caps})
	if info.Announce == AnnounceStandard || info.Announce == AnnounceFull {
		values = announce(values, ifname, mac, info)
	}
	return frame(ifname, mac, info, txTTL, values)
}

// ShutdownFrame returns the shutdown LLDPDU of IEEE 802.1AB (TTL 0 and only
// the mandatory TLVs, with the chassis and port ID of the normal frames): the
// neighbor removes this system at once instead of after the TTL of the last
// frame.
func ShutdownFrame(ifname string, mac net.HardwareAddr, info *Info) ([]byte, error) {
	return frame(ifname, mac, info, 0, nil)
}

func frame(ifname string, mac net.HardwareAddr, info *Info, ttl uint16, values []layers.LinkLayerDiscoveryValue) ([]byte, error) {
	chassis := layers.LLDPChassisID{Subtype: layers.LLDPChassisIDSubTypeLocal, ID: []byte(info.MachineID)}
	if info.MachineID == "" {
		chassis = layers.LLDPChassisID{Subtype: layers.LLDPChassisIDSubTypeMACAddr, ID: mac}
	}
	port := layers.LLDPPortID{Subtype: layers.LLDPPortIDSubtypeIfaceName, ID: []byte(ifname)}
	if info.Profile == ProfileBM4XCableCheck {
		port = bm4xPortID(info.PortIDFormat, ifname, mac)
	}
	buf := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{},
		&layers.Ethernet{SrcMAC: mac, DstMAC: nearestBridge, EthernetType: layers.EthernetTypeLinkLayerDiscovery},
		&layers.LinkLayerDiscovery{
			ChassisID: chassis,
			PortID:    port,
			TTL:       ttl,
			Values:    values,
		})
	if err != nil {
		return nil, fmt.Errorf("serialize LLDP frame: %w", err)
	}
	return buf.Bytes(), nil
}

func tlv(t layers.LLDPTLVType, s string) layers.LinkLayerDiscoveryValue {
	if len(s) > maxLLDPFieldLen {
		s = s[:maxLLDPFieldLen]
	}
	return layers.LinkLayerDiscoveryValue{Type: t, Length: uint16(len(s)), Value: []byte(s)} //nolint:gosec // at most 255
}

// Emit sends LLDP frames on every NIC until ctx ends, like networkd with
// EmitLLDP=yes. It returns a function that waits for the senders.
func Emit(ctx context.Context, ifaces []string, info *Info, log *slog.Logger) (wait func()) {
	var wg sync.WaitGroup
	for _, iface := range ifaces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			emitOn(ctx, iface, info, log.With("interface", iface))
		}()
	}
	return wg.Wait
}

// bm4xPortID is the port ID of the bm4x-cablecheck profile in a format of
// network.lldp.portIDFormat (validated by the config).
func bm4xPortID(format, ifname string, mac net.HardwareAddr) layers.LLDPPortID {
	switch format {
	case PortIDMACStringColon:
		return layers.LLDPPortID{Subtype: layers.LLDPPortIDSubtypeLocal, ID: []byte(mac.String())}
	case PortIDMACStringDotted:
		return layers.LLDPPortID{Subtype: layers.LLDPPortIDSubtypeLocal, ID: []byte(dottedMAC(mac))}
	case PortIDIfname:
		return layers.LLDPPortID{Subtype: layers.LLDPPortIDSubtypeIfaceName, ID: []byte(ifname)}
	default:
		return layers.LLDPPortID{Subtype: layers.LLDPPortIDSubtypeMACAddr, ID: mac}
	}
}

// dottedMAC is the Cisco form of a MAC: groups of four hex digits.
func dottedMAC(mac net.HardwareAddr) string {
	h := hex.EncodeToString(mac)
	var b strings.Builder
	for i := 0; i < len(h); i += 4 {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(h[i:min(i+4, len(h))])
	}
	return b.String()
}

func emitOn(ctx context.Context, iface string, info *Info, log *slog.Logger) {
	if info.Profile == ProfileBM4XCableCheck {
		log.Info("sending LLDP frames with the bm4x-cablecheck identity")
	} else {
		log.Info("sending LLDP frames like networkd EmitLLDP=yes")
	}
	failed := false
	var fast fastInit
	for {
		frame, err := sendFrame(iface, info, Frame)
		recordTX(iface, info, frame, err, time.Now())
		if err != nil && !failed {
			// Log once; the NIC may come back (carrier, rename).
			log.Warn("LLDP frame not sent", "error", err)
			failed = true
		} else if err == nil {
			failed = false
		}
		wait := fast.next(frame, err)
		select {
		case <-ctx.Done():
			// IEEE 802.1AB: a port that stops sending sends a shutdown
			// LLDPDU, so the switch does not keep BOOTy for the TTL.
			if _, err := sendFrame(iface, info, ShutdownFrame); err != nil {
				log.Debug("LLDP shutdown frame not sent", "error", err)
			}
			return
		case <-time.After(wait):
		}
	}
}

// sendFrame sends the frame that build makes for the NIC and returns it;
// nil when the NIC has no carrier. Tests replace it.
var sendFrame = func(iface string, info *Info, build func(string, net.HardwareAddr, *Info) ([]byte, error)) ([]byte, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", iface, err)
	}
	if info.carrierOnly() && ifi.Flags&net.FlagRunning == 0 {
		return nil, nil // no carrier: send once the link is up
	}
	frame, err := build(iface, ifi.HardwareAddr, info)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		return nil, fmt.Errorf("raw socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	addr := &unix.SockaddrLinklayer{Protocol: htons(0x88cc), Ifindex: ifi.Index, Halen: 6}
	copy(addr.Addr[:], nearestBridge)
	if err := unix.Sendto(fd, frame, 0, addr); err != nil {
		return nil, fmt.Errorf("send on %s: %w", iface, err)
	}
	return frame, nil
}

// fastInit counts the frames sent since the link came up: the first
// txFastInit go out txFastInterval apart, then one every txInterval. A NIC
// without carrier (frame nil, no error) starts over and is polled at the
// fast interval, so its first frames go out as soon as the link is up,
// like networkd after a carrier change.
type fastInit struct{ sent int }

func (f *fastInit) next(frame []byte, err error) time.Duration {
	switch {
	case err == nil && frame != nil:
		f.sent++
	case err == nil:
		f.sent = 0
	}
	if f.sent < txFastInit {
		return txFastInterval
	}
	return txInterval
}
