//go:build e2e_prodtpl

// LLDP of the production template (netplan "emit-lldp: true" on both
// uplinks): BOOTy must send the frames that systemd-networkd sends
// (sd-lldp-tx.c), so the leaf sees the server as its LLDP neighbor.
package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
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
		for i, nic := range prodtplUplinks {
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

// prodtplLLDPTLVs lists the TLV types of a frame in their order, so an
// extra or missing TLV shows.
func prodtplLLDPTLVs(frame []byte) string {
	pkt := gopacket.NewPacket(frame, layers.LayerTypeEthernet, gopacket.Default)
	l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
	if l == nil {
		return "none"
	}
	types := []string{"1", "2", "3"} // chassis ID, port ID, TTL
	for _, v := range l.Values {
		types = append(types, fmt.Sprint(uint8(v.Type)))
	}
	return strings.Join(types, ",")
}

// prodtplNodeLLDP returns the MAC of a NIC, the machine ID and the hostname
// of a lab container.
func prodtplNodeLLDP(t *testing.T, container, nic string) (mac, id, host string) {
	t.Helper()
	idFile := "/run/booty/machine-id"
	if container == prodtplNetworkd {
		idFile = "/etc/machine-id"
	}
	var out [3]string
	for i, args := range [][]string{{"cat", "/sys/class/net/" + nic + "/address"}, {"cat", idFile}, {"hostname"}} {
		o, err := prodtplExec(context.Background(), container, args...)
		if err != nil {
			t.Fatalf("%s %v: %v", container, args, err)
		}
		out[i] = strings.TrimSpace(o)
	}
	if container == prodtplNetworkd {
		out[1] = prodtplNetworkdChassisID(t, out[1])
	}
	return out[0], out[1], out[2]
}

// prodtplNetworkdLLDPAppID is LLDP_APP_ID of systemd sd-lldp-tx.c.
var prodtplNetworkdLLDPAppID = [16]byte{0x07, 0x3a, 0x43, 0xbf, 0x54, 0xde, 0x40, 0x8d, 0x8e, 0xc4, 0x96, 0xed, 0xfd, 0x94, 0x72, 0xdc}

// prodtplNetworkdChassisID returns the chassis ID that the networkd node
// sends for its machine ID. Up to systemd 257 (Ubuntu 22.04 and 24.04) it is
// the machine ID. From 258 (Ubuntu 26.04: 259) networkd sends an ID derived
// from it, sd_id128_get_machine_app_specific(LLDP_APP_ID), unless
// SYSTEMD_LLDP_SEND_MACHINE_ID=1 (systemd NEWS of 258): HMAC-SHA256 of the
// application ID keyed with the machine ID, first 16 bytes, as a v4 UUID.
// BOOTy sends the machine ID like the legacy ramdisk (249).
func prodtplNetworkdChassisID(t *testing.T, machineID string) string {
	t.Helper()
	v, err := prodtplExec(context.Background(), prodtplNetworkd, "dpkg-query", "-W", "-f", "${Version}", "systemd")
	if err != nil {
		t.Fatalf("networkd node systemd version: %v", err)
	}
	v = strings.TrimSpace(v)
	major, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if err != nil {
		t.Fatalf("networkd node systemd version %q: %v", v, err)
	}
	if major < 258 {
		t.Logf("networkd node: systemd %s, chassis ID is the machine ID", v)
		return machineID
	}
	key, err := hex.DecodeString(machineID)
	if err != nil || len(key) != 16 {
		t.Fatalf("networkd node machine ID %q: want 32 hex digits (%v)", machineID, err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(prodtplNetworkdLLDPAppID[:])
	id := mac.Sum(nil)[:16]
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	t.Logf("networkd node: systemd %s, chassis ID derived from the machine ID", v)
	return hex.EncodeToString(id)
}

// BOOTy sends the frame that systemd-networkd 249 sends with
// EmitLLDP=yes: on the same leaf, the frame of the networkd node and the
// frame of BOOTy are the same once the MAC, the machine ID, the interface
// name and the hostname are replaced, with the same TLVs in the same
// order. CI runs the networkd node on every Ubuntu LTS in standard support
// (networkd-test.Dockerfile, UBUNTU): 22.04 (systemd 249, the legacy
// ramdisk) is the parity reference; 24.04 (255) sends the same frame;
// 26.04 (259) differs only in the chassis ID (prodtplNetworkdChassisID).
func TestProdTplLLDPLikeNetworkd(t *testing.T) {
	norm := func(frame []byte, mac, id, nic, host string) string {
		r := strings.NewReplacer(mac, "MAC", id, "MACHINE-ID", nic, "IFNAME", host, "HOSTNAME")
		return r.Replace(prodtplLLDPView(frame)) + " tlvs=" + prodtplLLDPTLVs(frame)
	}
	capture := func(port string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		defer cancel()
		frame, err := prodtplLLDPFrame(ctx, port)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	mac, id, host := prodtplNodeLLDP(t, prodtplNetworkd, "lldp0")
	want := norm(capture("eth11"), mac, id, "lldp0", host)
	nic := prodtplUplinks[0]
	mac, id, host = prodtplNodeLLDP(t, prodtplBooty, nic)
	if got := norm(capture(prodtplBootyNode.leafPorts[0]), mac, id, nic, host); got != want {
		t.Errorf("BOOTy LLDP frame differs from networkd:\n  booty    %s\n  networkd %s", got, want)
	}
}

// BOOTy receives LLDP on every physical NIC (LLDP_RECEIVE, on by
// default): it logs the networkd node on lldp1 with its chassis ID (the
// machine ID up to systemd 257), its port and its hostname.
func TestProdTplLLDPReceive(t *testing.T) {
	_, id, host := prodtplNodeLLDP(t, prodtplNetworkd, "lldp1")
	want := []string{`msg="LLDP neighbor"`, "interface=lldp1", "chassisID=" + id, "portID=lldp1", "systemName=" + host}
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	for {
		out, err := exec.CommandContext(ctx, "docker", "logs", prodtplBooty).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if containsAll(line, want) {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("BOOTy did not log the networkd LLDP neighbor %v (%v)", want, err)
		case <-time.After(2 * time.Second):
		}
	}
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
