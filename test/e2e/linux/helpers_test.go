//go:build linux && linux_e2e

// Package linux contains Linux-specific E2E tests that require root access.
// These tests exercise real disk, filesystem, and mount operations using
// loopback devices. They run as root on Ubuntu CI runners.
package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/test/e2e/loopdev"
)

func failOrSkipUnsupportedHost(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

func requireTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		failOrSkipUnsupportedHost(t, "%s not available", tool)
	}
}

// requireRoot skips the test if not running as root outside CI.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		failOrSkipUnsupportedHost(t, "test requires root (run with sudo)")
	}
}

// createLoopDevice creates a sparse file of the given size in GB,
// partitions it with GPT (EFI + Linux root), and attaches it as
// a loopback device. Returns the loop device path (e.g. /dev/loop0).
func createLoopDevice(t *testing.T, sizeGB int) string {
	t.Helper()
	requireRoot(t)

	ctx := context.Background()

	f, err := os.CreateTemp("", "booty-e2e-*.img")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	f.Close()

	// Create sparse file.
	cmd := exec.CommandContext(ctx, "truncate", "-s", fmt.Sprintf("%dG", sizeGB), f.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(f.Name())
		t.Fatalf("truncate: %s: %v", out, err)
	}

	// Create GPT partition table with EFI + Linux root partitions.
	sfdisk := exec.CommandContext(ctx, "sfdisk", f.Name())
	sfdisk.Stdin = strings.NewReader(
		"label: gpt\n" +
			"1: size=100M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name=EFI\n" +
			"2: type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=root\n",
	)
	if out, err := sfdisk.CombinedOutput(); err != nil {
		os.Remove(f.Name())
		t.Fatalf("sfdisk: %s: %v", out, err)
	}

	// Attach loopback device with partition scanning and wait for p1, p2.
	loopDev, err := loopdev.Attach(f.Name(), 2)
	if err != nil {
		os.Remove(f.Name())
		t.Fatal(err)
	}

	t.Cleanup(func() {
		exec.CommandContext(ctx, "losetup", "-d", loopDev).Run() //nolint:errcheck
		os.Remove(f.Name())
	})

	return loopDev
}

// createRawLoopDevice creates a sparse file and attaches it as a loopback
// device without partitioning. Useful for image-streaming tests.
func createRawLoopDevice(t *testing.T, sizeMB int) string {
	t.Helper()
	requireRoot(t)

	ctx := context.Background()

	f, err := os.CreateTemp("", "booty-raw-*.img")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	f.Close()

	cmd := exec.CommandContext(ctx, "truncate", "-s", fmt.Sprintf("%dM", sizeMB), f.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(f.Name())
		t.Fatalf("truncate: %s: %v", out, err)
	}

	loopDev, err := loopdev.Attach(f.Name(), 0)
	if err != nil {
		os.Remove(f.Name())
		t.Fatal(err)
	}

	t.Cleanup(func() {
		exec.CommandContext(ctx, "losetup", "-d", loopDev).Run() //nolint:errcheck
		os.Remove(f.Name())
	})

	return loopDev
}

// runCmd is a helper that runs a command and fails the test on error.
func runCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %s: %v", name, strings.Join(args, " "), out, err)
	}
	return string(out)
}
