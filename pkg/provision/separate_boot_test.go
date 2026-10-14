//go:build linux

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

// Legacy evidence: provision.sh:366-395 mounts /boot from the image fstab
// when the root filesystem has no /boot/grub.
func TestSeparateBootSource(t *testing.T) {
	tests := []struct {
		name       string
		grubOnRoot bool
		grubFile   bool
		noBootDir  bool
		fstab      string
		wantNeeded bool
		wantSource string
	}{
		{name: "grub on root", grubOnRoot: true, fstab: "LABEL=BOOT /boot ext4 defaults 0 2\n"},
		{
			name:       "separate boot by label",
			fstab:      "# comment\nLABEL=cloudimg-rootfs / ext4 discard 0 1\nLABEL=BOOT /boot ext4 defaults 0 2\nLABEL=UEFI /boot/efi vfat umask=0077 0 1\n",
			wantNeeded: true,
			wantSource: "LABEL=BOOT",
		},
		{name: "grub is a file, not a directory", grubFile: true, fstab: "LABEL=BOOT /boot ext4 defaults 0 2\n", wantNeeded: true, wantSource: "LABEL=BOOT"},
		{name: "missing boot dir", noBootDir: true, fstab: "LABEL=BOOT /boot ext4 defaults 0 2\n", wantNeeded: true, wantSource: "LABEL=BOOT"},
		{name: "trailing slash", fstab: "LABEL=BOOT /boot/ ext4 defaults 0 2\n", wantNeeded: true, wantSource: "LABEL=BOOT"},
		{name: "double slash", fstab: "LABEL=BOOT //boot ext4 defaults 0 2\n", wantNeeded: true, wantSource: "LABEL=BOOT"},
		{name: "escaped label", fstab: "LABEL=BOOT\\040VOL /boot ext4 defaults 0 2\n", wantNeeded: true, wantSource: "LABEL=BOOT VOL"},
		{name: "no boot entry", fstab: "UUID=1 / ext4 defaults 0 1\n#LABEL=BOOT /boot ext4 defaults 0 2\n", wantNeeded: true},
		{name: "no fstab", wantNeeded: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if !tt.noBootDir {
				if err := os.MkdirAll(filepath.Join(root, "boot"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.grubFile {
				writeTestFile(t, filepath.Join(root, "boot", "grub"), "")
			}
			if tt.grubOnRoot {
				if err := os.MkdirAll(filepath.Join(root, "boot", "grub"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.fstab != "" {
				writeTestFile(t, filepath.Join(root, "etc", "fstab"), tt.fstab)
			}
			needed, source, err := separateBootSource(root)
			if err != nil {
				t.Fatalf("separateBootSource() error = %v", err)
			}
			if needed != tt.wantNeeded || source != tt.wantSource {
				t.Fatalf("separateBootSource() = (%v, %q), want (%v, %q)", needed, source, tt.wantNeeded, tt.wantSource)
			}
		})
	}
}

func TestSeparateBootSourceIgnoresSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "fstab"), "LABEL=BOOT /boot ext4 defaults 0 2\n")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if _, source, err := separateBootSource(root); err != nil || source != "" {
		t.Fatalf("separateBootSource() = %q, %v; must not read an fstab outside the root", source, err)
	}

	root2 := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root2, "boot")); err != nil {
		t.Fatal(err)
	}
	if needed, _, err := separateBootSource(root2); err != nil || needed {
		t.Fatalf("separateBootSource() = %v, %v; a /boot symlink must not be mounted over", needed, err)
	}
}

func TestParseBlkidExport(t *testing.T) {
	got := parseBlkidExport("DEVNAME=/dev/vda2\nLABEL=BOOT\\ VOL\nUUID=abcd\nTYPE=ext4\nPARTUUID=1234\n")
	if got.label != "BOOT VOL" || got.uuid != "abcd" || got.partUUID != "1234" {
		t.Fatalf("parseBlkidExport() = %+v", got)
	}
}

