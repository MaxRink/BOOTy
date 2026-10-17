//go:build e2e

package kvm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SECURE_ERASE=true erases the target disk with the method of its media type
// and never falls back to a wipe on its own. QEMU NVMe supports Format NVM
// (it zeroes the namespace); the QEMU AHCI disk has no ATA security feature
// set, so its secure erase fails unless SECURE_ERASE_FALLBACK=wipe.
func TestDiskWipeSecureEraseQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	for _, tool := range []string{"nvme", "hdparm"} {
		if _, err := exec.LookPath(tool); err != nil {
			failOrSkipUnsupportedHost(t, "%s not available for the secure erase test", tool)
		}
	}
	image := compressGzip(t, createChrootCapableTestDiskImage(t, 512))
	// BOOTy loads the flat /modules directory; the runner kernel has AHCI
	// as a module.
	ahci := flatModuleFiles(t, "ahci")

	t.Run("nvme format", func(t *testing.T) {
		disk := filepath.Join(t.TempDir(), "nvme.raw")
		run(t, "create NVMe disk", "truncate", "-s", "2G", disk)
		// A marker in the last MiB: the image does not reach it, the format does.
		marker := []byte("BOOTY-SECURE-ERASE-MARKER")
		const markerOff = 2<<30 - 1<<20
		writeAt(t, disk, markerOff, marker)

		caprf, output := runSecureEraseQEMU(t, image, nil, map[string]string{
			"DISK_DEVICE": "/dev/nvme0n1",
			// quick: no blkdiscard, so only the format can clear the marker.
			"WIPE_DEPTH": "quick",
		}, "-drive", "file="+disk+",format=raw,if=none,id=nv0", "-device", "nvme,serial=booty-se,drive=nv0")
		assertProvisionSucceeded(t, output)
		caprf.requireStatuses(t, "init", "success")
		requireOutput(t, output, `msg="NVMe secure erase" device=/dev/nvme0n1`)
		f, err := os.Open(disk)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		tailMiB := make([]byte, 1<<20)
		if _, err := f.ReadAt(tailMiB, markerOff); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(tailMiB, marker) {
			t.Fatal("the marker survived the NVMe secure erase")
		}
	})

	sata := func(t *testing.T) []string {
		disk := filepath.Join(t.TempDir(), "sata.raw")
		run(t, "create SATA disk", "truncate", "-s", "2G", disk)
		return []string{
			"-device", "ahci,id=ahci0",
			"-drive", "file=" + disk + ",format=raw,if=none,id=sata0",
			"-device", "ide-hd,drive=sata0,bus=ahci0.0",
		}
	}

	t.Run("ata without security fails", func(t *testing.T) {
		caprf, output := runSecureEraseQEMU(t, image, ahci, map[string]string{"DISK_DEVICE": "/dev/sda"}, sata(t)...)
		caprf.requireStatuses(t, "init", "error")
		requireOutput(t, output, `msg="ATA secure erase" device=/dev/sda`)
		requireOutput(t, output, "secure erase not supported: the drive has no ATA security feature set")
		if strings.Contains(string(output), "wiping disk") {
			t.Fatalf("the failed secure erase wiped the disk. tail:\n%s", tail(output, 4000))
		}
	})

	t.Run("ata without security and SECURE_ERASE_FALLBACK=wipe", func(t *testing.T) {
		caprf, output := runSecureEraseQEMU(t, image, ahci, map[string]string{
			"DISK_DEVICE":           "/dev/sda",
			"SECURE_ERASE_FALLBACK": "wipe",
		}, sata(t)...)
		assertProvisionSucceeded(t, output)
		caprf.requireStatuses(t, "init", "success")
		requireOutput(t, output, `level=WARN msg="secure erase failed, SECURE_ERASE_FALLBACK=wipe`)
		caprf.mu.Lock()
		msg := caprf.messages["success"]
		caprf.mu.Unlock()
		if !strings.Contains(msg, "secure erase not done, wiped instead (SECURE_ERASE_FALLBACK=wipe): /dev/sda") {
			t.Fatalf("success status %q does not name the fallback", msg)
		}
	})
}

// runSecureEraseQEMU provisions image with SECURE_ERASE=true onto the disk
// that diskArgs attach and returns the fake CAPRF and the serial output.
func runSecureEraseQEMU(t *testing.T, image string, files, extra map[string]string, diskArgs ...string) (*fakeCAPRF, []byte) {
	t.Helper()
	caprf := startFakeCAPRF(t, image)
	vars := caprf.nativeVars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["SUCCESS_ACTION"] = "poweroff"
	vars["SECURE_ERASE"] = "true"
	for k, v := range extra {
		vars[k] = v
	}
	args := []string{
		"-m", "1024", "-nographic", "-no-reboot",
		"-kernel", findKernel(t), "-initrd", buildProvisionInitramfsWithFiles(t, vars, files),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56", "-net", "user",
		"-append", "console=ttyS0 panic=1 net.ifnames=0",
	}
	args = append(args, diskArgs...)
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("QEMU timed out. tail:\n%s", tail(output, 4000))
	}
	t.Logf("secure erase run exited (%v). tail:\n%s", err, tail(output, 3000))
	return caprf, output
}

func requireOutput(t *testing.T, output []byte, want string) {
	t.Helper()
	if !strings.Contains(string(output), want) {
		t.Fatalf("serial output has no %q. tail:\n%s", want, tail(output, 4000))
	}
}

func writeAt(t *testing.T, path string, off int64, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, off); err != nil {
		t.Fatal(err)
	}
}

// flatModuleFiles returns module and its dependencies for the guest kernel
// as initramfs files modules/<name>.ko, uncompressed, for the BOOTy module
// loader (finit_module without decompression). Built-in modules are left
// out.
func flatModuleFiles(t *testing.T, module string) map[string]string {
	t.Helper()
	release := os.Getenv("BOOTY_KERNEL_RELEASE")
	if release == "" {
		release = kernelRelease()
	}
	files := map[string]string{}
	var add func(name string)
	add = func(name string) {
		key := "modules/" + strings.ReplaceAll(name, "-", "_") + ".ko"
		if _, done := files[key]; done {
			return
		}
		files[key] = ""
		for _, dep := range kernelModuleDependencies(t, release, name) {
			add(dep)
		}
		path := kernelModulePath(t, release, name)
		if path == "" {
			delete(files, key)
			return
		}
		var data []byte
		var err error
		switch filepath.Ext(path) {
		case ".zst":
			data, err = exec.Command("zstd", "-dc", path).Output()
		case ".xz":
			data, err = exec.Command("xz", "-dc", path).Output()
		default:
			data, err = os.ReadFile(path)
		}
		if err != nil {
			t.Fatalf("read module %s: %v", path, err)
		}
		files[key] = string(data)
	}
	add(module)
	t.Logf("guest modules for %s: %d files", module, len(files))
	return files
}
