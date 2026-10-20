//go:build linux

package provision

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/telekom/BOOTy/pkg/blockdev"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

// Root disk mirroring follows the CAPRF legacy provisioner: the image is
// written to the first disk, its root partition becomes the only member of a
// degraded RAID1 array with end-of-device metadata, the partition table and
// EFI system partition are copied to the second disk, and the second root
// partition joins the array and resyncs in the background.

var (
	copyBlockDeviceFn           = disk.CopyBlockDevice
	checkMirrorTailSpaceFn      = disk.CheckMirrorTailSpace
	shrinkFilesystemForMirrorFn = (*disk.Manager).ShrinkFilesystemForMirror
	unmountInspectionPart       = (*disk.Manager).Unmount
)

func (o *Orchestrator) mirroringMode() string {
	return strings.ToLower(strings.TrimSpace(o.cfg.Provision.Disk.MirroringMode))
}

// validateMirroringMode rejects mirroring setups that the mirror-root step
// cannot handle. It runs in validate-provision-inputs, before setup-raid and
// detect-disk change the disks or the disk selection.
func (o *Orchestrator) validateMirroringMode() error {
	mode := o.mirroringMode()
	switch mode {
	case "", disk.MirroringModeNone:
		return nil
	case disk.MirroringModeRAID1, disk.MirroringModeAuto:
	default:
		return fmt.Errorf("unsupported disk mirroring mode %q", mode)
	}
	// detect-disk ignores the mirroring mode when DISK_DEVICE or
	// DISK_SERIAL_NUMBER selects the disk, so no combination is rejected then.
	if strings.TrimSpace(o.cfg.Provision.Disk.Device) != "" || strings.TrimSpace(o.cfg.Provision.Disk.SerialNumber) != "" {
		return nil
	}
	imageMode := strings.ToLower(strings.TrimSpace(o.cfg.Provision.Image.Mode))
	if len(o.cfg.Provision.Disk.RAID) > 0 || o.cfg.Provision.Disk.PartitionLayout != nil ||
		strings.TrimSpace(o.cfg.Provision.Disk.NVMeNamespaces) != "" ||
		(imageMode != "" && imageMode != config.ImageModeWholeDisk) {
		return fmt.Errorf("disk mirroring mode %q requires a whole-disk image and cannot be combined with explicit RAID, NVMe namespace setup or a partition layout", mode)
	}
	return nil
}

// selectMirrorPair sets targetDisk and mirrorDisk when mirroring is requested
// and returns true when it chose the target disk. It returns false to fall
// back to single-disk detection.
func (o *Orchestrator) selectMirrorPair(ctx context.Context) (bool, error) {
	o.mirrorDisk = ""
	mode := o.mirroringMode()
	switch mode {
	case "", disk.MirroringModeNone:
		return false, nil
	case disk.MirroringModeRAID1, disk.MirroringModeAuto:
	default:
		return false, fmt.Errorf("unsupported disk mirroring mode %q", mode)
	}
	if o.resumeMirrorPair[0] != "" {
		return o.useResumeMirrorPair()
	}
	first, second, ok, err := o.disk.DetectMirrorPair(ctx, o.cfg.Provision.Disk.MinSizeGB)
	if err != nil {
		return false, err
	}
	if !ok {
		if mode == disk.MirroringModeRAID1 {
			return false, fmt.Errorf("disk mirroring mode raid-1 requested but no pair of equally sized disks was found")
		}
		o.log.Info("no matching disk pair, provisioning a single disk", "mirroringMode", mode)
		return false, nil
	}
	o.targetDisk = first
	o.mirrorDisk = second
	o.log.Info("mirroring root disk", "mirroringMode", mode, "disk1", first, "disk2", second)
	return true, nil
}

// useResumeMirrorPair keeps the disk selection made before a resume: the
// mirror pair, or the single disk that auto mode fell back to. Once disks are
// written, a new selection could mount a raw array member or an unwritten
// disk, so a missing disk is an error.
func (o *Orchestrator) useResumeMirrorPair() (bool, error) {
	for _, dev := range o.resumeMirrorPair {
		if dev == "" {
			continue
		}
		if _, err := statPath(dev); err != nil {
			return false, fmt.Errorf("disk %s selected before resume is unavailable: %w", dev, err)
		}
	}
	o.targetDisk, o.mirrorDisk = o.resumeMirrorPair[0], o.resumeMirrorPair[1]
	o.log.Info("using the disk selection made before resume", "disk1", o.targetDisk, "disk2", o.mirrorDisk)
	return true, nil
}

