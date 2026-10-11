//go:build linux

package runmode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

type standbyProvider struct {
	statuses        []config.Status
	heartbeat       int
	fetches         int
	commands        []config.Command
	statusErr       error
	heartErr        error
	commandErr      error
	statusErrOnCall int
	acks            []string
}

type standbyDeprovisionerFunc func(context.Context) error

func (f standbyDeprovisionerFunc) Deprovision(ctx context.Context) error {
	return f(ctx)
}

func (p *standbyProvider) GetConfig(_ context.Context) (*config.MachineConfig, error) {
	return &config.MachineConfig{}, nil
}

func (p *standbyProvider) ReportStatus(_ context.Context, status config.Status, _ string) error {
	p.statuses = append(p.statuses, status)
	if p.statusErrOnCall > 0 && len(p.statuses) != p.statusErrOnCall {
		return nil
	}
	return p.statusErr
}

func (p *standbyProvider) ShipLog(_ context.Context, _ string) error { return nil }

func (p *standbyProvider) Heartbeat(_ context.Context) error {
	p.heartbeat++
	return p.heartErr
}

func (p *standbyProvider) FetchCommands(_ context.Context) ([]config.Command, error) {
	p.fetches++
	return p.commands, p.commandErr
}

func (p *standbyProvider) AcknowledgeCommand(_ context.Context, cmdID, status, _ string) error {
	p.acks = append(p.acks, cmdID+":"+status)
	return nil
}

func (p *standbyProvider) ReportInventory(_ context.Context, _ []byte) error { return nil }
func (p *standbyProvider) ReportFirmware(_ context.Context, _ []byte) error  { return nil }

func standbyConfig() *config.MachineConfig {
	return &config.MachineConfig{
		Mode: "standby",
		Agent: config.AgentConfig{
			HeartbeatURL: "https://caprf.example.com/status/heartbeat",
			CommandsURL:  "https://caprf.example.com/commands",
		},
	}
}

func TestStandbyRequiresHeartbeatAndCommandsURLs(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.MachineConfig
		want string
	}{
		{
			name: "both missing",
			cfg:  &config.MachineConfig{Mode: "standby"},
			want: "HEARTBEAT_URL and COMMANDS_URL",
		},
		{
			name: "heartbeat missing",
			cfg: &config.MachineConfig{
				Mode:  "standby",
				Agent: config.AgentConfig{CommandsURL: "https://caprf.example.com/commands"},
			},
			want: "HEARTBEAT_URL",
		},
		{
			name: "commands missing",
			cfg: &config.MachineConfig{
				Mode:  "standby",
				Agent: config.AgentConfig{HeartbeatURL: "https://caprf.example.com/status/heartbeat"},
			},
			want: "COMMANDS_URL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mode := &StandbyMode{deps: Deps{Cfg: tc.cfg, Client: &standbyProvider{}}}
			err := mode.Run(context.Background())
			if err == nil {
				t.Fatal("expected missing endpoint error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestStandbyPerformsInitialReadinessChecks(t *testing.T) {
	provider := &standbyProvider{commands: []config.Command{{ID: "cmd-1", Type: "reboot"}}}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}

	err := mode.Run(context.Background())
	var rebootErr *RebootRequestedError
	if !errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %T %[1]v, want RebootRequestedError", err)
	}
	if len(provider.statuses) != 1 || provider.statuses[0] != config.StatusInit {
		t.Fatalf("statuses = %v, want one init status", provider.statuses)
	}
	if provider.heartbeat != 1 {
		t.Fatalf("heartbeat calls = %d, want 1", provider.heartbeat)
	}
	if provider.fetches != 1 {
		t.Fatalf("command fetches = %d, want 1", provider.fetches)
	}
	if len(provider.acks) != 1 || provider.acks[0] != "cmd-1:completed" {
		t.Fatalf("acks = %v, want completed reboot ack", provider.acks)
	}
}

func TestStandbyReadinessFailsOnCommandPollError(t *testing.T) {
	provider := &standbyProvider{commandErr: errors.New("commands unavailable")}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}

	err := mode.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "standby command readiness") {
		t.Fatalf("Run error = %v, want command readiness failure", err)
	}
}

func TestStandbyReadinessFailsOnStatusError(t *testing.T) {
	provider := &standbyProvider{statusErr: errors.New("status unavailable")}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}

	err := mode.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "standby status readiness") {
		t.Fatalf("Run error = %v, want status readiness failure", err)
	}
	if provider.heartbeat != 0 {
		t.Fatalf("heartbeat calls = %d, want 0 after status readiness failure", provider.heartbeat)
	}
	if provider.fetches != 0 {
		t.Fatalf("command fetches = %d, want 0 after status readiness failure", provider.fetches)
	}
}

