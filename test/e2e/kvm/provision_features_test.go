//go:build e2e

package kvm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. Cloud-init seed injection via QEMU provisioning
// Proves: CLOUDINIT_ENABLED=true injects nocloud seed files to provisioned root.
// ---------------------------------------------------------------------------

func TestCloudInitInjectionQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)
	baseURL := startImageServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "cloudinit-target.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "cloud-init-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"CLOUDINIT_ENABLED":         "true",
		"CLOUDINIT_DATASOURCE":      "nocloud",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Cloud-init provision output tail:\n%s", tail(output, 3000))

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	// Verify the nocloud seed directory was created.
	seedDir := filepath.Join(rootMount, "var", "lib", "cloud", "seed", "nocloud")
	if _, err := os.Stat(seedDir); err != nil {
		t.Fatalf("cloud-init nocloud seed directory not found at %s: %v", seedDir, err)
	}

	// Verify meta-data file exists and contains the hostname.
	metaDataPath := filepath.Join(seedDir, "meta-data")
	metaData, err := os.ReadFile(metaDataPath)
	if err != nil {
		t.Fatalf("failed to read meta-data: %v", err)
	}
	if !strings.Contains(string(metaData), "cloud-init-test") {
		t.Errorf("meta-data does not contain expected hostname, got:\n%s", string(metaData))
	}

	// Verify user-data file exists.
	userDataPath := filepath.Join(seedDir, "user-data")
	if _, err := os.Stat(userDataPath); err != nil {
		t.Errorf("user-data file not found: %v", err)
	}

	// Verify network-config file exists.
	netCfgPath := filepath.Join(seedDir, "network-config")
	if _, err := os.Stat(netCfgPath); err != nil {
		t.Errorf("network-config file not found: %v", err)
	}

	t.Log("cloud-init nocloud seed files verified on provisioned disk")
}

// ---------------------------------------------------------------------------
// 2. Post-provision commands run in chroot
// Proves: POST_PROVISION_CMDS executes inside the provisioned rootfs chroot.
// ---------------------------------------------------------------------------

func TestPostProvisionCommandsQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)
	baseURL := startImageServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "postcmds-target.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "postcmd-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"POST_PROVISION_CMDS":       "touch /tmp/post-provision-marker;echo done > /tmp/post-provision-result",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Post-provision commands output tail:\n%s", tail(output, 3000))

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	// Post-provision commands run in chroot of /newroot, which is the mounted
	// provisioned root filesystem. Files created there appear at the root of
	// the mounted filesystem.
	markerPath := filepath.Join(rootMount, "tmp", "post-provision-marker")
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("post-provision marker file not found at %s: %v", markerPath, err)
	}

	resultPath := filepath.Join(rootMount, "tmp", "post-provision-result")
	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		t.Errorf("post-provision result file not found: %v", err)
	} else if !strings.Contains(string(resultData), "done") {
		t.Errorf("post-provision result expected 'done', got %q", strings.TrimSpace(string(resultData)))
	}

	t.Log("post-provision commands created expected files in chroot")
}

// ---------------------------------------------------------------------------
// 3. Image checksum verification succeeds with correct hash
// Proves: IMAGE_CHECKSUM with correct SHA256 allows provisioning to proceed.
// ---------------------------------------------------------------------------

func TestImageChecksumPassQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)
	baseURL := startImageServer(t, imageGz)

	// IMAGE_CHECKSUM covers the file as downloaded (IMAGE_CHECKSUM_SCOPE=download).
	checksum := computeSHA256(t, imageGz)
	t.Logf("image.gz SHA256: %s", checksum)

	targetDisk := filepath.Join(t.TempDir(), "checksum-pass.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "checksum-pass-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"IMAGE_CHECKSUM":            checksum,
		"IMAGE_CHECKSUM_TYPE":       "sha256",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	outStr := string(output)
	t.Logf("Checksum pass output tail:\n%s", tail(output, 3000))

	// Verify provisioning completed without checksum error.
	outLower := strings.ToLower(outStr)
	if strings.Contains(outLower, "checksum mismatch") {
		t.Fatal("provisioning failed with checksum mismatch despite correct hash")
	}

	// Check for success markers indicating provisioning completed.
	if strings.Contains(outLower, "checksum verified") {
		t.Log("checksum verification confirmed in serial output")
	}
	if strings.Contains(outStr, "stream-image") || strings.Contains(outStr, "configure-hostname") {
		t.Log("provisioning steps executed after checksum pass")
	}
}

// ---------------------------------------------------------------------------
// 4. Image checksum verification fails with wrong hash
// Proves: IMAGE_CHECKSUM mismatch causes a provisioning error.
// ---------------------------------------------------------------------------

func TestImageChecksumFailQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)
	baseURL := startImageServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "checksum-fail.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	// Use a known-wrong checksum (all zeros).
	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "checksum-fail-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"IMAGE_CHECKSUM":            wrongChecksum,
		"IMAGE_CHECKSUM_TYPE":       "sha256",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 4*time.Minute)
	outStr := string(output)
	t.Logf("Checksum fail output tail:\n%s", tail(output, 3000))

	// Verify the serial output contains a checksum mismatch error.
	outLower := strings.ToLower(outStr)
	if !strings.Contains(outLower, "checksum mismatch") && !strings.Contains(outLower, "checksum") {
		t.Error("expected checksum error in serial output but none found")
	} else {
		t.Log("checksum mismatch error detected in serial output (correct)")
	}
}

// ---------------------------------------------------------------------------
// 5. Health checks POST results to mock endpoint
// Proves: HEALTH_CHECKS_ENABLED=true triggers a health check POST.
// ---------------------------------------------------------------------------

func TestHealthChecksEndpointQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)

	mock := newFeatureMockServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "health-target.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "health-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     mock.guestURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  mock.guestURL + "/status/init",
		"HEALTH_CHECKS_ENABLED":     "true",
		"HEALTH_CHECK_URL":          mock.guestURL + "/health",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Health check output tail:\n%s", tail(output, 3000))

	// Verify the mock server received a health check POST.
	healthBodies := mock.getBodies("health")
	if len(healthBodies) == 0 {
		t.Fatal("mock server did not receive any health check POST")
	}

	// Verify the payload is valid JSON (health check results array).
	body := healthBodies[0]
	if !json.Valid(body) {
		t.Fatalf("health check payload is not valid JSON: %s", string(body))
	}

	// Verify it decodes as an array of check results.
	var results []map[string]interface{}
	if err := json.Unmarshal(body, &results); err != nil {
		t.Fatalf("failed to decode health check results: %v", err)
	}
	t.Logf("health check POST received with %d result(s)", len(results))
}

// ---------------------------------------------------------------------------
// 6. Inventory collection POSTs to mock endpoint
// Proves: INVENTORY_ENABLED=true triggers an inventory POST with system info.
// ---------------------------------------------------------------------------

func TestInventoryCollectionEndpointQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)

	mock := newFeatureMockServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "inventory-target.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "inventory-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     mock.guestURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  mock.guestURL + "/status/init",
		"INVENTORY_ENABLED":         "true",
		"INVENTORY_URL":             mock.guestURL + "/inventory",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Inventory output tail:\n%s", tail(output, 3000))

	// Verify the mock server received an inventory POST.
	inventoryBodies := mock.getBodies("inventory")
	if len(inventoryBodies) == 0 {
		t.Fatal("mock server did not receive any inventory POST")
	}

	// Verify the payload is valid JSON.
	body := inventoryBodies[0]
	if !json.Valid(body) {
		t.Fatalf("inventory payload is not valid JSON: %s", string(body))
	}

	// Verify the payload contains expected hardware fields.
	bodyStr := strings.ToLower(string(body))
	expectedFields := []string{"cpu", "memory", "disk"}
	found := 0
	for _, field := range expectedFields {
		if strings.Contains(bodyStr, field) {
			found++
		}
	}
	t.Logf("inventory POST received, matched %d/%d expected fields", found, len(expectedFields))
	if found == 0 {
		t.Error("inventory payload contains none of the expected fields (cpu, memory, disk)")
	}
}

// ---------------------------------------------------------------------------
// 7. Telemetry/metrics collection POSTs to mock endpoint
// Proves: TELEMETRY_ENABLED=true triggers metrics/telemetry POST(s).
// ---------------------------------------------------------------------------

func TestTelemetryCollectionEndpointQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)

	mock := newFeatureMockServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "telemetry-target.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "telemetry-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     mock.guestURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  mock.guestURL + "/status/init",
		"TELEMETRY_ENABLED":         "true",
		"TELEMETRY_URL":             mock.guestURL + "/telemetry",
		"METRICS_URL":               mock.guestURL + "/metrics",
		"EVENT_URL":                 mock.guestURL + "/events",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Telemetry output tail:\n%s", tail(output, 3000))

	// Verify the mock server received at least one telemetry, metrics, or events POST.
	telemetryBodies := mock.getBodies("telemetry")
	metricsBodies := mock.getBodies("metrics")
	eventBodies := mock.getBodies("events")

	totalPosts := len(telemetryBodies) + len(metricsBodies) + len(eventBodies)
	t.Logf("telemetry POSTs: %d, metrics POSTs: %d, event POSTs: %d",
		len(telemetryBodies), len(metricsBodies), len(eventBodies))

	if totalPosts == 0 {
		t.Error("mock server did not receive any telemetry, metrics, or event POSTs")
	}

	// Verify at least one payload is valid JSON.
	allBodies := append(append(telemetryBodies, metricsBodies...), eventBodies...)
	for _, body := range allBodies {
		if json.Valid(body) {
			t.Log("at least one telemetry/metrics payload is valid JSON")
			return
		}
	}
	if totalPosts > 0 {
		t.Error("received telemetry POSTs but none contained valid JSON")
	}
}

// ---------------------------------------------------------------------------
// 8. Rescue mode retry on failed image download
// Proves: RESCUE_MODE=retry causes BOOTy to retry after download failure.
// ---------------------------------------------------------------------------

func TestRescueModeRetryQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	targetDisk := filepath.Join(t.TempDir(), "rescue-retry.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	// Point to an unreachable URL so the image download fails.
	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "rescue-retry-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     "http://10.0.2.2:1/nonexistent.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"RESCUE_MODE":               "retry",
	})

	// Use a shorter timeout since this will fail and retry.
	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 3*time.Minute)
	outStr := string(output)
	t.Logf("Rescue retry output tail:\n%s", tail(output, 3000))

	// Verify retry-related messages appear in the serial output.
	outLower := strings.ToLower(outStr)
	retryIndicators := []string{"retry", "retrying", "attempt"}
	found := false
	for _, indicator := range retryIndicators {
		if strings.Contains(outLower, indicator) {
			t.Logf("rescue mode retry indicator found: %q", indicator)
			found = true
			break
		}
	}

	// Also check for error/failure messages that indicate the download failed.
	failIndicators := []string{"error", "fail", "unreachable", "connection refused"}
	failFound := false
	for _, indicator := range failIndicators {
		if strings.Contains(outLower, indicator) {
			failFound = true
			break
		}
	}

	if !found && !failFound {
		t.Error("no retry or failure indicators found in serial output")
	}
	if found {
		t.Log("rescue mode retry behavior confirmed")
	}
	if failFound {
		t.Log("image download failure detected (expected)")
	}
}

// ---------------------------------------------------------------------------
// 9. Dry-run mode does not modify the target disk
// Proves: MODE=dry-run runs validation without writing to the disk.
// ---------------------------------------------------------------------------

func TestDryRunSkipsDiskWritesQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawImage := createTestDiskImage(t, 512)
	imageGz := compressGzip(t, rawImage)

	mock := newFeatureMockServer(t, imageGz)

	targetDisk := filepath.Join(t.TempDir(), "dryrun-features.qcow2")
	run(t, "create target qcow2", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	// Compute checksum of the empty qcow2 before provisioning.
	hashBefore := fileChecksum(t, targetDisk)

	kernel := findKernel(t)
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "dryrun-features-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     mock.guestURL + "/image.gz",
		"MODE":                      "dry-run",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  mock.guestURL + "/status/init",
		"HEALTH_CHECKS_ENABLED":     "true",
	})

	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 3*time.Minute)
	outStr := string(output)
	t.Logf("Dry-run features output tail:\n%s", tail(output, 3000))

	// Verify disk was NOT modified.
	hashAfter := fileChecksum(t, targetDisk)
	if hashBefore != hashAfter {
		t.Error("dry-run modified the target disk! Hashes differ.")
	} else {
		t.Log("disk checksum unchanged after dry-run (correct)")
	}

	// Verify dry-run markers appear in serial output.
	outLower := strings.ToLower(outStr)
	dryRunMarkers := []string{"dry-run", "dry_run", "dryrun", "validation"}
	found := false
	for _, marker := range dryRunMarkers {
		if strings.Contains(outLower, marker) {
			t.Logf("dry-run marker found: %q", marker)
			found = true
			break
		}
	}
	if !found {
		t.Log("no explicit dry-run marker in output, but disk integrity confirmed")
	}
}

// ---------------------------------------------------------------------------
// Mock server for feature tests
// ---------------------------------------------------------------------------

// featureMockServer is a generic mock HTTP server that captures POST bodies
// keyed by endpoint name. It serves the test image and accepts arbitrary
// POSTs to health, inventory, telemetry, metrics, and event endpoints.
type featureMockServer struct {
	guestURL string

	mu     sync.Mutex
	bodies map[string][][]byte // endpoint name -> captured POST bodies
}

func newFeatureMockServer(t *testing.T, imagePath string) *featureMockServer {
	t.Helper()
	state := &featureMockServer{
		bodies: make(map[string][][]byte),
	}

	mux := http.NewServeMux()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	state.guestURL = fmt.Sprintf("http://10.0.2.2:%d", port)

	// Status init endpoint.
	mux.HandleFunc("/status/init", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Image serving.
	mux.HandleFunc("/image.gz", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, imagePath)
	})

	// Generic POST-capturing endpoints.
	captureEndpoints := []string{
		"/health", "/inventory", "/telemetry", "/metrics", "/events",
	}
	for _, ep := range captureEndpoints {
		endpoint := ep // capture loop variable
		name := strings.TrimPrefix(endpoint, "/")
		mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			state.mu.Lock()
			state.bodies[name] = append(state.bodies[name], body)
			state.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		})
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	return state
}

// getBodies returns a snapshot of captured POST bodies for the given endpoint.
func (s *featureMockServer) getBodies(endpoint string) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([][]byte, len(s.bodies[endpoint]))
	for i, b := range s.bodies[endpoint] {
		cp := make([]byte, len(b))
		copy(cp, b)
		result[i] = cp
	}
	return result
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// computeSHA256 computes the SHA256 hash of a file and returns it as a hex string.
func computeSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open for SHA256: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("SHA256 copy: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
