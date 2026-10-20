//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	mirrorCheckBegin = "BOOTY-MIRROR-CHECK-BEGIN"
	mirrorCheckEnd   = "BOOTY-MIRROR-CHECK-END"
)

// mirrorCheckScript runs in the provisioned OS on every boot. It waits for the
// ESP synchronizer, writes a nested loader marker, and checks that a marker
// written during the previous boot survived onto the currently selected ESP.
// It then prints the root device, RAID state and firmware entries before
// powering the VM off.
const mirrorCheckScript = `#!/bin/sh
exec >/dev/ttyS0 2>&1
echo "` + mirrorCheckBegin + `"
for md in /dev/md[0-9]* /dev/md/*; do
	[ -b "$md" ] && mdadm --wait "$md" >/dev/null 2>&1
done
echo "ROOT_SOURCE=$(findmnt -n -o SOURCE /)"
echo "BOOT_SOURCE=$(findmnt -n -o SOURCE /boot)"
echo "ESP_PARTUUID=$(findmnt -n -o PARTUUID /boot/efi)"
for i in $(seq 1 60); do
  [ -f /run/booty/esp-sync.ready ] && break
  sleep 1
done
echo "ESP_SYNC_READY=$(test -f /run/booty/esp-sync.ready && echo yes || echo no)"
state=/var/lib/booty/mirror-check-boot-count
count=0
[ -r "$state" ] && count=$(cat "$state")
esp_marker=/boot/efi/EFI/BOOTY/mirror-loader-marker
if [ "$count" -gt 0 ]; then
  echo "ESP_PREVIOUS_MARKER=$(test -f "$esp_marker" && echo yes || echo no)"
fi
mkdir -p "$(dirname "$esp_marker")"
printf '%s\n' "$(findmnt -n -o PARTUUID /boot/efi)" > "$esp_marker"
mkdir -p "$(dirname "$state")"
printf '%s\n' $((count + 1)) > "$state"
sleep 5
echo "ESP_NESTED_MARKER=$(test -f "$esp_marker" && echo yes || echo no)"
cat /proc/mdstat
efibootmgr -v
echo "` + mirrorCheckEnd + `"
systemctl poweroff --no-block
`

// The check writes to ttyS0, so the serial getty waits for it: agetty hangs
// up the tty, and the check output after that is lost (seen on Ubuntu 26.04).
const mirrorCheckUnit = `[Unit]
Description=BOOTy mirrored root e2e check
After=local-fs.target
Before=serial-getty@ttyS0.service

[Service]
Type=oneshot
ExecStart=/bin/sh /usr/local/lib/booty-mirror-check.sh

[Install]
WantedBy=multi-user.target
`

// TestMirroredRootBootFailoverVM installs a real Ubuntu cloud image with
// DISK_MIRRORING_MODE=raid-1 onto two disks, boots the installed system from
// both disks, then boots it again with the first disk removed. The second
// boot must come up degraded from the second disk, under UEFI through the
// second disk's firmware boot entry and under BIOS through its own GRUB.
func TestMirroredRootBootFailoverVM(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	osImage := os.Getenv("MIRROR_OS_IMAGE")
	if osImage == "" {
		failOrSkipUnsupportedHost(t, "MIRROR_OS_IMAGE not set: path to an Ubuntu cloud image (qcow2)")
	}
	ovmfCode := envOrDefault("OVMF_CODE", findOVMF("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_CODE.fd"))
	ovmfVarsTemplate := envOrDefault("OVMF_VARS", findOVMF("/usr/share/OVMF/OVMF_VARS_4M.fd", "/usr/share/OVMF/OVMF_VARS.fd"))
	for _, f := range []string{osImage, ovmfCode, ovmfVarsTemplate} {
		if _, err := os.Stat(f); err != nil {
			failOrSkipUnsupportedHost(t, "mirror boot asset missing: %v", err)
		}
	}

	dir := t.TempDir()
	rawImage := filepath.Join(dir, "os.raw")
	run(t, "convert OS image to raw", "qemu-img", "convert", "-O", "raw", osImage, rawImage)
	gzImage := compressGzip(t, rawImage)
	if err := os.Remove(rawImage); err != nil {
		t.Fatalf("remove raw image: %v", err)
	}
	baseURL := startImageServer(t, gzImage)

	vars := map[string]string{
		"HOSTNAME":                  "mirror-e2e",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"MIN_DISK_SIZE_GB":          "1",
		"DISK_MIRRORING_MODE":       "raid-1",
		"DISABLE_KEXEC":             "true",
	}
	if n := os.Getenv("MIRROR_ROOT_PARTITION_NUMBER"); n != "" {
		vars["ROOT_PARTITION_NUMBER"] = n
	}
	initramfs := buildProvisionInitramfsWithFiles(t, vars, map[string]string{
		"deploy/file-system/usr/local/lib/booty-mirror-check.sh":           mirrorCheckScript,
		"deploy/file-system/etc/systemd/system/booty-mirror-check.service": mirrorCheckUnit,
		"deploy/machine-commands/enable-mirror-check":                      "systemctl enable booty-mirror-check.service",
	})
	kernel := findKernel(t)

	t.Run("uefi", func(t *testing.T) {
		ovmfVars := filepath.Join(t.TempDir(), "OVMF_VARS.fd")
		copyBinary(t, ovmfVarsTemplate, ovmfVars)
		report := runMirrorFailover(t, kernel, initramfs, func(disks ...string) []string {
			return mirrorQEMUBaseArgs([]string{
				"-drive", "if=pflash,format=raw,readonly=on,file=" + ovmfCode,
				"-drive", "if=pflash,format=raw,file=" + ovmfVars,
			}, disks...)
		})
		requireBootCurrentOnESP(t, report)
	})

	// BIOS: SeaBIOS boots the first disk, so booting the second disk alone
	// needs the GRUB that BOOTy installed into it.
	t.Run("bios", func(t *testing.T) {
		runMirrorFailover(t, kernel, initramfs, func(disks ...string) []string {
			return mirrorQEMUBaseArgs(nil, disks...)
		})
	})
}

