//go:build linux

package disk

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/telekom/BOOTy/pkg/executil"
)

// Commander abstracts command execution for testing.
type Commander = executil.Commander

// ExecCommander executes real system commands.
type ExecCommander = executil.ExecCommander

var (
	mountFunc               = syscall.Mount
	unmountFunc             = syscall.Unmount
	isMountPointFunc        = isMountPoint
	readMountsFile          = os.ReadFile
	runChrootSyscallCommand = runChrootSyscallExec
)

// Manager handles disk operations for provisioning.
type Manager struct {
	cmd       Commander
	sysfsRoot string // path to sysfs root, defaults to "/sys"
	// legacyCAPRFSelection makes DetectDisk pick the disk like the CAPRF
	// legacy provisioner. See SetLegacyCAPRFSelection.
	legacyCAPRFSelection bool
	// quickWipe limits the wipe to the disk signatures. See WithQuickWipe.
	quickWipe bool
	// procDir replaces /proc in tests. See CheckNotInUse.
	procDir string
}

// SetLegacyCAPRFSelection makes DetectDisk select the target disk like
// get_target_disks in the CAPRF legacy common.sh: NVMe (major 259) and
// virtio (major 253) disks first, then SCSI/SATA disks (major 8), each in
// lsblk order, and the first disk of at least the minimum size wins.
func (m *Manager) SetLegacyCAPRFSelection(enabled bool) {
	m.legacyCAPRFSelection = enabled
}

// NewManager creates a Manager with the given Commander (nil = ExecCommander).
func NewManager(cmd Commander) *Manager {
	if cmd == nil {
		cmd = &ExecCommander{}
	}
	return &Manager{cmd: cmd, sysfsRoot: "/sys"}
}

// SetSysfsRoot changes the sysfs root that disk discovery reads, "/sys" by
// default. Tests use it with a fixture tree.
func (m *Manager) SetSysfsRoot(root string) {
	m.sysfsRoot = root
}

// Run executes a host command through the manager's command runner.
func (m *Manager) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return m.cmd.Run(ctx, name, args...)
}

// newManagerWithSysfs creates a Manager with a custom sysfs root for testing.
func newManagerWithSysfs(cmd Commander, sysfsRoot string) *Manager {
	if cmd == nil {
		cmd = &ExecCommander{}
	}
	return &Manager{cmd: cmd, sysfsRoot: sysfsRoot}
}

// Partition represents a single disk partition from sfdisk output.
type Partition struct {
	Node  string `json:"node"`
	Start int64  `json:"start"`
	Size  int64  `json:"size"`
	Type  string `json:"type"`
	Name  string `json:"name,omitempty"`
}

// sfdiskOutput represents the JSON output of sfdisk --json.
type sfdiskOutput struct {
	PartitionTable struct {
		Partitions []Partition `json:"partitions"`
	} `json:"partitiontable"`
}

// EFI and Linux partition type GUIDs.
const (
	EFISystemPartitionGUID = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	LinuxFilesystemGUID    = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	LinuxFilesystemMBRType = "83"
)

// StopRAIDArrays stops all RAID arrays via mdadm.
func (m *Manager) StopRAIDArrays(ctx context.Context) error {
	slog.Info("stopping RAID arrays")
	out, err := m.cmd.Run(ctx, "mdadm", "--stop", "--scan")
	if err != nil {
		if isNoArraysFound(out, err) {
			slog.Debug("mdadm stop reported no arrays", "output", strings.TrimSpace(string(out)), "error", err)
			return nil
		}
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return fmt.Errorf("stop raid arrays: %w", err)
		}
		return fmt.Errorf("stop raid arrays: %s: %w", trimmed, err)
	}
	return nil
}

func isNoArraysFound(out []byte, err error) bool {
	combined := strings.ToLower(strings.TrimSpace(string(out)))
	if err != nil {
		combined = strings.TrimSpace(combined + " " + strings.ToLower(err.Error()))
	}
	return strings.Contains(combined, "no arrays found")
}

// WipeDisk clears the partitions and the partition table of a single device.
// It does not overwrite the data. See wipeDevice and WithQuickWipe.
func (m *Manager) WipeDisk(ctx context.Context, device string) error {
	device = strings.TrimSpace(device)
	if device == "" {
		return fmt.Errorf("wipe disk: device is required")
	}
	if err := m.CheckNotInUse(device); err != nil {
		return err
	}
	slog.Info("wiping disk", "device", device, "quick", m.quickWipe)
	if err := m.wipeDevice(ctx, device); err != nil {
		return fmt.Errorf("wipefs %s: %w", device, err)
	}
	return nil
}

// WipeFilesystemSignatures clears filesystem signatures on an existing
// partition without touching the parent disk partition table.
func (m *Manager) WipeFilesystemSignatures(ctx context.Context, device string) error {
	device = strings.TrimSpace(device)
	if device == "" {
		return fmt.Errorf("wipe filesystem signatures: device is required")
	}
	if err := m.CheckNotInUse(device); err != nil {
		return err
	}
	slog.Info("wiping filesystem signatures", "device", device)
	if _, err := m.cmd.Run(ctx, "wipefs", "-af", device); err != nil {
		return fmt.Errorf("wipefs %s: %w", device, err)
	}
	return nil
}

// WipeAllDisks wipes all block devices excluding loop and CD-ROM, like
// WipeDisk. It skips a disk that is in use and returns the skipped disks.
// It fails when a disk could not be checked (ErrInUseCheck), when every
// disk was skipped (ErrAllDisksInUse) or when a wipe failed.
func (m *Manager) WipeAllDisks(ctx context.Context) (skipped []string, err error) {
	slog.Info("wiping all disks", "quick", m.quickWipe)
	blockDir := filepath.Join(m.sysfsRoot, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", blockDir, err)
	}
	var (
		wiped int
		errs  []error
	)
	for _, entry := range entries {
		name := entry.Name()
		if isVirtualDisk(name) {
			continue
		}
		dev := "/dev/" + name
		skip, err := m.skipInUse(dev)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if skip {
			skipped = append(skipped, dev)
			continue
		}
		slog.Info("wiping disk", "device", dev)
		if err := m.wipeDevice(ctx, dev); err != nil {
			slog.Warn("wipefs failed", "device", dev, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", dev, err))
		} else {
			wiped++
		}
	}
	return skipped, allDisksResult("wipe", wiped, skipped, errs)
}

// allDisksResult is the error of an operation on all disks.
func allDisksResult(op string, done int, skipped []string, errs []error) error {
	switch {
	case len(errs) > 0 && done == 0:
		return fmt.Errorf("all %d disk %s(s) failed: %w", len(errs), op, errors.Join(errs...))
	case len(errs) > 0:
		return fmt.Errorf("%s done on %d disk(s) but %d failed: %w", op, done, len(errs), errors.Join(errs...))
	case done == 0 && len(skipped) > 0:
		return fmt.Errorf("%w (%s)", ErrAllDisksInUse, strings.Join(skipped, ", "))
	}
	return nil
}

// DiscardNVMeDisks runs blkdiscard on every NVMe disk, like wipe_all_disks
// in the CAPRF legacy common.sh. Failures are logged and ignored.
func (m *Manager) DiscardNVMeDisks(ctx context.Context) {
	blockDir := filepath.Join(m.sysfsRoot, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		slog.Warn("cannot list disks for discard", "path", blockDir, "error", err)
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "nvme") || nvmePathNodeRE.MatchString(entry.Name()) {
			continue
		}
		dev := "/dev/" + entry.Name()
		if err := m.CheckNotInUse(dev); err != nil {
			slog.Error("disk is in use, not discarded", "device", dev, "error", err)
			continue
		}
		if out, err := m.cmd.Run(ctx, "blkdiscard", dev); err != nil {
			slog.Info("blkdiscard failed, falling back to signature wipe", "device", dev, "error", err, "output", string(out))
			continue
		}
		slog.Info("blkdiscard complete", "device", dev)
	}
}

