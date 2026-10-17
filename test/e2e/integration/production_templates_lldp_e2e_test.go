//go:build e2e_prodtpl

// LLDP of the production template (netplan "emit-lldp: true" on both
// uplinks): BOOTy must send the frames that systemd-networkd sends
// (sd-lldp-tx.c), so the leaf sees the server as its LLDP neighbor.
package integration

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// prodtplLLDPFrame captures one LLDP frame on a leaf port. tcpdump runs in
// the test image in the network namespace of the leaf.
func prodtplLLDPFrame(ctx context.Context, port string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "container:"+prodtplLeaf,
		"--entrypoint", "tcpdump", "booty-gobgp-test:latest",
		"-c", "1", "-nn", "-xx", "-i", port, "ether", "proto", "0x88cc")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tcpdump on leaf %s: %w", port, err)
	}
	var frame []byte
	for _, line := range strings.Split(string(out), "\n") {
		hexPart, ok := strings.CutPrefix(strings.TrimSpace(line), "0x")
		if !ok {
			continue
		}
		_, words, ok := strings.Cut(hexPart, ":")
		if !ok {
			continue
		}
		b, err := hex.DecodeString(strings.Join(strings.Fields(words), ""))
		if err != nil {
			return nil, fmt.Errorf("decode tcpdump line %q: %w", line, err)
		}
		frame = append(frame, b...)
	}
	return frame, nil
}

// prodtplLLDPView describes a frame the way the networkd frame is defined.
func prodtplLLDPView(frame []byte) string {
	pkt := gopacket.NewPacket(frame, layers.LayerTypeEthernet, gopacket.Default)
	eth, _ := pkt.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
	l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	info, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
	if eth == nil || l == nil || info == nil {
		return fmt.Sprintf("not an LLDP frame: %x", frame)
	}
	chassis := string(l.ChassisID.ID)
	if l.ChassisID.Subtype == layers.LLDPChassisIDSubTypeMACAddr {
		chassis = net.HardwareAddr(l.ChassisID.ID).String()
	}
	caps := "none"
	for _, v := range l.Values {
		if v.Type == layers.LLDPTLVSysCapabilities && len(v.Value) == 4 {
			caps = fmt.Sprintf("%04x/%04x", binary.BigEndian.Uint16(v.Value), binary.BigEndian.Uint16(v.Value[2:]))
		}
	}
	return fmt.Sprintf("dst=%s src=%s chassis=%d:%s port=%d:%s ttl=%d name=%s caps=%s",
		eth.DstMAC, eth.SrcMAC, l.ChassisID.Subtype, chassis, l.PortID.Subtype, l.PortID.ID, l.TTL, info.SysName, caps)
}

// The templates (BOOTy) and LLDP_EMIT (vars-only server) send, on both
// uplinks, frames to the nearest-bridge address with the machine ID of the
// boot as locally assigned chassis ID (subtype 7, 32 hex digits, like
// networkd; the ramdisk has no /etc/machine-id, so BOOTy creates one in
// /run/booty/machine-id like systemd does at boot), the interface name as
// port ID, TTL 120, the hostname and the networkd capabilities: station,
// bridge and router supported, station enabled.
func TestProdTplLLDP(t *testing.T) {
	for _, node := range []prodtplNode{prodtplBootyNode, prodtplNativeNode} {
		c := prodtplContainer(node)
		host, err := prodtplExec(context.Background(), c, "hostname")
		if err != nil {
			t.Fatalf("%s hostname: %v", node.name, err)
		}
		id, err := prodtplExec(context.Background(), c, "cat", "/run/booty/machine-id")
		if err != nil {
			t.Fatalf("%s machine ID: %v", node.name, err)
		}
		id = strings.TrimSpace(id)
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
			t.Fatalf("%s machine ID %q, want 32 hex digits", node.name, id)
		}
		for i, nic := range []string{"ens1f0np0", "ens1f1np1"} {
			mac, err := prodtplExec(context.Background(), c, "cat", "/sys/class/net/"+nic+"/address")
			if err != nil {
				t.Fatalf("%s %s MAC: %v", node.name, nic, err)
			}
			mac = strings.TrimSpace(mac)
			want := fmt.Sprintf("dst=01:80:c2:00:00:0e src=%s chassis=7:%s port=5:%s ttl=120 name=%s caps=0094/0080",
				mac, id, nic, strings.TrimSpace(host))
			// A frame every 30 s after the first four.
			ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
			frame, err := prodtplLLDPFrame(ctx, node.leafPorts[i])
			cancel()
			if err != nil {
				t.Fatalf("%s %s: %v", node.name, nic, err)
			}
			if got := prodtplLLDPView(frame); got != want {
				t.Errorf("%s %s LLDP:\n  got  %s\n  want %s", node.name, nic, got, want)
			}
		}
	}
}
