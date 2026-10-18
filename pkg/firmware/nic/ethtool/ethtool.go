//go:build linux

package ethtool

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	netethtool "github.com/telekom/BOOTy/pkg/network/ethtool"
)

// PrivFlagIO reads and sets driver private flags. NetlinkPrivFlags (ethtool
// netlink) is the one implementation; tests replace it.
type PrivFlagIO struct {
	Get func(iface string) ([]netethtool.Flag, error)
	Set func(iface, name string, on bool) error
}

// NetlinkPrivFlags reads and sets private flags over ethtool netlink.
var NetlinkPrivFlags = PrivFlagIO{Get: netethtool.PrivateFlags, Set: netethtool.SetPrivateFlag}

// Commander is an alias for nic.Commander for backward compatibility.
type Commander = nic.Commander

// OSCommander is an alias for nic.OSCommander for backward compatibility.
type OSCommander = nic.OSCommander

// Manager provides a shared ethtool-based NIC firmware implementation
// for vendors that use ethtool (Broadcom, Intel).
type Manager struct {
	log       *slog.Logger
	vendor    nic.Vendor
	vendorID  string
	commander Commander
	flags     PrivFlagIO
}

// New creates an ethtool-based Manager for the given vendor.
func New(vendor nic.Vendor, vendorID string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, vendor: vendor, vendorID: vendorID, commander: OSCommander{}, flags: NetlinkPrivFlags}
}

// NewWithCommander creates an ethtool-based Manager with a custom Commander.
func NewWithCommander(vendor nic.Vendor, vendorID string, log *slog.Logger, cmd Commander) *Manager {
	if log == nil {
		log = slog.Default()
	}
	if cmd == nil {
		cmd = OSCommander{}
	}
	return &Manager{log: log, vendor: vendor, vendorID: vendorID, commander: cmd, flags: NetlinkPrivFlags}
}

// WithPrivFlags replaces the private flag access (for tests).
func (m *Manager) WithPrivFlags(io PrivFlagIO) *Manager {
	m.flags = io
	return m
}

// Vendor returns the NIC vendor this manager handles.
func (m *Manager) Vendor() nic.Vendor { return m.vendor }

// Supported checks if this manager can handle the given NIC.
func (m *Manager) Supported(n *nic.Identifier) bool {
	return n != nil && n.VendorID == m.vendorID
}

// Capture reads firmware parameters from a NIC via ethtool.
func (m *Manager) Capture(ctx context.Context, n *nic.Identifier) (*nic.FirmwareState, error) {
	if n == nil {
		return nil, fmt.Errorf("nic identifier required")
	}
	if n.Interface == "" {
		return nil, fmt.Errorf("interface name required for ethtool operations")
	}
	state := &nic.FirmwareState{
		NIC:        *n,
		Parameters: make(map[string]nic.Parameter),
	}
	if err := m.CaptureViaEthtool(ctx, n.Interface, state); err != nil {
		return nil, fmt.Errorf("capture firmware from %s: %w", n.Interface, err)
	}
	m.log.Info("captured NIC firmware", "vendor", m.vendor, "pci", n.PCIAddress, "params", len(state.Parameters))
	return state, nil
}

// Apply sets firmware parameters on a NIC via ethtool.
func (m *Manager) Apply(ctx context.Context, n *nic.Identifier, changes []nic.FlagChange) error {
	if n == nil {
		return fmt.Errorf("nic identifier required")
	}
	if n.Interface == "" {
		return fmt.Errorf("interface name required for ethtool operations")
	}
	for _, change := range changes {
		if err := m.ApplyViaEthtool(ctx, n.Interface, change); err != nil {
			return fmt.Errorf("set %s=%s: %w", change.Name, change.Value, err)
		}
	}
	return nil
}

// CaptureViaEthtool captures the driver metadata via "ethtool -i" and the
// private flags over ethtool netlink.
func (m *Manager) CaptureViaEthtool(ctx context.Context, iface string, state *nic.FirmwareState) error {
	out, err := m.commander.CombinedOutput(ctx, "ethtool", "-i", iface)
	if err != nil {
		return fmt.Errorf("ethtool -i %s: %w", iface, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "firmware-version" {
			state.FWVersion = val
		}
		state.Parameters[key] = nic.Parameter{
			Name:     key,
			Current:  val,
			ReadOnly: true,
		}
	}

	flags, err := m.flags.Get(iface)
	if err != nil {
		m.log.Debug("private flags not read (optional)", "interface", iface, "error", err)
		return nil
	}
	for _, f := range flags {
		value := "off"
		if f.On {
			value = "on"
		}
		state.Parameters[f.Name] = nic.Parameter{Name: f.Name, Current: value}
	}
	return nil
}

// ApplyViaEthtool sets a private flag ("on" or "off") over ethtool
// netlink.
func (m *Manager) ApplyViaEthtool(_ context.Context, iface string, change nic.FlagChange) error {
	var on bool
	switch change.Value {
	case "on":
		on = true
	case "off":
	default:
		return fmt.Errorf("private flag %s of %s: value %q, want on or off", change.Name, iface, change.Value)
	}
	if err := m.flags.Set(iface, change.Name, on); err != nil {
		return fmt.Errorf("set private flag %s of %s: %w", change.Name, iface, err)
	}
	return nil
}
