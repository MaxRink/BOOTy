//go:build linux

package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

// Deprovision runs the deprovisioning pipeline.
// Mode "soft-deprovision" (or "soft") renames grub.cfg to make the system unbootable.
// Mode "deprovision" or "hard" (default) wipes all disks and removes EFI boot entries.
// Mode "decommission" wipes all disks and removes all EFI boot entries on a
// best-effort basis, so defect or missing devices do not stop it.
func (o *Orchestrator) Deprovision(ctx context.Context) error {
	mode := o.cfg.Mode
	if mode == "" {
		mode = "hard"
	}
	o.cfg.Mode = mode
	o.log.Info("starting deprovisioning", "mode", mode)

	steps := o.deprovisionSteps(mode)
	defer o.flushTelemetry(context.Background())
	tracker := o.collector.Steps()

	for i, step := range steps {
		o.log.Info("deprovisioning step", "step", step.Name, "index", i+1, "total", len(steps))
		tracker.StartStep(step.Name)
		err := WithCode(step.Name, step.Fn(ctx))
		tracker.EndStep(step.Name, err)
		if err != nil {
			msg := stepFailureMessage(step.Name, err)
			o.log.Error("deprovisioning step failed", "step", step.Name, "error", err)
			DumpDebugState(step.Name)
			if reportErr := o.provider.ReportStatus(ctx, config.StatusError, msg); reportErr != nil {
				o.log.Error("failed to report error status", "error", reportErr)
			}
			return fmt.Errorf("deprovision step %s: %w", step.Name, err)
		}
	}
	return nil
}

func (o *Orchestrator) deprovisionSteps(mode string) []Step {
	steps := []Step{
		{"report-init", o.reportInit},
		{"copy-provisioner-files", o.copyProvisionerFiles},
		{"configure-dns", o.configureDNS},
	}

	switch mode {
	case "soft", "soft-deprovision":
		steps = append(steps, Step{"soft-deprovision", o.softDeprovision})
	case "decommission":
		steps = append(steps,
			Step{"decommission-wipe", o.decommissionWipe},
			Step{"mount-efivarfs", o.mountEFIVarsBestEffort},
			Step{"remove-all-efi-entries", o.removeAllEFIBootEntries},
		)
	default:
		steps = append(steps,
			// The EFI entry steps run before the wipe: when the EFI
			// variables are unavailable, the run fails before it destroys
			// anything (the legacy script removed them after the wipe).
			Step{"mount-efivarfs", o.mountEFIVars},
			Step{"remove-efi-entries", o.removeEFIBootEntries},
			Step{"select-deprovision-disk", o.selectDeprovisionDisk},
			Step{"disable-lvm", o.disableLVM},
			Step{"stop-raid", o.stopRAIDForDeprovision},
			Step{"wipe-disks", o.wipeOrSecureEraseDisks},
		)
	}
	return append(steps, Step{"report-success", o.reportDeprovisionSuccess})
}

// decommissionWipe matches decommission_wipe in the CAPRF legacy
// deprovision.sh: wipe every disk, log failures and continue.
func (o *Orchestrator) decommissionWipe(ctx context.Context) error {
	o.waitForDisks(ctx, "", "")
	if err := o.disk.DisableLVM(ctx); err != nil {
		o.log.Warn("decommission: disabling LVM failed, continuing", "error", err)
	}
	if err := o.disk.StopRAIDArrays(ctx); err != nil {
		o.log.Warn("decommission: stopping RAID arrays failed, continuing", "error", err)
	}
	if config.BoolOr(o.cfg.Provision.Disk.DiscardNVMe, false) {
		// wipe_all_disks discards NVMe disks before the signature wipe.
		o.disk.DiscardNVMeDisks(ctx)
	}
	if err := o.bestEffortWipeAll(ctx); err != nil {
		return err
	}
	o.log.Info("decommission: disk wipe complete")
	return nil
}

