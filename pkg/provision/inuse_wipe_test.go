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

// guardedCommander records commands like mockCommander but is not a
// disk.FakeDisks commander, so the in-use check of the disk manager runs.
// With the fixture sysfs of withFixtureDisks the check can not be made.
type guardedCommander struct{ mock *mockCommander }

func (g guardedCommander) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return g.mock.Run(ctx, name, args...)
}

func guardedOrchestrator(t *testing.T, cfg *config.MachineConfig) *Orchestrator {
	t.Helper()
	o := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(guardedCommander{newMockCommander()}))
	withFixtureDisks(t, o, "sda", "sdb")
	return o
}

// decommission and WIPE_ALL_DISKS log failed disk wipes and go on, but a
// failed in-use check stops the run.
func TestWipeAllFailsWhenTheInUseCheckFails(t *testing.T) {
	if err := guardedOrchestrator(t, &config.MachineConfig{Mode: "decommission"}).decommissionWipe(context.Background()); !errors.Is(err, disk.ErrInUseCheck) {
		t.Fatalf("decommissionWipe() = %v, want ErrInUseCheck", err)
	}
	o := guardedOrchestrator(t, wipeAllCfg("provision", new(true), nil))
	if err := o.wipeAllDisks(context.Background()); !errors.Is(err, disk.ErrInUseCheck) {
		t.Fatalf("wipeAllDisks() = %v, want ErrInUseCheck", err)
	}
}

// Provision with WIPE_ALL_DISKS checks the selected target disk first. In
// the fixture only sda is mounted, so the wipe of all disks alone would
// skip it and succeed: only the target check can fail the run.
func TestWipeAllChecksTheSelectedTarget(t *testing.T) {
	for _, tt := range []struct {
		target  string
		wantErr bool
	}{{"/dev/sda", true}, {"/dev/sdb", false}} {
		t.Run(tt.target, func(t *testing.T) {
			cmd := newMockCommander()
			o := NewOrchestrator(wipeAllCfg("provision", new(true), nil), &mockProvider{}, disk.NewManager(guardedCommander{cmd}))
			sys, proc := mountedDiskFixture(t)
			o.disk.SetSysfsRoot(sys)
			o.disk.SetProcRoot(proc)
			o.targetDisk = tt.target

			handled, err := o.wipeWithoutTargetSelection(context.Background())
			var inUse *disk.InUseError
			if !handled || errors.As(err, &inUse) != tt.wantErr || (!tt.wantErr && err != nil) {
				t.Fatalf("wipeWithoutTargetSelection() = %v, %v; want in-use error %v", handled, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !hasCall(cmd.calls, "wipefs", "-af /dev/sdb") {
				t.Fatalf("sdb not wiped: %v", cmd.calls)
			}
			for _, c := range cmd.calls {
				for _, a := range c.args {
					if a == "/dev/sda" {
						t.Fatalf("the mounted sda was touched: %s %v", c.name, c.args)
					}
				}
			}
		})
	}
}

// mountedDiskFixture returns a sysfs with the disks sda and sdb and a procfs
// in which sda is mounted at /target.
func mountedDiskFixture(t *testing.T) (sys, proc string) {
	t.Helper()
	root := t.TempDir()
	sys, proc = filepath.Join(root, "sys"), filepath.Join(root, "proc")
	for _, d := range []string{"devices/sda", "devices/sdb", "class/block", "dev/block", "block", "../proc/self"} {
		if err := os.MkdirAll(filepath.Join(sys, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sda", "sdb"} {
		for _, link := range []string{"class/block/" + name, "block/" + name} {
			if err := os.Symlink(filepath.Join(sys, "devices", name), filepath.Join(sys, link)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(filepath.Join(sys, "devices", "sda"), filepath.Join(sys, "dev", "block", "8:0")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), []byte("30 1 8:0 / /target rw - ext4 /dev/sda rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sys, proc
}

// The success status names the disks that the wipe of all disks skipped.
func TestDeprovisionSuccessNamesSkippedDisks(t *testing.T) {
	provider := &mockProvider{}
	o := newTestOrchestrator(t, &config.MachineConfig{Mode: "deprovision"}, provider)
	o.skippedDisks = []string{"/dev/sdc"}
	if err := o.reportDeprovisionSuccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := provider.statuses[0].message; got != "deprovisioning complete; not wiped, in use: /dev/sdc" {
		t.Fatalf("success message = %q", got)
	}
}
