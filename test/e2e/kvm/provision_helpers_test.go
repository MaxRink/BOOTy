//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/test/e2e/loopdev"
)

const testEFIFallbackPayload = "BOOTy KVM e2e EFI fallback\n"

// requireProvisionTools requires essential provisioning tools.
func requireProvisionTools(t *testing.T) {
	t.Helper()
	// growpart is copied into the guest: whole-disk provisioning runs it.
	for _, tool := range []string{"sgdisk", "sfdisk", "mkfs.ext4", "mkfs.vfat", "qemu-img", "losetup", "dd", "mount", "umount", "growpart"} {
		if _, err := exec.LookPath(tool); err != nil {
			failOrSkipUnsupportedHost(t, "%s not available for KVM provisioning test", tool)
		}
	}
}

// requireDiskInspectTools requires tools needed for post-provision inspection.
func requireDiskInspectTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"qemu-nbd", "partprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			failOrSkipUnsupportedHost(t, "%s not available for KVM disk-inspection test", tool)
		}
	}
}

// createTestDiskImage creates a minimal raw disk image with a GPT partition table:
// partition 1 = 50 MiB EFI System Partition (FAT32), partition 2 = rest (ext4 root).
// The ext4 root gets a basic /etc directory. Returns path to the raw image.
func createTestDiskImage(t *testing.T, sizeMB int) string {
	t.Helper()
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "test-os.raw")

	run(t, "create raw disk image",
		"dd", "if=/dev/zero", "of="+rawPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB))

	// Create GPT partition table: 50M EFI + rest root.
	// sfdisk reads partition definitions from stdin.
	sfdiskInput := "label: gpt\nsize=50M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B\ntype=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n"
	cmd := exec.Command("sfdisk", rawPath)
	cmd.Stdin = strings.NewReader(sfdiskInput)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sfdisk failed: %v\n%s", err, out)
	}

	// Set up loop device with partition scanning.
	loopOut := runOutput(t, "setup loop device", "losetup", "--find", "--show", "--partscan", rawPath)
	loopDev := strings.TrimSpace(string(loopOut))
	t.Cleanup(func() {
		_ = exec.Command("losetup", "-d", loopDev).Run()
	})

	// Wait for partition devices to appear.
	rootDev := loopDev + "p2"
	waitForDevice(t, rootDev, 5*time.Second)

	// Format EFI partition as FAT32 (if mkfs.vfat available).
	efiDev := loopDev + "p1"
	if _, err := exec.LookPath("mkfs.vfat"); err == nil {
		if _, statErr := os.Stat(efiDev); statErr == nil {
			// udev can still hold the new partition node; retry briefly.
			var out []byte
			var err error
			for attempt := 0; attempt < 5; attempt++ {
				if out, err = exec.Command("mkfs.vfat", "-F", "32", efiDev).CombinedOutput(); err == nil {
					break
				}
				settleDevices(2)
			}
			if err != nil {
				t.Fatalf("format EFI partition: mkfs.vfat -F 32 %s: %v\n%s", efiDev, err, out)
			}
		}
	}

	// Format root partition as ext4.
	run(t, "format root partition", "mkfs.ext4", "-F", "-q", rootDev)

	// Mount root and create minimal directory structure.
	mountDir := filepath.Join(dir, "rootmnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatalf("mkdir mountDir: %v", err)
	}
	mountWithRetry(t, "mount root partition", rootDev, mountDir)
	t.Cleanup(func() {
		_ = exec.Command("umount", mountDir).Run()
	})

	// Create minimal filesystem structure expected by provisioning.
	for _, d := range []string{
		"etc", "etc/default", "etc/default/grub.d",
		"boot", "var", "tmp", "bin", "usr/bin",
	} {
		if err := os.MkdirAll(filepath.Join(mountDir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Write pre-provision hostname so we can verify it's overwritten.
	writeFile(t, filepath.Join(mountDir, "etc", "hostname"), "pre-provision\n")
	writeFile(t, filepath.Join(mountDir, "etc", "os-release"), "ID=ubuntu\nID_LIKE=debian\n")

	// Unmount and detach before compressing.
	run(t, "unmount root", "umount", mountDir)
	run(t, "detach loop", "losetup", "-d", loopDev)

	return rawPath
}

func createChrootCapableTestDiskImage(t *testing.T, sizeMB int) string {
	t.Helper()

	rawPath := createTestDiskImage(t, sizeMB)
	loopOut := runOutput(t, "setup chroot-capable loop device", "losetup", "--find", "--show", "--partscan", rawPath)
	loopDev := strings.TrimSpace(string(loopOut))
	detached := false
	defer func() {
		if !detached {
			_ = exec.Command("losetup", "-d", loopDev).Run()
		}
	}()

	rootDev := loopDev + "p2"
	waitForDevice(t, rootDev, 5*time.Second)

	mountDir := filepath.Join(t.TempDir(), "rootmnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatalf("mkdir chroot-capable mountDir: %v", err)
	}
	mounted := false
	defer func() {
		if mounted {
			_ = exec.Command("umount", mountDir).Run()
		}
	}()
	mountWithRetry(t, "mount chroot-capable root partition", rootDev, mountDir)
	mounted = true

	installMinimalChrootFixture(t, mountDir)

	run(t, "unmount chroot-capable root", "umount", mountDir)
	mounted = false
	run(t, "detach chroot-capable loop", "losetup", "-d", loopDev)
	detached = true

	return rawPath
}

func installMinimalChrootFixture(t *testing.T, root string) {
	t.Helper()

	for _, d := range []string{
		"bin", "usr/sbin", "boot/grub", "etc/default/grub.d",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir chroot fixture %s: %v", d, err)
		}
	}

	busyboxBin := findBusybox(t)
	copyBinary(t, busyboxBin, filepath.Join(root, "bin", "busybox"))
	copySharedLibs(t, busyboxBin, root)
	for _, ld := range []string{"/lib64/ld-linux-x86-64.so.2", "/lib/ld-linux-x86-64.so.2"} {
		if _, err := os.Stat(ld); err == nil {
			ldTarget, err := pathInFixtureRoot(root, ld)
			if err != nil {
				t.Fatalf("dynamic linker fixture path: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(ldTarget), 0o755); err != nil {
				t.Fatalf("mkdir dynamic linker dir: %v", err)
			}
			copyBinary(t, ld, ldTarget)
			break
		}
	}
	for _, applet := range []string{"sh", "mkdir", "cat", "printf"} {
		link := filepath.Join(root, "bin", applet)
		_ = os.Remove(link)
		if err := os.Symlink("busybox", link); err != nil {
			t.Fatalf("symlink chroot applet %s: %v", applet, err)
		}
	}
	writeFile(t, filepath.Join(root, "bin", "bash"), `#!/bin/sh
exec /bin/sh "$@"
`)
	run(t, "chmod bash fixture", "chmod", "+x", filepath.Join(root, "bin", "bash"))

	updateGrub := `#!/bin/sh
set -eu
mkdir -p /boot/grub
printf '%s\n' 'set default=0' > /boot/grub/grub.cfg
`
	updateGrubPath := filepath.Join(root, "usr", "sbin", "update-grub")
	writeFile(t, updateGrubPath, updateGrub)
	run(t, "chmod update-grub fixture", "chmod", "+x", updateGrubPath)
}

// compressGzip gzips the file at src and returns the path to the .gz file.
func compressGzip(t *testing.T, src string) string {
	t.Helper()
	dst := src + ".gz"

	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open for gzip: %v", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create gzip file: %v", err)
	}
	defer out.Close()

	// The mirror test converts a cloud image to a large raw file, mostly
	// sparse/zero-filled. Use gzip's fast level: the guest only needs a valid
	// gzip stream, while the default level can spend the whole 15-minute test
	// budget compressing unused image space on a busy runner.
	cmd := exec.Command("gzip", "-1", "-c", src)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		t.Fatalf("gzip copy: %v", err)
	}

	return dst
}

