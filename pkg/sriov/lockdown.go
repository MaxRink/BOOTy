package sriov

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// DefaultLockdownPath is the kernel lockdown state in securityfs.
const DefaultLockdownPath = "/sys/kernel/security/lockdown"

// ErrFirmwareLockdown is the coded reason when a NIC firmware change can
// only be made with mstconfig or mlxconfig while the kernel is locked
// down. Secure Boot puts the kernel in lockdown, which forbids the PCI
// config writes and BAR mappings these tools need; devlink and sysfs keep
// working.
var ErrFirmwareLockdown = errors.New("nic-firmware-lockdown: NIC firmware change needs Secure Boot off or an out-of-band path " +
	"(Redfish NetworkAdapter settings); the kernel lockdown blocks mstconfig")

var lockdownMode = regexp.MustCompile(`\[(\w+)\]`)

// kernelLockdown returns the active lockdown mode ("integrity" or
// "confidentiality"), or "" when the kernel is not locked down or the
// file is missing.
func kernelLockdown(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // securityfs path
	if err != nil {
		return ""
	}
	m := lockdownMode.FindStringSubmatch(strings.TrimSpace(string(data)))
	if len(m) < 2 || m[1] == "none" {
		return ""
	}
	return m[1]
}

// permissionDenied reports tool output of an access that the kernel
// refused with EPERM ("Operation not permitted"), which lockdown returns
// for PCI config writes and BAR mappings. EACCES ("Permission denied") is
// a plain file permission problem, for example not running as root, and
// is not reported as lockdown.
func permissionDenied(out []byte, err error) bool {
	return errors.Is(err, syscall.EPERM) || strings.Contains(strings.ToLower(string(out)), "operation not permitted")
}
