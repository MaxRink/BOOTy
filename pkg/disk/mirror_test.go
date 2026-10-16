//go:build linux

package disk

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDetectMirrorPairPicksLargestEqualPair(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0", 100, "")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0", 20, "")
	writeSysfsBlockDevice(t, sysfs, "sdc", "0", 20, "")
	writeSysfsBlockDevice(t, sysfs, "sdd", "0", 50, "")
	writeSysfsBlockDevice(t, sysfs, "sde", "0", 50, "")
	writeSysfsBlockDevice(t, sysfs, "md2", "0", 50, "")

	first, second, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0)
	if err != nil || !ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want a pair", ok, err)
	}
	if first != "/dev/sdd" || second != "/dev/sde" {
		t.Fatalf("DetectMirrorPair() = %s, %s; want /dev/sdd, /dev/sde", first, second)
	}
}

func TestDetectMirrorPairWithoutEqualDisks(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0", 100, "")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0", 20, "")
	writeSysfsBlockDevice(t, sysfs, "sdc", "1", 20, "") // removable

	_, _, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0)
	if err != nil || ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want no pair", ok, err)
	}
}

func TestDetectMirrorPairHonorsMinimumSize(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0", 20, "")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0", 20, "")

	_, _, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 30)
	if err != nil || ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want no pair above 30 GiB", ok, err)
	}
}

func TestMirrorCommands(t *testing.T) {
	cmd := newMockCommander()
	m := NewManager(cmd)
	ctx := context.Background()

	if err := m.ReplicatePartitionTable(ctx, "/dev/sda", "/dev/sdb"); err != nil {
		t.Fatalf("ReplicatePartitionTable() error = %v", err)
	}
	if err := m.CreateDegradedMirror(ctx, MirrorRootArray, "/dev/sda2"); err != nil {
		t.Fatalf("CreateDegradedMirror() error = %v", err)
	}
	if err := m.AddMirrorMember(ctx, MirrorRootArray, "/dev/sdb2"); err != nil {
		t.Fatalf("AddMirrorMember() error = %v", err)
	}

	for _, want := range [][]string{
		{"sgdisk", "--replicate=/dev/sdb", "/dev/sda"},
		{"sgdisk", "--randomize-guids", "/dev/sdb"},
		{"mdadm", "--create", "/dev/md2", "--run", "--force", "--level=1", "--raid-devices=2", "--metadata=1.0", "--assume-clean", "/dev/sda2", "missing"},
		{"mdadm", "--add", "/dev/md2", "/dev/sdb2"},
	} {
		if !hasDiskCommandCall(cmd.calls, want[0], want[1:]...) {
			t.Errorf("missing command %v in %v", want, cmd.calls)
		}
	}
}

func TestDetectMirrorPairSkipsNamespacesOfOneController(t *testing.T) {
	tests := []struct {
		name             string
		disks            []string
		wantSecond       string
		perNamespaceLink bool
	}{
		{name: "namespaces of one controller only", disks: []string{"nvme0n1", "nvme0n2"}},
		{name: "second controller available", disks: []string{"nvme0n1", "nvme0n2", "nvme1n1"}, wantSecond: "/dev/nvme1n1"},
		{name: "namespace device links differ", disks: []string{"nvme0n1", "nvme0n2"}, perNamespaceLink: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysfs := t.TempDir()
			for _, name := range tt.disks {
				writeSysfsBlockDevice(t, sysfs, name, "0", 20, "")
				ctrl := filepath.Join(sysfs, "ctrl", name[:len("nvme0")])
				if tt.perNamespaceLink {
					// Some kernels link each namespace to its own sysfs node.
					ctrl = filepath.Join(ctrl, name)
				}
				if err := os.MkdirAll(ctrl, 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(sysfs, "block", name, "device")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(ctrl, link); err != nil {
					t.Fatal(err)
				}
			}
			first, second, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0)
			if err != nil {
				t.Fatalf("DetectMirrorPair() error = %v", err)
			}
			if ok != (tt.wantSecond != "") || second != tt.wantSecond {
				t.Fatalf("DetectMirrorPair() = %s, %s, %v; want second %q", first, second, ok, tt.wantSecond)
			}
		})
	}
}

func TestMirrorHasMember(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", []byte("   0  259  2  0  active sync  /dev/nvme0n1p2\n"), nil)
	m := NewManager(cmd)
	for member, want := range map[string]bool{"/dev/nvme0n1p2": true, "/dev/nvme1n1p2": false, "/dev/nvme0n1p": false} {
		got, err := m.MirrorHasMember(context.Background(), MirrorRootArray, member)
		if err != nil || got != want {
			t.Errorf("MirrorHasMember(%s) = %v, %v; want %v", member, got, err, want)
		}
	}
}

func writeSuperblockImage(t *testing.T, size int64, offset int64, sb []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "part.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(sb, offset); err != nil {
		t.Fatal(err)
	}
	return path
}

