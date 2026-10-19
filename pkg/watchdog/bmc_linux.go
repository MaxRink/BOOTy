//go:build linux

package watchdog

import (
	"context"
	"fmt"
	"time"
)

// bmcCommandTimeout bounds one IPMI command.
const bmcCommandTimeout = 20 * time.Second

// bmcWatchdog is the BMC watchdog command set of *ipmi.Manager.
type bmcWatchdog interface {
	SetWatchdog(ctx context.Context, timeout time.Duration) error
	ResetWatchdog(ctx context.Context) error
	StopWatchdog(ctx context.Context) error
}

// bmcDevice is the BMC watchdog, driven with IPMI commands. The BMC resets
// the host even when the kernel hangs, and the timer survives a reboot.
type bmcDevice struct {
	bmc     bmcWatchdog
	timeout time.Duration
}

// OpenBMC programs and starts the BMC watchdog.
func OpenBMC(bmc bmcWatchdog, timeout time.Duration) (Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bmcCommandTimeout)
	defer cancel()
	if err := bmc.SetWatchdog(ctx, timeout); err != nil {
		return nil, fmt.Errorf("arm the BMC watchdog: %w", err)
	}
	return &bmcDevice{bmc: bmc, timeout: timeout}, nil
}

func (d *bmcDevice) Name() string           { return "ipmi-bmc" }
func (d *bmcDevice) Timeout() time.Duration { return d.timeout }

func (d *bmcDevice) Keepalive() error {
	ctx, cancel := context.WithTimeout(context.Background(), bmcCommandTimeout)
	defer cancel()
	if err := d.bmc.ResetWatchdog(ctx); err != nil {
		return fmt.Errorf("pet the BMC watchdog: %w", err)
	}
	return nil
}

func (d *bmcDevice) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), bmcCommandTimeout)
	defer cancel()
	if err := d.bmc.StopWatchdog(ctx); err != nil {
		return fmt.Errorf("stop the BMC watchdog: %w", err)
	}
	return nil
}

// Release leaves the BMC timer running.
func (d *bmcDevice) Release() error { return nil }
