//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Legacy evidence: common.sh _discover_disks lists NVMe and virtio disks
// (lsblk --include 259,253) before SCSI/SATA disks (--include 8), and
// _select_single_disk takes the first eligible disk, not the largest one.
// The guest has a small virtio disk and a larger SCSI disk. BOOTy without
// legacy compat would pick the larger SCSI disk.
func TestLegacyCompatDiskSelectionQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	dir := t.TempDir()
	virtioDisk := filepath.Join(dir, "virtio-small.qcow2")
	scsiDisk := filepath.Join(dir, "scsi-large.qcow2")
	run(t, "create virtio disk", "qemu-img", "create", "-f", "qcow2", virtioDisk, "2G")
	run(t, "create scsi disk", "qemu-img", "create", "-f", "qcow2", scsiDisk, "4G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	initramfs := buildProvisionInitramfs(t, vars)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	args := []string{
		"-m", "1024", "-nographic", "-no-reboot",
		"-kernel", findKernel(t), "-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", virtioDisk),
		"-device", "virtio-scsi-pci,id=scsi0",
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=scsidisk", scsiDisk),
		"-device", "scsi-hd,drive=scsidisk,bus=scsi0.0",
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56", "-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	output, _ := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("QEMU timed out. tail:\n%s", tail(output, 4000))
	}
	t.Logf("legacy disk selection output tail:\n%s", tail(output, 3000))
	if !strings.Contains(string(output), "/dev/sda") {
		t.Fatalf("guest did not see the SCSI disk, the test cannot prove the order. tail:\n%s", tail(output, 4000))
	}
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	if !strings.Contains(string(output), `msg="selected disk" device=/dev/vda`) {
		t.Fatalf("legacy compat did not select the first virtio disk. tail:\n%s", tail(output, 4000))
	}

	rootMount, cleanup := mountQcow2(t, virtioDisk)
	defer cleanup()
	if got := strings.TrimSpace(readProvisionedFile(t, rootMount, "etc/hostname")); got != "legacy-compat" {
		t.Fatalf("virtio disk hostname = %q, want the provisioned image", got)
	}
}
