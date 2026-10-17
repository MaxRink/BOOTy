//go:build linux

package provision

import (
	"context"
	"log/slog"

	"github.com/telekom/BOOTy/pkg/sriov"
)

// sriovRunner is the reconcile of configure-sriov.
type sriovRunner interface {
	Run(ctx context.Context) (*sriov.Result, error)
}

// newSRIOVRunner builds the reconcile of configure-sriov. Tests replace it.
var newSRIOVRunner = func(cfg *sriov.Config, log *slog.Logger) sriovRunner {
	return &sriov.Reconciler{
		Config:   cfg,
		Discover: sriov.NewInspector().Discover,
		Journal:  &sriov.Journal{Path: sriov.DefaultJournalPath},
		Log:      log,
	}
}

// configureSRIOV checks the SR-IOV profiles (provision.sriov, SRIOV_PROFILES)
// against the PF ports and applies them with onDrift apply. Stored firmware
// settings take effect only after a reboot, so the handoff then reboots
// instead of kexec.
func (o *Orchestrator) configureSRIOV(ctx context.Context) error {
	cfg := &o.cfg.Provision.SRIOV
	if len(cfg.Profiles) == 0 {
		o.log.Info("No SR-IOV profiles, skipping")
		return nil
	}
	res, err := newSRIOVRunner(cfg, o.log.With("component", "sriov")).Run(ctx)
	if res != nil && res.Reboot {
		o.firmwareChanged = true
	}
	// The inventory of collect-inventory predates these changes; send it
	// again so the reported SR-IOV state is the one at the handoff.
	if invErr := o.collectInventory(ctx); invErr != nil {
		o.log.Warn("SR-IOV inventory after configure-sriov failed", "error", invErr)
	}
	return err
}
