//go:build e2e

package kvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// prodtplLeafConf is an FRR leaf with one unnumbered eBGP session per tap,
// like the fabric leaf of the production template lab (AS 65500).
const prodtplLeafConf = `frr defaults traditional
hostname leaf
!
router bgp 65500
 bgp router-id 10.255.0.1
 no bgp ebgp-requires-policy
 neighbor tpl0 interface remote-as external
 neighbor tpl1 interface remote-as external
 !
 address-family ipv4 unicast
  neighbor tpl0 activate
  neighbor tpl1 activate
 exit-address-family
!
`

// TestProdTplTemplatesQEMU boots BOOTy in a real VM with the rendered
// bm4x-operator templates (pkg/network/frrconf/testdata/bm4x/two-uplinks,
// uplinks enp0s5 and enp0s6 on PCI 00:05.0 and 00:06.0) as provisioner
// files. Each uplink is a tap into an FRR leaf. The ramdisk names the NICs
// eth0 and eth1; BOOTy must rename them to the template names, build the
// template devices in GoBGP mode and bring up both unnumbered leaf
// sessions with the local-as 65501, announcing the underlay /32 (#630
// section 4.3, the KVM run with the real kernel and initramfs).
func TestProdTplTemplatesQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for the FRR leaf")
	}

	fixture := filepath.Join("..", "..", "..", "pkg", "network", "frrconf", "testdata", "bm4x", "two-uplinks")
	files := map[string]string{}
	for src, dst := range map[string]string{"netplan.yaml": "deploy/file-system/etc/netplan/netplan.yaml", "frr.conf": "deploy/file-system/etc/frr/frr.conf"} {
		data, err := os.ReadFile(filepath.Join(fixture, src))
		if err != nil {
			t.Fatal(err)
		}
		files[dst] = strings.NewReplacer("ens1f0np0", "enp0s5", "ens1f1np1", "enp0s6").Replace(string(data))
	}

	kernel := findKernel(t)
	real, err := filepath.EvalSymlinks(kernel)
	if err != nil {
		t.Fatal(err)
	}
	addProdTplModules(t, files, productionTemplateKernelRelease(t, real))

	leaf := startProdTplLeaf(t)
	for _, tap := range []string{"tpl0", "tpl1"} {
		run(t, "create "+tap, "ip", "tuntap", "add", "dev", tap, "mode", "tap")
		run(t, "up "+tap, "ip", "link", "set", tap, "up")
		t.Cleanup(func() { _ = exec.Command("ip", "link", "del", tap).Run() })
	}

	caprf := startFakeCAPRF(t, "")
	disk := filepath.Join(t.TempDir(), "prodtpl.qcow2")
	run(t, "create disk", "qemu-img", "create", "-f", "qcow2", disk, "1G")
	vars := caprf.nativeVars("provision")
	for _, key := range []string{"STATIC_IP", "STATIC_GATEWAY", "STATIC_IFACE"} {
		delete(vars, key)
	}
	vars["FAILURE_ACTION"] = "wait"
	// The guest has no route to the fake CAPRF; log shipping would only
	// fill the console.
	delete(vars, "LOG_URL")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	console := filepath.Join(t.TempDir(), "console.log")
	args := []string{
		"-m", "1024", "-nographic", "-no-reboot",
		"-kernel", kernel, "-initrd", buildProvisionInitramfsWithFiles(t, vars, files),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-netdev", "tap,id=u0,ifname=tpl0,script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=u0,addr=0x5,mac=52:54:00:00:05:00",
		"-netdev", "tap,id=u1,ifname=tpl1,script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=u1,addr=0x6,mac=52:54:00:00:06:00",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	out, err := os.Create(console)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	qemu := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	qemu.Stdout, qemu.Stderr = out, out
	if err := qemu.Start(); err != nil {
		t.Fatalf("start QEMU: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = qemu.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(console)
			var keep []string
			for _, l := range strings.Split(string(data), "\n") {
				if !strings.Contains(l, "component=caprf") && !strings.Contains(l, "ship log") {
					keep = append(keep, l)
				}
			}
			t.Logf("console (without CAPRF retries):\n%s", tail([]byte(strings.Join(keep, "\n")), 12000))
		}
	})

	// Once QEMU holds a tap open it has carrier; then the tap moves into the
	// leaf's network namespace. Moving it earlier detaches it from QEMU.
	pid := strings.TrimSpace(runOut(t, "docker", "inspect", "-f", "{{.State.Pid}}", leaf))
	for _, tap := range []string{"tpl0", "tpl1"} {
		waitFor(t, ctx, "QEMU on "+tap, func() bool {
			carrier, err := os.ReadFile("/sys/class/net/" + tap + "/carrier")
			return err == nil && strings.TrimSpace(string(carrier)) == "1"
		})
		run(t, "move "+tap, "ip", "link", "set", tap, "netns", pid)
		run(t, "up "+tap, "docker", "exec", leaf, "ip", "link", "set", tap, "mtu", "9100", "up")
	}

	consoleText := func() string {
		data, _ := os.ReadFile(console)
		return string(data)
	}
	for _, tap := range []string{"tpl0", "tpl1"} {
		waitFor(t, ctx, "leaf session on "+tap, func() bool {
			out, err := exec.Command("docker", "exec", leaf, "vtysh", "-c", "show bgp neighbors "+tap+" json").Output()
			if err != nil {
				return false
			}
			var n map[string]struct {
				State    string `json:"bgpState"`
				RemoteAS uint32 `json:"remoteAs"`
			}
			if i := strings.Index(string(out), "{"); i < 0 || json.Unmarshal(out[i:], &n) != nil {
				return false
			}
			return n[tap].State == "Established" && n[tap].RemoteAS == 65501
		})
	}
	waitFor(t, ctx, "underlay route from both uplinks", func() bool {
		out, err := exec.Command("docker", "exec", leaf, "vtysh", "-c", "show bgp ipv4 unicast 192.0.2.17/32 json").Output()
		return err == nil && strings.Count(string(out), `"interface":"tpl`) >= 2
	})
	text := consoleText()
	for _, want := range []string{
		"from=eth0 to=enp0s5", "from=eth1 to=enp0s6",
		"using GoBGP/EVPN network mode",
		"created VXLAN and bridge",
		"installed blackhole route",
		"added lo address",
		"sending LLDP frames like networkd",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("console lacks %q. tail:\n%s", want, tail([]byte(text), 4000))
		}
	}
}

