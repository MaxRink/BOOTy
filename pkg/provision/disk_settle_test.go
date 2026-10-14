//go:build linux

package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

func shortDiskSettle(t *testing.T) {
	t.Helper()
	oldStable, oldPoll := diskSettleStable, diskSettlePoll
	diskSettleStable, diskSettlePoll = 10*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { diskSettleStable, diskSettlePoll = oldStable, oldPoll })
}

// The selection waits for the disk path it uses, not for another
// configured path (the stable window and poll are far shorter than the
// delay): the wanted disk appears at 150 ms, the other never appears. A
// wait for the wrong path runs into the 5 s timeout.
func TestDiskSelectionWaitsForConfiguredDevice(t *testing.T) {
	shortDiskSettle(t)
	for _, step := range []string{"DISK_DEVICE", "PARTITION_LAYOUT device", "deprovision.device"} {
		t.Run(step, func(t *testing.T) {
			late := filepath.Join(t.TempDir(), "late-disk")
			never := filepath.Join(t.TempDir(), "never-disk")
			cfg := &config.MachineConfig{}
			cfg.Provision.Disk.SettleTimeoutSec = new(5)
			fnName := "detect-disk"
			switch step {
			case "DISK_DEVICE":
				cfg.Provision.Disk.Device = late
			case "PARTITION_LAYOUT device":
				cfg.Provision.Disk.Device = never
				cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{Device: late, Table: "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}}}
			case "deprovision.device":
				cfg.Deprovision.Device = late
				cfg.Provision.Disk.Device = never
				fnName = "select-deprovision-disk"
			}
			o := newTestOrchestrator(t, cfg, &mockProvider{})
			o.disk.SetSysfsRoot(t.TempDir())
			go func() {
				time.Sleep(150 * time.Millisecond)
				_ = os.WriteFile(late, nil, 0o600)
			}()
			fn := o.detectDisk
			if fnName != "detect-disk" {
				fn = o.selectDeprovisionDisk
			}
			start := time.Now()
			_ = fn(context.Background()) // the late "disk" is a file, not a block device
			if _, err := os.Stat(late); err != nil {
				t.Fatalf("%s returned before the configured disk appeared", step)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("%s waited %v: it waited for a disk the selection does not use", step, elapsed)
			}
		})
	}
}

// DISK_SERIAL_NUMBER: the selection waits until a disk with the serial is
// in sysfs.
func TestDiskSelectionWaitsForSerial(t *testing.T) {
	shortDiskSettle(t)
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SettleTimeoutSec = new(5)
	cfg.Provision.Disk.SerialNumber = "SN-LATE-1"
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	sysfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfs, "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.disk.SetSysfsRoot(sysfs)
	dev := filepath.Join(sysfs, "block", "sdb")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(dev, "device"), 0o755)
		_ = os.WriteFile(filepath.Join(dev, "removable"), []byte("0\n"), 0o600)
		_ = os.WriteFile(filepath.Join(dev, "size"), []byte("2097152\n"), 0o600)
		_ = os.WriteFile(filepath.Join(dev, "device", "serial"), []byte("SN-LATE-1\n"), 0o600)
	}()
	start := time.Now()
	_ = o.detectDisk(context.Background())
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("detect-disk returned after %v, want after the disk with the serial appeared (150 ms)", elapsed)
	}
	if o.targetDisk != "/dev/sdb" {
		t.Fatalf("targetDisk = %q, want /dev/sdb", o.targetDisk)
	}
}

// The decommission wipe waits for /sys/block to settle.
func TestDecommissionWaitsForStableDisks(t *testing.T) {
	oldStable, oldPoll := diskSettleStable, diskSettlePoll
	diskSettleStable, diskSettlePoll = 200*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { diskSettleStable, diskSettlePoll = oldStable, oldPoll })
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SettleTimeoutSec = new(5)
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	sysfs := t.TempDir()
	o.disk.SetSysfsRoot(sysfs)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(sysfs, "block", "sdc"), 0o755)
	}()
	start := time.Now()
	_ = o.decommissionWipe(context.Background())
	if time.Since(start) < 300*time.Millisecond {
		t.Fatalf("decommission wipe returned after %v, want a stable window after the late disk", time.Since(start))
	}
}

// A /dev/disk/by-id DISK_DEVICE can not exist without udev: fail at once.
func TestDiskSelectionRejectsUdevLinks(t *testing.T) {
	for _, step := range []string{"detect-disk", "select-deprovision-disk"} {
		cfg := &config.MachineConfig{}
		cfg.Provision.Disk.SettleTimeoutSec = new(30)
		cfg.Provision.Disk.Device = "/dev/disk/by-id/nvme-SAMSUNG_123"
		o := newTestOrchestrator(t, cfg, &mockProvider{})
		fn := o.detectDisk
		if step != "detect-disk" {
			fn = o.selectDeprovisionDisk
		}
		start := time.Now()
		err := fn(context.Background())
		if err == nil || !strings.Contains(err.Error(), "no udev") {
			t.Fatalf("%s = %v, want the no-udev error", step, err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("%s waited %v before failing", step, time.Since(start))
		}
	}
}

// Two disks with the serial: waiting can not help, the selection fails at
// once instead of after the timeout.
func TestDiskSelectionSerialErrorEndsWait(t *testing.T) {
	shortDiskSettle(t)
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SettleTimeoutSec = new(5)
	cfg.Provision.Disk.SerialNumber = "SN-DUP"
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	sysfs := t.TempDir()
	for _, name := range []string{"sda", "sdb"} {
		dev := filepath.Join(sysfs, "block", name)
		if err := os.MkdirAll(filepath.Join(dev, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
		for f, v := range map[string]string{"removable": "0", "size": "2097152", "device/serial": "SN-DUP"} {
			if err := os.WriteFile(filepath.Join(dev, f), []byte(v+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	o.disk.SetSysfsRoot(sysfs)
	start := time.Now()
	err := o.detectDisk(context.Background())
	if err == nil || !strings.Contains(err.Error(), "multiple disks") {
		t.Fatalf("detect-disk = %v, want the duplicate serial error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("detect-disk waited %v for an error that waiting can not fix", elapsed)
	}
}
