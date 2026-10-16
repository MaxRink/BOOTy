//go:build linux

package disk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Disk mirroring modes accepted from the DISK_MIRRORING_MODE variable. They
// match the CAPRF RedfishHost.Spec.DiskMirroringMode values.
const (
	MirroringModeNone  = "none"
	MirroringModeRAID1 = "raid-1"
	MirroringModeAuto  = "auto"
)

// MirrorRootArray is the md device that mirrors the root partition. The name
// matches the CAPRF legacy provisioner so both installs look the same on disk
// (array name "<homehost>:2").
const MirrorRootArray = "/dev/md2"

// MirrorBootArray is the md device that mirrors a separate /boot partition,
// named like the CAPRF legacy provisioner (array name "<homehost>:1").
const MirrorBootArray = "/dev/md1"

// DetectMirrorPair returns the largest pair of non-removable, non-virtual
// disks with an identical size in bytes and logical sector size on different
// physical devices. NVMe
// namespaces of one controller share a device, so a mirror across them would
// not survive a drive failure. ok is false when no such pair exists.
func (m *Manager) DetectMirrorPair(_ context.Context, minSizeGB int) (first, second string, ok bool, err error) {
	candidates, err := m.mirrorCandidates(minSizeGB)
	if err != nil {
		return "", "", false, err
	}
	// Largest first, then by name, so the pick is deterministic and a host with
	// several matching pairs mirrors onto the biggest one.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].bytes != candidates[j].bytes {
			return candidates[i].bytes > candidates[j].bytes
		}
		return candidates[i].path < candidates[j].path
	})
	for i, a := range candidates {
		for _, b := range candidates[i+1:] {
			if b.bytes != a.bytes {
				break
			}
			// sgdisk --replicate keeps LBA values, so both disks need the
			// same logical sector size.
			if b.device == a.device || b.sectorBytes != a.sectorBytes || identitiesOverlap(a.identities, b.identities) {
				continue
			}
			slog.Info("selected mirror pair", "disk1", a.path, "disk2", b.path, "sizeBytes", a.bytes)
			return a.path, b.path, true, nil
		}
	}
	return "", "", false, nil
}

type mirrorCandidate struct {
	path        string
	device      string
	identities  []string
	bytes       int64
	sectorBytes string
}

// mirrorCandidates lists the non-removable, non-virtual disks of at least
// minSizeGB.
func (m *Manager) mirrorCandidates(minSizeGB int) ([]mirrorCandidate, error) {
	blockDir := m.sysfsRoot + "/block"
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", blockDir, err)
	}
	allowRemovable := os.Getenv("BOOTY_ALLOW_REMOVABLE") == "true"
	var candidates []mirrorCandidate
	for _, entry := range entries {
		name := entry.Name()
		if isVirtualDisk(name) || (!allowRemovable && m.isRemovableMedia(name)) {
			continue
		}
		size, sizeErr := m.readDiskSizeBytes(name)
		if sizeErr != nil || size <= 0 || (minSizeGB > 0 && size < int64(minSizeGB)*1024*1024*1024) {
			continue
		}
		candidates = append(candidates, mirrorCandidate{
			path: "/dev/" + name, device: m.physicalDevice(name), identities: m.diskIdentities(name), bytes: size, sectorBytes: m.logicalBlockSize(name),
		})
	}
	return candidates, nil
}

// logicalBlockSize returns the logical sector size of disk name, or "512"
// when sysfs does not report one.
func (m *Manager) logicalBlockSize(name string) string {
	data, err := os.ReadFile(filepath.Join(m.sysfsRoot, "block", name, "queue", "logical_block_size")) //nolint:gosec // sysfs path
	if size := strings.TrimSpace(string(data)); err == nil && size != "" {
		return size
	}
	return "512"
}

var nvmeNamespaceRE = regexp.MustCompile(`^(nvme\d+)n\d+$`)

