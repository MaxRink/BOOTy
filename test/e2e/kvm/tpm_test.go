//go:build e2e

package kvm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTPMSmokeQEMU(t *testing.T) {
	qemuAvailable(t)
	if _, err := exec.LookPath("swtpm"); err != nil {
		failOrSkipUnsupportedHost(t, "swtpm not available for TPM QEMU smoke")
	}

	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)
	tpmDir := t.TempDir()
	tpmSocket := filepath.Join(tpmDir, "swtpm-sock")

	// Start swtpm emulator.
	swtpmCtx, swtpmCancel := context.WithCancel(context.Background())
	swtpm := exec.CommandContext(swtpmCtx, "swtpm", "socket",
		"--tpmstate", "dir="+tpmDir,
		"--ctrl", "type=unixio,path="+tpmSocket,
		"--tpm2",
	)
	var swtpmStderr bytes.Buffer
	swtpm.Stderr = &swtpmStderr
	if err := swtpm.Start(); err != nil {
		swtpmCancel()
		t.Fatalf("failed to start swtpm: %v", err)
	}
	defer func() { swtpmCancel(); _ = swtpm.Wait() }()

	// Wait for swtpm socket to appear before launching QEMU.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(tpmSocket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("swtpm socket did not appear within 15s; stderr: %s", swtpmStderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	args := make([]string, 0, 20)
	args = append(args,
		"-m", "512",
		"-nographic",
		"-no-reboot",
		"-chardev", "socket,id=chrtpm,path="+tpmSocket,
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", "tpm-tis,tpmdev=tpm0",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-append", runnerKernelCmdline,
	)
	args = append(args, splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))...)

	out := runQEMUSmoke(t, args, 2*time.Minute, "tpm", true)
	t.Logf("TPM QEMU output (last 500 bytes): %s", tail(out, 500))
}
