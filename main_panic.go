//go:build linux

package main

import (
	"log/slog"
	"os"
	"strings"
)

// panicRebootSeconds is the kernel.panic value BOOTy sets: a kernel panic,
// also the "Attempted to kill init" panic when BOOTy (PID 1) dies, reboots
// the host after 10 s instead of hanging. The Debian and CAPRF boot entries
// set no panic= (the kernel default 0 hangs forever), and without an armed
// watchdog a hung ramdisk needs a manual power cycle. The 10 s keep the
// panic on the serial console and BMC screen long enough to see. A reboot
// boots the ramdisk again, which BOOTy already does after a failed run.
const panicRebootSeconds = "10"

// Test seams.
var (
	procCmdlinePath = "/proc/cmdline"
	kernelPanicPath = "/proc/sys/kernel/panic"
	panicGetpid     = os.Getpid
)

// setPanicReboot sets kernel.panic unless the kernel command line has a
// panic= parameter, which wins (for example panic=0 to keep a panicked host
// for debugging). Only PID 1 sets it: kernel.panic is global for the host,
// and BOOTy also runs as a normal process (the containerlab e2e nodes), where
// it must not change the CI runner's or a laptop's setting.
func setPanicReboot() {
	if panicGetpid() != 1 {
		return
	}
	cmdline, err := os.ReadFile(procCmdlinePath)
	if err != nil {
		slog.Warn("kernel.panic not set: cannot read the kernel command line", "error", err)
		return
	}
	// Parameters after "--" go to init, not to the kernel.
	for _, p := range strings.Fields(string(cmdline)) {
		if p == "--" {
			break
		}
		if name, _, _ := strings.Cut(p, "="); name == "panic" {
			return
		}
	}
	if err := os.WriteFile(kernelPanicPath, []byte(panicRebootSeconds), 0o644); err != nil { //nolint:gosec // procfs file
		slog.Warn("kernel.panic not set", "error", err)
	}
}
