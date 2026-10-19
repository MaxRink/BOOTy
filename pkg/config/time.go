package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// TimeConfig controls the clock sync before the first TLS connection.
// BOOTy measures the clock against NTP servers and the HTTP Date header of
// the CAPRF connectivity target, steps the system clock and writes the RTC.
type TimeConfig struct {
	// Sync selects the time sync.
	// "off": keep the RTC time.
	// "auto": sync from the sources that answer; failures are logged.
	// "require": fail the run (BOOTY-TIM-001) when no source answers.
	// Valid values: "off", "auto", "require".
	// Default: "" (auto)
	Sync string `yaml:"sync" json:"sync"`

	// NTPServers are the SNTP servers, as host or host:port, queried in
	// order. With DHCPNTP the servers from DHCP option 42 follow them.
	// Default: none
	NTPServers []string `yaml:"ntpServers" json:"ntpServers"`

	// DHCPNTP also queries the NTP servers of the DHCP leases (option 42).
	// The legacy ramdisk has no NTP client, so it is off by default.
	// Default: nil (false)
	DHCPNTP *bool `yaml:"dhcpNTP,omitempty" json:"dhcpNTP,omitempty"`

	// MaxSkewSeconds is the largest difference between the NTP time and the
	// authenticated CAPRF Date time that BOOTy accepts. Above it BOOTy uses
	// the Date time, since NTP is not authenticated.
	// Default: 0 (300)
	MaxSkewSeconds int `yaml:"maxSkewSeconds" json:"maxSkewSeconds"`

	// WriteRTC writes the synced time to the RTC (in UTC), so the installed
	// OS starts with it.
	// Default: nil (true)
	WriteRTC *bool `yaml:"writeRTC,omitempty" json:"writeRTC,omitempty"`
}

// Time sync modes.
const (
	TimeSyncOff     = "off"
	TimeSyncAuto    = "auto"
	TimeSyncRequire = "require"
)

const (
	// DefaultTimeMaxSkewSeconds is the default of MaxSkewSeconds.
	DefaultTimeMaxSkewSeconds = 300
	// MaxTimeMaxSkewSeconds bounds MaxSkewSeconds (one day).
	MaxTimeMaxSkewSeconds = 86400
	// MaxNTPServers bounds NTPServers.
	MaxNTPServers = 8
)

// SyncMode returns Sync with the default applied.
func (t *TimeConfig) SyncMode() string {
	if t.Sync == "" {
		return TimeSyncAuto
	}
	return t.Sync
}

// MaxSkew returns MaxSkewSeconds with the default applied.
func (t *TimeConfig) MaxSkew() int {
	if t.MaxSkewSeconds == 0 {
		return DefaultTimeMaxSkewSeconds
	}
	return t.MaxSkewSeconds
}

// validateTime checks the time sync options.
func validateTime(t *TimeConfig) []string {
	var errs []string
	switch t.Sync {
	case "", TimeSyncOff, TimeSyncAuto, TimeSyncRequire:
	default:
		errs = append(errs, fmt.Sprintf("time.sync (TIME_SYNC) must be off, auto or require, got %q", t.Sync))
	}
	if t.MaxSkewSeconds < 0 || t.MaxSkewSeconds > MaxTimeMaxSkewSeconds {
		errs = append(errs, fmt.Sprintf("time.maxSkewSeconds (TIME_MAX_SKEW_SEC) must be between 1 and %d, or 0 for the default %d", MaxTimeMaxSkewSeconds, DefaultTimeMaxSkewSeconds))
	}
	if len(t.NTPServers) > MaxNTPServers {
		errs = append(errs, fmt.Sprintf("time.ntpServers (NTP_SERVERS) has %d entries, at most %d are allowed", len(t.NTPServers), MaxNTPServers))
	}
	for _, server := range t.NTPServers {
		if err := ValidateNTPServer(server); err != nil {
			errs = append(errs, fmt.Sprintf("time.ntpServers (NTP_SERVERS): %v", err))
		}
	}
	return errs
}

// ValidateNTPServer checks one NTP server entry: a host name or IP address
// with an optional port. IPv6 addresses with a port need brackets.
func ValidateNTPServer(server string) error {
	if server == "" || strings.TrimSpace(server) != server {
		return fmt.Errorf("server %q must not be empty or have spaces", server)
	}
	if strings.Contains(server, "://") {
		return fmt.Errorf("server %q must be a host or host:port, not a URL", server)
	}
	host := server
	if h, port, err := net.SplitHostPort(server); err == nil {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("server %q has an invalid port", server)
		}
		host = h
	} else if strings.Count(server, ":") == 1 {
		return fmt.Errorf("server %q has an invalid port", server)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if !validHostName(host) {
		return fmt.Errorf("server %q is not a valid host name or IP address", server)
	}
	return nil
}

// validHostName checks a DNS name: labels of letters, digits and hyphens.
func validHostName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if !validHostLabel(label) {
			return false
		}
	}
	return true
}

func validHostLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, r := range label {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
