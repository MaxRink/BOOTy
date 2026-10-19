//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/timesync"
)

// clockSyncer is the time sync seam for tests.
type clockSyncer interface {
	Sync(ctx context.Context, opts timesync.Options) (timesync.Result, error)
}

// dhcpNTPServers returns the option 42 servers of the DHCP lease; a seam
// for tests.
var dhcpNTPServers = network.DHCPNTPServers

// newClockSyncer returns the syncer for the real clock.
var newClockSyncer = func(skipTLSVerify bool) clockSyncer {
	return timesync.NewSyncer(skipTLSVerify)
}

// syncClock sets the clock before the first request that checks a TLS
// certificate: the JWT exchange, the CAPRF callbacks and the image download.
// dateURL is the connectivity target, whose Date header is the baseline.
func syncClock(ctx context.Context, cfg *config.MachineConfig, dateURL string) error {
	mode := cfg.Time.SyncMode()
	if mode == config.TimeSyncOff {
		slog.Info("time sync is off, keeping the RTC time", "clock", time.Now().UTC().Format(time.RFC3339))
		return nil
	}
	opts := timesync.Options{
		Require:    mode == config.TimeSyncRequire,
		NTPServers: ntpServers(cfg.Time.NTPServers, leaseNTPServers(cfg)),
		DateURL:    dateURL,
		MaxSkew:    time.Duration(cfg.Time.MaxSkew()) * time.Second,
		WriteRTC:   config.BoolOr(cfg.Time.WriteRTC, true),
	}
	syncer := newClockSyncer(config.BoolOr(cfg.Transport.SkipTLSVerify, false))
	if _, err := syncer.Sync(ctx, opts); err != nil {
		return &provision.CodedError{Code: provision.CodeNoTime, Err: fmt.Errorf("time sync: %w", err)}
	}
	return nil
}

// leaseNTPServers returns the DHCP option 42 servers with TIME_DHCP_NTP,
// else none: the legacy ramdisk sends no NTP request.
func leaseNTPServers(cfg *config.MachineConfig) []string {
	if !config.BoolOr(cfg.Time.DHCPNTP, false) {
		return nil
	}
	return dhcpNTPServers()
}

// ntpServers returns the configured servers, then the DHCP servers, without
// duplicates.
func ntpServers(configured, dhcp []string) []string {
	var out []string
	for _, s := range slices.Concat(configured, dhcp) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
