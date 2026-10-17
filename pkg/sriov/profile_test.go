package sriov

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func ptr[T any](v T) *T { return &v }

// fullProfile sets every field with a valid value.
func fullProfile() Profile {
	return Profile{
		Name:  "cx6dx-2p",
		Match: Match{VendorID: "15b3", DeviceID: "101d", Driver: "mlx5_core", MAC: "B8:CE:F6:00:00:01", Interface: "ens1f*"},
		Firmware: FirmwareSettings{
			SRIOVEnabled: ptr(true), MaxVFs: ptr(64), LinkType: "eth", FWLLDPAgent: ptr(false),
			NVConfig:      []Setting{{"CQE_COMPRESSION", "1"}, {"FLEX_PARSER_PROFILE_ENABLE", "4"}},
			DevlinkParams: []Setting{{"enable_sriov", "true"}},
		},
		Runtime: RuntimeSettings{
			NumVFs: ptr(8), EswitchMode: "switchdev", Handoff: "reset",
			VFDefaults: VFSettings{Trust: ptr(false), SpoofChk: ptr(true), LinkState: "auto"},
			VFs: []VFOverride{{Index: 0, MAC: "02:00:00:00:00:10",
				VFSettings: VFSettings{VLAN: ptr(100), QoS: ptr(3), MinTxRate: ptr(100), MaxTxRate: ptr(1000)}}},
		},
		OnDrift: "fail",
	}
}

func TestValidateAcceptsFullConfig(t *testing.T) {
	c := Config{Profiles: []Profile{fullProfile()}, OnDrift: "apply", NICTools: "vendor"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&Config{}).Validate(); err != nil {
		t.Fatalf("empty config: %v", err)
	}
}

