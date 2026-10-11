//go:build linux

package realm

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// RequestReboot asks the kernel to reboot and returns only if the request did
// not take over the machine. BOOTY_NO_REBOOT suppresses the syscall for test
// containers and returns nil.
func RequestReboot() error {
	if os.Getenv("BOOTY_NO_REBOOT") != "" {
		slog.Info("reboot suppressed (BOOTY_NO_REBOOT set)")
		return nil
	}
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}

// RequestPowerOff asks the kernel to power off and returns only if the request
// did not take over the machine.
func RequestPowerOff() error {
	// reboot(2) does not flush dirty pages. Sync first like shutdown does.
	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		return fmt.Errorf("power off: %w", err)
	}
	return nil
}

// Reboot a host. When the BOOTY_NO_REBOOT environment variable is set,
// the process exits instead of issuing a reboot syscall (used in test containers).
func Reboot() {
	if err := RequestReboot(); err != nil {
		slog.Error("reboot failed", "error", err)
		Shell()
		os.Exit(1)
	}
	os.Exit(0)
}

// PowerOff will result in the host using an ACPI power off.
func PowerOff() {
	if err := RequestPowerOff(); err != nil {
		slog.Error("power off failed", "error", err)
		Shell()
		os.Exit(1)
	}
	os.Exit(0)
}

// Halt will instruct the CPU to enter a halt state.
func Halt() {
	err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_HALT)
	if err != nil {
		slog.Error("halt failed", "error", err)
		Shell()
	}
	os.Exit(1)
}

// Suspend will instruct the CPU to enter a suspended state.
func Suspend() {
	err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_SW_SUSPEND)
	if err != nil {
		slog.Error("suspend failed", "error", err)
		Shell()
		slog.Warn("attempting a reboot")
		Reboot()
	}
}
