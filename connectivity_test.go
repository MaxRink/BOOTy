//go:build linux

package main

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
)

type connectivityTestMode struct {
	gotTarget string
}

func (m *connectivityTestMode) Setup(context.Context, *network.Config) error { return nil }

func (m *connectivityTestMode) WaitForConnectivity(_ context.Context, target string, _ time.Duration) error {
	m.gotTarget = target
	return nil
}

func (m *connectivityTestMode) Teardown(context.Context) error { return nil }

func TestEnsureNetworkConnectivityRedactsTargetInLogs(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	target := "http://leaky-user:super-secret@example.com/init?token=abc#frag"
	mode := &connectivityTestMode{}
	_, err := ensureNetworkConnectivity(context.Background(), &config.MachineConfig{}, mode, target)
	if err != nil {
		t.Fatalf("ensureNetworkConnectivity() error = %v", err)
	}
	if mode.gotTarget != target {
		t.Fatalf("WaitForConnectivity target = %q, want raw target passed through", mode.gotTarget)
	}

	got := logs.String()
	for _, leaked := range []string{"leaky-user", "super-secret", "token=abc", "frag"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("logs leaked %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "http://example.com/init") {
		t.Fatalf("logs = %q, want redacted target context", got)
	}
}

func TestSetRamdiskHostname(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.MachineConfig
		want string
	}{
		{name: "provision", cfg: config.MachineConfig{Mode: "provision", Hostname: "node-1"}, want: "node-1"},
		{name: "default mode", cfg: config.MachineConfig{Hostname: "node-1"}, want: "node-1"},
		{name: "deprovision keeps the name", cfg: config.MachineConfig{Mode: "deprovision", Hostname: "node-1"}},
		{name: "no hostname", cfg: config.MachineConfig{Mode: "provision"}},
		{name: "dry run", cfg: config.MachineConfig{Mode: "provision", Hostname: "node-1", DryRun: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ""
			setRamdiskHostname(&tt.cfg, func(name []byte) error {
				got = string(name)
				return nil
			})
			if got != tt.want {
				t.Fatalf("hostname = %q, want %q", got, tt.want)
			}
		})
	}
}

// A failed sethostname is only logged; the run goes on.
func TestSetRamdiskHostnameFailureIsLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	setRamdiskHostname(&config.MachineConfig{Hostname: "node-1"}, func([]byte) error { return errors.New("operation not permitted") })
	if !strings.Contains(logs.String(), "setting the ramdisk hostname failed") {
		t.Fatalf("logs = %q, want the warning", logs.String())
	}
}

func TestNetworkWait(t *testing.T) {
	rounds := func(n *int) config.MachineConfig {
		return config.MachineConfig{Network: config.NetworkConfig{WaitMode: config.NetworkWaitRounds, WaitMaxAttempts: n}}
	}
	tests := []struct {
		name         string
		cfg          config.MachineConfig
		wantAttempts int
		wantTimeout  time.Duration
	}{
		{name: "default", wantAttempts: 3, wantTimeout: 5 * time.Minute},
		{name: "attempts ignored with the default wait", cfg: config.MachineConfig{Network: config.NetworkConfig{WaitMaxAttempts: new(2)}}, wantAttempts: 3, wantTimeout: 5 * time.Minute},
		{name: "rounds default", cfg: rounds(nil), wantAttempts: 1, wantTimeout: 300 * time.Second},
		{name: "rounds attempts", cfg: rounds(new(3)), wantAttempts: 1, wantTimeout: 60 * time.Second},
		{name: "rounds explicit zero", cfg: rounds(new(0)), wantAttempts: 1, wantTimeout: 0},
		{name: "legacy preset", cfg: config.MachineConfig{LegacyCompat: true}, wantAttempts: 1, wantTimeout: 300 * time.Second},
		{name: "explicit default wins over preset", cfg: config.MachineConfig{LegacyCompat: true, Network: config.NetworkConfig{WaitMode: config.NetworkWaitDefault}}, wantAttempts: 3, wantTimeout: 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.ApplyPresets()
			attempts, timeout := networkWait(&tt.cfg)
			if attempts != tt.wantAttempts || timeout != tt.wantTimeout {
				t.Fatalf("networkWait() = %d, %s, want %d, %s", attempts, timeout, tt.wantAttempts, tt.wantTimeout)
			}
		})
	}
}

type blockingConnectivityMode struct{ *connectivityTestMode }

func (blockingConnectivityMode) WaitForConnectivity(ctx context.Context, _ string, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// NETWORK_WAIT_MODE=rounds bounds the whole wait, also for modes such as
// GoBGP that wait in two phases.
func TestWaitForConnectivityRoundsBoundTheWait(t *testing.T) {
	cfg := &config.MachineConfig{Network: config.NetworkConfig{WaitMode: config.NetworkWaitRounds}}
	err := waitForConnectivity(context.Background(), cfg, blockingConnectivityMode{&connectivityTestMode{}}, "http://example.com/init", 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForConnectivity() = %v, want context.DeadlineExceeded", err)
	}
}
