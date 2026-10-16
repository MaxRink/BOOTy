//go:build linux

package disk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// mockCommander records calls and returns preset results.
type mockCommander struct {
	calls     []mockCall
	results   map[string]mockResult
	sequences map[string][]mockResult
}

type mockCall struct {
	name string
	args []string
}

type mockResult struct {
	output []byte
	err    error
}

func newMockCommander() *mockCommander {
	return &mockCommander{results: make(map[string]mockResult), sequences: make(map[string][]mockResult)}
}

// FakeDisks: this commander changes no real disk (disk.FakeDisks).
func (m *mockCommander) FakeDisks() {}

func (m *mockCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, mockCall{name: name, args: args})
	key := name
	if len(args) > 0 {
		key = name + " " + args[0]
	}
	if sequence := m.sequences[key]; len(sequence) > 0 {
		r := sequence[0]
		m.sequences[key] = sequence[1:]
		return r.output, r.err
	}
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}
	// Default: success with empty output.
	return nil, nil
}

func (m *mockCommander) setResult(key string, output []byte, err error) {
	m.results[key] = mockResult{output: output, err: err}
}

func (m *mockCommander) setSequence(key string, results ...mockResult) {
	m.sequences[key] = results
}

func hasDiskCommandCall(calls []mockCall, name string, args ...string) bool {
	wantArgs := strings.Join(args, " ")
	for _, call := range calls {
		if call.name == name && strings.Join(call.args, " ") == wantArgs {
			return true
		}
	}
	return false
}

// makeExitError creates an *exec.ExitError with the given exit code.
func makeExitError(code int) error {
	err := exec.CommandContext(context.Background(), "sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	return err
}

func writeSysfsBlockDevice(t *testing.T, sysfs, dev, removable string, sizeGB int, serial string) {
	t.Helper()
	dir := sysfs + "/block/" + dev
	if err := os.MkdirAll(dir+"/device", 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(dir+"/removable", []byte(removable), 0o644); err != nil {
		t.Fatalf("write removable: %v", err)
	}
	sectors := int64(sizeGB) * 1024 * 1024 * 1024 / 512
	if err := os.WriteFile(dir+"/size", []byte(fmt.Sprintf("%d\n", sectors)), 0o644); err != nil {
		t.Fatalf("write size: %v", err)
	}
	if serial != "" {
		if err := os.WriteFile(dir+"/device/serial", []byte(serial+"\n"), 0o644); err != nil {
			t.Fatalf("write serial: %v", err)
		}
	}
}

func TestExitCodeFromError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{"exit 1", makeExitError(1), 1},
		{"exit 4", makeExitError(4), 4},
		{"exit 8", makeExitError(8), 8},
		{"plain error", fmt.Errorf("not an exit error"), -1},
		{"wrapped exit error", fmt.Errorf("wrapped: %w", makeExitError(4)), 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := exitCodeFromError(tt.err)
			if got != tt.expected {
				t.Errorf("exitCodeFromError() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestNewManagerDefault(t *testing.T) {
	mgr := NewManager(nil)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
		return
	}
	// Should use ExecCommander by default.
	if _, ok := mgr.cmd.(*ExecCommander); !ok {
		t.Fatal("expected ExecCommander as default")
	}
}

func TestNewManagerCustomCommander(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)
	if mgr.cmd != cmd {
		t.Fatal("expected custom commander")
	}
}

func TestBindMountSkipsAlreadyMountedTarget(t *testing.T) {
	mgr := NewManager(newMockCommander())
	if err := mgr.BindMount("/proc", "/proc"); err != nil {
		t.Fatalf("BindMount should skip already-mounted target: %v", err)
	}
}

func TestSetupChrootBindMountsRollsBackPartialFailure(t *testing.T) {
	oldMount := mountFunc
	oldUnmount := unmountFunc
	t.Cleanup(func() {
		mountFunc = oldMount
		unmountFunc = oldUnmount
	})

	root := t.TempDir()
	var mounted []string
	var unmounted []string
	mountFunc = func(source, target, _ string, _ uintptr, _ string) error {
		if source == "/sys" {
			return syscall.EPERM
		}
		mounted = append(mounted, target)
		return nil
	}
	unmountFunc = func(target string, _ int) error {
		unmounted = append(unmounted, target)
		return nil
	}

	mgr := NewManager(newMockCommander())
	err := mgr.SetupChrootBindMounts(root)
	if err == nil {
		t.Fatal("SetupChrootBindMounts() error = nil, want /sys bind failure")
	}
	if !strings.Contains(err.Error(), "bind mount /sys") {
		t.Fatalf("SetupChrootBindMounts() error = %v, want /sys bind failure", err)
	}

	wantMounted := []string{root + "/dev", root + "/proc"}
	if strings.Join(mounted, ",") != strings.Join(wantMounted, ",") {
		t.Fatalf("mounted = %v, want %v", mounted, wantMounted)
	}
	wantUnmounted := []string{root + "/proc", root + "/dev"}
	if strings.Join(unmounted, ",") != strings.Join(wantUnmounted, ",") {
		t.Fatalf("unmounted = %v, want %v", unmounted, wantUnmounted)
	}
}

func TestSetupChrootBindMountsDoesNotRollbackPreExistingMount(t *testing.T) {
	oldMount := mountFunc
	oldUnmount := unmountFunc
	oldIsMountPoint := isMountPointFunc
	t.Cleanup(func() {
		mountFunc = oldMount
		unmountFunc = oldUnmount
		isMountPointFunc = oldIsMountPoint
	})

	root := t.TempDir()
	preMounted := root + "/proc"
	var mounted []string
	var unmounted []string
	isMountPointFunc = func(path string) bool {
		return path == preMounted
	}
	mountFunc = func(source, target, _ string, _ uintptr, _ string) error {
		if source == "/sys" {
			return syscall.EPERM
		}
		mounted = append(mounted, target)
		return nil
	}
	unmountFunc = func(target string, _ int) error {
		unmounted = append(unmounted, target)
		return nil
	}

	mgr := NewManager(newMockCommander())
	err := mgr.SetupChrootBindMounts(root)
	if err == nil {
		t.Fatal("SetupChrootBindMounts() error = nil, want /sys bind failure")
	}

	wantMounted := []string{root + "/dev"}
	if strings.Join(mounted, ",") != strings.Join(wantMounted, ",") {
		t.Fatalf("mounted = %v, want %v", mounted, wantMounted)
	}
	wantUnmounted := []string{root + "/dev"}
	if strings.Join(unmounted, ",") != strings.Join(wantUnmounted, ",") {
		t.Fatalf("unmounted = %v, want %v", unmounted, wantUnmounted)
	}
}

func TestStopRAIDArrays(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	// Should succeed if mdadm reports that no arrays exist.
	cmd.setResult("mdadm --stop", []byte("mdadm: No arrays found in config file or automatically"), fmt.Errorf("exec mdadm: exit 1"))
	if err := mgr.StopRAIDArrays(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 1 || cmd.calls[0].name != "mdadm" {
		t.Fatalf("expected mdadm call, got %v", cmd.calls)
	}
}

func TestStopRAIDArraysError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("mdadm --stop", []byte("mdadm: cannot stop /dev/md0: Device or resource busy"), fmt.Errorf("exec mdadm: exit 1"))
	if err := mgr.StopRAIDArrays(context.Background()); err == nil {
		t.Fatal("expected error for genuine mdadm stop failure")
	}
}

func TestClearRAIDSuperblocks(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk -ln", []byte(""+
		"/dev/sda \n"+
		"/dev/sda1 vfat\n"+
		"/dev/sda2 linux_raid_member\n"+
		"/dev/sdb linux_raid_member\n"+
		"/dev/sdc \n"+
		"/dev/sdc1 ext4\n"+
		"/dev/nvme0n1p3 linux_raid_member\n"), nil)
	cmd.setSequence("mdadm --zero-superblock",
		mockResult{},
		// A failed zeroing is logged and the others still run.
		mockResult{output: []byte("mdadm: Couldn't open /dev/sdb for write"), err: errors.New("exit status 1")},
	)
	if err := NewManager(cmd).ClearRAIDSuperblocks(context.Background()); err != nil {
		t.Fatalf("ClearRAIDSuperblocks() error = %v", err)
	}
	var zeroed []string
	for _, call := range cmd.calls {
		if call.name == "mdadm" {
			if len(call.args) != 2 || call.args[0] != "--zero-superblock" {
				t.Fatalf("unexpected mdadm call %v", call.args)
			}
			zeroed = append(zeroed, call.args[1])
		}
	}
	if want := "/dev/sda2,/dev/sdb,/dev/nvme0n1p3"; strings.Join(zeroed, ",") != want {
		t.Fatalf("zeroed %v, want %s", zeroed, want)
	}
	if !hasDiskCommandCall(cmd.calls, "lsblk", "-ln", "-o", "PATH,FSTYPE") {
		t.Fatalf("lsblk not called for all block devices: %v", cmd.calls)
	}
}

