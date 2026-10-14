//go:build e2e

package kvm

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file prove that CAPRF_LEGACY_COMPAT=true makes BOOTy
// behave like the CAPRF legacy shell provisioner
// (cluster-api-provider-redfish internal/provision/files/*.sh).

const fakeCAPRFToken = "legacy-compat-token"

// fakeCAPRF is a minimal CAPRF provision server. It serves the machine image
// and records the status callbacks that the legacy scripts send.
type fakeCAPRF struct {
	baseURL string

	mu       sync.Mutex
	statuses []string
	messages map[string]string
}

func startFakeCAPRF(t *testing.T, imagePath string) *fakeCAPRF {
	t.Helper()
	f := &fakeCAPRF{messages: map[string]string{}}

	// Listen on all interfaces so the QEMU guest reaches us via 10.0.2.2.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: f.handler(imagePath), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	f.baseURL = fmt.Sprintf("http://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)
	return f
}

func (f *fakeCAPRF) handler(imagePath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/image.gz", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, imagePath)
	})
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeCAPRFToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost {
			status := strings.TrimPrefix(r.URL.Path, "/status/")
			f.mu.Lock()
			f.statuses = append(f.statuses, status)
			f.messages[status] = string(body)
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// vars returns the /deploy/vars entries that the CAPRF ramdisk builder writes
// for every task, plus the legacy compat switch.
func (f *fakeCAPRF) vars(mode string) map[string]string {
	return map[string]string{
		"HOSTNAME":            "legacy-compat",
		"TOKEN":               fakeCAPRFToken,
		"MODE":                mode,
		"LOG_URL":             f.baseURL + "/log",
		"INIT_URL":            f.baseURL + "/status/init",
		"ERROR_URL":           f.baseURL + "/status/error",
		"SUCCESS_URL":         f.baseURL + "/status/success",
		"INSECURE_TRANSPORT":  "true",
		"CAPRF_LEGACY_COMPAT": "true",
		"STATIC_IP":           "10.0.2.15/24",
		"STATIC_GATEWAY":      "10.0.2.2",
		"STATIC_IFACE":        "eth0",
	}
}

func (f *fakeCAPRF) requireStatuses(t *testing.T, want ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.statuses, ",") != strings.Join(want, ",") {
		t.Fatalf("CAPRF status callbacks = %v, want %v (messages %v)", f.statuses, want, f.messages)
	}
}

// requirePowerOff checks the kernel message that the legacy scripts cause with
// `shutdown now` and `poweroff -f`. A reboot prints "Restarting system".
func requirePowerOff(t *testing.T, output []byte) {
	t.Helper()
	text := string(output)
	if !strings.Contains(text, "reboot: Power down") {
		t.Fatalf("guest did not power off. tail:\n%s", tail(output, 4000))
	}
	if strings.Contains(text, "reboot: Restarting system") {
		t.Fatalf("guest rebooted instead of powering off. tail:\n%s", tail(output, 4000))
	}
}

// Legacy evidence: provision.sh ends with status_success and `shutdown now`.
// CAPRF then waits for power off, ejects the ISO and powers the host on.
func TestLegacyCompatProvisionPowersOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "legacy-compat-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("legacy compat provision output tail:\n%s", tail(output, 3000))

	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	// Legacy provision.sh logs the firmware of every NIC.
	nicLine := false
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "nic_firmware") && strings.Contains(line, "nic=eth0") && strings.Contains(line, "version=") {
			nicLine = true
			break
		}
	}
	if !nicLine {
		t.Fatalf("legacy compat did not log the NIC firmware of eth0. tail:\n%s", tail(output, 4000))
	}
	requirePowerOff(t, output)
	// Legacy provision.sh runs `hostname "$HOSTNAME"` first.
	if !strings.Contains(string(output), "ramdisk hostname set") {
		t.Fatalf("legacy compat did not set the ramdisk hostname. tail:\n%s", tail(output, 4000))
	}
	if strings.Contains(string(output), "attempting kexec") {
		t.Fatalf("legacy compat must not kexec into the installed kernel. tail:\n%s", tail(output, 4000))
	}
}

// Legacy evidence: deprovision.sh ends with status_success, `sync` and
// `poweroff -f`.
func TestLegacyCompatDeprovisionPowersOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caprf := startFakeCAPRF(t, "")

	targetDisk := filepath.Join(t.TempDir(), "legacy-compat-deprovision.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	initramfs := buildProvisionInitramfs(t, caprf.vars("deprovision"))

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 4*time.Minute)
	t.Logf("legacy compat deprovision output tail:\n%s", tail(output, 3000))

	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, output)
}

