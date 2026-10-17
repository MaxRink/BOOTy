//go:build linux

package provision

import (
	"context"
	"errors"

	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/image"
)

// FailureClass groups failure codes by their likely cause.
type FailureClass string

// Failure classes.
const (
	ClassTransient   FailureClass = "transient"
	ClassPermanent   FailureClass = "permanent"
	ClassConfig      FailureClass = "config"
	ClassHardware    FailureClass = "hardware"
	ClassEnvironment FailureClass = "environment"
)

// FailureCode is a stable code BOOTY-<AREA>-<NNN> for a failure. A code
// never changes its meaning. A retired code stays in the table, so nobody
// uses it again.
type FailureCode struct {
	Code  string
	Class FailureClass
	// Step is the step or run phase that reports the code. AnyStep means
	// that every step can report it.
	Step string
	Text string
}

// AnyStep marks a code that every step can report.
const AnyStep = "*"

// failureCodes is the code catalog. docs/failure-codes.md is generated from
// it (go test ./pkg/provision -run TestFailureCodeCatalogDoc -update).
var failureCodes = []FailureCode{
	{"BOOTY-RUN-001", ClassPermanent, AnyStep, "Step failed with an error that has no specific code."},
	{"BOOTY-RUN-002", ClassEnvironment, AnyStep, "Run canceled or deadline exceeded."},

	{"BOOTY-NET-001", ClassEnvironment, "network-setup", "Network setup failed."},
	{"BOOTY-NET-002", ClassEnvironment, "network-setup", "Connectivity check to the provisioning server failed."},
	{"BOOTY-NET-003", ClassEnvironment, "network-setup", "Token exchange or token renewal start failed."},

	{"BOOTY-RPT-001", ClassTransient, "report-init", "Init callback failed."},
	{"BOOTY-RPT-002", ClassTransient, "report-success", "Success callback failed."},

	{"BOOTY-HW-001", ClassHardware, "collect-inventory", "Hardware inventory collection failed."},
	{"BOOTY-HW-002", ClassHardware, "collect-firmware", "Firmware collection failed."},
	{"BOOTY-HW-003", ClassHardware, "health-checks", "A health check failed."},
	{"BOOTY-HW-004", ClassHardware, "setup-mellanox", "Mellanox NIC setup failed."},
	{"BOOTY-HW-005", ClassHardware, "configure-sriov", "SR-IOV profile check or apply failed."},

	{"BOOTY-CFG-001", ClassConfig, "validate-provision-inputs", "Provisioning inputs are invalid."},
	{"BOOTY-CFG-002", ClassConfig, "config", "The vars file or the configuration is invalid."},

	{"BOOTY-IMG-001", ClassPermanent, "verify-image", "Image signature verification failed."},
	{"BOOTY-IMG-002", ClassTransient, "stream-image", "Image download or write failed."},
	{"BOOTY-IMG-003", ClassPermanent, AnyStep, "Image checksum mismatch."},
	{"BOOTY-IMG-004", ClassPermanent, AnyStep, "Image format probe failed."},
	{"BOOTY-IMG-005", ClassTransient, "apply-sysexts", "System extension download or install failed."},
	{"BOOTY-IMG-006", ClassTransient, "prepare-oci-prepulls", "OCI image pre-pull preparation failed."},

	{"BOOTY-DSK-001", ClassEnvironment, "release-stale-mounts", "Stale target mounts could not be released."},
	{"BOOTY-DSK-002", ClassHardware, "disable-lvm", "Disabling LVM failed."},
	{"BOOTY-DSK-003", ClassHardware, "stop-raid", "Stopping RAID arrays failed."},
	{"BOOTY-DSK-004", ClassHardware, "setup-nvme-namespaces", "NVMe namespace setup failed."},
	{"BOOTY-DSK-005", ClassHardware, "setup-raid", "RAID setup failed."},
	{"BOOTY-DSK-006", ClassHardware, "detect-disk", "No usable target disk found."},
	{"BOOTY-DSK-007", ClassHardware, "wipe-disks", "Disk wipe or secure erase failed."},
	{"BOOTY-DSK-008", ClassHardware, "apply-partition-layout", "Applying the partition layout failed."},
	{"BOOTY-DSK-009", ClassHardware, "partprobe", "Partition table re-read failed."},
	{"BOOTY-DSK-010", ClassEnvironment, AnyStep, "Target disk in use, or the in-use check failed."},
	{"BOOTY-DSK-011", ClassPermanent, "parse-partitions", "Partitions of the target disk not found."},
	{"BOOTY-DSK-012", ClassPermanent, AnyStep, "Ambiguous root partition candidates."},
	{"BOOTY-DSK-013", ClassHardware, "mirror-root", "Root mirror setup failed."},
	{"BOOTY-DSK-014", ClassHardware, "check-filesystem", "File system check failed."},
	{"BOOTY-DSK-015", ClassHardware, "enable-lvm", "Enabling LVM failed."},
	{"BOOTY-DSK-016", ClassHardware, "mount-root", "Mounting the root file system failed."},
	{"BOOTY-DSK-017", ClassHardware, "mount-boot", "Mounting the boot file system failed."},
	{"BOOTY-DSK-018", ClassHardware, "mount-shared-data", "Mounting the shared data file system failed."},
	{"BOOTY-DSK-019", ClassHardware, "grow-partition", "Growing the root partition failed."},
	{"BOOTY-DSK-020", ClassHardware, "resize-filesystem", "Resizing the root file system failed."},
	{"BOOTY-DSK-021", ClassConfig, "configure-mdadm", "Writing the mdadm configuration failed."},
	{"BOOTY-DSK-022", ClassConfig, "select-deprovision-disk", "Deprovision disk selection failed."},
	{"BOOTY-DSK-023", ClassHardware, "decommission-wipe", "Decommission disk wipe failed."},

	{"BOOTY-EFI-001", ClassEnvironment, "mount-efivarfs", "Mounting efivarfs failed."},
	{"BOOTY-EFI-002", ClassEnvironment, AnyStep, "EFI variables unavailable."},
	{"BOOTY-EFI-003", ClassHardware, "remove-efi-entries", "Removing EFI boot entries failed."},
	{"BOOTY-EFI-004", ClassHardware, "remove-all-efi-entries", "Removing all EFI boot entries failed."},
	{"BOOTY-EFI-005", ClassConfig, "install-esp-sync", "Installing ESP sync failed."},
	{"BOOTY-EFI-006", ClassConfig, "install-efi-fallback", "Installing the EFI fallback loader failed."},
	{"BOOTY-EFI-007", ClassPermanent, "verify-secureboot-chain", "Secure Boot chain verification failed."},
	{"BOOTY-EFI-008", ClassHardware, "create-efi-boot-entry", "Creating the EFI boot entry failed."},
	{"BOOTY-EFI-009", ClassHardware, "create-mirror-efi-boot-entry", "Creating the mirror EFI boot entry failed."},

	{"BOOTY-OS-001", ClassConfig, "configure-overlayfs", "Overlay file system configuration failed."},
	{"BOOTY-OS-002", ClassConfig, "set-hostname", "Setting the host name failed."},
	{"BOOTY-OS-003", ClassConfig, "copy-provisioner-files", "Copying provisioner files failed."},
	{"BOOTY-OS-004", ClassConfig, "configure-dns", "DNS configuration failed."},
	{"BOOTY-OS-005", ClassConfig, "write-fstab", "Writing fstab failed."},
	{"BOOTY-OS-006", ClassEnvironment, "setup-chroot-binds", "Chroot bind mounts failed."},
	{"BOOTY-OS-007", ClassConfig, "configure-kubelet", "Kubelet configuration failed."},
	{"BOOTY-OS-008", ClassConfig, "configure-serial-console", "Serial console configuration failed."},
	{"BOOTY-OS-009", ClassConfig, "configure-grub", "GRUB configuration failed."},
	{"BOOTY-OS-010", ClassConfig, "inject-cloudinit", "Cloud-init injection failed."},
	{"BOOTY-OS-011", ClassConfig, "copy-machine-files", "Copying machine files failed."},
	{"BOOTY-OS-012", ClassConfig, "run-machine-commands", "A machine command failed."},
	{"BOOTY-OS-013", ClassConfig, "run-post-provision-cmds", "A post-provision command failed."},
	{"BOOTY-OS-014", ClassEnvironment, "teardown-chroot", "Chroot teardown failed."},
	{"BOOTY-OS-015", ClassConfig, "soft-deprovision", "Soft deprovision failed."},
}

