//go:build linux

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/efi"
	"github.com/telekom/BOOTy/pkg/oob"
	"github.com/telekom/BOOTy/pkg/provision"
)

const consoleActivePath = "/sys/class/tty/console/active"

// oobRun is the run ID of every status line of this BOOTy start.
var oobRun = oob.NewRunID()

// startOOBStatus installs the out-of-band status reporter (OOB_STATUS,
// OOB_EFI_VAR). With OOB_EFI_VAR it logs the result of the previous run.
// Without a config (nil) it installs the default serial reporter, so a
// failure before the config is loaded also gets a status line.
func startOOBStatus(ctx context.Context, cfg *config.MachineConfig) {
	opts := oob.Options{
		Serial:   true,
		Consoles: oob.ActiveConsoles(consoleActivePath),
		Version:  Version,
		Run:      oobRun,
	}
	if cfg == nil {
		oob.Install(oob.NewReporter(&opts))
		return
	}
	opts.Serial = cfg.Telemetry.OOBStatus != config.OOBStatusOff
	opts.Mode = cfg.Mode
	if config.BoolOr(cfg.Telemetry.OOBEFIVar, false) {
		opts.Vars = efiVarsForOOB(ctx)
	}
	r := oob.NewReporter(&opts)
	slog.Info("out-of-band status", "run", r.Run(), "serial", opts.Serial, "efi_var", opts.Vars != nil)
	oob.Install(r)
}

func efiVarsForOOB(ctx context.Context) *efi.EFIVarReader {
	if err := provision.NewConfigurator(nil).MountEFIVars(ctx); err != nil {
		slog.Warn("no BootyLastResult UEFI variable: efivarfs is not available", "error", err)
		return nil
	}
	if _, err := os.Stat("/sys/firmware/efi/efivars"); err != nil {
		slog.Info("no BootyLastResult UEFI variable: not a UEFI host")
		return nil
	}
	vars := efi.NewEFIVarReader("")
	oob.LogLastResult(vars)
	return vars
}