// Only one partition of the target disk may match, so a generic label on
// another disk or a cloned UUID is never mounted.
func TestMatchFstabSource(t *testing.T) {
	devs := []blockIDs{
		{node: "/dev/vda1", label: "UEFI", uuid: "1111"},
		{node: "/dev/vda2", label: "BOOT", uuid: "2222", partUUID: "p2"},
		{node: "/dev/vda3", label: "root", uuid: "3333"},
		{node: "/dev/vda4", label: "dup", uuid: "4444"},
		{node: "/dev/vda5", label: "dup", uuid: "5555"},
	}
	tests := []struct{ source, want string }{
		{"LABEL=BOOT", "/dev/vda2"},
		{"UUID=2222", "/dev/vda2"},
		{"PARTUUID=p2", "/dev/vda2"},
		{"/dev/vda2", "/dev/vda2"},
		{"/dev/sdb1", ""},
		{"LABEL=dup", ""},
		{"LABEL=missing", ""},
		{"tmpfs", ""},
	}
	for _, tt := range tests {
		if got := matchFstabSource(tt.source, devs); got != tt.want {
			t.Errorf("matchFstabSource(%q) = %q, want %q", tt.source, got, tt.want)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFstabSourceDeviceSymlink(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "vda2")
	writeTestFile(t, node, "")
	link := filepath.Join(dir, "by-uuid-abcd")
	if err := os.Symlink(node, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := matchFstabSource(resolved, []blockIDs{{node: node}}); got != node {
		t.Fatalf("matchFstabSource(%q) = %q, want %q", resolved, got, node)
	}
}

// A declared partition layout owns /boot and the generated fstab.
func TestMountSeparateBootSkipsPartitionLayout(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{}
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	if err := o.mountSeparateBoot(context.Background()); err != nil {
		t.Fatalf("mountSeparateBoot() = %v", err)
	}
	if len(cmd.calls) != 0 || o.separateBootMounted {
		t.Fatalf("partition layout run touched /boot: %v", cmd.calls)
	}
}

// funcCommander answers commands with a function, so a test can vary the
// output per partition.
type funcCommander func(name string, args ...string) ([]byte, error)

// FakeDisks: this commander changes no real disk (disk.FakeDisks).
func (f funcCommander) FakeDisks() {}

func (f funcCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f(name, args...)
}

func TestResolveFstabSourceOnTargetDisk(t *testing.T) {
	sfdisk := `{"partitiontable":{"label":"gpt","device":"/dev/vda","partitions":[
		{"node":"/dev/vda1","start":2048,"size":102400,"type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
		{"node":"/dev/vda2","start":104448,"size":131072,"type":"BC13C2FF-59E6-4262-A352-B275FD6F7172"},
		{"node":"/dev/vda3","start":235520,"size":900000,"type":"0FC63DAF-8483-4772-8E79-3D69D8477DE4"}]}}`
	blkid := map[string]string{
		"/dev/vda1": "LABEL=UEFI\nUUID=1111\n",
		"/dev/vda2": "LABEL=BOOT\\ VOL\nUUID=2222\nPARTUUID=p2\n",
		"/dev/vda3": "garbage",
	}
	cmd := funcCommander(func(name string, args ...string) ([]byte, error) {
		switch name {
		case "sfdisk":
			return []byte(sfdisk), nil
		case "blkid":
			out, ok := blkid[args[len(args)-1]]
			if !ok {
				return nil, errors.New("no signature")
			}
			return []byte(out), nil
		}
		return nil, nil
	})
	o := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(cmd))
	o.targetDisk = "/dev/vda"
	for source, want := range map[string]string{
		"LABEL=BOOT VOL": "/dev/vda2",
		"UUID=2222":      "/dev/vda2",
		"PARTUUID=p2":    "/dev/vda2",
		"LABEL=root":     "",
		"/dev/vdb1":      "",
	} {
		if got := o.resolveFstabSource(context.Background(), source); got != want {
			t.Errorf("resolveFstabSource(%q) = %q, want %q", source, got, want)
		}
	}

	failing := NewOrchestrator(&config.MachineConfig{}, &mockProvider{}, disk.NewManager(funcCommander(
		func(string, ...string) ([]byte, error) { return nil, errors.New("sfdisk failed") })))
	failing.targetDisk = "/dev/vda"
	if got := failing.resolveFstabSource(context.Background(), "LABEL=BOOT"); got != "" {
		t.Fatalf("resolveFstabSource() = %q with a failing sfdisk, want empty", got)
	}
}

func TestDecodeFstabField(t *testing.T) {
	for in, want := range map[string]string{"LABEL=BOOT\\040VOL": "LABEL=BOOT VOL", "/boot": "/boot", "a\\9xy": "a\\9xy"} {
		if got := decodeFstabField(in); got != want {
			t.Errorf("decodeFstabField(%q) = %q, want %q", in, got, want)
		}
	}
}

// The ESP mountpoint check after mounting /boot rejects an efi symlink out
// of the target root.
func TestESPMountpointMustStayInRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "boot", "efi")); err != nil {
		t.Fatal(err)
	}
	if err := ensureTargetParentWithinRoot(root, filepath.Join(root, "boot", "efi")); err == nil {
		t.Fatal("an efi symlink out of the root was accepted")
	}
	if err := ensureTargetParentWithinRoot(root, filepath.Join(root, "boot", "grub")); err != nil {
		t.Fatalf("a missing mountpoint below the root was rejected: %v", err)
	}
}

