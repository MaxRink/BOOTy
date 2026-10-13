//go:build linux

package runmode

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/telekom/BOOTy/pkg/provision"
)

// DeprovisionMode wipes or soft-disables the installed OS.
type DeprovisionMode struct {
	deps             Deps
	newDeprovisioner func() standbyDeprovisioner
}

// Name returns the mode identifier.
func (m *DeprovisionMode) Name() string { return "deprovision" }

// Run executes the deprovisioning pipeline.
func (m *DeprovisionMode) Run(ctx context.Context) error {
	var orch standbyDeprovisioner
	if m.newDeprovisioner != nil {
		orch = m.newDeprovisioner()
	} else {
		orch = provision.NewOrchestrator(m.deps.Cfg, m.deps.Client, m.deps.DiskMgr)
	}
	if err := orch.Deprovision(ctx); err != nil {
		slog.Error("deprovisioning failed", "error", err)
		if m.deps.Cfg.WaitOnFailure() {
			// Like the legacy deprovision script: report the error and leave
			// the ramdisk running. A reboot would boot the ISO and wipe again.
			slog.Info("waiting after deprovisioning failure")
			<-ctx.Done()
			return fmt.Errorf("deprovisioning failed: %w", err)
		}
		return err
	}
	if m.deps.Cfg.PowerOffOnSuccess() {
		// Like the legacy deprovision script: power the host off.
		return &PowerOffRequestedError{}
	}
	return nil
}