// runMirrorFailover provisions two new disks, boots with both and then with
// only the second disk. It returns the check report of the second boot.
func runMirrorFailover(t *testing.T, kernel, initramfs string, qemuArgs func(disks ...string) []string) string {
	t.Helper()
	dir := t.TempDir()
	disk1 := filepath.Join(dir, "disk1.qcow2")
	disk2 := filepath.Join(dir, "disk2.qcow2")
	for _, d := range []string{disk1, disk2} {
		run(t, "create mirror disk", "qemu-img", "create", "-f", "qcow2", d, envOrDefault("MIRROR_DISK_SIZE", "4G"))
	}

	provisionArgs := append(qemuArgs(disk1, disk2),
		"-kernel", kernel,
		"-initrd", initramfs,
		"-append", runnerKernelCmdline+" net.ifnames=0",
		"-net", "nic,model=virtio",
		"-net", "user",
	)
	out := runMirrorQEMU(t, "provision", provisionArgs, 8*time.Minute)
	if !strings.Contains(string(out), "step=report-success") || strings.Contains(string(out), "provisioning step failed") {
		t.Fatal("mirrored provisioning did not succeed, see the provision output above")
	}

	// Boot 1: both disks. The check waits for the resync and writes a nested
	// loader marker, so the second member is in sync before the first disk is
	// removed.
	report := mirrorCheckReport(t, "boot with both disks",
		runMirrorQEMU(t, "boot with both disks", qemuArgs(disk1, disk2), 6*time.Minute))
	requireArraysOnMD(t, report)
	if got := strings.Count(report, "[2/2] [UU]"); got != mirrorArrayCount() {
		t.Fatalf("%d clean two-member mirrors, want %d:\n%s", got, mirrorArrayCount(), report)
	}

	// Boot 2: only the second disk. The second disk must boot on its own, the
	// initramfs must start the array degraded, and the nested marker written to
	// disk one must be present on disk two.
	report = mirrorCheckReport(t, "boot from second disk only",
		runMirrorQEMU(t, "boot from second disk only", qemuArgs(disk2), 6*time.Minute))
	requireArraysOnMD(t, report)
	if got := strings.Count(report, "[2/1]"); got != mirrorArrayCount() {
		t.Fatalf("%d arrays degraded to one member, want %d:\n%s", got, mirrorArrayCount(), report)
	}
	if !strings.Contains(report, "ESP_PREVIOUS_MARKER=yes") {
		t.Fatalf("second ESP did not contain the nested marker from the first boot:\n%s", report)
	}

	// Boot 3: only the first disk. This exercises the reverse direction: the
	// marker written while disk two was the mounted source must return to disk
	// one before this boot.
	report = mirrorCheckReport(t, "boot from first disk after reverse sync",
		runMirrorQEMU(t, "boot from first disk after reverse sync", qemuArgs(disk1), 6*time.Minute))
	requireArraysOnMD(t, report)
	if got := strings.Count(report, "[2/1]"); got != mirrorArrayCount() {
		t.Fatalf("%d arrays degraded to one member after reverse failover, want %d:\n%s", got, mirrorArrayCount(), report)
	}
	if !strings.Contains(report, "ESP_PREVIOUS_MARKER=yes") {
		t.Fatalf("first ESP did not contain the nested marker from the reverse boot:\n%s", report)
	}
	return report
}

