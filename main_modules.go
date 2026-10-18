//go:build linux

package main

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Test seams for loadModules.
var (
	efiFirmwareDir = "/sys/firmware/efi"
	moduleDir      = "/modules"
	loadModuleFn   = loadModule
	modinfoFn      = readModinfo
)

// loadModules loads the kernel modules in /modules (server NICs, storage
// controllers, file systems) with finit_module. Errors are non-fatal: a
// module may be built in or have no hardware.
func loadModules() {
	start := time.Now()
	loaded, failed := loadModuleDir(moduleDir, func(name string) bool {
		// efivarfs fails with ENODEV without EFI firmware; skip it on BIOS.
		if strings.HasPrefix(name, "efivarfs.ko") {
			if _, err := os.Stat(efiFirmwareDir); err != nil {
				slog.Info("no EFI firmware, skipping kernel module", "module", name)
				return true
			}
		}
		return false
	})
	for _, f := range failed {
		// Visible at the default level: a missing driver often shows up
		// later as "no disk" or "DHCP failed".
		slog.Warn("kernel module failed to load", "module", f.name, "error", f.err)
	}
	// One line for all modules: on a 115200 baud serial console every
	// line costs about 10 ms of boot time.
	slog.Info("loaded kernel modules", "count", len(loaded), "failed", len(failed),
		"duration", time.Since(start).Round(time.Millisecond), "modules", strings.Join(loaded, " "))
}

// kmod is one module file of a module directory.
type kmod struct {
	file string // file name, for example dm-mod.ko
	modinfo
	done chan struct{}
	err  error
}

// modinfo is what the loader reads from the .modinfo section of a module.
type modinfo struct {
	deps    []string // module names it needs first (depends and softdep pre)
	aliases []string // alias names without a bus prefix (crc32c, fs-xfs)
	// device marks a driver with a device alias. Device drivers probe
	// their hardware during the load, and the probe order names the NICs
	// (eth0, eth1), so they load one after the other in the order of the
	// former retry loop.
	device bool
}

type moduleFailure struct {
	name string
	err  error
}

// loadModuleDir loads every module file in dir that skip does not reject.
// A module waits for the modules it depends on (the depends= and softdep
// pre: fields of its .modinfo section), so no load fails for a missing
// symbol and is retried. Modules without such an order load in parallel;
// device drivers keep their former relative order. It returns the loaded
// file names in that order and the failures.
func loadModuleDir(dir string, skip func(name string) bool) ([]string, []moduleFailure) {
	mods, names := readModuleDir(dir, skip)
	order := legacyLoadOrder(names, mods)
	loadInOrder(dir, order, mods)

	var loaded []string
	var failed []moduleFailure
	for _, name := range order {
		m := mods[name]
		// A dependency the .modinfo did not name may have loaded since:
		// try a failed module once more, unless it has no hardware.
		if m.err != nil && !errors.Is(m.err, unix.ENODEV) {
			m.err = loadModuleFn(filepath.Join(dir, m.file))
		}
		if m.err != nil {
			failed = append(failed, moduleFailure{name: m.file, err: m.err})
			continue
		}
		loaded = append(loaded, m.file)
	}
	return loaded, failed
}

// readModuleDir reads the .modinfo of every module file in dir that skip
// does not reject, keyed by module name.
func readModuleDir(dir string, skip func(name string) bool) (mods map[string]*kmod, names []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Debug("no kernel modules directory, skipping", "path", dir)
		return nil, nil
	}
	mods = map[string]*kmod{}
	for _, entry := range entries {
		if entry.IsDir() || skip(entry.Name()) {
			continue
		}
		m := &kmod{file: entry.Name(), done: make(chan struct{})}
		m.modinfo = modinfoFn(filepath.Join(dir, entry.Name()))
		name := moduleName(entry.Name())
		mods[name] = m
		names = append(names, name)
	}
	resolveAliases(mods)
	return mods, names
}

