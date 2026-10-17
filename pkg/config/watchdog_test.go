package config

import (
	"strings"
	"testing"
)

func TestWatchdogConfigDefaults(t *testing.T) {
	var w WatchdogConfig
	if w.Timeout() != DefaultWatchdogTimeoutSeconds || w.KeepOnHandoff() {
		t.Fatalf("defaults: timeout %d, keep %v", w.Timeout(), w.KeepOnHandoff())
	}
	w = WatchdogConfig{TimeoutSeconds: 60, Handoff: WatchdogHandoffKeep}
	if w.Timeout() != 60 || !w.KeepOnHandoff() {
		t.Fatalf("explicit: timeout %d, keep %v", w.Timeout(), w.KeepOnHandoff())
	}
}

func TestValidateWatchdog(t *testing.T) {
	tests := []struct {
		name string
		w    WatchdogConfig
		want string
	}{
		{"defaults", WatchdogConfig{}, ""},
		{"full", WatchdogConfig{Enable: true, TimeoutSeconds: 120, Device: "/dev/watchdog1", Handoff: "keep", IPMIFallback: true,
			StepBudgets: map[string]int{"wipe-disks": 86400}}, ""},
		{"timeout too short", WatchdogConfig{TimeoutSeconds: 5}, "WATCHDOG_TIMEOUT_SEC"},
		{"timeout too long", WatchdogConfig{TimeoutSeconds: 3601}, "WATCHDOG_TIMEOUT_SEC"},
		{"negative timeout", WatchdogConfig{TimeoutSeconds: -1}, "WATCHDOG_TIMEOUT_SEC"},
		{"handoff", WatchdogConfig{Handoff: "maybe"}, "must be stop or keep"},
		{"relative device", WatchdogConfig{Device: "watchdog0"}, "WATCHDOG_DEVICE"},
		{"device outside dev", WatchdogConfig{Device: "/tmp/watchdog"}, "WATCHDOG_DEVICE"},
		{"unclean device", WatchdogConfig{Device: "/dev/../etc/passwd"}, "WATCHDOG_DEVICE"},
		{"zero budget", WatchdogConfig{StepBudgets: map[string]int{"wipe-disks": 0}}, "between 1 and 86400"},
		{"budget above a day", WatchdogConfig{StepBudgets: map[string]int{"wipe-disks": 86401}}, "between 1 and 86400"},
		{"blank step", WatchdogConfig{StepBudgets: map[string]int{" ": 10}}, "invalid step name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := strings.Join(validateWatchdog(&tt.w), "; ")
			if tt.want == "" && errs != "" {
				t.Fatalf("validateWatchdog = %q, want no error", errs)
			}
			if !strings.Contains(errs, tt.want) {
				t.Fatalf("validateWatchdog = %q, want %q", errs, tt.want)
			}
		})
	}
}

func TestValidateRejectsBadWatchdogConfig(t *testing.T) {
	cfg := &Config{Watchdog: WatchdogConfig{Handoff: "later"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "WATCHDOG_HANDOFF") {
		t.Fatalf("Validate = %v, want a WATCHDOG_HANDOFF error", err)
	}
}
