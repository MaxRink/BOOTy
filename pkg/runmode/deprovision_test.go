//go:build linux

package runmode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

func newTestDeprovisionMode(t *testing.T, legacyCompat bool, deprovErr error) *DeprovisionMode {
	t.Helper()
	return &DeprovisionMode{
		deps: Deps{Cfg: &config.MachineConfig{Mode: "deprovision", LegacyCompat: legacyCompat}},
		newDeprovisioner: func() standbyDeprovisioner {
			return standbyDeprovisionerFunc(func(context.Context) error { return deprovErr })
		},
	}
}

func TestDeprovisionModeSuccessExit(t *testing.T) {
	if err := newTestDeprovisionMode(t, false, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil (reboot) by default", err)
	}
	var powerOff *PowerOffRequestedError
	if err := newTestDeprovisionMode(t, true, nil).Run(context.Background()); !errors.As(err, &powerOff) {
		t.Fatalf("Run() = %v, want power off request in legacy compat", err)
	}
}

func TestDeprovisionModeFailureReturnsByDefault(t *testing.T) {
	wipeErr := errors.New("wipe failed")
	if err := newTestDeprovisionMode(t, false, wipeErr).Run(context.Background()); !errors.Is(err, wipeErr) {
		t.Fatalf("Run() = %v, want %v", err, wipeErr)
	}
}

// Legacy evidence: deprovision.sh reports the error and exits the script;
// the ramdisk keeps running. Run must block until its context ends.
func TestDeprovisionModeFailureWaitsInLegacyCompat(t *testing.T) {
	wipeErr := errors.New("wipe failed")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- newTestDeprovisionMode(t, true, wipeErr).Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Run() returned %v before the context ended", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, wipeErr) {
			t.Fatalf("Run() = %v, want %v", err, wipeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the context ended")
	}
}
