//go:build linux

package disk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateDiskLinks(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("blkid -c", []byte(`DEVNAME=/dev/sda1
UUID=7B77-95E7
BLOCK_SIZE=512
TYPE=vfat
PARTLABEL=EFI\ System\ Partition
PARTUUID=0f2c3a5e-01

DEVNAME=/dev/sda2
LABEL=cloudimg-rootfs
UUID=5d7a1d2b-1111-2222-3333-444455556666
TYPE=ext4
PARTUUID=0f2c3a5e-02

DEVNAME=/dev/nvme0n1p2
LABEL=data/a b
UUID=5d7a1d2b-1111-2222-3333-444455556666
TYPE=ext4

DEVNAME=/dev/sdb1
UUID=aaaa-raid-array
UUID_SUB=bbbb-member
LABEL=host:0
TYPE=linux_raid_member
PARTUUID=0f2c3a5e-11
`), nil)
	devDir := t.TempDir()
	// A stale link from an earlier run is replaced.
	stale := filepath.Join(devDir, "disk", "by-uuid", "dead-beef")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../sdz9", stale); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cmd)
	if err := m.CreateDiskLinks(context.Background(), devDir); err != nil {
		t.Fatalf("CreateDiskLinks() = %v", err)
	}
	want := map[string]string{
		"by-uuid/7B77-95E7":                            "../../sda1",
		"by-uuid/5d7a1d2b-1111-2222-3333-444455556666": "../../sda2", // first device wins
		"by-partuuid/0f2c3a5e-01":                      "../../sda1",
		"by-partuuid/0f2c3a5e-02":                      "../../sda2",
		"by-label/cloudimg-rootfs":                     "../../sda2",
		`by-label/data\x2fa\x20b`:                      "../../nvme0n1p2",
		`by-partlabel/EFI\x20System\x20Partition`:      "../../sda1",
		"by-partuuid/0f2c3a5e-11":                      "../../sdb1", // every partition
	}
	for rel, target := range want {
		got, err := os.Readlink(filepath.Join(devDir, "disk", rel))
		if err != nil || got != target {
			t.Errorf("link %s -> %q (%v), want %q", rel, got, err, target)
		}
	}
	// RAID members share the array UUID; udev makes no fs links for them.
	for _, rel := range []string{"by-uuid/aaaa-raid-array", "by-label/host:0"} {
		if _, err := os.Lstat(filepath.Join(devDir, "disk", rel)); !os.IsNotExist(err) {
			t.Errorf("link %s exists for a RAID member", rel)
		}
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale link %s still exists", stale)
	}
	if !hasDiskCommandCall(cmd.calls, "blkid", "-c", "/dev/null", "-o", "export") {
		t.Fatalf("blkid not called with the cache disabled: %v", cmd.calls)
	}
}

func TestCreateDiskLinksBlkidWithoutDevices(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("blkid -c", nil, makeExitError(2))
	if err := NewManager(cmd).CreateDiskLinks(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("CreateDiskLinks() = %v, want nil when blkid finds no device", err)
	}
}

func TestCreateDiskLinksFailsWhenBlkidFails(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("blkid -c", nil, makeExitError(4))
	if err := NewManager(cmd).CreateDiskLinks(context.Background(), t.TempDir()); err == nil {
		t.Fatal("CreateDiskLinks() = nil, want an error when blkid fails with exit code 4")
	}
}

func TestParseBlkidExportKeepsTrailingSpaces(t *testing.T) {
	got := ParseBlkidExport("DEVNAME=/dev/vda2\r\nLABEL=BOOT\\ \nUUID=1\n\nDEVNAME=/dev/vda3\n")
	if len(got) != 2 || got[0]["LABEL"] != "BOOT " || got[0]["UUID"] != "1" || got[1]["DEVNAME"] != "/dev/vda3" {
		t.Fatalf("ParseBlkidExport() = %q", got)
	}
}

func TestIsMemberSignature(t *testing.T) {
	for fsType, want := range map[string]bool{
		"ext4": false, "vfat": false, "crypto_LUKS": false, "swap": false, "zfs_member": false,
		"linux_raid_member": true, "LVM2_member": true, "isw_raid_member": true,
		"drbd": true, "drbdmanage_control_volume": true, "stratis": true,
	} {
		if got := isMemberSignature(fsType); got != want {
			t.Errorf("isMemberSignature(%q) = %v, want %v", fsType, got, want)
		}
	}
}
