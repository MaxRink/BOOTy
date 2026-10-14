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
	"time"

	"github.com/telekom/BOOTy/pkg/disk"
)

// wipeFixture is a loop disk with an LVM PV (VG, LV with ext4) in p1, ext4
// in p2, an MD RAID1 member with metadata 1.0 (superblock at the end) in p3
// and an open LUKS2 container in p4, all active.
type wipeFixture struct {
	loop, table, vg, md, crypt string
}

func (f *wipeFixture) part(n int) string { return fmt.Sprintf("%sp%d", f.loop, n) }

func newWipeFixture(t *testing.T) *wipeFixture {
	t.Helper()
	for _, tool := range []string{"sfdisk", "lvm", "mdadm", "cryptsetup", "mkfs.ext4", "wipefs", "lsblk", "blkdiscard", "dd", "sgdisk"} {
		requireTool(t, tool)
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	f := &wipeFixture{loop: createRawLoopDevice(t, 256), vg: "bootywipe" + id, md: "/dev/md/bootywipe" + id, crypt: "bootywipe" + id}
	sfdisk := exec.Command("sfdisk", f.loop)
	sfdisk.Stdin = strings.NewReader("label: gpt\nsize=64M, type=E6D6D379-F507-44C2-A23C-238F2A3DF928\nsize=48M\nsize=48M, type=A19D880F-05FC-4D3B-A006-743F0F84911E\nsize=48M\n")
	if out, err := sfdisk.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %s: %v", out, err)
	}
	for i := 1; i <= 4; i++ {
		waitForBlock(t, f.part(i))
	}
	t.Cleanup(f.teardown)

	runCmd(t, "lvm", "pvcreate", "-ff", "-y", f.part(1))
	runCmd(t, "lvm", "vgcreate", f.vg, f.part(1))
	runCmd(t, "lvm", "lvcreate", "-y", "-L", "16M", "-n", "lv", f.vg)
	runCmd(t, "mkfs.ext4", "-q", "-F", "/dev/"+f.vg+"/lv")
	runCmd(t, "mkfs.ext4", "-q", "-F", f.part(2))
	runCmd(t, "mdadm", "--create", f.md, "--run", "--level=1", "--raid-devices=1", "--force", "--metadata=1.0", f.part(3))
	runCmd(t, "mkfs.ext4", "-q", "-F", f.md)
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("booty-wipe-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "cryptsetup", "luksFormat", "--batch-mode", "--type", "luks2", "--pbkdf", "pbkdf2",
		"--pbkdf-force-iterations", "1000", "--key-file", key, f.part(4))
	runCmd(t, "cryptsetup", "open", "--key-file", key, f.part(4), f.crypt)
	f.table = runCmd(t, "sfdisk", "--dump", f.loop)
	return f
}

// teardown stops what the fixture or a restored table left active.
func (f *wipeFixture) teardown() {
	_ = exec.Command("cryptsetup", "close", f.crypt).Run()
	_ = exec.Command("lvm", "vgchange", "-an", f.vg).Run()
	_ = exec.Command("lvm", "vgremove", "-ff", "-y", f.vg).Run()
	_ = exec.Command("mdadm", "--stop", f.md).Run()
}

func waitForBlock(t *testing.T, dev string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dev); err == nil {
			return
		}
		<-time.After(100 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", dev)
}

// restoreTableSignatures writes the old partition table back and returns the
// signatures that wipefs finds in the partitions.
func (f *wipeFixture) restoreTableSignatures(t *testing.T) map[string]string {
	t.Helper()
	restore := exec.Command("sfdisk", "--no-reread", f.loop)
	restore.Stdin = strings.NewReader(f.table)
	if out, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore partition table: %s: %v", out, err)
	}
	runCmd(t, "partx", "-u", f.loop)
	found := map[string]string{}
	for i := 1; i <= 4; i++ {
		waitForBlock(t, f.part(i))
		out, _ := exec.Command("wipefs", "--noheadings", "--output", "TYPE", f.part(i)).CombinedOutput()
		if s := strings.Join(strings.Fields(string(out)), ","); s != "" {
			found[f.part(i)] = s
		}
	}
	return found
}

// Lab finding (TCAAS-862): after a wipe, restoring the old partition table
// brought back the LVM PV/VG and ext4 with the same UUID. The thorough wipe
// (default) leaves nothing to find.
func TestThoroughWipeLeavesNoPartitionSignatures(t *testing.T) {
	requireRoot(t)
	f := newWipeFixture(t)

	if err := disk.NewManager(nil).WipeDisk(context.Background(), f.loop); err != nil {
		t.Fatalf("WipeDisk() = %v", err)
	}
	if out, err := exec.Command("lvm", "vgs", f.vg).CombinedOutput(); err == nil {
		t.Fatalf("VG %s still exists after the wipe:\n%s", f.vg, out)
	}
	if found := f.restoreTableSignatures(t); len(found) > 0 {
		t.Fatalf("signatures after restoring the old partition table: %v", found)
	}
}

