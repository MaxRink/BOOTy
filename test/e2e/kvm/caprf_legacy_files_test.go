//go:build e2e

package kvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Legacy evidence: common.sh copy_files copies deploy/file-system into the
// ramdisk root before wait_for_network, so files such as /etc/hosts or
// /etc/resolv.conf change how the ramdisk reaches CAPRF. They never reach
// the installed OS.
func TestLegacyCompatProvisionerFilesStayInRamdiskQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "legacy-files-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	// The callback and image URLs use a name that only the provisioner
	// /etc/hosts resolves. The run only succeeds when BOOTy applies the
	// provisioner files to the ramdisk before it starts the network.
	byName := strings.Replace(caprf.baseURL, "10.0.2.2", "caprf.legacy.test", 1)
	vars := caprf.vars("provision")
	for _, key := range []string{"LOG_URL", "INIT_URL", "ERROR_URL", "SUCCESS_URL"} {
		vars[key] = strings.Replace(vars[key], caprf.baseURL, byName, 1)
	}
	vars["IMAGE"] = byName + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	files := map[string]string{
		"deploy/file-system/etc/hosts":                "127.0.0.1 localhost\n10.0.2.2 caprf.legacy.test\n",
		"deploy/file-system/etc/booty-legacy-marker":  "ramdisk only\n",
		"deploy/file-system/etc/frr/frr.conf.example": "! not parsed\n",
	}
	initramfs := buildProvisionInitramfsWithFiles(t, vars, files)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("legacy provisioner files output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()
	for _, rel := range []string{"etc/booty-legacy-marker", "etc/hosts", "etc/frr/frr.conf.example"} {
		_, err := os.Stat(filepath.Join(rootMount, rel))
		switch {
		case err == nil:
			t.Errorf("provisioner file %s reached the installed OS", rel)
		case !os.IsNotExist(err):
			t.Errorf("cannot check %s on the installed OS: %v", rel, err)
		}
	}
}
