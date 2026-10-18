package config

import (
	"fmt"
	"net"
	"slices"
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

	// LLDPNeighborWait makes check mode wait, before the checks, until
	// every physical NIC with carrier has an LLDP neighbor, at most this
	// long (a duration such as "90s"). Switches send every 30 s.
	// Default: "" (no wait)
	LLDPNeighborWait string `yaml:"lldpNeighborWait,omitempty" json:"lldpNeighborWait,omitempty"`

	// Profiles lists what check mode runs: "health" (the hardware health
	// checks) and "cabling" (LLDP neighbors, expected cabling, link,
	// PCIe and transceiver checks of every physical NIC).
	// Default: nil (health)
	Profiles []string `yaml:"profiles,omitempty" json:"profiles,omitempty"`

	// ExpectedCabling is the cabling to compare with the LLDP neighbors,
	// for example rendered from NetBox by the caller. BOOTy never reads
	// NetBox itself.
	ExpectedCabling []ExpectedLink `yaml:"expectedCabling,omitempty" json:"expectedCabling,omitempty"`

	// Sinks lists where the check result goes besides the log: "file"
	// (/run/booty/check-result.json), "http" (POST to ResultURL), "caprf"
	// (the check results to HEALTH_CHECK_URL) and "bm4x-report" (the
	// expected cabling diff in the bm4x ConnectionReport form, to
	// /run/booty/bm4x-connection-report.json and the log).
	Sinks []string `yaml:"sinks,omitempty" json:"sinks,omitempty"`

	// ResultURL receives the check result document (sink "http").
	ResultURL string `yaml:"resultURL,omitempty" json:"resultURL,omitempty"`

	// Thresholds set the action and the limits of the cabling checks.
	Thresholds CheckThresholds `yaml:"thresholds,omitempty" json:"thresholds,omitempty"`
}

// ExpectedLink is the expected switch port of a NIC, named by its MAC (or
// its interface name when MAC is empty).
type ExpectedLink struct {
	MAC       string `yaml:"mac,omitempty" json:"mac,omitempty"`
	Interface string `yaml:"interface,omitempty" json:"interface,omitempty"`
	// Switch is the system name of the switch, Port its port name.
	Switch string `yaml:"switch" json:"switch"`
	Port   string `yaml:"port" json:"port"`
	// SpeedMbps is the expected link speed; 0 checks none.
	SpeedMbps int `yaml:"speedMbps,omitempty" json:"speedMbps,omitempty"`
}

// Values of the check profiles, sinks and threshold actions.
const (
	CheckProfileHealth  = "health"
	CheckProfileCabling = "cabling"

	CheckSinkFile       = "file"
	CheckSinkHTTP       = "http"
	CheckSinkCAPRF      = "caprf"
	CheckSinkBM4XReport = "bm4x-report"

	CheckActionOff   = "off"
	CheckActionWarn  = "warn"
	CheckActionBlock = "block"
)

// CheckThresholds sets per cabling check the action on failure ("off",
// "warn" or "block"; block fails the check run) and its limits. An empty
// action is the default in parentheses.
type CheckThresholds struct {
	LLDPMissing     string `yaml:"lldpMissing,omitempty" json:"lldpMissing,omitempty"`         // (warn) NIC with carrier, no neighbor
	CablingMismatch string `yaml:"cablingMismatch,omitempty" json:"cablingMismatch,omitempty"` // (block) differs from expectedCabling
	DualHoming      string `yaml:"dualHoming,omitempty" json:"dualHoming,omitempty"`           // (warn) all uplinks on one switch
	LinkSpeed       string `yaml:"linkSpeed,omitempty" json:"linkSpeed,omitempty"`             // (warn) below minSpeedMbps or the expected speed
	LinkFlaps       string `yaml:"linkFlaps,omitempty" json:"linkFlaps,omitempty"`             // (warn) more than maxFlaps carrier losses
	LinkErrors      string `yaml:"linkErrors,omitempty" json:"linkErrors,omitempty"`           // (warn) more than maxErrors receive errors
	FEC             string `yaml:"fec,omitempty" json:"fec,omitempty"`                         // (warn) uncorrectable blocks, or no FEC at 25G+
	PCIe            string `yaml:"pcie,omitempty" json:"pcie,omitempty"`                       // (warn) PCIe link below its maximum
	OpticsAlarm     string `yaml:"opticsAlarm,omitempty" json:"opticsAlarm,omitempty"`         // (block) a transceiver alarm
	OpticsWarning   string `yaml:"opticsWarning,omitempty" json:"opticsWarning,omitempty"`     // (warn) a transceiver warning

	MinSpeedMbps int    `yaml:"minSpeedMbps,omitempty" json:"minSpeedMbps,omitempty"`
	MaxFlaps     *int   `yaml:"maxFlaps,omitempty" json:"maxFlaps,omitempty"`   // default 4
	MaxErrors    uint64 `yaml:"maxErrors,omitempty" json:"maxErrors,omitempty"` // default 0
}

