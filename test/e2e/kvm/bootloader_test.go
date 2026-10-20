//go:build e2e

package kvm

import (
	"os"
	"testing"
	"time"
)

// TestUEFIBootPathSmoke verifies BOOTy starts under OVMF firmware.
// Note: This uses QEMU's -kernel/-initrd direct Linux boot path which bypasses the
// UEFI firmware boot flow (GRUB/systemd-boot). This is intentional — the test validates
// that BOOTy's initramfs starts correctly with OVMF firmware present (exercising EFI
// variable access and firmware device exposure), not the full bootloader chain. Full
// UEFI boot-path validation is covered by the ISO boot tests (UEFI ISO Boot Validation).
func TestUEFIBootPathSmoke(t *testing.T) {
	qemuAvailable(t)
	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)
	ovmf := envOrDefault("OVMF_CODE", findOVMF(
		"/usr/share/OVMF/OVMF_CODE_4M.fd",
		"/usr/share/OVMF/OVMF_CODE.fd",
	))
	ovmfVars := envOrDefault("OVMF_VARS", "")

	if _, err := os.Stat(ovmf); err != nil {
		failOrSkipUnsupportedHost(t, "OVMF firmware not found at %s for UEFI boot-path smoke", ovmf)
	}

	args := []string{
		"-m", "512",
		"-nographic",
		"-no-reboot",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + ovmf,
	}
	if ovmfVars != "" {
		args = append(args, "-drive", "if=pflash,format=raw,file="+ovmfVars)
	}
	args = append(args,
		"-kernel", kernel,
		"-initrd", initramfs,
		"-append", runnerKernelCmdline,
	)
	args = append(args, splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))...)

	out := runQEMUSmoke(t, args, 2*time.Minute, "uefi-boot-path", true)
	t.Logf("Bootloader QEMU output (last 500 bytes): %s", tail(out, 500))
}
