package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()

	if cfg.BridgeName != "br.provision" {
		t.Errorf("BridgeName = %q, want %q", cfg.BridgeName, "br.provision")
	}
	if cfg.VRFName != "" {
		t.Errorf("VRFName = %q, want empty (no VRF by default)", cfg.VRFName)
	}
	if cfg.MTU != 9000 {
		t.Errorf("MTU = %d, want %d", cfg.MTU, 9000)
	}
	if cfg.VRFTableID != 1000 {
		t.Errorf("VRFTableID = %d, want %d", cfg.VRFTableID, 1000)
	}
	if cfg.OverlayVRFTableID != 1000 {
		t.Errorf("OverlayVRFTableID = %d, want %d", cfg.OverlayVRFTableID, 1000)
	}
}

func TestApplyDefaults_NoOverwrite(t *testing.T) {
	cfg := Config{
		BridgeName:        "custom-br",
		VRFName:           "custom-vrf",
		VRFTableID:        42,
		OverlayVRFTableID: 43,
		MTU:               1500,
	}
	cfg.ApplyDefaults()

	if cfg.BridgeName != "custom-br" {
		t.Errorf("BridgeName = %q, want %q", cfg.BridgeName, "custom-br")
	}
	if cfg.VRFName != "custom-vrf" {
		t.Errorf("VRFName = %q, want %q", cfg.VRFName, "custom-vrf")
	}
	if cfg.MTU != 1500 {
		t.Errorf("MTU = %d, want %d", cfg.MTU, 1500)
	}
	if cfg.VRFTableID != 42 {
		t.Errorf("VRFTableID = %d, want %d", cfg.VRFTableID, 42)
	}
	if cfg.OverlayVRFTableID != 43 {
		t.Errorf("OverlayVRFTableID = %d, want %d", cfg.OverlayVRFTableID, 43)
	}
}

func TestConfigureResolversWritesInitramfsResolvConf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	originalPath := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = originalPath })

	if err := ConfigureResolvers(" 8.8.8.8, ,1.1.1.1 "); err != nil {
		t.Fatalf("ConfigureResolvers: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if got, want := string(data), "nameserver 8.8.8.8\nnameserver 1.1.1.1\n"; got != want {
		t.Fatalf("resolv.conf = %q, want %q", got, want)
	}
}

func TestConfigureResolversReplacesDanglingSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.Symlink("../run/systemd/resolve/stub-resolv.conf", path); err != nil {
		t.Fatalf("symlink resolv.conf: %v", err)
	}

	originalPath := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = originalPath })

	if err := ConfigureResolvers("9.9.9.9"); err != nil {
		t.Fatalf("ConfigureResolvers: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat resolv.conf: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("resolv.conf is still a symlink")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if got, want := string(data), "nameserver 9.9.9.9\n"; got != want {
		t.Fatalf("resolv.conf = %q, want %q", got, want)
	}
}

func TestConfigureResolversEmptySkips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	originalPath := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = originalPath })

	if err := ConfigureResolvers(" , "); err != nil {
		t.Fatalf("ConfigureResolvers: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("resolv.conf should not exist after empty resolvers, stat error: %v", err)
	}
}

func TestConfigureResolversRejectsInjectedDirectives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	originalPath := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = originalPath })

	err := ConfigureResolvers("8.8.8.8\nsearch evil.example")
	if err == nil {
		t.Fatal("expected invalid resolver error")
	}
	if !strings.Contains(err.Error(), "invalid DNS resolver") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("resolv.conf should not be written for invalid resolver, stat error: %v", statErr)
	}
}

func TestResolverLines(t *testing.T) {
	lines, err := ResolverLines("8.8.8.8, 1.1.1.1, ,2001:4860:4860::8888")
	if err != nil {
		t.Fatalf("ResolverLines: %v", err)
	}
	got := strings.Join(lines, "\n")
	want := "nameserver 8.8.8.8\nnameserver 1.1.1.1\nnameserver 2001:4860:4860::8888"
	if got != want {
		t.Fatalf("ResolverLines = %q, want %q", got, want)
	}
}

func TestIsFRRMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"underlay_subnet_only", Config{UnderlaySubnet: "10.0.0.0/24"}, false},
		{"asn_only", Config{ASN: 65000}, false},
		{"underlay_subnet_and_asn", Config{UnderlaySubnet: "10.0.0.0/24", ASN: 65000}, true},
		{"underlay_ip_and_asn", Config{UnderlayIP: "10.0.0.1", ASN: 65000}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsFRRMode(); got != tt.want {
				t.Errorf("IsFRRMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsStaticMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"with_static_ip", Config{StaticIP: "10.0.0.5/24"}, true},
		{"with_gateway_only", Config{StaticGateway: "10.0.0.1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsStaticMode(); got != tt.want {
				t.Errorf("IsStaticMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsBondMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"with_interfaces", Config{BondInterfaces: "eth0,eth1"}, true},
		{"with_bond_mode_only", Config{BondMode: "802.3ad"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsBondMode(); got != tt.want {
				t.Errorf("IsBondMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsVLANMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"with_vlans", Config{VLANs: []VLANConfig{{ID: 100, Parent: "eth0"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsVLANMode(); got != tt.want {
				t.Errorf("IsVLANMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseVLANs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []VLANConfig
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace", "  ", nil, false},
		{
			"single_minimal",
			"200:eno1",
			[]VLANConfig{{ID: 200, Parent: "eno1"}},
			false,
		},
		{
			"single_with_address",
			"200:eno1:10.200.0.42/24",
			[]VLANConfig{{ID: 200, Parent: "eno1", Address: "10.200.0.42/24"}},
			false,
		},
		{
			"single_with_gateway",
			"200:eno1:10.200.0.42/24:10.200.0.1",
			[]VLANConfig{{ID: 200, Parent: "eno1", Address: "10.200.0.42/24", Gateway: "10.200.0.1"}},
			false,
		},
		{
			"single_with_ipv6_address",
			"200:eno1:[2001:db8:200::42/64]",
			[]VLANConfig{{ID: 200, Parent: "eno1", Address: "2001:db8:200::42/64"}},
			false,
		},
		{
			"single_with_ipv6_address_and_gateway",
			"200:eno1:[2001:db8:200::42/64]:[2001:db8:200::1]",
			[]VLANConfig{{ID: 200, Parent: "eno1", Address: "2001:db8:200::42/64", Gateway: "2001:db8:200::1"}},
			false,
		},
		{
			"multi_vlan",
			"200:eno1:10.200.0.42/24,300:eno2",
			[]VLANConfig{
				{ID: 200, Parent: "eno1", Address: "10.200.0.42/24"},
				{ID: 300, Parent: "eno2"},
			},
			false,
		},
		{
			"multi_vlan_mixed_ipv4_ipv6",
			"200:eno1:10.200.0.42/24,300:eno2:[2001:db8:300::42/64]:[2001:db8:300::1]",
			[]VLANConfig{
				{ID: 200, Parent: "eno1", Address: "10.200.0.42/24"},
				{ID: 300, Parent: "eno2", Address: "2001:db8:300::42/64", Gateway: "2001:db8:300::1"},
			},
			false,
		},
		{"invalid_no_parent", "200", nil, true},
		{"invalid_empty_parent", "200:", nil, true},
		{"invalid_id_zero", "0:eth0", nil, true},
		{"invalid_id_high", "4095:eth0", nil, true},
		{"invalid_id_text", "abc:eth0", nil, true},
		{"invalid_unbracketed_ipv6_address", "200:eno1:2001:db8:200::42/64", nil, true},
		{"invalid_unbracketed_ipv6_gateway", "200:eno1:10.200.0.42/24:2001:db8:200::1", nil, true},
		{"invalid_unterminated_ipv6_bracket", "200:eno1:[2001:db8:200::42/64", nil, true},
		{"invalid_unmatched_ipv6_bracket", "200:eno1:2001:db8:200::42/64]", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVLANs(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseVLANs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("ParseVLANs() returned %d configs, want %d", len(got), len(tt.want))
					return
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("ParseVLANs()[%d] = %+v, want %+v", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestIsGoBGPMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"gobgp", Config{NetworkMode: "gobgp"}, true},
		{"other", Config{NetworkMode: "frr"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsGoBGPMode(); got != tt.want {
				t.Errorf("IsGoBGPMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigureResolvConfWritesSearchDomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	originalPath := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = originalPath })

	if err := ConfigureResolvConf([]string{"10.0.2.3"}, []string{"l2.example", "b.example"}); err != nil {
		t.Fatalf("ConfigureResolvConf: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if got, want := string(data), "nameserver 10.0.2.3\nsearch l2.example b.example\n"; got != want {
		t.Fatalf("resolv.conf = %q, want %q", got, want)
	}
	// A bad domain is left out, the rest is written.
	if err := ConfigureResolvConf([]string{"10.0.2.3"}, []string{"corp_lan.example", "bad domain", "ok.example"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(path); string(data) != "nameserver 10.0.2.3\nsearch ok.example\n" {
		t.Fatalf("resolv.conf = %q, want the valid domain only", data)
	}
	// No servers: no search line either.
	if err := ConfigureResolvConf(nil, []string{"ok.example"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(path); string(data) != "# no DNS servers\n" {
		t.Fatalf("resolv.conf without servers = %q", data)
	}
}

func TestValidSearchDomainLength(t *testing.T) {
	long := strings.Repeat("a.", 126) + "b" // 253 characters
	if !validSearchDomain(long) || validSearchDomain(long+"c") {
		t.Fatalf("validSearchDomain: 253 characters must pass, 254 must not")
	}
}
