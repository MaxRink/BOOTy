//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/oob"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/watchdog"
)

type stubWatchdog struct {
	name              string
	stopped, released bool
}

func (d *stubWatchdog) Name() string           { return d.name }
func (d *stubWatchdog) Timeout() time.Duration { return time.Minute }
func (d *stubWatchdog) Keepalive() error       { return nil }
func (d *stubWatchdog) Stop() error            { d.stopped = true; return nil }
func (d *stubWatchdog) Release() error         { d.released = true; return nil }

func stubWatchdogOpeners(t *testing.T, devErr error, bmcErr error) (*[]string, *stubWatchdog, *stubWatchdog) {
	t.Helper()
	origDev, origBMC := openWatchdogDevice, openBMCWatchdog
	t.Cleanup(func() { openWatchdogDevice, openBMCWatchdog = origDev, origBMC })
	var paths []string
	dev, bmc := &stubWatchdog{name: "dev"}, &stubWatchdog{name: "bmc"}
	openWatchdogDevice = func(p []string, _ time.Duration) (watchdog.Device, error) {
		paths = p
		if devErr != nil {
			return nil, devErr
		}
		return dev, nil
	}
	openBMCWatchdog = func(time.Duration) (watchdog.Device, error) {
		if bmcErr != nil {
			return nil, bmcErr
		}
		return bmc, nil
	}
	return &paths, dev, bmc
}

func TestOpenConfiguredWatchdog(t *testing.T) {
	paths, dev, _ := stubWatchdogOpeners(t, nil, nil)
	got, err := openConfiguredWatchdog(&config.WatchdogConfig{})
	if err != nil || got != dev || !slices.Equal(*paths, watchdog.DefaultDevices) {
		t.Fatalf("default devices: %v, %v, paths %v", got, err, *paths)
	}
	if _, err := openConfiguredWatchdog(&config.WatchdogConfig{Device: "/dev/watchdog1"}); err != nil || !slices.Equal(*paths, []string{"/dev/watchdog1"}) {
		t.Fatalf("WATCHDOG_DEVICE: %v, paths %v", err, *paths)
	}
}

func TestOpenConfiguredWatchdogIPMIFallback(t *testing.T) {
	_, _, bmc := stubWatchdogOpeners(t, watchdog.ErrNoDevice, nil)
	if _, err := openConfiguredWatchdog(&config.WatchdogConfig{}); !errors.Is(err, watchdog.ErrNoDevice) {
		t.Fatalf("without fallback: %v", err)
	}
	got, err := openConfiguredWatchdog(&config.WatchdogConfig{IPMIFallback: true})
	if err != nil || got != bmc {
		t.Fatalf("with fallback: %v, %v", got, err)
	}
	stubWatchdogOpeners(t, watchdog.ErrNoDevice, errors.New("no /dev/ipmi0"))
	if _, err := openConfiguredWatchdog(&config.WatchdogConfig{IPMIFallback: true}); !errors.Is(err, watchdog.ErrNoDevice) || err.Error() == watchdog.ErrNoDevice.Error() {
		t.Fatalf("both fail: %v", err)
	}
}

func TestStartWatchdogAndHandoff(t *testing.T) {
	t.Cleanup(func() { watchdog.Activate(nil); watchdogKeep.Store(false) })
	for _, tt := range []struct {
		name         string
		handoff      string
		powerOff     bool
		wantReleased bool
	}{
		{"stop at reboot", "", false, false},
		{"keep at kexec or reboot", config.WatchdogHandoffKeep, false, true},
		{"power off always stops", config.WatchdogHandoffKeep, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, dev, _ := stubWatchdogOpeners(t, nil, nil)
			startWatchdog(context.Background(), &config.MachineConfig{Mode: "provision",
				Watchdog: config.WatchdogConfig{Enable: true, Handoff: tt.handoff}})
			watchdogHandoff(tt.powerOff)
			if dev.released != tt.wantReleased || dev.stopped == tt.wantReleased {
				t.Fatalf("released %v, stopped %v", dev.released, dev.stopped)
			}
		})
	}
}

func TestStartWatchdogSkips(t *testing.T) {
	t.Cleanup(func() { watchdog.Activate(nil) })
	paths, _, _ := stubWatchdogOpeners(t, nil, nil)
	startWatchdog(context.Background(), &config.MachineConfig{Mode: "provision"})
	startWatchdog(context.Background(), &config.MachineConfig{Mode: "dry-run", Watchdog: config.WatchdogConfig{Enable: true}})
	if *paths != nil {
		t.Fatal("the watchdog was opened while disabled or in dry-run")
	}
	// No device: the run goes on without a supervisor.
	stubWatchdogOpeners(t, watchdog.ErrNoDevice, nil)
	watchdog.Activate(nil)
	startWatchdog(context.Background(), &config.MachineConfig{Mode: "provision", Watchdog: config.WatchdogConfig{Enable: true}})
	if err := watchdog.Handoff(false); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWatchdogModules(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"iTCO_wdt.ko", "lpc_ich.ko", "sp5100_tco.ko"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	origDir, origLoad := watchdogModuleDir, loadModuleFn
	t.Cleanup(func() { watchdogModuleDir, loadModuleFn = origDir, origLoad })
	watchdogModuleDir = dir
	var calls []string
	loaded := map[string]bool{}
	loadModuleFn = func(path string) error {
		name := filepath.Base(path)
		calls = append(calls, name)
		switch {
		case name == "sp5100_tco.ko":
			return errors.New("ENODEV") // no AMD hardware
		case name == "iTCO_wdt.ko" && !loaded["lpc_ich.ko"]:
			return errors.New("unknown symbol") // needs lpc_ich first
		}
		loaded[name] = true
		return nil
	}
	loadWatchdogModules()
	if !loaded["iTCO_wdt.ko"] || !loaded["lpc_ich.ko"] {
		t.Fatalf("loaded %v (calls %v)", loaded, calls)
	}
	// A module that never loads stops the passes once nothing changes.
	if n := len(calls); n > 6 {
		t.Fatalf("%d load calls: %v", n, calls)
	}
}

// A stalled step writes BOOTY-WDT-001 out of band before the reset.
func TestWatchdogStalledWritesOOBRecord(t *testing.T) {
	console := filepath.Join(t.TempDir(), "ttyS0")
	if err := os.WriteFile(console, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oob.Install(oob.NewReporter(&oob.Options{Serial: true, Consoles: []string{console}, Run: "abcd1234"}))
	t.Cleanup(func() { oob.Install(nil) })
	watchdogStalled("partprobe")
	data, err := os.ReadFile(console)
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	if !strings.Contains(line, "code=BOOTY-WDT-001") || !strings.Contains(line, "step=partprobe") {
		t.Fatalf("status line = %q", line)
	}
}

func TestWatchdogStallCodeMatchesCatalog(t *testing.T) {
	if provision.CodeStalled != watchdog.CodeStalled {
		t.Fatalf("provision.CodeStalled %q != watchdog.CodeStalled %q", provision.CodeStalled, watchdog.CodeStalled)
	}
}
