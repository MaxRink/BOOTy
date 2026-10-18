//go:build linux

package sriov

import (
	"log/slog"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
)

// DefaultBackends returns the firmware backends of the ramdisk, in the
// order BOOTy asks them: devlink parameters first, because they also work
// under Secure Boot (kernel lockdown), then mstconfig (mstflint), mlxconfig
// (MFT, only with NIC_TOOLS=vendor and a flavor that has it) and the Intel
// private flags. A setting belongs to the first backend that reads it.
func DefaultBackends(log *slog.Logger) []FirmwareBackend {
	cmd := nic.OSCommander{}
	return []FirmwareBackend{
		NewDevlinkParams(),
		NewMstconfig(cmd),
		NewMlxconfig(cmd),
		NewIntelFlags(log, cmd),
	}
}