// SecureEraseAllDisks performs hardware-level secure erase on all disks.
// For NVMe drives: uses nvme format (User Data Erase).
// For SATA/SAS drives: uses ATA SECURITY ERASE UNIT via hdparm.
// Falls back to quick erase (wipefs) if secure erase is not supported.
func (m *Manager) SecureEraseAllDisks(ctx context.Context) (skipped []string, err error) {
	slog.Info("performing secure erase on all disks")
	blockDir := filepath.Join(m.sysfsRoot, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", blockDir, err)
	}
	var (
		erased int
		errs   []error
	)
	for _, entry := range entries {
		name := entry.Name()
		if isVirtualDisk(name) {
			continue
		}
		dev := "/dev/" + name
		skip, err := m.skipInUse(dev)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if skip {
			skipped = append(skipped, dev)
			continue
		}
		erase := m.secureEraseSATA
		if strings.HasPrefix(name, "nvme") {
			erase = m.secureEraseNVMe
		}
		if err := erase(ctx, dev); err != nil {
			errs = append(errs, err)
		} else {
			erased++
		}
	}
	return skipped, allDisksResult("secure erase", erased, skipped, errs)
}

// SecureEraseDisk performs hardware-level secure erase on a single disk.
// It falls back to quick erase when the device does not support secure erase.
func (m *Manager) SecureEraseDisk(ctx context.Context, device string) error {
	device = strings.TrimSpace(device)
	if device == "" {
		return fmt.Errorf("secure erase disk: device is required")
	}

	checkPath := device
	erasePath := device
	if resolved, err := filepath.EvalSymlinks(device); err == nil {
		checkPath = resolved
		erasePath = resolved
	}
	if strings.HasPrefix(filepath.Base(checkPath), "nvme") {
		return m.secureEraseNVMe(ctx, erasePath)
	}
	return m.secureEraseSATA(ctx, erasePath)
}

// secureEraseNVMe performs NVMe User Data Erase via nvme format command.
func (m *Manager) secureEraseNVMe(ctx context.Context, dev string) error {
	if err := m.CheckNotInUse(dev); err != nil {
		return err
	}
	slog.Info("NVMe secure erase", "device", dev)
	// ses=1: User Data Erase, ses=2: Crypto Erase (not all drives support it).
	out, err := m.cmd.Run(ctx, "nvme", "format", dev, "--ses=1", "--force")
	if err != nil {
		if isExecNotFound(err) {
			return fmt.Errorf("%s: nvme secure erase tool is required when secure erase is enabled: %w", dev, err)
		}
		slog.Warn("NVMe secure erase failed, falling back to wipefs",
			"device", dev, "output", string(out), "error", err)
		if out, err := m.cmd.Run(ctx, "wipefs", "-af", dev); err != nil {
			return fmt.Errorf("%s: nvme format and wipefs fallback failed: %s: %w", dev, string(out), err)
		}
	}
	return nil
}

// secureEraseSATA performs ATA SECURITY ERASE UNIT via hdparm.
func (m *Manager) secureEraseSATA(ctx context.Context, dev string) error {
	if err := m.CheckNotInUse(dev); err != nil {
		return err
	}
	slog.Info("SATA secure erase", "device", dev)
	pwd, err := temporaryErasePassword()
	if err != nil {
		return fmt.Errorf("generate temporary erase password for %s: %w", dev, err)
	}

	// Step 1: Check if security is supported.
	out, err := m.cmd.Run(ctx, "hdparm", "-I", dev)
	if err != nil && isExecNotFound(err) {
		return fmt.Errorf("%s: hdparm secure erase tool is required when secure erase is enabled: %w", dev, err)
	}
	if err != nil || !strings.Contains(string(out), "Security:") {
		slog.Info("drive does not support ATA security, using wipefs", "device", dev)
		if out, err := m.cmd.Run(ctx, "wipefs", "-af", dev); err != nil {
			return fmt.Errorf("%s: hdparm and wipefs fallback failed: %s: %w", dev, string(out), err)
		}
		return nil
	}
	if strings.Contains(string(out), "frozen") {
		slog.Warn("drive is security-frozen, cannot secure erase, using wipefs", "device", dev)
		if out, err := m.cmd.Run(ctx, "wipefs", "-af", dev); err != nil {
			return fmt.Errorf("%s: frozen drive wipefs fallback failed: %s: %w", dev, string(out), err)
		}
		return nil
	}
	// Step 2: Set a temporary password and issue secure erase.
	if out, err := m.cmd.Run(ctx, "hdparm", "--user-master", "u", "--security-set-pass", pwd, dev); err != nil {
		return fmt.Errorf("%s: failed to set security password: %s: %w", dev, string(out), err)
	}
	if out, err := m.cmd.Run(ctx, "hdparm", "--user-master", "u", "--security-erase", pwd, dev); err != nil {
		slog.Warn("ATA security erase failed", "device", dev, "output", string(out))
		// Clear the password we just set.
		if out2, err2 := m.cmd.Run(ctx, "hdparm", "--user-master", "u", "--security-disable", pwd, dev); err2 != nil {
			slog.Warn("failed to clear security password", "device", dev, "output", string(out2))
		}
		return fmt.Errorf("%s: ATA security erase failed: %w", dev, err)
	}
	if out, err := m.cmd.Run(ctx, "hdparm", "--user-master", "u", "--security-disable", pwd, dev); err != nil {
		slog.Warn("failed to clear temporary security password after erase", "device", dev, "output", string(out), "error", err)
	}
	return nil
}

func temporaryErasePassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return "Erase-" + hex.EncodeToString(b), nil
}

