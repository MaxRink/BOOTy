//go:build linux

package disk

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// WaitForStableDisks waits until the set of block devices in sysfs did not
// change for stable, at most maxWait. Storage drivers scan their buses
// asynchronously after the module load (SAS/RAID HBAs can take seconds), so
// a disk can appear after the first look at /sys/block. A timeout is only
// logged: the caller works with the disks there are.
// poll is the interval between two looks at /sys/block.
func (m *Manager) WaitForStableDisks(ctx context.Context, stable, poll, maxWait time.Duration) {
	if maxWait <= 0 {
		return
	}
	deadline := time.Now().Add(maxWait)
	last := m.blockDevices()
	since := time.Now()
	for time.Since(since) < stable {
		if time.Now().After(deadline) {
			slog.Warn("block devices still changing, continuing", "waited", maxWait, "devices", last)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		if cur := m.blockDevices(); !slices.Equal(cur, last) {
			slog.Info("block devices changed, waiting for them to settle", "devices", cur)
			last, since = cur, time.Now()
		}
	}
	slog.Info("block devices settled", "devices", last)
}

func (m *Manager) blockDevices() []string {
	entries, err := os.ReadDir(filepath.Join(m.sysfsRoot, "block"))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// HasDiskWithSerial reports whether exactly one disk with this serial
// number exists, like FindDiskBySerial, but logs only at debug level, so a
// wait loop can poll it. Errors that waiting can not fix (unreadable
// sysfs, a matching disk below the minimum size, several disks with the
// serial) are returned.
func (m *Manager) HasDiskWithSerial(ctx context.Context, serial string, minSizeGB int) (bool, error) {
	allowRemovable := os.Getenv("BOOTY_ALLOW_REMOVABLE") == "true"
	matches, _, err := m.findDiskBySerialWithSysfs(serial, minSizeGB, allowRemovable)
	if err != nil {
		return false, err
	}
	if len(matches) == 0 {
		var lsblkErr error
		matches, _, lsblkErr = m.findDiskBySerialWithLSBLK(ctx, serial, minSizeGB, allowRemovable)
		if lsblkErr != nil {
			slog.Debug("lsblk serial lookup failed", "serial", serial, "error", lsblkErr)
		}
	}
	slog.Debug("waiting for disk serial", "serial", serial, "matches", matches)
	if len(matches) > 1 {
		return false, fmt.Errorf("multiple disks found with serial %q: %s", serial, strings.Join(matches, ", "))
	}
	return len(matches) == 1, nil
}
