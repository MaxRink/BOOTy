//go:build e2e

package kvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

// cablecheckNICs are the MACs of the two NICs of the cable check VMs.
var cablecheckNICs = []string{"52:54:00:12:34:56", "52:54:00:12:34:57"}

// runCableCheckVM boots BOOTy with the vars and two virtio NICs whose
// traffic QEMU writes to a pcap file each (filter-dump). It stops the VM
// after hold, or earlier when BOOTy reboots (-no-reboot), and returns the
// serial output, whether the VM was still running at the end, and the
// pcap paths.
func runCableCheckVM(t *testing.T, vars map[string]string, hold time.Duration) (out []byte, running bool, pcaps []string) {
	t.Helper()
	dir := t.TempDir()
	args := []string{
		"-m", "1024", "-nographic", "-no-reboot",
		"-kernel", findKernel(t), "-initrd", buildProvisionInitramfs(t, vars),
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	for i, mac := range cablecheckNICs {
		pcap := filepath.Join(dir, fmt.Sprintf("nic%d.pcap", i))
		pcaps = append(pcaps, pcap)
		// restrict=on: the user backend only sees the frames, the guest
		// gets no DHCP server or router from it.
		args = append(args,
			"-netdev", fmt.Sprintf("user,id=n%d,restrict=on", i),
			"-device", fmt.Sprintf("virtio-net-pci,netdev=n%d,mac=%s", i, mac),
			"-object", fmt.Sprintf("filter-dump,id=f%d,netdev=n%d,file=%s", i, i, pcap))
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	ctx, cancel := context.WithTimeout(context.Background(), hold)
	defer cancel()
	out, _ = exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	return out, ctx.Err() != nil, pcaps
}

// cablecheckTraffic is what one NIC sent.
type cablecheckTraffic struct {
	lldp      int      // LLDP frames with the bm4x identity of the NIC
	otherLLDP []string // LLDP frames with another identity
	ipv4      int      // IPv4 or ARP frames (DHCP among them)
	globalV6  []string // IPv6 sources that are not link-local or ::
}

func readCableCheckPcap(t *testing.T, path, mac string) cablecheckTraffic {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	r, err := pcapgo.NewReader(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var tr cablecheckTraffic
	for {
		data, _, err := r.ReadPacketData()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return tr
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		pkt := gopacket.NewPacket(data, layers.LayerTypeEthernet, gopacket.Default)
		eth, _ := pkt.Layer(layers.LayerTypeEthernet).(*layers.Ethernet)
		if eth == nil || eth.SrcMAC.String() != mac {
			continue // only what the guest sent on this NIC
		}
		switch eth.EthernetType {
		case layers.EthernetTypeLinkLayerDiscovery:
			l, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscovery).(*layers.LinkLayerDiscovery)
			info, _ := pkt.Layer(layers.LayerTypeLinkLayerDiscoveryInfo).(*layers.LinkLayerDiscoveryInfo)
			if l != nil && info != nil && l.PortID.Subtype == layers.LLDPPortIDSubtypeMACAddr &&
				net.HardwareAddr(l.PortID.ID).String() == mac && info.SysName == "bare-minimal" {
				tr.lldp++
			} else {
				tr.otherLLDP = append(tr.otherLLDP, fmt.Sprintf("%x", data))
			}
		case layers.EthernetTypeIPv4, layers.EthernetTypeARP:
			tr.ipv4++
		case layers.EthernetTypeIPv6:
			ip, _ := pkt.Layer(layers.LayerTypeIPv6).(*layers.IPv6)
			if ip != nil && !ip.SrcIP.IsLinkLocalUnicast() && !ip.SrcIP.IsUnspecified() {
				tr.globalV6 = append(tr.globalV6, ip.SrcIP.String())
			}
		}
	}
}

// NETWORK_MODE=links brings both NICs up with IPv6 link-local only: no
// DHCP and no IPv4 at all, no global IPv6 address, and with
// LLDP_PROFILE=bm4x-cablecheck LLDP frames with the bm4x identity on every
// NIC.
func TestLinksModeLLDPQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	vars := map[string]string{
		"MODE":         "check",
		"NETWORK_MODE": "links",
		"LLDP_PROFILE": "bm4x-cablecheck",
	}
	out, _, pcaps := runCableCheckVM(t, vars, 3*time.Minute)
	if !strings.Contains(string(out), "using links network mode") {
		t.Fatalf("links mode not used. tail:\n%s", tail(out, 4000))
	}
	for i, mac := range cablecheckNICs {
		tr := readCableCheckPcap(t, pcaps[i], mac)
		if tr.lldp == 0 || len(tr.otherLLDP) > 0 {
			t.Errorf("NIC %s: %d bm4x LLDP frames, other LLDP frames %v", mac, tr.lldp, tr.otherLLDP)
		}
		if tr.ipv4 > 0 || len(tr.globalV6) > 0 {
			t.Errorf("NIC %s sent %d IPv4/ARP frames and IPv6 from %v, want link-local only", mac, tr.ipv4, tr.globalV6)
		}
	}
	if t.Failed() {
		t.Logf("serial tail:\n%s", tail(out, 4000))
	}
}