// startImageServer starts an HTTP server on a random port serving the image file.
// Returns the base URL (e.g. "http://127.0.0.1:PORT").
func startImageServer(t *testing.T, imagePath string) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/image.gz", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, imagePath)
	})

	// Listen on all interfaces so QEMU guest can reach us via 10.0.2.2.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	// QEMU user-mode networking maps the host to 10.0.2.2.
	return fmt.Sprintf("http://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)
}

// writeDeployVars creates a /deploy/vars file from a map of key=value pairs.
func writeDeployVars(t *testing.T, dir string, vars map[string]string) string {
	t.Helper()
	deployDir := filepath.Join(dir, "deploy")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		t.Fatalf("mkdir deploy: %v", err)
	}

	var b strings.Builder
	for k, v := range vars {
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, escaped)
	}
	varsPath := filepath.Join(deployDir, "vars")
	writeFile(t, varsPath, b.String())
	return varsPath
}

// buildProvisionInitramfs builds an initramfs containing BOOTy, busybox, essential
// provisioning tools (copied from host with shared libraries), and /deploy/vars.
// Returns path to the cpio.gz file.
func buildProvisionInitramfs(t *testing.T, vars map[string]string) string {
	t.Helper()
	return buildProvisionInitramfsWithFiles(t, vars, nil)
}

