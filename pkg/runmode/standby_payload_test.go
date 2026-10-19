//go:build linux

package runmode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/payload"
)

const payloadTestBaseVars = `MODE=standby
HOSTNAME=t1
TOKEN=base-token
HEARTBEAT_URL=https://caprf.example.com/agent/heartbeat
COMMANDS_URL=https://caprf.example.com/agent/commands
EXTRA_KERNEL_PARAMS=fromvars=t1
IMAGE=https://images.example.com/a.img
`

// payloadProvider merges payloads like caprf.Client.CommandConfig.
type payloadProvider struct {
	*standbyProvider
	// ramdiskFiles sets PROVISIONER_FILES_TARGET=ramdisk in the command config.
	ramdiskFiles bool
}

func (p *payloadProvider) CommandConfig(payload []byte) (*config.MachineConfig, []string, error) {
	cfg, keys, err := caprf.MergeCommandVars([]byte(payloadTestBaseVars), payload)
	if err == nil && p.ramdiskFiles {
		cfg.ProvisionerFilesTarget = config.ProvisionerFilesRamdisk
	}
	return cfg, keys, err
}

func payloadStandby(t *testing.T, cmds []config.Command, deprovision func(*config.MachineConfig) error) (*StandbyMode, *payloadProvider) {
	t.Helper()
	cfg, err := caprf.ParseVars(strings.NewReader(payloadTestBaseVars))
	if err != nil {
		t.Fatalf("parse base vars: %v", err)
	}
	cfg.Agent.DeprovisionSuccessAction = config.AgentDeprovisionStandby
	provider := &payloadProvider{standbyProvider: &standbyProvider{commands: cmds}}
	mode := &StandbyMode{deps: Deps{Cfg: cfg, Client: provider}}
	mode.newDeprovisioner = func(cfg *config.MachineConfig) standbyDeprovisioner {
		return standbyDeprovisionerFunc(func(context.Context) error { return deprovision(cfg) })
	}
	return mode, provider
}

// A deprovision command payload wins over the base vars for that command,
// the ACK names the payload keys, and with STANDBY_DEPROVISION_SUCCESS_ACTION
// =standby BOOTy returns to the standby loop with the base config: the next
// command (reboot) runs in the same boot.
func TestStandbyDeprovisionPayloadWinsThenReturnsToStandby(t *testing.T) {
	var gotHostname, gotParams, gotMode string
	mode, provider := payloadStandby(t, []config.Command{
		{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=t1-payload\nEXTRA_KERNEL_PARAMS=frompayload=t1\n")},
		{ID: "r1", Type: "reboot"},
	}, func(cfg *config.MachineConfig) error {
		gotHostname, gotParams, gotMode = cfg.Hostname, cfg.Provision.ExtraKernelParams, cfg.Mode
		return nil
	})

	err := mode.Run(context.Background())
	var rebootErr *RebootRequestedError
	if !errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %T %[1]v, want the reboot command to run after the deprovision", err)
	}
	if gotHostname != "t1-payload" || gotParams != "frompayload=t1" || gotMode != "deprovision" {
		t.Fatalf("deprovision ran with hostname %q, params %q, mode %q; want the payload values", gotHostname, gotParams, gotMode)
	}
	if !slices.Equal(provider.acks, []string{"d1:completed", "r1:completed"}) {
		t.Fatalf("acks = %v", provider.acks)
	}
	if want := "deprovisioning complete with payload keys EXTRA_KERNEL_PARAMS,HOSTNAME"; provider.ackMessages[0] != want {
		t.Fatalf("ack message = %q, want %q", provider.ackMessages[0], want)
	}
	// Back in standby: a second "standby" init status and the base config.
	if !slices.Equal(provider.statuses, []config.Status{config.StatusInit, config.StatusInit}) {
		t.Fatalf("statuses = %v, want standby init twice", provider.statuses)
	}
	cfg := mode.deps.Cfg
	if cfg.Hostname != "t1" || cfg.Provision.ExtraKernelParams != "fromvars=t1" || cfg.Mode != "standby" {
		t.Fatalf("config after the command = %q %q %q, want the base config back", cfg.Hostname, cfg.Provision.ExtraKernelParams, cfg.Mode)
	}
}

