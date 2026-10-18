//go:build linux

package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForStableDisksWaitsForLateDisk(t *testing.T) {
	sysfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfs, "block", "nvme0n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(sysfs)
	// An HBA disk shows up after the first look.
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(sysfs, "block", "sda"), 0o755)
	}()
	start := time.Now()
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second, false)
	if _, err := os.Stat(filepath.Join(sysfs, "block", "sda")); err != nil {
		t.Fatal("returned before the late disk appeared")
	}
	if got := m.blockDevices(); len(got) != 2 {
		t.Fatalf("devices = %v, want nvme0n1 and sda", got)
	}
	if time.Since(start) < 160*time.Millisecond {
		t.Fatalf("returned after %v, want a stable window after the late disk", time.Since(start))
	}
}

// A second wait, for example of the next standby command on a copy of the
// manager, returns at once while the device set did not change.
func TestWaitForStableDisksOncePerDeviceSet(t *testing.T) {
	sysfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfs, "block", "nvme0n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(sysfs)
	stable := 200 * time.Millisecond
	m.WaitForStableDisks(context.Background(), stable, 5*time.Millisecond, 5*time.Second, false)

	start := time.Now()
	m.WithQuickWipe(true).WaitForStableDisks(context.Background(), stable, 5*time.Millisecond, 5*time.Second, false)
	if d := time.Since(start); d >= stable {
		t.Fatalf("second wait took %v, want an immediate return", d)
	}

	if err := os.MkdirAll(filepath.Join(sysfs, "block", "sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	m.WaitForStableDisks(context.Background(), stable, 5*time.Millisecond, 5*time.Second, false)
	if d := time.Since(start); d < stable {
		t.Fatalf("wait after a new disk took %v, want a new stable window", d)
	}
}

func TestWaitForStableDisksDisabledAndBounded(t *testing.T) {
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(t.TempDir())
	start := time.Now()
	m.WaitForStableDisks(context.Background(), time.Hour, time.Millisecond, 0, false)
	m.WaitForStableDisks(context.Background(), time.Hour, time.Millisecond, 50*time.Millisecond, false)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %v, want the bound", time.Since(start))
	}
}

// HasDiskWithSerial ends the wait only for errors that waiting can not fix.
// A disk below the minimum size is one, also when only lsblk sees the
// serial. A failing lsblk, a disk without a size file and a disk that
// reports 0 sectors are "not there yet".
func TestHasDiskWithSerial(t *testing.T) {
	type file struct{ path, data string }
	for _, tt := range []struct {
		name      string
		files     []file
		lsblk     string
		lsblkErr  error
		minSizeGB int
		found     bool
		belowMin  bool
	}{
		{
			name:      "lsblk, too small",
			files:     []file{{"sdb/size", "4194304"}, {"sdb/removable", "0"}},
			lsblk:     "/dev/sdb SN-1\n",
			minSizeGB: 100,
			belowMin:  true,
		},
		{
			name:      "lsblk, 2 GB disk, minimum 1 GB",
			files:     []file{{"sdb/size", "4194304"}, {"sdb/removable", "0"}},
			lsblk:     "/dev/sdb SN-1\n",
			minSizeGB: 1,
			found:     true,
		},
		{
			name:      "lsblk, 2 GB disk, minimum 2 GB",
			files:     []file{{"sdb/size", "4194304"}, {"sdb/removable", "0"}},
			lsblk:     "/dev/sdb SN-1\n",
			minSizeGB: 2,
			found:     true,
		},
		{
			name:      "lsblk fails",
			files:     []file{{"sdb/size", "4194304"}, {"sdb/removable", "0"}},
			lsblkErr:  errors.New("lsblk: exit status 1"),
			minSizeGB: 100,
		},
		{
			name:      "lsblk, no sysfs size file",
			files:     []file{{"sdc/removable", "0"}},
			lsblk:     "/dev/sdc SN-1\n",
			minSizeGB: 100,
		},
		{
			name:      "lsblk, 0 sectors",
			files:     []file{{"sdb/size", "0"}, {"sdb/removable", "0"}},
			lsblk:     "/dev/sdb SN-1\n",
			minSizeGB: 1,
		},
		{
			name:      "sysfs, 0 sectors",
			files:     []file{{"sdb/size", "0"}, {"sdb/removable", "0"}, {"sdb/device/serial", "SN-1"}},
			minSizeGB: 1,
		},
		{
			name:      "sysfs, too small",
			files:     []file{{"sdb/size", "4194304"}, {"sdb/removable", "0"}, {"sdb/device/serial", "SN-1"}},
			minSizeGB: 100,
			belowMin:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sysfs := t.TempDir()
			for _, f := range tt.files {
				p := filepath.Join(sysfs, "block", f.path)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(f.data+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := newMockCommander()
			cmd.setResult("lsblk --nodeps", []byte(tt.lsblk), tt.lsblkErr)
			m := NewManager(cmd)
			m.SetSysfsRoot(sysfs)
			found, err := m.HasDiskWithSerial(context.Background(), "SN-1", tt.minSizeGB)
			if found != tt.found {
				t.Errorf("found = %v, want %v", found, tt.found)
			}
			if (err != nil) != tt.belowMin || errors.Is(err, errDiskBelowMinimum) != tt.belowMin {
				t.Errorf("err = %v, want below-minimum %v", err, tt.belowMin)
			}
		})
	}
}

// With a block watch that saw the devices stable for the window already,
// WaitForStableDisks returns at once; a change after the watch started
// still gets its full window.
func TestWaitForStableDisksCountsFromTheBlockWatch(t *testing.T) {
	sysfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfs, "block", "nvme0n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartBlockWatch(ctx, sysfs, 5*time.Millisecond)
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(sysfs)

	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second, true)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("waited %v for disks that were stable since the watch started", d)
	}

	if err := os.MkdirAll(filepath.Join(sysfs, "block", "sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the watch sees sda
	start = time.Now()
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second, true)
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("returned after %v, want most of the window after the new disk", d)
	}
}

