package caprf

import (
	"fmt"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
)

// applySRIOVVar handles the SR-IOV profile keys (provision.sriov in YAML).
func applySRIOVVar(cfg *config.MachineConfig, key, value string) (bool, error) {
	switch key {
	case "SRIOV_PROFILES":
		// A JSON list like provision.sriov.profiles. An unknown field fails,
		// like in the YAML config, so a typo never drops a setting.
		cfg.Provision.SRIOV.Profiles = nil
		if strings.TrimSpace(value) == "" {
			return true, nil
		}
		if err := strictJSONVar(value, &cfg.Provision.SRIOV.Profiles); err != nil {
			return true, fmt.Errorf("invalid %s: %w", key, err)
		}
	case "SRIOV_ON_DRIFT":
		cfg.Provision.SRIOV.OnDrift = strings.TrimSpace(value)
	case "NIC_TOOLS":
		cfg.Provision.SRIOV.NICTools = strings.TrimSpace(value)
	case "REBOOT_URL":
		// The reboot-required endpoint of the SR-IOV verify reboot.
		cfg.Transport.RebootURL = strings.TrimSpace(value)
	case "SRIOV_VERIFY_REBOOT":
		cfg.Provision.SRIOV.VerifyReboot = strings.TrimSpace(value)
	case "SRIOV_UPLINK_VF_DRIVERS":
		cfg.Provision.SRIOV.UplinkVFDrivers = nil
		for _, d := range strings.Split(value, ",") {
			if d = strings.TrimSpace(d); d != "" {
				cfg.Provision.SRIOV.UplinkVFDrivers = append(cfg.Provision.SRIOV.UplinkVFDrivers, d)
			}
		}
	default:
		return false, nil
	}
	return true, nil
}
