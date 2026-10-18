//go:build linux

package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/logging"
)

// readKernelLogFn is a test seam for readKernelLog.
var readKernelLogFn = readKernelLog

// readKernelLog returns the kernel ring buffer, like dmesg. The ISO boots
// with loglevel=4, so the console shows only kernel errors; the buffer
// keeps every message.
func readKernelLog() string {
	size, err := unix.Klogctl(unix.SYSLOG_ACTION_SIZE_BUFFER, nil)
	if err != nil || size <= 0 {
		return ""
	}
	buf := make([]byte, size)
	n, err := unix.Klogctl(unix.SYSLOG_ACTION_READ_ALL, buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

// kernelLogLine strips the "<6>[    0.123456] " prefix of a ring buffer line.
func kernelLogLine(line string) string {
	if strings.HasPrefix(line, "<") {
		if _, rest, ok := strings.Cut(line, ">"); ok {
			line = rest
		}
	}
	if strings.HasPrefix(line, "[") {
		if _, rest, ok := strings.Cut(line, "] "); ok {
			line = rest
		}
	}
	return line
}

// logSecureBootState logs the kernel's Secure Boot and lockdown messages
// ("secureboot: Secure boot enabled", "Kernel is locked down from EFI
// Secure Boot mode") in one line. loglevel=4 hides them from the console,
// and operators and the ISO tests read them there.
func logSecureBootState(klog string) {
	var found []string
	for line := range strings.SplitSeq(klog, "\n") {
		text := kernelLogLine(line)
		lower := strings.ToLower(text)
		if strings.Contains(lower, "secure boot") || strings.Contains(lower, "locked down") {
			found = append(found, text)
		}
	}
	if len(found) == 0 {
		slog.Info("kernel security state", "secureBoot", "not reported (BIOS boot or no EFI)")
		return
	}
	slog.Info("kernel security state", "kernel", strings.Join(found, "; "))
}

// uptimeSeconds returns the first field of /proc/uptime, the seconds since
// the kernel started, or "" when it cannot be read.
func uptimeSeconds() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return ""
	}
	up, _, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
	return up
}

// kernelLogShipper is the part of the CAPRF client that shipKernelLog uses.
type kernelLogShipper interface {
	ShipDebug(ctx context.Context, message string) error
}

// shipKernelLog sends the whole kernel log, redacted, as one message to
// DEBUG_URL when it is set. It never uses LOG_URL: CAPRF classifies each
// /log message, and kernel lines with "error" and "image" (the RAS
// "Correctable Errors" line, "Freeing unused kernel image", BOOT_IMAGE=)
// would raise an ImageError event that carries the whole log.
func shipKernelLog(ctx context.Context, client kernelLogShipper, debugURL string) {
	if debugURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.ShipDebug(ctx, logging.RedactText("kernel log\n"+readKernelLogFn())); err != nil {
		slog.Warn("kernel log not sent to DEBUG_URL", "reason", err.Error())
	}
}