// TestValidateRejects changes one field of a valid profile per case; every
// case must fail with a message that names the field.
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
		want   string
	}{
		{"onDrift", func(c *Config) { c.OnDrift = "Apply" }, "SRIOV_ON_DRIFT"},
		{"nicTools", func(c *Config) { c.NICTools = "mft" }, "NIC_TOOLS"},
		{"verifyReboot", func(c *Config) { c.VerifyReboot = "yes" }, "SRIOV_VERIFY_REBOOT"},
		{"duplicate", func(c *Config) { c.Profiles = append(c.Profiles, c.Profiles[0]) }, "defined twice"},
		{"name", func(c *Config) { c.Profiles[0].Name = "" }, "name must be"},
		{"name chars", func(c *Config) { c.Profiles[0].Name = "a b" }, "name must be"},
		{"profile onDrift", func(c *Config) { c.Profiles[0].OnDrift = "ignore" }, "onDrift"},
		{"empty match", func(c *Config) { c.Profiles[0].Match = Match{} }, "match needs"},
		{"vendor", func(c *Config) { c.Profiles[0].Match.VendorID = "0x15b3" }, "match.vendorId"},
		{"vendor case", func(c *Config) { c.Profiles[0].Match.VendorID = "15B3" }, "match.vendorId"},
		{"device", func(c *Config) { c.Profiles[0].Match.DeviceID = "101" }, "match.deviceId"},
		{"driver", func(c *Config) { c.Profiles[0].Match.Driver = "mlx5 core" }, "match.driver"},
		{"mac", func(c *Config) { c.Profiles[0].Match.MAC = "b8:ce:f6" }, "match.mac"},
		{"mac 64", func(c *Config) { c.Profiles[0].Match.MAC = "00:00:00:00:fe:80:00:00" }, "match.mac"},
		{"interface", func(c *Config) { c.Profiles[0].Match.Interface = "ens[" }, "match.interface"},
		{"interface slash", func(c *Config) { c.Profiles[0].Match.Interface = "a/b" }, "match.interface"},
		{"maxVFs", func(c *Config) { c.Profiles[0].Firmware.MaxVFs = ptr(-1) }, "firmware.maxVFs"},
		{"maxVFs high", func(c *Config) { c.Profiles[0].Firmware.MaxVFs = ptr(65536) }, "firmware.maxVFs"},
		{"maxVFs without sriov", func(c *Config) {
			c.Profiles[0].Firmware.SRIOVEnabled = ptr(false)
			c.Profiles[0].Runtime = RuntimeSettings{}
		}, "needs firmware.sriovEnabled"},
		{"numVFs without sriov", func(c *Config) {
			c.Profiles[0].Firmware.SRIOVEnabled = ptr(false)
			c.Profiles[0].Firmware.MaxVFs = nil
		}, "runtime.numVFs needs firmware.sriovEnabled"},
		{"linkType", func(c *Config) { c.Profiles[0].Firmware.LinkType = "ethernet" }, "firmware.linkType"},
		{"sriov on without VFs", func(c *Config) {
			c.Profiles[0].Firmware.MaxVFs = ptr(0)
			c.Profiles[0].Runtime = RuntimeSettings{}
		}, "firmware.sriovEnabled needs firmware.maxVFs > 0"},
		{"nv name", func(c *Config) { c.Profiles[0].Firmware.NVConfig[0].Name = "cqe" }, "firmware.nvConfig name"},
		{"nv value", func(c *Config) { c.Profiles[0].Firmware.NVConfig[0].Value = "1 2" }, "value"},
		{"nv empty value", func(c *Config) { c.Profiles[0].Firmware.NVConfig[0].Value = "" }, "value"},
		{"nv twice", func(c *Config) {
			c.Profiles[0].Firmware.NVConfig = append(c.Profiles[0].Firmware.NVConfig, Setting{"CQE_COMPRESSION", "0"})
		}, "set twice"},
		{"nv named", func(c *Config) { c.Profiles[0].Firmware.NVConfig[0].Name = "NUM_OF_VFS" }, "use sriovEnabled"},
		{"nv link type", func(c *Config) { c.Profiles[0].Firmware.NVConfig[0].Name = "LINK_TYPE_P2" }, "use sriovEnabled"},
		{"param name", func(c *Config) { c.Profiles[0].Firmware.DevlinkParams[0].Name = "Enable" }, "devlinkParams name"},
		{"numVFs", func(c *Config) { c.Profiles[0].Runtime.NumVFs = ptr(-2) }, "runtime.numVFs"},
		{"numVFs above max", func(c *Config) { c.Profiles[0].Runtime.NumVFs = ptr(65) }, "above firmware.maxVFs"},
		{"eswitch", func(c *Config) { c.Profiles[0].Runtime.EswitchMode = "Switchdev" }, "runtime.eswitchMode"},
		{"handoff", func(c *Config) { c.Profiles[0].Runtime.Handoff = "leave" }, "runtime.handoff"},
		{"vf without numVFs", func(c *Config) { c.Profiles[0].Runtime.NumVFs = ptr(0) }, "need runtime.numVFs"},
		{"vf index", func(c *Config) { c.Profiles[0].Runtime.VFs[0].Index = 8 }, "out of the VF range"},
		{"vf negative", func(c *Config) { c.Profiles[0].Runtime.VFs[0].Index = -1 }, "out of the VF range"},
		{"vf twice", func(c *Config) {
			c.Profiles[0].Runtime.VFs = append(c.Profiles[0].Runtime.VFs, VFOverride{Index: 0})
		}, "set twice"},
		{"vf mac", func(c *Config) { c.Profiles[0].Runtime.VFs[0].MAC = "zz" }, "unicast"},
		{"vf multicast", func(c *Config) { c.Profiles[0].Runtime.VFs[0].MAC = "01:00:5e:00:00:01" }, "unicast"},
		{"vlan", func(c *Config) { c.Profiles[0].Runtime.VFs[0].VLAN = ptr(4096) }, ".vlan"},
		{"qos", func(c *Config) { c.Profiles[0].Runtime.VFs[0].QoS = ptr(8) }, ".qos"},
		{"qos without vlan", func(c *Config) { c.Profiles[0].Runtime.VFs[0].VLAN = ptr(0) }, "qos needs a vlan"},
		{"min rate", func(c *Config) { c.Profiles[0].Runtime.VFs[0].MinTxRate = ptr(-1) }, "minTxRate"},
		{"max rate", func(c *Config) { c.Profiles[0].Runtime.VFDefaults.MaxTxRate = ptr(-1) }, "maxTxRate"},
		{"min above max", func(c *Config) { c.Profiles[0].Runtime.VFs[0].MinTxRate = ptr(2000) }, "minTxRate is above"},
		{"link state", func(c *Config) { c.Profiles[0].Runtime.VFDefaults.LinkState = "up" }, "linkState"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Profiles: []Profile{fullProfile()}}
			tc.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error with %q", err, tc.want)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	c := &Config{}
	p := &Profile{}
	if c.DriftAction(p) != OnDriftWarn || c.Tools() != ToolsOpenSource {
		t.Fatalf("defaults: %s %s", c.DriftAction(p), c.Tools())
	}
	c.OnDrift = OnDriftFail
	if c.DriftAction(p) != OnDriftFail {
		t.Fatal("config onDrift ignored")
	}
	p.OnDrift = OnDriftApply
	if c.DriftAction(p) != OnDriftApply {
		t.Fatal("profile onDrift does not win")
	}
	c.NICTools = ToolsKernel
	if c.Tools() != ToolsKernel {
		t.Fatal("nicTools ignored")
	}
	if !c.VerifyRebootEnabled() {
		t.Fatal("verify reboot is on by default")
	}
	c.VerifyReboot = VerifyRebootOff
	if c.VerifyRebootEnabled() {
		t.Fatal("verifyReboot off ignored")
	}
}

