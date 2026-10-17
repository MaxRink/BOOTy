//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/timesync"
)

type fakeClockSyncer struct {
	calls int
	opts  timesync.Options
	skip  bool
	err   error
}

func (f *fakeClockSyncer) Sync(_ context.Context, opts timesync.Options) (timesync.Result, error) {
	f.calls++
	f.opts = opts
	return timesync.Result{}, f.err
}

func withFakeClockSyncer(t *testing.T, f *fakeClockSyncer) {
	t.Helper()
	orig := newClockSyncer
	newClockSyncer = func(skip bool) clockSyncer { f.skip = skip; return f }
	t.Cleanup(func() { newClockSyncer = orig })
}

func TestSyncClockOptions(t *testing.T) {
	f := &fakeClockSyncer{}
	withFakeClockSyncer(t, f)
	skip, writeRTC := true, false
	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{SkipTLSVerify: &skip},
		Time: config.TimeConfig{Sync: config.TimeSyncRequire, NTPServers: []string{"ntp.example.com"},
			MaxSkewSeconds: 60, WriteRTC: &writeRTC},
	}
	if err := syncClock(context.Background(), cfg, "https://caprf.example/status/init"); err != nil {
		t.Fatalf("syncClock: %v", err)
	}
	o := f.opts
	if !o.Require || o.DateURL != "https://caprf.example/status/init" || o.MaxSkew != time.Minute || o.WriteRTC ||
		strings.Join(o.NTPServers, ",") != "ntp.example.com" || !f.skip {
		t.Fatalf("options = %+v, skip = %v", o, f.skip)
	}
}

func TestSyncClockDefaults(t *testing.T) {
	f := &fakeClockSyncer{}
	withFakeClockSyncer(t, f)
	if err := syncClock(context.Background(), &config.MachineConfig{}, ""); err != nil {
		t.Fatalf("syncClock: %v", err)
	}
	if o := f.opts; o.Require || o.MaxSkew != 5*time.Minute || !o.WriteRTC || f.skip {
		t.Fatalf("default options = %+v", o)
	}
}

func TestSyncClockOffAndErrors(t *testing.T) {
	f := &fakeClockSyncer{err: timesync.ErrNoTime}
	withFakeClockSyncer(t, f)
	if err := syncClock(context.Background(), &config.MachineConfig{Time: config.TimeConfig{Sync: config.TimeSyncOff}}, ""); err != nil || f.calls != 0 {
		t.Fatalf("TIME_SYNC=off: err %v, calls %d", err, f.calls)
	}
	err := syncClock(context.Background(), &config.MachineConfig{Time: config.TimeConfig{Sync: config.TimeSyncRequire}}, "")
	if !errors.Is(err, timesync.ErrNoTime) || !strings.Contains(err.Error(), timesync.CodeNoTime) {
		t.Fatalf("require without a source: err = %v", err)
	}
}

func TestNTPServersDeduplicates(t *testing.T) {
	got := ntpServers([]string{"a", "b"}, []string{"b", "10.0.2.3"})
	if strings.Join(got, ",") != "a,b,10.0.2.3" {
		t.Fatalf("ntpServers = %v", got)
	}
	if ntpServers(nil, nil) != nil {
		t.Fatal("ntpServers(nil, nil) != nil")
	}
}

// The DHCP option 42 servers follow NTP_SERVERS in the sync options.
func TestSyncClockUsesDHCPNTPServers(t *testing.T) {
	f := &fakeClockSyncer{}
	withFakeClockSyncer(t, f)
	orig := dhcpNTPServers
	t.Cleanup(func() { dhcpNTPServers = orig })
	dhcpNTPServers = func() []string { return []string{"10.0.2.3", "ntp.example.com"} }
	cfg := &config.MachineConfig{Time: config.TimeConfig{NTPServers: []string{"ntp.example.com"}}}
	if err := syncClock(context.Background(), cfg, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.opts.NTPServers, ","); got != "ntp.example.com,10.0.2.3" {
		t.Fatalf("NTPServers = %q, want the configured server, then the DHCP one", got)
	}
}

func TestSyncClockErrorCarriesFailureCode(t *testing.T) {
	withFakeClockSyncer(t, &fakeClockSyncer{err: timesync.ErrNoTime})
	err := syncClock(context.Background(), &config.MachineConfig{Time: config.TimeConfig{Sync: config.TimeSyncRequire}}, "")
	if got := provision.CodeOf(err); got != provision.CodeNoTime {
		t.Fatalf("CodeOf(%v) = %q, want %s", err, got, provision.CodeNoTime)
	}
}