// Failure codes used outside the step table.
const (
	CodeGeneric          = "BOOTY-RUN-001"
	CodeCanceled         = "BOOTY-RUN-002"
	CodeNetworkSetup     = "BOOTY-NET-001"
	CodeConnectivity     = "BOOTY-NET-002"
	CodeAuth             = "BOOTY-NET-003"
	CodeConfig           = "BOOTY-CFG-002"
	codeChecksumMismatch = "BOOTY-IMG-003"
	codeFormatProbe      = "BOOTY-IMG-004"
	codeDiskInUse        = "BOOTY-DSK-010"
	codeAmbiguousRoot    = "BOOTY-DSK-012"
	codeEFIVars          = "BOOTY-EFI-002"
)

// sentinelCodes maps known errors to a specific code. The first match wins.
var sentinelCodes = []struct {
	err  error
	code string
}{
	{context.Canceled, CodeCanceled},
	{context.DeadlineExceeded, CodeCanceled},
	{image.ErrChecksumMismatch, codeChecksumMismatch},
	{image.ErrFormatProbe, codeFormatProbe},
	{disk.ErrAllDisksInUse, codeDiskInUse},
	{disk.ErrInUseCheck, codeDiskInUse},
	{disk.ErrAmbiguousRootPartition, codeAmbiguousRoot},
	{ErrEFIVarsUnavailable, codeEFIVars},
}

// FailureCodes returns a copy of the code catalog.
func FailureCodes() []FailureCode {
	return append([]FailureCode(nil), failureCodes...)
}

// CodedError attaches a failure code to an error. Error returns the text
// of the wrapped error only, so a message shows the code once.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }

func (e *CodedError) Unwrap() error { return e.Err }

// FailureCode returns the code. pkg/telemetry reads it through this method.
func (e *CodedError) FailureCode() string { return e.Code }

// WithCode attaches the failure code of step to err. A code that err
// already carries wins. It returns nil for a nil error.
func WithCode(step string, err error) error {
	if err == nil {
		return nil
	}
	var coded *CodedError
	if errors.As(err, &coded) {
		return err
	}
	return &CodedError{Code: codeFor(step, err), Err: err}
}

// CodeOf returns the failure code that err carries, or "".
func CodeOf(err error) string {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func codeFor(step string, err error) string {
	for _, s := range sentinelCodes {
		if errors.Is(err, s.err) {
			return s.code
		}
	}
	for _, c := range failureCodes {
		if c.Step == step {
			return c.Code
		}
	}
	return CodeGeneric
}
