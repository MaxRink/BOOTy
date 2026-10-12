//go:build linux && linux_e2e

package linux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/disk"
)

// The root probe mounts candidates read-only without journal replay. Each
// journaled filesystem must accept its no-recovery option, or the probe
// would skip a valid root partition.
func TestMountPartitionInspectAcceptsNoRecoveryOptions(t *testing.T) {
	for _, tc := range []struct {
		fs   string
		mkfs []string
	}{
		{fs: "ext4", mkfs: []string{"mkfs.ext4", "-F", "-q"}},
		{fs: "xfs", mkfs: []string{"mkfs.xfs", "-f", "-q"}},
		{fs: "btrfs", mkfs: []string{"mkfs.btrfs", "-f", "-q"}},
	} {
		t.Run(tc.fs, func(t *testing.T) {
			requireTool(t, tc.mkfs[0])
			dev := createRawLoopDevice(t, 320)
			runCmd(t, tc.mkfs[0], append(tc.mkfs[1:], dev)...)

			mnt := t.TempDir()
			mgr := disk.NewManager(nil)
			if err := mgr.MountPartitionInspect(context.Background(), dev, mnt); err != nil {
				t.Fatalf("MountPartitionInspect(%s): %v", tc.fs, err)
			}
			t.Cleanup(func() { _ = mgr.Unmount(mnt) })
			if err := os.WriteFile(filepath.Join(mnt, "probe"), []byte("x"), 0o600); err == nil {
				t.Fatalf("%s inspection mount is writable", tc.fs)
			}
		})
	}
}
