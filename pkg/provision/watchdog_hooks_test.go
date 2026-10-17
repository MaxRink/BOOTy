//go:build linux

package provision

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/watchdog"
)

// watchdogRecorder is an active watchdog tracker that records the calls.
type watchdogRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *watchdogRecorder) add(c string)       { r.mu.Lock(); r.calls = append(r.calls, c); r.mu.Unlock() }
func (r *watchdogRecorder) Step(name string)   { r.add("step " + name) }
func (r *watchdogRecorder) Progress()          { r.add("progress") }
func (r *watchdogRecorder) Idle()              { r.add("idle") }
func (r *watchdogRecorder) Handoff(bool) error { r.add("handoff"); return nil }

func activateWatchdogRecorder(t *testing.T) *watchdogRecorder {
	t.Helper()
	r := &watchdogRecorder{}
	watchdog.Activate(r)
	t.Cleanup(func() { watchdog.Activate(nil) })
	return r
}

// Provision marks every step it starts and ends idle, also after a failed
// step, so the watchdog does not hold a finished step against its budget.
func TestProvisionReportsStepsToTheWatchdog(t *testing.T) {
	oldUnmount := unmountTargetTree
	t.Cleanup(func() { unmountTargetTree = oldUnmount })
	unmountTargetTree = func(*disk.Manager, string) error { return errors.New("device busy") }
	r := activateWatchdogRecorder(t)

	o, _ := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, &mockProvider{})
	if err := o.Provision(context.Background()); err == nil {
		t.Fatal("Provision() = nil, want the release-stale-mounts error")
	}
	if got := strings.Join(r.calls, ","); got != "step report-init,step release-stale-mounts,idle" {
		t.Fatalf("watchdog calls = %q", got)
	}
}

func TestDeprovisionReportsStepsToTheWatchdog(t *testing.T) {
	r := activateWatchdogRecorder(t)
	cfg := &config.MachineConfig{Mode: "soft"}
	o := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(newMockCommander()))
	_ = o.Deprovision(context.Background())

	if len(r.calls) < 2 || r.calls[len(r.calls)-1] != "idle" {
		t.Fatalf("watchdog calls = %v, want steps and then idle", r.calls)
	}
	steps := o.deprovisionSteps("soft")
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, "step "+step.Name)
	}
	got := r.calls[:len(r.calls)-1]
	if len(got) == 0 || !slices.Equal(got, names[:len(got)]) {
		t.Fatalf("watchdog steps = %v, want a prefix of %v", got, names)
	}
}