func TestMatches(t *testing.T) {
	pf := &PF{Interface: "ens1f0np0", VendorID: "15b3", DeviceID: "101d", Driver: "mlx5_core", MAC: "b8:ce:f6:00:00:01"}
	for _, m := range []Match{
		{VendorID: "15b3"}, {DeviceID: "101d"}, {Driver: "mlx5_core"}, {MAC: "B8-CE-F6-00-00-01"},
		{Interface: "ens1f*"}, {Interface: "ens1f0np0"},
		{VendorID: "15b3", DeviceID: "101d", Driver: "mlx5_core", MAC: "b8:ce:f6:00:00:01", Interface: "ens?f0np0"},
	} {
		if !m.Matches(pf) {
			t.Errorf("%+v does not match", m)
		}
	}
	for _, m := range []Match{
		{VendorID: "8086"}, {DeviceID: "1017"}, {Driver: "ice"}, {MAC: "b8:ce:f6:00:00:02"},
		{Interface: "ens2*"}, {Interface: "ens1f0"}, {Interface: "["},
		{VendorID: "15b3", Driver: "ice"},
	} {
		if m.Matches(pf) {
			t.Errorf("%+v matches", m)
		}
	}
}

// TestDecodeJSONAndYAML decodes the same profile in both formats; the VF
// override fields sit next to index in both.
func TestDecodeJSONAndYAML(t *testing.T) {
	js := `{"name":"e810","match":{"driver":"ice"},"firmware":{"fwLLDPAgent":false},
	 "runtime":{"numVFs":4,"vfDefaults":{"spoofChk":true},"vfs":[{"index":1,"mac":"02:00:00:00:00:01","vlan":10}]}}`
	ym := `
name: e810
match: {driver: ice}
firmware: {fwLLDPAgent: false}
runtime:
  numVFs: 4
  vfDefaults: {spoofChk: true}
  vfs:
    - {index: 1, mac: "02:00:00:00:00:01", vlan: 10}
`
	var fromJSON, fromYAML Profile
	dec := json.NewDecoder(strings.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fromJSON); err != nil {
		t.Fatal(err)
	}
	yd := yaml.NewDecoder(strings.NewReader(ym))
	yd.KnownFields(true)
	if err := yd.Decode(&fromYAML); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]Profile{"json": fromJSON, "yaml": fromYAML} {
		if p.Runtime.VFs[0].VLAN == nil || *p.Runtime.VFs[0].VLAN != 10 || p.Runtime.VFs[0].MAC != "02:00:00:00:00:01" ||
			p.Firmware.FWLLDPAgent == nil || *p.Firmware.FWLLDPAgent || *p.Runtime.VFDefaults.SpoofChk != true {
			t.Errorf("%s: %+v", name, p)
		}
		c := Config{Profiles: []Profile{p}}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestValidateAcceptsBoundaries keeps every inclusive limit valid, so a
// limit that moves by one fails here.
func TestValidateAcceptsBoundaries(t *testing.T) {
	cases := map[string]func(p *Profile){
		"maxVFs 0 with SR-IOV off": func(p *Profile) {
			p.Firmware = FirmwareSettings{SRIOVEnabled: ptr(false), MaxVFs: ptr(0)}
			p.Runtime = RuntimeSettings{}
		},
		"numVFs 0 with SR-IOV off": func(p *Profile) {
			p.Firmware = FirmwareSettings{SRIOVEnabled: ptr(false)}
			p.Runtime = RuntimeSettings{NumVFs: ptr(0)}
		},
		"maxVFs 65535": func(p *Profile) {
			p.Firmware.MaxVFs = ptr(65535)
			p.Runtime.NumVFs = ptr(65535)
		},
		"numVFs equal to maxVFs": func(p *Profile) { p.Runtime.NumVFs = ptr(64) },
		"vlan 4095 qos 7":        func(p *Profile) { p.Runtime.VFs[0].VLAN, p.Runtime.VFs[0].QoS = ptr(4095), ptr(7) },
		"qos 0 without vlan":     func(p *Profile) { p.Runtime.VFs[0].VLAN, p.Runtime.VFs[0].QoS = ptr(0), ptr(0) },
		"min rate equal to max":  func(p *Profile) { p.Runtime.VFs[0].MinTxRate, p.Runtime.VFs[0].MaxTxRate = ptr(100), ptr(100) },
		"min rate with no max":   func(p *Profile) { p.Runtime.VFs[0].MinTxRate, p.Runtime.VFs[0].MaxTxRate = ptr(100), ptr(0) },
		"last VF index":          func(p *Profile) { p.Runtime.VFs[0].Index = 7 },
		"no VF settings":         func(p *Profile) { p.Runtime = RuntimeSettings{EswitchMode: "legacy"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := fullProfile()
			mutate(&p)
			if err := (&Config{Profiles: []Profile{p}}).Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// VF defaults alone also need VFs.
	p := fullProfile()
	p.Runtime = RuntimeSettings{VFDefaults: VFSettings{Trust: ptr(true)}}
	if err := (&Config{Profiles: []Profile{p}}).Validate(); err == nil || !strings.Contains(err.Error(), "need runtime.numVFs") {
		t.Fatalf("VF defaults without VFs: %v", err)
	}
	for v, ok := range map[int]bool{-1: false, 0: true, 7: true, 8: false} {
		if got := intRange("x", &v, 0, 7) == ""; got != ok {
			t.Errorf("intRange(%d) ok = %v", v, got)
		}
	}
}

func TestHasFirmwareSettings(t *testing.T) {
	for name, f := range map[string]FirmwareSettings{
		"sriovEnabled": {SRIOVEnabled: ptr(false)}, "maxVFs": {MaxVFs: ptr(0)}, "linkType": {LinkType: "eth"},
		"fwLLDPAgent": {FWLLDPAgent: ptr(false)}, "nvConfig": {NVConfig: []Setting{{"A", "1"}}},
		"devlinkParams": {DevlinkParams: []Setting{{"a", "1"}}},
	} {
		if !hasFirmwareSettings(&f) {
			t.Errorf("%s alone is a firmware setting", name)
		}
	}
	if hasFirmwareSettings(&FirmwareSettings{}) {
		t.Fatal("empty firmware settings")
	}
}

func TestConfigDecodesStrictly(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("onDrift: fail\nprofiles:\n  - name: a\n    match: {driver: ice}\n"), &c); err != nil ||
		c.OnDrift != "fail" || c.Profiles[0].Match.Driver != "ice" {
		t.Fatalf("valid YAML: %+v %v", c, err)
	}
	for name, doc := range map[string]string{
		"top level": "onDrfit: fail\n",
		"profile":   "profiles:\n  - name: a\n    match: {driver: ice}\n    firmware: {maxVfss: 8}\n",
	} {
		if err := yaml.Unmarshal([]byte(doc), &c); err == nil {
			t.Errorf("YAML %s: unknown key accepted", name)
		}
	}
	if err := json.Unmarshal([]byte(`{"onDrift":"warn","nicTools":"kernel"}`), &c); err != nil || c.NICTools != "kernel" {
		t.Fatalf("valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"profiles":[{"name":"a","match":{"driver":"ice"},"extra":1}]}`), &c); err == nil {
		t.Fatal("JSON unknown key accepted")
	}
	// Inside a lenient parent decoder the section stays strict.
	var parent struct {
		SRIOV Config `yaml:"sriov"`
		Other string `yaml:"other"`
	}
	if err := yaml.Unmarshal([]byte("unknown: 1\nsriov:\n  profles: []\n"), &parent); err == nil {
		t.Fatal("lenient parent let an unknown sriov key through")
	}
}
