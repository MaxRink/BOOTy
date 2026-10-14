//go:build linux

package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	exec "github.com/telekom/BOOTy/pkg/executil"

	"github.com/telekom/BOOTy/pkg/config"
)

// withContainerMarkers sets the container marker files for the test, so the
// result does not depend on the machine that runs it (nil: no container).
func withContainerMarkers(t *testing.T, markers []string) {
	t.Helper()
	old := containerMarkers
	containerMarkers = markers
	t.Cleanup(func() { containerMarkers = old })
}

// withEFIFirmware points /sys/firmware/efi at a temporary path that exists
// (UEFI boot) or not (BIOS boot). efivarfs is never mounted there.
func withEFIFirmware(t *testing.T, uefi bool) {
	t.Helper()
	withContainerMarkers(t, nil)
	oldFirmwarePath, oldVarsPath := efiFirmwarePath, efiVarsPath
	efiFirmwarePath = filepath.Join(t.TempDir(), "efi")
	efiVarsPath = filepath.Join(efiFirmwarePath, "efivars")
	if uefi {
		if err := os.MkdirAll(efiFirmwarePath, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { efiFirmwarePath, efiVarsPath = oldFirmwarePath, oldVarsPath })
}

func withHostCommands(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	old := hostCommandCombinedOutput
	hostCommandCombinedOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		return fn(name, args...)
	}
	t.Cleanup(func() { hostCommandCombinedOutput = old })
}

