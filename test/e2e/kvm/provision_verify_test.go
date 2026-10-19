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

// TestProvisionVerifyHostnameAndDNS provisions a disk via BOOTy in QEMU and
// verifies that /etc/hostname and /etc/resolv.conf are correctly written.
func TestProvisionVerifyHostnameAndDNS(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	const hostname = "test-node-01"
	dnsResolvers := "8.8.8.8,1.1.1.1"

	rawDisk := createTestDiskImage(t, 512)
	gzImage := compressGzip(t, rawDisk)
	baseURL := startImageServer(t, gzImage)

	targetDisk := filepath.Join(t.TempDir(), "target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  hostname,
		"dns_resolver":              dnsResolvers,
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
	})

	kernel := findKernel(t)
	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("QEMU output tail:\n%s", tail(output, 3000))

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	hostnameContent := readProvisionedFile(t, rootMount, "etc/hostname")
	got := strings.TrimSpace(hostnameContent)
	if got != hostname {
		t.Errorf("hostname: got %q, want %q", got, hostname)
	}

	resolvConf := readProvisionedFile(t, rootMount, "etc/resolv.conf")
	for _, ns := range strings.Split(dnsResolvers, ",") {
		expected := "nameserver " + ns
		if !strings.Contains(resolvConf, expected) {
			t.Errorf("resolv.conf missing %q:\n%s", expected, resolvConf)
		}
	}
}

// TestProvisionVerifyGRUBKernelParams verifies GRUB kernel parameters are written.
func TestProvisionVerifyGRUBKernelParams(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	extraParams := "audit=0 quiet splash"

	rawDisk := createTestDiskImage(t, 512)
	gzImage := compressGzip(t, rawDisk)
	baseURL := startImageServer(t, gzImage)

	targetDisk := filepath.Join(t.TempDir(), "target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                    "grub-test",
		"IMAGE_ALLOW_INSECURE_HTTP":   "true",
		"IMAGE":                       baseURL + "/image.gz",
		"MODE":                        "provision",
		"DISK_DEVICE":                 "/dev/vda",
		"MACHINE_EXTRA_KERNEL_PARAMS": extraParams,
		"CLOUDINIT_ENABLED":           "true",
	})

	kernel := findKernel(t)
	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("QEMU output tail:\n%s", tail(output, 3000))

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	grubCfg := readProvisionedFile(t, rootMount, "etc/default/grub.d/10-caprf-kernel-params.cfg")
	if !strings.Contains(grubCfg, "GRUB_CMDLINE_LINUX=") {
		t.Errorf("GRUB config missing GRUB_CMDLINE_LINUX:\n%s", grubCfg)
	}
	if !strings.Contains(grubCfg, "ds=nocloud") {
		t.Errorf("GRUB config missing ds=nocloud:\n%s", grubCfg)
	}
	for _, param := range strings.Fields(extraParams) {
		if !strings.Contains(grubCfg, param) {
			t.Errorf("GRUB config missing param %q:\n%s", param, grubCfg)
		}
	}
}

// TestProvisionVerifyKubeletConfig verifies kubelet configuration files.
func TestProvisionVerifyKubeletConfig(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createTestDiskImage(t, 512)
	gzImage := compressGzip(t, rawDisk)
	baseURL := startImageServer(t, gzImage)

	targetDisk := filepath.Join(t.TempDir(), "target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "kubelet-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
		"PROVIDER_ID":               "redfish://10.0.0.1/Systems/1",
		"FAILURE_DOMAIN":            "az-1",
		"REGION":                    "eu-central",
	})

	kernel := findKernel(t)
	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("QEMU output tail:\n%s", tail(output, 3000))

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()

	kubeletCfg := readProvisionedFile(t, rootMount, "etc/default/kubelet")
	if !strings.Contains(kubeletCfg, "redfish://10.0.0.1/Systems/1") {
		t.Errorf("kubelet provider-id config missing expected value:\n%s", kubeletCfg)
	}
	if !strings.Contains(kubeletCfg, "topology.kubernetes.io/zone=az-1") {
		t.Errorf("kubelet labels config missing zone:\n%s", kubeletCfg)
	}
	if !strings.Contains(kubeletCfg, "topology.kubernetes.io/region=eu-central") {
		t.Errorf("kubelet labels config missing region:\n%s", kubeletCfg)
	}
}

// --- Helpers ---

// requireRoot requires root privileges.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		failOrSkipUnsupportedHost(t, "root privileges required for KVM provisioning test")
	}
}

// findKernel locates a usable Linux kernel for QEMU direct boot.
func findKernel(t *testing.T) string {
	t.Helper()
	if k := os.Getenv("BOOTY_KERNEL"); k != "" {
		if _, err := os.Stat(k); err == nil {
			return k
		}
	}
	candidates := []string{"/boot/vmlinuz", "/boot/vmlinuz-" + kernelRelease()}
	entries, _ := filepath.Glob("/boot/vmlinuz-*")
	candidates = append(candidates, entries...)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Fatal("no Linux kernel found for QEMU boot")
	return ""
}

// kernelRelease returns the running kernel version.
func kernelRelease() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readProvisionedFile reads a file from the provisioned root filesystem.
func readProvisionedFile(t *testing.T, rootMount, relPath string) string {
	t.Helper()
	fullPath := filepath.Join(rootMount, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("read provisioned file %s: %v", relPath, err)
	}
	return string(data)
}

// TestProvisionQCOW2ImageQEMU provisions from compressed qcow2 images
// (zlib and zstd clusters). The test ramdisk has no qemu-img: BOOTy
// converts qcow2 in process. The provisioned root must carry the hostname.
func TestProvisionQCOW2ImageQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	for _, tt := range []struct {
		name string
		opts []string
	}{
		{"zlib", []string{"-c"}},
		{"zstd extended L2", []string{"-c", "-o", "compression_type=zstd,extended_l2=on"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hostname := "qcow2-" + strings.ReplaceAll(tt.name, " ", "-")
			rawDisk := createTestDiskImage(t, 512)
			qcow2Image := filepath.Join(t.TempDir(), "image.qcow2")
			args := append([]string{"convert", "-f", "raw", "-O", "qcow2"}, tt.opts...)
			run(t, "convert image to qcow2", "qemu-img", append(args, rawDisk, qcow2Image)...)
			baseURL := startImageServer(t, qcow2Image)

			targetDisk := filepath.Join(t.TempDir(), "target.qcow2")
			run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")
			initramfs := buildProvisionInitramfs(t, map[string]string{
				"HOSTNAME":                  hostname,
				"IMAGE_ALLOW_INSECURE_HTTP": "true",
				// The server path is /image.gz; BOOTy finds qcow2 by its magic.
				"IMAGE":       baseURL + "/image.gz",
				"MODE":        "provision",
				"DISK_DEVICE": "/dev/vda",
			})
			output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 5*time.Minute)
			t.Logf("QEMU output tail:\n%s", tail(output, 3000))
			if !strings.Contains(string(output), "qcow2 conversion complete") {
				t.Fatalf("no in-process qcow2 conversion in the output; tail:\n%s", tail(output, 3000))
			}

			rootMount, cleanup := mountQcow2(t, targetDisk)
			defer cleanup()
			if got := strings.TrimSpace(readProvisionedFile(t, rootMount, "etc/hostname")); got != hostname {
				t.Errorf("hostname: got %q, want %q", got, hostname)
			}
		})
	}
}
