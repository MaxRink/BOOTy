//go:build linux

package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
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
		Backends: sriov.DefaultBackends(log),
		Runtime:  kernelRuntime(),
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
	if err == nil && res != nil && res.VerifyReboot {
		switch {
		case !cfg.VerifyRebootEnabled():
			o.log.Warn("SRIOV_VERIFY_REBOOT=off: the runtime settings that wait for the firmware change are not verified",
				"detail", res.VerifyDetail)
		case o.cfg.Transport.RebootURL == "":
			return fmt.Errorf("%w (%s)", ErrVerifyRebootUnsupported, res.VerifyDetail)
		default:
			return o.requestVerifyReboot(ctx, RebootReasonSRIOVFirmware, res.VerifyDetail)
		}
	}
	// The inventory of collect-inventory predates these changes; send it
	// again. Firmware values that wait for the reboot still show the old
	// active values (sriovTotalVFs, linkType).
	if invErr := o.collectInventory(ctx); invErr != nil {
		o.log.Warn("SR-IOV inventory after configure-sriov failed", "error", invErr)
	}
	return err
}

// ErrVerifyRebootUnsupported fails the run when a verify reboot is needed
// but CAPRF has no reboot-required support (no REBOOT_URL): a plain
// reboot would boot the old OS in the middle of the task, whose compute
// profile can rewrite NUM_OF_VFS, and the task would stall.
var ErrVerifyRebootUnsupported = errors.New("sriov-verify-reboot-unsupported: SR-IOV firmware changed; " +
	"a verify reboot needs REBOOT_URL support in CAPRF (or SRIOV_VERIFY_REBOOT=off to hand off without the runtime check)")

// kernelRuntime is the SR-IOV runtime with the interfaces of the ramdisk
// network, so SR-IOV changes leave the BGP uplinks alone.
func kernelRuntime() *sriov.KernelRuntime {
	k := sriov.NewKernelRuntime()
	k.Uplinks = func() ([]string, bool, bool) {
		u, known := network.ActiveInterfaces()
		return u.Names, u.AllPhysical, known
	}
	return k
}

// RebootRequiredError stops the task before any disk change: BOOTy stored
// firmware settings that need a reset before it can check the rest, and
// boots the ramdisk again. It is not a failure; the run mode reboots
// without rescue handling and without the error status.
type RebootRequiredError struct {
	// Reason is the reason token, for example sriov-firmware.
	Reason string
	// Detail are more key=value tokens, separated by spaces.
	Detail string
}

func (e *RebootRequiredError) Error() string { return e.Message() }

// Message is the body of the reboot-required status: one line of
// space-separated tokens, "reboot-required reason=<reason>" followed by
// the key=value tokens of Detail. Values have no spaces.
func (e *RebootRequiredError) Message() string {
	msg := "reboot-required reason=" + e.Reason
	if e.Detail != "" {
		msg += " " + e.Detail
	}
	return msg
}

// RebootReasonSRIOVFirmware is the reason of a verify reboot after stored
// SR-IOV firmware changes.
const RebootReasonSRIOVFirmware = "sriov-firmware"

// requestVerifyReboot reports the reboot-required status (REBOOT_URL) and
// returns the RebootRequiredError that ends the task. CAPRF keeps or
// inserts again the boot media, so the host boots BOOTy again; there the
// stored firmware values are active and the runtime settings are checked.
func (o *Orchestrator) requestVerifyReboot(ctx context.Context, reason, detail string) error {
	rerr := &RebootRequiredError{Reason: reason, Detail: detail}
	o.log.Warn("SR-IOV firmware changed: rebooting into the ramdisk to verify the runtime settings", "status", rerr.Message())
	o.reportRebootRequired(ctx, rerr.Message())
	return rerr
}

// Timing of the reboot-required report. CAPRF arms the boot media again
// before it answers 2xx, so BOOTy waits for that answer before the reboot.
var (
	rebootRequiredWait  = 30 * time.Second
	rebootRequiredRetry = 2 * time.Second
)

// reportRebootRequired posts the reboot-required status until the server
// answers 2xx, for at most rebootRequiredWait. Without an answer BOOTy
// reboots anyway and logs an error: CAPRF may then boot the disk instead
// of BOOTy.
func (o *Orchestrator) reportRebootRequired(ctx context.Context, msg string) {
	deadline := time.Now().Add(rebootRequiredWait)
	for attempt := 1; ; attempt++ {
		err := o.provider.ReportStatus(ctx, config.StatusRebootRequired, msg)
		if err == nil {
			return
		}
		if time.Now().Add(rebootRequiredRetry).After(deadline) || ctx.Err() != nil {
			o.log.Error("no 2xx for the reboot-required status, rebooting anyway; the host may not boot BOOTy again",
				"attempts", attempt, "error", err)
			return
		}
		o.log.Warn("reporting the reboot-required status failed, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(rebootRequiredRetry):
		}
	}
}
