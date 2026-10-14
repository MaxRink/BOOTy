//go:build linux

package runmode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
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
}

func (p *payloadProvider) CommandConfig(payload []byte) (*config.MachineConfig, []string, error) {
	return caprf.MergeCommandVars([]byte(payloadTestBaseVars), payload)
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

// skippingDeprovisioner reports disks in use that its wipe skipped.
type skippingDeprovisioner struct{ skipped []string }

func (skippingDeprovisioner) Deprovision(context.Context) error { return nil }
func (s skippingDeprovisioner) SkippedDisks() []string          { return s.skipped }

// The completed ACK names the disks in use that the wipe of all disks
// skipped, like the success status.
func TestStandbyDeprovisionAckNamesSkippedDisks(t *testing.T) {
	mode, provider := payloadStandby(t, []config.Command{{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=x")}},
		func(*config.MachineConfig) error { return nil })
	mode.deps.Cfg.Agent.DeprovisionSuccessAction = ""
	mode.newDeprovisioner = func(*config.MachineConfig) standbyDeprovisioner {
		return skippingDeprovisioner{skipped: []string{"/dev/sdc", "/dev/sdd"}}
	}

	_ = mode.Run(context.Background())
	want := "deprovisioning complete with payload keys HOSTNAME; not wiped, in use: /dev/sdc, /dev/sdd"
	if provider.ackMessages[0] != want {
		t.Fatalf("ack message = %q, want %q", provider.ackMessages[0], want)
	}
}