// CreateRAIDArray creates a software RAID array using mdadm.
// level is the RAID level (0, 1, 5, 6, 10). devices are the member disks.
func (m *Manager) CreateRAIDArray(ctx context.Context, name string, level int, devices []string) error {
	if len(devices) < 2 {
		return fmt.Errorf("RAID requires at least 2 devices, got %d", len(devices))
	}

	slog.Info("creating RAID array", "name", name, "level", level, "devices", devices)

	args := make([]string, 0, 10+len(devices))
	args = append(args,
		"--create", "/dev/"+name,
		"--level", strconv.Itoa(level),
		"--raid-devices", strconv.Itoa(len(devices)),
		"--run",   // don't ask for confirmation
		"--force", // force creation
		"--metadata", "1.2",
	)
	args = append(args, devices...)

	out, err := m.cmd.Run(ctx, "mdadm", args...)
	if err != nil {
		return fmt.Errorf("mdadm create %s: %s: %w", name, string(out), err)
	}

	slog.Info("RAID array created", "name", name)
	return nil
}

// DetectDisk finds the target disk for provisioning. By default it prefers
// NVMe, then the largest disk, and rejects removable media (USB drives, SD
// cards) unless BOOTY_ALLOW_REMOVABLE=true. With SetLegacyCAPRFSelection it
// takes the first disk in legacy lsblk order instead (NVMe and virtio, then
// SCSI/SATA) and, like the legacy script, does not skip removable media.
func (m *Manager) DetectDisk(ctx context.Context, minSizeGB int) (string, error) {
	if m.legacyCAPRFSelection {
		return m.detectDiskLegacyCAPRF(ctx, minSizeGB)
	}
	return m.detectDiskPreferNVMe(minSizeGB)
}

// detectDiskPreferNVMe picks an NVMe disk if present, else the largest disk.
func (m *Manager) detectDiskPreferNVMe(minSizeGB int) (string, error) {
	slog.Info("detecting target disk", "minSizeGB", minSizeGB)
	blockDir := m.sysfsRoot + "/block"
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", blockDir, err)
	}

	allowRemovable := os.Getenv("BOOTY_ALLOW_REMOVABLE") == "true"

	type candidate struct {
		path   string
		sizeGB int
		isNVMe bool
	}
	var candidates []candidate

	for _, entry := range entries {
		name := entry.Name()
		if isVirtualDisk(name) {
			continue
		}

		if !allowRemovable && m.isRemovableMedia(name) {
			slog.Warn("skipping removable media device", "device", "/dev/"+name)
			continue
		}

		sizeGB, err := m.readDiskSizeGB(name)
		if err != nil {
			continue
		}
		if minSizeGB > 0 && sizeGB < minSizeGB {
			continue
		}
		candidates = append(candidates, candidate{
			path:   "/dev/" + name,
			sizeGB: sizeGB,
			isNVMe: strings.HasPrefix(name, "nvme"),
		})
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no suitable disk found (min %d GB)", minSizeGB)
	}

	// Prefer NVMe, otherwise pick largest.
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.isNVMe && !best.isNVMe {
			best = c
		} else if c.isNVMe == best.isNVMe && c.sizeGB > best.sizeGB {
			best = c
		}
	}

	slog.Info("selected disk", "device", best.path, "sizeGB", best.sizeGB, "nvme", best.isNVMe)
	return best.path, nil
}

// legacyCAPRFDiskMajors are the lsblk --include groups of the CAPRF legacy
// _discover_disks, in preference order.
var legacyCAPRFDiskMajors = []string{"259,253", "8"}

func (m *Manager) detectDiskLegacyCAPRF(ctx context.Context, minSizeGB int) (string, error) {
	const maxMinSizeGB = math.MaxInt64 >> 30
	if minSizeGB < 0 || int64(minSizeGB) > maxMinSizeGB {
		return "", fmt.Errorf("minimum disk size %d GB is out of range", minSizeGB)
	}
	minBytes := int64(minSizeGB) << 30
	slog.Info("detecting target disk like the legacy CAPRF provisioner", "minSizeBytes", minBytes)
	var candidates, firstBus []string
	for _, majors := range legacyCAPRFDiskMajors {
		out, err := m.cmd.Run(ctx, "lsblk", "--nodeps", "--noheadings", "--include", majors,
			"--output", "PATH,SIZE", "--bytes")
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("detect disk: %w", ctxErr)
			}
			// The legacy script reads lsblk through process substitution and
			// ignores its exit status.
			slog.Warn("lsblk failed", "include", majors, "error", err, "output", string(out))
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			size, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || size < minBytes {
				continue
			}
			slog.Info("disk candidate", "device", fields[0], "sizeBytes", size, "majors", majors)
			candidates = append(candidates, fields[0])
		}
		if firstBus == nil && len(candidates) > 0 {
			firstBus = candidates
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no suitable disk found (min %d GB)", minSizeGB)
	}
	if len(firstBus) > 1 {
		// The kernel names disks of one bus in probe order, which can change
		// between boots (lab: the 40 GB disk was sda in one boot, sdb in the
		// next). The lsblk order then picks another disk.
		slog.Warn("lsblk disk selection is ambiguous: several disks of the same bus qualify and their names can change between boots; select the root disk with DISK_SERIAL_NUMBER, and exclude smaller disks with MIN_DISK_SIZE_GB",
			"selected", firstBus[0], "candidates", strings.Join(firstBus, ","))
	}
	slog.Info("selected disk", "device", candidates[0])
	return candidates[0], nil
}

// FindDiskBySerial finds a non-virtual target disk whose serial or stable
// identifier exactly matches serial. Removable media are rejected unless
// BOOTY_ALLOW_REMOVABLE=true.
func (m *Manager) FindDiskBySerial(ctx context.Context, serial string, minSizeGB int) (string, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return "", fmt.Errorf("disk serial is required")
	}
	slog.Info("detecting target disk by serial", "serial", serial, "minSizeGB", minSizeGB)

	allowRemovable := os.Getenv("BOOTY_ALLOW_REMOVABLE") == "true"
	matches, observed, err := m.findDiskBySerialWithSysfs(serial, minSizeGB, allowRemovable)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		lsblkMatches, lsblkObserved, err := m.findDiskBySerialWithLSBLK(ctx, serial, minSizeGB, allowRemovable)
		if err != nil {
			slog.Warn("lsblk serial lookup failed", "serial", serial, "err", err)
		}
		matches = append(matches, lsblkMatches...)
		observed = append(observed, lsblkObserved...)
	}

	switch len(matches) {
	case 0:
		if len(observed) > 0 {
			return "", fmt.Errorf("no suitable disk found with serial %q (observed: %s)", serial, strings.Join(observed, ", "))
		}
		return "", fmt.Errorf("no suitable disk found with serial %q", serial)
	case 1:
		slog.Info("selected disk by serial", "device", matches[0], "serial", serial)
		return matches[0], nil
	default:
		return "", fmt.Errorf("multiple disks found with serial %q: %s", serial, strings.Join(matches, ", "))
	}
}

