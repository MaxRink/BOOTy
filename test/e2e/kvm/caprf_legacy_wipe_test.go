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

// Legacy evidence: provision.sh calls wipe_all_disks, so a provision run
// clears the signatures of every disk, not only of the target disk.
func TestLegacyCompatProvisionWipesAllDisksQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	dir := t.TempDir()
	targetDisk := filepath.Join(dir, "target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")
	// The second disk carries an old GPT and filesystems.
	otherDisk := filepath.Join(dir, "other.qcow2")
	run(t, "convert second disk", "qemu-img", "convert", "-O", "qcow2", createTestDiskImage(t, 256), otherDisk)

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	initramfs := buildProvisionInitramfs(t, vars)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	output, _ := exec.CommandContext(ctx, "qemu-system-x86_64",
		"-m", "1024", "-nographic", "-no-reboot",
		"-kernel", findKernel(t), "-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", targetDisk),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", otherDisk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56", "-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("QEMU timed out. tail:\n%s", tail(output, 4000))
	}
	t.Logf("legacy wipe output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	raw := filepath.Join(dir, "other.raw")
	run(t, "convert second disk to raw", "qemu-img", "convert", "-O", "raw", otherDisk, raw)
	if sigs := strings.TrimSpace(string(runOutput(t, "list signatures", "wipefs", "--noheadings", raw))); sigs != "" {
		t.Fatalf("second disk kept its signatures:\n%s", sigs)
	}
}