// mirrorQEMUBaseArgs returns QEMU arguments with the firmware arguments and
// the given disks. No firmware arguments means SeaBIOS.
func mirrorQEMUBaseArgs(firmware []string, disks ...string) []string {
	args := []string{
		"-machine", "q35,accel=kvm:tcg",
		"-cpu", mirrorQEMUCPUModel(),
		"-m", "2048",
		"-smp", "2",
		"-nographic",
		"-no-reboot",
	}
	args = append(args, firmware...)
	for _, d := range disks {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", d))
	}
	return append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
}

// mirrorQEMUCPUModel avoids asking KVM to emulate the synthetic "max" CPU.
// TCG still uses "max" when /dev/kvm is unavailable, preserving portability
// on developer hosts and non-KVM CI runners.
func mirrorQEMUCPUModel() string {
	if _, err := os.Stat("/dev/kvm"); err == nil {
		return "host"
	}
	return "max"
}

func runMirrorQEMU(t *testing.T, phase string, args []string, timeout time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("QEMU %s timed out after %s. output:\n%s", phase, timeout, readableSerial(out))
	}
	t.Logf("QEMU %s exited (%v). output:\n%s", phase, err, readableSerial(out))
	return out
}

// mirrorCheckReport returns the check script output from a boot.
func mirrorCheckReport(t *testing.T, phase string, out []byte) string {
	t.Helper()
	s := strings.ReplaceAll(string(out), "\r", "")
	begin := strings.LastIndex(s, mirrorCheckBegin)
	if begin < 0 {
		t.Fatalf("%s: the installed OS did not run the mirror check. tail:\n%s", phase, tail(out, 6000))
	}
	end := strings.Index(s[begin:], mirrorCheckEnd)
	if end < 0 {
		t.Fatalf("%s: mirror check output is incomplete:\n%s", phase, s[begin:])
	}
	report := s[begin : begin+end]
	t.Logf("%s report:\n%s", phase, report)
	return report
}

// mirrorArrayCount is the number of md arrays the image gets: root, plus
// /boot when MIRROR_SEPARATE_BOOT is set for an image with a separate /boot.
func mirrorArrayCount() int {
	if os.Getenv("MIRROR_SEPARATE_BOOT") == "true" {
		return 2
	}
	return 1
}

// requireArraysOnMD checks that / and, for a separate /boot, /boot are
// mounted from md arrays.
func requireArraysOnMD(t *testing.T, report string) {
	t.Helper()
	sources := map[string]bool{"ROOT_SOURCE": true, "BOOT_SOURCE": mirrorArrayCount() == 2}
	for key, required := range sources {
		if !required {
			continue
		}
		m := regexp.MustCompile(key + `=(\S+)`).FindStringSubmatch(report)
		if m == nil || !strings.HasPrefix(m[1], "/dev/md") {
			t.Fatalf("%s is not an md array:\n%s", key, report)
		}
	}
}

// requireBootCurrentOnESP checks that firmware booted the entry whose device
// path names the mounted ESP, which is the second disk's ESP in this boot.
func requireBootCurrentOnESP(t *testing.T, report string) {
	t.Helper()
	esp := regexp.MustCompile(`ESP_PARTUUID=(\S+)`).FindStringSubmatch(report)
	current := regexp.MustCompile(`BootCurrent: ([0-9A-Fa-f]{4})`).FindStringSubmatch(report)
	if esp == nil || current == nil {
		t.Fatalf("ESP PARTUUID or BootCurrent missing:\n%s", report)
	}
	entry := regexp.MustCompile(`(?m)^Boot` + current[1] + `\*? .*$`).FindString(report)
	if !strings.Contains(strings.ToLower(entry), strings.ToLower(esp[1])) {
		t.Fatalf("firmware booted Boot%s %q, not the entry for the second disk ESP %s:\n%s",
			current[1], entry, esp[1], report)
	}
}

// readableSerial drops ANSI escapes and systemd progress spinner lines from
// serial output so the full log stays readable in the test output.
func readableSerial(out []byte) string {
	ansi := regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	var b strings.Builder
	for _, line := range strings.Split(ansi.ReplaceAllString(string(out), ""), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.Contains(line, "A start job is running") || strings.TrimSpace(line) == "M" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
