//go:build e2e

package kvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Legacy evidence: provision.sh runs every file in deploy/machine-commands
// with run_script_in_chroot. CAPRF writes MachineConfig.Commands[i] to
// commands-<i>.sh, so the commands are full shell scripts with pipes,
// redirects and command substitution.
func TestLegacyCompatMachineCommandsQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "legacy-commands-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	files := map[string]string{
		"deploy/machine-commands/commands-0.sh":  "mkdir -p /var/lib/legacy\nprintf '%s\\n' first >> /var/lib/legacy/order\n",
		"deploy/machine-commands/commands-1.sh":  "printf '%s\\n' \"$(printf second)\" | cat >> /var/lib/legacy/order; printf '%s\\n' done > /var/lib/legacy/second\n",
		"deploy/machine-commands/commands-2.sh":  "printf '%s\\n' third >> /var/lib/legacy/order\n",
		"deploy/machine-commands/commands-10.sh": "#!/bin/bash\nprintf '%s\\n' eleventh >> /var/lib/legacy/order\n",
	}
	initramfs := buildProvisionInitramfsWithFiles(t, vars, files)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("legacy machine commands output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	order := strings.Fields(readProvisionedFile(t, rootMount, "var/lib/legacy/order"))
	if strings.Join(order, ",") != "first,second,third,eleventh" {
		t.Fatalf("machine commands ran in order %v, want first,second,third,eleventh", order)
	}
	if got := strings.TrimSpace(readProvisionedFile(t, rootMount, "var/lib/legacy/second")); got != "done" {
		t.Fatalf("redirect in machine command wrote %q, want done", got)
	}
	if _, err := os.Stat(filepath.Join(rootMount, "tmp", "booty-machine-commands")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(rootMount, "tmp", "booty-machine-commands"))
		if len(entries) > 0 {
			t.Fatalf("staged machine command scripts left in the target root: %d", len(entries))
		}
	}
}