// Legacy evidence: deprovision.sh decommission_wipe wipes all disks, removes
// all EFI boot entries, reports success and powers off. CAPRF writes
// MODE=decommission for hosts with the force-clean annotation.
func TestLegacyCompatDecommissionWipesAndPowersOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	if _, err := exec.LookPath("wipefs"); err != nil {
		failOrSkipUnsupportedHost(t, "wipefs not available for the decommission test")
	}

	caprf := startFakeCAPRF(t, "")

	// Start from a disk that carries a GPT and filesystems.
	rawDisk := createTestDiskImage(t, 256)
	targetDisk := filepath.Join(t.TempDir(), "legacy-compat-decommission.qcow2")
	run(t, "convert disk to qcow2", "qemu-img", "convert", "-O", "qcow2", rawDisk, targetDisk)

	initramfs := buildProvisionInitramfs(t, caprf.vars("decommission"))

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 4*time.Minute)
	t.Logf("legacy compat decommission output tail:\n%s", tail(output, 3000))

	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, output)

	wiped := filepath.Join(t.TempDir(), "decommissioned.raw")
	run(t, "convert decommissioned disk to raw", "qemu-img", "convert", "-O", "raw", targetDisk, wiped)
	signatures := strings.TrimSpace(string(runOutput(t, "list disk signatures", "wipefs", "--noheadings", wiped)))
	if signatures != "" {
		t.Fatalf("decommission left disk signatures:\n%s", signatures)
	}
}

// Legacy evidence: provision.sh grows the last GPT Linux partition with
// growpart and resize2fs, also when it is a data partition after the root.
func TestLegacyCompatGrowsLastLinuxPartitionQEMU(t *testing.T) {
	testGrowsLastLinuxPartition(t, func(f *fakeCAPRF) map[string]string { return f.vars("provision") })
}

// GROW_PARTITION=last-linux grows the same partition without
// CAPRF_LEGACY_COMPAT. The default grows the root, so the size checks fail
// when BOOTy ignores the option.
func TestNativeParityGrowPartitionLastLinuxQEMU(t *testing.T) {
	testGrowsLastLinuxPartition(t, func(f *fakeCAPRF) map[string]string {
		vars := f.nativeVars("provision")
		vars["GROW_PARTITION"] = "last-linux"
		vars["SUCCESS_ACTION"] = "poweroff"
		return vars
	})
}

func testGrowsLastLinuxPartition(t *testing.T, varsFor func(*fakeCAPRF) map[string]string) {
	t.Helper()
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	// Append a 64 MiB ext4 data partition after the root.
	run(t, "extend image", "truncate", "-s", "+64M", rawDisk)
	run(t, "relocate backup GPT", "sfdisk", "--relocate", "gpt-bak-std", rawDisk)
	appendPart := exec.Command("sfdisk", "--append", rawDisk)
	appendPart.Stdin = strings.NewReader("type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n")
	if out, err := appendPart.CombinedOutput(); err != nil {
		t.Fatalf("append data partition: %v\n%s", err, out)
	}
	loopDev := strings.TrimSpace(string(runOutput(t, "attach image", "losetup", "--find", "--show", "--partscan", rawDisk)))
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loopDev).Run() })
	waitForDevice(t, loopDev+"p3", 5*time.Second)
	run(t, "format data partition", "mkfs.ext4", "-F", "-q", loopDev+"p3")
	run(t, "detach image", "losetup", "-d", loopDev)

	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))
	targetDisk := filepath.Join(t.TempDir(), "legacy-compat-grow.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := varsFor(caprf)
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 6*time.Minute)
	t.Logf("grow last Linux partition output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	rawTarget := filepath.Join(t.TempDir(), "target.raw")
	run(t, "convert target disk", "qemu-img", "convert", "-O", "raw", targetDisk, rawTarget)
	table := string(runOutput(t, "read target partitions", "sfdisk", "--dump", rawTarget))
	sizes := map[string]int64{}
	for _, line := range strings.Split(table, "\n") {
		node, rest, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		for _, field := range strings.Split(rest, ",") {
			if v, found := strings.CutPrefix(strings.TrimSpace(field), "size="); found {
				sectors, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				if err != nil {
					t.Fatalf("parse partition size %q: %v", v, err)
				}
				sizes[node[len(node)-1:]] = sectors * 512
			}
		}
	}
	if sizes["2"] > 500<<20 {
		t.Fatalf("root partition grew to %d bytes, want the original size. table:\n%s", sizes["2"], table)
	}
	if sizes["3"] < 1<<30 {
		t.Fatalf("last Linux partition has %d bytes, want it grown to the disk end. table:\n%s", sizes["3"], table)
	}
}