// The legacy scripts ran mount -t efivarfs and efibootmgr with set -e on
// UEFI, so a failure stopped the run. On BIOS they skipped the EFI steps.
func TestEFIBootEntryStepsFailOnUEFI(t *testing.T) {
	efibootmgr := func(out string, listErr, removeErr error) func(string, ...string) ([]byte, error) {
		return func(name string, args ...string) ([]byte, error) {
			switch {
			case name == "efibootmgr" && len(args) == 0:
				return []byte(out), listErr
			case name == "efibootmgr":
				return []byte("Could not delete variable"), removeErr
			}
			return []byte("modprobe: not found"), os.ErrNotExist
		}
	}
	listing := "BootOrder: 0001,0002\nBoot0001* ubuntu\tHD(1,GPT,x)/File(\\EFI\\ubuntu\\shimx64.efi)\nBoot0002* PXE\n"
	tests := []struct {
		name     string
		uefi     bool
		required *bool
		step     func(*Orchestrator) error
		commands func(string, ...string) ([]byte, error)
		efiReady func() (bool, string)
		wantErr  string
	}{
		{name: "mount fails on UEFI", uefi: true, step: mountStep, commands: efibootmgr("", nil, nil), wantErr: "mount efivarfs"},
		{name: "mount fails on UEFI, not required", uefi: true, required: new(false), step: mountStep, commands: efibootmgr("", nil, nil)},
		{name: "mount skipped on BIOS", step: mountStep, commands: efibootmgr("", nil, nil)},
		{name: "list fails on UEFI", uefi: true, step: removeStep, commands: efibootmgr("", errors.New("EFI variables are not supported"), nil), wantErr: "efibootmgr list"},
		{name: "remove fails on UEFI", uefi: true, step: removeStep, commands: efibootmgr(listing, nil, errors.New("exit 5")), wantErr: "efibootmgr remove Boot0001"},
		{name: "remove fails on UEFI, not required", uefi: true, required: new(false), step: removeStep, commands: efibootmgr(listing, nil, errors.New("exit 5"))},
		{name: "remove works on UEFI", uefi: true, step: removeStep, commands: efibootmgr(listing, nil, nil)},
		{name: "remove skipped on BIOS", step: removeStep, commands: efibootmgr("", errors.New("EFI variables are not supported"), nil)},
		{name: "create without efivarfs on UEFI", uefi: true, step: createStep, commands: efibootmgr("", nil, nil),
			efiReady: func() (bool, string) { return false, "efivarfs not mounted" }, wantErr: "efivarfs not mounted"},
		{name: "create without efivarfs on UEFI, not required", uefi: true, required: new(false), step: createStep, commands: efibootmgr("", nil, nil),
			efiReady: func() (bool, string) { return false, "efivarfs not mounted" }},
		{name: "create skipped on BIOS", step: createStep, commands: efibootmgr("", nil, nil),
			efiReady: func() (bool, string) { return false, efiNotBootedReason }},
		// The opt-out covers only unavailable EFI variables.
		{name: "create without EFI loader, not required", uefi: true, required: new(false), step: createStep, commands: efibootmgr("", nil, nil),
			efiReady: func() (bool, string) { return true, "" }, wantErr: "no EFI loader found"},
		{name: "efibootmgr missing on UEFI", uefi: true, step: removeStep,
			commands: func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("run: %w", exec.ErrNotFound) },
			wantErr:  "efibootmgr is not in the ramdisk"},
		{name: "efibootmgr missing on UEFI, not required", uefi: true, required: new(false), step: removeStep,
			commands: func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("run: %w", exec.ErrNotFound) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEFIFirmware(t, tt.uefi)
			withHostCommands(t, tt.commands)
			oldMount := mountEFIVarFS
			mountEFIVarFS = func(string, string, string, uintptr, string) error { return syscall.ENODEV }
			t.Cleanup(func() { mountEFIVarFS = oldMount })
			if tt.efiReady != nil {
				old := efiRuntimeReady
				efiRuntimeReady = tt.efiReady
				t.Cleanup(func() { efiRuntimeReady = old })
			}
			cfg := &config.MachineConfig{}
			cfg.Provision.EFIBootEntryRequired = tt.required
			o := newTestOrchestrator(t, cfg, &mockProvider{})
			o.targetDisk, o.bootPartition = "/dev/sda", "/dev/sda1"
			err := tt.step(o)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("step error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("step error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func mountStep(o *Orchestrator) error  { return o.mountEFIVars(context.Background()) }
func removeStep(o *Orchestrator) error { return o.removeEFIBootEntries(context.Background()) }
func createStep(o *Orchestrator) error { return o.createEFIBootEntry(context.Background()) }

// MODE=decommission keeps going without EFI variables, like decommission_wipe.
func TestDecommissionMountsEFIVarsBestEffort(t *testing.T) {
	withEFIFirmware(t, true)
	withHostCommands(t, func(string, ...string) ([]byte, error) { return nil, os.ErrNotExist })
	oldMount := mountEFIVarFS
	mountEFIVarFS = func(string, string, string, uintptr, string) error { return syscall.ENODEV }
	t.Cleanup(func() { mountEFIVarFS = oldMount })
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	for _, step := range o.deprovisionSteps("decommission") {
		if step.Name != "mount-efivarfs" {
			continue
		}
		if err := step.Fn(context.Background()); err != nil {
			t.Fatalf("decommission mount-efivarfs = %v, want nil", err)
		}
		return
	}
	t.Fatal("decommission has no mount-efivarfs step")
}

// Only a missing /sys/firmware/efi means BIOS; any other stat error fails.
func TestBootedViaUEFIStatError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetEFIFirmwarePathForTest(filepath.Join(file, "efi"))) // ENOTDIR
	withContainerMarkers(t, nil)
	if _, err := bootedViaUEFI(); err == nil {
		t.Fatal("bootedViaUEFI() = nil error for ENOTDIR, want an error")
	}
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	if err := o.removeEFIBootEntries(context.Background()); err == nil {
		t.Fatal("removeEFIBootEntries() = nil, want the stat error")
	}
}

// Deprovision touches the EFI variables before it wipes a disk.
func TestDeprovisionRemovesEFIEntriesBeforeWipe(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	idx := map[string]int{}
	for i, step := range o.deprovisionSteps("hard") {
		idx[step.Name] = i
	}
	if idx["mount-efivarfs"] >= idx["remove-efi-entries"] || idx["remove-efi-entries"] >= idx["wipe-disks"] {
		t.Fatalf("deprovision step order = %v, want mount-efivarfs, remove-efi-entries before wipe-disks", idx)
	}
}

// The create path: a missing efibootmgr is "EFI variables unavailable"
// (opt-out possible), a failed efibootmgr -c stays fatal.
func TestCreateEFIBootEntryEFIBootMgrFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		runErr   error
		required *bool
		wantErr  string
	}{
		{name: "missing, required", runErr: fmt.Errorf("run: %w", exec.ErrNotFound), wantErr: "efibootmgr is not in the ramdisk"},
		{name: "missing, not required", runErr: fmt.Errorf("run: %w", exec.ErrNotFound), required: new(false)},
		{name: "-c fails, required", runErr: errors.New("exit status 5"), wantErr: "efibootmgr create"},
		{name: "-c fails, not required", runErr: errors.New("exit status 5"), required: new(false), wantErr: "efibootmgr create"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withEFIFirmware(t, true)
			old := efiRuntimeReady
			efiRuntimeReady = func() (bool, string) { return true, "" }
			t.Cleanup(func() { efiRuntimeReady = old })
			cfg := &config.MachineConfig{}
			cfg.Provision.EFIBootEntryRequired = tt.required
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
			cmd.setResult("efibootmgr -c", []byte("Could not prepare Boot variable"), tt.runErr)
			loaderDir := filepath.Join(o.config.rootDir, "boot", "efi", "EFI", "ubuntu")
			if err := os.MkdirAll(loaderDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"shimx64.efi", "shimaa64.efi"} {
				if err := os.WriteFile(filepath.Join(loaderDir, f), []byte("shim"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			o.targetDisk, o.bootPartition = "/dev/sda", "/dev/sda1"
			err := o.createEFIBootEntry(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("createEFIBootEntry() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("createEFIBootEntry() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// In a container the host /sys/firmware/efi is visible; BOOTy must not
// touch the host EFI variables in any EFI step (real efiRuntimeReady).
func TestEFIStepsSkippedInContainer(t *testing.T) {
	withEFIFirmware(t, true)
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withContainerMarkers(t, []string{marker})
	called := false
	withHostCommands(t, func(string, ...string) ([]byte, error) { called = true; return nil, errors.New("must not run") })
	oldMount := mountEFIVarFS
	mountEFIVarFS = func(string, string, string, uintptr, string) error { called = true; return syscall.ENODEV }
	t.Cleanup(func() { mountEFIVarFS = oldMount })
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, &mockProvider{})
	o.targetDisk, o.bootPartition = "/dev/sda", "/dev/sda1"
	for name, step := range map[string]func(*Orchestrator) error{"mount": mountStep, "remove": removeStep, "create": createStep} {
		if err := step(o); err != nil {
			t.Errorf("%s in a container = %v, want a skip", name, err)
		}
	}
	o.config.RemoveAllEFIBootEntries(context.Background())
	if called {
		t.Fatal("an EFI step touched the host EFI variables from a container")
	}
	for _, c := range cmd.calls {
		if c.name == "efibootmgr" {
			t.Fatalf("efibootmgr ran in a container: %v", c.args)
		}
	}
}

// MODE=decommission skips the removal of every entry on BIOS, like
// remove_all_efi_boot_entries.
func TestRemoveAllEFIBootEntriesSkipsOnBIOS(t *testing.T) {
	withEFIFirmware(t, false)
	called := false
	withHostCommands(t, func(string, ...string) ([]byte, error) { called = true; return nil, nil })
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	o.config.RemoveAllEFIBootEntries(context.Background())
	if called {
		t.Fatal("RemoveAllEFIBootEntries ran efibootmgr on BIOS")
	}
}

// A failed efivars mountpoint creation is "EFI variables unavailable".
func TestMountEFIVarsMkdirFailure(t *testing.T) {
	withEFIFirmware(t, true)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := efiVarsPath
	efiVarsPath = filepath.Join(file, "efivars") // ENOTDIR
	t.Cleanup(func() { efiVarsPath = old })
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	err := o.config.MountEFIVars(context.Background())
	if !errors.Is(err, ErrEFIVarsUnavailable) {
		t.Fatalf("MountEFIVars() = %v, want ErrEFIVarsUnavailable", err)
	}
}