// buildProvisionInitramfsWithFiles is buildProvisionInitramfs plus extra
// files, keyed by their path relative to the initramfs root (for example
// "deploy/machine-commands/commands-0.sh").
func buildProvisionInitramfsWithFiles(t *testing.T, vars, files map[string]string) string {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	// KVM provision fixtures use Linux target images unless a test explicitly
	// overrides the target-OS preflight input.
	_, hasProvisionTargetOS := vars["PROVISION_TARGET_OS"]
	_, hasTargetOS := vars["TARGET_OS"]
	if strings.EqualFold(vars["MODE"], "provision") && !hasProvisionTargetOS && !hasTargetOS {
		vars["PROVISION_TARGET_OS"] = "linux"
	}

	dir := t.TempDir()
	rootDir := filepath.Join(dir, "initramfs")

	// Create directory structure.
	for _, d := range []string{
		"bin", "sbin", "dev", "proc", "sys", "etc", "tmp", "mnt",
		"usr/bin", "usr/sbin", "lib", "lib64", "modules", "deploy",
		"newroot",
	} {
		if err := os.MkdirAll(filepath.Join(rootDir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Copy BOOTy binary.
	bootyBin := envOrDefault("BOOTY_BINARY", "")
	if bootyBin == "" {
		// Build BOOTy if not provided.
		bootyBin = filepath.Join(dir, "booty")
		buildBooty(t, bootyBin)
	}
	copyBinary(t, bootyBin, filepath.Join(rootDir, "booty"))
	// The mirrored-ESP provisioner installs this helper from the ramdisk.
	// Keep the KVM fixture equivalent to the production initrd image so the
	// failover tests exercise installation and synchronization together.
	espSyncBin := filepath.Join(dir, "booty-esp-sync")
	buildESPSync(t, espSyncBin)
	if err := os.MkdirAll(filepath.Join(rootDir, "usr", "lib", "booty"), 0o755); err != nil {
		t.Fatalf("create ESP synchronizer asset directory: %v", err)
	}
	copyBinary(t, espSyncBin, filepath.Join(rootDir, "usr", "lib", "booty", "booty-esp-sync"))

	// Copy busybox.
	busyboxBin := findBusybox(t)
	copyBinary(t, busyboxBin, filepath.Join(rootDir, "bin", "busybox"))
	for _, applet := range []string{
		"sh", "mount", "umount", "ls", "cat", "echo", "sleep",
		"mkdir", "cp", "mv", "rm", "ln", "chmod", "chown",
		"mknod", "insmod", "modprobe", "setsid", "cttyhack",
		"chroot", "bash", "ash", "ip", "ifconfig", "udhcpc",
		"find", "xargs", "grep", "awk", "sed", "mdev",
		// growpart (cloud-guest-utils) is a shell script that needs these.
		"mktemp", "tr", "touch", "wc", "head", "tail", "cut", "sort",
		"uniq", "expr", "basename", "dirname", "readlink", "test", "[",
		"printf", "stat", "true", "false", "env", "getopt",
	} {
		link := filepath.Join(rootDir, "bin", applet)
		_ = os.Symlink("busybox", link)
	}
	installModuleLoader(t, rootDir)

	// Copy essential provisioning tools from host with their shared libraries.
	essentialTools := []string{
		"partprobe", "sgdisk", "sfdisk", "mkfs.ext4", "mkfs.vfat", "e2fsck", "resize2fs", "wipefs", "mdadm", "lvm",
		"losetup", "dd", "growpart", "partx", "blockdev", "flock", "lsblk", "blkdiscard", "blkid",
		// Root disk mirroring: resizepart for growpart and efibootmgr for
		// the boot entries on UEFI runs.
		"resizepart", "efibootmgr",
		// SECURE_ERASE: NVMe Format and ATA Security Erase.
		"nvme", "hdparm",
	}
	for _, tool := range essentialTools {
		toolPath, err := exec.LookPath(tool)
		if err != nil {
			t.Logf("tool %s not found, skipping", tool)
			continue
		}
		destBin := filepath.Join(rootDir, "sbin", tool)
		copyBinary(t, toolPath, destBin)
		copySharedLibs(t, toolPath, rootDir)
	}
	copyFilesystemModules(t, rootDir)
	installTestEFIFallbackAsset(t, rootDir)

	// Copy ld-linux dynamic linker if present.
	for _, ld := range []string{"/lib64/ld-linux-x86-64.so.2", "/lib/ld-linux-x86-64.so.2"} {
		if _, err := os.Stat(ld); err == nil {
			ldTarget, pathErr := pathInFixtureRoot(rootDir, ld)
			if pathErr != nil {
				t.Fatalf("dynamic linker fixture path: %v", pathErr)
			}
			destDir := filepath.Dir(ldTarget)
			if err := os.MkdirAll(destDir, 0o755); err == nil {
				copyBinary(t, ld, ldTarget)
			}
			break
		}
	}

	// Write /deploy/vars.
	writeDeployVars(t, rootDir, vars)

	for rel, content := range files {
		dest := filepath.Join(rootDir, filepath.Clean("/"+rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		writeFile(t, dest, content)
	}

	// Write /init script.
	initScript := `#!/bin/sh
export PATH=/bin:/sbin:/usr/bin:/usr/sbin
for mod in virtio_pci virtio_net virtio_blk e1000 e1000e virtio_scsi sd_mod fat vfat nls_cp437 nls_iso8859-1 nls_ascii nls_utf8 efivarfs md_mod raid1; do
	modprobe "$mod" 2>/dev/null || true
done
exec /booty
`
	writeFile(t, filepath.Join(rootDir, "init"), initScript)
	run(t, "chmod init", "chmod", "+x", filepath.Join(rootDir, "init"))

	// Create device nodes.
	for _, dev := range []struct {
		name       string
		major, min int
	}{
		{"dev/console", 5, 1},
		{"dev/ttyS0", 4, 64},
		{"dev/null", 1, 3},
	} {
		devPath := filepath.Join(rootDir, dev.name)
		cmd := exec.Command("mknod", devPath, "c",
			fmt.Sprintf("%d", dev.major), fmt.Sprintf("%d", dev.min))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("mknod %s: %v (%s)", dev.name, err, out)
		}
	}

	// Package as cpio.gz using explicit pipes instead of shell concatenation.
	cpioPath := filepath.Join(dir, "provision-initramfs.cpio.gz")

	findCmd := exec.Command("find", ".", "-print0")
	findCmd.Dir = rootDir

	cpioCmd := exec.Command("cpio", "--null", "-ov", "--format=newc")
	cpioCmd.Dir = rootDir
	cpioCmd.Stderr = nil

	gzipCmd := exec.Command("gzip")

	cpioCmd.Stdin, _ = findCmd.StdoutPipe()
	gzipCmd.Stdin, _ = cpioCmd.StdoutPipe()

	outFile, err := os.Create(cpioPath)
	if err != nil {
		t.Fatalf("create cpio.gz: %v", err)
	}
	gzipCmd.Stdout = outFile

	for _, c := range []*exec.Cmd{gzipCmd, cpioCmd, findCmd} {
		if err := c.Start(); err != nil {
			_ = outFile.Close()
			t.Fatalf("start %s: %v", c.Path, err)
		}
	}
	for _, c := range []*exec.Cmd{findCmd, cpioCmd, gzipCmd} {
		if err := c.Wait(); err != nil {
			_ = outFile.Close()
			t.Fatalf("wait %s: %v", c.Path, err)
		}
	}
	_ = outFile.Close()

	return cpioPath
}

// runQEMUProvision launches QEMU for a full provisioning run.
// Returns the serial output. QEMU exits when BOOTy reboots.
func runQEMUProvision(t *testing.T, kernel, initramfs, disk string, timeoutDur time.Duration) []byte {
	t.Helper()

	args := []string{
		"-m", "1024",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("QEMU provision timed out after %v. tail:\n%s", timeoutDur, tail(out, 2000))
	} else if err != nil {
		// Exit code from -no-reboot is expected when BOOTy calls reboot.
		t.Logf("QEMU provision exited: %v (expected on reboot)", err)
	}

	return out
}

// mountQcow2 mounts a qcow2 disk image via qemu-nbd and returns the root mount path
// and a cleanup function. The caller must defer cleanup.
func mountQcow2(t *testing.T, qcow2Path string) (rootMount string, cleanup func()) {
	t.Helper()
	return mountQcow2Partition(t, qcow2Path, 2)
}

// mountQcow2Partition mounts a qcow2 partition by 1-based partition number.
// It returns the mount path and a cleanup function. The caller must defer cleanup.
func mountQcow2Partition(t *testing.T, qcow2Path string, partNum int) (rootMount string, cleanup func()) {
	t.Helper()

	// Find an available nbd device.
	run(t, "load nbd module", "modprobe", "nbd", "max_part=8")

	nbdDev := ""
	for i := 0; i < 16; i++ {
		dev := fmt.Sprintf("/dev/nbd%d", i)
		// Check if device is free by trying to connect.
		cmd := exec.Command("qemu-nbd", "--connect="+dev, qcow2Path)
		if out, err := cmd.CombinedOutput(); err == nil {
			nbdDev = dev
			break
		} else {
			t.Logf("nbd%d busy: %s", i, out)
		}
	}
	if nbdDev == "" {
		t.Fatal("no free nbd device found")
	}

	connected := true
	disconnect := func() {
		if !connected {
			return
		}
		connected = false
		disconnectNBD(t, nbdDev)
	}
	// Register disconnect immediately so it runs even if partprobe/mount fail.
	t.Cleanup(disconnect)

	// Wait for partitions after qemu-nbd attach.
	if err := rereadPartitionTable(nbdDev); err != nil {
		disconnect()
		t.Fatalf("reread partition table on %s: %v", nbdDev, err)
	}
	partDev := fmt.Sprintf("%sp%d", nbdDev, partNum)
	waitForDevice(t, partDev, 10*time.Second)

	mountDir := t.TempDir()
	mountReadOnlyWithRetry(t, partDev, mountDir, func() {
		_ = rereadPartitionTable(nbdDev)
	})

	cleaned := false
	cleanup = func() {
		if cleaned {
			return
		}
		cleaned = true
		_ = exec.Command("umount", mountDir).Run()
		disconnect()
	}
	t.Cleanup(cleanup)

	return mountDir, cleanup
}

func disconnectNBD(t *testing.T, devPath string) {
	t.Helper()
	if out, err := exec.Command("qemu-nbd", "--disconnect", devPath).CombinedOutput(); err != nil {
		t.Logf("qemu-nbd --disconnect %s failed: %v\n%s", devPath, err, out)
	}
	if !waitForNBDDisconnect(devPath, 5*time.Second) {
		t.Fatalf("timed out waiting for %s to disconnect", devPath)
	}
}

func waitForNBDDisconnect(devPath string, timeout time.Duration) bool {
	pidPath := filepath.Join("/sys/block", filepath.Base(devPath), "pid")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err != nil || strings.TrimSpace(string(data)) == "" || strings.TrimSpace(string(data)) == "0" {
			settleDevices(2)
			return true
		}
		settleDevices(1)
		time.Sleep(100 * time.Millisecond)
	}
	settleDevices(2)
	return false
}

// --- Low-level helpers ---

func run(t *testing.T, desc string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %s %v failed: %v\n%s", desc, name, args, err, out)
	}
}

func runOutput(t *testing.T, desc string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %s %v failed: %v\n%s", desc, name, args, err, out)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// waitForDevice polls for a device node to appear, with a timeout.
func waitForDevice(t *testing.T, devPath string, timeout time.Duration) {
	t.Helper()
	if loop, part, ok := loopdev.SplitPartition(devPath); ok {
		// Also creates missing nodes and re-reads the partition table.
		if err := loopdev.WaitPartitions(loop, part, max(timeout, 30*time.Second)); err != nil {
			t.Fatal(err)
		}
		return
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		info, err := os.Stat(devPath)
		if err == nil {
			if info.Mode()&os.ModeDevice != 0 {
				return
			}
			lastErr = fmt.Errorf("%s exists but is not a device", devPath)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("device %s did not appear within %s: %v", devPath, timeout, lastErr)
}

func mountWithRetry(t *testing.T, label, devPath, mountDir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var out []byte
	var err error
	var lastStatErr error
	for time.Now().Before(deadline) {
		info, statErr := os.Stat(devPath)
		if statErr == nil && info.Mode()&os.ModeDevice != 0 {
			cmd := exec.Command("mount", devPath, mountDir)
			out, err = cmd.CombinedOutput()
			if err == nil {
				return
			}
		} else if statErr != nil {
			lastStatErr = statErr
		} else {
			lastStatErr = fmt.Errorf("%s exists but is not a device", devPath)
		}
		if _, lookErr := exec.LookPath("udevadm"); lookErr == nil {
			_ = exec.Command("udevadm", "settle", "--timeout=2").Run()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s: mount [%s %s] failed after retries: %v (last stat: %v)\n%s", label, devPath, mountDir, err, lastStatErr, out)
}

func rereadPartitionTable(devPath string) error {
	deadline := time.Now().Add(10 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		_ = exec.Command("blockdev", "--rereadpt", devPath).Run()
		cmd := exec.Command("partprobe", devPath)
		out, err := cmd.CombinedOutput()
		if err == nil {
			settleDevices(10)
			return nil
		}
		lastOut = out
		lastErr = err
		settleDevices(2)
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("partprobe %s failed after retries: %w\n%s", devPath, lastErr, lastOut)
}

func settleDevices(timeoutSeconds int) {
	if _, lookErr := exec.LookPath("udevadm"); lookErr == nil {
		_ = exec.Command("udevadm", "settle", fmt.Sprintf("--timeout=%d", timeoutSeconds)).Run()
	}
}

func mountReadOnlyWithRetry(t *testing.T, devPath, mountDir string, beforeRetry func()) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var out []byte
	var err error
	for time.Now().Before(deadline) {
		waitForDevice(t, devPath, time.Second)
		cmd := exec.Command("mount", "-o", "ro", devPath, mountDir)
		out, err = cmd.CombinedOutput()
		if err == nil {
			return
		}
		if beforeRetry != nil {
			beforeRetry()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("mount provisioned root: mount [-o ro %s %s] failed: %v\n%s", devPath, mountDir, err, out)
}

func buildESPSync(t *testing.T, output string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", output, "github.com/telekom/BOOTy/cmd/booty-esp-sync")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ESP synchronizer: %v\n%s", err, out)
	}
}

func buildBooty(t *testing.T, output string) {
	t.Helper()
	ldflags := "-linkmode external -extldflags '-static' -s -w"
	// Stamp the commit time like the release builds: the time sync uses it
	// as its floor.
	if out, err := exec.Command("git", "log", "-1", "--format=%cI").Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		ldflags += " -X github.com/telekom/BOOTy/pkg/buildinfo.commitTime=" + strings.TrimSpace(string(out))
	}
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", output, "github.com/telekom/BOOTy")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build booty: %v\n%s", err, out)
	}
}

func findBusybox(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"/usr/bin/busybox",
		"/bin/busybox",
		"/usr/local/bin/busybox",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("busybox"); err == nil {
		return p
	}
	t.Fatal("busybox not found")
	return ""
}

func copyFilesystemModules(t *testing.T, rootDir string) {
	t.Helper()
	releases := kernelModuleReleases(t)
	if len(releases) == 0 {
		t.Log("kernel module releases unavailable, not copying filesystem modules")
		return
	}
	for _, release := range releases {
		copyFilesystemModulesForRelease(t, rootDir, release)
	}
}

func copyFilesystemModulesForRelease(t *testing.T, rootDir, release string) {
	t.Helper()
	hostModuleRoot := filepath.Join("/lib/modules", release)
	if st, err := os.Stat(hostModuleRoot); err != nil || !st.IsDir() {
		t.Logf("host module tree %s unavailable, not copying filesystem modules", hostModuleRoot)
		return
	}

	copied := map[string]bool{}
	for _, module := range []string{
		"vfat", "fat", "nls_cp437", "nls_iso8859-1", "nls_ascii", "nls_utf8",
		"virtio_pci", "virtio_net", "e1000", "e1000e",
		"virtio_scsi", "sd_mod",
		"virtio_blk", "md_mod", "raid1", "efivarfs",
	} {
		copyKernelModuleWithDependencies(t, rootDir, release, module, copied, map[string]bool{})
	}
	for _, meta := range []string{
		"modules.dep",
		"modules.dep.bin",
		"modules.alias",
		"modules.alias.bin",
		"modules.symbols",
		"modules.symbols.bin",
		"modules.builtin",
		"modules.builtin.bin",
		"modules.order",
	} {
		src := filepath.Join(hostModuleRoot, meta)
		if _, err := os.Stat(src); err == nil {
			dst := filepath.Join(rootDir, "lib", "modules", release, meta)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatalf("mkdir module metadata dir: %v", err)
			}
			copyBinary(t, src, dst)
		}
	}
}

func kernelModuleReleases(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var releases []string
	add := func(release string) {
		release = strings.TrimSpace(release)
		if release == "" || seen[release] {
			return
		}
		seen[release] = true
		releases = append(releases, release)
	}

	add(kernelRelease())
	if kernel := os.Getenv("BOOTY_KERNEL"); kernel != "" {
		base := filepath.Base(kernel)
		add(strings.TrimPrefix(base, "vmlinuz-"))
	}
	entries, err := os.ReadDir("/lib/modules")
	if err != nil {
		t.Logf("read /lib/modules failed: %v", err)
		return releases
	}
	for _, entry := range entries {
		if entry.IsDir() {
			add(entry.Name())
		}
	}
	return releases
}

func installTestEFIFallbackAsset(t *testing.T, rootDir string) {
	t.Helper()
	efiDir := filepath.Join(rootDir, "usr", "lib", "booty", "efi")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatalf("mkdir efi fallback asset dir: %v", err)
	}
	writeFile(t, filepath.Join(efiDir, "BOOTX64.EFI"), testEFIFallbackPayload)
}

func installModuleLoader(t *testing.T, rootDir string) {
	t.Helper()
	for _, tool := range []string{"modprobe", "insmod"} {
		dst := filepath.Join(rootDir, "sbin", tool)
		if toolPath, err := exec.LookPath(tool); err == nil {
			copyBinary(t, toolPath, dst)
			copySharedLibs(t, toolPath, rootDir)
			continue
		}
		_ = os.Symlink(filepath.Join("..", "bin", "busybox"), dst)
	}
}

func kernelModulePath(t *testing.T, release, module string) string {
	t.Helper()
	out, err := exec.Command("modinfo", "-k", release, "-F", "filename", module).CombinedOutput()
	if err != nil {
		t.Logf("modinfo -k %s %s failed, not copying module: %v (%s)", release, module, err, strings.TrimSpace(string(out)))
		return ""
	}
	path := strings.TrimSpace(string(out))
	if path == "" || path == "(builtin)" {
		return ""
	}
	return path
}

func copyKernelModuleWithDependencies(t *testing.T, rootDir, release, module string, copied, visiting map[string]bool) {
	t.Helper()
	if visiting[module] {
		return
	}
	visiting[module] = true
	defer delete(visiting, module)

	for _, dep := range kernelModuleDependencies(t, release, module) {
		copyKernelModuleWithDependencies(t, rootDir, release, dep, copied, visiting)
	}

	path := kernelModulePath(t, release, module)
	if path == "" {
		return
	}
	copyKernelModule(t, rootDir, release, path, copied)
}

func kernelModuleDependencies(t *testing.T, release, module string) []string {
	t.Helper()
	out, err := exec.Command("modinfo", "-k", release, "-F", "depends", module).CombinedOutput()
	if err != nil {
		t.Logf("modinfo -k %s -F depends %s failed, not copying module dependencies: %v (%s)",
			release, module, err, strings.TrimSpace(string(out)))
		return nil
	}

	depends := strings.FieldsFunc(strings.TrimSpace(string(out)), func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	for i := range depends {
		depends[i] = strings.TrimSpace(depends[i])
	}
	return slices.DeleteFunc(depends, func(dep string) bool {
		return dep == ""
	})
}

func copyKernelModule(t *testing.T, rootDir, release, src string, copied map[string]bool) {
	t.Helper()
	if copied[src] {
		return
	}
	copied[src] = true
	if _, err := os.Stat(src); err != nil {
		t.Logf("kernel module %s unavailable for %s, skipping: %v", src, release, err)
		return
	}
	rel, err := filepath.Rel(filepath.Join("/lib/modules", release), src)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		t.Logf("kernel module %s is outside /lib/modules/%s, skipping", src, release)
		return
	}
	dst := filepath.Join(rootDir, "lib", "modules", release, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir kernel module dir: %v", err)
	}
	copyBinary(t, src, dst)
}

func copyBinary(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func pathInFixtureRoot(rootDir, hostPath string) (string, error) {
	cleanPath := filepath.Clean(hostPath)
	if !filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("%s is not absolute", hostPath)
	}
	relPath, err := filepath.Rel(string(os.PathSeparator), cleanPath)
	if err != nil {
		return "", fmt.Errorf("derive relative path for %s: %w", hostPath, err)
	}
	if relPath == "." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) || relPath == ".." {
		return "", fmt.Errorf("%s cannot be copied into fixture root", hostPath)
	}
	return filepath.Join(rootDir, relPath), nil
}

func TestPathInFixtureRootKeepsAbsoluteHostPathUnderRoot(t *testing.T) {
	rootDir := filepath.Join(t.TempDir(), "fixture")
	got, err := pathInFixtureRoot(rootDir, "/lib64/ld-linux-x86-64.so.2")
	if err != nil {
		t.Fatalf("pathInFixtureRoot: %v", err)
	}
	want := filepath.Join(rootDir, "lib64", "ld-linux-x86-64.so.2")
	if got != want {
		t.Fatalf("pathInFixtureRoot = %q, want %q", got, want)
	}

	if _, err := pathInFixtureRoot(rootDir, "relative/lib.so"); err == nil {
		t.Fatal("expected relative host path to be rejected")
	}
}

// copySharedLibs copies shared library dependencies of a binary into the initramfs.
func copySharedLibs(t *testing.T, binary, rootDir string) {
	t.Helper()
	cmd := exec.Command("ldd", binary)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Statically linked — no libs needed.
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Parse lines like: libext2fs.so.2 => /lib/x86_64-linux-gnu/libext2fs.so.2 (0x...)
		parts := strings.Fields(line)
		for i, p := range parts {
			if p == "=>" && i+1 < len(parts) {
				libPath := parts[i+1]
				if libPath == "" || libPath == "not" {
					continue
				}
				destPath, err := pathInFixtureRoot(rootDir, libPath)
				if err != nil {
					continue
				}
				destDir := filepath.Dir(destPath)
				if _, err := os.Stat(destPath); err == nil {
					continue // already copied
				}
				if err := os.MkdirAll(destDir, 0o755); err != nil {
					continue
				}
				// Resolve symlinks to copy the actual file.
				realPath, err := filepath.EvalSymlinks(libPath)
				if err != nil {
					continue
				}
				copyBinary(t, realPath, destPath)
			}
		}
	}
}
