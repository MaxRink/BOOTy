//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file prove that the native options select the legacy
// CAPRF behavior without CAPRF_LEGACY_COMPAT. Each test fails when BOOTy
// ignores the option, because the default behavior differs.

// nativeVars returns the fake CAPRF vars without CAPRF_LEGACY_COMPAT.
func (f *fakeCAPRF) nativeVars(mode string) map[string]string {
	vars := f.vars(mode)
	delete(vars, "CAPRF_LEGACY_COMPAT")
	return vars
}

// runQEMUExpectWait boots the provision VM and expects it to keep running
// until timeoutDur. It fails the test when the guest reboots or powers off
// before that, which is what BOOTy does without FAILURE_ACTION=wait.
func runQEMUExpectWait(t *testing.T, kernel, initramfs, disk string, timeoutDur time.Duration) []byte {
	t.Helper()
	args := []string{
		"-m", "1024",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDur)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("guest ended before %v instead of waiting. tail:\n%s", timeoutDur, tail(out, 4000))
	}
	text := string(out)
	if strings.Contains(text, "reboot: Restarting system") || strings.Contains(text, "reboot: Power down") {
		t.Fatalf("guest rebooted or powered off instead of waiting. tail:\n%s", tail(out, 4000))
	}
	return out
}

// SUCCESS_ACTION=poweroff: power off after a successful provision, no kexec.
// Without the option BOOTy kexecs or reboots, so requirePowerOff fails.
func TestNativeParityProvisionSuccessActionPowerOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "native-poweroff-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.nativeVars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["SUCCESS_ACTION"] = "poweroff"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("native poweroff provision output tail:\n%s", tail(output, 3000))

	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, output)
	if strings.Contains(string(output), "attempting kexec") {
		t.Fatalf("SUCCESS_ACTION=poweroff must not kexec. tail:\n%s", tail(output, 4000))
	}
}

// SUCCESS_ACTION=poweroff: power off after a successful deprovision.
func TestNativeParityDeprovisionSuccessActionPowerOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caprf := startFakeCAPRF(t, "")
	targetDisk := filepath.Join(t.TempDir(), "native-poweroff-deprovision.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	vars := caprf.nativeVars("deprovision")
	vars["SUCCESS_ACTION"] = "poweroff"
	output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 4*time.Minute)
	t.Logf("native poweroff deprovision output tail:\n%s", tail(output, 3000))

	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, output)
}

// FAILURE_ACTION=wait: a failed provision reports the error and keeps the
// ramdisk running (RESCUE_MODE defaults to wait). Without the option BOOTy
// reboots after the error.
func TestNativeParityProvisionFailureActionWaitQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caprf := startFakeCAPRF(t, "")
	targetDisk := filepath.Join(t.TempDir(), "native-wait-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	vars := caprf.nativeVars("provision")
	vars["IMAGE"] = caprf.baseURL + "/missing.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["FAILURE_ACTION"] = "wait"
	output := runQEMUExpectWait(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 3*time.Minute)
	t.Logf("native wait provision output tail:\n%s", tail(output, 3000))

	caprf.requireStatuses(t, "init", "error")
	if !strings.Contains(string(output), "waiting for manual intervention") {
		t.Fatalf("failed provision did not wait. tail:\n%s", tail(output, 4000))
	}
}

// FAILURE_ACTION=wait: a vars file that does not validate keeps the ramdisk
// running. caprf.FailureConfig reads the option from the broken file.
func TestNativeParityInvalidVarsFailureActionWaitQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caprf := startFakeCAPRF(t, "")
	targetDisk := filepath.Join(t.TempDir(), "native-wait-invalid.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	vars := caprf.nativeVars("no-such-mode")
	vars["FAILURE_ACTION"] = "wait"
	output := runQEMUExpectWait(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 90*time.Second)
	if !strings.Contains(string(output), "keeping the ramdisk running") {
		t.Fatalf("invalid vars did not wait. tail:\n%s", tail(output, 4000))
	}
}
