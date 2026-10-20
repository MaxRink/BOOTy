//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The tests in this file boot BOOTy under OVMF with a persistent NVRAM file,
// so the EFI boot entries survive from one QEMU run to the next.

var managedEFIEntry = regexp.MustCompile(`Boot[0-9A-F]{4}\*? ubuntu`)

// TestUEFIEFIBootEntryLifecycleQEMU provisions under UEFI and checks that
// the "ubuntu" boot entry is created, then deprovisions twice: the first
// run removes the entry, the second finds none left.
func TestUEFIEFIBootEntryLifecycleQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireEFIBootMgr(t)
	ovmfCode, ovmfVars := uefiFirmware(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	installESPLoader(t, rawDisk)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))
	targetDisk := filepath.Join(t.TempDir(), "uefi-entries.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.nativeVars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["SUCCESS_ACTION"] = "poweroff"
	out := runQEMUUEFI(t, ovmfCode, ovmfVars, buildProvisionInitramfs(t, vars), targetDisk, "", 6*time.Minute)
	assertProvisionSucceeded(t, out)
	created := afterMarker(out, "EFI boot entry created")
	if !managedEFIEntry.MatchString(created) {
		t.Fatalf("provision did not create the ubuntu EFI boot entry. tail:\n%s", tail(out, 4000))
	}

	deprovision := caprf.nativeVars("deprovision")
	deprovision["SUCCESS_ACTION"] = "poweroff"
	initramfs := buildProvisionInitramfs(t, deprovision)
	out = runQEMUUEFI(t, ovmfCode, ovmfVars, initramfs, targetDisk, "", 4*time.Minute)
	if !strings.Contains(string(out), "removing EFI boot entry") {
		t.Fatalf("deprovision did not remove the ubuntu EFI boot entry. tail:\n%s", tail(out, 4000))
	}
	requirePowerOff(t, out)

	out = runQEMUUEFI(t, ovmfCode, ovmfVars, initramfs, targetDisk, "", 4*time.Minute)
	if strings.Contains(string(out), "removing EFI boot entry") {
		t.Fatalf("the ubuntu EFI boot entry is still in NVRAM after deprovision. tail:\n%s", tail(out, 4000))
	}
	requirePowerOff(t, out)
	caprf.requireStatuses(t, "init", "success", "init", "success", "init", "success")
}

// TestUEFIEFIBootEntryRequiredQEMU boots under UEFI with efi=noruntime, so
// /sys/firmware/efi exists but efivarfs can not be mounted, like a ramdisk
// without the efivarfs module. Provisioning must fail; the legacy scripts
// stopped there too. EFI_BOOT_ENTRY_REQUIRED=false continues with a warning.
func TestUEFIEFIBootEntryRequiredQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireEFIBootMgr(t)
	ovmfCode, ovmfVars := uefiFirmware(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	installESPLoader(t, rawDisk)
	image := compressGzip(t, rawDisk)

	for _, required := range []bool{true, false} {
		t.Run(fmt.Sprintf("required=%v", required), func(t *testing.T) {
			caprf := startFakeCAPRF(t, image)
			targetDisk := filepath.Join(t.TempDir(), "uefi-required.qcow2")
			run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")
			vars := caprf.nativeVars("provision")
			vars["IMAGE"] = caprf.baseURL + "/image.gz"
			vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
			vars["DISK_DEVICE"] = "/dev/vda"
			vars["SUCCESS_ACTION"] = "poweroff"
			vars["EFI_BOOT_ENTRY_REQUIRED"] = fmt.Sprint(required)
			out := runQEMUUEFI(t, ovmfCode, ovmfVars, buildProvisionInitramfs(t, vars), targetDisk, "efi=noruntime", 6*time.Minute)
			if !required {
				assertProvisionSucceeded(t, out)
				caprf.requireStatuses(t, "init", "success")
				return
			}
			if !strings.Contains(string(out), "mount efivarfs") {
				t.Fatalf("provision did not fail on the efivarfs mount. tail:\n%s", tail(out, 4000))
			}
			caprf.requireStatuses(t, "init", "error")
		})
	}
}

func requireEFIBootMgr(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("efibootmgr"); err != nil {
		failOrSkipUnsupportedHost(t, "efibootmgr not available for the UEFI boot entry test")
	}
}

// uefiFirmware returns the OVMF code and a private copy of the NVRAM file.
func uefiFirmware(t *testing.T) (code, vars string) {
	t.Helper()
	code = envOrDefault("OVMF_CODE", findOVMF("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_CODE.fd"))
	template := findOVMF("/usr/share/OVMF/OVMF_VARS_4M.fd", "/usr/share/OVMF/OVMF_VARS.fd")
	for _, f := range []string{code, template} {
		if _, err := os.Stat(f); err != nil {
			failOrSkipUnsupportedHost(t, "OVMF firmware not found at %s", f)
		}
	}
	vars = filepath.Join(t.TempDir(), "OVMF_VARS.fd")
	copyFile(t, template, vars)
	return code, vars
}

// installESPLoader puts \EFI\ubuntu\shimx64.efi on the image ESP, which
// create-efi-boot-entry registers.
func installESPLoader(t *testing.T, rawImage string) {
	t.Helper()
	loopDev := strings.TrimSpace(string(runOutput(t, "setup ESP loop device",
		"losetup", "--find", "--show", "--partscan", rawImage)))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }()
	esp := loopDev + "p1"
	waitForDevice(t, esp, 5*time.Second)
	mnt := filepath.Join(t.TempDir(), "esp")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	mountWithRetry(t, "mount image ESP", esp, mnt)
	defer func() { _ = exec.Command("umount", mnt).Run() }()
	if err := os.MkdirAll(filepath.Join(mnt, "EFI", "ubuntu"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mnt, "EFI", "ubuntu", "shimx64.efi"), "BOOTy KVM e2e shim\n")
	run(t, "unmount image ESP", "umount", mnt)
}

func runQEMUUEFI(t *testing.T, code, vars, initramfs, disk, extraCmdline string, timeoutDur time.Duration) []byte {
	t.Helper()
	args := []string{
		"-m", "1024", "-nographic", "-no-reboot",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + code,
		"-drive", "if=pflash,format=raw,file=" + vars,
		"-kernel", findKernel(t),
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", strings.TrimSpace(runnerKernelCmdline + " net.ifnames=0 " + extraCmdline),
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	ctx, cancel := context.WithTimeout(context.Background(), timeoutDur)
	defer cancel()
	out, err := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("UEFI QEMU run timed out after %v. tail:\n%s", timeoutDur, tail(out, 4000))
	}
	t.Logf("UEFI QEMU run exited (%v). tail:\n%s", err, tail(out, 2500))
	return out
}

// afterMarker returns the output after the first line containing marker.
func afterMarker(out []byte, marker string) string {
	text := string(out)
	if i := strings.Index(text, marker); i >= 0 {
		return text[i:]
	}
	return ""
}
