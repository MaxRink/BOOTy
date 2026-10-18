//go:build linux

package provision

import (
	"errors"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/payload"
)

// RequirePayloads checks that the ramdisk has the payloads (pkg/payload)
// that the config needs: ipmitool for the IPMI watchdog fallback
// (WATCHDOG_ENABLE and WATCHDOG_IPMI_FALLBACK), qemu-img for a qcow2 image
// of a provision. A dry run needs none. FRR mode is checked where the
// network stack is chosen, after the netplan merge.
func RequirePayloads(cfg *config.MachineConfig) error {
	if cfg.DryRun || cfg.Mode == "dry-run" {
		return nil
	}
	var errs []error
	if cfg.Watchdog.Enable && cfg.Watchdog.IPMIFallback {
		errs = append(errs, payload.Require(payload.IPMITool))
	}
	if !config.IsDeprovisionMode(cfg.Mode) {
		for _, u := range cfg.Provision.Image.URLs {
			if image.LooksQCOW2(u) {
				errs = append(errs, payload.Require(payload.QemuImg))
				break
			}
		}
	}
	return errors.Join(errs...)
}
