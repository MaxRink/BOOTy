package config

import (
	"strings"
	"testing"
	"time"
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

// The firmware LLDP agent is stopped only with the bm4x-cablecheck
// profile unless disableFirmwareAgent says otherwise; a plain run keeps it
// (ice keeps the setting across boots, and the legacy ramdisk never stops
// it).
func TestDisableNICFirmwareLLDP(t *testing.T) {
	on, off := true, false
	for _, tt := range []struct {
		mode, profile string
		value         *bool
		want          bool
	}{
		// On by default in every mode: the agent consumes the switch's
		// LLDP frames. false (the opt-out) leaves the flag alone.
		{"check", "", nil, true},
		{"check", "bm4x-cablecheck", nil, true},
		{"check", "bm4x-cablecheck", &off, false},
		{"provision", "", nil, true},
		{"", "", nil, true},
		{"provision", "", &off, false},
		{"provision", "", &on, true},
		{"check", "cabling", nil, true},
		{"check", "cabling", &off, false},
	} {
		c := &Config{Mode: tt.mode}
		if tt.profile == "cabling" {
			c.Check.Profiles, tt.profile = []string{"health", "cabling"}, ""
		}
		c.Network.LLDP.Profile = tt.profile
		c.Network.LLDP.DisableFirmwareAgent = tt.value
		if got := c.DisableNICFirmwareLLDP(); got != tt.want {
			t.Errorf("%+v: %v, want %v", tt, got, tt.want)
		}
	}
	cfg, err := loadYAML(strings.NewReader("network:\n  lldp:\n    disableFirmwareAgent: false\n"), true)
	if err != nil || cfg.DisableNICFirmwareLLDP() {
		t.Fatalf("YAML network.lldp.disableFirmwareAgent false: %v", err)
	}
}

func TestValidateLinksMode(t *testing.T) {
	c := &Config{}
	c.Network.Mode = "links"
	if err := c.Validate(); err != nil {
		t.Fatalf("network.mode links: %v", err)
	}
}

func TestCheckHold(t *testing.T) {
	for _, tt := range []struct {
		hold string
		want time.Duration
		err  bool
	}{
		{"", 0, false},
		{"off", 0, false},
		{"until-poweroff", -1, false},
		{"10m", 10 * time.Minute, false},
		{"0s", 0, true},
		{"-1m", 0, true},
		{"forever", 0, true},
	} {
		c := &Config{Check: CheckConfig{Hold: tt.hold}}
		if got := c.Check.HoldDuration(); got != tt.want {
			t.Errorf("%q: HoldDuration = %v, want %v", tt.hold, got, tt.want)
		}
		err := c.Validate()
		if (err != nil) != tt.err || (err != nil && !strings.Contains(err.Error(), "check.hold (CHECK_HOLD)")) {
			t.Errorf("%q: Validate = %v, want error %v", tt.hold, err, tt.err)
		}
	}
	cfg, err := loadYAML(strings.NewReader("check:\n  hold: until-poweroff\n"), true)
	if err != nil || cfg.Check.Hold != "until-poweroff" {
		t.Fatalf("YAML check.hold: %+v, %v", cfg, err)
	}
	if _, err := loadYAML(strings.NewReader("check:\n  holdUntil: poweroff\n"), true); err == nil {
		t.Fatal("unknown check key accepted")
	}
}

func TestCheckNeighborWait(t *testing.T) {
	c := &Config{Check: CheckConfig{LLDPNeighborWait: "90s"}}
	if err := c.Validate(); err != nil || c.Check.NeighborWait() != 90*time.Second {
		t.Fatalf("90s: %v, %v", c.Check.NeighborWait(), err)
	}
	c.Check.LLDPNeighborWait = "soon"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "LLDP_NEIGHBOR_WAIT") {
		t.Fatalf("Validate = %v, want an LLDP_NEIGHBOR_WAIT error", err)
	}
	if (&CheckConfig{}).NeighborWait() != 0 {
		t.Fatal("no wait by default")
	}
}

func TestValidateCablingCheck(t *testing.T) {
	ok := CheckConfig{
		Profiles: []string{"health", "cabling"}, Sinks: []string{"file", "bm4x-report", "http", "caprf"}, ResultURL: "https://x/r",
		ExpectedCabling: []ExpectedLink{{MAC: "3c:fd:fe:00:00:01", Switch: "leaf01", Port: "Ethernet1/1"}, {Interface: "eno1", Switch: "s", Port: "p"}},
		Thresholds:      CheckThresholds{LinkFlaps: "block", OpticsAlarm: "off"},
	}
	if err := (&Config{Check: ok}).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, tt := range []struct {
		name string
		set  func(*CheckConfig)
		want string
	}{
		{"profile", func(c *CheckConfig) { c.Profiles = []string{"infra"} }, `check.profiles (CHECK_PROFILES) "infra"`},
		{"sink", func(c *CheckConfig) { c.Sinks = []string{"kafka"} }, `check.sinks (CHECK_RESULT_SINKS) "kafka"`},
		{"http without URL", func(c *CheckConfig) { c.ResultURL = "" }, "needs check.resultURL"},
		{"action", func(c *CheckConfig) { c.Thresholds.FEC = "fail" }, `check.thresholds.fec (CHECK_THRESHOLDS) "fail"`},
		{"negative flaps", func(c *CheckConfig) { c.Thresholds.MaxFlaps = new(-1) }, "must not be negative"},
		{"mac and interface", func(c *CheckConfig) { c.ExpectedCabling[0].Interface = "eno1" }, "check.expectedCabling[0]"},
		{"no port", func(c *CheckConfig) { c.ExpectedCabling[1].Port = "" }, "check.expectedCabling[1]"},
		{"bad mac", func(c *CheckConfig) { c.ExpectedCabling[0].MAC = "3c:fd" }, `bad mac "3c:fd"`},
	} {
		c := ok
		c.ExpectedCabling = append([]ExpectedLink(nil), ok.ExpectedCabling...)
		tt.set(&c)
		if err := (&Config{Check: c}).Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: Validate = %v, want %q", tt.name, err, tt.want)
		}
	}
	if !(&CheckConfig{}).HasProfile("health") || (&CheckConfig{}).HasProfile("cabling") || ok.HasProfile("infra") {
		t.Error("HasProfile")
	}
	if _, err := loadYAML(strings.NewReader("check:\n  expectedCabling:\n    - {mac: 3c:fd:fe:00:00:01, switch: s, port: p, vlan: 4}\n"), true); err == nil {
		t.Error("unknown expectedCabling key accepted")
	}
}

func TestValidateLLDPAnnounce(t *testing.T) {
	for _, a := range []string{"", "parity", "standard", "full"} {
		c := &Config{}
		c.Network.LLDP.Announce = a
		if err := c.Validate(); err != nil {
			t.Errorf("announce %q: %v", a, err)
		}
	}
	c := &Config{}
	c.Network.LLDP.Announce = "verbose"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `network.lldp.announce (LLDP_ANNOUNCE) "verbose"`) {
		t.Fatalf("Validate = %v, want an LLDP_ANNOUNCE error", err)
	}
}
