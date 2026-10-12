//go:build e2e

package kvm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	rescueRetryHostname = "rescue-retry"
	rescueRetryOKDir    = "var/lib/booty-retry-ok"
)

// failOnceScript fails the first post-provision run and succeeds on every
// later one. The marker lives in the ramdisk /run, which BOOTy bind mounts
// into the chroot, so it survives the re-image of the target disk.
const failOnceScript = `#!/bin/sh
if [ -e /run/booty-e2e-failed-once ]; then
  mkdir -p /` + rescueRetryOKDir + `
  exit 0
fi
mkdir -p /run/booty-e2e-failed-once
echo "injected first-attempt failure"
exit 1
`

// TestRescueRetryReleasesStaleMountsQEMU fails the first provisioning attempt
// in a post-provision command, after the target root and its chroot bind
// mounts are mounted. With RESCUE_MODE=retry, the second attempt must unmount
// what the first attempt left before it wipes and re-images the disk, and
// must then succeed. When the mounts stay, e2fsck aborts on the mounted
// root and every retry fails.
func TestRescueRetryReleasesStaleMountsQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	installFailOnceCommand(t, rawDisk)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "rescue-retry-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["HOSTNAME"] = rescueRetryHostname
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["RESCUE_MODE"] = "retry"
	vars["POST_PROVISION_CMDS"] = "/usr/sbin/booty-fail-once"
	initramfs := buildProvisionInitramfs(t, vars)

	// Two full attempts plus the 30 s default retry delay.
	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 10*time.Minute)
	out := string(output)
	t.Logf("rescue retry output tail:\n%s", tail(output, 4000))

	for _, want := range []string{
		"injected first-attempt failure",
		"retrying provisioning",
		"target root still mounted from an earlier attempt",
		"provisioning step\" component=provision step=report-success",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q. tail:\n%s", want, tail(output, 4000))
		}
	}
	requireStatusesEndWithSuccessAfterError(t, caprf)

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()
	hostname, err := os.ReadFile(filepath.Join(rootMount, "etc", "hostname"))
	if err != nil || strings.TrimSpace(string(hostname)) != rescueRetryHostname {
		t.Fatalf("/etc/hostname = %q, %v, want %q", hostname, err, rescueRetryHostname)
	}
	if _, err := os.Stat(filepath.Join(rootMount, rescueRetryOKDir)); err != nil {
		t.Fatalf("second attempt marker missing from the provisioned root: %v", err)
	}
}

func requireStatusesEndWithSuccessAfterError(t *testing.T, caprf *fakeCAPRF) {
	t.Helper()
	caprf.mu.Lock()
	defer caprf.mu.Unlock()
	got := strings.Join(caprf.statuses, ",")
	if !strings.Contains(got, "error") || !strings.HasSuffix(got, "success") {
		t.Fatalf("CAPRF status callbacks = %v, want an error and then success last", caprf.statuses)
	}
}

// installFailOnceCommand writes failOnceScript to /usr/sbin/booty-fail-once
// in the image root.
func installFailOnceCommand(t *testing.T, rawImage string) {
	t.Helper()

	loopDev := strings.TrimSpace(string(runOutput(t, "setup fail-once image loop device",
		"losetup", "--find", "--show", "--partscan", rawImage)))
	defer func() { _ = exec.Command("losetup", "-d", loopDev).Run() }()
	rootDev := loopDev + "p2"
	waitForDevice(t, rootDev, 5*time.Second)

	root := filepath.Join(t.TempDir(), "rootmnt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir fail-once image mount: %v", err)
	}
	mountWithRetry(t, "mount fail-once image root", rootDev, root)
	defer func() { _ = exec.Command("umount", root).Run() }()

	path := filepath.Join(root, "usr", "sbin", "booty-fail-once")
	writeFile(t, path, failOnceScript)
	run(t, "chmod fail-once fixture", "chmod", "+x", path)
	run(t, "unmount fail-once image root", "umount", root)
}
