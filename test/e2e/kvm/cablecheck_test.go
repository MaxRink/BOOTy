//go:build e2e

package kvm

import (
	"bytes"
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

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
)

// cablecheckNICs are the MACs of the two NICs of the cable check VMs.
var cablecheckNICs = []string{"52:54:00:12:34:56", "52:54:00:12:34:57"}

// cablecheckBoot boots BOOTy from the test initramfs with the vars.
func cablecheckBoot(t *testing.T, vars map[string]string) []string {
	t.Helper()
	return []string{
		"-kernel", findKernel(t), "-initrd", buildProvisionInitramfs(t, vars),
		"-append", runnerKernelCmdline + " net.ifnames=0",
	}
}

// runCableCheckVM boots BOOTy (boot args) with two virtio NICs whose
// traffic QEMU writes to a pcap file each (filter-dump). It stops the VM
// after hold, or earlier when BOOTy reboots (-no-reboot), and returns the
// serial output, whether the VM was still running at the end, and the
// pcap paths.
func runCableCheckVM(t *testing.T, boot []string, hold time.Duration) (out []byte, running bool, pcaps []string) {
	t.Helper()
	dir := t.TempDir()
	args := append([]string{"-m", "1024", "-nographic", "-no-reboot"}, boot...)
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
	shutdown  int      // shutdown LLDPDUs (TTL 0) for the port of the NIC
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
	// The chassis of the NIC's first bm4x frame; every later frame of the
	// NIC, also the shutdown LLDPDU, must carry the same.
	var chassis *layers.LLDPChassisID
	sameChassis := func(c layers.LLDPChassisID) bool {
		return chassis != nil && c.Subtype == chassis.Subtype && bytes.Equal(c.ID, chassis.ID)
	}
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
			ownPort := l != nil && l.PortID.Subtype == layers.LLDPPortIDSubtypeMACAddr &&
				net.HardwareAddr(l.PortID.ID).String() == mac
			bm4x := ownPort && l.TTL > 0 && info != nil && info.SysName == "bare-minimal"
			if bm4x && chassis == nil {
				c := l.ChassisID
				chassis = &c
			}
			switch {
			case ownPort && l.TTL == 0 && len(l.Values) == 0 && sameChassis(l.ChassisID):
				// IEEE 802.1AB shutdown LLDPDU when BOOTy stops sending:
				// the chassis and port of the frames before it, only the
				// mandatory TLVs.
				tr.shutdown++
			case bm4x && sameChassis(l.ChassisID):
				tr.lldp++
			default:
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
	out, _, pcaps := runCableCheckVM(t, cablecheckBoot(t, vars), 3*time.Minute)
	if !strings.Contains(string(out), "using links network mode") {
		t.Fatalf("links mode not used. tail:\n%s", tail(out, 4000))
	}
	for i, mac := range cablecheckNICs {
		tr := readCableCheckPcap(t, pcaps[i], mac)
		if tr.lldp == 0 || len(tr.otherLLDP) > 0 {
			t.Errorf("NIC %s: %d bm4x LLDP frames, other LLDP frames %v", mac, tr.lldp, tr.otherLLDP)
		}
		// The run ends with a reboot, which stops the sender first.
		if tr.shutdown == 0 {
			t.Errorf("NIC %s: no shutdown LLDPDU before the reboot", mac)
		}
		if tr.ipv4 > 0 || len(tr.globalV6) > 0 {
			t.Errorf("NIC %s sent %d IPv4/ARP frames and IPv6 from %v, want link-local only", mac, tr.ipv4, tr.globalV6)
		}
	}
	if t.Failed() {
		t.Logf("serial tail:\n%s", tail(out, 4000))
	}
}

// checkISOVars reads hack/check-iso/vars, the vars of the cable check ISO.
func checkISOVars(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "hack", "check-iso", "vars"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			vars[k] = v
		}
	}
	return vars
}