// loadInOrder loads the modules, at most one per CPU at a time. A module
// starts when the modules it depends on and, for a device driver, the
// device driver before it in order are done. It sets the err of each.
func loadInOrder(dir string, order []string, mods map[string]*kmod) {
	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	sem := make(chan struct{}, max(runtime.NumCPU(), 2))
	var wg sync.WaitGroup
	var prevDevice *kmod
	for i, name := range order {
		m := mods[name]
		var waitFor []*kmod
		for _, d := range m.deps {
			// Only modules earlier in the order: a softdep cycle must not
			// leave two modules waiting for each other.
			if j, ok := pos[d]; ok && j < i {
				waitFor = append(waitFor, mods[d])
			}
		}
		if m.device {
			if prevDevice != nil {
				waitFor = append(waitFor, prevDevice)
			}
			prevDevice = m
		}
		wg.Go(func() {
			defer close(m.done)
			for _, w := range waitFor {
				<-w.done
			}
			sem <- struct{}{}
			m.err = loadModuleFn(filepath.Join(dir, m.file))
			<-sem
		})
	}
	wg.Wait()
}

// moduleName is the kernel name of a module file: dm-mod.ko is dm_mod.
func moduleName(file string) string {
	name, _, _ := strings.Cut(file, ".ko")
	return strings.ReplaceAll(name, "-", "_")
}

// resolveAliases replaces a dependency that names an alias (the softdep
// "pre: crc32c" of libcrc32c) with the module in the directory that has
// this alias (crc32c_generic).
func resolveAliases(mods map[string]*kmod) {
	byAlias := map[string]string{}
	for name, m := range mods {
		for _, a := range m.aliases {
			byAlias[a] = name
		}
	}
	for _, m := range mods {
		for i, d := range m.deps {
			if _, ok := mods[d]; !ok && byAlias[d] != "" {
				m.deps[i] = byAlias[d]
			}
		}
	}
}

// readModinfo reads the .modinfo section of a module file. An unreadable
// file gives an empty modinfo; its load reports the error.
func readModinfo(path string) modinfo {
	f, err := elf.Open(path)
	if err != nil {
		return modinfo{}
	}
	defer func() { _ = f.Close() }()
	sec := f.Section(".modinfo")
	if sec == nil {
		return modinfo{}
	}
	data, err := sec.Data()
	if err != nil {
		return modinfo{}
	}
	return parseModinfo(data)
}

// parseModinfo reads the NUL-separated key=value fields of .modinfo.
func parseModinfo(data []byte) modinfo {
	var mi modinfo
	for field := range bytes.SplitSeq(data, []byte{0}) {
		key, value, ok := strings.Cut(string(field), "=")
		if !ok {
			continue
		}
		switch key {
		case "depends":
			for d := range strings.SplitSeq(value, ",") {
				if d != "" {
					mi.deps = append(mi.deps, moduleName(d))
				}
			}
		case "softdep":
			mi.deps = append(mi.deps, softdepPre(value)...)
		case "alias":
			// Device aliases have a bus prefix (pci:, virtio:, acpi*:);
			// devname: is a /dev node, fs-, crypto- and net- aliases have none.
			prefix, _, ok := strings.Cut(value, ":")
			switch {
			case !ok:
				mi.aliases = append(mi.aliases, value)
			case prefix != "devname":
				mi.device = true
			}
		}
	}
	return mi
}

// softdepPre returns the modules of the "pre:" part of a softdep value,
// for example "pre: crc32c post: x" gives crc32c.
func softdepPre(value string) []string {
	var deps []string
	pre := false
	for word := range strings.FieldsSeq(value) {
		switch {
		case strings.HasSuffix(word, ":"):
			pre = word == "pre:"
		case pre:
			deps = append(deps, moduleName(word))
		}
	}
	return deps
}

// legacyLoadOrder is the order in which the former loader loaded the
// modules: passes over the sorted names, where a module loads once all
// its dependencies in the directory loaded before it. Modules that never
// get their dependencies (a cycle) come last, sorted.
func legacyLoadOrder(names []string, mods map[string]*kmod) []string {
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(mods[a].file, mods[b].file) })
	loaded := map[string]bool{}
	var order []string
	pending := names
	for len(pending) > 0 {
		var next []string
		for _, name := range pending {
			ready := true
			for _, d := range mods[name].deps {
				if _, ok := mods[d]; ok && d != name && !loaded[d] {
					ready = false
					break
				}
			}
			if !ready {
				next = append(next, name)
				continue
			}
			loaded[name] = true
			order = append(order, name)
		}
		if len(next) == len(pending) {
			return append(order, next...)
		}
		pending = next
	}
	return order
}

// loadModule loads a single kernel module via the finit_module syscall.
// Returns nil when the module is already loaded or built-in (EEXIST).
func loadModule(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open module %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("finit_module %s: %w", filepath.Base(path), err)
	}
	return nil
}
