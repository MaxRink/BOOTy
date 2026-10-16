//go:build linux && linux_e2e

package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/blockdev"
	"github.com/telekom/BOOTy/pkg/disk"
)

const mirrorE2EArray = "/dev/md91"

// createLoopDeviceWithLayout attaches a sizeMB sparse file partitioned with
// the given sfdisk script as a loop device with partition scanning.
func createLoopDeviceWithLayout(t *testing.T, sizeMB int, layout string) string {
	t.Helper()
	ctx := context.Background()
	img := filepath.Join(t.TempDir(), "disk.img")
	runCmd(t, "truncate", "-s", fmt.Sprintf("%dM", sizeMB), img)
	sfdisk := exec.CommandContext(ctx, "sfdisk", img)
	sfdisk.Stdin = strings.NewReader(layout)
	if out, err := sfdisk.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %s: %v", out, err)
	}
	loopDev := strings.TrimSpace(runCmd(t, "losetup", "--find", "--show", "--partscan", img))
	t.Cleanup(func() { _ = exec.CommandContext(ctx, "losetup", "-d", loopDev).Run() })
	return loopDev
}

func requireMirrorTools(t *testing.T) {
	t.Helper()
	requireRoot(t)
	for _, tool := range []string{"mdadm", "sgdisk", "growpart", "partprobe", "sfdisk", "mkfs.ext4", "e2fsck", "resize2fs"} {
		requireTool(t, tool)
	}
}

// TestMirrorRootOnLoopDevices runs the mirror-root disk operations with real
// mdadm, sgdisk and growpart: the ext4 filesystem written to the first disk
// must survive metadata 1.0 creation and be readable through the array.
func TestMirrorRootOnLoopDevices(t *testing.T) {
	requireMirrorTools(t)
	ctx := context.Background()

	first := createLoopDeviceWithLayout(t, 1024, "label: gpt\n"+
		"1: size=32M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name=EFI\n"+
		"2: size=256M, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=root\n")
	second := createRawLoopDevice(t, 1024)
	esp, root := blockdev.PartitionDevicePath(first, 1), blockdev.PartitionDevicePath(first, 2)
	mirrorESP, mirrorRoot := blockdev.PartitionDevicePath(second, 1), blockdev.PartitionDevicePath(second, 2)

	// The EFI marker stands in for the ESP content that firmware reads.
	espMarker := []byte("booty-esp-marker")
	writeAt(t, esp, espMarker)
	runCmd(t, "mkfs.ext4", "-q", "-F", root)
	mnt := t.TempDir()
	runCmd(t, "mount", root, mnt)
	if err := os.WriteFile(filepath.Join(mnt, "marker"), []byte("root"), 0o600); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	runCmd(t, "umount", mnt)

	mgr := disk.NewManager(nil)
	if err := mgr.GrowPartition(ctx, first, 2); err != nil {
		t.Fatalf("GrowPartition: %v", err)
	}
	if err := disk.CheckMirrorTailSpace(root); err != nil {
		t.Fatalf("CheckMirrorTailSpace after grow: %v", err)
	}
	if err := mgr.ReplicatePartitionTable(ctx, first, second); err != nil {
		t.Fatalf("ReplicatePartitionTable: %v", err)
	}
	if err := disk.CopyBlockDevice(esp, mirrorESP); err != nil {
		t.Fatalf("CopyBlockDevice: %v", err)
	}
	if got := readAt(t, mirrorESP, len(espMarker)); string(got) != string(espMarker) {
		t.Fatalf("mirror ESP starts with %q, want %q", got, espMarker)
	}

	if err := mgr.CreateDegradedMirror(ctx, mirrorE2EArray, root); err != nil {
		t.Fatalf("CreateDegradedMirror: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.CommandContext(ctx, "mdadm", "--stop", mirrorE2EArray).Run()
		_ = exec.CommandContext(ctx, "mdadm", "--zero-superblock", root, mirrorRoot).Run()
	})
	if active, err := mgr.MirrorActive(ctx, mirrorE2EArray); err != nil || !active {
		t.Fatalf("MirrorActive = %v, %v after CreateDegradedMirror", active, err)
	}
	if err := mgr.AddMirrorMember(ctx, mirrorE2EArray, mirrorRoot); err != nil {
		t.Fatalf("AddMirrorMember: %v", err)
	}
	if ok, err := mgr.MirrorHasMember(ctx, mirrorE2EArray, mirrorRoot); err != nil || !ok {
		t.Fatalf("MirrorHasMember(%s) = %v, %v; want true", mirrorRoot, ok, err)
	}
	scan, err := mgr.MirrorScan(ctx, mirrorE2EArray)
	if err != nil {
		t.Fatalf("MirrorScan: %v", err)
	}
	if !strings.HasPrefix(scan, "ARRAY ") || !strings.Contains(scan, "metadata=1.0") {
		t.Fatalf("MirrorScan = %q, want an ARRAY line with metadata=1.0", scan)
	}
	// mdadm --wait exits 1 when there was nothing to wait for.
	_ = exec.CommandContext(ctx, "mdadm", "--wait", mirrorE2EArray).Run()

	runCmd(t, "e2fsck", "-f", "-n", mirrorE2EArray)
	runCmd(t, "resize2fs", mirrorE2EArray)
	runCmd(t, "mount", mirrorE2EArray, mnt)
	defer runCmd(t, "umount", mnt)
	if data, err := os.ReadFile(filepath.Join(mnt, "marker")); err != nil || string(data) != "root" {
		t.Fatalf("root marker through the array = %q, %v; want root", data, err)
	}
	if out := runCmd(t, "mdadm", "--examine", mirrorRoot); !strings.Contains(out, "Version : 1.0") {
		t.Fatalf("second member has no metadata 1.0 superblock:\n%s", out)
	}
}

// TestMirrorTailSpaceRejectsFullPartition covers an image whose root
// partition already fills the disk: growpart cannot grow it, so metadata 1.0
// would overwrite the end of the filesystem.
func TestMirrorTailSpaceRejectsFullPartition(t *testing.T) {
	requireMirrorTools(t)
	loopDev := createLoopDevice(t, 1)
	root := blockdev.PartitionDevicePath(loopDev, 2)
	runCmd(t, "mkfs.ext4", "-q", "-F", root)

	if err := disk.NewManager(nil).GrowPartition(context.Background(), loopDev, 2); err != nil {
		t.Fatalf("GrowPartition: %v", err)
	}
	if err := disk.CheckMirrorTailSpace(root); err == nil {
		t.Fatal("CheckMirrorTailSpace() error = nil for a filesystem that fills its partition")
	}
}

func writeAt(t *testing.T, device string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", device, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatalf("write %s: %v", device, err)
	}
}

func readAt(t *testing.T, device string, n int) []byte {
	t.Helper()
	f, err := os.Open(device)
	if err != nil {
		t.Fatalf("open %s: %v", device, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("read %s: %v", device, err)
	}
	return buf
}
