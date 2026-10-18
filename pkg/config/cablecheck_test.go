package config

import (
	"strings"
	"testing"
)

// The bm4x-cablecheck profile sends on every NIC unless emitAll is set;
// the networkd default sends only on the emitLLDP NICs.
func TestLLDPEmitAllNICs(t *testing.T) {
	for _, tt := range []struct {
		l    LLDPConfig
		want bool
	}{
		{LLDPConfig{}, false},
		{LLDPConfig{Profile: "networkd"}, false},
		{LLDPConfig{Profile: "networkd", EmitAll: new(true)}, true},
		{LLDPConfig{Profile: "bm4x-cablecheck"}, true},
		{LLDPConfig{Profile: "bm4x-cablecheck", EmitAll: new(false)}, false},
	} {
		if got := tt.l.EmitAllNICs(); got != tt.want {
			t.Errorf("%+v: EmitAllNICs = %v, want %v", tt.l, got, tt.want)
		}
	}
}

func TestValidateLLDPProfile(t *testing.T) {
	for _, p := range []string{"", "networkd", "bm4x-cablecheck"} {
		c := &Config{}
		c.Network.LLDP.Profile = p
		if err := c.Validate(); err != nil {
			t.Errorf("profile %q: %v", p, err)
		}
	}
	c := &Config{}
	c.Network.LLDP.Profile = "lldpd"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `network.lldp.profile (LLDP_PROFILE) "lldpd"`) {
		t.Fatalf("Validate = %v, want an LLDP profile error", err)
	}
}

// network.lldp is strict: an unknown key fails.
func TestLoadYAMLLLDP(t *testing.T) {
	cfg, err := loadYAML(strings.NewReader("network:\n  lldp:\n    profile: bm4x-cablecheck\n    emitAll: false\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.LLDP.Profile != "bm4x-cablecheck" || cfg.Network.LLDP.EmitAll == nil || *cfg.Network.LLDP.EmitAll {
		t.Fatalf("lldp = %+v", cfg.Network.LLDP)
	}
	if _, err := loadYAML(strings.NewReader("network:\n  lldp:\n    portIDSubtype: mac\n"), true); err == nil {
		t.Fatal("unknown network.lldp key accepted")
	}
}

// network.lldp.portIDFormat: a known format, and only with the
// bm4x-cablecheck profile.
func TestValidateLLDPPortIDFormat(t *testing.T) {
	for _, tt := range []struct {
		profile, format, err string
	}{
		{"bm4x-cablecheck", "", ""},
		{"bm4x-cablecheck", "mac", ""},
		{"bm4x-cablecheck", "mac-string-colon", ""},
		{"bm4x-cablecheck", "mac-string-dotted", ""},
		{"bm4x-cablecheck", "ifname", ""},
		{"bm4x-cablecheck", "MAC", `invalid network.lldp.portIDFormat (LLDP_PORT_ID_FORMAT) "MAC"`},
		{"bm4x-cablecheck", "mac-dotted", `invalid network.lldp.portIDFormat (LLDP_PORT_ID_FORMAT) "mac-dotted"`},
		{"", "mac", `"mac" needs network.lldp.profile (LLDP_PROFILE) "bm4x-cablecheck"`},
		{"networkd", "ifname", `"ifname" needs network.lldp.profile`},
	} {
		c := &Config{}
		c.Network.LLDP.Profile, c.Network.LLDP.PortIDFormat = tt.profile, tt.format
		err := c.Validate()
		switch {
		case tt.err == "" && err != nil:
			t.Errorf("%q/%q: %v", tt.profile, tt.format, err)
		case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
			t.Errorf("%q/%q: Validate = %v, want %q", tt.profile, tt.format, err, tt.err)
		}
	}
	cfg, err := loadYAML(strings.NewReader("network:\n  lldp:\n    profile: bm4x-cablecheck\n    portIDFormat: mac-string-colon\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.LLDP.PortIDFormat != "mac-string-colon" {
		t.Fatalf("portIDFormat = %q", cfg.Network.LLDP.PortIDFormat)
	}
}
