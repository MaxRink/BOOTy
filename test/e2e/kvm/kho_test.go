//go:build e2e

package kvm

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// khoGuestMemory is the guest RAM for boots with kexec handover left on.
// With Linux 7.0.0-1017-azure the KHO scratch area takes about 360 MiB at
// 1024 MiB and always fits, so about 520 MiB (OVMF) or 590 MiB (SeaBIOS) stay
// free: more than the 512 MiB kho=off guests have (about 370 MiB and 435 MiB).
const khoGuestMemory = "1024"

// maxAvailableWithKHO is the highest share of guest RAM the kernel may report
// as available when the KHO scratch area is reserved. At 1024 MiB with
// 7.0.0-1017-azure it is about 0.50 (OVMF) and 0.56 (SeaBIOS) with KHO, and
// 0.85 (OVMF) to 0.91 without it (kho=off, or 6.17).
const maxAvailableWithKHO = 0.75

var memoryLineRE = regexp.MustCompile(`Memory: (\d+)K/(\d+)K available`)

// TestKHODefaultBootSmoke boots the runner kernel with kexec handover at its
// default under OVMF and SeaBIOS, and checks that BOOTy starts. When the
// kernel turns KHO on by default, the scratch area must have been reserved.
func TestKHODefaultBootSmoke(t *testing.T) {
	qemuAvailable(t)
	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)
	khoOn := kernelEnablesKHO(t, kernel)
	ovmf := envOrDefault("OVMF_CODE", findOVMF(
		"/usr/share/OVMF/OVMF_CODE_4M.fd",
		"/usr/share/OVMF/OVMF_CODE.fd",
	))

	for _, fw := range []string{"ovmf", "seabios"} {
		t.Run(fw, func(t *testing.T) {
			args := []string{"-m", khoGuestMemory, "-nographic", "-no-reboot"}
			if fw == "ovmf" {
				if _, err := os.Stat(ovmf); err != nil {
					failOrSkipUnsupportedHost(t, "OVMF firmware not found at %s", ovmf)
				}
				args = append(args, "-drive", "if=pflash,format=raw,readonly=on,file="+ovmf)
				if vars := os.Getenv("OVMF_VARS"); vars != "" {
					args = append(args, "-drive", "if=pflash,format=raw,file="+vars)
				}
			}
			// No noKHO here: this guest keeps the kernel default.
			args = append(args, "-kernel", kernel, "-initrd", initramfs,
				"-append", "console=ttyS0 panic=1")
			args = append(args, splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))...)

			out := runQEMUSmoke(t, args, 2*time.Minute, "kho-default-"+fw, true)
			share, ok := availableMemoryShare(out)
			if !ok {
				t.Fatalf("console has no %q line. Console:\n%s", "Memory: ...K/...K available", out)
			}
			t.Logf("%s, %s MiB: %s (%.2f available)", fw, khoGuestMemory, memoryLineRE.Find(out), share)
			if !khoOn {
				t.Logf("the runner kernel does not turn KHO on by default; checked only that BOOTy starts")
				return
			}
			if strings.Contains(string(out), "KHO: Failed to reserve scratch area") {
				t.Fatalf("KHO did not reserve its scratch area in a %s MiB guest. Console:\n%s", khoGuestMemory, out)
			}
			if share >= maxAvailableWithKHO {
				t.Fatalf("%.2f of the guest RAM is available, want below %.2f: no KHO scratch area is reserved. Console:\n%s",
					share, maxAvailableWithKHO, out)
			}
		})
	}
}

// kernelEnablesKHO reports whether the kernel config turns kexec handover on
// by default. The config is /boot/config-<release> of the runner kernel.
func kernelEnablesKHO(t *testing.T, kernel string) bool {
	t.Helper()
	release := os.Getenv("BOOTY_KERNEL_RELEASE")
	if release == "" {
		release = strings.TrimPrefix(filepath.Base(kernel), "vmlinuz-")
	}
	path := "/boot/config-" + release
	cfg, err := os.ReadFile(path)
	if err != nil {
		failOrSkipUnsupportedHost(t, "cannot tell whether the kernel turns KHO on: %v", err)
	}
	on := strings.Contains(string(cfg), "\nCONFIG_KEXEC_HANDOVER_ENABLE_DEFAULT=y\n")
	t.Logf("%s: CONFIG_KEXEC_HANDOVER_ENABLE_DEFAULT=y is %v", path, on)
	return on
}

// availableMemoryShare returns available/total from the kernel "Memory:" line.
func availableMemoryShare(out []byte) (float64, bool) {
	m := memoryLineRE.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	avail, err1 := strconv.ParseFloat(string(m[1]), 64)
	total, err2 := strconv.ParseFloat(string(m[2]), 64)
	if err1 != nil || err2 != nil || total == 0 {
		return 0, false
	}
	return avail / total, true
}
