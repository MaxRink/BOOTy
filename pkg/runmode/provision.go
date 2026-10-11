//go:build linux

package runmode

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/rescue"
)

// ProvisionMode runs the full provisioning pipeline with rescue/retry handling.
type ProvisionMode struct {
	deps            Deps
	succeeded       bool
	firmwareChanged bool
}

// Name returns the mode identifier.
func (m *ProvisionMode) Name() string { return "provision" }

// Run executes the provisioning pipeline with rescue/retry handling.
func (m *ProvisionMode) Run(ctx context.Context) error {
	orch := provision.NewOrchestrator(m.deps.Cfg, m.deps.Client, m.deps.DiskMgr)
	rescueCfg := orch.RescueConfig()
	var retryState rescue.RetryState

	for {
		err := orch.Provision(ctx)
		if err == nil {
			m.succeeded = true
			m.firmwareChanged = orch.FirmwareChanged()
			return provisionComplete(m.firmwareChanged, m.deps.Cfg.LegacyCompat)
		}
		slog.Error("provisioning failed", "error", err)

		if setupErr := rescue.Setup(ctx, rescueCfg); setupErr != nil {
			slog.Warn("rescue setup error", "error", setupErr)
		}
		action := rescue.Decide(rescueCfg, &retryState)
		slog.Info("rescue action", "type", action.Type, "message", action.Message)

		switch action.Type {
		case rescue.ModeRetry:
			retryState.RecordAttempt(err)
			slog.Info("retrying provisioning", "attempt", retryState.Attempts, "delay", rescueCfg.RetryDelay)
			if !sleepWithContext(ctx, rescueCfg.RetryDelay) {
				return fmt.Errorf("retry canceled: %w", ctx.Err())
			}
			continue
		case rescue.ModeShell:
			slog.Info("dropping to rescue shell")
			return &RescueShellError{}
		case rescue.ModeWait:
			slog.Info("waiting for manual intervention")
			<-ctx.Done()
			return fmt.Errorf("wait mode canceled: %w", ctx.Err())
		default:
			return err
		}
	}
}

// Succeeded returns whether provisioning completed without error.
func (m *ProvisionMode) Succeeded() bool { return m.succeeded }

// FirmwareChanged returns whether firmware was modified during provisioning.
func (m *ProvisionMode) FirmwareChanged() bool { return m.firmwareChanged }

func provisionComplete(firmwareChanged, legacyCompat bool) *ProvisionCompleteError {
	// The legacy CAPRF provisioner shuts the host down after success and
	// CAPRF powers it on again from disk.
	return &ProvisionCompleteError{FirmwareChanged: firmwareChanged, PowerOff: legacyCompat}
}
