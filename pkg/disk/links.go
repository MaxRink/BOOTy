//go:build linux

package disk

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// diskLinkDirs maps blkid export keys to the /dev/disk directories that
// udev's 60-persistent-storage.rules create for them.
// fsLink marks the links that udev creates only for filesystems, swap and
// crypto containers (ID_FS_USAGE filesystem|other|crypto). Partition links
// exist for every partition.
var diskLinkDirs = []struct {
	key, dir string
	fsLink   bool
}{
	{"UUID", "by-uuid", true},
	{"PARTUUID", "by-partuuid", false},
	{"LABEL", "by-label", true},
	{"PARTLABEL", "by-partlabel", false},
}

// isMemberSignature reports a member of a RAID, LVM or storage pool (blkid
// usage "raid"), such as linux_raid_member, LVM2_member, drbd or stratis.
// Its UUID is the array or pool UUID, which every member shares; udev
// makes no by-uuid or by-label link for it. zfs_member is a filesystem for
// blkid, and udev links it.
func isMemberSignature(fsType string) bool {
	switch fsType {
	case "zfs_member":
		return false
	case "drbd", "drbdmanage_control_volume", "stratis":
		return true
	}
	return strings.HasSuffix(fsType, "_member")
}

// CreateDiskLinks creates the /dev/disk/by-uuid, by-partuuid, by-label and
// by-partlabel links below devDir from blkid, like udev does on the
// installed system. The ramdisk has no udev. Without the links grub-mkconfig
// (update-grub) does not find /dev/disk/by-uuid/<root UUID> and writes
// root=/dev/sdXN, which breaks the boot when the disk order changes.
// Existing links in these directories are replaced. When two devices carry
// the same value, the first one in blkid order wins and a warning is
// logged: a disk that was not wiped can hold a copy of the image with the
// same filesystem UUID, and then root=UUID= is ambiguous on the installed
// system too (the legacy scripts had the same limit).
func (m *Manager) CreateDiskLinks(ctx context.Context, devDir string) error {
	// -c /dev/null: probe the devices, never trust a blkid cache file.
	out, err := m.cmd.Run(ctx, "blkid", "-c", "/dev/null", "-o", "export")
	if err != nil {
		// blkid exits 2 without output when no device has a signature.
		// Anything else (blkid missing, killed, canceled) fails: without the
		// links update-grub silently writes root=/dev/sdXN again.
		if exitCodeFromError(err) != 2 || strings.TrimSpace(string(out)) != "" {
			return fmt.Errorf("blkid: %s: %w", strings.TrimSpace(string(out)), err)
		}
		out = nil
	}

	if err := resetDiskLinkDirs(devDir); err != nil {
		return err
	}
	created := 0
	for _, dev := range ParseBlkidExport(string(out)) {
		n, err := linkDevice(devDir, dev)
		if err != nil {
			return err
		}
		created += n
	}
	slog.Info("created /dev/disk links", "count", created)
	return nil
}

// resetDiskLinkDirs empties the link directories below devDir/disk.
func resetDiskLinkDirs(devDir string) error {
	for _, d := range diskLinkDirs {
		dir := filepath.Join(devDir, "disk", d.dir)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// linkDevice creates the links of one blkid device and returns their count.
func linkDevice(devDir string, dev map[string]string) (int, error) {
	name, ok := strings.CutPrefix(dev["DEVNAME"], "/dev/")
	if !ok || name == "" {
		return 0, nil
	}
	created := 0
	for _, d := range diskLinkDirs {
		value := dev[d.key]
		if value == "" || (d.fsLink && isMemberSignature(dev["TYPE"])) {
			continue
		}
		link := filepath.Join(devDir, "disk", d.dir, encodeDevLinkName(value))
		if err := os.Symlink("../../"+name, link); err != nil {
			if os.IsExist(err) {
				slog.Warn("duplicate disk link, keeping the first device", "link", link, "device", dev["DEVNAME"])
				continue
			}
			return created, fmt.Errorf("create %s: %w", link, err)
		}
		created++
	}
	return created, nil
}

// encodeDevLinkName escapes a link name like udev's encode_devnode_name:
// ASCII bytes outside [A-Za-z0-9#+-.:=@_] become \xHH, so a label with a slash
// or a space stays one path element.
func encodeDevLinkName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80 || strings.IndexByte("#+-.:=@_", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, `\x%02x`, c)
	}
	return b.String()
}
