//go:build !linux

package network

import (
	"context"
	"fmt"
	"time"
)

// DHCPMode is only functional on Linux, where raw netlink/DHCP access is
// available. The non-Linux implementation keeps tests and config parsing
// portable while returning an explicit setup error.
type DHCPMode struct{}

// NewDHCPMode returns a non-Linux DHCP mode stub.
func NewDHCPMode() *DHCPMode {
	return &DHCPMode{}
}

// Setup returns an explicit unsupported error on non-Linux platforms.
func (d *DHCPMode) Setup(context.Context, *Config) error {
	return fmt.Errorf("dhcp mode is only supported on Linux")
}

// WaitForConnectivity uses the generic HTTP readiness probe on non-Linux platforms.
func (d *DHCPMode) WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error {
	return WaitForHTTP(ctx, target, timeout)
}

// Teardown is a no-op for the non-Linux DHCP mode stub.
func (d *DHCPMode) Teardown(context.Context) error {
	return nil
}

// ManagesDNS is false: the stub writes nothing.
func (d *DHCPMode) ManagesDNS() bool { return false }
