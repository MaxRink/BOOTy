//go:build linux

package provision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

func TestReleaseStaleTargetMounts(t *testing.T) {
	oldMountPoint, oldUnmount := isMountPoint, unmountTargetTree
	t.Cleanup(func() { isMountPoint, unmountTargetTree = oldMountPoint, oldUnmount })

	var unmounted []string
	unmountTargetTree = func(_ *disk.Manager, root string) error {
		unmounted = append(unmounted, root)
		return nil
	}
	o := &Orchestrator{log: slog.Default(), sharedMounts: []string{"/newroot/var/lib/data"}}

	// The root probe can miss a stale child mount or fail to read the mount
	// table, so the recursive cleanup must run anyway.
	isMountPoint = func(string) bool { return false }
	if err := o.releaseStaleTargetMounts(context.Background()); err != nil {
		t.Fatalf("releaseStaleTargetMounts() = %v", err)
	}
	if len(unmounted) != 1 || unmounted[0] != newroot {
		t.Fatalf("unmounted = %v, want [%s]", unmounted, newroot)
	}
	if len(o.sharedMounts) != 0 {
		t.Fatalf("sharedMounts = %v, want reset", o.sharedMounts)
	}

	unmountTargetTree = func(*disk.Manager, string) error { return errors.New("read mounts: permission denied") }
	if err := o.releaseStaleTargetMounts(context.Background()); err == nil {
		t.Fatal("releaseStaleTargetMounts() = nil, want error when the target root may stay mounted")
	}
}

// A failed cleanup must reach CAPRF as an error, after init and before any
// destructive step, like every other step failure.
func TestProvisionReportsStaleMountCleanupFailure(t *testing.T) {
	oldUnmount := unmountTargetTree
	t.Cleanup(func() { unmountTargetTree = oldUnmount })
	unmountTargetTree = func(*disk.Manager, string) error { return errors.New("umount /newroot: device busy") }

	provider := &mockProvider{}
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, provider)
	err := o.Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "release-stale-mounts") {
		t.Fatalf("Provision() = %v, want a release-stale-mounts step error", err)
	}
	if len(provider.statuses) != 2 ||
		provider.statuses[0].status != config.StatusInit ||
		provider.statuses[1].status != config.StatusError ||
		!strings.Contains(provider.statuses[1].message, "device busy") {
		t.Fatalf("CAPRF statuses = %+v, want init, then an error with the unmount failure", provider.statuses)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("commands ran after the failed cleanup: %+v", cmd.calls)
	}
}

func TestReleaseStaleMountsRunsBeforeDestructiveSteps(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	steps := o.provisionSteps()
	if steps[0].Name != "report-init" || steps[1].Name != "release-stale-mounts" {
		t.Fatalf("first steps = %s, %s, want report-init, release-stale-mounts", steps[0].Name, steps[1].Name)
	}
	if _, ok := resumeStateSteps()["release-stale-mounts"]; !ok {
		t.Fatal("release-stale-mounts must also run on resume")
	}
}
