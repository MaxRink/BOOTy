//go:build e2e

package kvm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file boot the published BOOTy ISO (BOOTY_ISO, from
// `docker buildx build --target iso`) the way a server boots it from virtual
// media: BIOS through ISOLINUX, UEFI through the El Torito EFI image (Debian
// shim + signed GRUB + the Debian-signed kernel), and UEFI with Secure Boot
// and the Microsoft keys enrolled, like a server with factory keys.

func bootyISO(t *testing.T) string {
	t.Helper()
	iso := envOrDefault("BOOTY_ISO", "booty.iso")
	if _, err := os.Stat(iso); err != nil {
		failOrSkipUnsupportedHost(t, "BOOTY_ISO %s not found: build it with --target iso", iso)
	}
	return iso
}

// isoFirmware selects the firmware for an ISO boot. bios uses SeaBIOS (QEMU
// default); uefi uses OVMF without Secure Boot; secureboot uses OVMF with SMM
// and the Microsoft UEFI CA in db, so only Microsoft-signed loaders (shim)
// start.
func isoFirmware(t *testing.T, mode string) []string {
	t.Helper()
	var code, vars string
	switch mode {
	case "bios":
		return []string{"-machine", "q35,accel=kvm:tcg"}
	case "uefi":
		code = findOVMF("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_CODE.fd")
		vars = findOVMF("/usr/share/OVMF/OVMF_VARS_4M.fd", "/usr/share/OVMF/OVMF_VARS.fd")
	case "secureboot":
		code = findOVMF("/usr/share/OVMF/OVMF_CODE_4M.ms.fd", "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd")
		vars = findOVMF("/usr/share/OVMF/OVMF_VARS_4M.ms.fd")
	default:
		t.Fatalf("unknown firmware mode %q", mode)
	}
	for _, f := range []string{code, vars} {
		if _, err := os.Stat(f); err != nil {
			failOrSkipUnsupportedHost(t, "OVMF firmware %s not found for %s ISO boot", f, mode)
		}
	}
	varsCopy := filepath.Join(t.TempDir(), "OVMF_VARS.fd")
	copyFile(t, vars, varsCopy)
	args := []string{"-machine", "q35,accel=kvm:tcg"}
	if mode == "secureboot" {
		args = []string{
			"-machine", "q35,smm=on,accel=kvm:tcg",
			"-global", "driver=cfi.pflash01,property=secure,value=on",
		}
	}
	return append(args,
		"-drive", "if=pflash,format=raw,readonly=on,file="+code,
		"-drive", "if=pflash,format=raw,file="+varsCopy,
	)
}

// isoCDArgs attaches the ISO as a SCSI CD, like BMC virtual media.
func isoCDArgs(iso string) []string {
	return []string{
		"-device", "virtio-scsi-pci,id=scsi0",
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=cd0,readonly=on,media=cdrom", iso),
		"-device", "scsi-cd,drive=cd0,bootindex=1",
	}
}

func isoQEMUArgs(t *testing.T, mode string, media []string) []string {
	t.Helper()
	args := isoFirmware(t, mode)
	args = append(args, "-m", "2048", "-display", "none", "-nodefaults", "-no-reboot", "-serial", "stdio")
	args = append(args, media...)
	return append(args, splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))...)
}

func TestISOFirmwareBIOS(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	out := runQEMUSmoke(t, isoQEMUArgs(t, "bios", isoCDArgs(iso)), 4*time.Minute, "iso-bios", true)
	if !strings.Contains(string(out), "ISOLINUX") {
		t.Fatalf("ISOLINUX banner missing; tail:\n%s", tail(out, 1500))
	}
}

func TestISOFirmwareUEFI(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	out := runQEMUSmoke(t, isoQEMUArgs(t, "uefi", isoCDArgs(iso)), 4*time.Minute, "iso-uefi", true)
	if !strings.Contains(string(out), "Secure boot disabled") {
		t.Fatalf("kernel did not report UEFI boot without Secure Boot; tail:\n%s", tail(out, 1500))
	}
}

// TestISOFirmwareUEFIUSB boots the ISO as a USB disk (isohybrid GPT): the
// firmware starts shim from the EFI system partition, not from El Torito.
func TestISOFirmwareUEFIUSB(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	media := []string{
		"-device", "qemu-xhci",
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=usb0,readonly=on", iso),
		"-device", "usb-storage,drive=usb0,bootindex=1",
	}
	runQEMUSmoke(t, isoQEMUArgs(t, "uefi", media), 4*time.Minute, "iso-uefi-usb", true)
}

// TestISOFirmwareBIOSUSB boots the ISO as a disk on BIOS: the isohybrid MBR
// starts ISOLINUX.
func TestISOFirmwareBIOSUSB(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	media := []string{
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=d0,readonly=on", iso),
		"-device", "virtio-blk-pci,drive=d0,bootindex=1",
	}
	runQEMUSmoke(t, isoQEMUArgs(t, "bios", media), 4*time.Minute, "iso-bios-usb", true)
}

// wrongMediaMarker is printed by the decoy disk of
// TestISOFirmwareIgnoresOtherBootyDisk if GRUB loads its config.
const wrongMediaMarker = "WRONG-MEDIA-BOOTED"

