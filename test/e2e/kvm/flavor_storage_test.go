//go:build e2e

package kvm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestFlavorInitrdStorageControllersQEMU boots a release initramfs flavor
// (initrd.Dockerfile) with the Debian kernel it ships modules for. The
// target disk is NVMe and a second disk sits on an AHCI controller; there is
// no virtio disk, and the firmware is OVMF. The Debian kernel builds nvme,
// ahci, md and efivarfs as modules, so without them in the initramfs BOOTy
// finds no disk. The run also needs working busybox applets (blockdev, sync).
//
// BOOTY_FLAVOR_INITRAMFS: initramfs.cpio.zst of a flavor build.
// BOOTY_FLAVOR_KERNEL: vmlinuz of `--target kernel-image`.
func TestFlavorInitrdStorageControllersQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	flavor := os.Getenv("BOOTY_FLAVOR_INITRAMFS")
	kernel := os.Getenv("BOOTY_FLAVOR_KERNEL")
	if flavor == "" || kernel == "" {
		failOrSkipUnsupportedHost(t, "BOOTY_FLAVOR_INITRAMFS and BOOTY_FLAVOR_KERNEL are required")
	}
	requireKVMAssets(t, flavor, kernel)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	// create-efi-boot-entry registers \EFI\ubuntu\shimx64.efi.
	writeImageESPFile(t, rawDisk, "EFI/ubuntu/shimx64.efi", "BOOTy KVM e2e shim\n")
	installRAIDCheck(t, rawDisk)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	dir := t.TempDir()
	nvmeDisk := filepath.Join(dir, "nvme.raw")
	sataDisk := filepath.Join(dir, "sata.raw")
	run(t, "create NVMe disk", "truncate", "-s", "2G", nvmeDisk)
	run(t, "create SATA disk", "truncate", "-s", "1G", sataDisk)
	// A partition table on the SATA disk proves the wipe of every disk reached it.
	sgdisk := exec.Command("sgdisk", "-n", "1:0:+64M", sataDisk)
	if out, err := sgdisk.CombinedOutput(); err != nil {
		t.Fatalf("partition SATA disk: %v\n%s", err, out)
	}

	vars := caprf.nativeVars("provision")
	vars["PROVISION_TARGET_OS"] = "linux"
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/nvme0n1"
	vars["WIPE_ALL_DISKS"] = "true"
	vars["SUCCESS_ACTION"] = "poweroff"
	// The machine command creates a RAID 1 and a RAID 5 with mdadm and the
	// md modules of the ramdisk kernel.
	initramfs := flavorInitramfsWithVars(t, flavor, vars, map[string]string{
		"deploy/machine-commands/commands-0.sh": "/bin/sh /usr/local/sbin/booty-raid-check.sh",
	})

	// UEFI, so efivarfs loads (it fails with ENODEV on BIOS).
	ovmfCode := envOrDefault("OVMF_CODE", findOVMF("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_CODE.fd"))
	ovmfVars := filepath.Join(dir, "OVMF_VARS.fd")
	copyFile(t, findOVMF("/usr/share/OVMF/OVMF_VARS_4M.fd", "/usr/share/OVMF/OVMF_VARS.fd"), ovmfVars)

	args := []string{
		"-m", "2048", "-nographic", "-no-reboot",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + ovmfCode,
		"-drive", "if=pflash,format=raw,file=" + ovmfVars,
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", "file=" + nvmeDisk + ",format=raw,if=none,id=nv0",
		"-device", "nvme,serial=booty-nvme,drive=nv0",
		"-device", "ahci,id=ahci0",
		"-drive", "file=" + sataDisk + ",format=raw,if=none,id=sata0",
		"-device", "ide-hd,drive=sata0,bus=ahci0.0",
		"-device", "megasas-gen2,id=sas0",
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	// A disk behind the MegaRAID SAS HBA. QEMU's megasas emulation does not
	// serve the I/O of the current megaraid_sas driver (Read(6) is
	// rejected), so the test only checks that the driver finds the disk.
	sasDisk := filepath.Join(dir, "sas.raw")
	run(t, "create SAS disk", "truncate", "-s", "256M", sasDisk)
	args = append(args,
		"-drive", "file="+sasDisk+",format=raw,if=none,id=sas0d",
		"-device", "scsi-hd,drive=sas0d,bus=sas0.0")
	// RAID members (virtio): two 64 MiB disks (RAID 1), three 96 MiB disks (RAID 5).
	for i, size := range []string{"64M", "64M", "96M", "96M", "96M"} {
		member := filepath.Join(dir, fmt.Sprintf("raid%d.raw", i))
		run(t, "create RAID member", "truncate", "-s", size, member)
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", member))
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("flavor QEMU run timed out. tail:\n%s", tail(out, 4000))
	}
	t.Logf("flavor QEMU run exited (%v). tail:\n%s", err, tail(out, 3000))

	// BOOTy logs one line for all modules: msg="loaded kernel modules" ... modules="a.ko b.ko".
	m := regexp.MustCompile(`msg="loaded kernel modules" .*modules="([^"]*)"`).FindSubmatch(out)
	if m == nil {
		t.Fatal("no loaded kernel modules line in the console log")
	}
	loaded := strings.Fields(string(m[1]))
	for _, ko := range []string{"nvme.ko", "ahci.ko", "megaraid_sas.ko", "md-mod.ko", "raid1.ko", "raid456.ko", "efivarfs.ko"} {
		if !slices.Contains(loaded, ko) {
			t.Errorf("kernel module %s was not loaded (loaded: %v)", ko, loaded)
		}
	}
	assertProvisionSucceeded(t, out)
	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, out)

	// The image went to the NVMe disk.
	if table := runOutput(t, "read NVMe partition table", "sfdisk", "-d", nvmeDisk); !bytes.Contains(table, []byte("C12A7328-F81F-11D2-BA4B-00A0C93EC93B")) {
		t.Fatalf("NVMe disk has no image partition table:\n%s", table)
	}
	// The AHCI disk was wiped by WIPE_ALL_DISKS.
	if out, err := exec.Command("sfdisk", "-d", sataDisk).CombinedOutput(); err == nil {
		t.Fatalf("SATA disk still has a partition table after the wipe of every disk:\n%s", out)
	}
	// megaraid_sas attached the 256 MiB SAS disk.
	if !regexp.MustCompile(`\[sd[a-z]+\] 524288 512-byte logical blocks`).Match(out) {
		t.Fatalf("megaraid_sas did not attach the SAS disk. tail:\n%s", tail(out, 4000))
	}
}

