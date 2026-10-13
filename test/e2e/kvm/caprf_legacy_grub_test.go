//go:build e2e

package kvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Legacy evidence: provision.sh:452-481 writes
// GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud console=ttyS0,115200n8 $EXTRA_KERNEL_PARAMS"
// to /etc/default/grub.d/10-caprf-kernel-params.cfg, runs update-grub and
// leaves the getty units of the image alone.
func TestLegacyCompatGRUBKernelParamsQEMU(t *testing.T) {
	testAppendGRUBKernelParams(t, func(f *fakeCAPRF) map[string]string { return f.vars("provision") })
}

// GRUB_CMDLINE_MODE=append writes the same drop-in without
// CAPRF_LEGACY_COMPAT. The default resolved mode writes a different line,
// so the test fails when BOOTy ignores the option.
func TestNativeParityGRUBCmdlineAppendQEMU(t *testing.T) {
	testAppendGRUBKernelParams(t, func(f *fakeCAPRF) map[string]string {
		vars := f.nativeVars("provision")
		vars["GRUB_CMDLINE_MODE"] = "append"
		vars["SUCCESS_ACTION"] = "poweroff"
		return vars
	})
}

func testAppendGRUBKernelParams(t *testing.T, varsFor func(*fakeCAPRF) map[string]string) {
	t.Helper()
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "legacy-grub-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := varsFor(caprf)
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["EXTRA_KERNEL_PARAMS"] = "intel_iommu=on"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("legacy grub output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	got := strings.TrimSpace(readProvisionedFile(t, rootMount, "etc/default/grub.d/10-caprf-kernel-params.cfg"))
	want := `GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud console=ttyS0,115200n8 intel_iommu=on"`
	if got != want {
		t.Fatalf("grub drop-in = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(rootMount, "boot", "grub", "grub.cfg")); err != nil {
		t.Fatalf("update-grub did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootMount, "etc", "systemd", "system", "serial-getty@ttyS0.service.d")); err == nil {
		t.Fatal("append mode wrote a serial getty drop-in")
	}
}