// mirrorRoot turns the root partition of the streamed image, and a separate
// /boot partition if the image has one, into RAID1 arrays across both disks.
// It is idempotent so it can rerun on resume.
func (o *Orchestrator) mirrorRoot(ctx context.Context) error {
	if o.mirrorDisk == "" {
		return nil
	}
	rootNum, err := disk.PartitionNumberChecked(o.rootPartition, o.targetDisk)
	if err != nil {
		return fmt.Errorf("determine root partition number: %w", err)
	}
	if err := o.requireGPTLayout(); err != nil {
		return err
	}
	bootData, err := o.separateBootPartition(ctx)
	if err != nil {
		return err
	}

	o.mirrorArrays = []string{disk.MirrorRootArray}
	rootPrepare := func() error {
		// Grow before creating the array: metadata 1.0 lives at the end of
		// the member, past the end of the filesystem the image carried.
		if err := o.disk.GrowPartition(ctx, o.targetDisk, rootNum); err != nil {
			return err
		}
		if err := checkMirrorTailSpaceFn(o.rootPartition); err != nil {
			return err
		}
		return o.disk.ReplicatePartitionTable(ctx, o.targetDisk, o.mirrorDisk)
	}
	if err := o.mirrorPartition(ctx, disk.MirrorRootArray, o.rootPartition, rootPrepare); err != nil {
		return err
	}
	o.rootPartition = disk.MirrorRootArray
	if bootData == "" {
		return nil
	}

	// A separate /boot sits before other partitions and cannot grow, so its
	// filesystem shrinks to make room for the metadata.
	o.mirrorArrays = append(o.mirrorArrays, disk.MirrorBootArray)
	bootPrepare := func() error {
		if err := shrinkFilesystemForMirrorFn(o.disk, ctx, bootData); err != nil {
			return err
		}
		return checkMirrorTailSpaceFn(bootData)
	}
	return o.mirrorPartition(ctx, disk.MirrorBootArray, bootData, bootPrepare)
}

// mirrorPartition makes member the first device of array and adds the
// partition with the same number on the mirror disk. prepare runs before the
// array is created. An already assembled array only gets a missing second
// member, so a resume after a failed mdadm --add completes the mirror.
func (o *Orchestrator) mirrorPartition(ctx context.Context, array, member string, prepare func() error) error {
	num, err := disk.PartitionNumberChecked(member, o.targetDisk)
	if err != nil {
		return fmt.Errorf("determine partition number of %s: %w", member, err)
	}
	mirrorMember := blockdev.PartitionDevicePath(o.mirrorDisk, num)
	active, err := o.disk.MirrorActive(ctx, array)
	if err != nil {
		return err
	}
	if active {
		o.log.Info("mirror already assembled", "array", array)
		present, err := o.disk.MirrorHasMember(ctx, array, mirrorMember)
		if err != nil || present {
			return err
		}
		return o.disk.AddMirrorMember(ctx, array, mirrorMember)
	}
	if err := prepare(); err != nil {
		return err
	}
	// The replicated partition can still carry an md superblock or
	// filesystem signature from an earlier install of the second disk.
	if err := o.disk.WipeFilesystemSignatures(ctx, mirrorMember); err != nil {
		return err
	}
	if err := o.disk.CreateDegradedMirror(ctx, array, member); err != nil {
		return err
	}
	return o.disk.AddMirrorMember(ctx, array, mirrorMember)
}

// requireGPTLayout rejects MBR images: the second disk gets its layout from
// sgdisk --replicate, which handles GPT only. MBR partition types are short
// hexadecimal codes instead of GUIDs.
func (o *Orchestrator) requireGPTLayout() error {
	for _, p := range o.targetPartitions {
		if len(p.Type) != len(disk.LinuxFilesystemGUID) {
			return fmt.Errorf("disk mirroring needs a GPT image, but partition %s has the MBR type %q", p.Node, p.Type)
		}
	}
	return nil
}

