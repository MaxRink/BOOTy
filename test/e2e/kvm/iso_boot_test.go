//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestISOBootHeadlessQ35 replicates the exact ISO packing and QEMU VM
// configuration used by CAPRF's E2E environment (kindmetal):
//
//   - xorrisofs without -isohybrid-mbr (El Torito only, no hybrid MBR)
//   - ISOLINUX with SERIAL 0 115200 (required for headless VMs)
//   - q35 machine type, -display none -nodefaults, isa-serial on ttyS0
//   - SCSI CD-ROM at bootindex=1 (same as kindmetal's libvirt domain)
//
// The test deliberately builds a custom ISO from BOOTY_KERNEL/BOOTY_INITRAMFS
// rather than using the published gobgp-iso artifact.  This isolates the
// ISOLINUX + serial console boot chain from initramfs content, catching
// packaging regressions on every push (the published artifact is validated
// separately in the CAPRF kindmetal E2E pipeline).
func TestISOBootHeadlessQ35(t *testing.T) {
	qemuAvailable(t)
	requireXorrisofs(t)

	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)

	isoDir := t.TempDir()
	isoPath := filepath.Join(isoDir, "test-booty.iso")

	buildCAPRFStyleISO(t, kernel, initramfs, isoPath)

	// Launch QEMU with a configuration matching the kindmetal VMs:
	// q35, no VGA, serial on stdio, SCSI CD-ROM.
	args := []string{
		"-machine", "q35,usb=off",
		"-m", "512",
		"-display", "none",
		"-nodefaults",
		"-no-reboot",
		// Serial port on stdio so we capture ISOLINUX + kernel output.
		"-chardev", "stdio,id=charserial0",
		"-device", "isa-serial,chardev=charserial0,id=serial0",
		// SCSI controller + CD-ROM at bootindex=1 (matching kindmetal).
		"-device", "virtio-scsi-pci,id=scsi0",
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=cd0,readonly=on", isoPath),
		"-device", "scsi-cd,drive=cd0,bootindex=1",
	}
	args = append(args, splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))...)

	out := runQEMUSmoke(t, args, 3*time.Minute, "iso-boot-headless-q35", true)

	// Dump artifacts for debugging.
	t.Logf("ISO boot QEMU output (last 2000 bytes):\n%s", tail(out, 2000))
	outStr := string(out)

	// Assert ISOLINUX loaded (its banner appears on serial when SERIAL is set).
	if !strings.Contains(outStr, "ISOLINUX") && !strings.Contains(outStr, "SYSLINUX") {
		t.Fatal("ISOLINUX/SYSLINUX banner not found in serial output — ISO boot failed")
	}

	// Assert the kernel started — earlycon output must appear.
	if !strings.Contains(outStr, "Linux version") {
		t.Fatal("'Linux version' not found in serial output — kernel did not boot")
	}
	t.Logf("Kernel boot confirmed via 'Linux version' string")
}

// TestISOBootHeadlessQ35NoSerial is an observational test that checks whether
// ISOLINUX outputs anything on serial WITHOUT the SERIAL directive on a headless
// VM.  On most QEMU versions ISOLINUX hangs, confirming the CAPRF E2E root cause.
// The test is informational — it does not fail because behavior is QEMU-version-
// dependent.  The positive test (TestISOBootHeadlessQ35) is the strict validation.
func TestISOBootHeadlessQ35NoSerial(t *testing.T) {
	if os.Getenv("KVM_NOSERIAL_TEST") == "" {
		failOrSkipUnsupportedHost(t, "KVM_NOSERIAL_TEST not set for diagnostic no-serial ISO boot test")
	}
	qemuAvailable(t)
	requireXorrisofs(t)

	initramfs := envOrDefault("BOOTY_INITRAMFS", "test-initramfs.cpio.gz")
	kernel := envOrDefault("BOOTY_KERNEL", "vmlinuz")
	requireKVMAssets(t, initramfs, kernel)

	isoDir := t.TempDir()
	isoPath := filepath.Join(isoDir, "test-booty-noserial.iso")

	buildCAPRFStyleISONoSerial(t, kernel, initramfs, isoPath)

	args := []string{
		"-machine", "q35,usb=off",
		"-m", "512",
		"-display", "none",
		"-nodefaults",
		"-no-reboot",
		"-chardev", "stdio,id=charserial0",
		"-device", "isa-serial,chardev=charserial0,id=serial0",
		"-device", "virtio-scsi-pci,id=scsi0",
		"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=cd0,readonly=on", isoPath),
		"-device", "scsi-cd,drive=cd0,bootindex=1",
	}
	args = append(args, splitExtraArgs(envOrDefault("QEMU_EXTRA_ARGS", ""))...)

	// Short timeout — ISOLINUX should hang, so we expect no BOOTy marker.
	// Run QEMU directly instead of runQEMUSmoke — this test is informational,
	// so early QEMU exits (e.g., "no bootable device") should be logged, not fatal.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != context.DeadlineExceeded {
		t.Logf("INFO: QEMU iso-boot-noserial exited early: %v (informational — not a failure)", err)
	}

	outStr := string(out)
	if strings.Contains(outStr, bootyStartMarker) {
		// On some QEMU versions ISOLINUX can boot on a headless VM
		// even without SERIAL.  Log as informational — the positive
		// test (TestISOBootHeadlessQ35) is the important validation.
		t.Logf("INFO: ISOLINUX booted without SERIAL on this QEMU version — headless hang is QEMU-version-dependent")
	} else {
		t.Logf("Confirmed: no ISOLINUX output without SERIAL on headless VM (last 500 bytes):\n%s", tail(out, 500))
	}
}

