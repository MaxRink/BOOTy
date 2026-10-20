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

// TestPredictableNICNamesQEMU puts the virtio NIC at PCI 00:05.0 and names
// it enp0s5 in the vars, like a netplan or frr.conf written for the
// installed OS. On i440fx udev names this NIC ens5 (ACPI hotplug slot 5)
// with enp0s5 as an alternative name; both resolve to it. The ramdisk
// kernel calls it eth0 (no udev, net.ifnames=0). BOOTy renames eth0 to
// enp0s5 and configures the static IP on it, so the CAPRF callbacks
// arrive. PREDICTABLE_NIC_NAMES=false leaves eth0 alone.
func TestPredictableNICNamesQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			caprf := startFakeCAPRF(t, "")
			disk := filepath.Join(t.TempDir(), "names.qcow2")
			run(t, "create disk", "qemu-img", "create", "-f", "qcow2", disk, "1G")
			vars := caprf.nativeVars("deprovision")
			vars["STATIC_IFACE"] = "enp0s5"
			vars["SUCCESS_ACTION"] = "poweroff"
			vars["PREDICTABLE_NIC_NAMES"] = fmt.Sprint(enabled)

			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			args := []string{
				"-m", "1024", "-nographic", "-no-reboot",
				"-kernel", findKernel(t), "-initrd", buildProvisionInitramfs(t, vars),
				"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
				"-netdev", "user,id=n0",
				"-device", "virtio-net-pci,netdev=n0,addr=0x5,mac=52:54:00:12:34:56",
				"-append", runnerKernelCmdline + " net.ifnames=0",
			}
			args = append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
			out, _ := exec.CommandContext(ctx, "qemu-system-x86_64", args...).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("QEMU timed out. tail:\n%s", tail(out, 4000))
			}
			renamed := strings.Contains(string(out), "renamed NIC to its predictable name") &&
				strings.Contains(string(out), "from=eth0 to=enp0s5")
			if renamed != enabled {
				t.Fatalf("rename of eth0 to enp0s5 = %v, want %v. tail:\n%s", renamed, enabled, tail(out, 4000))
			}
			if enabled {
				if !strings.Contains(string(out), "static IP configured") || !strings.Contains(string(out), "iface=enp0s5") {
					t.Fatalf("static IP not configured on enp0s5. tail:\n%s", tail(out, 4000))
				}
				caprf.requireStatuses(t, "init", "success")
			}
		})
	}
}
