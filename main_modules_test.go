//go:build linux

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestParseModinfo(t *testing.T) {
	data := []byte("license=GPL\x00depends=mlxfw,psample,tls\x00softdep=pre: crc32c post: foo\x00alias=pci:v000015B3d00001017sv*sd*bc*sc*i*\x00")
	mi := parseModinfo(data)
	if want := []string{"mlxfw", "psample", "tls", "crc32c"}; !slices.Equal(mi.deps, want) {
		t.Errorf("deps = %v, want %v", mi.deps, want)
	}
	if !mi.device {
		t.Error("a pci: alias must mark a device driver")
	}
	for _, alias := range []string{"alias=fs-xfs", "alias=devname:loop-control", "alias=crypto-crc32c", "alias=rtnl-link-vxlan"} {
		if mi := parseModinfo([]byte(alias + "\x00depends=\x00")); mi.device {
			t.Errorf("%s must not mark a device driver", alias)
		}
	}
}

func TestModuleName(t *testing.T) {
	for file, want := range map[string]string{"dm-mod.ko": "dm_mod", "nls_iso8859-1.ko": "nls_iso8859_1", "raid1.ko.xz": "raid1"} {
		if got := moduleName(file); got != want {
			t.Errorf("moduleName(%q) = %q, want %q", file, got, want)
		}
	}
}

// fakeModules writes empty module files and makes modinfoFn return deps
// and device flags from info.
func fakeModules(t *testing.T, info map[string]modinfo) string {
	t.Helper()
	dir := t.TempDir()
	for file := range info {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := modinfoFn
	t.Cleanup(func() { modinfoFn = orig })
	modinfoFn = func(path string) modinfo { return info[filepath.Base(path)] }
	return dir
}

func TestLoadModuleDirLoadsDependenciesFirstWithoutRetries(t *testing.T) {
	type mi = modinfo
	dir := fakeModules(t, map[string]mi{
		"virtio_net.ko":         {deps: []string{"net_failover"}, device: true},
		"net_failover.ko":       {deps: []string{"failover"}},
		"failover.ko":           {},
		"mlx5_core.ko":          {deps: []string{"mlxfw"}, device: true},
		"mlxfw.ko":              {},
		"e1000e.ko":             {device: true},
		"raid456.ko":            {deps: []string{"raid6_pq", "async_tx"}},
		"raid6_pq.ko":           {},
		"async_tx.ko":           {},
		"libcrc32c.ko":          {deps: []string{"crc32c"}},
		"zz_crc32c_provider.ko": {aliases: []string{"crc32c", "crypto-crc32c"}},
		"cycle_a.ko":            {deps: []string{"cycle_b"}},
		"cycle_b.ko":            {deps: []string{"cycle_a"}},
		"no_hardware.ko":        {device: true},
		"needs_missing.ko":      {deps: []string{"builtin_dep"}},
	})
	origLoad := loadModuleFn
	t.Cleanup(func() { loadModuleFn = origLoad })
	var mu sync.Mutex
	var calls []string
	loaded := map[string]bool{}
	loadModuleFn = func(path string) error {
		name := filepath.Base(path)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, name)
		if name == "no_hardware.ko" {
			return unix.ENODEV
		}
		loaded[name] = true
		return nil
	}

	got, failed := loadModuleDir(dir, func(string) bool { return false })

	if len(failed) != 1 || failed[0].name != "no_hardware.ko" || !errors.Is(failed[0].err, unix.ENODEV) {
		t.Fatalf("failed = %v, want only no_hardware.ko with ENODEV", failed)
	}
	if len(got) != 14 {
		t.Fatalf("loaded %d modules %v, want 14", len(got), got)
	}
	// Every module is loaded once: no retry of a dependency order failure,
	// and no retry of a driver without hardware.
	if len(calls) != 15 {
		t.Fatalf("finit_module calls = %v, want one per module", calls)
	}
	before := func(a, b string) {
		t.Helper()
		if slices.Index(calls, a) > slices.Index(calls, b) {
			t.Errorf("%s loaded after %s: %v", a, b, calls)
		}
	}
	before("failover.ko", "net_failover.ko")
	before("net_failover.ko", "virtio_net.ko")
	before("mlxfw.ko", "mlx5_core.ko")
	before("raid6_pq.ko", "raid456.ko")
	before("async_tx.ko", "raid456.ko")
	before("zz_crc32c_provider.ko", "libcrc32c.ko") // softdep pre: crc32c names an alias
	// Device drivers keep the order of the former retry loop, which names
	// the NICs: e1000e, no_hardware and virtio_net (its dependencies sort
	// before it) in the first pass, mlx5_core (mlxfw sorts after it) in the
	// second.
	var devices []string
	for _, c := range calls {
		if slices.Contains([]string{"e1000e.ko", "mlx5_core.ko", "no_hardware.ko", "virtio_net.ko"}, c) {
			devices = append(devices, c)
		}
	}
	if want := []string{"e1000e.ko", "no_hardware.ko", "virtio_net.ko", "mlx5_core.ko"}; !slices.Equal(devices, want) {
		t.Errorf("device driver order = %v, want %v", devices, want)
	}
}