// Without STANDBY_DEPROVISION_SUCCESS_ACTION a successful deprovision ends
// the run with a reboot, like before.
func TestStandbyDeprovisionDefaultActionReboots(t *testing.T) {
	mode, provider := payloadStandby(t, []config.Command{{ID: "d1", Type: "deprovision"}},
		func(*config.MachineConfig) error { return nil })
	mode.deps.Cfg.Agent.DeprovisionSuccessAction = ""

	err := mode.Run(context.Background())
	var rebootErr *RebootRequestedError
	if !errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %T %[1]v, want RebootRequestedError", err)
	}
	if provider.ackMessages[0] != "deprovisioning complete" {
		t.Fatalf("ack message = %q", provider.ackMessages[0])
	}
}

// A payload that changes settings of the running ramdisk is rejected with a
// failed ACK, the command does not run and standby goes on.
func TestStandbyRejectsPayloadThatChangesRuntimeSettings(t *testing.T) {
	for _, payload := range []string{
		"TOKEN=other",
		"COMMANDS_URL=https://evil.example.com/agent/commands",
		"MODE=provision",
		"STATIC_IP=10.0.0.5/24",
		"CAPRF_LEGACY_COMPAT=true",
		"STANDBY_DEPROVISION_SUCCESS_ACTION=standby",
		"not a vars line",
		"SUCCESS_ACTION=explode",
	} {
		t.Run(payload, func(t *testing.T) {
			ran := false
			mode, provider := payloadStandby(t, []config.Command{
				{ID: "d1", Type: "deprovision", Payload: []byte(payload)},
				{ID: "r1", Type: "reboot"},
			}, func(*config.MachineConfig) error { ran = true; return nil })

			err := mode.Run(context.Background())
			var rebootErr *RebootRequestedError
			if !errors.As(err, &rebootErr) {
				t.Fatalf("Run error = %T %[1]v, want standby to go on to the reboot command", err)
			}
			if ran {
				t.Fatal("the deprovision ran with a rejected payload")
			}
			if !slices.Equal(provider.acks, []string{"d1:failed", "r1:completed"}) {
				t.Fatalf("acks = %v", provider.acks)
			}
			if !strings.HasPrefix(provider.ackMessages[0], "invalid deprovision payload: ") {
				t.Fatalf("ack message = %q", provider.ackMessages[0])
			}
			if mode.deps.Cfg.Transport.Token != "base-token" || mode.deps.Cfg.Mode != "standby" {
				t.Fatalf("config changed by a rejected payload: %+v", mode.deps.Cfg.Transport)
			}
		})
	}
}

// A provider without payload support rejects a command with a payload.
func TestStandbyPayloadNeedsProviderSupport(t *testing.T) {
	provider := &standbyProvider{commands: []config.Command{
		{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=x")},
		{ID: "r1", Type: "reboot"},
	}}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}
	_ = mode.Run(context.Background())
	if !slices.Equal(provider.acks, []string{"d1:failed", "r1:completed"}) {
		t.Fatalf("acks = %v", provider.acks)
	}
}

// The command config is a deep copy: a run that changes its slices does not
// change the base config.
func TestStandbyCommandConfigIsADeepCopy(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("HOSTNAME=x")} {
		mode, _ := payloadStandby(t, []config.Command{
			{ID: "d1", Type: "deprovision", Payload: payload},
			{ID: "r1", Type: "reboot"},
		}, func(c *config.MachineConfig) error {
			c.Provision.Image.URLs[0] = "changed"
			return nil
		})
		_ = mode.Run(context.Background())
		if got := mode.deps.Cfg.Provision.Image.URLs[0]; got != "https://images.example.com/a.img" {
			t.Fatalf("payload %q: base image URL = %q after the run", payload, got)
		}
	}
}

// skippingDeprovisioner reports disks in use that its wipe skipped and
// disks that got the wipe instead of the secure erase.
type skippingDeprovisioner struct{ skipped, fallbacks []string }

func (skippingDeprovisioner) Deprovision(context.Context) error { return nil }
func (s skippingDeprovisioner) SkippedDisks() []string          { return s.skipped }
func (s skippingDeprovisioner) SecureEraseFallbacks() []string  { return s.fallbacks }