// bestEffortWipeAll wipes every disk like the legacy wipe_all_disks: a
// failed disk wipe is logged and the run goes on. A failed in-use check, or
// every disk in use, fails the run: then BOOTy can not tell what it wiped.
func (o *Orchestrator) bestEffortWipeAll(ctx context.Context) error {
	skipped, err := o.disk.WipeAllDisks(ctx)
	o.skippedDisks = skipped
	switch {
	case errors.Is(err, disk.ErrInUseCheck), errors.Is(err, disk.ErrAllDisksInUse):
		return err
	case err != nil:
		o.log.Warn("disk wipe failed, continuing", "error", err)
	}
	return nil
}

func (o *Orchestrator) removeAllEFIBootEntries(ctx context.Context) error {
	o.config.RemoveAllEFIBootEntries(ctx)
	return nil
}

func (o *Orchestrator) selectDeprovisionDisk(ctx context.Context) error {
	device := strings.TrimSpace(o.cfg.Deprovision.Device)
	name := "deprovision.device"
	if device == "" {
		device, name = strings.TrimSpace(o.cfg.Provision.Disk.Device), "provision.disk.device/DISK_DEVICE"
	}
	// The ramdisk has no udev and BOOTy creates no /dev/disk links before
	// the deprovision disk selection, so an alias can never exist here.
	// detect-disk of provisioning uses the same rule.
	if err := checkRamdiskDevicePath(name, device); err != nil {
		return err
	}
	o.waitForDisks(ctx, device, "")
	device, err := o.configuredDeprovisionDisk()
	if err != nil {
		return err
	}
	if device == "" {
		o.log.Info("no deprovision disk override configured; hard deprovision will wipe all disks")
		return nil
	}
	o.targetDisk = device
	// If a configured target is a member of an assembled BOOTy mirror, retain
	// its companion before stop-raid tears the array down. Auto mode may also
	// have selected a single disk, so only an observed array membership expands
	// the wipe scope.
	if err := o.captureMirrorCompanion(ctx, device); err != nil {
		return err
	}
	o.log.Info("using configured deprovision disk", "device", device)
	return nil
}

func (o *Orchestrator) configuredDeprovisionDisk() (string, error) {
	for _, candidate := range []struct {
		name   string
		device string
	}{
		{name: "deprovision.device", device: o.cfg.Deprovision.Device},
		{name: "provision.disk.device/DISK_DEVICE", device: o.cfg.Provision.Disk.Device},
	} {
		device := strings.TrimSpace(candidate.device)
		if device == "" {
			continue
		}
		if err := checkRamdiskDevicePath(candidate.name, device); err != nil {
			return "", err
		}
		if err := validateConfiguredBlockDevice(candidate.name, device); err != nil {
			return "", err
		}
		return device, nil
	}
	return "", nil
}

func validateConfiguredBlockDevice(name, device string) error {
	if !strings.HasPrefix(device, "/dev/") {
		return fmt.Errorf("%s %q must be an absolute /dev/ path", name, device)
	}
	info, err := statPath(device)
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, device, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("%s %q is not a block device", name, device)
	}
	return nil
}

// softDeprovision renames grub.cfg so the system won't boot.
func (o *Orchestrator) softDeprovision(ctx context.Context) error {
	var d string
	var err error
	if dev, devErr := o.configuredDeprovisionDisk(); devErr != nil {
		return devErr
	} else if dev != "" {
		d = dev
	} else {
		d, err = o.disk.DetectDisk(ctx, o.cfg.Provision.Disk.MinSizeGB)
		if err != nil {
			return fmt.Errorf("detecting disk: %w", err)
		}
	}
	if err := o.disk.PartProbe(ctx, d); err != nil {
		return fmt.Errorf("partprobe: %w", err)
	}
	parts, err := o.disk.ParsePartitions(ctx, d)
	if err != nil {
		return fmt.Errorf("parsing partitions: %w", err)
	}
	root, err := o.disk.FindRootPartition(parts)
	if err != nil {
		return fmt.Errorf("finding root: %w", err)
	}
	// A member of a DISK_MIRRORING_MODE array mounts as a plain filesystem,
	// but writing to it bypasses the array, and the other disk stays
	// bootable. Hard deprovision without a device override wipes both disks.
	fsType, err := o.disk.FilesystemType(ctx, root.Node)
	if err != nil {
		// Without the type BOOTy can not rule out a RAID member, and a
		// mount could still recognize the filesystem inside it.
		return fmt.Errorf("detecting root filesystem type: %w", err)
	}
	if fsType == "linux_raid_member" {
		return fmt.Errorf("root partition %s is a RAID member of a mirrored install; soft deprovision supports single-disk roots only, use hard deprovision without a device override", root.Node)
	}
	if err := o.disk.MountPartition(ctx, root.Node, newroot); err != nil {
		return fmt.Errorf("mounting root: %w", err)
	}
	return disableGRUBAndUnmount(newroot, o.disk.Unmount)
}