func identitiesOverlap(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := seen[id]; ok {
			return true
		}
	}
	return false
}

// diskIdentities returns every stable identifier reported for a disk. A
// multipath device can expose different serial and WWID values through
// different paths; any shared identifier means the paths are one physical LUN.
func (m *Manager) diskIdentities(name string) []string {
	var ids []string
	for _, source := range []struct {
		path string
		vpd  bool
	}{
		{filepath.Join(m.sysfsRoot, "block", name, "device", "serial"), false},
		{filepath.Join(m.sysfsRoot, "block", name, "serial"), false},
		{filepath.Join(m.sysfsRoot, "block", name, "device", "wwid"), false},
		{filepath.Join(m.sysfsRoot, "block", name, "device", "vpd_pg80"), true},
		{filepath.Join(m.sysfsRoot, "block", name, "device", "vpd_pg83"), true},
	} {
		data, err := os.ReadFile(source.path)
		if err != nil {
			continue
		}
		id := printableIdentifier(data)
		if source.vpd {
			id = printableSCSIVPDIdentifier(data)
		}
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// physicalDevice returns an identity of the physical device behind disk name:
// the serial number or WWID, else the NVMe controller for a namespace, else
// the sysfs device. It falls back to name, so such disks never count as
// sharing a device.
func (m *Manager) physicalDevice(name string) string {
	// Multipath gives one drive or LUN several block devices with distinct
	// names and device links but one serial or WWID. NVMe namespaces of one
	// controller also report the controller serial.
	if serial, err := m.readDiskSerial(name); err == nil {
		return "serial:" + serial
	}
	// Without a serial, NVMe namespaces named nvme<controller>n<namespace>
	// of one controller share the prefix whatever the sysfs layout.
	if ctrl := nvmeNamespaceRE.FindStringSubmatch(name); ctrl != nil {
		return "nvme:" + ctrl[1]
	}
	dev, err := filepath.EvalSymlinks(filepath.Join(m.sysfsRoot, "block", name, "device"))
	if err != nil {
		return name
	}
	return dev
}

func (m *Manager) readDiskSizeBytes(name string) (int64, error) {
	data, err := os.ReadFile(m.sysfsRoot + "/block/" + name + "/size") //nolint:gocritic // sysfs path
	if err != nil {
		return 0, fmt.Errorf("read disk size %s: %w", name, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse disk size %s: %w", name, err)
	}
	return sectors * 512, nil
}

// ReplicatePartitionTable copies the GPT of src onto dst and gives dst new
// disk and partition GUIDs so firmware and udev can tell the members apart.
func (m *Manager) ReplicatePartitionTable(ctx context.Context, src, dst string) error {
	if out, err := m.cmd.Run(ctx, "sgdisk", "--replicate="+dst, src); err != nil {
		return fmt.Errorf("sgdisk replicate %s to %s: %s: %w", src, dst, string(out), err)
	}
	if out, err := m.cmd.Run(ctx, "sgdisk", "--randomize-guids", dst); err != nil {
		return fmt.Errorf("sgdisk randomize guids %s: %s: %w", dst, string(out), err)
	}
	return m.PartProbe(ctx, dst)
}

// CreateDegradedMirror creates a two-member RAID1 array with only member
// present. Metadata 1.0 sits at the end of the partition, so the filesystem
// already on member stays readable at offset 0 and the array can be assembled
// around it without copying data.
func (m *Manager) CreateDegradedMirror(ctx context.Context, array, member string) error {
	out, err := m.cmd.Run(ctx, "mdadm", "--create", array,
		"--run", "--force", "--level=1", "--raid-devices=2", "--metadata=1.0", "--assume-clean",
		member, "missing")
	if err != nil {
		return fmt.Errorf("mdadm create %s with %s: %s: %w", array, member, string(out), err)
	}
	slog.Info("created degraded mirror", "array", array, "member", member)
	return nil
}

// MirrorActive reports whether array is already assembled. It reports false
// only when the array is positively absent or inactive, and an error when
// mdadm fails otherwise, so callers never take the destructive create path
// because of a transient failure.
//
// It never assembles arrays. The ramdisk has no mdadm.conf, so mdadm
// --assemble --scan would activate any array it finds: a foreign data array,
// or a stale md1 on the second disk after a quick-wipe reprovision that then
// keeps the fresh /boot out of the mirror. The CAPRF legacy provisioner never
// assembles during provisioning either.
func (m *Manager) MirrorActive(ctx context.Context, array string) (bool, error) {
	out, err := m.cmd.Run(ctx, "mdadm", "--detail", array)
	if err == nil {
		return true, nil
	}
	if strings.Contains(string(out), "does not appear to be active") {
		return false, nil
	}
	if _, statErr := os.Stat(array); errors.Is(statErr, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("mdadm detail %s: %s: %w", array, strings.TrimSpace(string(out)), err)
}

// MirrorHasMember reports whether member is a device of the assembled array.
func (m *Manager) MirrorHasMember(ctx context.Context, array, member string) (bool, error) {
	out, err := m.cmd.Run(ctx, "mdadm", "--detail", array)
	if err != nil {
		return false, fmt.Errorf("mdadm detail %s: %s: %w", array, string(out), err)
	}
	for line := range strings.Lines(string(out)) {
		if fields := strings.Fields(line); len(fields) > 0 && fields[len(fields)-1] == member {
			return true, nil
		}
	}
	return false, nil
}

// MirrorMembers returns the block devices currently listed in an assembled array.
func (m *Manager) MirrorMembers(ctx context.Context, array string) ([]string, error) {
	out, err := m.cmd.Run(ctx, "mdadm", "--detail", array)
	if err != nil {
		return nil, fmt.Errorf("mdadm detail %s: %s: %w", array, string(out), err)
	}
	var members []string
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		device := fields[len(fields)-1]
		// Member rows begin with the numeric RAID-device index. This excludes
		// headers and other detail lines that happen to end in a device path.
		if len(fields) > 1 && strings.HasPrefix(device, "/dev/") {
			if _, indexErr := strconv.Atoi(fields[0]); indexErr != nil {
				continue
			}
			members = append(members, device)
		}
	}
	return members, nil
}

// AddMirrorMember adds member to array; the kernel resyncs it in the background.
func (m *Manager) AddMirrorMember(ctx context.Context, array, member string) error {
	if out, err := m.cmd.Run(ctx, "mdadm", "--add", array, member); err != nil {
		return fmt.Errorf("mdadm add %s to %s: %s: %w", member, array, string(out), err)
	}
	slog.Info("added mirror member", "array", array, "member", member)
	return nil
}

// MirrorScan returns "mdadm --detail --scan" output for array, suitable for
// mdadm.conf.
func (m *Manager) MirrorScan(ctx context.Context, array string) (string, error) {
	// mdadm's --scan form accepts no array operand; filter the returned ARRAY
	// records at the caller.
	out, err := m.cmd.Run(ctx, "mdadm", "--detail", "--scan")
	if err != nil {
		return "", fmt.Errorf("mdadm detail scan %s: %s: %w", array, string(out), err)
	}
	return string(out), nil
}

// CopyBlockDevice copies src onto dst, for partitions that cannot be md
// members, such as the EFI system partition that firmware reads directly.
func CopyBlockDevice(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // block device path from partition scan
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0) //nolint:gosec // block device path from partition scan
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

// mirrorMetadataReserve is the space kept free for mdadm metadata 1.0 at the
// end of a member: a superblock 8 KiB before the end plus at most 128 KiB of
// internal bitmap, rounded up.
const mirrorMetadataReserve = 1 << 20

// ShrinkFilesystemForMirror shrinks the ext2/3/4 filesystem on member so it
// ends mirrorMetadataReserve bytes before the end of the partition, which
// cannot grow when another partition follows it. It does nothing when the
// filesystem already leaves that space.
func (m *Manager) ShrinkFilesystemForMirror(ctx context.Context, member string) error {
	if CheckMirrorTailSpace(member) == nil {
		return nil
	}
	fsType, err := m.fsType(ctx, member)
	if err != nil {
		return fmt.Errorf("filesystem type of %s: %w", member, err)
	}
	if !strings.HasPrefix(fsType, "ext") {
		return fmt.Errorf("cannot shrink the %s filesystem on %s for mirror metadata; only ext2/3/4 can shrink", fsType, member)
	}
	partBytes, err := deviceSize(member)
	if err != nil {
		return err
	}
	if err := m.CheckFilesystem(ctx, member); err != nil {
		return err
	}
	size := fmt.Sprintf("%dK", (partBytes-mirrorMetadataReserve)/1024)
	if out, err := m.cmd.Run(ctx, "resize2fs", member, size); err != nil {
		return fmt.Errorf("resize2fs %s to %s: %s: %w", member, size, string(out), err)
	}
	slog.Info("shrank filesystem for mirror metadata", "device", member, "size", size)
	return nil
}

func deviceSize(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // block device path from partition scan
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", path, err)
	}
	return size, nil
}

// CheckMirrorTailSpace fails unless the filesystem on member ends at least
// mirrorMetadataReserve bytes before the end of the partition. Metadata 1.0
// is written at the end of the member, so without that space it would
// overwrite the filesystem, for example when growpart could not grow the
// partition because the image already fills the disk.
func CheckMirrorTailSpace(member string) error {
	f, err := os.Open(member) //nolint:gosec // block device path from partition scan
	if err != nil {
		return fmt.Errorf("open %s: %w", member, err)
	}
	defer func() { _ = f.Close() }()
	partBytes, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size of %s: %w", member, err)
	}
	fsBytes, err := filesystemSize(f)
	if err != nil {
		return fmt.Errorf("filesystem size on %s: %w", member, err)
	}
	if fsBytes > uint64(max(partBytes-mirrorMetadataReserve, 0)) { //nolint:gosec // clamped to non-negative
		return fmt.Errorf("filesystem on %s (%d bytes) leaves no room for mirror metadata in the %d byte partition; "+
			"the root partition could not grow", member, fsBytes, partBytes)
	}
	return nil
}

// filesystemSize returns the size in bytes that the ext2/3/4, XFS or btrfs
// filesystem on r records in its superblock.
func filesystemSize(r io.ReaderAt) (uint64, error) {
	le, be := binary.LittleEndian, binary.BigEndian

	ext := make([]byte, 1024)
	if _, err := r.ReadAt(ext, 1024); err == nil && le.Uint16(ext[56:]) == 0xEF53 {
		blocks := uint64(le.Uint32(ext[4:]))
		if le.Uint32(ext[96:])&0x80 != 0 { // INCOMPAT_64BIT
			blocks |= uint64(le.Uint32(ext[0x150:])) << 32
		}
		logBlockSize := le.Uint32(ext[24:])
		if logBlockSize > 16 {
			return 0, fmt.Errorf("invalid ext block size exponent %d", logBlockSize)
		}
		return blocks * (1024 << logBlockSize), nil
	}

	xfs := make([]byte, 16)
	if _, err := r.ReadAt(xfs, 0); err == nil && string(xfs[:4]) == "XFSB" {
		return be.Uint64(xfs[8:]) * uint64(be.Uint32(xfs[4:])), nil
	}

	btrfs := make([]byte, 0x78)
	if _, err := r.ReadAt(btrfs, 0x10000); err == nil && string(btrfs[0x40:0x48]) == "_BHRfS_M" {
		return le.Uint64(btrfs[0x70:]), nil
	}
	return 0, fmt.Errorf("no ext, XFS or btrfs superblock found")
}
