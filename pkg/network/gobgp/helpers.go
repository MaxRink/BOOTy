//go:build linux

package gobgp

import (
	"fmt"
	"log/slog"
	"os"
)

// procSysNet is the sysctl tree of the network settings; tests point it
// to a directory.
var procSysNet = "/proc/sys/net"

// forwardingSysctls are the critical sysctls of the forwarding options.
// IPv4 forwarding is on unless the configuration turns it off (frr.conf
// "no ip forwarding", or IP_FORWARDING=false). IPv6 forwarding is off
// unless it is turned on (frr.conf "ipv6 forwarding", or
// IPV6_FORWARDING=true): the legacy ramdisk keeps the kernel default (0),
// zebra and netplan do not set it, and BOOTy only originates and receives
// IPv6 traffic. With 1 the kernel would also set the Router flag in its
// Neighbor Advertisements.
func forwardingSysctls(ipv4, ipv6 *bool) map[string]string {
	return map[string]string{
		procSysNet + "/ipv4/ip_forward":             sysctlBool(ipv4, true),
		procSysNet + "/ipv6/conf/all/forwarding":    sysctlBool(ipv6, false),
		procSysNet + "/ipv4/conf/all/rp_filter":     "0",
		procSysNet + "/ipv4/conf/default/rp_filter": "0",
	}
}

// enableForwarding sets kernel sysctls for IP forwarding.
func enableForwarding(log *slog.Logger, ipv4, ipv6 *bool) error {
	for path, val := range forwardingSysctls(ipv4, ipv6) {
		if err := os.WriteFile(path, []byte(val), 0o644); err != nil { //nolint:gosec // sysctl paths are trusted
			return fmt.Errorf("set sysctl %s: %w", path, err)
		}
	}

	// Best-effort: accept_ra settings help but are not fatal.
	optional := map[string]string{
		"/proc/sys/net/ipv6/conf/all/accept_ra":            "2",
		"/proc/sys/net/ipv6/conf/all/accept_ra_defrtr":     "1",
		"/proc/sys/net/ipv6/conf/default/accept_ra":        "2",
		"/proc/sys/net/ipv6/conf/default/accept_ra_defrtr": "1",
	}
	for path, val := range optional {
		if err := os.WriteFile(path, []byte(val), 0o644); err != nil { //nolint:gosec // sysctl paths are trusted
			log.Debug("Failed to set sysctl", "path", path, "error", err)
		}
	}
	return nil
}
