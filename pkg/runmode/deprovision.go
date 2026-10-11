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
		if m.deps.Cfg.LegacyCompat {
			// The legacy deprovision script reports the error and leaves the
			// ramdisk running. A reboot would boot the ISO and wipe again.
			slog.Info("legacy compat: waiting after deprovisioning failure")
			<-ctx.Done()
			return fmt.Errorf("deprovisioning failed: %w", err)
		}
		return err
	}
	if m.deps.Cfg.LegacyCompat {
		// The legacy deprovision script powers the host off after success.
		return &PowerOffRequestedError{}
	}
	return nil
}