// startProdTplLeaf runs FRR without network; the taps are moved into it.
func startProdTplLeaf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "frr.conf"), []byte(prodtplLeafConf), 0o644); err != nil {
		t.Fatal(err)
	}
	daemons := "bgpd=yes\nzebra=yes\nvtysh_enable=yes\nzebra_options=\" -A 127.0.0.1 -s 90000000\"\nbgpd_options=\"   -A 127.0.0.1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "daemons"), []byte(daemons), 0o644); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("booty-kvm-leaf-%d", os.Getpid())
	run(t, "start FRR leaf", "docker", "run", "-d", "--name", name, "--network", "none", "--privileged",
		"-v", filepath.Join(dir, "frr.conf")+":/etc/frr/frr.conf:ro",
		"-v", filepath.Join(dir, "daemons")+":/etc/frr/daemons:ro",
		"quay.io/frrouting/frr:10.3.1")
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range [][]string{
				{"vtysh", "-c", "show bgp summary"},
				{"vtysh", "-c", "show bgp neighbors tpl0"},
				{"ip", "-d", "addr"},
				{"ip", "-6", "neigh"},
			} {
				out, _ := exec.Command("docker", append([]string{"exec", name}, c...)...).CombinedOutput()
				t.Logf("leaf %v:\n%s", c, out)
			}
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	return name
}

func runOut(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}

func waitFor(t *testing.T, ctx context.Context, what string, ok func() bool) {
	t.Helper()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(2 * time.Second):
		}
	}
}

// productionTemplateKernelRelease resolves the module tree for the kernel
// passed to QEMU. Ubuntu runners commonly expose /boot/vmlinuz as a symlink
// whose resolved basename is just "vmlinuz", so deriving the release only
// from that basename incorrectly selects /lib/modules/vmlinuz.
func productionTemplateKernelRelease(t *testing.T, kernel string) string {
	t.Helper()
	releaseHint := strings.TrimSpace(os.Getenv("BOOTY_KERNEL_RELEASE"))
	release, err := resolveProductionTemplateKernelRelease(kernel, "/lib/modules", releaseHint)
	if err != nil {
		t.Fatalf("resolve kernel module release for %s: %v", kernel, err)
	}
	return release
}

func resolveProductionTemplateKernelRelease(kernel, modulesRoot, releaseHint string) (string, error) {
	base := filepath.Base(kernel)
	var release = strings.TrimSpace(releaseHint)
	if release == "" && strings.HasPrefix(base, "vmlinuz-") {
		release = strings.TrimPrefix(base, "vmlinuz-")
	} else if release == "" {
		// Ubuntu's /boot/vmlinuz is commonly a symlink into
		// /usr/lib/modules/<release>/vmlinuz. This path metadata remains
		// reliable when the image itself is compressed.
		parts := strings.Split(filepath.Clean(kernel), string(filepath.Separator))
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "modules" && parts[i+1] != "" {
				release = parts[i+1]
				break
			}
		}
		if release == "" {
			data, err := os.ReadFile(kernel)
			if err != nil {
				return "", fmt.Errorf("read kernel: %w", err)
			}
			releaseMatch := regexp.MustCompile(`Linux version ([^ \x00]+)`)
			match := releaseMatch.FindSubmatch(data)
			if len(match) != 2 {
				return "", fmt.Errorf("kernel has no embedded Linux release")
			}
			release = string(match[1])
		}
	}
	if st, err := os.Stat(filepath.Join(modulesRoot, release)); err != nil || !st.IsDir() {
		return "", fmt.Errorf("module release %s is unavailable under %s", release, modulesRoot)
	}
	return release, nil
}

// addProdTplModules puts the VRF, VXLAN, bridge and dummy modules of the
// guest kernel into /modules, where BOOTy loads them like the release
// initramfs (initrd.Dockerfile). Compressed modules are decompressed.
func addProdTplModules(t *testing.T, files map[string]string, release string) {
	t.Helper()
	for _, m := range []string{"vrf", "dummy", "vxlan", "bridge"} {
		out, err := exec.Command("modprobe", "--show-depends", "-S", release, m).Output()
		if err != nil {
			t.Logf("modprobe --show-depends %s: %v (built in?)", m, err)
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "insmod" {
				continue
			}
			path := fields[1]
			var data []byte
			switch {
			case strings.HasSuffix(path, ".zst"):
				data, err = exec.Command("zstd", "-dc", path).Output()
			case strings.HasSuffix(path, ".xz"):
				data, err = exec.Command("xz", "-dc", path).Output()
			default:
				data, err = os.ReadFile(path)
			}
			if err != nil {
				t.Fatalf("read module %s: %v", path, err)
			}
			name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".zst"), ".xz")
			files["modules/"+name] = string(data)
		}
	}
}