func TestCanonicalFstabSource(t *testing.T) {
	for in, want := range map[string]string{
		"/dev/disk/by-uuid/2222":          "UUID=2222",
		"/dev/disk/by-partuuid/p2":        "PARTUUID=p2",
		"/dev/disk/by-label/BOOT\\x20VOL": "LABEL=BOOT VOL",
		"LABEL=BOOT":                      "LABEL=BOOT",
		"/dev/vda2":                       "/dev/vda2",
	} {
		if got := canonicalFstabSource(in); got != want {
			t.Errorf("canonicalFstabSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseBlkidExportKeepsTrailingSpace(t *testing.T) {
	got := parseBlkidExport("LABEL=BOOT\\ \nUUID=1\n")
	if got.label != "BOOT " {
		t.Fatalf("label = %q, want %q", got.label, "BOOT ")
	}
	if matchFstabSource(decodeFstabField("LABEL=BOOT\\040"), []blockIDs{{node: "/dev/vda2", label: got.label}}) != "/dev/vda2" {
		t.Fatal("fstab LABEL=BOOT\\040 does not match the blkid label")
	}
}

// The ESP inside /boot must be unmounted before /boot itself.
func TestTeardownUnmountsESPBeforeSeparateBoot(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.DisableKexec = true
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	o.bootPartition = "/dev/vda1"
	o.separateBootMounted = true

	mounted := map[string]bool{bootEFIMountPoint(): true, separateBootMountPoint(): true}
	oldIsMountPoint := isMountPoint
	isMountPoint = func(path string) bool { return mounted[path] }
	t.Cleanup(func() { isMountPoint = oldIsMountPoint })
	var order []string
	restore := disk.SetUnmountFuncForTest(func(target string, _ int) error {
		order = append(order, target)
		delete(mounted, target)
		return nil
	})
	t.Cleanup(restore)

	_ = o.teardownChroot(context.Background())
	esp, boot := -1, -1
	for i, target := range order {
		switch target {
		case bootEFIMountPoint():
			esp = i
		case separateBootMountPoint():
			boot = i
		}
	}
	if esp < 0 || boot < 0 || esp > boot {
		t.Fatalf("unmount order = %v, want the ESP before /boot", order)
	}
	if o.separateBootMounted {
		t.Fatal("separate /boot still marked as mounted after teardown")
	}
}
