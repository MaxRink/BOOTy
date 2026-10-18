//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/ipmi"
	"github.com/telekom/BOOTy/pkg/oob"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/watchdog"
)

// watchdogModuleDir holds the watchdog drivers. BOOTy loads them only when
// the watchdog is enabled, so a host without WATCHDOG_ENABLE keeps the
// behavior of its firmware watchdog settings.
var watchdogModuleDir = "/modules/watchdog"

// Test seams.
var (
	openWatchdogDevice = watchdog.OpenFirst
	openBMCWatchdog    = func(timeout time.Duration) (watchdog.Device, error) {
		return watchdog.OpenBMC(ipmi.New(nil), timeout)
	}
)

// watchdogKeep is WATCHDOG_HANDOFF=keep of the running config.
var watchdogKeep atomic.Bool

// startWatchdog arms the hardware watchdog when WATCHDOG_ENABLE is set and
// starts the supervisor. A missing device is logged; the run goes on.
func startWatchdog(ctx context.Context, cfg *config.MachineConfig) {
	w := &cfg.Watchdog
	if !w.Enable {
		return
	}
	if cfg.Mode == "dry-run" {
		slog.Info("dry-run mode: not arming the hardware watchdog")
		return
	}
	loadWatchdogModules()
	dev, err := openConfiguredWatchdog(w)
	if err != nil {
		slog.Error("hardware watchdog not armed", "error", err)
		return
	}
	budgets := make(map[string]time.Duration, len(w.StepBudgets))
	for step, seconds := range w.StepBudgets {
		budgets[step] = time.Duration(seconds) * time.Second
	}
	sup := watchdog.NewSupervisor(dev, watchdog.Options{Budgets: budgets, OnStall: watchdogStalled})
	watchdogKeep.Store(w.KeepOnHandoff())
	watchdog.Activate(sup)
	// Not ctx: only a handoff ends the keepalive, never a signal.
	go sup.Run(context.WithoutCancel(ctx))
	slog.Info("hardware watchdog armed", "device", dev.Name(), "timeout", dev.Timeout(),
		"keepalive_every", sup.Interval(), "handoff", w.Handoff)
}

// openConfiguredWatchdog opens WATCHDOG_DEVICE or the default devices, and
// the BMC watchdog when none exists and WATCHDOG_IPMI_FALLBACK is set.
func openConfiguredWatchdog(w *config.WatchdogConfig) (watchdog.Device, error) {
	timeout := time.Duration(w.Timeout()) * time.Second
	paths := watchdog.DefaultDevices
	if w.Device != "" {
		paths = []string{w.Device}
	}
	dev, err := openWatchdogDevice(paths, timeout)
	if err == nil || !w.IPMIFallback {
		return dev, err //nolint:wrapcheck // the watchdog errors name the device
	}
	slog.Warn("no usable watchdog device, arming the BMC watchdog through IPMI", "error", err)
	bmc, bmcErr := openBMCWatchdog(timeout)
	if bmcErr != nil {
		return nil, errors.Join(err, bmcErr)
	}
	return bmc, nil
}

// loadWatchdogModules loads the drivers in watchdogModuleDir. A driver
// without its hardware fails with ENODEV, so failures are debug messages.
func loadWatchdogModules() {
	entries, err := os.ReadDir(watchdogModuleDir)
	if err != nil {
		slog.Debug("no watchdog modules", "path", watchdogModuleDir)
		return
	}
	var pending []string
	for _, entry := range entries {
		if !entry.IsDir() {
			pending = append(pending, filepath.Join(watchdogModuleDir, entry.Name()))
		}
	}
	// A few passes resolve the dependency order, like loadModules.
	for range 3 {
		var failed []string
		for _, path := range pending {
			if err := loadModuleFn(path); err != nil {
				failed = append(failed, path)
				slog.Debug("watchdog module not loaded", "module", filepath.Base(path), "error", err)
				continue
			}
			slog.Info("loaded watchdog module", "module", filepath.Base(path))
		}
		if len(failed) == 0 || len(failed) == len(pending) {
			return
		}
		pending = failed
	}
}

// newDiskCommander runs the disk commands of the run through
// watchdog.ProgressRunner: a finished wipe or erase command is progress.
func newDiskCommander() disk.Commander {
	return watchdog.ProgressRunner{Inner: &disk.ExecCommander{}}
}

// watchdogStalled records the stalled step with BOOTY-WDT-001 out of band
// (serial status line, BootyLastResult UEFI variable), so the next run and
// the operator see why the watchdog reset the host. No Go error flows
// there: the hardware ends the run.
var watchdogStalled = func(step string) {
	oob.Fail(step, provision.CodeStalled)
}

// watchdogHandoff ends the watchdog supervision before kexec, reboot or
// power off. A power off always disarms it: a BMC watchdog would power the
// host on again.
func watchdogHandoff(powerOff bool) {
	if err := watchdog.Handoff(watchdogKeep.Load() && !powerOff); err != nil {
		slog.Warn("watchdog handoff failed", "error", err)
	}
}
