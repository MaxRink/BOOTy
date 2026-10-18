package caprf

import (
	"strings"
	"testing"
)

func TestCableCheckVars(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader("LLDP_PROFILE=bm4x-cablecheck\nLLDP_EMIT_ALL=false\nLLDP_ANNOUNCE=full\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.LLDP.Profile != "bm4x-cablecheck" || cfg.Network.LLDP.EmitAll == nil || *cfg.Network.LLDP.EmitAll ||
		cfg.Network.LLDP.Announce != "full" {
		t.Fatalf("lldp = %+v", cfg.Network.LLDP)
	}
	cfg, err = ParseVars(strings.NewReader("LLDP_PROFILE=bm4x-cablecheck\nLLDP_PORT_ID_FORMAT=mac-string-dotted\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.LLDP.PortIDFormat != "mac-string-dotted" {
		t.Fatalf("LLDP_PORT_ID_FORMAT = %q", cfg.Network.LLDP.PortIDFormat)
	}
	for _, bad := range []string{"LLDP_PROFILE=bm4x-cablecheck\nLLDP_PORT_ID_FORMAT=dotted\n", "LLDP_PORT_ID_FORMAT=mac\n"} {
		if _, err := ParseVars(strings.NewReader(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if cfg, err := ParseVars(strings.NewReader("HOSTNAME=x\n")); err != nil || !cfg.DisableNICFirmwareLLDP() {
		t.Fatalf("default: %+v, %v", cfg.Network.LLDP, err)
	}
	if cfg, err := ParseVars(strings.NewReader("LLDP_DISABLE_FW_AGENT=false\n")); err != nil || cfg.DisableNICFirmwareLLDP() {
		t.Fatalf("LLDP_DISABLE_FW_AGENT=false: %+v, %v", cfg.Network.LLDP, err)
	}
	if _, err := ParseVars(strings.NewReader("LLDP_DISABLE_FW_AGENT=disable\n")); err == nil {
		t.Fatal("LLDP_DISABLE_FW_AGENT=disable accepted")
	}
	if cfg, err := ParseVars(strings.NewReader("CHECK_HOLD=until-poweroff\n")); err != nil || cfg.Check.Hold != "until-poweroff" {
		t.Fatalf("CHECK_HOLD: %+v, %v", cfg.Check, err)
	}
	if cfg, err := ParseVars(strings.NewReader("LLDP_NEIGHBOR_WAIT=90s\n")); err != nil || cfg.Check.LLDPNeighborWait != "90s" {
		t.Fatalf("LLDP_NEIGHBOR_WAIT: %+v, %v", cfg.Check, err)
	}
	if _, err := ParseVars(strings.NewReader("LLDP_EMIT_ALL=maybe\n")); err == nil {
		t.Fatal("LLDP_EMIT_ALL=maybe accepted")
	}
}

func TestCableCheckResultVars(t *testing.T) {
	vars := strings.Join([]string{
		"CHECK_PROFILES=health, cabling",
		"CHECK_RESULT_SINKS=file,bm4x-report",
		"CHECK_RESULT_URL=https://caprf.example/check",
		`CHECK_EXPECTED_CABLING='[{"mac":"3c:fd:fe:00:00:01","switch":"leaf01","port":"Ethernet1/1","speedMbps":25000}]'`,
		`CHECK_THRESHOLDS='{"lldpMissing":"block","maxFlaps":3}'`,
	}, "\n")
	cfg, err := ParseVars(strings.NewReader(vars))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Check
	if len(c.Profiles) != 2 || c.Profiles[1] != "cabling" || len(c.Sinks) != 2 || c.ResultURL == "" ||
		len(c.ExpectedCabling) != 1 || c.ExpectedCabling[0].SpeedMbps != 25000 ||
		c.Thresholds.LLDPMissing != "block" || c.Thresholds.MaxFlaps == nil || *c.Thresholds.MaxFlaps != 3 {
		t.Fatalf("check = %+v", c)
	}
	if _, err := ParseVars(strings.NewReader(`CHECK_THRESHOLDS='{"crc":1}'` + "\n")); err == nil {
		t.Fatal("unknown threshold accepted")
	}
}