// disableGRUBAndUnmount disables GRUB under root and unmounts root. The
// rename must reach the disk, so an unmount failure is an error too.
func disableGRUBAndUnmount(root string, unmount func(string) error) error {
	disableErr := disableGRUBConfig(root)
	if err := unmount(root); err != nil {
		return errors.Join(disableErr, fmt.Errorf("unmounting root: %w", err))
	}
	return disableErr
}

// disableGRUBConfig renames the GRUB config under root to grub.cfg.bak so
// the system does not boot. It checks the Debian and the RHEL path. A
// config that was already renamed counts as done, so a rerun succeeds. No
// config at all is an error, because the system would stay bootable while
// deprovisioning reports success.
func disableGRUBConfig(root string) error {
	renamed := false
	for _, dir := range []string{"grub", "grub2"} {
		grubDir := filepath.Join(root, "boot", dir)
		// A symlinked boot or grub directory must not lead the rename out
		// of the target root.
		if err := ensureTargetParentWithinRoot(root, grubDir); err != nil {
			return fmt.Errorf("checking %s: %w", grubDir, err)
		}
		grubCfg := filepath.Join(grubDir, "grub.cfg")
		grubBak := grubCfg + ".bak"
		done, err := renameGRUBConfig(grubCfg, grubBak)
		if err != nil {
			return err
		}
		renamed = renamed || done
	}
	if !renamed {
		return fmt.Errorf("no grub.cfg found under %s/boot/grub or %s/boot/grub2; system would stay bootable", root, root)
	}
	return nil
}

// renameGRUBConfig renames cfg to bak. It reports true when cfg was renamed
// now or bak already exists. Only ENOENT counts as absence.
func renameGRUBConfig(cfg, bak string) (bool, error) {
	if _, err := os.Lstat(cfg); err == nil {
		slog.Info("renaming grub.cfg", "from", cfg, "to", bak)
		if err := os.Rename(cfg, bak); err != nil {
			return false, fmt.Errorf("renaming grub.cfg: %w", err)
		}
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("checking %s: %w", cfg, err)
	}
	if _, err := os.Lstat(bak); err == nil {
		slog.Info("grub.cfg already renamed", "path", bak)
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("checking %s: %w", bak, err)
	}
	return false, nil
}

func (o *Orchestrator) reportDeprovisionSuccess(ctx context.Context) error {
	return o.reportTerminalSuccess(ctx, "deprovisioning", "deprovisioning complete"+NotWipedNote(o.skippedDisks)+o.eraseFallbackNote())
}

// SkippedDisks returns the disks in use that the wipe of all disks of the
// last run skipped.
func (o *Orchestrator) SkippedDisks() []string {
	return slices.Clone(o.skippedDisks)
}

// SecureEraseFallbacks returns the disks of the last run that got the plain
// wipe instead of the secure erase (SECURE_ERASE_FALLBACK=wipe).
func (o *Orchestrator) SecureEraseFallbacks() []string {
	if o.disk == nil {
		return nil
	}
	return o.disk.SecureEraseFallbacks()
}

func (o *Orchestrator) eraseFallbackNote() string {
	return EraseFallbackNote(o.SecureEraseFallbacks())
}