func extSuperblock(blocks uint64, logBlockSize uint32) []byte {
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[4:], uint32(blocks))
	binary.LittleEndian.PutUint32(sb[24:], logBlockSize)
	binary.LittleEndian.PutUint16(sb[56:], 0xEF53)
	if blocks>>32 != 0 {
		binary.LittleEndian.PutUint32(sb[96:], 0x80)
		binary.LittleEndian.PutUint32(sb[0x150:], uint32(blocks>>32))
	}
	return sb
}

func TestCheckMirrorTailSpace(t *testing.T) {
	const mib = 1 << 20
	xfs := make([]byte, 16)
	copy(xfs, "XFSB")
	binary.BigEndian.PutUint32(xfs[4:], 4096)
	binary.BigEndian.PutUint64(xfs[8:], 64*mib/4096)
	btrfs := make([]byte, 0x78)
	copy(btrfs[0x40:], "_BHRfS_M")
	binary.LittleEndian.PutUint64(btrfs[0x70:], 64*mib)

	tests := []struct {
		name    string
		size    int64
		offset  int64
		sb      []byte
		wantErr bool
	}{
		{name: "ext4 filesystem with grown partition", size: 128 * mib, offset: 1024, sb: extSuperblock(64*mib/4096, 2)},
		{name: "ext4 filesystem fills partition", size: 64 * mib, offset: 1024, sb: extSuperblock(64*mib/4096, 2), wantErr: true},
		{name: "ext4 filesystem within metadata reserve", size: 64*mib + 512*1024, offset: 1024, sb: extSuperblock(64*mib/4096, 2), wantErr: true},
		{name: "ext4 64bit block count", size: 128 * mib, offset: 1024, sb: extSuperblock(1<<32, 2), wantErr: true},
		{name: "xfs filesystem with grown partition", size: 128 * mib, offset: 0, sb: xfs},
		{name: "xfs filesystem fills partition", size: 64 * mib, offset: 0, sb: xfs, wantErr: true},
		{name: "btrfs filesystem with grown partition", size: 128 * mib, offset: 0x10000, sb: btrfs},
		{name: "unknown filesystem", size: 128 * mib, offset: 0, sb: []byte("nothing"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSuperblockImage(t, tt.size, tt.offset, tt.sb)
			if err := CheckMirrorTailSpace(path); (err != nil) != tt.wantErr {
				t.Fatalf("CheckMirrorTailSpace() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDetectMirrorPairSkipsPathsToOneLUN(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0", 20, "LUN-1")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0", 20, "LUN-1")
	if _, _, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0); err != nil || ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want no pair for two paths to one LUN", ok, err)
	}
	writeSysfsBlockDevice(t, sysfs, "sdc", "0", 20, "LUN-2")
	first, second, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0)
	if err != nil || !ok || first != "/dev/sda" || second != "/dev/sdc" {
		t.Fatalf("DetectMirrorPair() = %s, %s, %v, %v; want /dev/sda and /dev/sdc", first, second, ok, err)
	}
}

func TestDetectMirrorPairRequiresSameSectorSize(t *testing.T) {
	sysfs := t.TempDir()
	for name, lbs := range map[string]string{"sda": "512", "sdb": "4096"} {
		writeSysfsBlockDevice(t, sysfs, name, "0", 20, "")
		dir := filepath.Join(sysfs, "block", name, "queue")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "logical_block_size"), []byte(lbs+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0); err != nil || ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want no pair for 512 and 4096 byte sectors", ok, err)
	}
}

func TestShrinkFilesystemForMirror(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name       string
		size       int64
		fsType     string
		wantResize string
		wantErr    bool
	}{
		{name: "ext4 filling its partition shrinks", size: 64 * mib, fsType: "ext4", wantResize: "64512K"},
		{name: "ext4 with tail space stays", size: 128 * mib, fsType: "ext4"},
		{name: "xfs filling its partition fails", size: 64 * mib, fsType: "xfs", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSuperblockImage(t, tt.size, 1024, extSuperblock(64*mib/4096, 2))
			cmd := newMockCommander()
			cmd.setResult("blkid -o", []byte(tt.fsType+"\n"), nil)
			err := NewManager(cmd).ShrinkFilesystemForMirror(context.Background(), path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ShrinkFilesystemForMirror() error = %v, wantErr %v", err, tt.wantErr)
			}
			resized := hasDiskCommandCall(cmd.calls, "resize2fs", path, tt.wantResize)
			if (tt.wantResize != "") != resized {
				t.Fatalf("resize2fs %s called = %v, want %v (calls %v)", tt.wantResize, resized, tt.wantResize != "", cmd.calls)
			}
		})
	}
}

func TestDetectMirrorPairSkipsPathsSharingWWID(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0", 20, "")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0", 20, "")
	for _, name := range []string{"sda", "sdb"} {
		if err := os.WriteFile(filepath.Join(sysfs, "block", name, "device", "wwid"), []byte("naa.600a0b80\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0); err != nil || ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want no pair for two paths with one WWID", ok, err)
	}
}