// requireXorrisofs skips the test if xorrisofs is not available.
func requireXorrisofs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("xorrisofs"); err != nil {
		failOrSkipUnsupportedHost(t, "xorrisofs not available for ISO boot test")
	}
}

// requireISOLINUX returns paths to isolinux.bin and ldlinux.c32, or skips.
func requireISOLINUX(t *testing.T) (string, string) {
	t.Helper()
	isolinuxBin := envOrDefault("ISOLINUX_BIN", "/usr/lib/ISOLINUX/isolinux.bin")
	ldlinuxC32 := envOrDefault("LDLINUX_C32", "/usr/lib/syslinux/modules/bios/ldlinux.c32")

	if _, err := os.Stat(isolinuxBin); err != nil {
		failOrSkipUnsupportedHost(t, "isolinux.bin not found at %s for ISO boot test", isolinuxBin)
	}
	if _, err := os.Stat(ldlinuxC32); err != nil {
		failOrSkipUnsupportedHost(t, "ldlinux.c32 not found at %s for ISO boot test", ldlinuxC32)
	}
	return isolinuxBin, ldlinuxC32
}

// buildCAPRFStyleISO assembles an ISO using the same xorrisofs command that
// CAPRF uses — El Torito boot (no -isohybrid-mbr), with the SERIAL directive
// in isolinux.cfg.
func buildCAPRFStyleISO(t *testing.T, kernel, initramfs, isoPath string) {
	t.Helper()
	isoRoot := buildISORoot(t, kernel, initramfs, true)
	runXorrisofs(t, isoRoot, isoPath)
}

// buildCAPRFStyleISONoSerial is the same as buildCAPRFStyleISO but omits the
// SERIAL directive — used to confirm the hang on headless VMs.
func buildCAPRFStyleISONoSerial(t *testing.T, kernel, initramfs, isoPath string) {
	t.Helper()
	isoRoot := buildISORoot(t, kernel, initramfs, false)
	runXorrisofs(t, isoRoot, isoPath)
}

// buildISORoot creates the ISO directory structure matching CAPRF's layout.
// When withSerial is true, the ISOLINUX config includes SERIAL 0 115200 (as
// CAPRF does for headless VMs).  The ISO includes a deploy.cpio with a minimal
// /deploy/vars stub to match the real initrd chain:
//
//	initrd=/boot/initrd.img,/boot/deploy.cpio
func buildISORoot(t *testing.T, kernel, initramfs string, withSerial bool) string {
	t.Helper()

	isolinuxBin, ldlinuxC32 := requireISOLINUX(t)

	isoRoot := filepath.Join(t.TempDir(), "iso-root")
	for _, dir := range []string{
		filepath.Join(isoRoot, "boot"),
		filepath.Join(isoRoot, "isolinux"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Copy kernel and initramfs.
	copyFile(t, kernel, filepath.Join(isoRoot, "boot", "vmlinuz"))
	copyFile(t, initramfs, filepath.Join(isoRoot, "boot", "initrd.img"))

	// Copy ISOLINUX files.
	copyFile(t, isolinuxBin, filepath.Join(isoRoot, "isolinux", "isolinux.bin"))
	copyFile(t, ldlinuxC32, filepath.Join(isoRoot, "isolinux", "ldlinux.c32"))

	// Dummy efiboot.img — this test only validates BIOS/ISOLINUX boot.
	// The image is required by xorrisofs for the EFI El Torito stanza but
	// is not actually booted.  A real efiboot.img would be needed for UEFI
	// boot testing.
	if err := os.WriteFile(filepath.Join(isoRoot, "efiboot.img"), make([]byte, 1024), 0644); err != nil {
		t.Fatalf("write efiboot.img: %v", err)
	}

	// Build a stub deploy.cpio containing /deploy/vars, matching CAPRF's
	// actual ISO structure where deploy.cpio is loaded as a supplementary
	// initrd alongside the main initramfs.
	buildStubDeployCpio(t, filepath.Join(isoRoot, "boot", "deploy.cpio"))

	// Write isolinux.cfg — with or without SERIAL directive.
	// The initrd line uses a comma-separated list to load both the main
	// initramfs and the deploy cpio overlay, matching CAPRF's template.
	var cfg string
	if withSerial {
		cfg = `SERIAL 0 115200
PROMPT 0
TIMEOUT 1
DEFAULT booty
LABEL booty
  KERNEL /boot/vmlinuz
  APPEND initrd=/boot/initrd.img,/boot/deploy.cpio console=tty0 console=ttyS0,115200n8 earlycon=uart8250,io,0x3f8,115200n8 panic=1 ` + noKHO + `
`
	} else {
		cfg = `PROMPT 0
TIMEOUT 1
DEFAULT booty
LABEL booty
  KERNEL /boot/vmlinuz
  APPEND initrd=/boot/initrd.img,/boot/deploy.cpio console=tty0 console=ttyS0,115200n8 earlycon=uart8250,io,0x3f8,115200n8 panic=1 ` + noKHO + `
`
	}
	if err := os.WriteFile(filepath.Join(isoRoot, "isolinux", "isolinux.cfg"), []byte(cfg), 0644); err != nil {
		t.Fatalf("write isolinux.cfg: %v", err)
	}

	t.Logf("ISO root contents:")
	_ = filepath.Walk(isoRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(isoRoot, p)
		t.Logf("  %s (%d bytes)", rel, info.Size())
		return nil
	})

	return isoRoot
}

// runXorrisofs builds the ISO using the same command as CAPRF's genISOCommand
// (no -isohybrid-mbr, El Torito only).
func runXorrisofs(t *testing.T, isoRoot, isoPath string) {
	t.Helper()

	args := []string{
		"-input-charset", "utf-8",
		"-rational-rock",
		"-volid", "caas-deploy-image",
		"-cache-inodes",
		"-joliet",
		"-full-iso9660-filenames",
		"-output", isoPath,
		"-eltorito-catalog", "boot.cat",
		"-eltorito-boot", "isolinux/isolinux.bin",
		"-no-emul-boot",
		"-boot-load-size", "4",
		"-boot-info-table",
		"-eltorito-alt-boot",
		"-eltorito-platform", "efi",
		"-eltorito-boot", "efiboot.img",
		"-no-emul-boot",
		isoRoot,
	}

	cmd := exec.Command("xorrisofs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xorrisofs failed: %v\nOutput:\n%s", err, out)
	}

	fi, err := os.Stat(isoPath)
	if err != nil {
		t.Fatalf("ISO not created: %v", err)
	}
	t.Logf("Built ISO: %s (%d bytes)", isoPath, fi.Size())
}

