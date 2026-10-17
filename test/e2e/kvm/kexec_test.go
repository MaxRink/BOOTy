//go:build e2e

package kvm

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// kexecCmdlineMarker is a kernel parameter that only the grub.cfg of the
// provisioned target has: the second kernel prints its /proc/cmdline.
const kexecCmdlineMarker = "booty.kexec.target=k42"

// TestKexecSmokeQEMU provisions a target whose grub.cfg points at a kernel
// and a verify initramfs, lets BOOTy kexec into it, and checks from the
// second kernel that it booted with the target command line. Under OVMF the
// second kernel must also have the EFI runtime services: efivarfs lists the
// variables (kexec_file_load passes the EFI runtime mapping on x86).
func TestKexecSmokeQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	for _, firmware := range []string{"bios", "uefi"} {
		t.Run(firmware, func(t *testing.T) {
			out := runKexecTarget(t, firmware)
			cmdline := kexecVerifyLine(out, "cmdline=")
			if !strings.Contains(cmdline, kexecCmdlineMarker) || strings.Contains(cmdline, "net.ifnames=0") {
				t.Fatalf("second kernel cmdline = %q, want the target grub.cfg line with %s and not the BOOTy one. tail:\n%s",
					cmdline, kexecCmdlineMarker, tail([]byte(out), 4000))
			}
			efi := kexecVerifyLine(out, "efi=")
			if firmware == "bios" {
				if efi != "efi=no" {
					t.Fatalf("BIOS boot: %q, want efi=no", efi)
				}
				return
			}
			m := regexp.MustCompile(`efivars=(\d+)`).FindStringSubmatch(efi)
			if !strings.HasPrefix(efi, "efi=yes") || m == nil {
				t.Fatalf("UEFI boot: %q, want efi=yes with an efivars count. tail:\n%s", efi, tail([]byte(out), 4000))
			}
			if n, _ := strconv.Atoi(m[1]); n == 0 {
				t.Fatalf("UEFI boot: no EFI variables after kexec (%q): the EFI runtime services did not survive", efi)
			}
		})
	}
}

func runKexecTarget(t *testing.T, firmware string) string {
	t.Helper()
	kernel := findKernel(t)
	rawImage := createChrootCapableTestDiskImage(t, 512)
	installKexecVerifyBoot(t, rawImage, kernel, "console=ttyS0 panic=1 "+kexecCmdlineMarker)
	if firmware == "uefi" {
		installESPLoader(t, rawImage) // create-efi-boot-entry needs a loader
	}
	baseURL := startImageServer(t, compressGzip(t, rawImage))

	targetDisk := filepath.Join(t.TempDir(), "kexec-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "kexec-target",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"DISABLE_KEXEC":             "false",
	})
	appendInitramfsFile(t, initramfs, "sbin/growpart", `#!/bin/sh
echo "NOCHANGE: partition $2 could only be grown by 0"
exit 1
`)
	var out []byte
	if firmware == "uefi" {
		code, vars := uefiFirmware(t)
		out = runQEMUUEFI(t, code, vars, initramfs, targetDisk, "", 6*time.Minute)
	} else {
		out = runQEMUProvision(t, kernel, initramfs, targetDisk, 6*time.Minute)
	}
	if !strings.Contains(string(out), "attempting kexec") {
		t.Fatalf("BOOTy did not attempt kexec. tail:\n%s", tail(out, 4000))
	}
	_, second, _ := strings.Cut(string(out), "attempting kexec")
	return second
}

// kexecVerifyLine returns the text after "KEXEC-VERIFY <prefix>" up to
// the end of that line, including the prefix.
func kexecVerifyLine(out, prefix string) string {
	_, rest, ok := strings.Cut(out, "KEXEC-VERIFY "+prefix)
	if !ok {
		return ""
	}
	line, _, _ := strings.Cut(rest, "\n")
	return prefix + strings.TrimSpace(line)
}
