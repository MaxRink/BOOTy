//go:build e2e

package kvm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// createDataBeforeRootTestDiskImage builds ESP, an unnamed data partition
// and an unnamed root partition, both with the Linux filesystem type.
func createDataBeforeRootTestDiskImage(t *testing.T, sizeMB int) string {
	t.Helper()
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "data-before-root.raw")
	run(t, "create raw disk image", "dd", "if=/dev/zero", "of="+rawPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB))
	layout := "label: gpt\n" +
		"size=50M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B\n" +
		"size=32M, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n" +
		"type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n"
	cmd := exec.Command("sfdisk", rawPath)
	cmd.Stdin = strings.NewReader(layout)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk failed: %v\n%s", err, out)
	}
	loopDev := strings.TrimSpace(string(runOutput(t, "setup loop device", "losetup", "--find", "--show", "--partscan", rawPath)))
	dataMount := filepath.Join(dir, "data")
	rootMount := filepath.Join(dir, "root")
	// Unmount before detaching on every exit path, also after t.Fatal.
	defer func() {
		_ = exec.Command("umount", dataMount).Run()
		_ = exec.Command("umount", rootMount).Run()
		_ = exec.Command("losetup", "-d", loopDev).Run()
	}()
	efiDev, dataDev, rootDev := loopDev+"p1", loopDev+"p2", loopDev+"p3"
	waitForDevice(t, rootDev, 5*time.Second)
	run(t, "format EFI partition", "mkfs.vfat", "-F", "32", efiDev)
	run(t, "format data partition", "mkfs.ext4", "-F", "-q", dataDev)
	run(t, "format root partition", "mkfs.ext4", "-F", "-q", rootDev)

	if err := os.MkdirAll(dataMount, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount data partition", dataDev, dataMount)
	writeFile(t, filepath.Join(dataMount, "data-marker"), "data\n")
	run(t, "unmount data", "umount", dataMount)

	if err := os.MkdirAll(rootMount, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount root partition", rootDev, rootMount)
	for _, d := range []string{"etc/default/grub.d", "boot", "var", "tmp", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(rootMount, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(rootMount, "etc", "hostname"), "pre-provision\n")
	writeFile(t, filepath.Join(rootMount, "etc", "os-release"), "ID=ubuntu\nID_LIKE=debian\n")
	installMinimalChrootFixture(t, rootMount)
	run(t, "unmount root", "umount", rootMount)
	return rawPath
}

// Legacy evidence: provision.sh select_root_partition mounts each Linux
// filesystem partition and takes the first one with a root filesystem.
// BOOTy rejected such images as ambiguous.
func TestLegacyCompatRootPartitionProbeQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	caprf := startFakeCAPRF(t, compressGzip(t, createDataBeforeRootTestDiskImage(t, 512)))
	targetDisk := filepath.Join(t.TempDir(), "root-probe-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("root probe output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	rootMount, cleanup := mountQcow2Partition(t, targetDisk, 3)
	defer cleanup()
	if got := strings.TrimSpace(readProvisionedFile(t, rootMount, "etc/hostname")); got != "legacy-compat" {
		t.Fatalf("hostname on partition 3 = %q, want legacy-compat", got)
	}
}
