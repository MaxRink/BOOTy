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

const diskLinksHostname = "disk-links"

// grubRootParam is the root= parameter of the first linux line of grub.cfg.
var grubRootParam = regexp.MustCompile(`(?m)^\s*linux\s+\S+.*\broot=(\S+)`)

// TestDiskLinksRootUUIDSurvivesDiskOrderQEMU provisions /dev/vda with an
// update-grub fixture that picks root= like grub-mkconfig's 10_linux: root=UUID=
// only when /dev/disk/by-uuid/<root UUID> exists, else the device path. The
// ramdisk has no udev, so without DISK_LINKS the result is root=/dev/vda2.
// The provisioned disk then boots behind a new blank disk, so it is vdb, and
// the verify initramfs mounts root= like initramfs-tools.
func TestDiskLinksRootUUIDSurvivesDiskOrderQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	kernel := findKernel(t)
	rawImage := createChrootCapableTestDiskImage(t, 512)
	installGrubMkconfigRootFixture(t, rawImage)
	caprf := startFakeCAPRF(t, compressGzip(t, rawImage))

	targetDisk := filepath.Join(t.TempDir(), "disk-links.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")
	vars := caprf.nativeVars("provision")
	vars["HOSTNAME"] = diskLinksHostname
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["SUCCESS_ACTION"] = "poweroff"
	out := runQEMUProvision(t, kernel, buildProvisionInitramfs(t, vars), targetDisk, 6*time.Minute)
	assertProvisionSucceeded(t, out)

	root, cleanup := mountQcow2(t, targetDisk)
	grubCfg, err := os.ReadFile(filepath.Join(root, "boot", "grub", "grub.cfg"))
	cleanup()
	if err != nil {
		t.Fatalf("read provisioned grub.cfg: %v", err)
	}
	m := grubRootParam.FindSubmatch(grubCfg)
	if m == nil {
		t.Fatalf("grub.cfg has no linux line with root=:\n%s", grubCfg)
	}
	rootParam := string(m[1])
	if !strings.HasPrefix(rootParam, "UUID=") {
		t.Fatalf("update-grub wrote root=%s, want root=UUID=: /dev/disk/by-uuid was missing in the chroot", rootParam)
	}

	// Boot the provisioned disk as the second disk.
	extraDisk := filepath.Join(t.TempDir(), "extra.qcow2")
	run(t, "create extra disk", "qemu-img", "create", "-f", "qcow2", extraDisk, "1G")
	verifyInitrd := filepath.Join(t.TempDir(), "root-verify.img")
	buildRootVerifyInitramfs(t, verifyInitrd)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	boot, _ := exec.CommandContext(ctx, "qemu-system-x86_64",
		"-m", "512", "-nographic", "-no-reboot",
		"-kernel", kernel, "-initrd", verifyInitrd,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", extraDisk),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", targetDisk),
		"-append", "console=ttyS0 panic=1 root="+rootParam,
	).CombinedOutput()
	if !strings.Contains(string(boot), "ROOT-VERIFY hostname="+diskLinksHostname) {
		t.Fatalf("root=%s did not mount after the disk order changed. tail:\n%s", rootParam, tail(boot, 3000))
	}
}