func TestClearRAIDSuperblocksScopedToDisks(t *testing.T) {
	cmd := newMockCommander()
	if err := NewManager(cmd).ClearRAIDSuperblocks(context.Background(), "/dev/sda", "/dev/sdb"); err != nil {
		t.Fatalf("ClearRAIDSuperblocks() error = %v", err)
	}
	if !hasDiskCommandCall(cmd.calls, "lsblk", "-ln", "-o", "PATH,FSTYPE", "/dev/sda", "/dev/sdb") {
		t.Fatalf("lsblk not limited to the given disks: %v", cmd.calls)
	}
}

func TestClearRAIDSuperblocksListError(t *testing.T) {
	cmd := newMockCommander()
	cmd.setResult("lsblk -ln", []byte("lsblk: failed"), errors.New("exit status 1"))
	if err := NewManager(cmd).ClearRAIDSuperblocks(context.Background()); err == nil {
		t.Fatal("ClearRAIDSuperblocks() succeeded although lsblk failed")
	}
}

func TestWipeDiskValidationAndCommands(t *testing.T) {
	tests := []struct {
		name      string
		device    string
		wantErr   string
		wantCalls int
	}{
		{name: "empty device", device: "", wantErr: "wipe disk: device is required"},
		{name: "whitespace device", device: " \t ", wantErr: "wipe disk: device is required"},
		{name: "valid device", device: " /dev/sda ", wantCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			mgr := NewManager(cmd)
			mgr = mgr.WithQuickWipe(true)

			err := mgr.WipeDisk(context.Background(), tt.device)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("WipeDisk() error = %v, want %q", err, tt.wantErr)
				}
				if len(cmd.calls) != 0 {
					t.Fatalf("expected no command calls, got %d", len(cmd.calls))
				}
				return
			}

			if err != nil {
				t.Fatalf("WipeDisk() unexpected error = %v", err)
			}
			if got := len(cmd.calls); got != tt.wantCalls {
				t.Fatalf("expected %d command calls, got %d", tt.wantCalls, got)
			}
			if cmd.calls[0].name != "sgdisk" || strings.Join(cmd.calls[0].args, " ") != "--zap-all /dev/sda" {
				t.Fatalf("first command = %s %v, want sgdisk --zap-all /dev/sda", cmd.calls[0].name, cmd.calls[0].args)
			}
			if cmd.calls[1].name != "wipefs" || strings.Join(cmd.calls[1].args, " ") != "-af /dev/sda" {
				t.Fatalf("second command = %s %v, want wipefs -af /dev/sda", cmd.calls[1].name, cmd.calls[1].args)
			}
		})
	}
}

func TestSecureEraseDiskValidationAndCommands(t *testing.T) {
	tests := []struct {
		name      string
		device    string
		wantErr   string
		wantFirst mockCall
	}{
		{
			name:    "empty device",
			device:  "",
			wantErr: "secure erase disk: device is required",
		},
		{
			name:    "whitespace device",
			device:  " \t ",
			wantErr: "secure erase disk: device is required",
		},
		{
			name:   "sata device",
			device: " /dev/sda ",
			wantFirst: mockCall{
				name: "hdparm",
				args: []string{"-I", "/dev/sda"},
			},
		},
		{
			name:   "nvme device",
			device: "/dev/nvme0n1",
			wantFirst: mockCall{
				name: "nvme",
				args: []string{"format", "/dev/nvme0n1", "--ses=1", "--force"},
			},
		},
		{
			name: "nvme symlink uses resolved command path",
			device: func() string {
				dir := t.TempDir()
				target := filepath.Join(dir, "nvme0n1")
				if err := os.WriteFile(target, []byte("disk"), 0o600); err != nil {
					t.Fatalf("write target: %v", err)
				}
				link := filepath.Join(dir, "disk-by-id")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("symlink target: %v", err)
				}
				return link
			}(),
			wantFirst: mockCall{
				name: "nvme",
				args: []string{"format", "", "--ses=1", "--force"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			mgr := NewManager(cmd)

			err := mgr.SecureEraseDisk(context.Background(), tt.device)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("SecureEraseDisk() error = %v, want %q", err, tt.wantErr)
				}
				if len(cmd.calls) != 0 {
					t.Fatalf("expected no command calls, got %d", len(cmd.calls))
				}
				return
			}

			if err != nil {
				t.Fatalf("SecureEraseDisk() unexpected error = %v", err)
			}
			if len(cmd.calls) == 0 {
				t.Fatal("expected command calls")
			}
			if tt.name == "nvme symlink uses resolved command path" {
				tt.wantFirst.args[1], _ = filepath.EvalSymlinks(tt.device)
			}
			if cmd.calls[0].name != tt.wantFirst.name || strings.Join(cmd.calls[0].args, " ") != strings.Join(tt.wantFirst.args, " ") {
				t.Fatalf("first command = %s %v, want %s %v", cmd.calls[0].name, cmd.calls[0].args, tt.wantFirst.name, tt.wantFirst.args)
			}
		})
	}
}

