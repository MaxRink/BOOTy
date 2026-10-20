//go:build e2e

package kvm

import (
	"os"
	"testing"
	"time"
)

// TestUEFISecureBootSmoke validates that QEMU boots with secureboot-enabled OVMF firmware
// without crashing. The BOOTy startup marker is not required (requireBootyMarker=false)
// because unsigned direct-kernel boot may be blocked by Secure Boot policy in the firmware,
// preventing BOOTy from reaching its init. The test passes if QEMU exits cleanly or times
// out, confirming the firmware + kernel + initramfs combination doesn't hard-fail.
// Full bootloader-chain validation requires a signed shim/GRUB and is covered by ISO tests.
func TestUEFISecureBootSmoke(t *testing.T) {
	qemuAvailable(t)
	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)
	ovmf := envOrDefault("OVMF_CODE", findOVMF(
		"/usr/share/OVMF/OVMF_CODE_4M.secboot.fd",
		"/usr/share/OVMF/OVMF_CODE.secboot.fd",
	))
	ovmfVars := envOrDefault("OVMF_VARS", "")

	if _, err := os.Stat(ovmf); err != nil {
		failOrSkipUnsupportedHost(t, "OVMF Secure Boot firmware not found at %s for Secure Boot smoke", ovmf)
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

	out := runQEMUSmoke(t, args, 2*time.Minute, "uefi-secureboot-firmware", false)
	t.Logf("SecureBoot QEMU output (last 500 bytes): %s", tail(out, 500))
}
