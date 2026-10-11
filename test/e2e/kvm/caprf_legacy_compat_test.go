//go:build e2e

package kvm

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file prove that CAPRF_LEGACY_COMPAT=true makes BOOTy
// behave like the CAPRF legacy shell provisioner
// (cluster-api-provider-redfish internal/provision/files/*.sh).

const fakeCAPRFToken = "legacy-compat-token"

// fakeCAPRF is a minimal CAPRF provision server. It serves the machine image
// and records the status callbacks that the legacy scripts send.
type fakeCAPRF struct {
	baseURL string

	mu       sync.Mutex
	statuses []string
	messages map[string]string
}

func startFakeCAPRF(t *testing.T, imagePath string) *fakeCAPRF {
	t.Helper()
	f := &fakeCAPRF{messages: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/image.gz", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, imagePath)
	})
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeCAPRFToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost {
			status := strings.TrimPrefix(r.URL.Path, "/status/")
			f.mu.Lock()
			f.statuses = append(f.statuses, status)
			f.messages[status] = string(body)
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	// Listen on all interfaces so the QEMU guest reaches us via 10.0.2.2.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	f.baseURL = fmt.Sprintf("http://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)
	return f
}

// vars returns the /deploy/vars entries that the CAPRF ramdisk builder writes
// for every task, plus the legacy compat switch.
func (f *fakeCAPRF) vars(mode string) map[string]string {
	return map[string]string{
		"HOSTNAME":            "legacy-compat",
		"TOKEN":               fakeCAPRFToken,
		"MODE":                mode,
		"LOG_URL":             f.baseURL + "/log",
		"INIT_URL":            f.baseURL + "/status/init",
		"ERROR_URL":           f.baseURL + "/status/error",
		"SUCCESS_URL":         f.baseURL + "/status/success",
		"INSECURE_TRANSPORT":  "true",
		"CAPRF_LEGACY_COMPAT": "true",
		"STATIC_IP":           "10.0.2.15/24",
		"STATIC_GATEWAY":      "10.0.2.2",
		"STATIC_IFACE":        "eth0",
	}
}

func (f *fakeCAPRF) requireStatuses(t *testing.T, want ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.statuses, ",") != strings.Join(want, ",") {
		t.Fatalf("CAPRF status callbacks = %v, want %v (messages %v)", f.statuses, want, f.messages)
	}
}

// requirePowerOff checks the kernel message that the legacy scripts cause with
// `shutdown now` and `poweroff -f`. A reboot prints "Restarting system".
func requirePowerOff(t *testing.T, output []byte) {
	t.Helper()
	text := string(output)
	if !strings.Contains(text, "reboot: Power down") {
		t.Fatalf("guest did not power off. tail:\n%s", tail(output, 4000))
	}
	if strings.Contains(text, "reboot: Restarting system") {
		t.Fatalf("guest rebooted instead of powering off. tail:\n%s", tail(output, 4000))
	}
}

// Legacy evidence: provision.sh ends with status_success and `shutdown now`.
// CAPRF then waits for power off, ejects the ISO and powers the host on.
func TestLegacyCompatProvisionPowersOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))

	targetDisk := filepath.Join(t.TempDir(), "legacy-compat-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.vars("provision")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("legacy compat provision output tail:\n%s", tail(output, 3000))

	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, output)
	if strings.Contains(string(output), "attempting kexec") {
		t.Fatalf("legacy compat must not kexec into the installed kernel. tail:\n%s", tail(output, 4000))
	}
}

// Legacy evidence: deprovision.sh ends with status_success, `sync` and
// `poweroff -f`.
func TestLegacyCompatDeprovisionPowersOffQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caprf := startFakeCAPRF(t, "")

	targetDisk := filepath.Join(t.TempDir(), "legacy-compat-deprovision.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	initramfs := buildProvisionInitramfs(t, caprf.vars("deprovision"))

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 4*time.Minute)
	t.Logf("legacy compat deprovision output tail:\n%s", tail(output, 3000))

	caprf.requireStatuses(t, "init", "success")
	requirePowerOff(t, output)
}
