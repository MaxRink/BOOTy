//go:build linux

package intel

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	"github.com/telekom/BOOTy/pkg/firmware/nic/ethtool"
)

// Manager handles Intel NIC firmware operations.
type Manager struct {
	*ethtool.Manager
}

// New creates an Intel firmware manager.
func New(log *slog.Logger) *Manager {
	return &Manager{Manager: ethtool.New(nic.VendorIntel, "0x8086", log)}
}

// NewWithCommander creates an Intel firmware manager with a custom commander.
func NewWithCommander(log *slog.Logger, commander ethtool.Commander) *Manager {
	return &Manager{Manager: ethtool.NewWithCommander(nic.VendorIntel, "0x8086", log, commander)}
}

// DisableFWLLDP stops the firmware LLDP agent of an Intel NIC with the
// flag of its driver (i40e disable-fw-lldp, ice fw-lldp-agent) through
// nic.DisableFirmwareLLDP, which also remembers the value for
// nic.RestoreFirmwareLLDP.
func (m *Manager) DisableFWLLDP(_ context.Context, iface string) error {
	if iface == "" {
		return fmt.Errorf("interface name required")
	}
	_, err := nic.DisableFirmwareLLDP(iface)
	return err //nolint:wrapcheck // nic wraps its errors
}

// CriticalParams returns the list of critical Intel firmware parameters.
func CriticalParams() []string {
	return []string{
		"disable-fw-lldp",
		"link-down-on-close",
		"channel-inline-flow-director",
	}
}
