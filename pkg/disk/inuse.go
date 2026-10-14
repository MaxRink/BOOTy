//go:build linux

package disk

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// FakeDisks marks a Commander that changes no real disk, such as a test
// double. CheckNotInUse skips the /proc check for it. Every other Commander,
// also a wrapper around ExecCommander, gets the check.
type FakeDisks interface {
	FakeDisks()
}

// CheckNotInUse returns an error when device, or a device on it, backs a
// mounted file system or an active swap area. Every destructive disk
// operation calls it first. A device on top counts through the slaves of
// dm and MD devices (LVM, dm-crypt, RAID), a partition makes its disk busy,
// and a multi-device btrfs or bcachefs makes all its devices busy. ZFS is
// not covered: its mount source is a pool/dataset, not a device. So the running root
// and the disks of a host that runs BOOTy in a container are not wiped.
// When the check can not be made, the wipe is refused.
//
// /proc/self/mountinfo is per mount namespace: a mount that only another
// namespace (another container) sees is not found.
func (m *Manager) CheckNotInUse(device string) error {
	if _, fake := m.cmd.(FakeDisks); fake {
		return nil
	}
	busy, err := m.busyDevices()
	if err != nil {
		return fmt.Errorf("refusing to wipe %s: %w: %w", device, ErrInUseCheck, err)
	}
	if reason, ok := busy[blockName(device)]; ok {
		return &InUseError{Device: device, Reason: reason}
	}
	return nil
}

// ErrInUseCheck marks a failed in-use check. A wipe of all disks fails
// with it; it never skips a disk that it could not check.
var ErrInUseCheck = errors.New("cannot check whether it is in use")

// ErrAllDisksInUse is the error of a wipe of all disks that skipped every
// disk because each one is in use.
var ErrAllDisksInUse = errors.New("no disk wiped: every disk is in use")

// InUseError is the refusal of CheckNotInUse for a disk that is in use. A
// wipe of all disks skips such a disk; a wipe of a selected disk fails.
type InUseError struct {
	Device, Reason string
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("refusing to wipe %s: it backs %s", e.Device, e.Reason)
}

// skipInUse reports whether a wipe of all disks skips dev because it is in
// use. Any other check error is returned: the wipe of all disks then fails.
func (m *Manager) skipInUse(dev string) (bool, error) {
	err := m.CheckNotInUse(dev)
	var inUse *InUseError
	if errors.As(err, &inUse) {
		slog.Warn("disk is in use, skipping it in the wipe of all disks", "device", dev, "reason", inUse.Reason)
		return true, nil
	}
	return false, err
}

// blockName returns the kernel name of a /dev path, following symlinks
// such as /dev/md/name or /dev/mapper/name.
func blockName(device string) string {
	if resolved, err := filepath.EvalSymlinks(device); err == nil {
		device = resolved
	}
	return filepath.Base(device)
}

// busyDevices maps the kernel name of each block device that backs a
// mounted file system or an active swap area to the reason.
func (m *Manager) busyDevices() (map[string]string, error) {
	for _, dir := range []string{"dev/block", "class/block"} {
		if _, err := os.Stat(filepath.Join(m.sysfsRoot, dir)); err != nil {
			return nil, fmt.Errorf("sysfs: %w", err)
		}
	}
	busy := map[string]string{}
	if err := m.markMounts(busy); err != nil {
		return nil, err
	}
	swaps, err := os.ReadFile(filepath.Join(m.procRoot(), "swaps"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read swaps: %w", err)
	}
	for i, line := range strings.Split(string(swaps), "\n") {
		fields := strings.Fields(line)
		if i == 0 || len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/") {
			continue // header, or a swap file on a mounted file system
		}
		if err := m.markBusy(busy, blockName(unescapeMount(fields[0])), "the active swap "+unescapeMount(fields[0])); err != nil {
			return nil, err
		}
	}
	return busy, nil
}

// markMounts marks the devices of every mount in /proc/self/mountinfo,
// by the device number and by the mount source.
func (m *Manager) markMounts(busy map[string]string) error {
	mountinfo, err := os.ReadFile(filepath.Join(m.procRoot(), "self", "mountinfo"))
	if err != nil {
		return fmt.Errorf("read mountinfo: %w", err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(mountinfo))
	for scanner.Scan() {
		// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw
		fields := strings.Fields(scanner.Text())
		sep := slices.Index(fields, "-")
		if len(fields) < 5 || sep < 0 || sep+2 >= len(fields) {
			continue
		}
		reason := "the file system mounted at " + unescapeMount(fields[4])
		fstype, source := fields[sep+1], unescapeMount(fields[sep+2])
		link, err := os.Readlink(filepath.Join(m.sysfsRoot, "dev", "block", fields[2]))
		switch {
		case err == nil:
			if err := m.markBusy(busy, filepath.Base(link), reason); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("resolve device %s: %w", fields[2], err)
		}
		// btrfs and bcachefs report an anonymous device number; the source
		// names the device, for a multi-device bcachefs as /dev/a:/dev/b.
		// ZFS names a pool/dataset, so a ZFS pool disk is not found.
		if strings.HasPrefix(source, "/dev/") {
			for _, dev := range strings.Split(source, ":") {
				if err := m.markBusy(busy, blockName(dev), reason); err != nil {
					return err
				}
			}
		}
		if fstype == "btrfs" {
			if err := m.markBtrfsMembers(busy, blockName(source), reason); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan mountinfo: %w", err)
	}
	return nil
}

// markBtrfsMembers marks every device of the btrfs file system that has
// the device name among its devices (/sys/fs/btrfs/<uuid>/devices).
func (m *Manager) markBtrfsMembers(busy map[string]string, name, reason string) error {
	filesystems, err := os.ReadDir(filepath.Join(m.sysfsRoot, "fs", "btrfs"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read btrfs devices: %w", err)
	}
	for _, fsys := range filesystems {
		devices, err := os.ReadDir(filepath.Join(m.sysfsRoot, "fs", "btrfs", fsys.Name(), "devices"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read btrfs devices of %s: %w", fsys.Name(), err)
		}
		if !slices.ContainsFunc(devices, func(d os.DirEntry) bool { return d.Name() == name }) {
			continue
		}
		for _, d := range devices {
			if err := m.markBusy(busy, d.Name(), reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// markBusy marks name, the devices below it and, for a partition, its disk.
func (m *Manager) markBusy(busy map[string]string, name, reason string) error {
	if _, done := busy[name]; done || name == "" {
		return nil
	}
	busy[name] = reason
	dir := filepath.Join(m.sysfsRoot, "class", "block", name)
	slaves, err := os.ReadDir(filepath.Join(dir, "slaves"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read slaves of %s: %w", name, err)
	}
	for _, s := range slaves {
		if err := m.markBusy(busy, s.Name(), reason); err != nil {
			return err
		}
	}
	_, err = os.Stat(filepath.Join(dir, "partition")) //nolint:gosec // sysfs path from kernel device names
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat partition of %s: %w", name, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve disk of %s: %w", name, err)
	}
	return m.markBusy(busy, filepath.Base(filepath.Dir(resolved)), reason)
}

// procRoot is /proc, or the fixture root of a test.
func (m *Manager) procRoot() string {
	if m.procDir != "" {
		return m.procDir
	}
	return "/proc"
}

// unescapeMount decodes the octal escapes (\040 for a space) of a
// mountinfo or swaps field.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