func TestWipeFilesystemSignaturesOnlyUsesWipefs(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.WipeFilesystemSignatures(context.Background(), "/dev/sda3"); err != nil {
		t.Fatalf("WipeFilesystemSignatures: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("calls = %d, want 1: %#v", len(cmd.calls), cmd.calls)
	}
	if cmd.calls[0].name != "wipefs" || strings.Join(cmd.calls[0].args, " ") != "-af /dev/sda3" {
		t.Fatalf("call = %s %v, want wipefs -af /dev/sda3", cmd.calls[0].name, cmd.calls[0].args)
	}
}

func TestParsePartitions(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	sfdisk := sfdiskOutput{}
	sfdisk.PartitionTable.Partitions = []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID},
	}
	data, _ := json.Marshal(sfdisk)
	cmd.setResult("sfdisk --json", data, nil)

	parts, err := mgr.ParsePartitions(context.Background(), "/dev/sda")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(parts))
	}
	if parts[0].Node != "/dev/sda1" {
		t.Errorf("expected /dev/sda1, got %s", parts[0].Node)
	}
}

func TestParsePartitionsError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("sfdisk --json", nil, fmt.Errorf("exec sfdisk: exit 1"))
	_, err := mgr.ParsePartitions(context.Background(), "/dev/sda")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParsePartitionsNoPartitionTable(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	// sfdisk reports "does not contain a recognized partition table" for empty disks.
	// ParsePartitions should return nil (empty list), not an error.
	cmd.setResult("sfdisk --json",
		[]byte("sfdisk: /dev/loop0: does not contain a recognized partition table"),
		fmt.Errorf("exec sfdisk: exit status 1"),
	)
	parts, err := mgr.ParsePartitions(context.Background(), "/dev/loop0")
	if err != nil {
		t.Fatalf("expected nil error for empty partition table, got: %v", err)
	}
	if len(parts) != 0 {
		t.Errorf("expected 0 partitions, got %d", len(parts))
	}
}

func TestParsePartitionsInvalidJSON(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("sfdisk --json", []byte("not json"), nil)
	_, err := mgr.ParsePartitions(context.Background(), "/dev/sda")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParsePartitionsWithGPTWarningPrefix(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	sfdisk := sfdiskOutput{}
	sfdisk.PartitionTable.Partitions = []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
	}
	data, _ := json.Marshal(sfdisk)
	// Simulate sfdisk stderr warning merged before JSON via CombinedOutput.
	prefixed := append([]byte("GPT PMBR size mismatch (7340031 != 488397167) will be corrected by write.\n"), data...)
	cmd.setResult("sfdisk --json", prefixed, nil)

	parts, err := mgr.ParsePartitions(context.Background(), "/dev/sda")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 || parts[0].Node != "/dev/sda1" {
		t.Errorf("expected 1 partition /dev/sda1, got %v", parts)
	}
}

func TestFindBootPartition(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID},
	}
	boot, err := mgr.FindBootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if boot.Node != "/dev/sda1" {
		t.Errorf("expected /dev/sda1, got %s", boot.Node)
	}
}

func TestFindBootPartitionNotFound(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: LinuxFilesystemGUID},
	}
	_, err := mgr.FindBootPartition(parts)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFindRootPartition(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID},
	}
	root, err := mgr.FindRootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.Node != "/dev/sda2" {
		t.Errorf("expected /dev/sda2, got %s", root.Node)
	}
}

func TestFindRootPartitionAcceptsMBRLinuxType(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: LinuxFilesystemMBRType},
	}
	root, err := mgr.FindRootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.Node != "/dev/sda1" {
		t.Errorf("expected /dev/sda1, got %s", root.Node)
	}
}

func TestFindRootPartitionNotFound(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
	}
	_, err := mgr.FindRootPartition(parts)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFindRootPartitionRejectsAmbiguousLinuxPartitions(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID, Name: "boot"},
		{Node: "/dev/sda3", Type: LinuxFilesystemGUID, Name: ""},
	}
	_, err := mgr.FindRootPartition(parts)
	if err == nil {
		t.Fatal("expected ambiguous root partition error")
	}
	if !strings.Contains(err.Error(), "ambiguous Linux root partition candidates") {
		t.Fatalf("error = %q, want ambiguity context", err.Error())
	}
}

func TestFindRootPartitionPrefersNamed(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID, Name: "boot"},
		{Node: "/dev/sda3", Type: LinuxFilesystemGUID, Name: "root"},
		{Node: "/dev/sda4", Type: LinuxFilesystemGUID, Name: "data"},
	}
	root, err := mgr.FindRootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.Node != "/dev/sda3" {
		t.Errorf("expected /dev/sda3 (named root), got %s", root.Node)
	}
}

func TestFindRootPartitionAcceptsSlashName(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID, Name: "boot"},
		{Node: "/dev/sda3", Type: LinuxFilesystemGUID, Name: "/"},
		{Node: "/dev/sda4", Type: LinuxFilesystemGUID, Name: "data"},
	}
	root, err := mgr.FindRootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.Node != "/dev/sda3" {
		t.Errorf("expected /dev/sda3 (named /), got %s", root.Node)
	}
}

func TestFindRootPartitionRejectsMultipleNamedRoots(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID, Name: "root"},
		{Node: "/dev/sda3", Type: LinuxFilesystemGUID, Name: "/"},
	}
	_, err := mgr.FindRootPartition(parts)
	if err == nil {
		t.Fatal("expected multiple root partition error")
	}
	if !strings.Contains(err.Error(), "multiple partitions named root or /") {
		t.Fatalf("error = %q, want multiple roots context", err.Error())
	}
}

func TestFindRootPartitionRejectsBOOTyABLayoutWithoutExplicitRoot(t *testing.T) {
	mgr := NewManager(newMockCommander())
	parts := []Partition{
		{Node: "/dev/sda1", Type: EFISystemPartitionGUID, Name: "BOOTY-EFI"},
		{Node: "/dev/sda2", Type: LinuxFilesystemGUID, Name: "BOOTY-ROOT-A"},
		{Node: "/dev/sda3", Type: LinuxFilesystemGUID, Name: "BOOTY-ROOT-B"},
		{Node: "/dev/sda4", Type: LinuxFilesystemGUID, Name: "BOOTY-STATE"},
	}
	_, err := mgr.FindRootPartition(parts)
	if err == nil {
		t.Fatal("expected ambiguous A/B root partition error")
	}
	if !strings.Contains(err.Error(), "BOOTY-ROOT-A") ||
		!strings.Contains(err.Error(), "BOOTY-ROOT-B") ||
		!strings.Contains(err.Error(), "BOOTY-STATE") {
		t.Fatalf("error = %q, want A/B candidate names", err.Error())
	}
}

