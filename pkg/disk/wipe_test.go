//go:build linux

package disk

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lsblk tree of a disk with an LVM PV in p1 (one LV with ext4), LUKS with
// LVM inside in p2, an MD member in p3 and ext4 in p4.
const wipeTestTree = `{"blockdevices": [{"name": "/dev/sda", "type": "disk", "fstype": null, "size": 107374182400, "children": [
 {"name": "/dev/sda1", "type": "part", "fstype": "LVM2_member", "size": 10737418240, "children": [
  {"name": "/dev/mapper/vg0-root", "type": "lvm", "fstype": "ext4", "size": 5368709120}]},
 {"name": "/dev/sda2", "type": "part", "fstype": "crypto_LUKS", "size": "1073741824", "children": [
  {"name": "/dev/mapper/luks0", "type": "crypt", "fstype": "LVM2_member", "size": 1056964608, "children": [
   {"name": "/dev/mapper/vg1-data", "type": "lvm", "fstype": "xfs", "size": 1048576000}]}]},
 {"name": "/dev/sda3", "type": "part", "fstype": "linux_raid_member", "size": 1048576, "children": [
  {"name": "/dev/md0", "type": "raid1", "fstype": "ext4", "size": 1040384}]},
 {"name": "/dev/sda4", "type": "part", "fstype": "ext4", "size": 1073741824}]}]}`

func callList(calls []mockCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.name+" "+strings.Join(c.args, " "))
	}
	return out
}

func TestThoroughWipeOrder(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk --json", []byte(wipeTestTree), nil)
	if err := NewManager(cmd).WipeDisk(context.Background(), "/dev/sda"); err != nil {
		t.Fatalf("WipeDisk() = %v", err)
	}
	dd := func(dev string, seek int64) string {
		return "dd if=/dev/zero of=" + dev + " bs=512 count=2048 seek=" + strconv.FormatInt(seek, 10) + " conv=notrunc,fsync"
	}
	want := []string{
		"lsblk --json --bytes --paths --output NAME,TYPE,FSTYPE,SIZE /dev/sda",
		// Holders, the top ones first.
		"lvm lvchange -an /dev/mapper/vg0-root",
		"lvm lvchange -an /dev/mapper/vg1-data",
		"cryptsetup close /dev/mapper/luks0",
		"mdadm --stop /dev/md0",
		// Partitions.
		"lvm pvremove -ff -y /dev/sda1",
		"wipefs -af /dev/sda1",
		dd("/dev/sda1", 0), dd("/dev/sda1", 10737418240/512-2048),
		"wipefs -af /dev/sda2",
		dd("/dev/sda2", 0), dd("/dev/sda2", 1073741824/512-2048),
		"mdadm --zero-superblock /dev/sda3",
		"wipefs -af /dev/sda3",
		dd("/dev/sda3", 0), // 1 MiB partition: no second edge
		"wipefs -af /dev/sda4",
		dd("/dev/sda4", 0), dd("/dev/sda4", 1073741824/512-2048),
		// Disk.
		"sgdisk --zap-all /dev/sda",
		"wipefs -af /dev/sda",
		dd("/dev/sda", 0), dd("/dev/sda", 107374182400/512-2048),
		"blkdiscard /dev/sda",
	}
	if got := callList(cmd.calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestThoroughWipeLogsStepFailuresAndFailsOnDiskWipefs(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk --json", []byte(wipeTestTree), nil)
	cmd.setResult("lvm pvremove", nil, errors.New("not a PV"))
	cmd.setResult("dd if=/dev/zero", nil, errors.New("read-only"))
	cmd.setResult("blkdiscard /dev/sda", nil, errors.New("not supported"))
	cmd.setResult("sgdisk --zap-all", nil, errors.New("no GPT"))
	if err := NewManager(cmd).WipeDisk(context.Background(), "/dev/sda"); err != nil {
		t.Fatalf("WipeDisk() = %v, want step failures only logged", err)
	}
	// The best-effort steps ran (a quick wipe would skip them).
	for _, step := range []string{"lvm pvremove -ff -y /dev/sda1", "blkdiscard /dev/sda"} {
		if !slices.Contains(callList(cmd.calls), step) {
			t.Fatalf("step %q did not run: %v", step, callList(cmd.calls))
		}
	}

	for _, quick := range []bool{false, true} {
		cmd = newMockCommander()
		cmd.setResult("wipefs -af", nil, errors.New("busy"))
		if err := NewManager(cmd).WithQuickWipe(quick).WipeDisk(context.Background(), "/dev/sda"); err == nil {
			t.Fatalf("quick=%v: WipeDisk() = nil, want the disk wipefs error", quick)
		}
	}
}

// A holder that can not be stopped keeps its members in use: the wipe
// fails before it changes the disk.
func TestThoroughWipeFailsWhenAHolderDoesNotStop(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk --json", []byte(wipeTestTree), nil)
	cmd.setResult("lvm lvchange", nil, errors.New("in use"))
	err := NewManager(cmd).WipeDisk(context.Background(), "/dev/sda")
	if err == nil || !strings.Contains(err.Error(), "stop lvm /dev/mapper/vg0-root") {
		t.Fatalf("WipeDisk() = %v, want the holder error", err)
	}
	for _, c := range callList(cmd.calls) {
		if strings.HasPrefix(c, "wipefs") || strings.HasPrefix(c, "sgdisk") || strings.HasPrefix(c, "dd ") {
			t.Fatalf("the wipe went on after the holder error: %q", c)
		}
	}
}

// Without lsblk output the thorough wipe still clears the disk itself.
func TestThoroughWipeWithoutPartitionList(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk --json", nil, errors.New("lsblk missing"))
	if err := NewManager(cmd).WipeDisk(context.Background(), "/dev/vda"); err != nil {
		t.Fatalf("WipeDisk() = %v", err)
	}
	want := "lsblk --json --bytes --paths --output NAME,TYPE,FSTYPE,SIZE /dev/vda\nsgdisk --zap-all /dev/vda\nwipefs -af /dev/vda\nblkdiscard /dev/vda"
	if got := strings.Join(callList(cmd.calls), "\n"); got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
}

func TestQuickWipeTouchesTheDiskOnly(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk --json", []byte(wipeTestTree), nil)
	mgr := NewManager(cmd)
	mgr = mgr.WithQuickWipe(true)
	if err := mgr.WipeDisk(context.Background(), "/dev/sda"); err != nil {
		t.Fatalf("WipeDisk() = %v", err)
	}
	for _, c := range callList(cmd.calls) {
		if !strings.HasSuffix(c, " /dev/sda") || strings.HasPrefix(c, "blkdiscard") || strings.HasPrefix(c, "dd ") {
			t.Fatalf("quick wipe ran %q", c)
		}
	}
}

// zeroEdges stays inside a small device: no write after its end.
func TestZeroEdgesClampsToTheDevice(t *testing.T) {
	for _, tt := range []struct {
		size string
		want []string
	}{
		{"524288", []string{"count=1024 seek=0"}},                          // 512 KiB
		{"1572864", []string{"count=2048 seek=0", "count=2048 seek=1024"}}, // 1.5 MiB: both edges overlap
		{"", []string{"count=2048 seek=0"}},                                // unknown size
	} {
		cmd := newMockCommander()
		NewManager(cmd).zeroEdges(context.Background(), "/dev/x", json.Number(tt.size))
		got := make([]string, 0, len(cmd.calls))
		for _, c := range cmd.calls {
			got = append(got, c.args[3]+" "+c.args[4])
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("size %q: dd %v, want %v", tt.size, got, tt.want)
		}
	}
}