func (m *Manager) findDiskBySerialWithSysfs(serial string, minSizeGB int, allowRemovable bool) (matches, observed []string, err error) {
	blockDir := m.sysfsRoot + "/block"
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", blockDir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if isVirtualDisk(name) {
			continue
		}
		if !allowRemovable && m.isRemovableMedia(name) {
			slog.Warn("skipping removable media device", "device", "/dev/"+name)
			continue
		}
		deviceSerial, err := m.readDiskSerial(name)
		if err != nil || !diskSerialMatches(deviceSerial, serial) {
			if deviceSerial != "" {
				observed = append(observed, "/dev/"+name+"="+deviceSerial)
			}
			continue
		}
		sizeGB, err := m.readDiskSizeGB(name)
		if err != nil {
			return nil, observed, fmt.Errorf("read size for disk with serial %q at /dev/%s: %w", serial, name, err)
		}
		if minSizeGB > 0 && sizeGB < minSizeGB {
			return nil, observed, fmt.Errorf("disk with serial %q at /dev/%s is %d GB, below minimum %d GB", serial, name, sizeGB, minSizeGB)
		}
		matches = append(matches, "/dev/"+name)
	}
	return matches, observed, nil
}

func (m *Manager) findDiskBySerialWithLSBLK(ctx context.Context, serial string, minSizeGB int, allowRemovable bool) (matches, observed []string, err error) {
	out, err := m.cmd.Run(ctx, "lsblk", "--nodeps", "--noheadings", "--paths", "--output", "NAME,SERIAL")
	if err != nil {
		return nil, nil, fmt.Errorf("lsblk NAME,SERIAL: %s: %w", string(out), err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		path := fields[0]
		deviceSerial := strings.Join(fields[1:], " ")
		if deviceSerial == "" {
			continue
		}
		observed = append(observed, path+"="+deviceSerial)
		if !diskSerialMatches(deviceSerial, serial) {
			continue
		}
		name := filepath.Base(path)
		if isVirtualDisk(name) {
			continue
		}
		if !allowRemovable && m.isRemovableMedia(name) {
			slog.Warn("skipping removable media device", "device", path)
			continue
		}
		sizeGB, err := m.readDiskSizeGB(name)
		if err != nil {
			return nil, observed, fmt.Errorf("read size for disk with serial %q at %s: %w", serial, path, err)
		}
		if minSizeGB > 0 && sizeGB < minSizeGB {
			return nil, observed, fmt.Errorf("disk with serial %q at %s is %d GB, below minimum %d GB", serial, path, sizeGB, minSizeGB)
		}
		matches = append(matches, path)
	}
	return matches, observed, nil
}

// isRemovableMedia reports whether the block device is removable (USB, SD card).
// Fails closed: if the sysfs attribute cannot be read, the device is treated as
// removable to err on the side of caution.
func (m *Manager) isRemovableMedia(name string) bool {
	path := m.sysfsRoot + "/block/" + name + "/removable"
	data, err := os.ReadFile(path) //nolint:gocritic // path join not needed for sysfs
	if err != nil {
		slog.Warn("cannot read removable attribute; treating as removable", "device", name, "err", err)
		return true
	}
	return strings.TrimSpace(string(data)) == "1"
}

// isVirtualDisk checks if a block device name is virtual and should be
// excluded from physical disk selection. Covers: loop devices, CD-ROMs,
// RAM disks, compressed RAM (zram), device-mapper, software RAID (md),
// ZFS zvols (zd), and network block devices (nbd).
func isVirtualDisk(name string) bool {
	return strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "sr") ||
		strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") ||
		strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md") ||
		strings.HasPrefix(name, "zd") || strings.HasPrefix(name, "nbd") ||
		nvmePathNodeRE.MatchString(name)
}

// nvmePathNodeRE matches NVMe multipath path nodes such as nvme0c0n1. With
// native NVMe multipath they show up in /sys/block next to the head node
// nvme0n1, but they are hidden and have no /dev node.
var nvmePathNodeRE = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)

// resizeKnownFilesystem grows ext2/3/4, xfs or btrfs with its own tool. It
// reports false for other types, so the caller falls back to trying tools.
func (m *Manager) resizeKnownFilesystem(ctx context.Context, fsType, device, mountpoint string) (bool, error) {
	var name string
	var args []string
	switch fsType {
	case "ext2", "ext3", "ext4":
		name, args = "resize2fs", []string{device}
	case "xfs":
		name, args = "xfs_growfs", []string{mountpoint}
	case "btrfs":
		name, args = "btrfs", []string{"filesystem", "resize", "max", mountpoint}
	default:
		return false, nil
	}
	if name != "resize2fs" && strings.TrimSpace(mountpoint) == "" {
		return true, fmt.Errorf("resize filesystem %s requires mountpoint for %s", device, fsType)
	}
	if out, err := m.cmd.Run(ctx, name, args...); err != nil {
		return true, fmt.Errorf("resize %s filesystem %s with %s: %s: %w", fsType, device, name, strings.TrimSpace(string(out)), err)
	}
	return true, nil
}

// readDiskSizeGB reads the size of a block device in GB from sysfs.
func (m *Manager) readDiskSizeGB(name string) (int, error) {
	sizePath := m.sysfsRoot + "/block/" + name + "/size" //nolint:gocritic // path join not needed for sysfs
	data, err := os.ReadFile(sizePath)
	if err != nil {
		return 0, fmt.Errorf("read disk size %s: %w", name, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse disk size %s: %w", name, err)
	}
	return int(sectors * 512 / (1024 * 1024 * 1024)), nil
}

func diskSerialMatches(candidate, target string) bool {
	candidate = strings.TrimSpace(candidate)
	target = strings.TrimSpace(target)
	return candidate != "" && target != "" && candidate == target
}

// readDiskSerial reads a block device serial number or stable identifier from
// common sysfs locations. Some virtual SCSI devices expose the stable identifier
// only through WWID/VPD attributes, so callers compare the normalized value from
// each source against the requested identifier.
func (m *Manager) readDiskSerial(name string) (string, error) {
	sources := []struct {
		path string
		vpd  bool
	}{
		{path: filepath.Join(m.sysfsRoot, "block", name, "device", "serial")},
		{path: filepath.Join(m.sysfsRoot, "block", name, "serial")},
		{path: filepath.Join(m.sysfsRoot, "block", name, "device", "wwid")},
		{path: filepath.Join(m.sysfsRoot, "block", name, "device", "vpd_pg80"), vpd: true},
		{path: filepath.Join(m.sysfsRoot, "block", name, "device", "vpd_pg83"), vpd: true},
	}
	for _, source := range sources {
		data, err := os.ReadFile(source.path) //nolint:gosec // path is constrained to test or sysfs root
		if err == nil {
			serial := printableIdentifier(data)
			if source.vpd {
				serial = printableSCSIVPDIdentifier(data)
			}
			if serial != "" {
				return serial, nil
			}
		}
	}
	return "", fmt.Errorf("serial not found for %s", name)
}

func printableSCSIVPDIdentifier(data []byte) string {
	if len(data) < 4 {
		return printableIdentifier(data)
	}

	pageCode := data[1]
	pageLength := int(binary.BigEndian.Uint16(data[2:4]))
	if pageLength <= 0 || pageLength > len(data)-4 {
		return printableIdentifier(data)
	}

	body := data[4 : 4+pageLength]
	switch pageCode {
	case 0x80:
		return printableIdentifier(body)
	case 0x83:
		for len(body) >= 4 {
			codeSet := body[0] & 0x0f
			identifierLength := int(body[3])
			if identifierLength <= 0 || identifierLength > len(body)-4 {
				break
			}
			identifier := body[4 : 4+identifierLength]
			if codeSet == 2 || codeSet == 3 {
				if value := printableIdentifier(identifier); value != "" {
					return value
				}
			}
			body = body[4+identifierLength:]
		}
	}

	return printableIdentifier(data)
}

func printableIdentifier(data []byte) string {
	value := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, string(data))
	return strings.TrimSpace(value)
}