func TestDetectMirrorPairSkipsOverlappingSecondaryIdentity(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0", 20, "SERIAL-A")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0", 20, "SERIAL-B")
	for _, name := range []string{"sda", "sdb"} {
		if err := os.WriteFile(filepath.Join(sysfs, "block", name, "device", "wwid"), []byte("naa.shared\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok, err := newManagerWithSysfs(newMockCommander(), sysfs).DetectMirrorPair(context.Background(), 0); err != nil || ok {
		t.Fatalf("DetectMirrorPair() ok=%v err=%v, want no pair for overlapping secondary identity", ok, err)
	}
}

func TestMirrorActive(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "md2")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		array      string
		out        string
		err        error
		wantActive bool
		wantErr    bool
	}{
		{name: "assembled", array: existing, wantActive: true},
		{name: "no device node", array: filepath.Join(t.TempDir(), "md2"), out: "mdadm: cannot open /dev/md2: No such file or directory", err: errors.New("exit status 1")},
		{name: "inactive", array: existing, out: "mdadm: md device " + existing + " does not appear to be active.", err: errors.New("exit status 1")},
		{name: "other failure", array: existing, out: "mdadm: cannot open: Input/output error", err: errors.New("exit status 1"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			cmd.setResult("mdadm --detail", []byte(tt.out), tt.err)
			active, err := NewManager(cmd).MirrorActive(context.Background(), tt.array)
			if active != tt.wantActive || (err != nil) != tt.wantErr {
				t.Fatalf("MirrorActive() = %v, %v; want %v, wantErr %v", active, err, tt.wantActive, tt.wantErr)
			}
			assertNoMdadmAssemble(t, cmd.calls)
		})
	}
}

// assertNoMdadmAssemble fails when mdadm --assemble ran. Without an
// mdadm.conf in the ramdisk it would activate every array it finds.
func assertNoMdadmAssemble(t *testing.T, calls []mockCall) {
	t.Helper()
	for _, call := range calls {
		if call.name == "mdadm" && slices.Contains(call.args, "--assemble") {
			t.Fatalf("mdadm --assemble ran: %v", calls)
		}
	}
}

// A foreign data array on another disk must stay inactive: assembling it
// would make it busy and break mirroring. MirrorActive reports the absent
// mirror array without touching it.
func TestMirrorActiveLeavesForeignArrayAlone(t *testing.T) {
	cmd := newMockCommander()
	missing := filepath.Join(t.TempDir(), "md2")
	cmd.setResult("mdadm --detail", []byte("mdadm: cannot open "+missing+": No such file or directory"), errors.New("exit status 1"))
	// A scan would find the foreign array if anything asked for it.
	cmd.setResult("mdadm --assemble", []byte("mdadm: /dev/md/data has been started with 4 drives."), nil)
	active, err := NewManager(cmd).MirrorActive(context.Background(), missing)
	if err != nil || active {
		t.Fatalf("MirrorActive() = %v, %v; want false, nil", active, err)
	}
	assertNoMdadmAssemble(t, cmd.calls)
}

// After a quick-wipe reprovision, the second disk can still carry the md1
// superblock of the earlier install. MirrorActive must report the fresh /boot
// array as not active, so the create path adds the new partition, instead of
// assembling the stale array and finding it already complete.
func TestMirrorActiveDoesNotAssembleStaleBootArray(t *testing.T) {
	cmd := newMockCommander()
	node := filepath.Join(t.TempDir(), "md1")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.setSequence("mdadm --detail",
		mockResult{output: []byte("mdadm: md device " + node + " does not appear to be active."), err: errors.New("exit status 1")},
		// The stale array would look complete if it were assembled.
		mockResult{output: []byte("   0   8   1   0   active sync   /dev/sda1\n   1   8  17   1   active sync   /dev/sdb1\n")},
	)
	active, err := NewManager(cmd).MirrorActive(context.Background(), node)
	if err != nil || active {
		t.Fatalf("MirrorActive() = %v, %v; want false, nil", active, err)
	}
	assertNoMdadmAssemble(t, cmd.calls)
	if len(cmd.calls) != 1 {
		t.Fatalf("MirrorActive ran %d commands, want only mdadm --detail: %v", len(cmd.calls), cmd.calls)
	}
}

func TestMirrorMembersParsesOnlyDeviceRows(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", []byte(""+
		"/dev/md2:\n"+
		"      Number   Major   Minor   RaidDevice State\n"+
		"         0       8        2        0      active sync   /dev/sda2\n"+
		"         1       8       18        1      faulty         /dev/sdb2\n"+
		"   2       8       34        -        spare rebuilding  /dev/sdc2\n"), nil)
	members, err := NewManager(cmd).MirrorMembers(context.Background(), MirrorRootArray)
	if err != nil {
		t.Fatalf("MirrorMembers() error = %v", err)
	}
	want := []string{"/dev/sda2", "/dev/sdb2", "/dev/sdc2"}
	if len(members) != len(want) {
		t.Fatalf("MirrorMembers() = %#v, want %#v", members, want)
	}
	for i := range want {
		if members[i] != want[i] {
			t.Fatalf("MirrorMembers()[%d] = %q, want %q", i, members[i], want[i])
		}
	}
}
