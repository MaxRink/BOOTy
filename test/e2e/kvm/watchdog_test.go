//go:build e2e

package kvm

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// The guest gets an Intel 6300ESB watchdog. With -no-reboot a watchdog
// reset ends QEMU, like a reboot, but without the kernel "reboot:" message.
const watchdogQEMUArgs = "-device i6300esb -action watchdog=reset"

// TestWatchdogHungStepResetsQEMU hangs a machine command. The step makes no
// progress, BOOTy stops petting the i6300esb, and the watchdog resets the
// guest without a reboot or power off by BOOTy.
func TestWatchdogHungStepResetsQEMU(t *testing.T) {
	output, caprf := runWatchdogProvision(t, map[string]string{
		"WATCHDOG_ENABLE":       "true",
		"WATCHDOG_TIMEOUT_SEC":  "60",
		"WATCHDOG_STEP_BUDGETS": `{"run-machine-commands":15}`,
		"MACHINE_COMMANDS_MODE": "script",
	}, map[string]string{
		"deploy/machine-commands/commands-0.sh": "#!/bin/sh\n/bin/busybox sleep 3600\n",
	})
	text := string(output)
	requireWatchdogArmed(t, output)
	if !strings.Contains(text, "BOOTY-WDT-001") || !strings.Contains(text, "step=run-machine-commands") {
		t.Fatalf("no stall of run-machine-commands (BOOTY-WDT-001) in the output. tail:\n%s", tail(output, 4000))
	}
	for _, bad := range []string{"reboot: Restarting system", "reboot: Power down", "disarming the watchdog"} {
		if strings.Contains(text, bad) {
			t.Fatalf("output contains %q: BOOTy ended the run, not the watchdog. tail:\n%s", bad, tail(output, 4000))
		}
	}
	caprf.requireStatuses(t, "init")
}

// TestWatchdogCleanRunDisarmsQEMU provisions with the watchdog armed. The
// run lasts longer than the timeout, so BOOTy must pet it, and the power
// off disarms it with the magic close.
func TestWatchdogCleanRunDisarmsQEMU(t *testing.T) {
	output, caprf := runWatchdogProvision(t, map[string]string{
		"WATCHDOG_ENABLE":      "true",
		"WATCHDOG_TIMEOUT_SEC": "60",
	}, nil)
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	requireWatchdogArmed(t, output)
	requirePowerOff(t, output)
	text := string(output)
	if !strings.Contains(text, "disarming the watchdog") {
		t.Fatalf("BOOTy did not disarm the watchdog. tail:\n%s", tail(output, 4000))
	}
	// The watchdog core prints this when the device closes without the
	// magic character.
	if strings.Contains(text, "watchdog did not stop") {
		t.Fatalf("the magic close did not stop the watchdog. tail:\n%s", tail(output, 4000))
	}
}

func runWatchdogProvision(t *testing.T, extraVars, extraFiles map[string]string) ([]byte, *fakeCAPRF) {
	t.Helper()
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	files := watchdogModuleFiles(t)
	for k, v := range extraFiles {
		files[k] = v
	}
	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))
	targetDisk := filepath.Join(t.TempDir(), "watchdog-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.nativeVars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["SUCCESS_ACTION"] = "poweroff"
	for k, v := range extraVars {
		vars[k] = v
	}
	initramfs := buildProvisionInitramfsWithFiles(t, vars, files)

	t.Setenv("QEMU_EXTRA_ARGS", strings.TrimSpace(os.Getenv("QEMU_EXTRA_ARGS")+" "+watchdogQEMUArgs))
	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 8*time.Minute)
	t.Logf("watchdog output tail:\n%s", tail(output, 3000))
	return output, caprf
}

func requireWatchdogArmed(t *testing.T, output []byte) {
	t.Helper()
	if !strings.Contains(string(output), "hardware watchdog armed") {
		t.Fatalf("BOOTy did not arm the watchdog. tail:\n%s", tail(output, 4000))
	}
}

// watchdogModuleFiles returns the i6300esb driver and its dependencies of
// the host kernel, uncompressed, under modules/watchdog: BOOTy loads them
// with finit_module, which takes plain modules.
func watchdogModuleFiles(t *testing.T) map[string]string {
	t.Helper()
	release := kernelRelease()
	files := map[string]string{}
	var add func(module string, visiting map[string]bool)
	add = func(module string, visiting map[string]bool) {
		if visiting[module] {
			return
		}
		visiting[module] = true
		for _, dep := range kernelModuleDependencies(t, release, module) {
			add(dep, visiting)
		}
		path := kernelModulePath(t, release, module)
		if path == "" {
			return // built in
		}
		files["modules/watchdog/"+module+".ko"] = string(readKernelModule(t, path))
	}
	add("i6300esb", map[string]bool{})
	if _, ok := files["modules/watchdog/i6300esb.ko"]; !ok && !builtinModule(release, "i6300esb") {
		failOrSkipUnsupportedHost(t, "the host kernel %s has no i6300esb watchdog driver", release)
	}
	return files
}

func builtinModule(release, module string) bool {
	data, err := os.ReadFile(filepath.Join("/lib/modules", release, "modules.builtin"))
	return err == nil && bytes.Contains(data, []byte("/"+module+".ko"))
}

// readKernelModule reads a module file and removes the zstd, xz or gzip
// compression of distribution kernels.
func readKernelModule(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open module %s: %v", path, err)
	}
	defer f.Close()
	var r io.Reader = f
	switch {
	case strings.HasSuffix(path, ".zst"):
		dec, err := zstd.NewReader(f)
		if err != nil {
			t.Fatalf("zstd %s: %v", path, err)
		}
		defer dec.Close()
		r = dec
	case strings.HasSuffix(path, ".xz"):
		dec, err := xz.NewReader(f)
		if err != nil {
			t.Fatalf("xz %s: %v", path, err)
		}
		r = dec
	case strings.HasSuffix(path, ".gz"):
		dec, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("gzip %s: %v", path, err)
		}
		defer dec.Close()
		r = dec
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read module %s: %v", path, err)
	}
	return data
}