// ParsePartitions reads the partition table using sfdisk --json.
//
// sfdisk may emit non-JSON warnings (e.g. "GPT PMBR size mismatch") on
// stderr which CombinedOutput merges before the JSON body. We strip
// everything before the first '{' so the JSON decoder sees clean input.
func (m *Manager) ParsePartitions(ctx context.Context, disk string) ([]Partition, error) {
	out, err := m.cmd.Run(ctx, "sfdisk", "--json", disk)
	if err != nil {
		// A disk with no partition table is not an error — return empty list
		// so the caller can decide what to do (e.g. create partitions).
		if strings.Contains(string(out), "does not contain a recognized partition table") {
			slog.Info("disk has no partition table", "disk", disk)
			return nil, nil
		}
		return nil, fmt.Errorf("sfdisk %s: %w", disk, err)
	}

	jsonBytes := stripNonJSONPrefix(out)

	var result sfdiskOutput
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		raw := strings.Join(strings.Fields(string(out)), " ")
		if len(raw) > 512 {
			raw = raw[:512] + "...(truncated)"
		}
		return nil, fmt.Errorf("parsing sfdisk output: %w [raw: %s]", err, raw)
	}
	return result.PartitionTable.Partitions, nil
}

// stripNonJSONPrefix returns the slice starting at the first '{'.
// sfdisk --json may prepend stderr warnings (e.g. GPT geometry messages)
// when captured via CombinedOutput.
func stripNonJSONPrefix(b []byte) []byte {
	if idx := bytes.IndexByte(b, '{'); idx > 0 {
		return b[idx:]
	}
	return b
}

// FindBootPartition finds the EFI System Partition.
func (m *Manager) FindBootPartition(parts []Partition) (*Partition, error) {
	for i := range parts {
		if strings.EqualFold(parts[i].Type, EFISystemPartitionGUID) {
			return &parts[i], nil
		}
	}
	return nil, fmt.Errorf("no EFI system partition found")
}

// ErrAmbiguousRootPartition reports several Linux filesystem partitions
// without exactly one named "root" or "/", so the partition table alone
// cannot tell which one is root.
var ErrAmbiguousRootPartition = errors.New("ambiguous Linux root partition candidates")

// FindRootPartition finds the primary Linux filesystem partition.
// When multiple partitions share a Linux filesystem type, it requires exactly
// one partition named "root" or "/" to avoid selecting data/state partitions.
func (m *Manager) FindRootPartition(parts []Partition) (*Partition, error) {
	var candidates []*Partition
	var namedRoot []*Partition
	for i := range parts {
		if !isLinuxFilesystemPartitionType(parts[i].Type) {
			continue
		}
		candidates = append(candidates, &parts[i])
		lower := strings.ToLower(strings.TrimSpace(parts[i].Name))
		if lower == "root" || lower == "/" {
			namedRoot = append(namedRoot, &parts[i])
		}
	}
	switch {
	case len(namedRoot) == 1:
		return namedRoot[0], nil
	case len(namedRoot) > 1:
		return nil, fmt.Errorf("ambiguous Linux root partitions: multiple partitions named root or /: %s", partitionCandidateList(namedRoot))
	case len(candidates) == 1:
		return candidates[0], nil
	case len(candidates) > 1:
		return nil, fmt.Errorf("%w: %s; name exactly one partition root or /", ErrAmbiguousRootPartition, partitionCandidateList(candidates))
	default:
		return nil, fmt.Errorf("no Linux filesystem partition found")
	}
}

func isLinuxFilesystemPartitionType(partitionType string) bool {
	return strings.EqualFold(partitionType, LinuxFilesystemGUID) ||
		strings.EqualFold(partitionType, LinuxFilesystemMBRType)
}

func partitionCandidateList(parts []*Partition) string {
	candidates := make([]string, 0, len(parts))
	for _, part := range parts {
		label := strings.TrimSpace(part.Name)
		if label == "" {
			candidates = append(candidates, part.Node)
			continue
		}
		candidates = append(candidates, fmt.Sprintf("%s (%s)", part.Node, label))
	}
	return strings.Join(candidates, ", ")
}

// GrowPartition grows a partition to fill available space using growpart.
func (m *Manager) GrowPartition(ctx context.Context, disk string, partNum int) error {
	slog.Info("growing partition", "disk", disk, "partition", partNum)
	out, err := m.cmd.Run(ctx, "growpart", "--update", "on", disk, strconv.Itoa(partNum))
	if err != nil {
		// growpart exits 1 if partition already fills the disk.
		if strings.Contains(string(out), "NOCHANGE") {
			slog.Info("partition already at max size")
			return nil
		}
		return fmt.Errorf("growpart %s %d: %s: %w", disk, partNum, string(out), err)
	}
	return nil
}

// ResizeFilesystem resizes the filesystem on the given device.
// Supports ext2/3/4 via the block device and XFS/btrfs via the mounted path.
func (m *Manager) ResizeFilesystem(ctx context.Context, device, mountpoint string) error {
	slog.Info("resizing filesystem", "device", device, "mountpoint", mountpoint)
	fsType, fsErr := m.fsType(ctx, device)
	if fsErr != nil {
		slog.Warn("failed to detect filesystem type before resize, falling back to resize tools", "device", device, "error", fsErr)
	}
	if fsErr == nil && fsType == "vfat" {
		return fmt.Errorf("resize filesystem %s: vfat resize is not supported; vfat is supported for ESP mount/format only", device)
	}
	// With a known type, run only its tool so its error is the one reported.
	if fsErr == nil {
		if handled, err := m.resizeKnownFilesystem(ctx, fsType, device, mountpoint); handled {
			return err
		}
	}
	// Unknown type: try the tools in turn.
	// Try resize2fs for ext2/3/4 first.
	if out, err := m.cmd.Run(ctx, "resize2fs", device); err != nil {
		slog.Debug("resize2fs failed, trying xfs_growfs", "output", string(out))
		if strings.TrimSpace(mountpoint) == "" {
			return fmt.Errorf("resize filesystem %s requires mountpoint for xfs/btrfs fallback", device)
		}
		// Fall back to xfs_growfs.
		if out, err := m.cmd.Run(ctx, "xfs_growfs", mountpoint); err != nil {
			slog.Debug("xfs_growfs failed, trying btrfs resize", "output", string(out))
			// Fall back to btrfs filesystem resize.
			if out, err := m.cmd.Run(ctx, "btrfs", "filesystem", "resize", "max", mountpoint); err != nil {
				return fmt.Errorf("resize filesystem %s at %s: %s: %w", device, mountpoint, string(out), err)
			}
		}
	}
	return nil
}

