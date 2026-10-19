//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/telekom/BOOTy/pkg/payload"
)

// checkPayloads runs before the network setup. It checks that every
// payload archive is from the build of this BOOTy. FRR mode checks its
// payload where the network stack is chosen (requireFRRPayload).
func checkPayloads() error {
	return payload.Check(Build, runtime.GOARCH) //nolint:wrapcheck // names the payload
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
