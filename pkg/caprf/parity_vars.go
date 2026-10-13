package caprf

import "github.com/telekom/BOOTy/pkg/config"

// applyParityVar sets the options that select CAPRF legacy behavior
// without CAPRF_LEGACY_COMPAT. The preset fills the ones that the vars file
// does not set (config.Config.ApplyPresets).
func applyParityVar(cfg *config.MachineConfig, key, value string) bool {
	strFields := map[string]*string{
		"SUCCESS_ACTION": &cfg.SuccessAction,
		"FAILURE_ACTION": &cfg.FailureAction,
	}
	if ptr, ok := strFields[key]; ok {
		*ptr = value
		return true
	}
	return false
}