// PartProbe re-reads partition table and triggers device node creation.
//
// Calls partprobe first; falls back to blockdev --rereadpt if partprobe fails.
// After re-reading, runs mdev -s (busybox mini-udev) to ensure partition
// device nodes are created — devtmpfs alone may not create them in minimal
// initramfs environments without udevd.
func (m *Manager) PartProbe(ctx context.Context, disk string) error {
	slog.Info("re-reading partition table", "disk", disk)

	// Flush all pending writes to the block device backend before re-reading
	// the partition table. On QEMU/KVM, BLKRRPART may invalidate the page
	// cache and read stale data from the virtual disk if writeback is pending.
	if syncOut, syncErr := m.cmd.Run(ctx, "sync"); syncErr != nil {
		slog.Warn("sync before partprobe failed", "error", syncErr, "output", string(syncOut))
	}

	// Diagnostic: show device state before partprobe.
	logDeviceState(disk, "before-partprobe")

	out, err := m.cmd.Run(ctx, "partprobe", disk)
	if err != nil {
		slog.Warn("partprobe failed, trying blockdev --rereadpt", "disk", disk, "error", err, "output", string(out))
		out, err = m.cmd.Run(ctx, "blockdev", "--rereadpt", disk)
		if err != nil {
			return fmt.Errorf("re-read partition table %s: %s: %w", disk, string(out), err)
		}
		slog.Info("blockdev --rereadpt succeeded", "disk", disk)
	}

	// Brief settle time for the kernel to populate /sys/block/ partition entries
	// after BLKRRPART ioctl. On QEMU/KVM virtio disks the partition scan can be
	// asynchronous and /sys entries may not exist immediately.
	time.Sleep(200 * time.Millisecond)

	// Diagnostic: show device state after partprobe + settle.
	logDeviceState(disk, "after-partprobe")

	// Trigger device node creation for partitions. In initramfs environments
	// without udevd, devtmpfs may not auto-create partition nodes after the
	// kernel re-reads the partition table. mdev -s scans /sys and creates
	// missing device nodes.
	if mdevOut, mdevErr := m.cmd.Run(ctx, "mdev", "-s"); mdevErr != nil {
		slog.Warn("mdev -s failed", "error", mdevErr, "output", string(mdevOut))
	}

	// Diagnostic: show device state after mdev -s.
	logDeviceState(disk, "after-mdev")
	return nil
}

// logDeviceState logs /dev/sd*, /sys/block/ entries, and /proc/partitions
// for debugging partition node creation in initramfs environments.
func logDeviceState(disk, label string) {
	// List /dev/sd* (or /dev/vd* etc) device nodes.
	base := filepath.Dir(disk) // /dev
	prefix := filepath.Base(disk)
	devNodes := listGlob(filepath.Join(base, prefix+"*"))
	slog.Info("device state", "label", label, "dev_nodes", devNodes)

	// List /sys/block/<disk>/ partition entries.
	sysBase := filepath.Base(disk)
	sysDir := "/sys/block/" + sysBase
	sysEntries := listDir(sysDir)
	slog.Info("device state", "label", label, "sys_entries", sysEntries)

	// Read /proc/partitions for the disk.
	if data, err := os.ReadFile("/proc/partitions"); err != nil {
		slog.Warn("device state: cannot read /proc/partitions", "label", label, "error", err)
	} else {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		var relevant []string
		for _, line := range lines {
			if strings.Contains(line, sysBase) || strings.HasPrefix(line, "major") {
				relevant = append(relevant, strings.TrimSpace(line))
			}
		}
		slog.Info("device state", "label", label, "proc_partitions", relevant)
	}
}

func listGlob(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	return matches
}

func listDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// supportedFilesystems lists filesystem types to try when mounting, in priority order.
var supportedFilesystems = []string{"ext4", "xfs", "btrfs", "ext3", "ext2", "vfat"}

// noRecoveryData marks a read-only inspection mount that must not replay a
// filesystem journal. mountData maps it to the option of each filesystem.
const noRecoveryData = "ro,norecovery"

func mountData(fsType, data string) string {
	if data != noRecoveryData {
		return data
	}
	switch fsType {
	case "ext4", "ext3":
		return "ro,noload"
	case "xfs":
		return "ro,norecovery"
	case "btrfs":
		return "ro,rescue=nologreplay"
	default:
		return "ro"
	}
}

// MountPartitionInspect mounts a device read-only without journal replay, so
// inspecting a dirty filesystem does not write to it.
func (m *Manager) MountPartitionInspect(ctx context.Context, device, mountpoint string) error {
	return m.mountPartition(ctx, device, mountpoint, syscall.MS_RDONLY, noRecoveryData)
}

// MountPartition mounts a device at the given mountpoint.
// Tries all supported filesystem types in order: ext4, xfs, btrfs, ext3, ext2, vfat.
// Waits up to 10 seconds for the device node to appear (devtmpfs may lag after partprobe).
func (m *Manager) MountPartition(ctx context.Context, device, mountpoint string) error {
	return m.mountPartition(ctx, device, mountpoint, 0, "")
}

// MountPartitionReadOnly mounts a device read-only at the given mountpoint.
// It tries the same supported filesystem list as MountPartition.
func (m *Manager) MountPartitionReadOnly(ctx context.Context, device, mountpoint string) error {
	return m.mountPartition(ctx, device, mountpoint, syscall.MS_RDONLY, "ro")
}

func (m *Manager) mountPartition(ctx context.Context, device, mountpoint string, flags uintptr, data string) error {
	slog.Info("mounting partition", "device", device, "mountpoint", mountpoint)

	// Wait for the device node to appear — devtmpfs can lag after partprobe.
	if err := waitForDevice(ctx, device); err != nil {
		return fmt.Errorf("device %s not available: %w", device, err)
	}

	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("creating mountpoint %s: %w", mountpoint, err)
	}
	var errs []string
	for _, fsType := range supportedFilesystems {
		err := syscall.Mount(device, mountpoint, fsType, flags, mountData(fsType, data))
		if err == nil {
			slog.Info("mounted partition", "device", device, "fsType", fsType)
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s=%v", fsType, err))
	}
	return fmt.Errorf("mounting %s at %s: tried %s", device, mountpoint, strings.Join(errs, ", "))
}

