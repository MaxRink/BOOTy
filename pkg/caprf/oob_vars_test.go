package caprf

import (
	"strings"
	"testing"
)

func TestParseVarsOOBKeys(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader("OOB_STATUS=Off\nOOB_EFI_VAR=true\n"))
	if err != nil {
		t.Fatalf("ParseVars() = %v", err)
	}
	if cfg.Telemetry.OOBStatus != "off" || cfg.Telemetry.OOBEFIVar == nil || !*cfg.Telemetry.OOBEFIVar {
		t.Fatalf("OOBStatus = %q, OOBEFIVar = %v", cfg.Telemetry.OOBStatus, cfg.Telemetry.OOBEFIVar)
	}
	for _, line := range []string{"OOB_STATUS=sel\n", "OOB_EFI_VAR=yes please\n"} {
		if _, err := ParseVars(strings.NewReader(line)); err == nil {
			t.Errorf("ParseVars(%q) accepted an invalid value", line)
		}
	}
}