func TestFindRootPartitionRejectsUnsupportedTargetPartitionTypes(t *testing.T) {
	const (
		microsoftBasicDataGUID = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"
		vmwareVMFSGUID         = "AA31E02A-400F-11DB-9590-000C2911D1B8"
	)

	tests := []struct {
		name string
		part Partition
	}{
		{
			name: "windows basic data",
			part: Partition{Node: "/dev/sda3", Type: microsoftBasicDataGUID, Name: "Windows"},
		},
		{
			name: "vmware vmfs",
			part: Partition{Node: "/dev/sda3", Type: vmwareVMFSGUID, Name: "datastore1"},
		},
	}
	mgr := NewManager(newMockCommander())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mgr.FindRootPartition([]Partition{
				{Node: "/dev/sda1", Type: EFISystemPartitionGUID},
				tt.part,
			})
			if err == nil {
				t.Fatal("expected unsupported target partition type error")
			}
			if !strings.Contains(err.Error(), "no Linux filesystem partition found") {
				t.Fatalf("error = %q, want no Linux filesystem context", err.Error())
			}
		})
	}
}

func TestGrowPartitionSuccess(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.GrowPartition(context.Background(), "/dev/sda", 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("expected growpart call, got %v", cmd.calls)
	}
	if got := cmd.calls[0]; got.name != "growpart" || strings.Join(got.args, " ") != "--update on /dev/sda 2" {
		t.Fatalf("expected growpart --update on /dev/sda 2, got %s %v", got.name, got.args)
	}
}

func TestGrowPartitionNoChange(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("growpart --update", []byte("NOCHANGE: partition already fills disk"), fmt.Errorf("exit 1"))
	if err := mgr.GrowPartition(context.Background(), "/dev/sda", 2); err != nil {
		t.Fatalf("unexpected error for NOCHANGE: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("NOCHANGE should not refresh partition table, got %v", cmd.calls)
	}
}

func TestChrootRunSuccess(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("chroot /newroot", []byte("hello"), nil)
	out, err := mgr.ChrootRun(context.Background(), "/newroot", "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("expected 'hello', got %q", string(out))
	}
}

func TestChrootRunCommandError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("chroot /newroot", []byte("error output"), fmt.Errorf("exec chroot: exit 1"))
	_, err := mgr.ChrootRun(context.Background(), "/newroot", "false")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "chroot exec") {
		t.Errorf("expected 'chroot exec' in error, got: %v", err)
	}
}

func TestMountedSubpathsFromRejectsUnsafeRoots(t *testing.T) {
	for _, root := range []string{"", "/", "relative"} {
		t.Run(fmt.Sprintf("root=%q", root), func(t *testing.T) {
			if _, err := mountedSubpathsFrom(root, []byte("dev /newroot ext4 rw 0 0\n")); err == nil {
				t.Fatal("mountedSubpathsFrom() error = nil, want unsafe-root error")
			}
		})
	}
}

func TestMountedSubpathsFromScopesAndSortsDeepestFirst(t *testing.T) {
	data := []byte(strings.Join([]string{
		"rootfs / rootfs rw 0 0",
		"dev /newroot ext4 rw 0 0",
		"proc /newroot/proc proc rw 0 0",
		"sys /newroot/proc/sys sysfs rw 0 0",
		"other /newroot-old ext4 rw 0 0",
		"",
	}, "\n"))

	got, err := mountedSubpathsFrom("/newroot/", data)
	if err != nil {
		t.Fatalf("mountedSubpathsFrom() error = %v", err)
	}
	want := []string{"/newroot/proc/sys", "/newroot/proc", "/newroot"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("targets = %v, want %v", got, want)
	}
}

func TestUnmountRecursiveIgnoresDisappearingMounts(t *testing.T) {
	oldUnmount := unmountFunc
	oldReadMounts := readMountsFile
	t.Cleanup(func() {
		unmountFunc = oldUnmount
		readMountsFile = oldReadMounts
	})

	readMountsFile = func(string) ([]byte, error) {
		return []byte(strings.Join([]string{
			"dev /newroot ext4 rw 0 0",
			"proc /newroot/proc proc rw 0 0",
			"",
		}, "\n")), nil
	}
	var targets []string
	unmountFunc = func(target string, _ int) error {
		targets = append(targets, target)
		if target == "/newroot/proc" {
			return syscall.EINVAL
		}
		return syscall.ENOENT
	}

	mgr := NewManager(newMockCommander())
	if err := mgr.UnmountRecursive("/newroot"); err != nil {
		t.Fatalf("UnmountRecursive() error = %v, want disappearing mounts ignored", err)
	}
	want := []string{"/newroot/proc", "/newroot"}
	if strings.Join(targets, ",") != strings.Join(want, ",") {
		t.Fatalf("unmounted = %v, want %v", targets, want)
	}
}

func TestChrootRunFallbackOnNotFound(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	// Simulate chroot binary not found.
	cmd.setResult("chroot /nonexistent", nil, fmt.Errorf("exec chroot: %w", exec.ErrNotFound))

	// The fallback will try to exec /bin/bash with SysProcAttr.Chroot.
	// In test context this will fail because /nonexistent doesn't exist,
	// but we verify the fallback path is triggered (not the "chroot exec" error).
	_, err := mgr.ChrootRun(context.Background(), "/nonexistent", "echo hi")
	if err == nil {
		t.Fatal("expected error (nonexistent root)")
	}
	// Should be a syscall fallback error, not "chroot exec" error.
	if strings.Contains(err.Error(), "chroot exec") {
		t.Error("expected syscall fallback error, got chroot exec error")
	}
}

func TestChrootSyscallFallsBackToShWhenBashMissing(t *testing.T) {
	mgr := NewManager(newMockCommander())
	previous := runChrootSyscallCommand
	t.Cleanup(func() {
		runChrootSyscallCommand = previous
	})

	var shells []string
	runChrootSyscallCommand = func(_ context.Context, _, shell, _ string) ([]byte, error) {
		shells = append(shells, shell)
		if shell == "/bin/bash" {
			return nil, fmt.Errorf("fork/exec /bin/bash: no such file or directory")
		}
		return []byte("ok\n"), nil
	}

	out, err := mgr.chrootSyscall(context.Background(), "/target", "echo ok")
	if err != nil {
		t.Fatalf("chrootSyscall() error = %v", err)
	}
	if string(out) != "ok\n" {
		t.Fatalf("output = %q, want ok", out)
	}
	want := []string{"/bin/bash", "/bin/sh"}
	if len(shells) != len(want) {
		t.Fatalf("shells = %v, want %v", shells, want)
	}
	for i := range want {
		if shells[i] != want[i] {
			t.Fatalf("shells = %v, want %v", shells, want)
		}
	}
}