// GPT partition types that mirroring handles besides root and the ESP.
const (
	// biosBootPartitionGUID is the BIOS boot partition that GRUB embeds its
	// core image into.
	biosBootPartitionGUID = "21686148-6449-6E6F-744E-656564454649"
	// xbootldrPartitionGUID is the extended boot partition that images such
	// as the Ubuntu 24.04 cloud image use for a separate /boot.
	xbootldrPartitionGUID = "BC13C2FF-59E6-4262-A352-B275FD6F7172"
)

// separateBootPartition returns the image partition, besides root, ESP and
// BIOS boot, that mirroring also covers: the separate /boot that the legacy
// provisioner mirrors as md1. An extended boot partition is /boot by type; a
// Linux filesystem partition must be the /boot entry of the image fstab. Any
// other partition is rejected because its data would exist on the first disk
// only.
func (o *Orchestrator) separateBootPartition(ctx context.Context) (string, error) {
	var boot disk.Partition
	for _, p := range o.targetPartitions {
		switch {
		case p.Node == o.rootPartition || p.Node == o.bootPartition || strings.EqualFold(p.Type, biosBootPartitionGUID):
			continue
		case boot.Node == "" && (strings.EqualFold(p.Type, disk.LinuxFilesystemGUID) || strings.EqualFold(p.Type, xbootldrPartitionGUID)):
			boot = p
		default:
			return "", fmt.Errorf("disk mirroring covers the root partition, one separate /boot partition, the ESP and "+
				"the BIOS boot partition, but the image also has partition %s (type %s)", p.Node, p.Type)
		}
	}
	// An assembled /boot array means the partition was identified before
	// a resume; its member no longer shows the filesystem identifiers.
	if boot.Node == "" || strings.EqualFold(boot.Type, xbootldrPartitionGUID) {
		return boot.Node, nil
	}
	if active, err := o.disk.MirrorActive(ctx, disk.MirrorBootArray); err != nil || active {
		return boot.Node, err
	}
	if err := o.verifyFstabBoot(ctx, boot.Node); err != nil {
		return "", err
	}
	return boot.Node, nil
}

// verifyFstabBoot checks that the image fstab mounts partition at /boot.
func (o *Orchestrator) verifyFstabBoot(ctx context.Context, partition string) error {
	rootDev := o.rootPartition
	active, err := o.disk.MirrorActive(ctx, disk.MirrorRootArray)
	if err != nil {
		return err
	}
	if active {
		rootDev = disk.MirrorRootArray
	}
	dir, err := os.MkdirTemp("", "booty-mirror-root-")
	if err != nil {
		return fmt.Errorf("create root inspection directory: %w", err)
	}
	defer func() { _ = os.Remove(dir) }()
	if err := mountReadOnlyPart(ctx, o.disk, rootDev, dir); err != nil {
		return fmt.Errorf("mount %s to read its fstab: %w", rootDev, err)
	}
	source, readErr := fstabEntrySource(dir, "/boot")
	// Mirroring must not start while the root filesystem is still mounted.
	if err := unmountInspectionPart(o.disk, dir); err != nil {
		return fmt.Errorf("unmount root inspection mount %s: %w", dir, err)
	}
	if readErr != nil {
		return readErr
	}
	out, err := o.disk.Run(ctx, "blkid", "-o", "export", partition)
	if err != nil {
		return fmt.Errorf("read identifiers of %s: %w", partition, err)
	}
	if source == partition {
		return nil
	}
	for line := range strings.Lines(string(out)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || value == "" || !slices.Contains(fstabIdentifierKeys, key) {
			continue
		}
		if source == key+"="+value || source == "/dev/disk/by-"+strings.ToLower(key)+"/"+value {
			return nil
		}
	}
	return fmt.Errorf("partition %s is not the /boot of the image fstab (/boot source %q); "+
		"disk mirroring covers only root and a separate /boot", partition, source)
}

// fstabIdentifierKeys are the blkid keys that fstab sources can name.
var fstabIdentifierKeys = []string{"UUID", "LABEL", "PARTUUID", "PARTLABEL"}

// fstabEntrySource returns the source of the entry for mountpoint in the
// fstab of root, or "" when there is none.
func fstabEntrySource(root, mountpoint string) (string, error) {
	data, err := readTargetFstab(root)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") && fields[1] == mountpoint {
			return fields[0], nil
		}
	}
	return "", nil
}

