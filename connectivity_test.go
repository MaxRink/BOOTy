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

func TestSetLegacyRamdiskHostname(t *testing.T) {
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
			setLegacyRamdiskHostname(&tt.cfg, func(name []byte) error {
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
func TestSetLegacyRamdiskHostnameFailureIsLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	setLegacyRamdiskHostname(&config.MachineConfig{Hostname: "node-1"}, func([]byte) error { return errors.New("operation not permitted") })
	if !strings.Contains(logs.String(), "setting the ramdisk hostname failed") {
		t.Fatalf("logs = %q, want the warning", logs.String())
	}
}
