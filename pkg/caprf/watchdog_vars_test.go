package caprf

import (
	"strings"
	"testing"
)

func TestParseVarsWatchdogOptions(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader(`WATCHDOG_ENABLE=true
WATCHDOG_TIMEOUT_SEC=120
WATCHDOG_DEVICE=/dev/watchdog1
WATCHDOG_HANDOFF=Keep
WATCHDOG_IPMI_FALLBACK=yes
WATCHDOG_STEP_BUDGETS='{"wipe-disks":7200,"run-machine-commands":600}'
`))
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Watchdog
	if !w.Enable || w.TimeoutSeconds != 120 || w.Device != "/dev/watchdog1" || w.Handoff != "keep" || !w.IPMIFallback ||
		w.StepBudgets["wipe-disks"] != 7200 || w.StepBudgets["run-machine-commands"] != 600 {
		t.Fatalf("watchdog = %+v", w)
	}

	cfg, err = ParseVars(strings.NewReader("WATCHDOG_ENABLE=\nWATCHDOG_STEP_BUDGETS=\n"))
	if err != nil || cfg.Watchdog.Enable || cfg.Watchdog.StepBudgets != nil {
		t.Fatalf("empty values: %+v, %v", cfg.Watchdog, err)
	}

	for _, bad := range []string{
		"WATCHDOG_ENABLE=on",
		"WATCHDOG_IPMI_FALLBACK=maybe",
		"WATCHDOG_TIMEOUT_SEC=x",
		"WATCHDOG_TIMEOUT_SEC=5",
		"WATCHDOG_HANDOFF=never",
		"WATCHDOG_DEVICE=watchdog0",
		`WATCHDOG_STEP_BUDGETS='{"wipe-disks":"long"}'`,
		`WATCHDOG_STEP_BUDGETS='{"wipe-disks":0}'`,
		"WATCHDOG_STEP_BUDGETS=wipe-disks=60",
	} {
		if _, err := ParseVars(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
