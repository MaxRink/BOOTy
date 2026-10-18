// Package payload checks the optional ramdisk payloads (FRR, qemu-img,
// ipmitool). A split build ships them as extra cpio archives that the boot
// loader loads after the base initrd, only for the runs that need them
// (docs/proposal-ramdisk-split.md).
//
// Each payload archive carries a manifest Dir/<name> with the line
// "build=<id>". The base archive lists the payloads that its build split
// off in Dir/split. A ramdisk without Dir/split, such as the default and
// gobgp flavors, has every payload it ships built in, and Require never
// fails for it.
package payload

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Payload names.
const (
	FRR      = "frr"
	QemuImg  = "qemu-img"
	IPMITool = "ipmitool"
)

// Dir holds the payload manifests; tests change it.
var Dir = "/etc/booty/payloads"

// splitFile lists the payloads that the base build split off.
const splitFile = "split"

var (
	// ErrMissing marks a run that needs a payload the ramdisk lacks.
	ErrMissing = errors.New("ramdisk payload missing")
	// ErrBuildMismatch marks a payload from another build than BOOTy.
	ErrBuildMismatch = errors.New("ramdisk payload from another build")
)

// Require returns an error that wraps ErrMissing when the build split off
// the payload name and the boot loader did not load its archive.
func Require(name string) error {
	split, err := splitOff()
	if err != nil {
		return err
	}
	if !slices.Contains(split, name) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(Dir, name)); err != nil {
		return fmt.Errorf("%w: %s: add %s.cpio.zst to the initrd list of the boot loader", ErrMissing, name, name)
	}
	return nil
}

// Check verifies that every payload manifest in Dir names build, the
// build of the running BOOTy. An empty build (a development binary) skips
// the check.
func Check(build string) error {
	if build == "" {
		return nil
	}
	entries, err := os.ReadDir(Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading payload manifests: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == splitFile {
			continue
		}
		got, err := manifestBuild(filepath.Join(Dir, e.Name()))
		if err != nil {
			return err
		}
		if got != build {
			return fmt.Errorf("%w: %s is build %q, BOOTy is build %q", ErrBuildMismatch, e.Name(), got, build)
		}
	}
	return nil
}

// splitOff returns the payload names in Dir/split, or none without it.
func splitOff() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(Dir, splitFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the split payload list: %w", err)
	}
	return strings.Fields(string(data)), nil
}

// manifestBuild returns the value of the build= line of a manifest.
func manifestBuild(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // manifest path in the ramdisk
	if err != nil {
		return "", fmt.Errorf("reading payload manifest: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "build="); ok {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading payload manifest %s: %w", filepath.Base(path), err)
	}
	return "", fmt.Errorf("%w: %s has no build= line", ErrBuildMismatch, filepath.Base(path))
}
