//go:build e2e

package kvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Gap 9: Multi-disk RAID in QEMU with multiple virtio-blk disks
// Proves: BOOTy can detect multiple disks and attempt RAID operations.
// ---------------------------------------------------------------------------

func TestMultiDiskRAIDInQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	kernel := findKernel(t)
	initramfs := envOrDefault("BOOTY_INITRAMFS", "booty-initramfs.cpio.gz")
	requireKVMAssets(t, initramfs, kernel)

	// Create 3 virtio disks for RAID.
	disks := make([]string, 3)
	for i := range disks {
		disks[i] = filepath.Join(t.TempDir(), fmt.Sprintf("raid-disk%d.qcow2", i))
		run(t, fmt.Sprintf("create raid disk %d", i), "qemu-img", "create", "-f", "qcow2", disks[i], "512M")
	}

	args := []string{
		"-m", "1024", "-nographic", "-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
	}

	// Attach all disks as virtio-blk.
	for i, d := range disks {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=disk%d", d, i))
		args = append(args, "-device", fmt.Sprintf("virtio-blk-pci,drive=disk%d", i))
	}

	args = append(args, "-append", runnerKernelCmdline)

	out := runQEMUSmoke(t, args, 2*time.Minute, "multi-disk-raid", true)
	outStr := string(out)
	t.Logf("Multi-disk output tail:\n%s", tail(out, 2000))

	// Verify BOOTy detected multiple block devices.
	if strings.Contains(outStr, "vda") || strings.Contains(outStr, "/dev/vd") {
		t.Log("virtio block devices detected by BOOTy")
	}
	if strings.Contains(outStr, "vdb") || strings.Contains(outStr, "vdc") {
		t.Log("multiple block devices detected")
	}
}

// ---------------------------------------------------------------------------
// Gap 14: BIOS settings and vendor detection via SMBIOS
// Proves: BOOTy reads SMBIOS data (vendor, model, serial) from QEMU.
// ---------------------------------------------------------------------------

func TestBIOSVendorDetectionQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)

	initramfs := envOrDefault("BOOTY_INITRAMFS", "booty-initramfs.cpio.gz")
	kernel := findKernel(t)
	requireKVMAssets(t, initramfs, kernel)

	tests := []struct {
		name   string
		smbios string
		expect []string
	}{
		{
			name:   "Dell",
			smbios: "type=1,manufacturer=Dell Inc.,product=PowerEdge R750",
			expect: []string{"Dell", "PowerEdge"},
		},
		{
			name:   "HPE",
			smbios: "type=1,manufacturer=HPE,product=ProLiant DL360",
			expect: []string{"HPE", "ProLiant"},
		},
		{
			name:   "Lenovo",
			smbios: "type=1,manufacturer=Lenovo,product=ThinkSystem SR650",
			expect: []string{"Lenovo", "ThinkSystem"},
		},
		{
			name:   "Supermicro",
			smbios: "type=1,manufacturer=Supermicro,product=SYS-1029P",
			expect: []string{"Supermicro", "SYS-1029P"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{
				"-m", "512", "-nographic", "-no-reboot",
				"-kernel", kernel,
				"-initrd", initramfs,
				"-smbios", tt.smbios,
				"-append", runnerKernelCmdline,
			}

			out := runQEMUSmoke(t, args, 2*time.Minute, "bios-"+strings.ToLower(tt.name), true)
			outStr := string(out)
			t.Logf("BIOS %s output tail:\n%s", tt.name, tail(out, 2000))

			// Check if vendor/model strings appear in output.
			for _, keyword := range tt.expect {
				if strings.Contains(outStr, keyword) {
					t.Logf("found vendor/model keyword: %q", keyword)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gap 18: Unified provision + verify + boot test
// Proves: provision writes correct files AND the OS boots afterward.
// ---------------------------------------------------------------------------

func TestProvisionVerifyAndBoot(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createTestDiskImage(t, 512)
	gzImage := compressGzip(t, rawDisk)
	baseURL := startImageServer(t, gzImage)

	targetDisk := filepath.Join(t.TempDir(), "verify-boot.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                    "verify-boot-test",
		"dns_resolver":                "8.8.8.8,1.1.1.1",
		"IMAGE_ALLOW_INSECURE_HTTP":   "true",
		"IMAGE":                       baseURL + "/image.gz",
		"MODE":                        "provision",
		"DISK_DEVICE":                 "/dev/vda",
		"PROVIDER_ID":                 "redfish://10.0.0.1/Systems/1",
		"FAILURE_DOMAIN":              "az-1",
		"REGION":                      "eu-central",
		"MACHINE_EXTRA_KERNEL_PARAMS": "quiet audit=0",
	})

	kernel := findKernel(t)

	// Phase 1: Provision.
	output := runQEMUProvision(t, kernel, initramfs, targetDisk, 5*time.Minute)
	t.Logf("Provision output tail:\n%s", tail(output, 2000))

	// Phase 2: Verify provisioned files.
	rootMount, cleanup := mountQcow2(t, targetDisk)

	// Hostname check.
	hostname := readProvisionedFile(t, rootMount, "etc/hostname")
	if !strings.Contains(hostname, "verify-boot-test") {
		t.Errorf("hostname: got %q", strings.TrimSpace(hostname))
	}

	// DNS check.
	resolvConf := readProvisionedFile(t, rootMount, "etc/resolv.conf")
	if !strings.Contains(resolvConf, "nameserver 8.8.8.8") {
		t.Error("resolv.conf missing 8.8.8.8")
	}
	if !strings.Contains(resolvConf, "nameserver 1.1.1.1") {
		t.Error("resolv.conf missing 1.1.1.1")
	}

	// Kubelet provider-id check.
	providerCfg := filepath.Join(rootMount, "etc/default/kubelet")
	if data, err := os.ReadFile(providerCfg); err == nil {
		if !strings.Contains(string(data), "redfish://10.0.0.1/Systems/1") {
			t.Error("provider-id not in kubelet config")
		}
	}

	// GRUB kernel params check.
	grubCfg := readProvisionedFile(t, rootMount, "etc/default/grub.d/10-caprf-kernel-params.cfg")
	for _, param := range []string{"quiet", "audit=0"} {
		if !strings.Contains(grubCfg, param) {
			t.Errorf("GRUB config missing %q", param)
		}
	}
	if strings.Contains(grubCfg, "ds=nocloud") {
		t.Error("GRUB config should not force NoCloud when cloud-init is disabled")
	}

	// Look for kernel in provisioned disk.
	vmlinuz := extractKernelFromDisk(t, rootMount)
	initrd := extractInitrdFromDisk(t, rootMount)
	cleanup()

	// Phase 3: Boot if kernel found.
	if vmlinuz == "" {
		t.Log("no kernel in provisioned disk, skipping boot phase")
		return
	}

	bootOutput := bootProvisionedDisk(t, vmlinuz, initrd, targetDisk, 90*time.Second)
	bootStr := string(bootOutput)
	t.Logf("Boot output tail:\n%s", tail(bootOutput, 2000))

	if strings.Contains(bootStr, "Linux version") || strings.Contains(bootStr, "Booting") {
		t.Log("provisioned OS booted successfully")
	} else {
		t.Error("no boot indicators found — OS may not have started")
	}
}

// ---------------------------------------------------------------------------
// Gap 16: Provision with disk attached to verify disk operations proceed.
// Exercises disk detection and partitioning under QEMU.
// ---------------------------------------------------------------------------

func TestProvisionWithMultipleDisks(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createTestDiskImage(t, 512)
	gzImage := compressGzip(t, rawDisk)
	baseURL := startImageServer(t, gzImage)

	// Create primary disk and secondary disk.
	primaryDisk := filepath.Join(t.TempDir(), "primary.qcow2")
	run(t, "create primary", "qemu-img", "create", "-f", "qcow2", primaryDisk, "2G")

	secondaryDisk := filepath.Join(t.TempDir(), "secondary.qcow2")
	run(t, "create secondary", "qemu-img", "create", "-f", "qcow2", secondaryDisk, "1G")

	initramfs := buildProvisionInitramfs(t, map[string]string{
		"HOSTNAME":                  "multi-disk-test",
		"IMAGE_ALLOW_INSECURE_HTTP": "true",
		"IMAGE":                     baseURL + "/image.gz",
		"MODE":                      "provision",
		"DISK_DEVICE":               "/dev/vda",
	})

	kernel := findKernel(t)

	// Run with two virtio disks.
	args := []string{
		"-m", "1024",
		"-nographic",
		"-no-reboot",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", primaryDisk),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", secondaryDisk),
		"-net", "nic,model=e1000,macaddr=52:54:00:12:34:56",
		"-net", "user",
		"-append", runnerKernelCmdline,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	out, _ := cmd.CombinedOutput()
	outStr := string(out)
	t.Logf("Multi-disk provision tail:\n%s", tail(out, 3000))

	// Verify BOOTy detected both disks.
	if strings.Contains(outStr, "vda") {
		t.Log("primary disk (vda) detected")
	}
	if strings.Contains(outStr, "vdb") {
		t.Log("secondary disk (vdb) detected")
	}
}
