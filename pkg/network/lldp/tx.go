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
}

// SystemInfo reads the machine ID of the boot, the hostname and the pretty
// hostname like systemd. Without a machine ID (it could not be written)
// the frames use the NIC MAC as chassis ID, a BOOTy extension.
func SystemInfo() Info {
	info := Info{MachineID: machineID()}
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

// Frame returns the Ethernet frame that networkd sends on a NIC.
func Frame(ifname string, mac net.HardwareAddr, info Info) ([]byte, error) {
	chassis := layers.LLDPChassisID{Subtype: layers.LLDPChassisIDSubTypeLocal, ID: []byte(info.MachineID)}
	if info.MachineID == "" {
		chassis = layers.LLDPChassisID{Subtype: layers.LLDPChassisIDSubTypeMACAddr, ID: mac}
	}
	caps := make([]byte, 4)
	binary.BigEndian.PutUint16(caps[0:], capSupported)
	binary.BigEndian.PutUint16(caps[2:], capEnabled)
	values := []layers.LinkLayerDiscoveryValue{tlv(layers.LLDPTLVSysName, info.SystemName)}
	if info.PrettyHostname != "" {
		values = append(values, tlv(layers.LLDPTLVSysDescription, info.PrettyHostname))
	}
	values = append(values, layers.LinkLayerDiscoveryValue{Type: layers.LLDPTLVSysCapabilities, Length: 4, Value: caps})
	buf := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{},
		&layers.Ethernet{SrcMAC: mac, DstMAC: nearestBridge, EthernetType: layers.EthernetTypeLinkLayerDiscovery},
		&layers.LinkLayerDiscovery{
			ChassisID: chassis,
			PortID:    layers.LLDPPortID{Subtype: layers.LLDPPortIDSubtypeIfaceName, ID: []byte(ifname)},
			TTL:       txTTL,
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
func Emit(ctx context.Context, ifaces []string, info Info, log *slog.Logger) (wait func()) {
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

func emitOn(ctx context.Context, iface string, info Info, log *slog.Logger) {
	log.Info("sending LLDP frames like networkd EmitLLDP=yes")
	failed := false
	for n := 0; ; n++ {
		if err := send(iface, info); err != nil && !failed {
			// Log once; the NIC may come back (carrier, rename).
			log.Warn("LLDP frame not sent", "error", err)
			failed = true
		} else if err == nil {
			failed = false
		}
		wait := txInterval
		if n < txFastInit-1 {
			wait = txFastInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func send(iface string, info Info) error {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("interface %s: %w", iface, err)
	}
	frame, err := Frame(iface, ifi.HardwareAddr, info)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		return fmt.Errorf("raw socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	addr := &unix.SockaddrLinklayer{Protocol: htons(0x88cc), Ifindex: ifi.Index, Halen: 6}
	copy(addr.Addr[:], nearestBridge)
	if err := unix.Sendto(fd, frame, 0, addr); err != nil {
		return fmt.Errorf("send on %s: %w", iface, err)
	}
	return nil
}