// The completed ACK names the disks in use that the wipe of all disks
// skipped, like the success status.
func TestStandbyDeprovisionAckNamesSkippedDisks(t *testing.T) {
	mode, provider := payloadStandby(t, []config.Command{{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=x")}},
		func(*config.MachineConfig) error { return nil })
	mode.deps.Cfg.Agent.DeprovisionSuccessAction = ""
	mode.newDeprovisioner = func(*config.MachineConfig) standbyDeprovisioner {
		return skippingDeprovisioner{skipped: []string{"/dev/sdc", "/dev/sdd"}, fallbacks: []string{"/dev/sda"}}
	}

	_ = mode.Run(context.Background())
	want := "deprovisioning complete with payload keys HOSTNAME; not wiped, in use: /dev/sdc, /dev/sdd" +
		"; secure erase not done, wiped instead (SECURE_ERASE_FALLBACK=wipe): /dev/sda"
	if provider.ackMessages[0] != want {
		t.Fatalf("ack message = %q, want %q", provider.ackMessages[0], want)
	}
}

// ctxAckProvider refuses an ACK with a canceled context, like an HTTP client.
type ctxAckProvider struct {
	*standbyProvider
}

func (p *ctxAckProvider) AcknowledgeCommand(ctx context.Context, cmdID, status, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.standbyProvider.AcknowledgeCommand(ctx, cmdID, status, message)
}

// The failure ACK leaves after the token stop canceled the run context.
func TestStandbyFailureAckSurvivesACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := &ctxAckProvider{standbyProvider: &standbyProvider{}}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}
	cfg := standbyConfig()
	cfg.Mode = "provision"
	_ = mode.handleProvision(ctx, config.Command{ID: "p1", Type: "provision"}, cfg, nil)

	mode.newDeprovisioner = func(*config.MachineConfig) standbyDeprovisioner {
		return standbyDeprovisionerFunc(func(context.Context) error { return errors.New("wipe failed") })
	}
	cfg = standbyConfig()
	cfg.Mode = "deprovision"
	_ = mode.handleDeprovision(ctx, config.Command{ID: "d1", Type: "deprovision"}, cfg, nil)

	if !slices.Equal(provider.acks, []string{"p1:failed", "d1:failed"}) {
		t.Fatalf("acks = %v, want both failure ACKs despite the canceled context", provider.acks)
	}
}

// stagingProvider stages files on a payload like caprf.Client: the first
// command's payload stages and fails, the apply reports what is staged.
type stagingProvider struct {
	*payloadProvider
	failFirst bool
	applyErr  error
	staged    bool
	applied   []bool
	discards  int
	calls     int
}

func (p *stagingProvider) CommandConfig(payload []byte) (*config.MachineConfig, []string, error) {
	p.calls++
	p.staged = len(payload) > 0
	if p.failFirst && p.calls == 1 {
		return nil, nil, errors.New("rejected after the staging")
	}
	return p.payloadProvider.CommandConfig(payload)
}

func (p *stagingProvider) ApplyCommandFiles() (bool, error) {
	applied := p.staged
	p.staged = false
	p.applied = append(p.applied, applied)
	return applied, p.applyErr
}

func (p *stagingProvider) DiscardCommandFiles() {
	p.discards++
	p.staged = false
}

// The files that a command staged never reach a later command: the
// command drops them on every path, also when it is rejected.
func TestStandbyRejectedCommandFilesDoNotReachTheNext(t *testing.T) {
	mode, provider := payloadStandby(t, []config.Command{
		{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=job-a\n")},
		{ID: "d2", Type: "deprovision"},
		{ID: "r1", Type: "reboot"},
	}, func(*config.MachineConfig) error { return nil })
	stager := &stagingProvider{payloadProvider: provider, failFirst: true}
	mode.deps.Client = stager

	var rebootErr *RebootRequestedError
	if err := mode.Run(context.Background()); !errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %v", err)
	}
	if !slices.Equal(stager.applied, []bool{false}) || stager.discards != 2 {
		t.Fatalf("applied %v, discards %d; want d2 with nothing staged and a discard per command", stager.applied, stager.discards)
	}
	if !slices.Equal(provider.acks[:2], []string{"d1:failed", "d2:completed"}) {
		t.Fatalf("acks = %v", provider.acks)
	}
}

