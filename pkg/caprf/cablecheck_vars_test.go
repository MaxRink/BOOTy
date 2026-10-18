package caprf

import (
	"strings"
	"testing"
)

func TestCableCheckVars(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader("LLDP_PROFILE=bm4x-cablecheck\nLLDP_EMIT_ALL=false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.LLDP.Profile != "bm4x-cablecheck" || cfg.Network.LLDP.EmitAll == nil || *cfg.Network.LLDP.EmitAll {
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
	if _, err := ParseVars(strings.NewReader("LLDP_EMIT_ALL=maybe\n")); err == nil {
		t.Fatal("LLDP_EMIT_ALL=maybe accepted")
	}
}