func TestChrootSyscallDoesNotFallbackOnBashCommandFailure(t *testing.T) {
	mgr := NewManager(newMockCommander())
	previous := runChrootSyscallCommand
	t.Cleanup(func() {
		runChrootSyscallCommand = previous
	})

	var shells []string
	runChrootSyscallCommand = func(_ context.Context, _, shell, _ string) ([]byte, error) {
		shells = append(shells, shell)
		return []byte("permission denied\n"), fmt.Errorf("exit status 126")
	}

	_, err := mgr.chrootSyscall(context.Background(), "/target", "echo ok")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "chroot syscall /bin/bash") {
		t.Fatalf("error = %q, want bash syscall context", err.Error())
	}
	if len(shells) != 1 || shells[0] != "/bin/bash" {
		t.Fatalf("shells = %v, want only /bin/bash", shells)
	}
}

func TestIsExecNotFound(t *testing.T) {
	if !isExecNotFound(exec.ErrNotFound) {
		t.Error("expected true for exec.ErrNotFound")
	}
	if !isExecNotFound(fmt.Errorf("exec chroot: %w", exec.ErrNotFound)) {
		t.Error("expected true for wrapped exec.ErrNotFound")
	}
	if !isExecNotFound(fmt.Errorf("executable file not found in $PATH")) {
		t.Error("expected true for message-based detection")
	}
	if isExecNotFound(fmt.Errorf("some other error")) {
		t.Error("expected false for unrelated error")
	}
}

func TestIsBashNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bash missing in chroot", fmt.Errorf("exec chroot: exit status 127 [output: chroot: can't execute '/bin/bash': No such file or directory]"), true},
		{"busybox bash applet missing in chroot", fmt.Errorf("exec chroot: exit status 127 [output: bash: applet not found]"), true},
		{"exit 127 without no such file", fmt.Errorf("exit status 127"), false},
		{"no such file without 127", fmt.Errorf("No such file or directory"), false},
		{"bash applet error without 127", fmt.Errorf("bash: applet not found"), false},
		{"normal error", fmt.Errorf("exec chroot: exit status 1"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBashNotFound(tt.err); got != tt.want {
				t.Errorf("isBashNotFound() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChrootRunBashNotFoundFallsBackToSh(t *testing.T) {
	testChrootRunBashUnavailableFallsBackToSh(t, fmt.Errorf("exec chroot: exit status 127 [output: chroot: can't execute '/bin/bash': No such file or directory]"))
}

func TestChrootRunBusyboxBashAppletMissingFallsBackToSh(t *testing.T) {
	testChrootRunBashUnavailableFallsBackToSh(t, fmt.Errorf("exec chroot: exit status 127 [output: bash: applet not found]"))
}

func testChrootRunBashUnavailableFallsBackToSh(t *testing.T, bashErr error) {
	t.Helper()

	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("chroot /newroot", nil, bashErr)

	// The fallback /bin/sh call uses the same mock key, so it also errors.
	// We verify that bash-unavailable detection triggers and /bin/sh is attempted.
	_, _ = mgr.ChrootRun(context.Background(), "/newroot", "ls /dev/mst/")

	// Verify both /bin/bash and /bin/sh were attempted.
	var bashCall, shCall bool
	for _, c := range cmd.calls {
		if c.name == "chroot" && len(c.args) >= 2 {
			switch c.args[1] {
			case "/bin/bash":
				bashCall = true
			case "/bin/sh":
				shCall = true
			}
		}
	}
	if !bashCall {
		t.Error("expected /bin/bash attempt")
	}
	if !shCall {
		t.Error("expected /bin/sh fallback attempt after bash is unavailable")
	}
}

func TestGrowPartitionError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("growpart --update", []byte("error"), fmt.Errorf("exit 1"))
	if err := mgr.GrowPartition(context.Background(), "/dev/sda", 2); err == nil {
		t.Fatal("expected error")
	}
}

func TestResizeFilesystemExt4(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", "/newroot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 2 || cmd.calls[1].name != "resize2fs" {
		t.Fatalf("expected resize2fs call, got %v", cmd.calls)
	}
}

func TestResizeFilesystemUsesDetectedType(t *testing.T) {
	t.Run("ext4 failure reports resize2fs", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("blkid -o", []byte("ext4\n"), nil)
		cmd.setResult("resize2fs /dev/sda2", []byte("Bad magic number in super-block"), fmt.Errorf("exit 1"))
		err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", "/newroot")
		if err == nil || !strings.Contains(err.Error(), "Bad magic number") {
			t.Fatalf("error = %v, want the resize2fs output", err)
		}
		if hasDiskCommandCall(cmd.calls, "xfs_growfs", "/newroot") {
			t.Fatalf("ext4 must not fall back to xfs_growfs: %#v", cmd.calls)
		}
	})
	for _, tt := range []struct {
		fsType   string
		wantName string
		wantArgs []string
	}{
		{"xfs", "xfs_growfs", []string{"/newroot"}},
		{"btrfs", "btrfs", []string{"filesystem", "resize", "max", "/newroot"}},
	} {
		t.Run(tt.fsType+" runs only its tool on the mountpoint", func(t *testing.T) {
			cmd := newMockCommander()
			mgr := NewManager(cmd)
			cmd.setResult("blkid -o", []byte(tt.fsType+"\n"), nil)
			if err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", "/newroot"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cmd.calls) != 2 || !hasDiskCommandCall(cmd.calls[1:], tt.wantName, tt.wantArgs...) {
				t.Fatalf("expected blkid then %s %v, got %#v", tt.wantName, tt.wantArgs, cmd.calls)
			}
		})
	}
}

func TestResizeFilesystemVFATUnsupported(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("blkid -o", []byte("vfat\n"), nil)
	err := mgr.ResizeFilesystem(context.Background(), "/dev/sda1", "/newroot/boot/efi")
	if err == nil {
		t.Fatal("expected unsupported vfat resize error")
	}
	if !strings.Contains(err.Error(), "vfat resize is not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("vfat resize should not try resize tools, got %#v", cmd.calls)
	}
}

func TestResizeFilesystemXFS(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("resize2fs /dev/sda2", nil, fmt.Errorf("not ext4"))
	if err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", "/newroot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 3 {
		t.Fatalf("expected 3 calls (blkid, resize2fs, xfs_growfs), got %d", len(cmd.calls))
	}
	if cmd.calls[2].name != "xfs_growfs" {
		t.Errorf("expected xfs_growfs, got %s", cmd.calls[2].name)
	}
	if len(cmd.calls[2].args) != 1 || cmd.calls[2].args[0] != "/newroot" {
		t.Fatalf("expected xfs_growfs /newroot, got %s %v", cmd.calls[2].name, cmd.calls[2].args)
	}
}

func TestResizeFilesystemBothFail(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("resize2fs /dev/sda2", nil, fmt.Errorf("not ext4"))
	cmd.setResult("xfs_growfs /newroot", nil, fmt.Errorf("not xfs"))
	cmd.setResult("btrfs filesystem", nil, fmt.Errorf("not btrfs"))
	err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", "/newroot")
	if err == nil {
		t.Fatal("expected error when all resize methods fail")
	}
	if !strings.Contains(err.Error(), "/newroot") {
		t.Fatalf("error should include mountpoint, got %v", err)
	}
}

func TestResizeFilesystemFallbackRequiresMountpoint(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("resize2fs /dev/sda2", nil, fmt.Errorf("not ext4"))
	err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", " ")
	if err == nil {
		t.Fatal("expected error when fallback needs empty mountpoint")
	}
	if !strings.Contains(err.Error(), "requires mountpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 2 {
		t.Fatalf("fallback commands should not run, got %#v", cmd.calls)
	}
}

func TestResizeFilesystemBtrfsUsesMountpoint(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("resize2fs /dev/sda2", nil, fmt.Errorf("not ext4"))
	cmd.setResult("xfs_growfs /newroot", nil, fmt.Errorf("not xfs"))
	if err := mgr.ResizeFilesystem(context.Background(), "/dev/sda2", "/newroot"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 4 {
		t.Fatalf("expected 4 calls, got %#v", cmd.calls)
	}
	got := cmd.calls[3]
	if got.name != "btrfs" || strings.Join(got.args, " ") != "filesystem resize max /newroot" {
		t.Fatalf("expected btrfs filesystem resize max /newroot, got %s %v", got.name, got.args)
	}
}

func TestPartProbe(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.PartProbe(context.Background(), "/dev/sda"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPartProbeError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	// Both partprobe and blockdev fallback fail → error expected.
	cmd.setResult("partprobe /dev/sda", nil, fmt.Errorf("exit 1"))
	cmd.setResult("blockdev --rereadpt", nil, fmt.Errorf("exit 1"))
	if err := mgr.PartProbe(context.Background(), "/dev/sda"); err == nil {
		t.Fatal("expected error")
	}
}

func TestPartProbeFallback(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	// partprobe fails but blockdev --rereadpt succeeds → no error.
	cmd.setResult("partprobe /dev/sda", nil, fmt.Errorf("exit 1"))
	if err := mgr.PartProbe(context.Background(), "/dev/sda"); err != nil {
		t.Fatalf("expected fallback to succeed: %v", err)
	}
}

func TestSecureEraseSATA(t *testing.T) {
	t.Run("erase succeeds", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("hdparm -I", []byte("Security:\n  supported"), nil)
		if err := mgr.secureEraseSATA(context.Background(), "/dev/sda"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("no security support falls back to wipefs", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("hdparm -I", []byte("no security here"), nil)
		if err := mgr.secureEraseSATA(context.Background(), "/dev/sda"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing hdparm fails without wipefs fallback", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("hdparm -I", nil, fmt.Errorf("exec hdparm: %w", exec.ErrNotFound))

		err := mgr.secureEraseSATA(context.Background(), "/dev/sda")
		if err == nil {
			t.Fatal("expected missing hdparm error")
		}
		if !strings.Contains(err.Error(), "hdparm secure erase tool is required") {
			t.Fatalf("error = %v, want missing hdparm diagnostic", err)
		}
		if hasDiskCommandCall(cmd.calls, "wipefs", "-af", "/dev/sda") {
			t.Fatalf("secure erase must not fall back to wipefs when hdparm is missing: %#v", cmd.calls)
		}
	})

	t.Run("set-pass failure returns error", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("hdparm -I", []byte("Security:\n  supported"), nil)
		cmd.setResult("hdparm --user-master", nil, fmt.Errorf("set-pass failed"))
		err := mgr.secureEraseSATA(context.Background(), "/dev/sda")
		if err == nil {
			t.Fatal("expected error when set-pass fails")
		}
		if !strings.Contains(err.Error(), "failed to set security password") {
			t.Fatalf("error should mention security password: %v", err)
		}
	})

	t.Run("frozen drive falls back to wipefs", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("hdparm -I", []byte("Security:\n  frozen"), nil)
		if err := mgr.secureEraseSATA(context.Background(), "/dev/sda"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestSecureEraseNVMEMissingToolFailsWithoutWipefsFallback(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)
	cmd.setResult("nvme format", nil, fmt.Errorf("exec nvme: %w", exec.ErrNotFound))

	err := mgr.secureEraseNVMe(context.Background(), "/dev/nvme0n1")
	if err == nil {
		t.Fatal("expected missing nvme error")
	}
	if !strings.Contains(err.Error(), "nvme secure erase tool is required") {
		t.Fatalf("error = %v, want missing nvme diagnostic", err)
	}
	if hasDiskCommandCall(cmd.calls, "wipefs", "-af", "/dev/nvme0n1") {
		t.Fatalf("secure erase must not fall back to wipefs when nvme is missing: %#v", cmd.calls)
	}
}

func TestCheckFilesystem(t *testing.T) {
	t.Run("exit 0 clean", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		if err := mgr.CheckFilesystem(context.Background(), "/dev/sda2"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("exit 1 errors corrected", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		exitErr := makeExitError(1)
		cmd.setResult("e2fsck -fy", nil, exitErr)
		if err := mgr.CheckFilesystem(context.Background(), "/dev/sda2"); err != nil {
			t.Fatalf("exit code 1 should be acceptable: %v", err)
		}
	})

	t.Run("exit 4 uncorrectable", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		exitErr := makeExitError(4)
		cmd.setResult("e2fsck -fy", nil, exitErr)
		err := mgr.CheckFilesystem(context.Background(), "/dev/sda2")
		if err == nil {
			t.Fatal("expected error for exit code 4 (uncorrectable)")
		}
		if !strings.Contains(err.Error(), "uncorrectable") {
			t.Fatalf("error should mention uncorrectable: %v", err)
		}
	})

	t.Run("exit 8 operational failure", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		exitErr := makeExitError(8)
		cmd.setResult("e2fsck -fy", nil, exitErr)
		err := mgr.CheckFilesystem(context.Background(), "/dev/sda2")
		if err == nil {
			t.Fatal("expected error for exit code 8")
		}
	})

	t.Run("killed or canceled run fails", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("e2fsck -fy", nil, context.Canceled)
		if err := mgr.CheckFilesystem(context.Background(), "/dev/sda2"); err == nil {
			t.Fatal("expected error when e2fsck did not complete")
		}
	})

	t.Run("missing e2fsck skips the check", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("e2fsck -fy", nil, &exec.Error{Name: "e2fsck", Err: exec.ErrNotFound})
		if err := mgr.CheckFilesystem(context.Background(), "/dev/sda2"); err != nil {
			t.Fatalf("missing e2fsck should skip the check: %v", err)
		}
	})

	t.Run("vfat unsupported", func(t *testing.T) {
		cmd := newMockCommander()
		mgr := NewManager(cmd)
		cmd.setResult("blkid -o", []byte("vfat\n"), nil)
		err := mgr.CheckFilesystem(context.Background(), "/dev/sda1")
		if err == nil {
			t.Fatal("expected unsupported vfat check error")
		}
		if !strings.Contains(err.Error(), "vfat fsck is not supported") {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cmd.calls) != 1 {
			t.Fatalf("vfat check should not try e2fsck, got %#v", cmd.calls)
		}
	})
}

func TestEnableLVM(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.EnableLVM(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) == 0 || cmd.calls[0].name != "lvm" {
		t.Fatal("expected lvm call")
	}
}

func TestEnableLVMError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("lvm vgchange", nil, fmt.Errorf("no vg found"))
	if err := mgr.EnableLVM(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestChrootRun(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("chroot /newroot", []byte("output"), nil)
	out, err := mgr.ChrootRun(context.Background(), "/newroot", "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "output" {
		t.Errorf("expected 'output', got %q", string(out))
	}
}

func TestIsVirtualDisk(t *testing.T) {
	tests := []struct {
		name   string
		expect bool
	}{
		{"loop0", true},
		{"sr0", true},
		{"ram0", true},
		{"dm-0", true},
		{"zram0", true},
		{"md0", true},
		{"zd0", true},
		{"nbd0", true},
		{"sda", false},
		{"nvme0n1", false},
		{"nvme0c0n1", true},
		{"nvme12c3n2", true},
		{"vda", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVirtualDisk(tt.name); got != tt.expect {
				t.Errorf("isVirtualDisk(%q) = %v, want %v", tt.name, got, tt.expect)
			}
		})
	}
}

func TestFindPartitionsCaseInsensitive(t *testing.T) {
	mgr := NewManager(newMockCommander())
	// Test that GUID matching is case-insensitive.
	parts := []Partition{
		{Node: "/dev/sda1", Type: "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"}, // lowercase
		{Node: "/dev/sda2", Type: "0fc63daf-8483-4772-8e79-3d69d8477de4"}, // lowercase
	}
	boot, err := mgr.FindBootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if boot.Node != "/dev/sda1" {
		t.Errorf("expected /dev/sda1, got %s", boot.Node)
	}
	root, err := mgr.FindRootPartition(parts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.Node != "/dev/sda2" {
		t.Errorf("expected /dev/sda2, got %s", root.Node)
	}
}

func TestDisableLVM(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.DisableLVM(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 1 || cmd.calls[0].name != "lvm" {
		t.Fatalf("expected lvm call, got %v", cmd.calls)
	}
	if cmd.calls[0].args[0] != "vgchange" || cmd.calls[0].args[1] != "-an" {
		t.Errorf("expected vgchange -an, got %v", cmd.calls[0].args)
	}
}

func TestDisableLVMSuccess(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	if err := mgr.DisableLVM(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsRemovableMedia(t *testing.T) {
	sysfs := t.TempDir()

	writeRemovable := func(dev, value string) {
		t.Helper()
		dir := sysfs + "/block/" + dev
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(dir+"/removable", []byte(value), 0o644); err != nil {
			t.Fatalf("write removable: %v", err)
		}
	}

	writeRemovable("sda", "0\n")
	writeRemovable("sdb", "1\n")
	writeRemovable("sdc", "1")

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)

	if mgr.isRemovableMedia("sda") {
		t.Error("sda: expected non-removable (value 0)")
	}
	if !mgr.isRemovableMedia("sdb") {
		t.Error("sdb: expected removable (value 1)")
	}
	if !mgr.isRemovableMedia("sdc") {
		t.Error("sdc: expected removable (value 1, no newline)")
	}
	if !mgr.isRemovableMedia("sdz") {
		t.Error("sdz: missing sysfs file should be treated as removable (fail closed)")
	}
}

func TestIsRemovableMediaAllowEnv(t *testing.T) {
	sysfs := t.TempDir()

	writeDevice := func(dev, removable, sectors string) {
		t.Helper()
		dir := sysfs + "/block/" + dev
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(dir+"/removable", []byte(removable), 0o644); err != nil {
			t.Fatalf("write removable: %v", err)
		}
		if err := os.WriteFile(dir+"/size", []byte(sectors), 0o644); err != nil {
			t.Fatalf("write size: %v", err)
		}
	}

	// sda is removable; large enough to be selected if env var is set.
	writeDevice("sda", "1\n", "419430400\n") // 200 GB in 512-byte sectors

	t.Setenv("BOOTY_ALLOW_REMOVABLE", "true")

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	disk, err := mgr.DetectDisk(t.Context(), 0)
	if err != nil {
		t.Fatalf("DetectDisk: unexpected error: %v", err)
	}
	if disk != "/dev/sda" {
		t.Errorf("expected /dev/sda to be selected, got %q", disk)
	}
}

// A disk that reports 0 sectors is not a candidate for the automatic
// selection, also when it is NVMe.
func TestDetectDiskPreferNVMeSkipsZeroSectorDisk(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "nvme0n1", "0\n", 0, "")
	writeSysfsBlockDevice(t, sysfs, "sda", "0\n", 100, "")

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	disk, err := mgr.DetectDisk(t.Context(), 0)
	if err != nil {
		t.Fatalf("DetectDisk: unexpected error: %v", err)
	}
	if disk != "/dev/sda" {
		t.Fatalf("DetectDisk = %q, want /dev/sda", disk)
	}
}

func TestFindDiskBySerialSelectsMatchingDisk(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sda", "0\n", 40, "BOOT-DISK")
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "RAID-DISK-1")

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	disk, err := mgr.FindDiskBySerial(t.Context(), "RAID-DISK-1", 64)
	if err != nil {
		t.Fatalf("FindDiskBySerial: unexpected error: %v", err)
	}
	if disk != "/dev/sdb" {
		t.Fatalf("FindDiskBySerial = %q, want /dev/sdb", disk)
	}
}

func TestFindDiskBySerialFallsBackToLSBLK(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "")

	cmd := newMockCommander()
	cmd.setResult("lsblk --nodeps", []byte("/dev/sda BOOT-DISK\n/dev/sdb RAID-DISK-1\n"), nil)

	mgr := newManagerWithSysfs(cmd, sysfs)
	disk, err := mgr.FindDiskBySerial(t.Context(), "RAID-DISK-1", 64)
	if err != nil {
		t.Fatalf("FindDiskBySerial: unexpected error: %v", err)
	}
	if disk != "/dev/sdb" {
		t.Fatalf("FindDiskBySerial = %q, want /dev/sdb", disk)
	}
}

func TestFindDiskBySerialMatchesExactWWID(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "")
	wwid := "scsi-0QEMU_QEMU_HARDDISK_RAID-DISK-1"
	if err := os.WriteFile(sysfs+"/block/sdb/device/wwid", []byte(wwid+"\n"), 0o644); err != nil {
		t.Fatalf("write wwid: %v", err)
	}

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	disk, err := mgr.FindDiskBySerial(t.Context(), wwid, 64)
	if err != nil {
		t.Fatalf("FindDiskBySerial: unexpected error: %v", err)
	}
	if disk != "/dev/sdb" {
		t.Fatalf("FindDiskBySerial = %q, want /dev/sdb", disk)
	}
}

func TestFindDiskBySerialMatchesExactVPDIdentifier(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "")
	if err := os.WriteFile(sysfs+"/block/sdb/device/vpd_pg80", []byte{0x00, 0x80, 0x00, 0x0b, 'R', 'A', 'I', 'D', '-', 'D', 'I', 'S', 'K', '-', '1'}, 0o644); err != nil {
		t.Fatalf("write vpd_pg80: %v", err)
	}

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	disk, err := mgr.FindDiskBySerial(t.Context(), "RAID-DISK-1", 64)
	if err != nil {
		t.Fatalf("FindDiskBySerial: unexpected error: %v", err)
	}
	if disk != "/dev/sdb" {
		t.Fatalf("FindDiskBySerial = %q, want /dev/sdb", disk)
	}
}

func TestFindDiskBySerialMatchesExactVPDPage83TextIdentifier(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "")
	vpdPage83 := []byte{
		0x00, 0x83, 0x00, 0x0f,
		0x02, 0x01, 0x00, 0x0b,
		'R', 'A', 'I', 'D', '-', 'D', 'I', 'S', 'K', '-', '1',
	}
	if err := os.WriteFile(sysfs+"/block/sdb/device/vpd_pg83", vpdPage83, 0o644); err != nil {
		t.Fatalf("write vpd_pg83: %v", err)
	}

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	disk, err := mgr.FindDiskBySerial(t.Context(), "RAID-DISK-1", 64)
	if err != nil {
		t.Fatalf("FindDiskBySerial: unexpected error: %v", err)
	}
	if disk != "/dev/sdb" {
		t.Fatalf("FindDiskBySerial = %q, want /dev/sdb", disk)
	}
}