// readTargetFstab reads etc/fstab below root without following symlinks out
// of root; the file comes from the streamed image. A missing fstab is empty.
func readTargetFstab(root string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open target root %s: %w", root, err)
	}
	defer func() { _ = r.Close() }()
	data, err := r.ReadFile(filepath.Join("etc", "fstab"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read fstab below %s: %w", root, err)
	}
	return data, nil
}

// rewriteMirrorFstab points fstab entries that name a partition of one disk
// at sources that work from either disk: / and /boot at their md arrays, and
// a /boot/efi that names a partition at the ESP filesystem UUID, which the
// copy shares. The second disk gets new partition GUIDs, a partition label
// matches both disks, and a member partition must never be mounted directly.
func (o *Orchestrator) rewriteMirrorFstab(ctx context.Context, root string) error {
	path := filepath.Join(root, "etc", "fstab")
	// A missing fstab counts as empty: the /boot array still needs an entry.
	data, err := readTargetFstab(root)
	if err != nil {
		return err
	}
	arrays := map[string]string{"/": disk.MirrorRootArray}
	if slices.Contains(o.mirrorArrays, disk.MirrorBootArray) {
		arrays["/boot"] = disk.MirrorBootArray
	}
	lines := strings.SplitAfter(string(data), "\n")
	changed, hasBoot := false, false
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		hasBoot = hasBoot || fields[1] == "/boot"
		source := fields[0]
		replacement, err := o.mirrorFstabSource(ctx, arrays[fields[1]], source, fields[1])
		if err != nil {
			return err
		}
		if replacement != "" {
			o.log.Info("fstab entry now valid from both mirror disks", "mountpoint", fields[1], "from", source, "to", replacement)
			lines[i] = strings.Replace(line, source, replacement, 1)
			changed = true
		}
	}
	if arrays["/boot"] != "" && !hasBoot {
		// An extended boot partition can lack an fstab entry; then
		// systemd-gpt-auto-generator would mount a raw array member.
		if lines, err = o.appendMirrorBootFstabEntry(ctx, lines); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := writeTargetRootFile(root, path, []byte(strings.Join(lines, ""))); err != nil {
		return fmt.Errorf("write fstab: %w", err)
	}
	return nil
}

// appendMirrorBootFstabEntry appends an fstab line that mounts the /boot
// array.
func (o *Orchestrator) appendMirrorBootFstabEntry(ctx context.Context, lines []string) ([]string, error) {
	fsType, err := o.disk.FilesystemType(ctx, disk.MirrorBootArray)
	if err != nil {
		return nil, fmt.Errorf("filesystem type of %s for its fstab entry: %w", disk.MirrorBootArray, err)
	}
	if fsType == "" {
		return nil, fmt.Errorf("%s has no filesystem type for its fstab entry", disk.MirrorBootArray)
	}
	o.log.Info("adding fstab entry for the /boot array", "array", disk.MirrorBootArray)
	if last := len(lines) - 1; last >= 0 && lines[last] != "" && !strings.HasSuffix(lines[last], "\n") {
		lines = append(lines, "\n")
	}
	return append(lines, fmt.Sprintf("%s /boot %s defaults 0 2\n", disk.MirrorBootArray, fsType)), nil
}

// mirrorFstabSource returns the replacement for an fstab source that names a
// partition of one disk, or "" to keep it.
func (o *Orchestrator) mirrorFstabSource(ctx context.Context, array, source, mountpoint string) (string, error) {
	switch {
	case array != "" && source != array && namesPartition(source):
		return array, nil
	case mountpoint == "/boot/efi" && namesPartition(source) && o.bootPartition != "":
		// Caveat: both ESP copies share this FAT UUID, like legacy, so
		// either disk mounts /boot/efi alone, but later bootloader updates
		// reach only the mounted copy. Keep both in sync (for example
		// grub-multi-install) if stale second-disk ESPs become a problem.
		out, err := o.disk.Run(ctx, "blkid", "-s", "UUID", "-o", "value", o.bootPartition)
		if err != nil {
			return "", fmt.Errorf("read the ESP filesystem UUID of %s for fstab: %w", o.bootPartition, err)
		}
		uuid := strings.TrimSpace(string(out))
		if uuid == "" {
			return "", fmt.Errorf("the ESP %s has no filesystem UUID for its fstab entry", o.bootPartition)
		}
		return "UUID=" + uuid, nil
	}
	return "", nil
}

