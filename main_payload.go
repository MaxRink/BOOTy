//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/payload"
)

// checkPayloads runs before the network setup. It checks that every
// payload archive is from the build of this BOOTy, and that the payloads
// the vars need and that a split build left out were loaded: ipmitool for
// the IPMI watchdog fallback, qemu-img for a qcow2 image URL. FRR mode is
// checked where the network stack is chosen, after the netplan merge.
func checkPayloads(cfg *config.MachineConfig) error {
	errs := []error{payload.Check(Build)}
	if cfg.Watchdog.Enable && cfg.Watchdog.IPMIFallback && cfg.Mode != "dry-run" {
		errs = append(errs, payload.Require(payload.IPMITool))
	}
	for _, u := range cfg.Provision.Image.URLs {
		if image.LooksQCOW2(u) {
			errs = append(errs, payload.Require(payload.QemuImg))
			break
		}
	}
	return errors.Join(errs...)
}

// requireFRRPayload fails FRR network mode on a split ramdisk without the
// FRR archive, instead of the DHCP fallback that would hide the missing
// payload. It tears the link layers down on that failure.
func requireFRRPayload(ctx context.Context, linkCleanup *linkLayerCleanup) error {
	err := payload.Require(payload.FRR)
	if err == nil {
		return nil
	}
	if cleanupErr := linkCleanup.Teardown(ctx); cleanupErr != nil {
		slog.Warn("link-layer cleanup after the FRR payload check failed", "error", cleanupErr)
	}
	return fmt.Errorf("FRR network mode: %w", err)
}