// copyFile streams src to dst without buffering the entire file in memory.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	sf, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer sf.Close()
	df, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer df.Close()
	if _, err := io.Copy(df, sf); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

// buildStubDeployCpio creates a minimal newc-format cpio archive containing
// /deploy/vars (matching CAPRF's ISO structure).  The kernel overlays this
// on top of the main initramfs when loaded as a comma-separated initrd entry.
func buildStubDeployCpio(t *testing.T, cpioPath string) {
	t.Helper()

	if _, err := exec.LookPath("cpio"); err != nil {
		t.Fatal("cpio not available")
	}
	if _, err := exec.LookPath("find"); err != nil {
		t.Fatal("find not available")
	}

	deployDir := filepath.Join(t.TempDir(), "deploy-root", "deploy")
	if err := os.MkdirAll(deployDir, 0755); err != nil {
		t.Fatalf("mkdir deploy: %v", err)
	}

	// Keep /deploy/vars minimal so this test focuses on ISO + serial boot path.
	// The headless QEMU config intentionally has no NIC, so BOOTYURL is omitted.
	vars := "MODE=dry-run\n"
	if err := os.WriteFile(filepath.Join(deployDir, "vars"), []byte(vars), 0644); err != nil {
		t.Fatalf("write vars: %v", err)
	}

	// Build the cpio using find | cpio (same approach as CAPRF's buildDeployCpio).
	// Paths are passed as separate arguments to avoid shell injection.
	cpioRoot := filepath.Dir(deployDir)
	findCmd := exec.Command("find", ".", "-print0")
	findCmd.Dir = cpioRoot
	cpioCmd := exec.Command("cpio", "--null", "-o", "--format=newc")
	cpioCmd.Dir = cpioRoot

	var cpioErr strings.Builder
	cpioCmd.Stderr = &cpioErr

	pipe, err := findCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cpioCmd.Stdin = pipe

	outFile, err := os.Create(cpioPath)
	if err != nil {
		t.Fatalf("create %s: %v", cpioPath, err)
	}
	defer outFile.Close()
	cpioCmd.Stdout = outFile

	if err := findCmd.Start(); err != nil {
		t.Fatalf("find start: %v", err)
	}
	if err := cpioCmd.Start(); err != nil {
		t.Fatalf("cpio start: %v", err)
	}
	// Wait on cpio first so its errors are reported before find's SIGPIPE.
	if err := cpioCmd.Wait(); err != nil {
		t.Fatalf("cpio: %v\n%s", err, cpioErr.String())
	}
	// find may fail with SIGPIPE if cpio closes early — tolerate it.
	if err := findCmd.Wait(); err != nil {
		t.Logf("find exited with: %v (non-fatal after cpio success)", err)
	}

	fi, err := os.Stat(cpioPath)
	if err != nil {
		t.Fatalf("deploy.cpio not created: %v", err)
	}
	t.Logf("Built deploy.cpio: %s (%d bytes)", cpioPath, fi.Size())
}