func TestLoadModulesLogsOneSummaryLine(t *testing.T) {
	type mi = modinfo
	origDir, origLoad := moduleDir, loadModuleFn
	t.Cleanup(func() { moduleDir, loadModuleFn = origDir, origLoad })
	moduleDir = fakeModules(t, map[string]mi{"nvme.ko": {device: true}, "xfs.ko": {}})
	loadModuleFn = func(string) error { return nil }
	origLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(origLogger) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	loadModules()

	if n := strings.Count(logs.String(), "\n"); n != 1 {
		t.Fatalf("want one log line, got %d:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), `msg="loaded kernel modules" count=2 failed=0`) ||
		!strings.Contains(logs.String(), `modules="nvme.ko xfs.ko"`) {
		t.Fatalf("summary line misses the modules:\n%s", logs.String())
	}
}

// A module whose dependency the .modinfo does not name (libcrc32c with a
// softdep that resolves to a module outside the directory, such as
// crc32c_intel) fails its first load. It gets one retry after all other
// modules loaded; a driver without hardware (ENODEV) gets none.
func TestLoadModuleDirRetriesAFailedLoadOnce(t *testing.T) {
	type mi = modinfo
	dir := fakeModules(t, map[string]mi{
		"libcrc32c.ko":   {deps: []string{"crc32c"}},
		"always_fail.ko": {},
		"no_hardware.ko": {device: true},
	})
	origLoad := loadModuleFn
	t.Cleanup(func() { loadModuleFn = origLoad })
	var mu sync.Mutex
	calls := map[string]int{}
	loadModuleFn = func(path string) error {
		name := filepath.Base(path)
		mu.Lock()
		defer mu.Unlock()
		calls[name]++
		switch {
		case name == "libcrc32c.ko" && calls[name] == 1:
			return unix.ENOENT // Unknown symbol
		case name == "always_fail.ko":
			return unix.ENOENT
		case name == "no_hardware.ko":
			return unix.ENODEV
		}
		return nil
	}

	got, failed := loadModuleDir(dir, func(string) bool { return false })

	if !slices.Equal(got, []string{"libcrc32c.ko"}) {
		t.Errorf("loaded = %v, want libcrc32c.ko after its retry", got)
	}
	names := make([]string, 0, len(failed))
	for _, f := range failed {
		names = append(names, f.name)
	}
	if want := []string{"always_fail.ko", "no_hardware.ko"}; !slices.Equal(names, want) {
		t.Errorf("failed = %v, want %v", names, want)
	}
	if want := map[string]int{"libcrc32c.ko": 2, "always_fail.ko": 2, "no_hardware.ko": 1}; !maps.Equal(calls, want) {
		t.Errorf("finit_module calls = %v, want %v", calls, want)
	}
}

// A module that names itself as a dependency must not wait for itself:
// it keeps its place in the order instead of moving to the cycle tail.
func TestLegacyLoadOrderIgnoresSelfDependency(t *testing.T) {
	mods := map[string]*kmod{
		"y":      {file: "y.ko", modinfo: modinfo{deps: []string{"z_self"}}},
		"z_self": {file: "z_self.ko", modinfo: modinfo{deps: []string{"z_self"}}},
	}
	if got, want := legacyLoadOrder([]string{"y", "z_self"}, mods), []string{"z_self", "y"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Device drivers load one after the other. Each fake device load holds
// for 50 ms; without the wait for the previous device driver, the
// semaphore (at least 2) lets a second one start in that time, every run.
func TestLoadModuleDirLoadsDeviceDriversOneAtATime(t *testing.T) {
	type mi = modinfo
	dir := fakeModules(t, map[string]mi{
		"a_nic.ko":  {device: true},
		"b_nic.ko":  {device: true},
		"c_nic.ko":  {device: true},
		"helper.ko": {},
	})
	origLoad := loadModuleFn
	t.Cleanup(func() { loadModuleFn = origLoad })
	var mu sync.Mutex
	inFlight, overlaps, deviceLoads := 0, 0, 0
	loadModuleFn = func(path string) error {
		if !strings.HasSuffix(filepath.Base(path), "_nic.ko") {
			return nil
		}
		mu.Lock()
		deviceLoads++
		inFlight++
		if inFlight > 1 {
			overlaps++
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	loaded, failed := loadModuleDir(dir, func(string) bool { return false })
	if len(failed) != 0 || len(loaded) != 4 {
		t.Fatalf("loaded = %v, failed = %v, want all 4 modules loaded", loaded, failed)
	}
	// The overlap check below is only meaningful if the device loads ran.
	if deviceLoads != 3 {
		t.Fatalf("device driver loads = %d, want 3", deviceLoads)
	}
	if overlaps != 0 {
		t.Fatalf("%d device driver loads overlapped, want one at a time", overlaps)
	}
}
