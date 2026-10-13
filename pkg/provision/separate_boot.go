//go:build linux

package provision

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// separateBootMountPoint is where a separate /boot filesystem of the image is
// mounted below the target root.
func separateBootMountPoint() string {
	return filepath.Join(newroot, "boot")
}

// mountSeparateBoot mounts the image /boot filesystem from the target fstab
// when the root filesystem carries no /boot/grub, like provision.sh of the
// CAPRF legacy provisioner. Images such as the Ubuntu 24.04 cloud image keep
// grub.cfg, kernels and initramfs on a separate /boot partition, so
// update-grub and update-initramfs must see it. The device must be a
// partition of the target disk.
func (o *Orchestrator) mountSeparateBoot(ctx context.Context) error {
	if o.cfg.Provision.Disk.PartitionLayout != nil {
		// A declared partition layout owns the mounts and rewrites fstab.
		return nil
	}
	mountpoint := separateBootMountPoint()
	if isMountPoint(mountpoint) {
		return o.adoptSeparateBoot(ctx, mountpoint)
	}
	needed, source, err := separateBootSource(newroot)
	if err != nil || !needed {
		return err
	}
	if source == "" {
		o.log.Warn("no /boot/grub on the root filesystem and no /boot entry in fstab")
		return nil
	}
	device := o.resolveFstabSource(ctx, source)
	if device == "" {
		o.log.Warn("cannot resolve the /boot fstab source on the target disk, keeping /boot on the root filesystem", "source", source)
		return nil
	}
	o.log.Info("mounting separate /boot", "device", device, "mountpoint", mountpoint)
	if err := o.disk.MountPartition(ctx, device, mountpoint); err != nil {
		return fmt.Errorf("mounting separate /boot %s: %w", device, err)
	}
	o.separateBootMounted = true
	return nil
}

// adoptSeparateBoot accepts an existing /boot mount on resume only when it
// comes from the device of the image fstab /boot entry.
func (o *Orchestrator) adoptSeparateBoot(ctx context.Context, mountpoint string) error {
	source, err := targetFstabSource(newroot, "/boot")
	if err != nil {
		return err
	}
	device := o.resolveFstabSource(ctx, source)
	if device == "" {
		return fmt.Errorf("%s is already mounted but the fstab /boot source %q cannot be resolved on the target disk", mountpoint, source)
	}
	if _, err := mountedFromExpectedSource(mountpoint, device, "separate /boot"); err != nil {
		return err
	}
	o.separateBootMounted = true
	return nil
}

// separateBootSource reports whether root lacks /boot/grub and returns the
// fstab source of its /boot entry, if any. It never follows a symlink out of
// root: an image whose /boot, /etc or /etc/fstab is a symlink is treated as
// having no separate /boot.
func separateBootSource(root string) (needed bool, source string, err error) {
	boot := filepath.Join(root, "boot")
	info, err := os.Lstat(boot)
	switch {
	case os.IsNotExist(err):
		// The fstab can still declare /boot; mounting creates the directory.
	case err != nil:
		return false, "", fmt.Errorf("inspect %s: %w", boot, err)
	case !info.IsDir():
		// A symlink or file at /boot is not a mountpoint BOOTy uses.
		return false, "", nil
	default:
		grub, err := os.Lstat(filepath.Join(boot, "grub"))
		switch {
		case err == nil && grub.IsDir():
			// GRUB lives on the root filesystem.
			return false, "", nil
		case err != nil && !os.IsNotExist(err):
			return false, "", fmt.Errorf("inspect %s: %w", filepath.Join(boot, "grub"), err)
		}
	}
	source, err = targetFstabSource(root, "/boot")
	return true, source, err
}

// targetFstabSource reads the fstab source for target from root/etc/fstab
// without following symlinks.
func targetFstabSource(root, target string) (string, error) {
	for _, p := range []string{filepath.Join(root, "etc"), filepath.Join(root, "etc", "fstab")} {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil
		}
	}
	return fstabSource(filepath.Join(root, "etc", "fstab"), target)
}

