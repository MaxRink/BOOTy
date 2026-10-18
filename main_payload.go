//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/payload"
	"github.com/telekom/BOOTy/pkg/provision"
)

// checkPayloads runs before the network setup. It checks that every
// payload archive is from the build of this BOOTy and that the ramdisk has
// the payloads that the config needs (provision.RequirePayloads).
func checkPayloads(cfg *config.MachineConfig) error {
	return errors.Join(payload.Check(Build, runtime.GOARCH), provision.RequirePayloads(cfg))
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
