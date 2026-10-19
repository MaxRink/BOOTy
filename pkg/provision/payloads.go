//go:build linux

package provision

import (
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/payload"
)

// RequirePayloads checks that the ramdisk has the payloads (pkg/payload)
// that the config needs: ipmitool for the IPMI watchdog fallback
// (WATCHDOG_ENABLE and WATCHDOG_IPMI_FALLBACK). A dry run needs none. FRR
// mode is checked where the network stack is chosen, after the netplan merge.
func RequirePayloads(cfg *config.MachineConfig) error {
	if cfg.DryRun || cfg.Mode == "dry-run" {
		return nil
	}
	if cfg.Watchdog.Enable && cfg.Watchdog.IPMIFallback {
		return payload.Require(payload.IPMITool)
	}
	return nil
}
