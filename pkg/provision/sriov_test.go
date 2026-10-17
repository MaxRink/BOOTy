//go:build linux

package provision

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
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
	if !ok || r.Discover == nil || r.Config == nil || r.Runtime == nil || r.Journal == nil || r.Journal.Path != sriov.DefaultJournalPath {
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

// TestConfigureSRIOVVerifyReboot: stored SR-IOV firmware changes that the
// runtime settings depend on stop the task with the reboot-required status
// before any disk step, and without the error status.
func TestConfigureSRIOVVerifyReboot(t *testing.T) {
	stubSRIOVRunner(t, &fakeSRIOVRunner{res: &sriov.Result{Reboot: true, VerifyReboot: true,
		VerifyDetail: "adapters=0000:3b:00 settings=maxVFs"}})
	cfg := &config.MachineConfig{}
	cfg.Provision.SRIOV.Profiles = []sriov.Profile{{Name: "cx6", Match: sriov.Match{Driver: "mlx5_core"}}}
	cfg.Provision.Inventory.Enabled = true
	cfg.Transport.RebootURL = "http://caprf.example/status/reboot-required"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	counter := &inventoryCounter{mockProvider: provider}
	o.provider = counter
	err := o.configureSRIOV(context.Background())
	var rerr *RebootRequiredError
	if !errors.As(err, &rerr) || rerr.Message() != "reboot-required reason=sriov-firmware adapters=0000:3b:00 settings=maxVFs" {
		t.Fatalf("err = %v", err)
	}
	if len(provider.statuses) != 1 || provider.statuses[0].status != config.StatusRebootRequired ||
		provider.statuses[0].message != rerr.Message() {
		t.Fatalf("statuses %+v", provider.statuses)
	}
	if counter.reports != 0 {
		t.Fatal("no inventory before the verify reboot")
	}

	// executeStep passes it on without the error status.
	provider.statuses = nil
	cp := &Checkpoint{}
	step := Step{"configure-sriov", func(context.Context) error { return rerr }}
	if err := o.executeStep(context.Background(), step, cp); !errors.As(err, &rerr) || len(provider.statuses) != 0 ||
		cp.FailureCount != 0 || cp.IsCompleted("configure-sriov") {
		t.Fatalf("executeStep %v statuses %+v checkpoint %+v", err, provider.statuses, cp)
	}
	if (&RebootRequiredError{Reason: "x"}).Message() != "reboot-required reason=x" {
		t.Fatal("message without detail")
	}
}

// TestReportRebootRequiredWaitsFor2xx: CAPRF arms the boot media again
// before it answers 2xx, so BOOTy retries the status until it gets one,
// within a bounded time, and then reboots anyway with an error log.
func TestReportRebootRequiredWaitsFor2xx(t *testing.T) {
	origWait, origRetry := rebootRequiredWait, rebootRequiredRetry
	rebootRequiredWait, rebootRequiredRetry = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { rebootRequiredWait, rebootRequiredRetry = origWait, origRetry })

	failing := errors.New("CAPRF answered 503")
	provider := &mockProvider{reportStatusErrs: []error{failing, failing, nil}}
	o := newTestOrchestrator(t, &config.MachineConfig{}, provider)
	var logs bytes.Buffer
	o.log = slog.New(slog.NewTextHandler(&logs, nil))
	o.reportRebootRequired(context.Background(), "reboot-required reason=sriov-firmware")
	if len(provider.statuses) != 3 || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("attempts %d, logs:\n%s", len(provider.statuses), logs.String())
	}

	// No 2xx at all: bounded by the wait, then an error log.
	provider = &mockProvider{reportStatusErr: failing}
	o = newTestOrchestrator(t, &config.MachineConfig{}, provider)
	logs.Reset()
	o.log = slog.New(slog.NewTextHandler(&logs, nil))
	start := time.Now()
	o.reportRebootRequired(context.Background(), "reboot-required reason=sriov-firmware")
	if elapsed := time.Since(start); elapsed > 2*time.Second || len(provider.statuses) < 2 {
		t.Fatalf("elapsed %v attempts %d", elapsed, len(provider.statuses))
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "rebooting anyway") {
		t.Fatalf("logs:\n%s", logs.String())
	}

	// A canceled context ends the wait at once.
	provider = &mockProvider{reportStatusErr: failing}
	o = newTestOrchestrator(t, &config.MachineConfig{}, provider)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.reportRebootRequired(ctx, "reboot-required reason=sriov-firmware")
	if len(provider.statuses) != 1 {
		t.Fatalf("attempts after cancel %d", len(provider.statuses))
	}
}

// TestConfigureSRIOVVerifyRebootNeedsCAPRF: without REBOOT_URL CAPRF cannot
// keep the boot media for a verify reboot, so the run fails with a coded
// error instead of rebooting into the old OS. SRIOV_VERIFY_REBOOT=off goes
// on to the handoff without the runtime check.
func TestConfigureSRIOVVerifyRebootNeedsCAPRF(t *testing.T) {
	res := &sriov.Result{Reboot: true, VerifyReboot: true, VerifyDetail: "adapters=0000:3b:00 settings=maxVFs"}
	stubSRIOVRunner(t, &fakeSRIOVRunner{res: res})
	cfg := &config.MachineConfig{}
	cfg.Provision.SRIOV.Profiles = []sriov.Profile{{Name: "cx6", Match: sriov.Match{Driver: "mlx5_core"}}}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	err := o.configureSRIOV(context.Background())
	var rerr *RebootRequiredError
	if !errors.Is(err, ErrVerifyRebootUnsupported) || errors.As(err, &rerr) || len(provider.statuses) != 0 ||
		!strings.Contains(err.Error(), "a verify reboot needs REBOOT_URL support in CAPRF") || !strings.Contains(err.Error(), "settings=maxVFs") {
		t.Fatalf("err %v statuses %+v", err, provider.statuses)
	}
	if !o.FirmwareChanged() {
		t.Fatal("the stored firmware change still needs the reboot at the end")
	}
	if code := CodeOf(WithCode("configure-sriov", err)); code != "BOOTY-HW-006" {
		t.Fatalf("failure code %q", code)
	}

	cfg.Provision.SRIOV.VerifyReboot = sriov.VerifyRebootOff
	o = newTestOrchestrator(t, cfg, provider)
	if err := o.configureSRIOV(context.Background()); err != nil || len(provider.statuses) != 0 || !o.FirmwareChanged() {
		t.Fatalf("off: %v statuses %+v", err, provider.statuses)
	}
}

func TestKernelRuntimeUsesTheNetworkConfig(t *testing.T) {
	network.RecordUsedInterfaces(&network.Config{StaticIface: "eth0"})
	names, all, known := kernelRuntime().Uplinks()
	if !known || all || len(names) != 1 || names[0] != "eth0" {
		t.Fatalf("uplinks %v all=%v known=%v", names, all, known)
	}
}
