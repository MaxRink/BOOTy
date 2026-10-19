package caprf

import (
	"fmt"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
)

// applyTimeVar sets the time sync options. config.Validate checks the
// values.
func applyTimeVar(cfg *config.MachineConfig, key, value string) (bool, error) {
	switch key {
	case "TIME_SYNC":
		cfg.Time.Sync = strings.ToLower(strings.TrimSpace(value))
	case "NTP_SERVERS":
		cfg.Time.NTPServers = strings.Fields(strings.ReplaceAll(value, ",", " "))
	case "TIME_MAX_SKEW_SEC":
		if err := setIntField(&cfg.Time.MaxSkewSeconds, strings.TrimSpace(value)); err != nil {
			return true, fmt.Errorf("invalid %s: %w", key, err)
		}
	case "TIME_WRITE_RTC", "TIME_DHCP_NTP":
		b, set, err := parseStrictBoolVar(value)
		if err != nil {
			return true, fmt.Errorf("invalid %s: %w", key, err)
		}
		field := &cfg.Time.WriteRTC
		if key == "TIME_DHCP_NTP" {
			field = &cfg.Time.DHCPNTP
		}
		*field = nil
		if set {
			*field = &b
		}
	default:
		return false, nil
	}
	return true, nil
}

// parseStrictBoolVar parses a boolean vars value. An empty value means
// unset (the default); anything but true, false, 1, 0, yes or no is an
// error.
func parseStrictBoolVar(value string) (b, set bool, err error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return false, false, nil
	case "true", "1", "yes":
		return true, true, nil
	case "false", "0", "no":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("%q is not a boolean (true, false, 1, 0, yes, no)", value)
	}
}
