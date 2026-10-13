package config

import "strings"

// Values of SuccessAction and FailureAction.
const (
	SuccessActionReboot   = "reboot"
	SuccessActionPowerOff = "poweroff"
	FailureActionReboot   = "reboot"
	FailureActionWait     = "wait"
)

// ApplyPresets sets the options that a preset such as LegacyCompat implies.
// It only sets options that are not set, so an explicit option always wins.
// Validate calls it first. It is safe to call more than once.
func (c *Config) ApplyPresets() {
	if c.LegacyCompat {
		c.applyLegacyCompatPreset()
	}
}

// applyLegacyCompatPreset holds the option values of CAPRF_LEGACY_COMPAT,
// which make BOOTy behave like the CAPRF legacy shell provisioner.
// docs/caprf-legacy-migration.md lists them in the preset column.
func (c *Config) applyLegacyCompatPreset() {
	presetString(&c.SuccessAction, SuccessActionPowerOff)
	presetString(&c.FailureAction, FailureActionWait)
}

// presetString sets an unset (blank) string option.
func presetString(field *string, value string) {
	if strings.TrimSpace(*field) == "" {
		*field = value
	}
}