// namesPartition reports whether an fstab source names a partition rather
// than a filesystem.
func namesPartition(source string) bool {
	return strings.HasPrefix(source, "PARTUUID=") || strings.HasPrefix(source, "PARTLABEL=") || strings.HasPrefix(source, "/dev/")
}

// mirrorInitramfsCmd rebuilds the target initramfs with update-initramfs, or
// dracut where that is missing. Without mdadm in the target, update-initramfs
// still succeeds but the initramfs cannot assemble the root array.
const mirrorInitramfsCmd = "if ! command -v mdadm >/dev/null 2>&1; then " +
	"echo 'mdadm is not installed in the target image' >&2; exit 1; fi; " +
	"if command -v update-initramfs >/dev/null 2>&1; then update-initramfs -u -k all; " +
	"else dracut --force --regenerate-all; fi"

// mdadmConfHeader matches raid_save_mdadm_config of the CAPRF legacy
// provisioner (common.sh), including its AUTO line: the installed system
// auto-assembles IMSM and metadata 1.x arrays and no others. The ARRAY lines
// of the mirror arrays follow it.
const mdadmConfHeader = "# mdadm.conf - written by BOOTy\n\nDEVICE partitions\nHOMEHOST <system>\nMAILADDR root\n\n" +
	"AUTO +imsm +1.x -all\n\n"

// mdadmArrayLines returns the ARRAY line of array from mdadm --detail --scan
// output, with the device named array. mdadm reports /dev/md/<name>; the
// legacy provisioner records /dev/mdN. Assembly matches arrays by UUID. Only
// the line that belongs to array is kept, so the configuration never lists
// another array under this name.
func mdadmArrayLines(array, scan string) []string {
	num := strings.TrimPrefix(array, "/dev/md")
	var lines []string
	for line := range strings.Lines(scan) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "ARRAY" || !mdadmLineIsArray(fields, array, num) {
			continue
		}
		fields[1] = array
		lines = append(lines, strings.Join(fields, " "))
	}
	return lines
}

// mdadmLineIsArray reports whether the fields of an ARRAY line describe the
// md device array with number num, by device name or by the array name
// "<homehost>:<num>" that mdadm --create gives it.
func mdadmLineIsArray(fields []string, array, num string) bool {
	if fields[1] == array || fields[1] == "/dev/md/"+num {
		return true
	}
	for _, f := range fields[2:] {
		if name, ok := strings.CutPrefix(f, "name="); ok && (name == num || strings.HasSuffix(name, ":"+num)) {
			return true
		}
	}
	return false
}