func TestFindDiskBySerialDoesNotMatchSubstring(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "RAID-DISK-10")

	cmd := newMockCommander()
	cmd.setResult("lsblk --nodeps", []byte("/dev/sdb RAID-DISK-10\n"), nil)

	mgr := newManagerWithSysfs(cmd, sysfs)
	_, err := mgr.FindDiskBySerial(t.Context(), "RAID-DISK-1", 64)
	if err == nil {
		t.Fatal("expected substring-only serial lookup to fail")
	}
	if !strings.Contains(err.Error(), "no suitable disk found with serial") {
		t.Fatalf("error = %q, want no suitable disk detail", err)
	}
}

func TestFindDiskBySerialRejectsTooSmallDisk(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 10, "RAID-DISK-1")

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	_, err := mgr.FindDiskBySerial(t.Context(), "RAID-DISK-1", 64)
	if err == nil {
		t.Fatal("expected error for disk below minimum size")
	}
	if !strings.Contains(err.Error(), "below minimum 64 GB") {
		t.Fatalf("error = %q, want minimum size detail", err)
	}
}

func TestFindDiskBySerialRejectsDuplicateSerials(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsBlockDevice(t, sysfs, "sdb", "0\n", 96, "DUPLICATE")
	writeSysfsBlockDevice(t, sysfs, "sdc", "0\n", 96, "DUPLICATE")

	mgr := newManagerWithSysfs(newMockCommander(), sysfs)
	_, err := mgr.FindDiskBySerial(t.Context(), "DUPLICATE", 0)
	if err == nil {
		t.Fatal("expected error for duplicate disk serial")
	}
	if !strings.Contains(err.Error(), "multiple disks found") {
		t.Fatalf("error = %q, want duplicate detail", err)
	}
}