// HasProfile reports whether check mode runs a profile.
func (c *CheckConfig) HasProfile(p string) bool {
	if len(c.Profiles) == 0 {
		return p == CheckProfileHealth
	}
	return slices.Contains(c.Profiles, p)
}

// HasSink reports whether the check result goes to a sink.
func (c *CheckConfig) HasSink(s string) bool { return slices.Contains(c.Sinks, s) }

// NeighborWait returns the LLDP neighbor wait, 0 for none.
func (c *CheckConfig) NeighborWait() time.Duration {
	d, err := time.ParseDuration(c.LLDPNeighborWait)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// Values of CheckConfig.Hold besides a duration.
const (
	CheckHoldOff           = "off"
	CheckHoldUntilPowerOff = "until-poweroff"
)

// HoldDuration returns how long check mode holds after the checks: 0 for
// off and a negative value for until power off.
func (c *CheckConfig) HoldDuration() time.Duration {
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

func (c *CheckConfig) validate() []string {
	var errs []string
	switch c.Hold {
	case "", CheckHoldOff, CheckHoldUntilPowerOff:
	default:
		if d, err := time.ParseDuration(c.Hold); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("invalid check.hold (CHECK_HOLD) %q: want off, until-poweroff or a positive duration", c.Hold))
		}
	}
	errs = append(errs, validateList("check.profiles (CHECK_PROFILES)", c.Profiles, CheckProfileHealth, CheckProfileCabling)...)
	errs = append(errs, validateList("check.sinks (CHECK_RESULT_SINKS)", c.Sinks, CheckSinkFile, CheckSinkHTTP, CheckSinkCAPRF, CheckSinkBM4XReport)...)
	if c.HasSink(CheckSinkHTTP) && c.ResultURL == "" {
		errs = append(errs, "check.sinks (CHECK_RESULT_SINKS) http needs check.resultURL (CHECK_RESULT_URL)")
	}
	errs = append(errs, c.Thresholds.validate()...)
	errs = append(errs, validateExpectedCabling(c.ExpectedCabling)...)
	if c.LLDPNeighborWait != "" {
		if d, err := time.ParseDuration(c.LLDPNeighborWait); err != nil || d < 0 {
			errs = append(errs, fmt.Sprintf("invalid check.lldpNeighborWait (LLDP_NEIGHBOR_WAIT) %q: want a duration", c.LLDPNeighborWait))
		}
	}
	return errs
}

func (t *CheckThresholds) validate() []string {
	var errs []string
	for _, a := range []struct{ name, value string }{
		{"lldpMissing", t.LLDPMissing}, {"cablingMismatch", t.CablingMismatch}, {"dualHoming", t.DualHoming},
		{"linkSpeed", t.LinkSpeed}, {"linkFlaps", t.LinkFlaps}, {"linkErrors", t.LinkErrors}, {"fec", t.FEC},
		{"pcie", t.PCIe}, {"opticsAlarm", t.OpticsAlarm}, {"opticsWarning", t.OpticsWarning},
	} {
		if msg := validateEnum(a.value, "check.thresholds."+a.name+" (CHECK_THRESHOLDS)", CheckActionOff, CheckActionWarn, CheckActionBlock); msg != "" {
			errs = append(errs, msg)
		}
	}
	if t.MinSpeedMbps < 0 || t.MaxFlaps != nil && *t.MaxFlaps < 0 {
		errs = append(errs, "check.thresholds (CHECK_THRESHOLDS): minSpeedMbps and maxFlaps must not be negative")
	}
	return errs
}

// validateList checks that every value of a list is allowed.
func validateList(name string, values []string, allowed ...string) []string {
	var errs []string
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			errs = append(errs, fmt.Sprintf("invalid %s %q", name, v))
		}
	}
	return errs
}

func validateExpectedCabling(links []ExpectedLink) []string {
	var errs []string
	for i, l := range links {
		if (l.MAC == "") == (l.Interface == "") || l.Switch == "" || l.Port == "" {
			errs = append(errs, fmt.Sprintf("check.expectedCabling[%d] (CHECK_EXPECTED_CABLING): want mac or interface, switch and port", i))
		}
		if _, err := net.ParseMAC(l.MAC); l.MAC != "" && err != nil {
			errs = append(errs, fmt.Sprintf("check.expectedCabling[%d] (CHECK_EXPECTED_CABLING): bad mac %q", i, l.MAC))
		}
	}
	return errs
}