// writeMdadmConf writes /etc/mdadm/mdadm.conf, which Debian-based images
// read, and also /etc/mdadm.conf when the image ships one, because mdadm and
// dracut use that file when it exists.
func writeMdadmConf(root, content string) error {
	// dracut reads /etc/mdadm.conf, while Debian's initramfs tooling reads
	// /etc/mdadm/mdadm.conf. Write both so a target that did not ship the
	// dracut path still assembles the mirror during early boot.
	paths := []string{filepath.Join(root, "etc", "mdadm", "mdadm.conf"), filepath.Join(root, "etc", "mdadm.conf")}
	for _, path := range paths {
		if err := writeTargetRootFile(root, path, []byte(content)); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// dracutMirrorConf is the dracut configuration that assembles the mirror
// arrays in the initramfs. Without rd.md.uuid or rd.auto on the command line,
// dracut (Ubuntu 26.04) skips MD RAID activation, and a generic (not
// host-only) initramfs carries no rd.md.uuid, so the root array never
// appears. dracut writes kernel_cmdline into the initramfs. initramfs-tools
// ignores the file.
func dracutMirrorConf(arrayLines []string) string {
	var b strings.Builder
	b.WriteString("# Written by BOOTy: assemble the mirror arrays in the initramfs.\n" +
		"add_dracutmodules+=\" mdraid \"\nmdadmconf=\"yes\"\nkernel_cmdline+=\"")
	for _, line := range arrayLines {
		for _, f := range strings.Fields(line) {
			if uuid, ok := strings.CutPrefix(f, "UUID="); ok {
				b.WriteString(" rd.md.uuid=" + uuid)
			}
		}
	}
	b.WriteString(" \"\n")
	return b.String()
}

const mirrorGrubConfig = "# Written by BOOTy: the root filesystem is a RAID1 array. Booting by the\n" +
	"# PARTUUID of one member would bypass the array.\nGRUB_FORCE_PARTUUID=\n"

// configureMdadm records the root array in the installed system and rebuilds
// its initramfs so the array is assembled before the root filesystem mounts.
func (o *Orchestrator) configureMdadm(ctx context.Context) error {
	return o.configureMdadmAt(ctx, newroot)
}

// configureMdadmAt is configureMdadm for the target root mounted at root.
func (o *Orchestrator) configureMdadmAt(ctx context.Context, root string) error {
	if o.mirrorDisk == "" {
		return nil
	}
	var arrayLines []string
	for _, array := range o.mirrorArrays {
		scan, err := o.disk.MirrorScan(ctx, array)
		if err != nil {
			return err
		}
		arrayLines = append(arrayLines, mdadmArrayLines(array, scan)...)
	}
	if err := writeMdadmConf(root, mdadmConfHeader+strings.Join(arrayLines, "\n")+"\n"); err != nil {
		return err
	}
	if err := writeTargetRootFile(root, filepath.Join(root, "etc", "dracut.conf.d", "90-booty-mirror.conf"),
		[]byte(dracutMirrorConf(arrayLines))); err != nil {
		return fmt.Errorf("write dracut mirror config: %w", err)
	}
	if err := o.rewriteMirrorFstab(ctx, root); err != nil {
		return err
	}
	// Ubuntu cloud images set GRUB_FORCE_PARTUUID, which boots the kernel
	// without initramfs from one member partition and bypasses the array.
	// configure-grub runs update-grub after this step, so clear it here.
	if err := writeTargetRootFile(root, filepath.Join(root, "etc", "default", "grub.d", "99-booty-mirror.cfg"),
		[]byte(mirrorGrubConfig)); err != nil {
		return fmt.Errorf("write grub mirror config: %w", err)
	}
	if out, err := o.disk.ChrootRun(ctx, root, mirrorInitramfsCmd); err != nil {
		return fmt.Errorf("rebuild initramfs for root mirror: %s: %w", string(out), err)
	}
	return nil
}

// createMirrorEFIBootEntry makes the second disk bootable on its own, so the
// host still boots when the first disk fails. On UEFI it adds a firmware boot
// entry for the second disk; on BIOS it installs GRUB into both disks.
// Both copy the ESP, if the image has one, after the last step that writes
// to it.
func (o *Orchestrator) createMirrorEFIBootEntry(ctx context.Context) error {
	if o.mirrorDisk == "" {
		return nil
	}
	// Decide by the firmware, not by efivarfs: a UEFI host without EFI
	// variable access still boots from the ESP, and CreateEFIBootEntry only
	// skips the NVRAM entry then.
	if !bootedUEFI() {
		o.log.Info("host booted with BIOS, installing GRUB on both mirror disks", "disk1", o.targetDisk, "disk2", o.mirrorDisk)
		if err := o.installMirrorBIOSBootloader(ctx); err != nil {
			return err
		}
		if o.bootPartition == "" {
			return nil
		}
		mirrorEFI, err := o.mirrorESP()
		if err != nil {
			return err
		}
		return o.syncMirrorESP(ctx, mirrorEFI)
	}
	if o.bootPartition == "" {
		return fmt.Errorf("the image has no EFI system partition, so the mirror disk %s cannot boot on UEFI", o.mirrorDisk)
	}
	mirrorEFI, err := o.mirrorESP()
	if err != nil {
		return err
	}
	ready, reason := efiRuntimeReady()
	if !ready {
		// Without NVRAM access the second disk boots only through the
		// removable fallback loader that the copied ESP must carry.
		if err := requireRemovableEFILoader(filepath.Join(o.config.rootDir, "boot", "efi")); err != nil {
			return fmt.Errorf("no EFI boot entry for mirror disk %s (%s): %w", o.mirrorDisk, reason, err)
		}
	}
	// Copy first: a retry after a failed copy then starts over with the
	// copy, and the entry is only created for a complete ESP. The loader path
	// comes from the ESP, which syncMirrorESP mounts again.
	if err := o.syncMirrorESP(ctx, mirrorEFI); err != nil {
		return err
	}
	if !ready {
		o.log.Warn("EFI variables unavailable, skipping the EFI boot entry for the mirror disk; it boots through the removable fallback loader",
			"disk", o.mirrorDisk, "reason", reason)
		return nil
	}
	return o.efiEntryResult("create mirror EFI boot entry", o.config.CreateMirrorEFIBootEntry(ctx, o.mirrorDisk, mirrorEFI))
}

// requireRemovableEFILoader checks that the ESP mounted at espDir carries the
// removable fallback loader that firmware boots without a boot entry.
func requireRemovableEFILoader(espDir string) error {
	name, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		return err
	}
	bootDir := filepath.Join(espDir, "EFI", "BOOT")
	entries, err := os.ReadDir(bootDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", bootDir, err)
	}
	for _, e := range entries {
		// vfat names are case-insensitive.
		if strings.EqualFold(e.Name(), name) && e.Type().IsRegular() {
			return nil
		}
	}
	return fmt.Errorf("the ESP has no removable fallback loader EFI/BOOT/%s", name)
}

// mirrorESP returns the ESP partition of the mirror disk.
func (o *Orchestrator) mirrorESP() (string, error) {
	efiNum, err := disk.PartitionNumberChecked(o.bootPartition, o.targetDisk)
	if err != nil {
		return "", fmt.Errorf("determine EFI partition number: %w", err)
	}
	return blockdev.PartitionDevicePath(o.mirrorDisk, efiNum), nil
}

// installMirrorBIOSBootloader installs GRUB for BIOS boot into both disks
// from the target root, like the legacy provisioner. The replicated partition
// table gives the second disk the BIOS boot partition that GRUB embeds its
// core image into.
func (o *Orchestrator) installMirrorBIOSBootloader(ctx context.Context) error {
	for _, d := range []string{o.targetDisk, o.mirrorDisk} {
		dev, err := validateChrootDevicePath("mirror disk", d)
		if err != nil {
			return err
		}
		if out, err := o.disk.ChrootRun(ctx, newroot, "grub-install --target=i386-pc "+dev); err != nil {
			return fmt.Errorf("install BIOS bootloader on %s: %s: %w", dev, string(out), err)
		}
	}
	return nil
}

// syncMirrorESP copies the ESP block device with the ESP unmounted, so the
// copy is consistent, and mounts it again for the remaining steps.
func (o *Orchestrator) syncMirrorESP(ctx context.Context, mirrorEFI string) error {
	if err := o.unmountBoot(); err != nil {
		return err
	}
	if err := copyBlockDeviceFn(o.bootPartition, mirrorEFI); err != nil {
		return fmt.Errorf("copy EFI system partition: %w", err)
	}
	return o.mountBoot(ctx)
}

// bootedUEFI reports whether the host booted through UEFI firmware.
func bootedUEFI() bool {
	_, err := os.Stat(efiFirmwarePath)
	return err == nil
}

// mountMirrorBoot mounts the separate /boot array at the target /boot, so the
// configuration steps update the kernels, initramfs and GRUB files that the
// host boots from. mount-boot calls it before mounting the ESP below it.
func (o *Orchestrator) mountMirrorBoot(ctx context.Context) error {
	if !slices.Contains(o.mirrorArrays, disk.MirrorBootArray) {
		return nil
	}
	mountpoint := filepath.Join(newroot, "boot")
	if isMountPoint(mountpoint) {
		_, err := mountedFromExpectedSource(mountpoint, disk.MirrorBootArray, "separate /boot")
		return err
	}
	if err := mountBootPart(ctx, o.disk, disk.MirrorBootArray, mountpoint); err != nil {
		return fmt.Errorf("mounting separate /boot %s at %s: %w", disk.MirrorBootArray, mountpoint, err)
	}
	return nil
}

// unmountMirrorBoot unmounts the separate /boot array after the ESP below it.
func (o *Orchestrator) unmountMirrorBoot() error {
	mountpoint := filepath.Join(newroot, "boot")
	if !slices.Contains(o.mirrorArrays, disk.MirrorBootArray) || !isMountPoint(mountpoint) {
		return nil
	}
	if err := o.disk.Unmount(mountpoint); err != nil {
		return fmt.Errorf("unmount separate /boot %s: %w", mountpoint, err)
	}
	return nil
}

// writeTargetRootFile writes data to path below root without following
// symlinks out of the target root, which comes from the streamed image.
func writeTargetRootFile(root, path string, data []byte) error {
	if err := ensureWithinRoot(root, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureTargetParentWithinRoot(root, dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := ensureTargetParentWithinRoot(root, dir); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o644)
}