// decoyDisk builds an ISO9660 image with the BOOTy volume label and GRUB
// configs at every path that BOOTy's GRUB could read.
func decoyDisk(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("xorriso"); err != nil {
		failOrSkipUnsupportedHost(t, "xorriso not available")
	}
	root := filepath.Join(t.TempDir(), "decoy")
	cfg := "echo " + wrongMediaMarker + "\nsleep 60\n"
	for _, p := range []string{"boot/grub/grub.cfg", "EFI/debian/grub.cfg", "EFI/BOOT/grub.cfg"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(t.TempDir(), "decoy.iso")
	if out, err := exec.Command("xorriso", "-as", "mkisofs", "-o", img, "-V", "BOOTY", "-r", root).CombinedOutput(); err != nil {
		t.Fatalf("build decoy disk: %v\n%s", err, out)
	}
	return img
}

// TestISOFirmwareIgnoresOtherBootyDisk attaches a second disk with the same
// volume label and GRUB configs. GRUB enumerates disks before CDs, so a
// label search would load the decoy. BOOTy must boot from its own medium,
// from CD and from USB.
func TestISOFirmwareIgnoresOtherBootyDisk(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	decoy := decoyDisk(t)
	decoyArgs := []string{
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=decoy,readonly=on", decoy),
		"-device", "virtio-blk-pci,drive=decoy,bootindex=2", // in the boot order, so OVMF connects it
	}
	usb := []string{
		"-device", "qemu-xhci",
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=usb0,readonly=on", iso),
		"-device", "usb-storage,drive=usb0,bootindex=1",
	}
	for name, media := range map[string][]string{"cd": isoCDArgs(iso), "usb": usb} {
		t.Run(name, func(t *testing.T) {
			args := isoQEMUArgs(t, "secureboot", append(append([]string{}, decoyArgs...), media...))
			out := runQEMUSmoke(t, args, 4*time.Minute, "iso-decoy-"+name, true)
			if strings.Contains(string(out), wrongMediaMarker) {
				t.Fatalf("GRUB loaded the config of the decoy disk; tail:\n%s", tail(out, 2000))
			}
		})
	}
}

// TestISOFirmwareSecureBoot checks the whole signature chain: the firmware
// accepts shim (Microsoft UEFI CA), shim accepts GRUB (Debian CA), GRUB
// accepts the kernel through shim, and the kernel locks itself down.
func TestISOFirmwareSecureBoot(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	out := runQEMUSmoke(t, isoQEMUArgs(t, "secureboot", isoCDArgs(iso)), 4*time.Minute, "iso-secureboot", true)
	for _, want := range []string{"Secure boot enabled", "Kernel is locked down from EFI Secure Boot"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("serial output lacks %q; tail:\n%s", want, tail(out, 2000))
		}
	}
}

// TestISOFirmwareSecureBootRejectsUnsignedKernel replaces the kernel in the ISO
// with a copy whose Authenticode signature is removed. GRUB must refuse it.
func TestISOFirmwareSecureBootRejectsUnsignedKernel(t *testing.T) {
	qemuAvailable(t)
	iso := bootyISO(t)
	if _, err := exec.LookPath("xorriso"); err != nil {
		failOrSkipUnsupportedHost(t, "xorriso not available")
	}
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinuz")
	if out, err := exec.Command("xorriso", "-osirrox", "on", "-indev", iso, "-extract", "/boot/vmlinuz", kernel).CombinedOutput(); err != nil {
		t.Fatalf("extract kernel: %v\n%s", err, out)
	}
	if err := stripAuthenticode(kernel); err != nil {
		t.Fatalf("strip signature: %v", err)
	}
	unsigned := filepath.Join(dir, "unsigned.iso")
	if out, err := exec.Command("xorriso", "-indev", iso, "-outdev", unsigned,
		"-boot_image", "any", "replay", "-map", kernel, "/boot/vmlinuz").CombinedOutput(); err != nil {
		t.Fatalf("repack ISO: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "qemu-system-x86_64", isoQEMUArgs(t, "secureboot", isoCDArgs(unsigned))...).CombinedOutput()
	if err != nil && ctx.Err() == nil {
		t.Fatalf("QEMU failed: %v\n%s", err, tail(out, 1500))
	}
	if strings.Contains(string(out), bootyStartMarker) || strings.Contains(string(out), "Linux version") {
		t.Fatalf("an unsigned kernel booted with Secure Boot; tail:\n%s", tail(out, 2000))
	}
	if !strings.Contains(string(out), "bad shim signature") {
		t.Fatalf("GRUB did not report the rejected kernel signature; tail:\n%s", tail(out, 2000))
	}
}

// stripAuthenticode removes the PE certificate table (the Authenticode
// signature) from a kernel image in place.
func stripAuthenticode(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) < 0x40 {
		return errors.New("file too small for a PE image")
	}
	pe := int(binary.LittleEndian.Uint32(b[0x3c:]))
	if pe+24+2 > len(b) || string(b[pe:pe+4]) != "PE\x00\x00" {
		return errors.New("no PE header")
	}
	optional := pe + 24
	dataDirs := optional + 96
	if binary.LittleEndian.Uint16(b[optional:]) == 0x20b { // PE32+
		dataDirs = optional + 112
	}
	certEntry := dataDirs + 4*8 // IMAGE_DIRECTORY_ENTRY_SECURITY
	off := int(binary.LittleEndian.Uint32(b[certEntry:]))
	size := int(binary.LittleEndian.Uint32(b[certEntry+4:]))
	if off == 0 || size == 0 || off+size > len(b) {
		return errors.New("kernel has no signature to strip")
	}
	binary.LittleEndian.PutUint32(b[certEntry:], 0)
	binary.LittleEndian.PutUint32(b[certEntry+4:], 0)
	b = append(b[:off], b[off+size:]...)
	return os.WriteFile(path, b, 0o644)
}