// WIPE_DEPTH=quick (legacy wipe_all_disks): the partition contents survive.
// This is the lab behaviour that the thorough wipe fixes.
func TestQuickWipeKeepsPartitionSignatures(t *testing.T) {
	requireRoot(t)
	f := newWipeFixture(t)
	f.teardown() // the quick wipe does not stop holders; free the partitions

	mgr := disk.NewManager(nil)
	mgr = mgr.WithQuickWipe(true)
	if err := mgr.WipeDisk(context.Background(), f.loop); err != nil {
		t.Fatalf("WipeDisk() = %v", err)
	}
	found := f.restoreTableSignatures(t)
	for i, want := range []string{"LVM2_member", "ext4", "linux_raid_member", "crypto_LUKS"} {
		if !strings.Contains(found[f.part(i+1)], want) {
			t.Fatalf("quick wipe: %s has %q, want %s to survive (all: %v)", f.part(i+1), found[f.part(i+1)], want, found)
		}
	}
}

// partitionedLoop returns a loop disk with one 64 MiB partition.
func partitionedLoop(t *testing.T) string {
	t.Helper()
	loop := createRawLoopDevice(t, 128)
	sfdisk := exec.Command("sfdisk", loop)
	sfdisk.Stdin = strings.NewReader("label: gpt\nsize=64M\n")
	if out, err := sfdisk.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %s: %v", out, err)
	}
	waitForBlock(t, loop+"p1")
	return loop
}

// The CI runner incident: BOOTy in a privileged container wiped the disks
// of the host. A disk that backs a mounted file system (also through LVM)
// or an active swap area is never wiped, with either depth.
func TestWipeRefusesDisksInUse(t *testing.T) {
	requireRoot(t)
	for _, tool := range []string{"sfdisk", "lvm", "mkfs.ext4", "mkswap", "swapon", "wipefs"} {
		requireTool(t, tool)
	}

	mounted := partitionedLoop(t)
	runCmd(t, "mkfs.ext4", "-q", "-F", mounted+"p1")
	mnt := t.TempDir()
	runCmd(t, "mount", mounted+"p1", mnt)
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() })

	lvmDisk := partitionedLoop(t)
	vg := fmt.Sprintf("bootyinuse%d", time.Now().UnixNano()%1_000_000)
	runCmd(t, "lvm", "pvcreate", "-ff", "-y", lvmDisk+"p1")
	runCmd(t, "lvm", "vgcreate", vg, lvmDisk+"p1")
	t.Cleanup(func() { _ = exec.Command("lvm", "vgremove", "-ff", "-y", vg).Run() })
	runCmd(t, "lvm", "lvcreate", "-y", "-L", "16M", "-n", "lv", vg)
	runCmd(t, "mkfs.ext4", "-q", "-F", "/dev/"+vg+"/lv")
	lvMnt := t.TempDir()
	runCmd(t, "mount", "/dev/"+vg+"/lv", lvMnt)
	t.Cleanup(func() {
		_ = exec.Command("umount", lvMnt).Run()
		_ = exec.Command("lvm", "vgchange", "-an", vg).Run()
	})

	swap := partitionedLoop(t)
	runCmd(t, "mkswap", swap+"p1")
	runCmd(t, "swapon", swap+"p1")
	t.Cleanup(func() { _ = exec.Command("swapoff", swap+"p1").Run() })

	for _, quick := range []bool{false, true} {
		mgr := disk.NewManager(nil)
		mgr = mgr.WithQuickWipe(quick)
		for device, want := range map[string]string{
			mounted:        "mounted at " + mnt,
			mounted + "p1": "mounted at " + mnt,
			lvmDisk:        "mounted at " + lvMnt,
			swap:           "active swap",
		} {
			err := mgr.WipeDisk(context.Background(), device)
			if err == nil || !strings.Contains(err.Error(), "refusing to wipe") || !strings.Contains(err.Error(), want) {
				t.Fatalf("quick=%v: WipeDisk(%s) = %v, want a refusal naming %q", quick, device, err, want)
			}
		}
	}
	for part, want := range map[string]string{mounted + "p1": "ext4", lvmDisk + "p1": "LVM2_member", swap + "p1": "swap"} {
		out, _ := exec.Command("wipefs", "--noheadings", "--output", "TYPE", part).CombinedOutput()
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s lost its %s signature: %q", part, want, out)
		}
	}
}
