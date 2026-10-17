//go:build e2e

package kvm

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	oobNetworkFailedLine = regexp.MustCompile(`BOOTY1 run=[0-9a-f]{8} phase=network code=BOOTY-NET-002 step=network-setup result=failed`)
	oobNetworkPhaseLine  = regexp.MustCompile(`BOOTY1 run=[0-9a-f]{8} phase=network code=- step=- result=running`)
	oobPreviousResult    = regexp.MustCompile(`previous BOOTy run result"? mode=provision phase=network step=network-setup code=BOOTY-NET-002 result=failed`)
	oobPreviousDeprov    = regexp.MustCompile(`previous BOOTy run result"? mode=deprovision phase=network step=network-setup code=BOOTY-NET-002 result=failed`)
)

// TestOOBStatusNetworkFailureQEMU forces a network failure (no wait round)
// under OVMF with a persistent NVRAM file. The first boot writes the BOOTY1
// failure line to the serial console and the BootyLastResult variable. The
// second boot, a reboot with the same NVRAM, reads and logs the stored
// result, fails the same way and does not write the unchanged variable. The
// third boot ends with another result (deprovision mode): efivarfs made the
// variable immutable, and BOOTy must still rewrite it. The fourth boot logs
// the new content.
func TestOOBStatusNetworkFailureQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	ovmfCode, ovmfVars := uefiFirmware(t)

	caprf := startFakeCAPRF(t, "")
	targetDisk := filepath.Join(t.TempDir(), "oob-status.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	vars := caprf.nativeVars("provision")
	vars["IMAGE"] = caprf.baseURL + "/missing.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["NETWORK_WAIT_MODE"] = "rounds"
	vars["NETWORK_WAIT_MAX_ATTEMPTS"] = "0"
	vars["OOB_EFI_VAR"] = "true"
	initramfs := buildProvisionInitramfs(t, vars)

	out := runQEMUUEFI(t, ovmfCode, ovmfVars, initramfs, targetDisk, "", 3*time.Minute)
	text := string(out)
	for _, re := range []*regexp.Regexp{oobNetworkPhaseLine, oobNetworkFailedLine} {
		if !re.MatchString(text) {
			t.Fatalf("console has no line matching %q. tail:\n%s", re, tail(out, 4000))
		}
	}
	if !strings.Contains(text, "no BootyLastResult UEFI variable from an earlier run") {
		t.Fatalf("first boot found a stored result in a fresh NVRAM. tail:\n%s", tail(out, 4000))
	}
	if !strings.Contains(text, "wrote the BootyLastResult UEFI variable") {
		t.Fatalf("first boot did not write the BootyLastResult variable. tail:\n%s", tail(out, 4000))
	}

	out = runQEMUUEFI(t, ovmfCode, ovmfVars, initramfs, targetDisk, "", 3*time.Minute)
	text = string(out)
	if !oobPreviousResult.MatchString(text) {
		t.Fatalf("second boot did not log the stored result. tail:\n%s", tail(out, 4000))
	}
	if !oobNetworkFailedLine.MatchString(text) {
		t.Fatalf("second boot has no failure line. tail:\n%s", tail(out, 4000))
	}
	if strings.Contains(text, "wrote the BootyLastResult UEFI variable") {
		t.Fatalf("second boot wrote an unchanged result to NVRAM. tail:\n%s", tail(out, 4000))
	}

	deprovision := caprf.nativeVars("deprovision")
	deprovision["NETWORK_WAIT_MODE"] = "rounds"
	deprovision["NETWORK_WAIT_MAX_ATTEMPTS"] = "0"
	deprovision["OOB_EFI_VAR"] = "true"
	deprovisionInitramfs := buildProvisionInitramfs(t, deprovision)
	out = runQEMUUEFI(t, ovmfCode, ovmfVars, deprovisionInitramfs, targetDisk, "", 3*time.Minute)
	text = string(out)
	if !oobPreviousResult.MatchString(text) || !oobNetworkFailedLine.MatchString(text) {
		t.Fatalf("third boot did not log the stored result or the failure line. tail:\n%s", tail(out, 4000))
	}
	if strings.Contains(text, "writing the BootyLastResult UEFI variable failed") ||
		!strings.Contains(text, "wrote the BootyLastResult UEFI variable") {
		t.Fatalf("third boot did not rewrite the changed result. tail:\n%s", tail(out, 4000))
	}

	out = runQEMUUEFI(t, ovmfCode, ovmfVars, deprovisionInitramfs, targetDisk, "", 3*time.Minute)
	text = string(out)
	if !oobPreviousDeprov.MatchString(text) {
		t.Fatalf("fourth boot did not log the rewritten result. tail:\n%s", tail(out, 4000))
	}
	if strings.Contains(text, "wrote the BootyLastResult UEFI variable") {
		t.Fatalf("fourth boot wrote an unchanged result to NVRAM. tail:\n%s", tail(out, 4000))
	}
}
