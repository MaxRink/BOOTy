//go:build e2e

package kvm

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Legacy evidence: the legacy ramdisk runs `netplan apply` on the provisioner
// netplan files. CAPRF e2e (test/e2e/lib.sh) gives the old deployer
// `match` with `set-name: hbn`. The run only reaches CAPRF when BOOTy renames
// the NIC and puts the static address on the renamed NIC.
func TestLegacyCompatNetplanSetNameQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))
	targetDisk := filepath.Join(t.TempDir(), "legacy-netplan-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	for _, key := range []string{"STATIC_IP", "STATIC_GATEWAY", "STATIC_IFACE"} {
		delete(vars, key)
	}
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	files := map[string]string{
		"deploy/file-system/etc/netplan/10-provision.yaml": `network:
  version: 2
  ethernets:
    prov0:
      match:
        macaddress: "52:54:00:12:34:56"
      set-name: prov0
      addresses: [10.0.2.15/24]
      routes:
      - to: default
        via: 10.0.2.2
`,
	}
	output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfsWithFiles(t, vars, files), targetDisk, 6*time.Minute)
	t.Logf("legacy netplan set-name output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	if !strings.Contains(string(output), "renamed NIC like netplan set-name") || !strings.Contains(string(output), "to=prov0") {
		t.Fatalf("BOOTy did not rename the NIC to prov0. tail:\n%s", tail(output, 4000))
	}
	// QEMU user networking also answers DHCP, so the run would succeed
	// after a DHCP fallback too: the static address must be on prov0.
	if !regexp.MustCompile(`msg="static IP configured".* iface=prov0 address=10\.0\.2\.15/24`).Match(output) ||
		strings.Contains(string(output), "falling back to DHCP") {
		t.Fatalf("BOOTy did not configure the static address on prov0. tail:\n%s", tail(output, 4000))
	}
}