// flavorInitramfsWithVars appends /deploy/vars and the deploy files to a
// flavor initramfs. The
// kernel unpacks concatenated archives in order, each with its own
// compression.
func flavorInitramfsWithVars(t *testing.T, flavor string, vars, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeDeployVars(t, dir, vars)
	for rel, content := range files {
		path := filepath.Join(dir, filepath.Clean("/"+rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, content)
	}
	out := filepath.Join(t.TempDir(), "flavor-with-vars.cpio")
	copyFile(t, flavor, out)
	cmd := exec.Command("sh", "-c", `find deploy -print0 | cpio --null -o --format=newc 2>/dev/null | gzip >> "$1"`, "sh", out)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("append vars to flavor initramfs: %v\n%s", err, output)
	}
	return out
}

// writeImageESPFile writes a file into the ESP (partition 1) of a raw image.
func writeImageESPFile(t *testing.T, rawImage, rel, content string) {
	t.Helper()
	loopDev := strings.TrimSpace(string(runOutput(t, "setup ESP loop device",
		"losetup", "--find", "--show", "--partscan", rawImage)))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }()
	waitForDevice(t, loopDev+"p1", 5*time.Second)
	mnt := filepath.Join(t.TempDir(), "esp")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount image ESP", loopDev+"p1", mnt)
	defer func() { _ = exec.Command("umount", mnt).Run() }()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(mnt, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mnt, rel), content)
	run(t, "unmount image ESP", "umount", mnt)
}

// installRAIDCheck puts the host mdadm and a script into the image root. The
// script finds the RAID members by size and creates a RAID 1 and a RAID 5;
// it fails when the md personalities are missing in the ramdisk kernel.
func installRAIDCheck(t *testing.T, rawImage string) {
	t.Helper()
	loopDev := strings.TrimSpace(string(runOutput(t, "setup root loop device",
		"losetup", "--find", "--show", "--partscan", rawImage)))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }()
	waitForDevice(t, loopDev+"p2", 5*time.Second)
	mnt := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount image root", loopDev+"p2", mnt)
	defer func() { _ = exec.Command("umount", mnt).Run() }()
	mdadm, err := exec.LookPath("mdadm")
	if err != nil {
		t.Fatalf("mdadm: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mnt, "usr", "local", "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyBinary(t, mdadm, filepath.Join(mnt, "usr", "sbin", "mdadm"))
	copySharedLibs(t, mdadm, mnt)
	writeFile(t, filepath.Join(mnt, "usr", "local", "sbin", "booty-raid-check.sh"), `set -e
r1=""; r5=""
for b in /sys/block/vd*; do
	case "$(cat "$b/size")" in
	131072) r1="$r1 /dev/${b##*/}" ;;
	196608) r5="$r5 /dev/${b##*/}" ;;
	esac
done
/usr/sbin/mdadm --create /dev/md101 --run --metadata=1.2 --level=1 --raid-devices=2 $r1
/usr/sbin/mdadm --create /dev/md105 --run --metadata=1.2 --level=5 --raid-devices=3 $r5
cat /proc/mdstat
/usr/sbin/mdadm --stop /dev/md101 /dev/md105
`)
	run(t, "unmount image root", "umount", mnt)
}