// The staged files are applied after the checks and, with the ramdisk as
// the provisioner files target, copied into the ramdisk before the run; a
// failed apply ACKs the command as failed and standby goes on.
func TestStandbyAppliesCommandFilesBeforeTheRun(t *testing.T) {
	prev := applyProvisionerFiles
	t.Cleanup(func() { applyProvisionerFiles = prev })
	for _, tc := range []struct {
		name     string
		ramdisk  bool
		applyErr error
		wantRun  bool
		wantCopy int
	}{
		{"target files", false, nil, true, 0},
		{"ramdisk files", true, nil, true, 1},
		{"failed apply", false, errors.New("rename failed"), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			applyProvisionerFiles = func(context.Context) error { order = append(order, "copy"); return nil }
			mode, provider := payloadStandby(t, []config.Command{
				{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=t1\n")},
				{ID: "r1", Type: "reboot"},
			}, func(*config.MachineConfig) error { order = append(order, "run"); return nil })
			if tc.ramdisk {
				provider.ramdiskFiles = true
			}
			mode.deps.Client = &stagingProvider{payloadProvider: provider, applyErr: tc.applyErr}

			var rebootErr *RebootRequestedError
			if err := mode.Run(context.Background()); !errors.As(err, &rebootErr) {
				t.Fatalf("Run error = %v", err)
			}
			want := []string{}
			if tc.wantCopy > 0 {
				want = append(want, "copy")
			}
			if tc.wantRun {
				want = append(want, "run")
			}
			if !slices.Equal(order, want) {
				t.Fatalf("order = %v, want %v", order, want)
			}
			wantAck := "d1:completed"
			if !tc.wantRun {
				wantAck = "d1:failed"
			}
			if provider.acks[0] != wantAck {
				t.Fatalf("acks = %v, want %s first", provider.acks, wantAck)
			}
		})
	}
}

// The ramdisk copy runs only when the command swapped archive files in: a
// command without an archive leaves the ramdisk files of the boot.
func TestStandbyCopiesRamdiskFilesOnlyAfterAnArchive(t *testing.T) {
	prev := applyProvisionerFiles
	t.Cleanup(func() { applyProvisionerFiles = prev })
	copies := 0
	applyProvisionerFiles = func(context.Context) error { copies++; return nil }
	mode, provider := payloadStandby(t, []config.Command{
		{ID: "d1", Type: "deprovision"},
		{ID: "r1", Type: "reboot"},
	}, func(*config.MachineConfig) error { return nil })
	provider.ramdiskFiles = true
	mode.deps.Cfg.ProvisionerFilesTarget = config.ProvisionerFilesRamdisk
	mode.deps.Client = &stagingProvider{payloadProvider: provider}

	var rebootErr *RebootRequestedError
	if err := mode.Run(context.Background()); !errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %v", err)
	}
	if copies != 0 {
		t.Fatalf("ramdisk copies = %d without an archive, want 0", copies)
	}
}

// A command that needs a payload the split ramdisk lacks is rejected up
// front with BOOTY-PAY-001, and standby goes on.
func TestStandbyRejectsCommandThatNeedsAMissingPayload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "payloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "split"), []byte("frr ipmitool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := payload.Dir
	t.Cleanup(func() { payload.Dir = orig })
	payload.Dir = dir

	ran := false
	mode, provider := payloadStandby(t, []config.Command{
		{ID: "p1", Type: "provision", Payload: []byte("WATCHDOG_ENABLE=true\nWATCHDOG_IPMI_FALLBACK=true\n")},
		{ID: "r1", Type: "reboot"},
	}, func(*config.MachineConfig) error { ran = true; return nil })

	err := mode.Run(context.Background())
	var rebootErr *RebootRequestedError
	if !errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %T %[1]v, want standby to go on to the reboot command", err)
	}
	if ran {
		t.Fatal("the command ran without its payload")
	}
	if !slices.Equal(provider.acks, []string{"p1:failed", "r1:completed"}) {
		t.Fatalf("acks = %v", provider.acks)
	}
	if !strings.Contains(provider.ackMessages[0], "[BOOTY-PAY-001]") {
		t.Fatalf("ack message = %q, want the payload code", provider.ackMessages[0])
	}
}