// waitForDevice polls for a device node to appear, retrying up to 10 seconds.
// On each iteration, runs mdev -s to trigger device node creation in initramfs
// environments without udevd, where the kernel may update /sys/block/ partition
// entries asynchronously after BLKRRPART.
// Respects context cancellation for clean shutdown.
func waitForDevice(ctx context.Context, device string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for i := range 20 {
		if _, err := os.Stat(device); err == nil {
			slog.Info("device appeared", "device", device, "iteration", i)
			return nil
		}
		// Trigger device node creation from /sys entries that may have appeared
		// since the last poll. mdev -s is fast (~10ms) and idempotent.
		//nolint:gosec // mdev is a fixed busybox command, no user input
		if err := executil.CommandContext(ctx, "mdev", "-s").Run(); err != nil {
			slog.Debug("mdev -s failed during device wait", "device", device, "error", err)
		}
		if _, err := os.Stat(device); err == nil {
			slog.Info("device appeared after mdev", "device", device, "iteration", i)
			return nil
		}
		if i == 0 || i == 9 || i == 19 {
			// Log device state on first, middle, and last iteration.
			disk := strings.TrimRight(device, "0123456789")
			logDeviceState(disk, fmt.Sprintf("waitForDevice-iter%d", i))
		}
		slog.Debug("waiting for device node", "device", device, "iteration", i)
		select {
		case <-ctx.Done():
			return fmt.Errorf("device %s wait canceled: %w", device, ctx.Err())
		case <-ticker.C:
		}
	}
	return fmt.Errorf("device %s did not appear after 10s", device)
}

// BindMount performs a bind mount.
func (m *Manager) BindMount(source, target string) error {
	_, err := m.bindMount(source, target)
	return err
}

func (m *Manager) bindMount(source, target string) (bool, error) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return false, fmt.Errorf("creating bind mount target %s: %w", target, err)
	}
	if isMountPointFunc(target) {
		slog.Info("bind mount target already mounted, skipping", "source", source, "target", target)
		return false, nil
	}
	if err := mountFunc(source, target, "", syscall.MS_BIND, ""); err != nil {
		return false, fmt.Errorf("bind mount %s -> %s: %w", source, target, err)
	}
	return true, nil
}

func isMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

// SetUnmountFuncForTest replaces the unmount syscall and returns a function
// that restores it. Tests only.
func SetUnmountFuncForTest(fn func(target string, flags int) error) func() {
	old := unmountFunc
	unmountFunc = fn
	return func() { unmountFunc = old }
}

// Unmount unmounts a filesystem.
func (m *Manager) Unmount(target string) error {
	if err := unmountFunc(target, 0); err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

// UnmountRecursive unmounts root and any mounted children below it, deepest
// paths first. Missing mount entries are treated as already clean.
func (m *Manager) UnmountRecursive(root string) error {
	targets, err := mountedSubpaths(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, target := range targets {
		if err := m.Unmount(target); err != nil {
			if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func mountedSubpaths(root string) ([]string, error) {
	data, err := readMountsFile("/proc/mounts")
	if err != nil {
		return nil, fmt.Errorf("read mounts: %w", err)
	}
	return mountedSubpathsFrom(root, data)
}

func mountedSubpathsFrom(root string, data []byte) ([]string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == string(os.PathSeparator) || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("refusing unsafe recursive unmount root %q", root)
	}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		target := fields[1]
		if target == root || strings.HasPrefix(target, root+"/") {
			seen[target] = struct{}{}
		}
	}
	targets := make([]string, 0, len(seen))
	for target := range seen {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		return len(targets[i]) > len(targets[j])
	})
	return targets, nil
}

// exitCodeFromError extracts the process exit code from an error chain.
// Returns -1 if no exit code is found.
func exitCodeFromError(err error) int {
	var exitErr *executil.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// CheckFilesystem runs e2fsck on the device.
// Exit codes 0 (clean) and 1 (errors corrected) are acceptable.
// Exit code >= 4 indicates uncorrectable errors and returns an error.
func (m *Manager) CheckFilesystem(ctx context.Context, device string) error {
	slog.Info("checking filesystem", "device", device)
	fsType, fsErr := m.fsType(ctx, device)
	if fsErr != nil {
		slog.Debug("failed to detect filesystem type, falling back to ext check", "device", device, "error", fsErr)
	}

	switch fsType {
	case "xfs":
		out, err := m.cmd.Run(ctx, "xfs_repair", "-n", device)
		if err != nil {
			return fmt.Errorf("xfs_repair check failed on %s: %s: %w", device, strings.TrimSpace(string(out)), err)
		}
		return nil
	case "btrfs":
		out, err := m.cmd.Run(ctx, "btrfs", "check", "--readonly", device)
		if err != nil {
			return fmt.Errorf("btrfs check failed on %s: %s: %w", device, strings.TrimSpace(string(out)), err)
		}
		return nil
	case "vfat":
		return fmt.Errorf("check filesystem %s: vfat fsck is not supported; vfat is supported for ESP mount/format only", device)
	}

	return m.runE2fsck(ctx, device, "-fy")
}

// CheckExtFilesystemPreen runs `e2fsck -p -f`, like the CAPRF legacy
// provision.sh. Preen mode repairs only what is safe without questions and
// fails on other errors.
func (m *Manager) CheckExtFilesystemPreen(ctx context.Context, device string) error {
	slog.Info("checking filesystem in preen mode", "device", device)
	return m.runE2fsck(ctx, device, "-p", "-f")
}

func (m *Manager) runE2fsck(ctx context.Context, device string, flags ...string) error {
	out, err := m.cmd.Run(ctx, "e2fsck", append(flags, device)...)
	if err != nil {
		if errors.Is(err, executil.ErrNotFound) {
			slog.Warn("e2fsck not found, skipping filesystem check", "device", device)
			return nil
		}
		exitCode := exitCodeFromError(err)
		if exitCode < 0 {
			return fmt.Errorf("e2fsck did not complete on %s: %w", device, err)
		}
		if exitCode >= 4 {
			return fmt.Errorf("e2fsck: uncorrectable filesystem errors on %s (exit %d): %w", device, exitCode, err)
		}
		slog.Warn("e2fsck returned non-zero (errors corrected)", "device", device, "exit_code", exitCode, "output", string(out))
	}
	return nil
}

func (m *Manager) fsType(ctx context.Context, device string) (string, error) {
	out, err := m.cmd.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(string(out))), nil
}

// FilesystemType returns the filesystem type reported by blkid for device.
func (m *Manager) FilesystemType(ctx context.Context, device string) (string, error) {
	return m.fsType(ctx, device)
}

// DisableLVM deactivates LVM volume groups before disk wipe.
func (m *Manager) DisableLVM(ctx context.Context) error {
	slog.Info("deactivating LVM volume groups")
	out, err := m.cmd.Run(ctx, "lvm", "vgchange", "-an")
	if err != nil {
		if isExecNotFound(err) {
			slog.Debug("lvm binary not found, skipping deactivation", "error", err)
			return nil
		}
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("lvm vgchange: %s: %w", trimmed, err)
		}
		return fmt.Errorf("lvm vgchange: %w", err)
	}
	return nil
}

// EnableLVM activates LVM volume groups.
func (m *Manager) EnableLVM(ctx context.Context) error {
	slog.Info("activating LVM volume groups")
	out, err := m.cmd.Run(ctx, "lvm", "vgchange", "-ay")
	if err != nil {
		if isExecNotFound(err) {
			slog.Debug("lvm binary not found, skipping activation", "error", err)
			return nil
		}
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("lvm vgchange: %s: %w", trimmed, err)
		}
		return fmt.Errorf("lvm vgchange: %w", err)
	}
	return nil
}

