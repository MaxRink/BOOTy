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

// createSeparateBootTestDiskImage builds an image like the Ubuntu 24.04
// cloud image: ESP, a separate /boot (XBOOTLDR, label BOOT) and the root
// filesystem, whose fstab mounts /boot by label. The root filesystem has no
// /boot/grub.
func createSeparateBootTestDiskImage(t *testing.T, sizeMB int) string {
	t.Helper()
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "separate-boot.raw")
	run(t, "create raw disk image", "dd", "if=/dev/zero", "of="+rawPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB))

	layout := "label: gpt\n" +
		"size=50M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B\n" +
		"size=64M, type=BC13C2FF-59E6-4262-A352-B275FD6F7172\n" +
		"type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n"
	cmd := exec.Command("sfdisk", rawPath)
	cmd.Stdin = strings.NewReader(layout)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk failed: %v\n%s", err, out)
	}

	loopDev := strings.TrimSpace(string(runOutput(t, "setup loop device", "losetup", "--find", "--show", "--partscan", rawPath)))
	rootMount := filepath.Join(dir, "root")
	bootMount := filepath.Join(dir, "boot")
	// Unmount before detaching on every exit path, also after t.Fatal.
	defer func() {
		_ = exec.Command("umount", rootMount).Run()
		_ = exec.Command("umount", bootMount).Run()
		_ = exec.Command("losetup", "-d", loopDev).Run()
	}()
	efiDev, bootDev, rootDev := loopDev+"p1", loopDev+"p2", loopDev+"p3"
	waitForDevice(t, rootDev, 5*time.Second)

	run(t, "format EFI partition", "mkfs.vfat", "-F", "32", efiDev)
	run(t, "format boot partition", "mkfs.ext4", "-F", "-q", "-L", "BOOT", bootDev)
	run(t, "format root partition", "mkfs.ext4", "-F", "-q", rootDev)

	if err := os.MkdirAll(rootMount, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount root partition", rootDev, rootMount)
	for _, d := range []string{"etc/default/grub.d", "var", "tmp", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(rootMount, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(rootMount, "etc", "hostname"), "pre-provision\n")
	writeFile(t, filepath.Join(rootMount, "etc", "os-release"), "ID=ubuntu\nID_LIKE=debian\n")
	writeFile(t, filepath.Join(rootMount, "etc", "fstab"),
		"LABEL=cloudimg-rootfs / ext4 discard 0 1\nLABEL=BOOT /boot ext4 defaults 0 2\n")
	installMinimalChrootFixture(t, rootMount)
	// The fixture creates /boot/grub on the root filesystem. This image keeps
	// it on the separate /boot partition instead.
	if err := os.RemoveAll(filepath.Join(rootMount, "boot", "grub")); err != nil {
		t.Fatal(err)
	}
	run(t, "unmount root", "umount", rootMount)

	if err := os.MkdirAll(bootMount, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount boot partition", bootDev, bootMount)
	if err := os.MkdirAll(filepath.Join(bootMount, "grub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(bootMount, "grub", "grub.cfg"), "# image grub.cfg\n")
	run(t, "unmount boot", "umount", bootMount)
	return rawPath
}

// Legacy evidence: provision.sh:366-395 mounts the image /boot from its
// fstab when the root filesystem has no /boot/grub, so update-grub writes
// the grub.cfg that the firmware actually boots.
func TestLegacyCompatSeparateBootPartitionGetsGRUBConfigQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)
	if _, err := exec.LookPath("blkid"); err != nil {
		failOrSkipUnsupportedHost(t, "blkid not available for the separate /boot test")
	}

	rawDisk := createSeparateBootTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "separate-boot-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("separate /boot output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	bootMount, cleanupBoot := mountQcow2Partition(t, targetDisk, 2)
	grubCfg, err := os.ReadFile(filepath.Join(bootMount, "grub", "grub.cfg"))
	cleanupBoot()
	if err != nil {
		t.Fatalf("read grub.cfg on the /boot partition: %v", err)
	}
	if !strings.Contains(string(grubCfg), "set default=0") {
		t.Fatalf("update-grub did not write the /boot partition grub.cfg:\n%s", grubCfg)
	}

	rootMount, cleanupRoot := mountQcow2Partition(t, targetDisk, 3)
	defer cleanupRoot()
	if _, err := os.Stat(filepath.Join(rootMount, "boot", "grub", "grub.cfg")); err == nil {
		t.Fatal("update-grub wrote grub.cfg into /boot of the root filesystem")
	}
}
