package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// WatchdogConfig controls the hardware watchdog. BOOTy arms the watchdog at
// start and pets it while the run makes progress. A provisioning step that
// makes no progress for its budget stops the petting, and the hardware
// resets the host.
type WatchdogConfig struct {
	// Enable arms the hardware watchdog.
	// Default: false
	Enable bool `yaml:"enable" json:"enable"`

	// TimeoutSeconds is the hardware timeout. The driver may round it; BOOTy
	// uses the value that the driver reports.
	// Default: 0 (300)
	TimeoutSeconds int `yaml:"timeoutSeconds" json:"timeoutSeconds"`

	// Device is the watchdog device. Empty tries /dev/watchdog0, then
	// /dev/watchdog.
	// Default: ""
	Device string `yaml:"device" json:"device"`

	// Handoff is what BOOTy does with the watchdog at kexec and reboot.
	// "stop": disarm it with the magic close.
	// "keep": leave it armed, for a target OS that takes it over (systemd
	// RuntimeWatchdogSec). A power off always disarms it.
	// Valid values: "stop", "keep".
	// Default: "" (stop)
	Handoff string `yaml:"handoff" json:"handoff"`

	// IPMIFallback arms the BMC watchdog through IPMI (Set Watchdog Timer)
	// when no watchdog device exists. It needs /dev/ipmi0.
	// Default: false
	IPMIFallback bool `yaml:"ipmiFallback" json:"ipmiFallback"`

	// StepBudgets overrides how long a provisioning or deprovisioning step
	// may run without progress, in seconds, by step name. Steps without an
	// entry use the built-in budget of the step or TimeoutSeconds.
	// Default: none
	StepBudgets map[string]int `yaml:"stepBudgets" json:"stepBudgets"`
}

// Watchdog handoff actions.
const (
	WatchdogHandoffStop = "stop"
	WatchdogHandoffKeep = "keep"
)

const (
	// DefaultWatchdogTimeoutSeconds is the default of TimeoutSeconds.
	DefaultWatchdogTimeoutSeconds = 300
	// MinWatchdogTimeoutSeconds is the smallest TimeoutSeconds.
	MinWatchdogTimeoutSeconds = 10
	// MaxWatchdogTimeoutSeconds is the largest TimeoutSeconds.
	MaxWatchdogTimeoutSeconds = 3600
	// MaxWatchdogStepBudgetSeconds bounds a step budget (one day).
	MaxWatchdogStepBudgetSeconds = 86400
)

// Timeout returns TimeoutSeconds with the default applied.
func (w *WatchdogConfig) Timeout() int {
	if w.TimeoutSeconds == 0 {
		return DefaultWatchdogTimeoutSeconds
	}
	return w.TimeoutSeconds
}

// KeepOnHandoff reports whether the watchdog stays armed at kexec and
// reboot.
func (w *WatchdogConfig) KeepOnHandoff() bool {
	return w.Handoff == WatchdogHandoffKeep
}

// validateWatchdog checks the watchdog options.
func validateWatchdog(w *WatchdogConfig) []string {
	var errs []string
	if w.TimeoutSeconds != 0 && (w.TimeoutSeconds < MinWatchdogTimeoutSeconds || w.TimeoutSeconds > MaxWatchdogTimeoutSeconds) {
		errs = append(errs, fmt.Sprintf("watchdog.timeoutSeconds (WATCHDOG_TIMEOUT_SEC) must be between %d and %d, or 0 for the default %d",
			MinWatchdogTimeoutSeconds, MaxWatchdogTimeoutSeconds, DefaultWatchdogTimeoutSeconds))
	}
	switch w.Handoff {
	case "", WatchdogHandoffStop, WatchdogHandoffKeep:
	default:
		errs = append(errs, fmt.Sprintf("watchdog.handoff (WATCHDOG_HANDOFF) must be stop or keep, got %q", w.Handoff))
	}
	if w.Device != "" && (!filepath.IsAbs(w.Device) || filepath.Clean(w.Device) != w.Device || !strings.HasPrefix(w.Device, "/dev/")) {
		errs = append(errs, fmt.Sprintf("watchdog.device (WATCHDOG_DEVICE) must be a clean path under /dev/, got %q", w.Device))
	}
	return append(errs, validateWatchdogStepBudgets(w.StepBudgets)...)
}

// validateWatchdogStepBudgets checks WATCHDOG_STEP_BUDGETS, in step name
// order.
func validateWatchdogStepBudgets(budgets map[string]int) []string {
	var errs []string
	steps := make([]string, 0, len(budgets))
	for step := range budgets {
		steps = append(steps, step)
	}
	sort.Strings(steps)
	for _, step := range steps {
		budget := budgets[step]
		if strings.TrimSpace(step) == "" || strings.TrimSpace(step) != step {
			errs = append(errs, fmt.Sprintf("watchdog.stepBudgets (WATCHDOG_STEP_BUDGETS) has an invalid step name %q", step))
		}
		if budget < 1 || budget > MaxWatchdogStepBudgetSeconds {
			errs = append(errs, fmt.Sprintf("watchdog.stepBudgets (WATCHDOG_STEP_BUDGETS) %s must be between 1 and %d seconds", step, MaxWatchdogStepBudgetSeconds))
		}
	}
	return errs
}