func TestStandbyReadinessFailsOnHeartbeatError(t *testing.T) {
	provider := &standbyProvider{heartErr: errors.New("heartbeat unavailable")}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}

	err := mode.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "standby heartbeat readiness") {
		t.Fatalf("Run error = %v, want heartbeat readiness failure", err)
	}
	if provider.heartbeat != 1 {
		t.Fatalf("heartbeat calls = %d, want 1", provider.heartbeat)
	}
	if provider.fetches != 0 {
		t.Fatalf("command fetches = %d, want 0 after heartbeat readiness failure", provider.fetches)
	}
}

func TestStandbyInitialCommandCanExit(t *testing.T) {
	provider := &standbyProvider{commands: []config.Command{{ID: "cmd-1", Type: "health-check"}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}

	err := mode.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "standby canceled") {
		t.Fatalf("Run error = %v, want standby canceled after health-check", err)
	}
	if len(provider.acks) != 1 || provider.acks[0] != "cmd-1:completed" {
		t.Fatalf("acks = %v, want completed health-check ack", provider.acks)
	}
}

func TestStandbyDeprovisionCommandFailureReturnsErrorWithoutReboot(t *testing.T) {
	deprovisionErr := errors.New("deprovision status failed")
	provider := &standbyProvider{
		commands: []config.Command{{ID: "cmd-1", Type: "deprovision"}},
	}
	var deprovisionCalls int
	mode := &StandbyMode{
		deps: Deps{Cfg: standbyConfig(), Client: provider},
		newDeprovisioner: func() standbyDeprovisioner {
			return standbyDeprovisionerFunc(func(context.Context) error {
				deprovisionCalls++
				return deprovisionErr
			})
		},
	}

	err := mode.Run(context.Background())
	if !errors.Is(err, deprovisionErr) {
		t.Fatalf("Run error = %v, want deprovision error", err)
	}
	var rebootErr *RebootRequestedError
	if errors.As(err, &rebootErr) {
		t.Fatalf("Run error = %T %[1]v, did not want RebootRequestedError", err)
	}
	if len(provider.acks) != 1 || provider.acks[0] != "cmd-1:failed" {
		t.Fatalf("acks = %v, want failed deprovision ack", provider.acks)
	}
	if deprovisionCalls != 1 {
		t.Fatalf("deprovision calls = %d, want 1", deprovisionCalls)
	}
}

func newLegacyCompatStandby(t *testing.T, deprovErr error) *StandbyMode {
	t.Helper()
	cfg := standbyConfig()
	cfg.LegacyCompat = true
	return &StandbyMode{
		deps: Deps{Cfg: cfg, Client: &standbyProvider{commands: []config.Command{{ID: "cmd-1", Type: "deprovision"}}}},
		newDeprovisioner: func() standbyDeprovisioner {
			return standbyDeprovisionerFunc(func(context.Context) error { return deprovErr })
		},
	}
}

func TestStandbyDeprovisionCommandLegacyCompatPowersOff(t *testing.T) {
	err := newLegacyCompatStandby(t, nil).Run(context.Background())
	var powerOff *PowerOffRequestedError
	if !errors.As(err, &powerOff) {
		t.Fatalf("Run error = %v, want power off request", err)
	}
}

func TestStandbyDeprovisionCommandLegacyCompatFailureWaits(t *testing.T) {
	deprovisionErr := errors.New("wipe failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- newLegacyCompatStandby(t, deprovisionErr).Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Run returned %v before the context ended", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, deprovisionErr) {
			t.Fatalf("Run error = %v, want %v", err, deprovisionErr)
		}
		var rebootErr *RebootRequestedError
		if errors.As(err, &rebootErr) {
			t.Fatalf("Run error = %v, legacy compat must not reboot", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
}

// ackSignalProvider reports each ACK on a channel, so a test can see it
// while Run is still blocked.
type ackSignalProvider struct {
	*standbyProvider
	acked chan string
}

func (p *ackSignalProvider) AcknowledgeCommand(_ context.Context, cmdID, status, _ string) error {
	p.acked <- cmdID + ":" + status
	return nil
}

// A failed hot provision is ACKed before the rescue wait starts.
func TestStandbyProvisionFailureAcksBeforeWait(t *testing.T) {
	cfg := standbyConfig()
	cfg.LegacyCompat = true // rescue mode defaults to wait
	provider := &ackSignalProvider{
		standbyProvider: &standbyProvider{commands: []config.Command{{ID: "p1", Type: "provision"}}},
		acked:           make(chan string, 1),
	}
	mode := &StandbyMode{deps: Deps{Cfg: cfg, Client: provider}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mode.Run(ctx) }()

	select {
	case ack := <-provider.acked:
		if ack != "p1:failed" {
			t.Fatalf("ack = %q, want p1:failed", ack)
		}
	case err := <-done:
		t.Fatalf("Run returned %v before the failed provision was ACKed", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no ACK while Run waits")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v after the ACK, want it to wait", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
}
