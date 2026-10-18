//go:build linux

package runmode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/linkcheck"
)

// statusProvider sends each reported status on a channel.
type statusProvider struct {
	standbyProvider
	reported chan config.Status
}

func (p *statusProvider) ReportStatus(_ context.Context, status config.Status, _ string) error {
	p.reported <- status
	return nil
}

// runCheck runs check mode with CHECK_HOLD on p and fails when Run does
// not return within limit.
func runCheck(ctx context.Context, t *testing.T, p *statusProvider, holdValue string, limit time.Duration) {
	t.Helper()
	cfg := &config.MachineConfig{Mode: "check", Check: config.CheckConfig{Hold: holdValue}}
	cfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,minimum-memory,minimum-cpu,nic-link-state,thermal-state"
	done := make(chan struct{})
	go func() {
		_ = (&CheckMode{deps: Deps{Cfg: cfg, Client: p}}).Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("hold %q: Run did not return within %s", holdValue, limit)
	}
}

func newStatusProvider() *statusProvider {
	return &statusProvider{reported: make(chan config.Status, 4)}
}

// CHECK_HOLD=until-poweroff reports success and then keeps Run going
// until the context ends; off returns at once; a duration holds that long.
func TestCheckModeHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newStatusProvider()
	var held time.Time
	go func() {
		// The success report comes before the hold, while Run still runs.
		for s := range p.reported {
			if s == config.StatusSuccess {
				break
			}
		}
		held = time.Now()
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	runCheck(ctx, t, p, "until-poweroff", 5*time.Second)
	if held.IsZero() || time.Since(held) < 200*time.Millisecond {
		t.Fatalf("until-poweroff: Run returned before the context ended (success reported at %v)", held)
	}

	started := time.Now()
	runCheck(context.Background(), t, newStatusProvider(), "300ms", 5*time.Second)
	if d := time.Since(started); d < 300*time.Millisecond {
		t.Fatalf("hold 300ms returned after %s", d)
	}
	runCheck(context.Background(), t, newStatusProvider(), "", time.Second)
	runCheck(context.Background(), t, newStatusProvider(), "off", time.Second)
}

// With LLDP_RECEIVE=false no neighbor can arrive, so check mode does not
// wait for one.
func TestCheckModeNoNeighborWaitWithoutReceive(t *testing.T) {
	off := false
	cfg := &config.MachineConfig{Mode: "check", Check: config.CheckConfig{LLDPNeighborWait: "1h"}}
	cfg.Network.LLDPReceive = &off
	cfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,minimum-memory,minimum-cpu,nic-link-state,thermal-state"
	done := make(chan struct{})
	go func() {
		_ = (&CheckMode{deps: Deps{Cfg: cfg, Client: newStatusProvider()}}).Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("check mode waited for LLDP neighbors although LLDP receive is off")
	}
}

// Result sinks also get the result of a run without the cabling profile.
func TestCheckModeSinksWithoutCablingProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "check-result.json")
	orig := linkcheck.ResultPath
	t.Cleanup(func() { linkcheck.ResultPath = orig })
	linkcheck.ResultPath = path
	off := false
	cfg := &config.MachineConfig{Mode: "check", Check: config.CheckConfig{Profiles: []string{config.CheckProfileHealth}, Sinks: []string{config.CheckSinkFile}}}
	cfg.Network.LLDPReceive = &off
	cfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,minimum-memory,minimum-cpu,nic-link-state,thermal-state"
	if err := (&CheckMode{deps: Deps{Cfg: cfg, Client: newStatusProvider()}}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file sink not written: %v", err)
	}
}