// installGrubMkconfigRootFixture installs an update-grub that decides root=
// like /etc/grub.d/10_linux, and a blkid for the root UUID (grub-probe in
// the real image).
func installGrubMkconfigRootFixture(t *testing.T, rawImage string) {
	t.Helper()
	loopDev := strings.TrimSpace(string(runOutput(t, "setup image loop device",
		"losetup", "--find", "--show", "--partscan", rawImage)))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }()
	rootDev := loopDev + "p2"
	waitForDevice(t, rootDev, 5*time.Second)
	mnt := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount image root", rootDev, mnt)
	defer func() { _ = exec.Command("umount", mnt).Run() }()

	blkid, err := exec.LookPath("blkid")
	if err != nil {
		t.Fatalf("blkid: %v", err)
	}
	copyBinary(t, blkid, filepath.Join(mnt, "usr", "sbin", "blkid"))
	copySharedLibs(t, blkid, mnt)
	for _, applet := range []string{"sed", "tail", "test", "["} {
		_ = os.Symlink("busybox", filepath.Join(mnt, "bin", applet))
	}
	writeFile(t, filepath.Join(mnt, "usr", "sbin", "update-grub"), `#!/bin/sh
set -eu
export PATH=/bin:/sbin:/usr/bin:/usr/sbin
# grub-probe --target=device /: the source of the chroot root mount.
GRUB_DEVICE=$(sed -n 's/^[^ ]* [^ ]* [^ ]* [^ ]* \/ .* - [^ ]* \([^ ]*\) .*/\1/p' /proc/self/mountinfo | tail -n 1)
GRUB_DEVICE_UUID=$(blkid -s UUID -o value "$GRUB_DEVICE")
# 10_linux: UUID only when udev made the by-uuid link.
if [ -z "$GRUB_DEVICE_UUID" ] || ! test -e "/dev/disk/by-uuid/$GRUB_DEVICE_UUID"; then
	LINUX_ROOT_DEVICE=$GRUB_DEVICE
else
	LINUX_ROOT_DEVICE=UUID=$GRUB_DEVICE_UUID
fi
mkdir -p /boot/grub
printf '%s\n' 'menuentry "Ubuntu" {' "  linux /boot/vmlinuz root=$LINUX_ROOT_DEVICE ro" '}' > /boot/grub/grub.cfg
`)
	run(t, "chmod update-grub fixture", "chmod", "+x", filepath.Join(mnt, "usr", "sbin", "update-grub"))
	run(t, "unmount image root", "umount", mnt)
}

// buildRootVerifyInitramfs resolves root= like initramfs-tools (UUID= via
// findfs), mounts it read-only, prints the host name and powers off.
func buildRootVerifyInitramfs(t *testing.T, out string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "verify")
	for _, d := range []string{"bin", "dev", "proc", "mnt"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	busybox := findBusybox(t)
	copyBinary(t, busybox, filepath.Join(dir, "bin", "busybox"))
	copySharedLibs(t, busybox, dir)
	for _, ld := range []string{"/lib64/ld-linux-x86-64.so.2", "/lib/ld-linux-x86-64.so.2"} {
		if _, err := os.Stat(ld); err == nil {
			target, err := pathInFixtureRoot(dir, ld)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			copyBinary(t, ld, target)
			break
		}
	}
	for _, applet := range []string{"sh", "mount", "cat", "echo", "sleep", "poweroff", "findfs", "sed"} {
		if err := os.Symlink("busybox", filepath.Join(dir, "bin", applet)); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "init"), `#!/bin/sh
export PATH=/bin
mount -t proc proc /proc
mount -t devtmpfs devtmpfs /dev
root=$(sed -n 's/.*root=\([^ ]*\).*/\1/p' /proc/cmdline)
i=0
while [ $i -lt 50 ]; do
	case "$root" in
	UUID=*) dev=$(findfs "$root" 2>/dev/null || true) ;;
	*) dev=$root ;;
	esac
	if [ -n "$dev" ] && mount -o ro "$dev" /mnt 2>/dev/null; then
		echo "ROOT-VERIFY root=$root dev=$dev"
		echo "ROOT-VERIFY hostname=$(cat /mnt/etc/hostname)"
		poweroff -f
	fi
	sleep 0.2
	i=$((i+1))
done
echo "ALERT! $root does not exist"
poweroff -f
`)
	run(t, "chmod verify init", "chmod", "+x", filepath.Join(dir, "init"))
	cmd := exec.Command("sh", "-c", `find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip > "$1"`, "sh", out)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pack verify initramfs: %v\n%s", err, output)
	}
}
