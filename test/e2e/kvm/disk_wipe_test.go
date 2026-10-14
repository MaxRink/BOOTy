//go:build e2e

package kvm

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lab finding (TCAAS-862): after a deprovision, writing the old partition
// table back brought back the LVM PV/VG and ext4 with the same UUID. A
// deprovision with the default WIPE_DEPTH=thorough leaves nothing to find;
// WIPE_DEPTH=quick (the legacy wipe) keeps the partition contents.
func TestDiskWipeDeprovisionPartitionSignaturesQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	for _, tool := range []string{"lvm", "wipefs", "sfdisk", "losetup", "mkfs.ext4"} {
		if _, err := exec.LookPath(tool); err != nil {
			failOrSkipUnsupportedHost(t, "%s not available for the wipe test", tool)
		}
	}

	for _, tt := range []struct {
		depth string
		want  []string // signatures in p1 and p2 after the old table is back
	}{
		{depth: "", want: []string{"", ""}},
		{depth: "quick", want: []string{"LVM2_member", "ext4"}},
	} {
		t.Run("depth="+tt.depth, func(t *testing.T) {
			raw, table := createLVMTestDisk(t)
			targetDisk := filepath.Join(t.TempDir(), "wipe-target.qcow2")
			run(t, "convert disk to qcow2", "qemu-img", "convert", "-O", "qcow2", raw, targetDisk)

			caprf := startFakeCAPRF(t, "")
			vars := caprf.nativeVars("deprovision")
			vars["SUCCESS_ACTION"] = "poweroff"
			if tt.depth != "" {
				vars["WIPE_DEPTH"] = tt.depth
			}
			output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 4*time.Minute)
			t.Logf("deprovision output tail:\n%s", tail(output, 3000))
			caprf.requireStatuses(t, "init", "success")
			requirePowerOff(t, output)

			wiped := filepath.Join(t.TempDir(), "wiped.raw")
			run(t, "convert wiped disk to raw", "qemu-img", "convert", "-O", "raw", targetDisk, wiped)
			restore := exec.Command("sfdisk", wiped)
			restore.Stdin = strings.NewReader(table)
			if out, err := restore.CombinedOutput(); err != nil {
				t.Fatalf("restore the old partition table: %v\n%s", err, out)
			}
			loop := strings.TrimSpace(string(runOutput(t, "attach wiped disk", "losetup", "--find", "--show", "--partscan", "--read-only", wiped)))
			t.Cleanup(func() { _ = exec.Command("losetup", "-d", loop).Run() })
			for i, want := range tt.want {
				part := fmt.Sprintf("%sp%d", loop, i+1)
				waitForDevice(t, part, 5*time.Second)
				out, _ := exec.Command("wipefs", "--noheadings", "--output", "TYPE", part).CombinedOutput()
				if got := strings.TrimSpace(string(out)); got != want {
					t.Fatalf("%s after the restored table: signatures %q, want %q", part, got, want)
				}
			}
		})
	}
}

// createLVMTestDisk returns a raw disk with an LVM PV (VG and LV with
// ext4) in p1 and ext4 in p2, and its sfdisk dump.
func createLVMTestDisk(t *testing.T) (raw, table string) {
	t.Helper()
	raw = filepath.Join(t.TempDir(), "lvm-disk.raw")
	run(t, "create disk", "truncate", "-s", "1G", raw)
	sfdisk := exec.Command("sfdisk", raw)
	sfdisk.Stdin = strings.NewReader("label: gpt\nsize=256M, type=E6D6D379-F507-44C2-A23C-238F2A3DF928\nsize=256M\n")
	if out, err := sfdisk.CombinedOutput(); err != nil {
		t.Fatalf("partition disk: %v\n%s", err, out)
	}
	loop := strings.TrimSpace(string(runOutput(t, "attach disk", "losetup", "--find", "--show", "--partscan", raw)))
	vg := fmt.Sprintf("bootykvmwipe%d", time.Now().UnixNano()%1_000_000)
	detached := false
	detach := func() {
		if !detached {
			_ = exec.Command("lvm", "vgchange", "-an", vg).Run()
			_ = exec.Command("losetup", "-d", loop).Run()
			detached = true
		}
	}
	t.Cleanup(detach)
	waitForDevice(t, loop+"p2", 5*time.Second)
	run(t, "create PV", "lvm", "pvcreate", "-ff", "-y", loop+"p1")
	run(t, "create VG", "lvm", "vgcreate", vg, loop+"p1")
	run(t, "create LV", "lvm", "lvcreate", "-y", "-L", "64M", "-n", "lv", vg)
	run(t, "format LV", "mkfs.ext4", "-q", "-F", "/dev/"+vg+"/lv")
	run(t, "format p2", "mkfs.ext4", "-q", "-F", loop+"p2")
	// Drop the device paths: the subtest name in the temp path has a "=",
	// which sfdisk does not accept in a script.
	for _, line := range strings.Split(string(runOutput(t, "dump partition table", "sfdisk", "--dump", raw)), "\n") {
		if strings.HasPrefix(line, "device:") {
			continue
		}
		if _, fields, ok := strings.Cut(line, " : "); ok {
			line = fields
		}
		table += line + "\n"
	}
	detach()
	return raw, table
}
