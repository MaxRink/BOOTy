//go:build e2e

package kvm

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLUKSSmokeQEMU(t *testing.T) {
	qemuAvailable(t)
	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)
	diskImg := envOrDefault("LUKS_DISK_IMAGE", "")
	if diskImg == "" {
		failOrSkipUnsupportedHost(t, "LUKS_DISK_IMAGE not set for LUKS QEMU smoke")
	}

	extra := splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))
	args := make([]string, 0, 16+len(extra))
	args = append(args,
		"-m", "512",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-append", runnerKernelCmdline,
		"-drive", "file="+diskImg+",format=qcow2,if=virtio",
	)
	args = append(args, extra...)

	out := runQEMUSmoke(t, args, 2*time.Minute, "luks", true)
	t.Logf("LUKS QEMU output (last 500 bytes): %s", tail(out, 500))
}

// TestLUKSVerifyHeader verifies that the LUKS test disk has a valid LUKS header.
// This ensures the CI-generated LUKS disk is properly formatted before being
// used in smoke tests.
func TestLUKSVerifyHeader(t *testing.T) {
	diskImg := envOrDefault("LUKS_DISK_IMAGE", "")
	if diskImg == "" {
		failOrSkipUnsupportedHost(t, "LUKS_DISK_IMAGE not set for LUKS header verification")
	}

	if _, err := os.Stat(diskImg); err != nil {
		t.Fatalf("LUKS disk image not found: %v", err)
	}

	if _, err := exec.LookPath("cryptsetup"); err != nil {
		failOrSkipUnsupportedHost(t, "cryptsetup not available for LUKS header verification")
	}

	// Verify LUKS header via cryptsetup luksDump (works on disk images directly).
	// For qcow2, we need to use qemu-nbd first.
	if _, err := exec.LookPath("qemu-nbd"); err != nil {
		failOrSkipUnsupportedHost(t, "qemu-nbd not available for LUKS header verification")
	}

	requireRoot(t)

	// Connect qcow2 to nbd device.
	cmd := exec.Command("modprobe", "nbd", "max_part=8")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("modprobe nbd: %v\n%s", err, out)
	}

	nbdDev := ""
	for i := 0; i < 16; i++ {
		dev := fmt.Sprintf("/dev/nbd%d", i)
		cmd := exec.Command("qemu-nbd", "--connect="+dev, diskImg)
		if _, err := cmd.CombinedOutput(); err == nil {
			nbdDev = dev
			break
		}
	}
	if nbdDev == "" {
		failOrSkipUnsupportedHost(t, "no free nbd device found for LUKS header verification")
	}
	t.Cleanup(func() {
		disconnectNBD(t, nbdDev)
	})

	// Poll cryptsetup luksDump until the nbd device is ready (qemu-nbd connect
	// is async — the block device exists but may not serve data immediately).
	var out []byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		out, err = exec.Command("cryptsetup", "luksDump", nbdDev).CombinedOutput()
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	output := string(out)
	if !strings.Contains(output, "LUKS header information") {
		t.Fatalf("LUKS header not found in luksDump output:\n%s", output)
	}

	t.Logf("LUKS header verified:\n%s", output[:min(len(output), 500)])
}
