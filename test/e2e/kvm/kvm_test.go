//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const bootyStartMarker = "starting BOOTy"

// noKHO turns off kexec handover. Linux 7.0 (runner image 20261018) enables
// it by default, and when its scratch area fits it reserves about two thirds
// of a 512 MiB guest. Under OVMF only about 40 MiB stay free and the
// initramfs unpack fails with "write error". Whether it fits depends on where
// the kernel and initrd land, so such boots fail at random. Kernels without
// KHO ignore the parameter.
const noKHO = "kho=off"

// runnerKernelCmdline is the base command line for every guest that boots the
// runner kernel (BOOTY_KERNEL or findKernel). Guests that boot the BOOTy
// flavor kernel or the kernel of a provisioned OS do not need it.
const runnerKernelCmdline = "console=ttyS0 panic=1 " + noKHO

// qemuAvailable checks if qemu-system-x86_64 is installed.
func qemuAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		failOrSkipUnsupportedHost(t, "qemu-system-x86_64 not available")
	}
}

func failOrSkipUnsupportedHost(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// envOrDefault returns the environment variable value or a default.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// requireKVMAssets fails the test if the initramfs or kernel files are not present.
func requireKVMAssets(t *testing.T, initramfs, kernel string) {
	t.Helper()
	if _, err := os.Stat(initramfs); err != nil {
		t.Fatalf("initramfs %s not found", initramfs)
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Fatalf("kernel %s not found", kernel)
	}
}

// findOVMF returns the first existing path from candidates, or the first candidate if none exist.
func findOVMF(candidates ...string) string {
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return candidates[0]
}

// splitExtraArgs splits a shell-style argument string into separate arguments,
// respecting single and double quotes so values like "-append 'console=ttyS0 panic=1'"
// are parsed correctly.
func splitExtraArgs(env string) []string {
	if env == "" {
		return nil
	}

	var args []string
	var current strings.Builder
	var quote rune
	for _, r := range env {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// tail returns the last n bytes of data.
func tail(data []byte, n int) []byte {
	if len(data) <= n {
		return data
	}
	return data[len(data)-n:]
}

// runQEMUSmoke launches QEMU and optionally requires BOOTy startup marker in serial output.
func runQEMUSmoke(t *testing.T, args []string, timeout time.Duration, scenario string, requireBootyMarker bool) []byte {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()
	outStr := string(out)

	if ctx.Err() == context.DeadlineExceeded {
		if requireBootyMarker && !strings.Contains(outStr, bootyStartMarker) {
			failWithoutBootyMarker(t, scenario, "timed out before", out)
		}
		if requireBootyMarker {
			t.Logf("QEMU %s timed out after BOOTy marker (acceptable for smoke)", scenario)
		} else {
			t.Logf("QEMU %s timed out during firmware-path smoke", scenario)
		}
		return out
	}

	if err != nil {
		t.Fatalf("QEMU %s failed: %v\nOutput:\n%s", scenario, err, out)
	}

	if requireBootyMarker && !strings.Contains(outStr, bootyStartMarker) {
		failWithoutBootyMarker(t, scenario, "completed without", out)
	}

	return out
}

// failWithoutBootyMarker fails the test with the full console: a tail can
// hide the early boot error that kept BOOTy from starting.
func failWithoutBootyMarker(t *testing.T, scenario, how string, out []byte) {
	t.Helper()
	if strings.Contains(string(out), "Initramfs unpacking failed") {
		t.Fatalf("QEMU %s %s BOOTy marker: the guest kernel could not unpack the initramfs (often too little guest RAM). Console:\n%s", scenario, how, out)
	}
	t.Fatalf("QEMU %s %s BOOTy marker. Console:\n%s", scenario, how, out)
}
