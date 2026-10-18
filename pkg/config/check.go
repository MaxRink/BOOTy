package config

import (
	"fmt"
	"time"
)

// CheckConfig controls the check mode (MODE=check) beyond the health
// checks.
type CheckConfig struct {
	// Hold keeps BOOTy running after the checks instead of rebooting:
	// "off" reboots (the default), "until-poweroff" holds until the
	// machine is powered off, as the bm4x connection check does (its
	// STOP_LLDP job powers the host off), and a duration such as "10m"
	// holds that long and then reboots. The links and LLDP stay up while
	// BOOTy holds.
	// Default: "" (off)
	Hold string `yaml:"hold,omitempty" json:"hold,omitempty"`
}

// Values of CheckConfig.Hold besides a duration.
const (
	CheckHoldOff           = "off"
	CheckHoldUntilPowerOff = "until-poweroff"
)

// HoldDuration returns how long check mode holds after the checks: 0 for
// off and a negative value for until power off.
func (c CheckConfig) HoldDuration() time.Duration {
	switch c.Hold {
	case "", CheckHoldOff:
		return 0
	case CheckHoldUntilPowerOff:
		return -1
	}
	d, err := time.ParseDuration(c.Hold)
	if err != nil || d <= 0 {
		return 0 // Validate rejects it
	}
	return d
}

func (c CheckConfig) validate() []string {
	switch c.Hold {
	case "", CheckHoldOff, CheckHoldUntilPowerOff:
		return nil
	}
	if d, err := time.ParseDuration(c.Hold); err != nil || d <= 0 {
		return []string{fmt.Sprintf("invalid check.hold (CHECK_HOLD) %q: want off, until-poweroff or a positive duration", c.Hold)}
	}
	return nil
}
