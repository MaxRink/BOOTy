//go:build e2e

package kvm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	kexecPersistHostname = "kexec-persist"
	kexecMarkerDir       = "var/lib/booty-kexec-marker"
)

// TestKexecHandoffPersistsTargetWritesQEMU provisions a disk, lets BOOTy
// kexec into a kernel from the provisioned root, and checks from the second
// kernel that the last writes before the handoff reached the disk. The
// marker directory is created by a post-provision command, so it is written
// just before kexec. Without a sync before kexec it stays in the page cache
// and is lost.
func TestKexecHandoffPersistsTargetWritesQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	kernel := findKernel(t)
	rawImage := createChrootCapableTestDiskImage(t, 512)
	installKexecVerifyBoot(t, rawImage, kernel)
	baseURL := startImageServer(t, compressGzip(t, rawImage))

	targetDisk := filepath.Join(t.TempDir(), "kexec-persist.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  kexecPersistHostname,
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"DISABLE_KEXEC":             "false",
		"POST_PROVISION_CMDS":       "mkdir -p /" + kexecMarkerDir,
	})

	// The test initramfs has no growpart. The image root already fills the
	// test disk image, so a NOCHANGE stub is enough for grow-partition.
	appendInitramfsFile(t, initramfs, "sbin/growpart", `#!/bin/sh
echo "NOCHANGE: partition $2 could only be grown by 0"
exit 1
`)

	out := string(runQEMUProvision(t, kernel, initramfs, targetDisk, 6*time.Minute))
	if !strings.Contains(out, "attempting kexec") {
		t.Fatalf("BOOTy did not attempt kexec. tail:\n%s", tail([]byte(out), 4000))
	}
	for _, want := range []string{
		"KEXEC-VERIFY hostname=" + kexecPersistHostname,
		"KEXEC-VERIFY marker=present",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("second kernel output missing %q. tail:\n%s", want, tail([]byte(out), 4000))
		}
	}
}

// installKexecVerifyBoot puts a kernel, a verify initramfs and an
// update-grub fixture that points GRUB at them into the image root.
func installKexecVerifyBoot(t *testing.T, rawImage, kernel string) {
	t.Helper()

	loopDev := strings.TrimSpace(string(runOutput(t, "setup kexec image loop device",
		"losetup", "--find", "--show", "--partscan", rawImage)))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }()
	rootDev := loopDev + "p2"
	waitForDevice(t, rootDev, 5*time.Second)

	root := filepath.Join(t.TempDir(), "rootmnt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir kexec image mount: %v", err)
	}
	mountWithRetry(t, "mount kexec image root", rootDev, root)
	defer func() { _ = exec.Command("umount", root).Run() }()

	copyBinary(t, kernel, filepath.Join(root, "boot", "vmlinuz-kexec"))
	buildKexecVerifyInitramfs(t, filepath.Join(root, "boot", "initrd-kexec-verify.img"))

	updateGrub := `#!/bin/sh
set -eu
mkdir -p /boot/grub
printf '%s\n' 'set default=0' 'menuentry "kexec-verify" {' \
  '  linux /boot/vmlinuz-kexec console=ttyS0 panic=1' \
  '  initrd /boot/initrd-kexec-verify.img' '}' > /boot/grub/grub.cfg
`
	updateGrubPath := filepath.Join(root, "usr", "sbin", "update-grub")
	writeFile(t, updateGrubPath, updateGrub)
	run(t, "chmod kexec update-grub fixture", "chmod", "+x", updateGrubPath)

	run(t, "unmount kexec image root", "umount", root)
}

// buildKexecVerifyInitramfs builds the initramfs for the second kernel. It
// mounts the provisioned root read-only, prints what it finds and powers off.
func buildKexecVerifyInitramfs(t *testing.T, out string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "verify")
	for _, d := range []string{"bin", "dev", "proc", "mnt"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatalf("mkdir verify %s: %v", d, err)
		}
	}
	busybox := findBusybox(t)
	copyBinary(t, busybox, filepath.Join(dir, "bin", "busybox"))
	copySharedLibs(t, busybox, dir)
	for _, ld := range []string{"/lib64/ld-linux-x86-64.so.2", "/lib/ld-linux-x86-64.so.2"} {
		if _, err := os.Stat(ld); err == nil {
			target, err := pathInFixtureRoot(dir, ld)
			if err != nil {
				t.Fatalf("dynamic linker fixture path: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir dynamic linker dir: %v", err)
			}
			copyBinary(t, ld, target)
			break
		}
	}
	for _, applet := range []string{"sh", "mount", "cat", "echo", "sleep", "poweroff"} {
		if err := os.Symlink("busybox", filepath.Join(dir, "bin", applet)); err != nil {
			t.Fatalf("symlink verify applet %s: %v", applet, err)
		}
	}

	initScript := `#!/bin/sh
export PATH=/bin
mount -t proc proc /proc
mount -t devtmpfs devtmpfs /dev
i=0
while [ ! -b /dev/vda2 ] && [ $i -lt 50 ]; do sleep 0.2; i=$((i+1)); done
if mount -o ro /dev/vda2 /mnt; then
	echo "KEXEC-VERIFY hostname=$(cat /mnt/etc/hostname)"
	if [ -d /mnt/` + kexecMarkerDir + ` ]; then
		echo "KEXEC-VERIFY marker=present"
	else
		echo "KEXEC-VERIFY marker=missing"
	fi
else
	echo "KEXEC-VERIFY mount failed"
fi
poweroff -f
`
	writeFile(t, filepath.Join(dir, "init"), initScript)
	run(t, "chmod verify init", "chmod", "+x", filepath.Join(dir, "init"))

	cmd := exec.Command("sh", "-c", `find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip > "$1"`, "sh", out)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pack verify initramfs: %v\n%s", err, output)
	}
}

// appendInitramfsFile appends a gzipped cpio archive with one executable
// file to an initramfs. The kernel unpacks concatenated archives in order.
func appendInitramfsFile(t *testing.T, initramfs, name, content string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // executable test fixture
		t.Fatalf("write %s: %v", name, err)
	}
	cmd := exec.Command("sh", "-c", `find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip >> "$1"`, "sh", initramfs)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("append %s to initramfs: %v\n%s", name, err, output)
	}
}
