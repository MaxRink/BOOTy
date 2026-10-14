//go:build linux

package disk

import (
	"context"
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
