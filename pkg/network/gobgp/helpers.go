//go:build linux

package gobgp

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/server"
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

// addPath originates a path built with the GoBGP API types. GoBGP v4
// BgpServer.AddPath takes the native types (pkg/server/server.go:2589), so
// the path is converted the way the gRPC server does it.
func addPath(s *server.BgpServer, p *apipb.Path) error {
	nlri, err := apiutil.GetNativeNlri(p)
	if err != nil {
		return fmt.Errorf("convert NLRI: %w", err)
	}
	attrs, err := apiutil.GetNativePathAttributes(p)
	if err != nil {
		return fmt.Errorf("convert path attributes: %w", err)
	}
	if _, err := s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{{
		Family: apiutil.ToFamily(p.GetFamily()),
		Nlri:   nlri,
		Attrs:  attrs,
	}}}); err != nil {
		return fmt.Errorf("add path %s: %w", nlri, err)
	}
	return nil
}

// apiPath converts a native path of a GoBGP v4 watch event into the API
// type the overlay handlers use.
func apiPath(p *apiutil.Path) (*apipb.Path, error) {
	out, err := apiutil.NewPath(p.Family, p.Nlri, p.Withdrawal, p.Attrs, time.Unix(p.Age, 0))
	if err != nil {
		return nil, fmt.Errorf("convert path %s: %w", p.Nlri, err)
	}
	return out, nil
}