// requireHeldCableCheck checks a VM that ran the cable check ISO vars for
// hold: it still ran at the end (CHECK_HOLD=until-poweroff, no reboot),
// and each NIC sent the 4 fast LLDP frames and the next one 30 s later
// with the bm4x identity, and nothing on IPv4.
func requireHeldCableCheck(t *testing.T, out []byte, running bool, pcaps []string) {
	t.Helper()
	if !running || !strings.Contains(string(out), "checks done, holding until power off") {
		t.Fatalf("VM running %v at the end, want it holding until power off. tail:\n%s", running, tail(out, 4000))
	}
	for i, mac := range cablecheckNICs {
		tr := readCableCheckPcap(t, pcaps[i], mac)
		if tr.lldp < 5 || len(tr.otherLLDP) > 0 || tr.ipv4 > 0 || len(tr.globalV6) > 0 {
			t.Errorf("NIC %s: %d bm4x LLDP frames (want 5 or more), other LLDP %v, %d IPv4/ARP, IPv6 from %v",
				mac, tr.lldp, tr.otherLLDP, tr.ipv4, tr.globalV6)
		}
	}
	if t.Failed() {
		t.Logf("serial tail:\n%s", tail(out, 4000))
	}
}

// The vars of the cable check ISO (hack/check-iso/vars): BOOTy waits up to
// 90 s for LLDP neighbors (there are none here), runs the checks and keeps
// the links and LLDP up until the VM is powered off; the test powers it off
// after 130 s.
func TestCheckISOVarsHoldQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	out, running, pcaps := runCableCheckVM(t, cablecheckBoot(t, checkISOVars(t)), 130*time.Second)
	requireHeldCableCheck(t, out, running, pcaps)
}

// checkISO returns BOOTY_CHECK_ISO, a booty-check.iso built with make
// check-iso; the tests skip without it.
func checkISO(t *testing.T) string {
	t.Helper()
	iso := os.Getenv("BOOTY_CHECK_ISO")
	if iso == "" {
		t.Skip("BOOTY_CHECK_ISO not set (make check-iso)")
	}
	qemuAvailable(t)
	return iso
}

// checkISOBoot boots the cable check ISO from a SCSI CD, like BMC virtual
// media, with the firmware of the ISO tests: bios, uefi or secureboot
// (OVMF with the Microsoft keys). extra adds devices, such as a decoy disk.
func checkISOBoot(t *testing.T, iso, mode string, extra ...string) []string {
	t.Helper()
	return append(append(isoFirmware(t, mode), extra...), isoCDArgs(iso)...)
}

// The cable check ISO booted from CD with nothing else (BIOS, ISOLINUX)
// runs the cable check and holds.
func TestCheckISOQEMU(t *testing.T) {
	iso := checkISO(t)
	out, running, pcaps := runCableCheckVM(t, checkISOBoot(t, iso, "bios"), 240*time.Second)
	requireHeldCableCheck(t, out, running, pcaps)
}

// The same with UEFI: shim, GRUB and the kernel from the EFI system
// partition of make-iso.sh.
func TestCheckISOUEFIQEMU(t *testing.T) {
	iso := checkISO(t)
	out, running, pcaps := runCableCheckVM(t, checkISOBoot(t, iso, "uefi"), 240*time.Second)
	requireHeldCableCheck(t, out, running, pcaps)
}

// With Secure Boot on (Microsoft keys enrolled): shim (Microsoft-signed),
// GRUB and the kernel (Debian-signed) pass the verification and the kernel
// reports Secure Boot.
func TestCheckISOSecureBootQEMU(t *testing.T) {
	iso := checkISO(t)
	out, running, pcaps := runCableCheckVM(t, checkISOBoot(t, iso, "secureboot"), 240*time.Second)
	if !strings.Contains(string(out), "Secure boot enabled") {
		t.Fatalf("the kernel does not report Secure Boot. tail:\n%s", tail(out, 4000))
	}
	requireHeldCableCheck(t, out, running, pcaps)
}

// A second disk with the BOOTy volume label and GRUB configs at every path
// (decoyDisk of the ISO tests) must not take over the boot: GRUB uses the
// ISO it was started from, and the cable check runs from it.
func TestCheckISOIgnoresOtherBootyDiskQEMU(t *testing.T) {
	iso := checkISO(t)
	decoy := []string{
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=decoy,readonly=on", decoyDisk(t)),
		"-device", "virtio-blk-pci,drive=decoy,bootindex=2", // in the boot order, so OVMF connects it
	}
	out, running, pcaps := runCableCheckVM(t, checkISOBoot(t, iso, "secureboot", decoy...), 240*time.Second)
	if strings.Contains(string(out), wrongMediaMarker) {
		t.Fatalf("GRUB loaded the config of the decoy disk. tail:\n%s", tail(out, 4000))
	}
	requireHeldCableCheck(t, out, running, pcaps)
}