func TestCreateRAIDArray(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	err := mgr.CreateRAIDArray(context.Background(), "md0", 1, []string{"/dev/sda", "/dev/sdb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 1 || cmd.calls[0].name != "mdadm" {
		t.Fatalf("expected mdadm call, got %v", cmd.calls)
	}
}

func TestCreateRAIDArrayTooFewDevices(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	err := mgr.CreateRAIDArray(context.Background(), "md0", 1, []string{"/dev/sda"})
	if err == nil {
		t.Fatal("expected error for single device RAID")
	}
}

func TestCreateRAIDArrayError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)

	cmd.setResult("mdadm --create", nil, fmt.Errorf("mdadm: exit 1"))
	err := mgr.CreateRAIDArray(context.Background(), "md0", 1, []string{"/dev/sda", "/dev/sdb"})
	if err == nil {
		t.Fatal("expected error when mdadm fails")
	}
}

func TestDisableLVMNotFound(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)
	cmd.setResult("lvm vgchange", nil, fmt.Errorf("executable file not found in $PATH"))
	if err := mgr.DisableLVM(context.Background()); err != nil {
		t.Fatalf("DisableLVM should not fail when lvm binary is absent: %v", err)
	}
}

func TestDisableLVMError(t *testing.T) {
	cmd := newMockCommander()
	mgr := NewManager(cmd)
	cmd.setResult("lvm vgchange", nil, fmt.Errorf("lvm: device busy"))
	if err := mgr.DisableLVM(context.Background()); err == nil {
		t.Fatal("expected error when lvm vgchange fails")
	}
}

func TestMountDataNoRecovery(t *testing.T) {
	for fsType, want := range map[string]string{
		"ext4": "ro,noload", "ext3": "ro,noload", "xfs": "ro,norecovery", "btrfs": "ro,rescue=nologreplay", "ext2": "ro", "vfat": "ro",
	} {
		if got := mountData(fsType, noRecoveryData); got != want {
			t.Errorf("mountData(%s) = %q, want %q", fsType, got, want)
		}
	}
	if got := mountData("ext4", "ro"); got != "ro" {
		t.Errorf("mountData(ext4, ro) = %q, other callers must keep their data", got)
	}
}
