package config

import (
	"strings"
	"testing"
)

func TestLoadFullYAMLSRIOV(t *testing.T) {
	cfg, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Provision.SRIOV
	if s.OnDrift != "fail" || s.NICTools != "opensource" || len(s.Profiles) != 2 {
		t.Fatalf("sriov = %+v", s)
	}
	p := s.Profiles[0]
	if p.Name != "cx6dx" || p.Match.DeviceID != "101d" || *p.Firmware.MaxVFs != 64 || p.Firmware.NVConfig[0].Name != "CQE_COMPRESSION" ||
		*p.Runtime.NumVFs != 8 || p.Runtime.EswitchMode != "switchdev" || *p.Runtime.VFs[0].VLAN != 100 || !*p.Runtime.VFDefaults.SpoofChk {
		t.Fatalf("profile = %+v", p)
	}
	if !s.Profiles[1].Optional || *s.Profiles[1].Firmware.FWLLDPAgent {
		t.Fatalf("e810 = %+v", s.Profiles[1])
	}
}

func TestLoadRejectsBadSRIOV(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown field": {"provision:\n  sriov:\n    profiles:\n      - name: a\n        match: {driver: ice}\n        firmware: {maxVfs: 8}\n", "maxVfs"},
		"on drift":      {"provision:\n  sriov:\n    onDrift: ignore\n", "SRIOV_ON_DRIFT"},
		"bad profile":   {"provision:\n  sriov:\n    profiles:\n      - name: a\n        match: {}\n", "match needs"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadWithOptions(LoadOptions{Path: writeTestFile(t, "sriov.yaml", tc.yaml), Strict: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDefaultLoadIsStrictForSRIOV(t *testing.T) {
	// Load without Strict ignores unknown keys elsewhere, but not in
	// provision.sriov.
	yaml := "unknownTopLevel: 1\nprovision:\n  sriov:\n    profiles:\n      - name: a\n        match: {driver: ice}\n        firmware: {maxVirtualFunctions: 8}\n"
	if _, err := Load(writeTestFile(t, "lenient.yaml", yaml)); err == nil || !strings.Contains(err.Error(), "maxVirtualFunctions") {
		t.Fatalf("Load() = %v", err)
	}
}