// settleSysfs makes a sysfs root with these block devices.
func settleSysfs(t *testing.T, devices ...string) string {
	t.Helper()
	sysfs := t.TempDir()
	for _, d := range devices {
		if err := os.MkdirAll(filepath.Join(sysfs, "block", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return sysfs
}

// waitTime is how long WaitForStableDisks waits with a 100 ms window.
func waitTime(sysfs string, creditBoot bool) time.Duration {
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(sysfs)
	start := time.Now()
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second, creditBoot)
	return time.Since(start)
}

// Without the boot credit (every caller that does not wait for a named
// disk), the full window is observed even when the watch saw the disks
// stable for longer: a late SCSI scan can still present a disk.
func TestWaitForStableDisksObservesTheWindowWithoutCredit(t *testing.T) {
	sysfs := settleSysfs(t, "nvme0n1")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartBlockWatch(ctx, sysfs, 5*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if d := waitTime(sysfs, false); d < 100*time.Millisecond {
		t.Fatalf("returned after %v, want the full window", d)
	}
}

// The credit needs a watch on the same sysfs, a watch that saw the same
// device set as the first look of the call, and a watch that has run for
// the window: the time before the watch started does not count.
func TestWaitForStableDisksCreditGuards(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, ctx context.Context) string{
		"other sysfs": func(t *testing.T, ctx context.Context) string {
			t.Helper()
			StartBlockWatch(ctx, settleSysfs(t, "nvme0n1"), 5*time.Millisecond)
			time.Sleep(150 * time.Millisecond)
			return settleSysfs(t, "nvme0n1")
		},
		"device set changed since the last look of the watch": func(t *testing.T, ctx context.Context) string {
			t.Helper()
			sysfs := settleSysfs(t, "nvme0n1")
			StartBlockWatch(ctx, sysfs, time.Hour)
			time.Sleep(150 * time.Millisecond)
			if err := os.MkdirAll(filepath.Join(sysfs, "block", "sda"), 0o755); err != nil {
				t.Fatal(err)
			}
			return sysfs
		},
		"watch just started": func(t *testing.T, ctx context.Context) string {
			t.Helper()
			sysfs := settleSysfs(t, "nvme0n1")
			StartBlockWatch(ctx, sysfs, 5*time.Millisecond)
			return sysfs
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if d := waitTime(setup(t, ctx), true); d < 90*time.Millisecond {
				t.Fatalf("returned after %v, want (nearly) the full 100 ms window", d)
			}
		})
	}
}

// A wait that took the boot credit does not mark the devices settled for
// later calls: a later call without the credit still observes its window.
func TestWaitForStableDisksCreditDoesNotMarkSettled(t *testing.T) {
	sysfs := settleSysfs(t, "nvme0n1")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartBlockWatch(ctx, sysfs, 5*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(sysfs)
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second, true)
	start := time.Now()
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second, false)
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("returned after %v, want the full window after a credited wait", d)
	}
}