// fstabSource returns the source field of the first fstab entry for target.
// Mountpoints are compared as cleaned paths, so "/boot/" is "/boot".
func fstabSource(path, target string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // fstab of the provisioned root, checked by the caller
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if filepath.Clean(decodeFstabField(fields[1])) == filepath.Clean(target) {
			return decodeFstabField(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return "", nil
}

// blockIDs holds the identifiers of one partition of the target disk.
type blockIDs struct {
	node, uuid, label, partUUID string
}

// resolveFstabSource maps an fstab source (UUID=, PARTUUID=, LABEL= or a
// device path) to exactly one partition of the target disk. It returns ""
// when no partition or more than one matches, so a label such as BOOT on
// another disk can never be mounted.
func (o *Orchestrator) resolveFstabSource(ctx context.Context, source string) string {
	parts, err := o.disk.ParsePartitions(ctx, o.targetDisk)
	if err != nil {
		o.log.Warn("cannot list target disk partitions", "disk", o.targetDisk, "error", err)
		return ""
	}
	devs := make([]blockIDs, 0, len(parts))
	for _, part := range parts {
		out, err := o.disk.Run(ctx, "blkid", "-o", "export", part.Node)
		if err != nil {
			// blkid fails for partitions without a filesystem signature.
			devs = append(devs, blockIDs{node: part.Node})
			continue
		}
		ids := parseBlkidExport(string(out))
		ids.node = part.Node
		devs = append(devs, ids)
	}
	return matchFstabSource(canonicalFstabSource(source), devs)
}

// canonicalFstabSource maps /dev/disk/by-uuid|by-partuuid|by-label links to
// UUID=, PARTUUID= and LABEL= sources, because the initramfs has no udev to
// create those links, and resolves other device symlinks to their node.
func canonicalFstabSource(source string) string {
	for prefix, key := range map[string]string{
		"/dev/disk/by-uuid/":     "UUID=",
		"/dev/disk/by-partuuid/": "PARTUUID=",
		"/dev/disk/by-label/":    "LABEL=",
	} {
		if id, ok := strings.CutPrefix(source, prefix); ok {
			return key + decodeUdevEscapes(id)
		}
	}
	if strings.HasPrefix(source, "/dev/") {
		if resolved, err := filepath.EvalSymlinks(source); err == nil {
			return resolved
		}
	}
	return source
}

// decodeUdevEscapes decodes the \xNN escapes udev uses in link names, such as
// \x20 for a space.
func decodeUdevEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// decodeFstabField decodes the octal escapes of fstab fields, such as \040
// for a space.
func decodeFstabField(field string) string {
	var b strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) && isOctal(field[i+1:i+4]) {
			n := (int(field[i+1]-'0') << 6) | (int(field[i+2]-'0') << 3) | int(field[i+3]-'0')
			b.WriteByte(byte(n))
			i += 3
			continue
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

func isOctal(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return len(s) == 3
}

// unescapeBlkidValue removes the shell escaping of blkid -o export, which
// writes a label "BOOT VOL" as BOOT\ VOL.
func unescapeBlkidValue(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			i++
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

func parseBlkidExport(out string) blockIDs {
	var ids blockIDs
	for _, line := range strings.Split(out, "\n") {
		// Keep trailing spaces: blkid escapes a label ending in a space as
		// "LABEL=BOOT\\ ".
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		value = unescapeBlkidValue(value)
		switch key {
		case "UUID":
			ids.uuid = value
		case "LABEL":
			ids.label = value
		case "PARTUUID":
			ids.partUUID = value
		}
	}
	return ids
}

func matchFstabSource(source string, devs []blockIDs) string {
	match := func(ids blockIDs) bool { return false }
	switch {
	case strings.HasPrefix(source, "UUID="):
		want := strings.TrimPrefix(source, "UUID=")
		match = func(ids blockIDs) bool { return ids.uuid != "" && strings.EqualFold(ids.uuid, want) }
	case strings.HasPrefix(source, "PARTUUID="):
		want := strings.TrimPrefix(source, "PARTUUID=")
		match = func(ids blockIDs) bool { return ids.partUUID != "" && strings.EqualFold(ids.partUUID, want) }
	case strings.HasPrefix(source, "LABEL="):
		want := strings.TrimPrefix(source, "LABEL=")
		match = func(ids blockIDs) bool { return ids.label != "" && ids.label == want }
	case strings.HasPrefix(source, "/"):
		match = func(ids blockIDs) bool { return ids.node == source }
	}
	found := ""
	for _, ids := range devs {
		if !match(ids) {
			continue
		}
		if found != "" {
			return ""
		}
		found = ids.node
	}
	return found
}

// unmountSeparateBoot unmounts the separate /boot mounted by mountSeparateBoot.
func (o *Orchestrator) unmountSeparateBoot() error {
	mountpoint := separateBootMountPoint()
	if !o.separateBootMounted || !isMountPoint(mountpoint) {
		return nil
	}
	if err := o.disk.Unmount(mountpoint); err != nil {
		return fmt.Errorf("unmount separate /boot %s: %w", mountpoint, err)
	}
	o.separateBootMounted = false
	return nil
}
