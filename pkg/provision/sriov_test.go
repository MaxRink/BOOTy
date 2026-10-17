//go:build linux

package provision

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/sriov"
)

type fakeSRIOVRunner struct {
	res *sriov.Result
	err error
	cfg *sriov.Config
}

func (f *fakeSRIOVRunner) Run(context.Context) (*sriov.Result, error) { return f.res, f.err }

func stubSRIOVRunner(t *testing.T, f *fakeSRIOVRunner) {
	t.Helper()
	orig := newSRIOVRunner
	newSRIOVRunner = func(cfg *sriov.Config, _ *slog.Logger) sriovRunner {
		f.cfg = cfg
		return f
	}
	t.Cleanup(func() { newSRIOVRunner = orig })
}

func TestConfigureSRIOV(t *testing.T) {
	profiles := []sriov.Profile{{Name: "cx6", Match: sriov.Match{Driver: "mlx5_core"}}}
	cases := []struct {
		name       string
		profiles   []sriov.Profile
		res        *sriov.Result
		err        error
		wantRun    bool
		wantReboot bool
		wantErr    bool
	}{
		{"no profiles", nil, nil, nil, false, false, false},
		{"compliant", profiles, &sriov.Result{}, nil, true, false, false},
		{"applied", profiles, &sriov.Result{Reboot: true}, nil, true, true, false},
		// A failure after a stored change still reboots.
		{"failed after apply", profiles, &sriov.Result{Reboot: true}, errors.New("not compliant"), true, true, true},
		{"failed", profiles, nil, errors.New("ambiguous"), true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSRIOVRunner{res: tc.res, err: tc.err}
			stubSRIOVRunner(t, f)
			cfg := &config.MachineConfig{}
			cfg.Provision.SRIOV.Profiles = tc.profiles
			o := newTestOrchestrator(t, cfg, &mockProvider{})
			err := o.configureSRIOV(context.Background())
			if (err != nil) != tc.wantErr || o.FirmwareChanged() != tc.wantReboot || (f.cfg != nil) != tc.wantRun {
				t.Fatalf("err %v reboot %v ran %v", err, o.FirmwareChanged(), f.cfg != nil)
			}
			if tc.wantRun && f.cfg != &cfg.Provision.SRIOV {
				t.Fatal("runner did not get the machine config")
			}
		})
	}
}

func TestProvisionStepsConfigureSRIOVAfterMellanox(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	steps := o.provisionSteps()
	if requireStepIndex(t, steps, "configure-sriov") != requireStepIndex(t, steps, "setup-mellanox")+1 {
		t.Fatal("configure-sriov must run right after setup-mellanox")
	}
	if _, ok := resumeStateSteps()["configure-sriov"]; !ok {
		t.Fatal("configure-sriov sets the in-memory reboot flag and must re-run on resume")
	}
}

func TestNewSRIOVRunnerIsReconciler(t *testing.T) {
	r, ok := newSRIOVRunner(&sriov.Config{}, slog.Default()).(*sriov.Reconciler)
	if !ok || r.Discover == nil || r.Config == nil || r.Journal == nil || r.Journal.Path != sriov.DefaultJournalPath {
		t.Fatalf("runner %+v", r)
	}
}

type inventoryCounter struct {
	*mockProvider
	reports int
}

func (p *inventoryCounter) ReportInventory(context.Context, []byte) error {
	p.reports++
	return nil
}

func TestConfigureSRIOVResendsInventory(t *testing.T) {
	stubSRIOVRunner(t, &fakeSRIOVRunner{res: &sriov.Result{}})
	cfg := &config.MachineConfig{}
	cfg.Provision.SRIOV.Profiles = []sriov.Profile{{Name: "cx6", Match: sriov.Match{Driver: "mlx5_core"}}}
	cfg.Provision.Inventory.Enabled = true
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	counter := &inventoryCounter{mockProvider: &mockProvider{}}
	o.provider = counter
	if err := o.configureSRIOV(context.Background()); err != nil {
		t.Fatal(err)
	}
	if counter.reports != 1 {
		t.Fatalf("inventory reports after configure-sriov = %d, want 1", counter.reports)
	}
}
