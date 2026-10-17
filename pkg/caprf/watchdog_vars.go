package caprf

import (
	"fmt"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
)

// applyWatchdogVar sets the hardware watchdog options. config.Validate
// checks the values.
func applyWatchdogVar(cfg *config.MachineConfig, key, value string) (bool, error) {
	w := &cfg.Watchdog
	var err error
	switch key {
	case "WATCHDOG_ENABLE":
		w.Enable, err = parseWatchdogBool(value)
	case "WATCHDOG_IPMI_FALLBACK":
		w.IPMIFallback, err = parseWatchdogBool(value)
	case "WATCHDOG_TIMEOUT_SEC":
		err = setIntField(&w.TimeoutSeconds, strings.TrimSpace(value))
	case "WATCHDOG_DEVICE":
		w.Device = strings.TrimSpace(value)
	case "WATCHDOG_HANDOFF":
		w.Handoff = strings.ToLower(strings.TrimSpace(value))
	case "WATCHDOG_STEP_BUDGETS":
		w.StepBudgets = nil
		if strings.TrimSpace(value) != "" {
			err = unmarshalJSONVar(value, &w.StepBudgets)
		}
	default:
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("invalid %s: %w", key, err)
	}
	return true, nil
}

// parseWatchdogBool parses a boolean strictly: a typo must not silently
// leave the watchdog off. Empty means false.
func parseWatchdogBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "0", "no":
		return false, nil
	case "true", "1", "yes":
		return true, nil
	default:
		return false, fmt.Errorf("%q is not a boolean (true, false, 1, 0, yes, no)", value)
	}
}
