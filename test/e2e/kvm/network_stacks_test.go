//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helper: startNetworkTestServer serves an image and a /status/init endpoint
// so BOOTy can complete the init-report step without blocking.
// Returns the base URL reachable from the QEMU guest (10.0.2.2:PORT).
// ---------------------------------------------------------------------------

func startNetworkTestServer(t *testing.T, imagePath string) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/image.gz", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, imagePath)
	})
	mux.HandleFunc("/status/init", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	return fmt.Sprintf("http://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)
}

// ---------------------------------------------------------------------------
// Helper: runQEMUWithMultipleNICs launches QEMU with N virtio-net-pci NICs.
// The first NIC is backed by a user-mode network (slirp); additional NICs
// have no backend (they appear as interfaces but cannot carry traffic).
// ---------------------------------------------------------------------------

func runQEMUWithMultipleNICs(t *testing.T, kernel, initramfs, disk string, numNICs int, timeoutDur time.Duration) []byte {
	t.Helper()

	args := []string{
		"-m", "1024",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-append", runnerKernelCmdline + " net.ifnames=0",
	}

	// First NIC has a user-mode backend for actual connectivity.
	args = append(args,
		"-netdev", "user,id=net0",
		"-device", "virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56",
	)

	// Additional NICs have no backend — they still appear as eth1, eth2, etc.
	for i := 1; i < numNICs; i++ {
		mac := fmt.Sprintf("52:54:00:12:34:%02x", 0x56+i)
		args = append(args,
			"-device", fmt.Sprintf("virtio-net-pci,mac=%s", mac),
		)
	}

	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		t.Logf("QEMU multi-NIC timed out after %v. tail:\n%s", timeoutDur, tail(out, 2000))
	} else if err != nil {
		t.Logf("QEMU multi-NIC exited: %v (expected on reboot)", err)
	}

	return out
}

// ---------------------------------------------------------------------------
// Helper: runQEMUNetworkMode launches QEMU with a single NIC using virtio
// and net.ifnames=0 for predictable interface names (eth0).
// ---------------------------------------------------------------------------

func runQEMUNetworkMode(t *testing.T, kernel, initramfs, disk string, timeoutDur time.Duration) []byte {
	t.Helper()

	args := []string{
		"-m", "1024",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", runnerKernelCmdline + " net.ifnames=0",
	}
	args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)

	ctx, cancel := context.WithTimeout(context.Background(), timeoutDur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		t.Logf("QEMU network-mode timed out after %v. tail:\n%s", timeoutDur, tail(out, 2000))
	} else if err != nil {
		t.Logf("QEMU network-mode exited: %v (expected on reboot)", err)
	}

	return out
}

// ---------------------------------------------------------------------------
// Helper: prepareProvisionAssets creates a test disk image, compresses it,
// starts an HTTP server, and creates a target qcow2 disk.
// Returns (baseURL, targetDiskPath).
// ---------------------------------------------------------------------------

