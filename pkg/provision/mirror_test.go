//go:build linux

package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

func hasProvisionCall(calls []mockCall, name string, args ...string) bool {
	want := strings.Join(args, " ")
	for _, call := range calls {
		if call.name == name && strings.Join(call.args, " ") == want {
			return true
		}
	}
	return false
}

func TestMirrorRootBuildsRAID1AcrossBothDisks(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", nil, errors.New("no such array"))
	var copied []string
	orig := copyBlockDeviceFn
	copyBlockDeviceFn = func(src, dst string) error {
		copied = append(copied, src+"->"+dst)
		return nil
	}
	t.Cleanup(func() { copyBlockDeviceFn = orig })
	stubMirrorTailSpace(t, nil)

	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.mirrorDisk = "/dev/sda", "/dev/sdb"
	o.rootPartition, o.bootPartition = "/dev/sda2", "/dev/sda1"

	if err := o.mirrorRoot(context.Background()); err != nil {
		t.Fatalf("mirrorRoot() error = %v", err)
	}
	if o.rootPartition != disk.MirrorRootArray {
		t.Fatalf("rootPartition = %s, want %s", o.rootPartition, disk.MirrorRootArray)
	}
	if len(copied) != 0 {
		t.Fatalf("mirrorRoot() copied the ESP %v; create-mirror-efi-boot-entry copies it after the last ESP write", copied)
	}
	for _, want := range [][]string{
		{"growpart", "--update", "on", "/dev/sda", "2"},
		{"sgdisk", "--replicate=/dev/sdb", "/dev/sda"},
		{"wipefs", "-af", "/dev/sdb2"},
		{"mdadm", "--create", "/dev/md2", "--run", "--force", "--level=1", "--raid-devices=2", "--metadata=1.0", "--assume-clean", "/dev/sda2", "missing"},
		{"mdadm", "--add", "/dev/md2", "/dev/sdb2"},
	} {
		if !hasProvisionCall(cmd.calls, want[0], want[1:]...) {
			t.Errorf("missing command %v", want)
		}
	}
}

func stubMirrorTailSpace(t *testing.T, err error) {
	t.Helper()
	orig := checkMirrorTailSpaceFn
	checkMirrorTailSpaceFn = func(string) error { return err }
	t.Cleanup(func() { checkMirrorTailSpaceFn = orig })
}

func TestMirrorRootStopsWithoutTailSpace(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", nil, errors.New("no such array"))
	stubMirrorTailSpace(t, errors.New("no room"))

	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.mirrorDisk, o.rootPartition = "/dev/sda", "/dev/sdb", "/dev/sda2"

	if err := o.mirrorRoot(context.Background()); err == nil {
		t.Fatal("mirrorRoot() error = nil, want tail space error")
	}
	for _, call := range cmd.calls {
		if call.name == "sgdisk" || (call.name == "mdadm" && len(call.args) > 0 && call.args[0] == "--create") {
			t.Fatalf("mirrorRoot() ran %s %v after the tail space check failed", call.name, call.args)
		}
	}
}

func TestMirrorRootReusesAssembledArray(t *testing.T) {
	const add = "--add /dev/md2 /dev/sdb2"
	tests := []struct {
		name    string
		detail  string
		wantAdd bool
	}{
		{name: "both members present", detail: "   0  8  2  0  active sync  /dev/sda2\n   1  8  18  1  spare rebuilding  /dev/sdb2\n"},
		{name: "second member missing after failed add", detail: "   0  8  2  0  active sync  /dev/sda2\n   -  0  0  1  removed\n", wantAdd: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			cmd.setResult("mdadm --detail", []byte(tt.detail), nil)
			o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
			o.targetDisk, o.mirrorDisk, o.rootPartition = "/dev/sda", "/dev/sdb", "/dev/sda2"

			if err := o.mirrorRoot(context.Background()); err != nil {
				t.Fatalf("mirrorRoot() error = %v", err)
			}
			if o.rootPartition != disk.MirrorRootArray {
				t.Fatalf("rootPartition = %s, want %s", o.rootPartition, disk.MirrorRootArray)
			}
			for _, call := range cmd.calls {
				if call.name == "growpart" || call.name == "sgdisk" || (call.name == "mdadm" && call.args[0] == "--create") {
					t.Fatalf("mirrorRoot() ran %s %v on an assembled array", call.name, call.args)
				}
			}
			if got := hasProvisionCall(cmd.calls, "mdadm", strings.Fields(add)...); got != tt.wantAdd {
				t.Fatalf("mdadm %s called = %v, want %v", add, got, tt.wantAdd)
			}
		})
	}
}

