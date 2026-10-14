//go:build linux

package provision

import (
	"context"
	"errors"
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

// Provision with WIPE_ALL_DISKS checks the selected target disk first.
func TestWipeAllChecksTheSelectedTarget(t *testing.T) {
	o := guardedOrchestrator(t, wipeAllCfg("provision", new(true), nil))
	o.targetDisk = "/dev/sda"
	handled, err := o.wipeWithoutTargetSelection(context.Background())
	if !handled || !errors.Is(err, disk.ErrInUseCheck) {
		t.Fatalf("wipeWithoutTargetSelection() = %v, %v; want the target check error", handled, err)
	}
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