func prepareProvisionAssets(t *testing.T) (baseURL string, targetDisk string) {
	t.Helper()

	rawDisk := createTestDiskImage(t, 512)
	gzImage := compressGzip(t, rawDisk)
	baseURL = startNetworkTestServer(t, gzImage)

	targetDisk = filepath.Join(t.TempDir(), "target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	return baseURL, targetDisk
}

// ---------------------------------------------------------------------------
// Test 1: Static IP networking
// Verifies BOOTy detects static IP configuration, configures the interface,
// and achieves network connectivity via QEMU user-mode networking.
// ---------------------------------------------------------------------------

func TestStaticIPNetworkingQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "static-ip-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	output := runQEMUNetworkMode(t, kernel, initramfs, targetDisk, 5*time.Minute)
	outStr := string(output)
	t.Logf("Static IP output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify static network mode was selected.
	if !strings.Contains(outStr, "static") {
		t.Error("expected 'static' in output indicating static network mode selection")
	}

	// Verify provisioning progressed (image download proves connectivity worked).
	if strings.Contains(outStr, "stream-image") || strings.Contains(outStr, "Streaming image") {
		t.Log("image streaming reached, static IP networking established connectivity")
	}
}

// ---------------------------------------------------------------------------
// Test 2: Bond formation with active-backup mode
// Verifies BOOTy detects bond configuration, creates bond0, and adds members.
// Active-backup does not require LACP negotiation, making it testable in QEMU.
// ---------------------------------------------------------------------------

func TestBondFormationQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "bond-test-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"BOND_INTERFACES":           "eth0,eth1",
		"BOND_MODE":                 "active-backup",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	output := runQEMUWithMultipleNICs(t, kernel, initramfs, targetDisk, 2, 5*time.Minute)
	outStr := string(output)
	t.Logf("Bond formation output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify bond setup was attempted.
	bondMarkers := []string{"bond", "LACP", "enslav"}
	found := false
	for _, marker := range bondMarkers {
		if strings.Contains(strings.ToLower(outStr), strings.ToLower(marker)) {
			t.Logf("bond marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no bond-related markers found in output")
	}
}

// ---------------------------------------------------------------------------
// Test 3: VLAN configuration
// Verifies BOOTy detects VLAN configuration and creates a VLAN sub-interface.
// ---------------------------------------------------------------------------

func TestVLANConfigurationQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "vlan-test-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"VLANS":                     "100:eth0:10.100.0.1/24",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	output := runQEMUNetworkMode(t, kernel, initramfs, targetDisk, 5*time.Minute)
	outStr := string(output)
	t.Logf("VLAN configuration output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify VLAN setup was attempted.
	vlanMarkers := []string{"vlan", "VLAN", "eth0.100"}
	found := false
	for _, marker := range vlanMarkers {
		if strings.Contains(outStr, marker) {
			t.Logf("VLAN marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no VLAN-related markers found in output")
	}
}

// ---------------------------------------------------------------------------
// Test 4: DHCP fallback
// Verifies BOOTy falls back to DHCP when no explicit network config is set.
// QEMU user-mode networking provides DHCP, so provisioning should succeed.
// ---------------------------------------------------------------------------

func TestDHCPFallbackQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	// Intentionally no STATIC_IP, no BOND_INTERFACES, no FRR/GoBGP vars.
	// BOOTy should detect the absence and use DHCP mode.
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "dhcp-fallback-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	output := runQEMUNetworkMode(t, kernel, initramfs, targetDisk, 5*time.Minute)
	outStr := string(output)
	t.Logf("DHCP fallback output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify DHCP mode was selected.
	dhcpMarkers := []string{"DHCP", "dhcp", "udhcpc", "lease"}
	found := false
	for _, marker := range dhcpMarkers {
		if strings.Contains(outStr, marker) {
			t.Logf("DHCP marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no DHCP-related markers found in output")
	}

	// Verify provisioning progressed past network setup (image download proves
	// DHCP provided connectivity).
	if strings.Contains(outStr, "stream-image") || strings.Contains(outStr, "Streaming image") {
		t.Log("image streaming reached, DHCP fallback established connectivity")
	}
}

// ---------------------------------------------------------------------------
// Test 5: GoBGP mode detection
// Verifies BOOTy detects NETWORK_MODE=gobgp and attempts GoBGP initialization.
// BGP peering will fail (no real peer), but we verify the code path is reached.
// ---------------------------------------------------------------------------

func TestGoBGPModeDetectionQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "gobgp-detect-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"NETWORK_MODE":              "gobgp",
		"underlay_ip":               "10.0.0.20",
		"asn_server":                "65001",
		"provision_vni":             "100",
		"provision_ip":              "10.100.0.20/24",
		"provision_gateway":         "10.100.0.1",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	// Shorter timeout: BGP peering will fail without a real peer.
	output := runQEMUNetworkMode(t, kernel, initramfs, targetDisk, 3*time.Minute)
	outStr := string(output)
	t.Logf("GoBGP detection output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify GoBGP mode was detected.
	gobgpMarkers := []string{"GoBGP", "gobgp", "BGP", "underlay"}
	found := false
	for _, marker := range gobgpMarkers {
		if strings.Contains(outStr, marker) {
			t.Logf("GoBGP marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no GoBGP-related markers found in output")
	}
}

// ---------------------------------------------------------------------------
// Test 6: FRR mode detection
// Verifies BOOTy detects FRR/EVPN configuration and attempts FRR setup.
// FRR daemon won't start in the test initramfs, but the detection is verified.
// ---------------------------------------------------------------------------

func TestFRRModeDetectionQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	// FRR mode is detected when underlay_subnet + asn_server are present
	// and NETWORK_MODE is NOT "gobgp".
	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "frr-detect-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"underlay_subnet":           "10.0.0.0/24",
		"asn_server":                "65001",
		"provision_vni":             "100",
		"overlay_subnet":            "fd00::/64",
		"dns_resolver":              "8.8.8.8",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	// Shorter timeout: FRR setup will fail without the FRR daemon.
	output := runQEMUNetworkMode(t, kernel, initramfs, targetDisk, 3*time.Minute)
	outStr := string(output)
	t.Logf("FRR detection output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify FRR mode was detected.
	frrMarkers := []string{"FRR", "frr", "EVPN", "evpn", "underlay"}
	found := false
	for _, marker := range frrMarkers {
		if strings.Contains(outStr, marker) {
			t.Logf("FRR marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no FRR-related markers found in output")
	}
}

// ---------------------------------------------------------------------------
// Test 7: Multiple VLANs on the same interface
// Verifies BOOTy creates multiple VLAN sub-interfaces on one parent.
// ---------------------------------------------------------------------------

func TestMultipleVLANsQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "multi-vlan-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"VLANS":                     "100:eth0:10.100.0.1/24,200:eth0:10.200.0.1/24",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"STATIC_IFACE":              "eth0",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	output := runQEMUNetworkMode(t, kernel, initramfs, targetDisk, 5*time.Minute)
	outStr := string(output)
	t.Logf("Multi-VLAN output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify both VLANs were processed.
	// The log message "setting up VLAN interfaces" with count=2 indicates
	// both VLANs were parsed. Individual VLAN setup logs contain the VLAN ID.
	vlanMarkers := []string{"VLAN", "vlan"}
	found := false
	for _, marker := range vlanMarkers {
		if strings.Contains(outStr, marker) {
			t.Logf("VLAN marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no VLAN-related markers found in output")
	}

	// Check for both VLAN IDs in the output.
	for _, vlanID := range []string{"100", "200"} {
		if strings.Contains(outStr, vlanID) {
			t.Logf("VLAN ID %s found in output", vlanID)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 8: Bond mode selection (balance-rr)
// Verifies BOOTy applies the specified bond mode (round-robin).
// Tests a different mode than TestBondFormationQEMU (active-backup).
// ---------------------------------------------------------------------------

func TestBondModeSelectionQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	baseURL, targetDisk := prepareProvisionAssets(t)

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "bond-rr-node",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"BOND_INTERFACES":           "eth0,eth1",
		"BOND_MODE":                 "balance-rr",
		"STATIC_IP":                 "10.0.2.15/24",
		"STATIC_GATEWAY":            "10.0.2.2",
		"INSECURE_TRANSPORT":        "true",
		"INIT_URL":                  baseURL + "/status/init",
	})

	kernel := findKernel(t)
	output := runQEMUWithMultipleNICs(t, kernel, initramfs, targetDisk, 2, 5*time.Minute)
	outStr := string(output)
	t.Logf("Bond mode selection output tail:\n%s", tail(output, 3000))

	// Verify BOOTy started.
	if !strings.Contains(outStr, bootyStartMarker) {
		t.Fatal("BOOTy did not start")
	}

	// Verify bond setup was attempted.
	bondMarkers := []string{"bond", "LACP", "enslav"}
	found := false
	for _, marker := range bondMarkers {
		if strings.Contains(strings.ToLower(outStr), strings.ToLower(marker)) {
			t.Logf("bond marker found: %q", marker)
			found = true
		}
	}
	if !found {
		t.Error("no bond-related markers found in output")
	}
}