func TestMirrorRootWithoutMirrorDiskIsNoop(t *testing.T) {
	cmd := newMockCommander()
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.rootPartition = "/dev/sda", "/dev/sda2"
	if err := o.mirrorRoot(context.Background()); err != nil {
		t.Fatalf("mirrorRoot() error = %v", err)
	}
	if len(cmd.calls) != 0 || o.rootPartition != "/dev/sda2" {
		t.Fatalf("mirrorRoot() ran %v and set root %s for a single disk", cmd.calls, o.rootPartition)
	}
}

func TestValidateMirroringMode(t *testing.T) {
	raid := []config.RAIDConfig{{Name: "md0", Level: 1, Devices: []string{"/dev/sda", "/dev/sdb"}}}
	tests := []struct {
		name      string
		disk      config.DiskConfig
		imageMode string
		wantErr   bool
	}{
		{name: "unset"},
		{name: "none with explicit raid", disk: config.DiskConfig{MirroringMode: "none", RAID: raid}},
		{name: "raid-1 whole-disk", disk: config.DiskConfig{MirroringMode: "raid-1"}, imageMode: config.ImageModeWholeDisk},
		{name: "auto with explicit disk", disk: config.DiskConfig{MirroringMode: "auto", Device: "/dev/sda"}},
		{name: "unknown mode", disk: config.DiskConfig{MirroringMode: "raid-5"}, wantErr: true},
		{name: "explicit raid", disk: config.DiskConfig{MirroringMode: "raid-1", RAID: raid}, wantErr: true},
		{name: "partition layout", disk: config.DiskConfig{MirroringMode: "auto", PartitionLayout: &config.PartitionLayout{}}, wantErr: true},
		{name: "A/B image mode", disk: config.DiskConfig{MirroringMode: "raid-1"}, imageMode: config.ImageModeAB, wantErr: true},
		{name: "partition image mode", disk: config.DiskConfig{MirroringMode: "auto"}, imageMode: config.ImageModePartition, wantErr: true},
		{name: "NVMe namespace setup", disk: config.DiskConfig{MirroringMode: "raid-1", NVMeNamespaces: "2"}, wantErr: true},
		{name: "explicit disk ignores A/B", disk: config.DiskConfig{MirroringMode: "raid-1", Device: "/dev/sda"}, imageMode: config.ImageModeAB},
		{name: "explicit serial ignores explicit raid", disk: config.DiskConfig{MirroringMode: "auto", SerialNumber: "S1", RAID: raid}},
		{name: "explicit disk still checks the mode", disk: config.DiskConfig{MirroringMode: "raid-5", Device: "/dev/sda"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.Disk = tt.disk
			cfg.Provision.Image.Mode = tt.imageMode
			o := NewOrchestrator(cfg, &mockProvider{}, newTestDiskManager())
			if err := o.validateMirroringMode(); (err != nil) != tt.wantErr {
				t.Fatalf("validateMirroringMode() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSelectMirrorPairDisabled(t *testing.T) {
	for _, mode := range []string{"", "none", "NONE"} {
		cfg := &config.MachineConfig{}
		cfg.Provision.Disk.MirroringMode = mode
		o := NewOrchestrator(cfg, &mockProvider{}, newTestDiskManager())
		mirrored, err := o.selectMirrorPair(context.Background())
		if err != nil || mirrored {
			t.Fatalf("selectMirrorPair(%q) = %v, %v; want false, nil", mode, mirrored, err)
		}
	}
}

func newTestDiskManager() *disk.Manager {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", nil, errors.New("no such array"))
	return disk.NewManager(cmd)
}

func TestCreateMirrorEFIBootEntryBIOSInstallsGrubAndCopiesESP(t *testing.T) {
	var events []string
	origCopy, origMounted, origMount, origEFI := copyBlockDeviceFn, isMountPoint, mountBootPart, efiFirmwarePath
	t.Cleanup(func() {
		copyBlockDeviceFn, isMountPoint, mountBootPart, efiFirmwarePath = origCopy, origMounted, origMount, origEFI
	})
	efiFirmwarePath = filepath.Join(t.TempDir(), "missing-efi")
	isMountPoint = func(string) bool { return false }
	copyBlockDeviceFn = func(src, dst string) error {
		events = append(events, "copy "+src+"->"+dst)
		return nil
	}
	mountBootPart = func(_ context.Context, _ *disk.Manager, device, mountpoint string) error {
		events = append(events, "mount "+device+" "+mountpoint)
		return nil
	}

	cmd := newMockCommander()
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.mirrorDisk, o.bootPartition = "/dev/sda", "/dev/sdb", "/dev/sda1"
	if err := o.createMirrorEFIBootEntry(context.Background()); err != nil {
		t.Fatalf("createMirrorEFIBootEntry() error = %v", err)
	}
	for _, d := range []string{"/dev/sda", "/dev/sdb"} {
		if !hasProvisionCall(cmd.calls, "chroot", newroot, "/bin/bash", "-c", "grub-install --target=i386-pc "+d) {
			t.Fatalf("missing BIOS grub-install for %s in %v", d, cmd.calls)
		}
	}
	want := []string{"copy /dev/sda1->/dev/sdb1", "mount /dev/sda1 " + bootEFIMountPoint()}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestCreateMirrorEFIBootEntryUEFIRequiresESP(t *testing.T) {
	origEFI := efiFirmwarePath
	t.Cleanup(func() { efiFirmwarePath = origEFI })
	efiFirmwarePath = t.TempDir()

	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, newTestDiskManager())
	o.targetDisk, o.mirrorDisk = "/dev/sda", "/dev/sdb"
	if err := o.createMirrorEFIBootEntry(context.Background()); err == nil {
		t.Fatal("createMirrorEFIBootEntry() error = nil for a UEFI install without ESP")
	}
}

func TestMirrorPairSurvivesResume(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.MirroringMode = "auto"
	o := NewOrchestrator(cfg, &mockProvider{}, newTestDiskManager())
	o.targetDisk, o.mirrorDisk = "/dev/sda", "/dev/sdb"
	cp := &Checkpoint{}
	o.recordCheckpointDerivedState("detect-disk", cp)
	if cp.MirrorTargetDisk != "/dev/sda" || cp.MirrorDisk != "/dev/sdb" {
		t.Fatalf("checkpoint mirror pair = %q, %q", cp.MirrorTargetDisk, cp.MirrorDisk)
	}

	tests := []struct {
		name    string
		missing string
		wantErr bool
	}{
		{name: "both disks present"},
		{name: "second disk missing", missing: "/dev/sdb", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMockStat(t, func(path string) (os.FileInfo, error) {
				if path == tt.missing {
					return nil, os.ErrNotExist
				}
				return fakeFileInfo{name: path, mode: os.ModeDevice}, nil
			})
			resumed := NewOrchestrator(cfg, &mockProvider{}, newTestDiskManager())
			resumed.restoreCheckpointDerivedState(cp)
			mirrored, err := resumed.selectMirrorPair(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("selectMirrorPair() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (!mirrored || resumed.targetDisk != "/dev/sda" || resumed.mirrorDisk != "/dev/sdb") {
				t.Fatalf("selectMirrorPair() = %v, %s, %s; want the checkpointed pair", mirrored, resumed.targetDisk, resumed.mirrorDisk)
			}
		})
	}
}

func TestSingleDiskFallbackSurvivesResume(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.MirroringMode = "auto"
	o := NewOrchestrator(cfg, &mockProvider{}, newTestDiskManager())
	o.targetDisk = "/dev/sda"
	cp := &Checkpoint{}
	o.recordCheckpointDerivedState("detect-disk", cp)
	if cp.MirrorTargetDisk != "/dev/sda" || cp.MirrorDisk != "" {
		t.Fatalf("checkpoint disk selection = %q, %q; want /dev/sda and no mirror", cp.MirrorTargetDisk, cp.MirrorDisk)
	}

	withMockStat(t, func(path string) (os.FileInfo, error) {
		return fakeFileInfo{name: path, mode: os.ModeDevice}, nil
	})
	resumed := NewOrchestrator(cfg, &mockProvider{}, newTestDiskManager())
	resumed.restoreCheckpointDerivedState(cp)
	chosen, err := resumed.selectMirrorPair(context.Background())
	if err != nil || !chosen || resumed.targetDisk != "/dev/sda" || resumed.mirrorDisk != "" {
		t.Fatalf("selectMirrorPair() = %v, %v, %s, %q; want the single disk from before resume",
			chosen, err, resumed.targetDisk, resumed.mirrorDisk)
	}
}

func TestSeparateBootPartition(t *testing.T) {
	tests := []struct {
		name     string
		parts    []disk.Partition
		wantBoot string
		wantErr  bool
	}{
		{name: "root, BIOS boot and ESP", parts: []disk.Partition{
			{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID},
			{Node: "/dev/sda14", Type: biosBootPartitionGUID},
			{Node: "/dev/sda15", Type: disk.EFISystemPartitionGUID},
		}},
		{name: "separate /boot partition", parts: []disk.Partition{
			{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID},
			{Node: "/dev/sda15", Type: disk.EFISystemPartitionGUID},
			{Node: "/dev/sda16", Type: xbootldrPartitionGUID},
		}, wantBoot: "/dev/sda16"},
		{name: "two extra Linux partitions", parts: []disk.Partition{
			{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID},
			{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID},
			{Node: "/dev/sda3", Type: disk.LinuxFilesystemGUID},
		}, wantErr: true},
		{name: "second ESP", parts: []disk.Partition{
			{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID},
			{Node: "/dev/sda15", Type: disk.EFISystemPartitionGUID},
			{Node: "/dev/sda16", Type: disk.EFISystemPartitionGUID},
		}, wantErr: true},
		{name: "swap partition", parts: []disk.Partition{
			{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID},
			{Node: "/dev/sda2", Type: "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F"},
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, newTestDiskManager())
			o.rootPartition, o.bootPartition, o.targetPartitions = "/dev/sda1", "/dev/sda15", tt.parts
			boot, err := o.separateBootPartition(context.Background())
			if (err != nil) != tt.wantErr || boot != tt.wantBoot {
				t.Fatalf("separateBootPartition() = %q, %v; want %q, wantErr %v", boot, err, tt.wantBoot, tt.wantErr)
			}
		})
	}
}

func TestMirrorRootMirrorsSeparateBoot(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", nil, errors.New("no such array"))
	stubMirrorTailSpace(t, nil)
	var shrunk []string
	origShrink := shrinkFilesystemForMirrorFn
	shrinkFilesystemForMirrorFn = func(_ *disk.Manager, _ context.Context, member string) error {
		shrunk = append(shrunk, member)
		return nil
	}
	t.Cleanup(func() { shrinkFilesystemForMirrorFn = origShrink })

	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.mirrorDisk = "/dev/sda", "/dev/sdb"
	o.rootPartition, o.bootPartition = "/dev/sda1", "/dev/sda15"
	o.targetPartitions = []disk.Partition{
		{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID},
		{Node: "/dev/sda15", Type: disk.EFISystemPartitionGUID},
		{Node: "/dev/sda16", Type: xbootldrPartitionGUID},
	}
	if err := o.mirrorRoot(context.Background()); err != nil {
		t.Fatalf("mirrorRoot() error = %v", err)
	}
	if len(shrunk) != 1 || shrunk[0] != "/dev/sda16" {
		t.Fatalf("shrunk filesystems = %v, want /dev/sda16", shrunk)
	}
	if strings.Join(o.mirrorArrays, ",") != disk.MirrorRootArray+","+disk.MirrorBootArray {
		t.Fatalf("mirrorArrays = %v", o.mirrorArrays)
	}
	for _, want := range [][]string{
		{"wipefs", "-af", "/dev/sdb16"},
		{"mdadm", "--create", "/dev/md1", "--run", "--force", "--level=1", "--raid-devices=2", "--metadata=1.0", "--assume-clean", "/dev/sda16", "missing"},
		{"mdadm", "--add", "/dev/md1", "/dev/sdb16"},
		{"mdadm", "--add", "/dev/md2", "/dev/sdb1"},
	} {
		if !hasProvisionCall(cmd.calls, want[0], want[1:]...) {
			t.Errorf("missing command %v", want)
		}
	}
}

func TestMdadmArrayLines(t *testing.T) {
	tests := []struct {
		name, array, scan, want string
	}{
		{
			name: "named by array number", array: disk.MirrorRootArray,
			scan: "ARRAY /dev/md/2 metadata=1.0 name=host:2 UUID=a:b:c:d\n",
			want: "ARRAY /dev/md2 metadata=1.0 name=host:2 UUID=a:b:c:d",
		},
		{
			name: "scan lists both arrays", array: disk.MirrorBootArray,
			scan: "ARRAY /dev/md/host:2 metadata=1.0 name=host:2 UUID=a:a:a:a\nARRAY /dev/md/host:1 metadata=1.0 name=host:1 UUID=b:b:b:b\n",
			want: "ARRAY /dev/md1 metadata=1.0 name=host:1 UUID=b:b:b:b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mdadmArrayLines(tt.array, tt.scan)
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("mdadmArrayLines() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteMdadmConf(t *testing.T) {
	for _, shipsEtcConf := range []bool{false, true} {
		t.Run(fmt.Sprintf("image ships /etc/mdadm.conf=%v", shipsEtcConf), func(t *testing.T) {
			root := t.TempDir()
			alt := filepath.Join(root, "etc", "mdadm.conf")
			if shipsEtcConf {
				if err := os.MkdirAll(filepath.Dir(alt), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(alt, []byte("ARRAY /dev/md0 UUID=old\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			const content = "AUTO -all\nARRAY /dev/md2 UUID=new\n"
			if err := writeMdadmConf(root, content); err != nil {
				t.Fatalf("writeMdadmConf() error = %v", err)
			}
			wantPaths := []string{filepath.Join(root, "etc", "mdadm", "mdadm.conf"), alt}
			for _, path := range wantPaths {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != content {
					t.Fatalf("%s = %q, %v; want %q", path, data, err, content)
				}
			}
		})
	}
}

func TestMountMirrorBootMountsArrayBeforeESP(t *testing.T) {
	var mounts []string
	origMounted, origMount := isMountPoint, mountBootPart
	t.Cleanup(func() { isMountPoint, mountBootPart = origMounted, origMount })
	isMountPoint = func(string) bool { return false }
	mountBootPart = func(_ context.Context, _ *disk.Manager, device, mountpoint string) error {
		mounts = append(mounts, device+" "+mountpoint)
		return nil
	}

	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, newTestDiskManager())
	o.bootPartition = "/dev/sda15"
	o.mirrorArrays = []string{disk.MirrorRootArray, disk.MirrorBootArray}
	if err := o.mountBoot(context.Background()); err != nil {
		t.Fatalf("mountBoot() error = %v", err)
	}
	want := []string{disk.MirrorBootArray + " " + filepath.Join(newroot, "boot"), "/dev/sda15 " + bootEFIMountPoint()}
	if strings.Join(mounts, "|") != strings.Join(want, "|") {
		t.Fatalf("mounts = %v, want %v", mounts, want)
	}
}

func TestSeparateBootPartitionNeedsFstabForLinuxType(t *testing.T) {
	tests := []struct {
		name     string
		fstab    string
		wantBoot string
		wantErr  bool
	}{
		{name: "fstab mounts the partition at /boot by label", fstab: "LABEL=root / ext4 defaults 0 1\nLABEL=BOOT /boot ext4 defaults 0 2\n", wantBoot: "/dev/sda2"},
		{name: "fstab mounts the partition at /var", fstab: "LABEL=root / ext4 defaults 0 1\nLABEL=BOOT /var ext4 defaults 0 2\n", wantErr: true},
		{name: "fstab mounts another /boot", fstab: "UUID=other /boot ext4 defaults 0 2\n", wantErr: true},
		{name: "fstab mounts the partition at /boot by partition label", fstab: "PARTLABEL=boot /boot ext4 defaults 0 2\n", wantBoot: "/dev/sda2"},
		{name: "fstab mounts the partition at /boot by uuid symlink", fstab: "/dev/disk/by-uuid/1234 /boot ext4 defaults 0 2\n", wantBoot: "/dev/sda2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origMount, origUnmount := mountReadOnlyPart, unmountInspectionPart
			t.Cleanup(func() { mountReadOnlyPart, unmountInspectionPart = origMount, origUnmount })
			unmountInspectionPart = func(*disk.Manager, string) error { return nil }
			mountReadOnlyPart = func(_ context.Context, _ *disk.Manager, _, dir string) error {
				if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "etc", "fstab"), []byte(tt.fstab), 0o644)
			}
			cmd := newMockCommander()
			cmd.setResult("mdadm --detail", nil, errors.New("no such array"))
			cmd.setResult("blkid -o", []byte("DEVNAME=/dev/sda2\nLABEL=BOOT\nUUID=1234\nTYPE=ext4\nPARTLABEL=boot\n"), nil)
			o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
			o.targetDisk, o.rootPartition = "/dev/sda", "/dev/sda1"
			o.targetPartitions = []disk.Partition{
				{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID, Name: "root"},
				{Node: "/dev/sda2", Type: disk.LinuxFilesystemGUID},
			}
			boot, err := o.separateBootPartition(context.Background())
			if (err != nil) != tt.wantErr || boot != tt.wantBoot {
				t.Fatalf("separateBootPartition() = %q, %v; want %q, wantErr %v", boot, err, tt.wantBoot, tt.wantErr)
			}
		})
	}
}

func TestRewriteMirrorFstab(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	fstab := "# static\nPARTUUID=aaa / ext4 defaults 0 1\nPARTLABEL=boot /boot ext4 defaults 0 2\n" +
		"/dev/sda15 /boot/efi vfat umask=0077 0 1\nLABEL=data /data ext4 defaults 0 2\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "fstab"), []byte(fstab), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newMockCommander()
	cmd.setResult("blkid -s", []byte("ABCD-1234\n"), nil)
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.bootPartition = "/dev/sda15"
	o.mirrorArrays = []string{disk.MirrorRootArray, disk.MirrorBootArray}
	if err := o.rewriteMirrorFstab(context.Background(), root); err != nil {
		t.Fatalf("rewriteMirrorFstab() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "etc", "fstab"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# static\n/dev/md2 / ext4 defaults 0 1\n/dev/md1 /boot ext4 defaults 0 2\n" +
		"UUID=ABCD-1234 /boot/efi vfat umask=0077 0 1\nLABEL=data /data ext4 defaults 0 2\n"
	if string(got) != want {
		t.Fatalf("fstab =\n%s\nwant\n%s", got, want)
	}
}

func TestRewriteMirrorFstabAddsBootEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "fstab"), []byte("LABEL=root / ext4 defaults 0 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newMockCommander()
	cmd.setResult("blkid -o", []byte("ext4\n"), nil)
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.mirrorArrays = []string{disk.MirrorRootArray, disk.MirrorBootArray}
	if err := o.rewriteMirrorFstab(context.Background(), root); err != nil {
		t.Fatalf("rewriteMirrorFstab() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "etc", "fstab"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "LABEL=root / ext4 defaults 0 1\n/dev/md1 /boot ext4 defaults 0 2\n"; string(got) != want {
		t.Fatalf("fstab = %q, want %q", got, want)
	}
}

func TestRewriteMirrorFstabWithoutFstab(t *testing.T) {
	root := t.TempDir()
	cmd := newMockCommander()
	cmd.setResult("blkid -o", []byte("ext4\n"), nil)
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.mirrorArrays = []string{disk.MirrorRootArray, disk.MirrorBootArray}
	if err := o.rewriteMirrorFstab(context.Background(), root); err != nil {
		t.Fatalf("rewriteMirrorFstab() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "etc", "fstab"))
	if err != nil || string(got) != "/dev/md1 /boot ext4 defaults 0 2\n" {
		t.Fatalf("fstab = %q, %v; want the /boot array entry", got, err)
	}
}

func TestReadTargetFstabStaysInRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(outside, []byte("host /boot ext4 defaults 0 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc", "fstab")); err != nil {
		t.Fatal(err)
	}
	if _, err := fstabEntrySource(root, "/boot"); err == nil {
		t.Fatal("fstabEntrySource() followed an fstab symlink out of the target root")
	}
}

func TestRewriteMirrorFstabFailsWithoutESPUUID(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "fstab"), []byte("PARTUUID=ccc /boot/efi vfat umask=0077 0 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newMockCommander()
	cmd.setResult("blkid -s", nil, nil)
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.bootPartition = "/dev/sda15"
	o.mirrorArrays = []string{disk.MirrorRootArray}
	if err := o.rewriteMirrorFstab(context.Background(), root); err == nil {
		t.Fatal("rewriteMirrorFstab() error = nil, want an error when the ESP UUID is unknown")
	}
}

func TestMirrorRootRejectsMBRImages(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", nil, errors.New("no such array"))
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.mirrorDisk, o.rootPartition = "/dev/sda", "/dev/sdb", "/dev/sda1"
	o.targetPartitions = []disk.Partition{{Node: "/dev/sda1", Type: disk.LinuxFilesystemMBRType}}
	if err := o.mirrorRoot(context.Background()); err == nil {
		t.Fatal("mirrorRoot() error = nil for an MBR image")
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("mirrorRoot() ran %v for an MBR image", cmd.calls)
	}
}

func TestRequireRemovableEFILoader(t *testing.T) {
	esp := t.TempDir()
	if err := requireRemovableEFILoader(esp); err == nil {
		t.Fatal("requireRemovableEFILoader() error = nil for an ESP without fallback loader")
	}
	name, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		t.Skipf("no removable loader name for %s", runtime.GOARCH)
	}
	if err := os.MkdirAll(filepath.Join(esp, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(esp, "EFI", "BOOT", strings.ToLower(name)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireRemovableEFILoader(esp); err != nil {
		t.Fatalf("requireRemovableEFILoader() error = %v with %s present", err, name)
	}
}

func TestMirrorInitramfsCmd(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	tests := []struct {
		name    string
		tools   map[string]string // tool name -> exit status
		want    string
		wantErr bool
	}{
		{name: "update-initramfs preferred", tools: map[string]string{"mdadm": "0", "update-initramfs": "0", "dracut": "0"}, want: "update-initramfs -u -k all"},
		{name: "dracut without update-initramfs", tools: map[string]string{"mdadm": "0", "dracut": "0"}, want: "dracut --force --regenerate-all"},
		{name: "dracut failure propagates", tools: map[string]string{"mdadm": "0", "dracut": "3"}, want: "dracut --force --regenerate-all", wantErr: true},
		{name: "no mdadm", tools: map[string]string{"update-initramfs": "0"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
			for tool, status := range tt.tools {
				script := "#!" + sh + "\necho \"" + tool + " $*\" >> " + log + "\nexit " + status + "\n"
				if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(context.Background(), sh, "-c", mirrorInitramfsCmd)
			cmd.Env = []string{"PATH=" + bin}
			err := cmd.Run()
			calls, _ := os.ReadFile(log)
			if (err != nil) != tt.wantErr || (tt.want != "" && strings.TrimSpace(string(calls)) != tt.want) {
				t.Fatalf("run = %v, calls %q; want %q, wantErr %v", err, calls, tt.want, tt.wantErr)
			}
		})
	}
}

func TestRequireRemovableEFILoaderRejectsDirectory(t *testing.T) {
	name, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		t.Skipf("no removable loader name for %s", runtime.GOARCH)
	}
	esp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(esp, "EFI", "BOOT", name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireRemovableEFILoader(esp); err == nil {
		t.Fatal("requireRemovableEFILoader() accepted a directory as the fallback loader")
	}
}

func TestSoftDeprovisionRejectsMirroredRoot(t *testing.T) {
	withMockStat(t, func(path string) (os.FileInfo, error) {
		return fakeFileInfo{name: path, mode: os.ModeDevice}, nil
	})
	cmd := newMockCommander()
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID}}), nil)
	cmd.setResult("blkid -o", []byte("linux_raid_member\n"), nil)
	cfg := &config.MachineConfig{}
	cfg.Deprovision.Device = "/dev/sda"
	o := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(cmd))
	// The check runs before the root is mounted.
	if err := o.softDeprovision(context.Background()); err == nil || !strings.Contains(err.Error(), "RAID member") {
		t.Fatalf("softDeprovision() error = %v, want the mirrored root rejection", err)
	}
}

// Soft deprovision must not mount the root when blkid fails: the root may
// be a RAID member.
func TestSoftDeprovisionFailsWhenRootTypeUnknown(t *testing.T) {
	withMockStat(t, func(path string) (os.FileInfo, error) {
		return fakeFileInfo{name: path, mode: os.ModeDevice}, nil
	})
	cmd := newMockCommander()
	cmd.setResult("sfdisk --json", sfdiskJSON(t, []disk.Partition{{Node: "/dev/sda1", Type: disk.LinuxFilesystemGUID}}), nil)
	cmd.setResult("blkid -o", nil, errors.New("blkid failed"))
	cfg := &config.MachineConfig{}
	cfg.Deprovision.Device = "/dev/sda"
	o := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(cmd))
	if err := o.softDeprovision(context.Background()); err == nil || !strings.Contains(err.Error(), "root filesystem type") {
		t.Fatalf("softDeprovision() = %v, want a filesystem type error", err)
	}
}

// stubMirrorESPCopy makes createMirrorEFIBootEntry run on UEFI without
// touching block devices or mounts.
func stubMirrorESPCopy(t *testing.T, ready bool) {
	t.Helper()
	origCopy, origMounted, origMount, origEFI, origReady := copyBlockDeviceFn, isMountPoint, mountBootPart, efiFirmwarePath, efiRuntimeReady
	t.Cleanup(func() {
		copyBlockDeviceFn, isMountPoint, mountBootPart, efiFirmwarePath, efiRuntimeReady = origCopy, origMounted, origMount, origEFI, origReady
	})
	efiFirmwarePath = t.TempDir()
	efiRuntimeReady = func() (bool, string) {
		if ready {
			return true, ""
		}
		return false, "efivarfs not mounted"
	}
	isMountPoint = func(string) bool { return false }
	copyBlockDeviceFn = func(string, string) error { return nil }
	mountBootPart = func(context.Context, *disk.Manager, string, string) error { return nil }
}

func writeTestEFILoader(t *testing.T, root string, rel ...string) {
	t.Helper()
	path := filepath.Join(append([]string{root, "boot", "efi", "EFI"}, rel...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("loader"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateMirrorEFIBootEntryLabelsSecondDisk(t *testing.T) {
	stubMirrorESPCopy(t, true)
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, &mockProvider{})
	o.targetDisk, o.mirrorDisk, o.bootPartition = "/dev/sda", "/dev/sdb", "/dev/sda1"
	shim, _, err := efiLoaderNames(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	writeTestEFILoader(t, o.config.rootDir, "ubuntu", shim)
	if err := o.createMirrorEFIBootEntry(context.Background()); err != nil {
		t.Fatalf("createMirrorEFIBootEntry() error = %v", err)
	}
	// Legacy raid_configure_efibootmgr: -L "${EFI_BOOT_ENTRY_LABEL}-disk2".
	if !hasProvisionCall(cmd.calls, "efibootmgr", "-c", "-d", "/dev/sdb", "-p", "1", "-L", "ubuntu-disk2", "-l", `\EFI\ubuntu\`+shim) {
		t.Fatalf("missing efibootmgr entry ubuntu-disk2 for /dev/sdb: %v", cmd.calls)
	}
}

func TestCreateMirrorEFIBootEntryHonorsEFIBootEntryRequired(t *testing.T) {
	for _, tt := range []struct {
		name     string
		required *bool
		wantErr  bool
	}{
		{name: "required by default", wantErr: true},
		{name: "not required", required: new(false)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubMirrorESPCopy(t, true)
			cfg := &config.MachineConfig{}
			cfg.Provision.EFIBootEntryRequired = tt.required
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
			o.targetDisk, o.mirrorDisk, o.bootPartition = "/dev/sda", "/dev/sdb", "/dev/sda1"
			shim, _, err := efiLoaderNames(runtime.GOARCH)
			if err != nil {
				t.Fatal(err)
			}
			writeTestEFILoader(t, o.config.rootDir, "ubuntu", shim)
			cmd.setResult("efibootmgr -c", nil, fmt.Errorf("run: %w", exec.ErrNotFound))
			err = o.createMirrorEFIBootEntry(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("createMirrorEFIBootEntry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateMirrorEFIBootEntrySkipsEntryWithoutEFIVariables(t *testing.T) {
	stubMirrorESPCopy(t, false)
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, &mockProvider{})
	o.targetDisk, o.mirrorDisk, o.bootPartition = "/dev/sda", "/dev/sdb", "/dev/sda1"
	name, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	writeTestEFILoader(t, o.config.rootDir, "BOOT", name)
	if err := o.createMirrorEFIBootEntry(context.Background()); err != nil {
		t.Fatalf("createMirrorEFIBootEntry() error = %v", err)
	}
	if hasCommandName(cmd.calls, "efibootmgr") {
		t.Fatalf("efibootmgr ran without EFI variables: %v", cmd.calls)
	}
}

func TestConfigureMdadmAtWritesTargetFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	fstab := "PARTUUID=root-a / ext4 defaults 0 1\nPARTUUID=esp-a /boot/efi vfat umask=0077 0 1\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "fstab"), []byte(fstab), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newMockCommander()
	cmd.setResult("mdadm --detail", []byte("ARRAY /dev/md/2 metadata=1.0 name=host:2 UUID=aaaa:bbbb:cccc:dddd\n"+
		"ARRAY /dev/md/data metadata=1.2 name=host:data UUID=1111:2222:3333:4444\n"), nil)
	cmd.setResult("blkid -s", []byte("ABCD-1234\n"), nil)
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk, o.mirrorDisk, o.bootPartition = "/dev/sda", "/dev/sdb", "/dev/sda1"
	o.mirrorArrays = []string{disk.MirrorRootArray}

	if err := o.configureMdadmAt(context.Background(), root); err != nil {
		t.Fatalf("configureMdadmAt() error = %v", err)
	}

	wantConf := "# mdadm.conf - written by BOOTy\n\nDEVICE partitions\nHOMEHOST <system>\nMAILADDR root\n\n" +
		"AUTO +imsm +1.x -all\n\nARRAY /dev/md2 metadata=1.0 name=host:2 UUID=aaaa:bbbb:cccc:dddd\n"
	for _, rel := range []string{"etc/mdadm/mdadm.conf", "etc/mdadm.conf"} {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != wantConf {
			t.Fatalf("%s =\n%s\nwant\n%s", rel, got, wantConf)
		}
	}
	dracut, err := os.ReadFile(filepath.Join(root, "etc", "dracut.conf.d", "90-booty-mirror.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Written by BOOTy: assemble the mirror arrays in the initramfs.\n" +
		"add_dracutmodules+=\" mdraid \"\nmdadmconf=\"yes\"\nkernel_cmdline+=\" rd.md.uuid=aaaa:bbbb:cccc:dddd \"\n"; string(dracut) != want {
		t.Fatalf("90-booty-mirror.conf =\n%s\nwant\n%s", dracut, want)
	}
	gotFstab, err := os.ReadFile(filepath.Join(root, "etc", "fstab"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/dev/md2 / ext4 defaults 0 1\nUUID=ABCD-1234 /boot/efi vfat umask=0077 0 1\n"; string(gotFstab) != want {
		t.Fatalf("fstab =\n%s\nwant\n%s", gotFstab, want)
	}
	grub, err := os.ReadFile(filepath.Join(root, "etc", "default", "grub.d", "99-booty-mirror.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(grub) != mirrorGrubConfig {
		t.Fatalf("99-booty-mirror.cfg = %q, want %q", grub, mirrorGrubConfig)
	}
	if !hasProvisionCall(cmd.calls, "chroot", root, "/bin/bash", "-c", mirrorInitramfsCmd) {
		t.Fatalf("initramfs not rebuilt in %s: %v", root, cmd.calls)
	}
}

// Two arrays (root and a separate /boot): one rd.md.uuid each, one space apart.
func TestDracutMirrorConfTwoArrays(t *testing.T) {
	got := dracutMirrorConf([]string{
		"ARRAY /dev/md2 metadata=1.0 name=host:2 UUID=aaaa:bbbb:cccc:dddd",
		"ARRAY /dev/md1 metadata=1.0 name=host:1 UUID=1111:2222:3333:4444",
	})
	want := "# Written by BOOTy: assemble the mirror arrays in the initramfs.\n" +
		"add_dracutmodules+=\" mdraid \"\nmdadmconf=\"yes\"\n" +
		"kernel_cmdline+=\" rd.md.uuid=aaaa:bbbb:cccc:dddd rd.md.uuid=1111:2222:3333:4444 \"\n"
	if got != want {
		t.Fatalf("dracutMirrorConf =\n%s\nwant\n%s", got, want)
	}
}

func TestConfigureMdadmAtWithoutMirrorDoesNothing(t *testing.T) {
	root := t.TempDir()
	cmd := newMockCommander()
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	if err := o.configureMdadmAt(context.Background(), root); err != nil {
		t.Fatalf("configureMdadmAt() error = %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 || len(cmd.calls) != 0 {
		t.Fatalf("configureMdadmAt() without mirror wrote %v and ran %v", entries, cmd.calls)
	}
}
