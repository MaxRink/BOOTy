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
	m.WaitForStableDisks(context.Background(), 100*time.Millisecond, 5*time.Millisecond, 5*time.Second)
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

func TestWaitForStableDisksDisabledAndBounded(t *testing.T) {
	m := NewManager(newMockCommander())
	m.SetSysfsRoot(t.TempDir())
	start := time.Now()
	m.WaitForStableDisks(context.Background(), time.Hour, time.Millisecond, 0)
	m.WaitForStableDisks(context.Background(), time.Hour, time.Millisecond, 50*time.Millisecond)
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