// EraseFallbackNote is the status and ACK suffix that names the disks that
// got the plain wipe instead of the secure erase, or "" when there are none.
func EraseFallbackNote(disks []string) string {
	if len(disks) == 0 {
		return ""
	}
	return "; secure erase not done, wiped instead (SECURE_ERASE_FALLBACK=wipe): " + strings.Join(disks, ", ")
}

// NotWipedNote is the status and ACK suffix that names skipped disks, or
// "" when no disk was skipped.
func NotWipedNote(disks []string) string {
	if len(disks) == 0 {
		return ""
	}
	return "; not wiped, in use: " + strings.Join(disks, ", ")
}

func canonicalBlockDevice(device string) string {
	if !strings.HasPrefix(device, "/dev/disk/") {
		return device
	}
	resolved, err := evalRootSymlinks(device)
	if err != nil || !strings.HasPrefix(resolved, "/dev/") {
		return ""
	}
	return resolved
}

// wholeDiskForPartition returns the whole disk that holds device, or device
// itself when it is no partition, such as a whole disk, /dev/md2 or
// /dev/dm-0. It asks sysfs instead of parsing the name: a partition has a
// "partition" attribute, and its sysfs directory sits in the directory of
// its parent disk. A device that sysfs does not know is an error, because
// then the wipe scope can not be told.
func wholeDiskForPartition(device string) (string, error) {
	if !strings.HasPrefix(device, "/dev/") {
		return "", fmt.Errorf("%q is not a /dev/ path", device)
	}
	dir := filepath.Join(sysBlockRoot, filepath.Base(device))
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s in sysfs: %w", device, err)
	}
	if _, err := os.Stat(filepath.Join(resolved, "partition")); errors.Is(err, fs.ErrNotExist) {
		return device, nil
	} else if err != nil {
		return "", fmt.Errorf("check whether %s is a partition: %w", device, err)
	}
	return "/dev/" + filepath.Base(filepath.Dir(resolved)), nil
}

func (o *Orchestrator) captureMirrorCompanion(ctx context.Context, target string) error {
	mode := o.mirroringMode()
	if mode != disk.MirroringModeRAID1 && mode != disk.MirroringModeAuto {
		return nil
	}
	targetDisk, err := wholeDiskForPartition(target)
	if err != nil {
		return fmt.Errorf("refusing scoped deprovision: %w", err)
	}
	var companion string
	for _, array := range []string{disk.MirrorRootArray, disk.MirrorBootArray} {
		arrayCompanion, err := o.mirrorArrayCompanion(ctx, array, targetDisk)
		if err != nil {
			return err
		}
		if arrayCompanion == "" {
			continue
		}
		if companion != "" && companion != arrayCompanion {
			return fmt.Errorf("refusing scoped deprovision: mirror arrays disagree on companion disk (%s and %s)", companion, arrayCompanion)
		}
		companion = arrayCompanion
	}
	if companion != "" {
		o.mirrorDisk = companion
	}
	return nil
}

func (o *Orchestrator) mirrorArrayCompanion(ctx context.Context, array, targetDisk string) (string, error) {
	members, err := o.disk.MirrorMembers(ctx, array)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such") || strings.Contains(strings.ToLower(err.Error()), "not found") {
			return "", nil
		}
		return "", err
	}
	disks := make(map[string]struct{}, len(members))
	for _, originalMember := range members {
		member := canonicalBlockDevice(originalMember)
		if member == "" {
			return "", fmt.Errorf("refusing scoped deprovision: cannot resolve mirror member %q", originalMember)
		}
		wholeDisk, err := wholeDiskForPartition(member)
		if err != nil {
			return "", fmt.Errorf("refusing scoped deprovision: mirror member %s: %w", originalMember, err)
		}
		disks[wholeDisk] = struct{}{}
	}
	if _, ok := disks[targetDisk]; !ok {
		return "", nil
	}
	if len(disks) != 2 {
		return "", fmt.Errorf("refusing scoped deprovision: mirror array %s has %d whole-disk members, want exactly 2", array, len(disks))
	}
	for candidate := range disks {
		if candidate != targetDisk {
			return candidate, nil
		}
	}
	return "", nil
}
