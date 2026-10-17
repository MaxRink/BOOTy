package caprf

import (
	"strings"
	"testing"
)

func TestParseVarsSRIOV(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader(`SRIOV_PROFILES=[{"name":"cx6","match":{"vendorId":"15b3"},"firmware":{"sriovEnabled":true,"maxVFs":64},"runtime":{"numVFs":4,"vfs":[{"index":0,"vlan":10}]}}]
SRIOV_ON_DRIFT=apply
NIC_TOOLS=kernel
SRIOV_VERIFY_REBOOT=off
SRIOV_UPLINK_VF_DRIVERS=mlx5_core, bnxt_en
`))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Provision.SRIOV
	if len(s.Profiles) != 1 || s.Profiles[0].Name != "cx6" || *s.Profiles[0].Firmware.MaxVFs != 64 ||
		*s.Profiles[0].Runtime.VFs[0].VLAN != 10 || s.OnDrift != "apply" || s.NICTools != "kernel" || s.VerifyReboot != "off" ||
		strings.Join(s.UplinkVFDrivers, ",") != "mlx5_core,bnxt_en" {
		t.Fatalf("sriov = %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	// An empty value clears the profiles.
	cfg, err = ParseVars(strings.NewReader("SRIOV_PROFILES=\n"))
	if err != nil || cfg.Provision.SRIOV.Profiles != nil {
		t.Fatalf("empty SRIOV_PROFILES: %+v %v", cfg.Provision.SRIOV, err)
	}
	for _, v := range []string{
		`SRIOV_PROFILES=[{`,
		`SRIOV_PROFILES=[{"name":"a","match":{"driver":"ice"},"firmware":{"maxFunctions":8}}]`,
		`SRIOV_PROFILES=[{"name":"a","match":{"driver":"ice"}}] x`,
		`SRIOV_PROFILES={"name":"a"}`,
	} {
		if _, err := ParseVars(strings.NewReader(v + "\n")); err == nil {
			t.Errorf("%s parsed without error", v)
		}
	}
}