// ChrootRun executes a command in a chroot environment.
// When using a mock commander (tests), the command is routed through the commander.
// For real execution, the external chroot binary is used. If the chroot binary
// is not found (e.g., in minimal initramfs), falls back to syscall-based chroot.
func (m *Manager) ChrootRun(ctx context.Context, root, command string) ([]byte, error) {
	out, err := m.cmd.Run(ctx, "chroot", root, "/bin/bash", "-c", command)
	if err != nil {
		// If chroot binary itself is missing, fall back to syscall-based chroot.
		if isExecNotFound(err) {
			slog.Info("chroot binary not found, using syscall fallback", "root", root)
			return m.chrootSyscall(ctx, root, command)
		}
		// If /bin/bash is missing or unusable inside the chroot target, try /bin/sh.
		if isBashNotFound(err) {
			slog.Info("bash unavailable in chroot, trying /bin/sh", "root", root)
			return m.cmd.Run(ctx, "chroot", root, "/bin/sh", "-c", command)
		}
		return out, fmt.Errorf("chroot exec in %s: %w", root, err)
	}
	return out, nil
}

// chrootSyscall runs a command using SysProcAttr.Chroot instead of the chroot binary.
func (m *Manager) chrootSyscall(ctx context.Context, root, command string) ([]byte, error) {
	shells := []string{"/bin/bash", "/bin/sh"}
	for i, shell := range shells {
		out, err := runChrootSyscallCommand(ctx, root, shell, command)
		if err == nil {
			return out, nil
		}
		if i == 0 && isShellNotFound(err, shell) {
			slog.Info("bash unavailable in syscall chroot, trying /bin/sh", "root", root)
			continue
		}
		return out, formatChrootSyscallError(root, shell, out, err)
	}
	return nil, fmt.Errorf("chroot syscall in %s: no shell configured", root)
}

func runChrootSyscallExec(ctx context.Context, root, shell, command string) ([]byte, error) {
	cmd := executil.CommandContext(ctx, shell, "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("run %s in chroot %s: %w", shell, root, err)
	}
	return out, nil
}

func formatChrootSyscallError(root, shell string, out []byte, err error) error {
	raw := strings.TrimSpace(string(out))
	if len(raw) > 512 {
		raw = raw[:512] + "...(truncated)"
	}
	raw = strings.NewReplacer("\n", " ", "\r", " ").Replace(raw)
	if raw != "" {
		return fmt.Errorf("chroot syscall %s in %s: %w [output: %s]", shell, root, err, raw)
	}
	return fmt.Errorf("chroot syscall %s in %s: %w", shell, root, err)
}

// isExecNotFound checks whether an error indicates the executable was not found.
func isExecNotFound(err error) bool {
	if errors.Is(err, executil.ErrNotFound) {
		return true
	}
	// The Commander wraps errors, so also check the message.
	// Some environments report "command not found" instead of "executable file not found".
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		strings.Contains(msg, "command not found")
}

// isBashNotFound checks whether /bin/bash is unavailable inside a chroot target.
func isBashNotFound(err error) bool {
	return isShellNotFound(err, "/bin/bash")
}

func isShellNotFound(err error, shell string) bool {
	msg := err.Error()
	if strings.Contains(msg, shell) &&
		(strings.Contains(msg, "No such file or directory") ||
			strings.Contains(msg, "no such file or directory") ||
			strings.Contains(msg, "executable file not found")) {
		return true
	}
	if !strings.Contains(msg, "exit status 127") {
		return false
	}
	return strings.Contains(msg, "No such file or directory") ||
		strings.Contains(msg, "bash: applet not found")
}

// SetupChrootBindMounts creates standard bind mounts for chroot operations.
func (m *Manager) SetupChrootBindMounts(root string) error {
	binds := []struct{ src, rel string }{
		{"/dev", "dev"},
		{"/proc", "proc"},
		{"/sys", "sys"},
		{"/run", "run"},
	}
	var mounted []string
	for _, b := range binds {
		target := root + "/" + b.rel
		didMount, err := m.bindMount(b.src, target)
		if err != nil {
			m.rollbackChrootBindMounts(root, mounted)
			return fmt.Errorf("bind mount %s: %w", b.src, err)
		}
		if didMount {
			mounted = append(mounted, b.rel)
		}
	}

	// MS_BIND is non-recursive — efivarfs (under /sys) is not propagated.
	// Bind it separately so chroot efibootmgr can access EFI variables.
	efiSrc := "/sys/firmware/efi/efivars"
	efiDst := root + "/sys/firmware/efi/efivars"
	if _, err := os.Stat(efiSrc); err == nil {
		if err := os.MkdirAll(efiDst, 0o755); err != nil {
			slog.Debug("cannot create efivarfs mount target", "path", efiDst, "error", err)
		} else if err := m.BindMount(efiSrc, efiDst); err != nil {
			slog.Debug("cannot bind-mount efivarfs into chroot", "error", err)
		}
	}
	return nil
}

func (m *Manager) rollbackChrootBindMounts(root string, mounted []string) {
	for i := len(mounted) - 1; i >= 0; i-- {
		path := root + "/" + mounted[i]
		if err := m.Unmount(path); err != nil {
			slog.Warn("chroot bind rollback failed", "path", path, "error", err)
		}
	}
}

// TeardownChrootBindMounts unmounts the standard chroot bind mounts and
// returns a joined error from the mandatory unmounts (/run, /sys, /proc,
// /dev). The efivarfs unmount is best-effort because efivarfs is only
// bind-mounted when /sys/firmware/efi/efivars exists on the host; failures
// there are logged at Debug level and never returned.
func (m *Manager) TeardownChrootBindMounts(root string) error {
	var errs []error
	// efivarfs is best-effort: it may legitimately not have been mounted.
	efiPath := root + "/sys/firmware/efi/efivars"
	if err := m.Unmount(efiPath); err != nil {
		slog.Debug("chroot efivarfs unmount skipped", "path", efiPath, "error", err)
	}
	for _, rel := range []string{"run", "sys", "proc", "dev"} {
		if err := m.Unmount(root + "/" + rel); err != nil {
			slog.Warn("chroot unmount failed", "path", root+"/"+rel, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
