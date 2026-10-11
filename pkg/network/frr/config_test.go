package frr

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/network"
)

func TestDeriveAddresses_DirectUnderlayIP(t *testing.T) {
	cfg := network.Config{
		UnderlayIP: "192.168.4.42",
		IPMIMAC:    "aa:bb:cc:dd:ee:ff",
	}
	underlay, overlay, mac, err := DeriveAddresses(&cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if underlay != "192.168.4.42" {
		t.Errorf("underlay = %q, want %q", underlay, "192.168.4.42")
	}
	if overlay != underlay {
		t.Errorf("overlay = %q, want %q (fallback to underlay)", overlay, underlay)
	}
	if mac != "02:54:cc:dd:ee:ff" {
		t.Errorf("mac = %q, want %q", mac, "02:54:cc:dd:ee:ff")
	}
}

func TestDeriveAddresses_FromIPMIOffset(t *testing.T) {
	cfg := network.Config{
		UnderlaySubnet: "192.168.4.0/24",
		OverlaySubnet:  "10.0.0.0/24",
		IPMISubnet:     "172.30.0.0/24",
		IPMIIP:         "172.30.0.42",
		IPMIMAC:        "aa:bb:cc:dd:ee:ff",
	}
	underlay, overlay, mac, err := DeriveAddresses(&cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if underlay != "192.168.4.42" {
		t.Errorf("underlay = %q, want %q", underlay, "192.168.4.42")
	}
	if overlay != "10.0.0.42" {
		t.Errorf("overlay = %q, want %q", overlay, "10.0.0.42")
	}
	if mac != "02:54:cc:dd:ee:ff" {
		t.Errorf("mac = %q, want %q", mac, "02:54:cc:dd:ee:ff")
	}
}

func TestDeriveAddresses_NoUnderlayInfo(t *testing.T) {
	cfg := network.Config{}
	_, _, _, err := DeriveAddresses(&cfg)
	if err == nil {
		t.Fatal("expected error for empty config")
	}
	if !strings.Contains(err.Error(), "underlay IP or") {
		t.Errorf("error = %q, want to contain 'underlay IP or'", err.Error())
	}
}

func TestDeriveAddresses_BridgeMACWithoutIPMI(t *testing.T) {
	tests := []struct {
		name     string
		underlay string
		ipmiMAC  string
		want     string
	}{
		{"ipv4_underlay", "10.0.0.1", "", "02:54:0a:00:00:01"},
		{"other_ipv4_underlay", "10.0.0.2", "", "02:54:0a:00:00:02"},
		{"invalid_ipmi_mac", "192.168.4.7", "invalid", "02:54:c0:a8:04:07"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := network.Config{UnderlayIP: tt.underlay, IPMIMAC: tt.ipmiMAC}
			_, _, mac, err := DeriveAddresses(&cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mac != tt.want {
				t.Errorf("mac = %q, want %q", mac, tt.want)
			}
		})
	}
}

func TestDeriveAddresses_BridgeMACOverride(t *testing.T) {
	tests := []struct {
		name      string
		bridgeMAC string
		want      string
		wantErr   bool
	}{
		{"netplan_macaddress_wins", "02:AA:BB:CC:DD:EE", "02:aa:bb:cc:dd:ee", false},
		{"invalid_macaddress", "not-a-mac", "", true},
		{"eui64_macaddress", "02:00:00:00:00:00:10:01", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := network.Config{UnderlayIP: "10.0.0.1", IPMIMAC: "aa:bb:cc:dd:ee:ff", BridgeMAC: tt.bridgeMAC}
			_, _, mac, err := DeriveAddresses(&cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if mac != tt.want {
				t.Errorf("mac = %q, want %q", mac, tt.want)
			}
		})
	}
}

func TestDeriveAddresses_InvalidUnderlayIPForBridgeMAC(t *testing.T) {
	tests := []struct {
		name      string
		underlay  string
		ipmiMAC   string
		bridgeMAC string
	}{
		{"not_an_ip", "not-an-ip", "", ""},
		{"ipv6", "2001:db8::1", "", ""},
		{"ipv6_with_ipmi_mac", "2001:db8::1", "aa:bb:cc:dd:ee:ff", ""},
		{"ipv6_with_bridge_mac", "2001:db8::1", "", "02:00:00:00:10:01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := network.Config{UnderlayIP: tt.underlay, IPMIMAC: tt.ipmiMAC, BridgeMAC: tt.bridgeMAC}
			if _, _, _, err := DeriveAddresses(&cfg); err == nil {
				t.Fatal("expected error for a non-IPv4 underlay IP")
			}
		})
	}
}

// TestBridgeMACUniqueStableLAA checks that hosts get distinct, stable,
// locally administered unicast MACs.
func TestBridgeMACUniqueStableLAA(t *testing.T) {
	tests := []struct {
		name    string
		ipmiMAC string
		ip      string
	}{
		{"ipmi_a", "aa:bb:cc:dd:ee:01", ""},
		{"ipmi_b", "aa:bb:cc:dd:ee:02", ""},
		{"ip_a", "", "10.0.0.11"},
		{"ip_b", "", "10.0.0.12"},
		{"ip_c", "", "10.0.1.11"},
	}
	seen := map[string]string{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			derive := func() string {
				if tt.ipmiMAC != "" {
					return DeriveBridgeMAC(tt.ipmiMAC)
				}
				mac, err := deriveBridgeMACFromIP(tt.ip)
				if err != nil {
					t.Fatalf("deriveBridgeMACFromIP: %v", err)
				}
				return mac
			}
			got := derive()
			if again := derive(); again != got {
				t.Errorf("not stable: %q then %q", got, again)
			}
			if prev, ok := seen[got]; ok {
				t.Errorf("mac %q of %s collides with %s", got, tt.name, prev)
			}
			seen[got] = tt.name
			hw, err := net.ParseMAC(got)
			if err != nil {
				t.Fatalf("invalid MAC %q: %v", got, err)
			}
			if hw[0]&0x02 == 0 {
				t.Errorf("mac %q: locally administered bit not set", got)
			}
			if hw[0]&0x01 != 0 {
				t.Errorf("mac %q: multicast bit set", got)
			}
		})
	}
}

func TestDeriveAddresses_InvalidSourceIP(t *testing.T) {
	cfg := network.Config{
		UnderlaySubnet: "192.168.4.0/24",
		IPMISubnet:     "172.30.0.0/24",
		IPMIIP:         "not-an-ip",
	}
	_, _, _, err := DeriveAddresses(&cfg)
	if err == nil {
		t.Fatal("expected error for invalid source IP")
	}
}

func TestDeriveAddresses_IPMIOutsideSubnet(t *testing.T) {
	cfg := network.Config{
		UnderlaySubnet: "192.168.4.0/24",
		IPMISubnet:     "172.30.0.0/24",
		IPMIIP:         "10.0.0.42",
	}
	_, _, _, err := DeriveAddresses(&cfg)
	if err == nil {
		t.Fatal("expected error for IPMI IP outside IPMI subnet")
	}
	if !strings.Contains(err.Error(), "derive underlay IP") ||
		!strings.Contains(err.Error(), "outside source subnet") {
		t.Fatalf("error = %q, want underlay derivation source subnet context", err.Error())
	}
}

func TestDeriveAddresses_InvalidSubnet(t *testing.T) {
	cfg := network.Config{
		UnderlaySubnet: "bad-subnet",
		IPMISubnet:     "172.30.0.0/24",
		IPMIIP:         "172.30.0.1",
	}
	_, _, _, err := DeriveAddresses(&cfg)
	if err == nil {
		t.Fatal("expected error for invalid subnet")
	}
}

func TestDeriveAddresses_InvalidOverlaySubnet(t *testing.T) {
	cfg := network.Config{
		UnderlayIP:    "10.0.0.1",
		OverlaySubnet: "bad",
		IPMISubnet:    "172.30.0.0/24",
		IPMIIP:        "172.30.0.1",
	}
	_, _, _, err := DeriveAddresses(&cfg)
	if err == nil {
		t.Fatal("expected error for invalid overlay subnet")
	}
}

func TestDeriveBridgeMAC(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"standard", "aa:bb:cc:dd:ee:ff", "02:54:cc:dd:ee:ff"},
		{"dashes", "aa-bb-cc-dd-ee-ff", "02:54:cc:dd:ee:ff"},
		{"too_short", "aa:bb", ""},
		{"invalid_hex", "aa:bb:cc:dd:ee:zz", ""},
		{"empty", "", ""},
		{"eui64", "aa:bb:cc:dd:ee:ff:00:11", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveBridgeMAC(tt.input)
			if got != tt.want {
				t.Errorf("DeriveBridgeMAC(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDeriveIPFromOffset(t *testing.T) {
	tests := []struct {
		name      string
		srcIP     string
		srcSubnet string
		tgtSubnet string
		want      string
	}{
		{"offset_42", "172.30.0.42", "172.30.0.0/24", "192.168.4.0/24", "192.168.4.42"},
		{"offset_1", "172.30.0.1", "172.30.0.0/24", "10.0.0.0/24", "10.0.0.1"},
		{"offset_254", "172.30.0.254", "172.30.0.0/24", "10.0.0.0/24", "10.0.0.254"},
		{"zero_offset_31", "172.30.0.0", "172.30.0.0/24", "10.0.0.0/31", "10.0.0.0"},
		// IPv6 → IPv6
		{"ipv6_offset", "fd00::a", "fd00::/64", "fd01::/64", "fd01::a"},
		{"ipv6_offset_1", "fd00::1", "fd00::/64", "fd01::/64", "fd01::1"},
		// IPv4 → IPv6 (cross-family)
		{"cross_ipv4_to_ipv6", "172.30.0.42", "172.30.0.0/24", "fd00::/64", "fd00::2a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveIPFromOffset(tt.srcIP, tt.srcSubnet, tt.tgtSubnet)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("DeriveIPFromOffset() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveIPFromOffset_Errors(t *testing.T) {
	tests := []struct {
		name      string
		srcIP     string
		srcSubnet string
		tgtSubnet string
	}{
		{"bad_ip", "not-an-ip", "10.0.0.0/24", "10.0.0.0/24"},
		{"bad_src_subnet", "10.0.0.1", "bad", "10.0.0.0/24"},
		{"bad_tgt_subnet", "10.0.0.1", "10.0.0.0/24", "bad"},
		{"source_outside_subnet", "10.0.0.42", "172.30.0.0/24", "192.168.4.0/24"},
		{"derived_outside_target_subnet", "172.30.1.42", "172.30.0.0/16", "10.0.0.0/24"},
		{"derived_network_address", "172.30.0.0", "172.30.0.0/24", "10.0.0.0/24"},
		{"derived_broadcast_address", "172.30.0.255", "172.30.0.0/24", "10.0.0.0/24"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DeriveIPFromOffset(tt.srcIP, tt.srcSubnet, tt.tgtSubnet)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRenderConfig_Basic(t *testing.T) {
	cfg := network.Config{
		ASN:     65000,
		VRFName: "Vrf_underlay",
	}
	conf, err := RenderConfig(&cfg, "192.168.4.42", "192.168.4.42", []string{"eth0", "eth1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []string{
		"router bgp 65000 vrf Vrf_underlay",
		"bgp router-id 192.168.4.42",
		"neighbor eth0 interface peer-group fabric",
		"neighbor eth1 interface peer-group fabric",
		"advertise-all-vni",
		"frr defaults datacenter",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("config missing %q", check)
		}
	}
}

func TestRenderConfig_Onefabric(t *testing.T) {
	cfg := network.Config{
		ASN:              65000,
		VRFName:          "Vrf_underlay",
		DCGWIPs:          "10.0.0.1,10.0.0.2",
		LeafASN:          65001,
		LocalASN:         65002,
		OverlayAggregate: "10.10.0.0/16",
		VPNRT:            "65000:100",
	}
	conf, err := RenderConfig(&cfg, "192.168.4.42", "192.168.4.42", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []string{
		"neighbor fabric local-as 65002 no-prepend replace-as",
		"neighbor 10.0.0.1 remote-as internal",
		"neighbor 10.0.0.2 remote-as internal",
		"aggregate-address 10.10.0.0/16",
		"route-target both 65000:100",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("onefabric config missing %q", check)
		}
	}
}

func TestRenderConfig_BGPAuthPassword(t *testing.T) {
	cfg := network.Config{
		ASN:             65000,
		VRFName:         "Vrf_underlay",
		DCGWIPs:         "10.0.0.1",
		BGPAuthPassword: "s3cret",
	}
	conf, err := RenderConfig(&cfg, "192.168.4.42", "192.168.4.42", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"neighbor fabric password s3cret",
		"neighbor 10.0.0.1 password s3cret",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("config missing BGP auth password directive %q:\n%s", want, conf)
		}
	}
}

func TestRenderConfig_BGPAuthPasswordTrimsOuterWhitespace(t *testing.T) {
	cfg := network.Config{
		ASN:             65000,
		VRFName:         "Vrf_underlay",
		DCGWIPs:         "10.0.0.1",
		BGPAuthPassword: "  s3cret  ",
	}
	conf, err := RenderConfig(&cfg, "192.168.4.42", "192.168.4.42", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(conf, "  s3cret  ") {
		t.Fatalf("config should not render untrimmed BGP auth password:\n%s", conf)
	}
	if !strings.Contains(conf, "neighbor fabric password s3cret") {
		t.Fatalf("config missing trimmed BGP auth password:\n%s", conf)
	}
}

func TestRenderConfig_BGPAuthPasswordRejectsWhitespaceAndControlCharacters(t *testing.T) {
	for _, password := range []string{
		"bad password",
		"bad\tpassword",
		"bad\npassword",
		"bad\x7fpassword",
	} {
		cfg := network.Config{
			ASN:             65000,
			VRFName:         "Vrf_underlay",
			BGPAuthPassword: password,
		}
		conf, err := RenderConfig(&cfg, "192.168.4.42", "192.168.4.42", []string{"eth0"})
		if err == nil {
			t.Fatalf("expected invalid BGP auth password error for %q, got config:\n%s", password, conf)
		}
		if strings.Contains(err.Error(), password) {
			t.Fatalf("error should not include BGP auth password value: %v", err)
		}
		if !strings.Contains(err.Error(), "normalize BGP auth password: bgp auth password") {
			t.Fatalf("error should include contextual lowercase BGP auth password message: %v", err)
		}
	}
}

func TestRenderConfig_EmptyBGPAuthPasswordOmitsPassword(t *testing.T) {
	cfg := network.Config{
		ASN:     65000,
		VRFName: "Vrf_underlay",
	}
	conf, err := RenderConfig(&cfg, "192.168.4.42", "192.168.4.42", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(conf, " password ") {
		t.Fatalf("config should not contain BGP password directive when auth is empty:\n%s", conf)
	}
}

func TestRenderConfig_NoNICs(t *testing.T) {
	cfg := network.Config{
		ASN:     65000,
		VRFName: "Vrf_test",
	}
	conf, err := RenderConfig(&cfg, "10.0.0.1", "10.0.0.1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(conf, "router bgp 65000 vrf Vrf_test") {
		t.Error("config missing router bgp line")
	}
}

func TestRenderConfig_NoVRF(t *testing.T) {
	cfg := network.Config{
		ASN: 65020,
	}
	conf, err := RenderConfig(&cfg, "10.0.0.20", "10.0.0.20", []string{"eth1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(conf, "router bgp 65020\n") {
		t.Errorf("expected 'router bgp 65020' without VRF, got:\n%s", conf)
	}
	if strings.Contains(conf, "vrf") {
		t.Errorf("config should not contain VRF reference when VRFName is empty:\n%s", conf)
	}
}

func TestRenderConfig_IPv6Overlay(t *testing.T) {
	cfg := network.Config{
		ASN:     64497,
		VRFName: "p_zerotrust",
	}
	conf, err := RenderConfig(&cfg, "10.50.0.42", "fd21:0cc2:0981::2a", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []string{
		"address-family ipv4 unicast",
		"address-family ipv6 unicast",
		"address-family l2vpn evpn",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("IPv6 overlay config missing %q:\n%s", check, conf)
		}
	}

	// Without onefabric (no DCGWIPs), advertise lines should NOT be present.
	noChecks := []string{
		"advertise ipv4 unicast",
		"advertise ipv6 unicast",
	}
	for _, check := range noChecks {
		if strings.Contains(conf, check) {
			t.Errorf("IPv6 overlay config without onefabric should NOT contain %q:\n%s", check, conf)
		}
	}
}

func TestRenderConfig_IPv4OnlyNoIPv6AF(t *testing.T) {
	cfg := network.Config{
		ASN: 64497,
	}
	conf, err := RenderConfig(&cfg, "10.50.0.42", "10.50.0.42", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(conf, "address-family ipv6 unicast") {
		t.Errorf("IPv4-only config should NOT contain address-family ipv6 unicast:\n%s", conf)
	}
	if strings.Contains(conf, "advertise ipv6 unicast") {
		t.Errorf("IPv4-only config should NOT contain advertise ipv6 unicast:\n%s", conf)
	}
}

func TestRenderConfig_VBM4XParams(t *testing.T) {
	cfg := network.Config{
		ASN:           64497,
		VRFName:       "p_zerotrust",
		VRFTableID:    10,
		DCGWIPs:       "10.10.10.1,10.10.10.2",
		LeafASN:       64498,
		LocalASN:      65500,
		VPNRT:         "64497:1000",
		BGPKeepalive:  30,
		BGPHold:       90,
		BFDTransmitMS: 150,
		BFDReceiveMS:  150,
	}
	conf, err := RenderConfig(&cfg, "10.50.0.42", "10.50.0.42", []string{"swp0", "swp1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []string{
		"router bgp 64497 vrf p_zerotrust",
		"timers bgp 30 90",
		"profile datacenter",
		"transmit-interval 150",
		"receive-interval 150",
		"neighbor fabric bfd profile datacenter",
		"neighbor fabric local-as 65500 no-prepend replace-as",
		"neighbor swp0 interface peer-group fabric",
		"neighbor swp1 interface peer-group fabric",
		"neighbor 10.10.10.1 remote-as internal",
		"neighbor 10.10.10.1 bfd profile datacenter",
		"neighbor 10.10.10.2 remote-as internal",
		"neighbor 10.10.10.2 bfd profile datacenter",
		"route-target both 64497:1000",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("vbm4x config missing %q:\n%s", check, conf)
		}
	}
}

func TestRenderConfig_DualStack(t *testing.T) {
	cfg := network.Config{
		ASN:           64497,
		VRFName:       "p_zerotrust",
		VRFTableID:    10,
		DCGWIPs:       "10.10.10.1",
		VPNRT:         "64497:1000",
		BGPKeepalive:  30,
		BGPHold:       90,
		BFDTransmitMS: 150,
		BFDReceiveMS:  150,
	}
	conf, err := RenderConfig(&cfg, "10.50.0.42", "fd21:0cc2:0981::2a", []string{"eth0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []string{
		"address-family ipv4 unicast",
		"address-family ipv6 unicast",
		"address-family l2vpn evpn",
		"advertise ipv4 unicast",
		"advertise ipv6 unicast",
		"timers bgp 30 90",
		"profile datacenter",
		"route-target both 64497:1000",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("dual-stack config missing %q:\n%s", check, conf)
		}
	}
}

func TestFRRConfigBuilder_Basic(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithNICs([]string{"eth0"}).
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn")

	conf := b.Build()

	checks := []string{
		"frr version 10.3",
		"frr defaults datacenter",
		"router bgp 65000",
		"bgp router-id 10.0.0.1",
		"no bgp default ipv4-unicast",
		"neighbor fabric peer-group",
		"neighbor fabric remote-as external",
		"neighbor eth0 interface peer-group fabric",
		"address-family ipv4 unicast",
		"address-family l2vpn evpn",
		"advertise-all-vni",
		"line vty",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("builder config missing %q:\n%s", check, conf)
		}
	}
}

func TestFRRConfigBuilder_WithBFDAndTimers(t *testing.T) {
	b := NewFRRConfigBuilder(64497, "10.50.0.42").
		WithVRF("p_zerotrust", 10).
		WithNICs([]string{"swp0"}).
		WithBGPTimers(30, 90).
		WithBFDProfile("datacenter", 150, 150).
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn")

	conf := b.Build()

	checks := []string{
		"bfd",
		"profile datacenter",
		"transmit-interval 150",
		"receive-interval 150",
		"timers bgp 30 90",
		"neighbor fabric bfd profile datacenter",
		"router bgp 64497 vrf p_zerotrust",
	}
	for _, check := range checks {
		if !strings.Contains(conf, check) {
			t.Errorf("BFD/timers config missing %q:\n%s", check, conf)
		}
	}
}

func TestFRRConfigBuilder_WithAuthPassword(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithAuthPassword("hunter2").
		WithOnefabric([]string{"10.0.0.2"}, "10.10.0.0/16", "65000:100").
		WithNICs([]string{"eth0"}).
		WithAddressFamily("ipv4", "unicast")

	conf := b.Build()

	for _, want := range []string{
		"neighbor fabric password hunter2",
		"neighbor 10.0.0.2 password hunter2",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("builder config missing BGP auth password directive %q:\n%s", want, conf)
		}
	}
}

func TestFRRConfigBuilder_InvalidAuthPasswordDoesNotRender(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithAuthPassword("bad\nneighbor 10.0.0.3 remote-as internal").
		WithNICs([]string{"eth0"}).
		WithAddressFamily("ipv4", "unicast")

	conf := b.Build()

	if strings.Contains(conf, "bad") || strings.Contains(conf, "10.0.0.3") || strings.Contains(conf, " password ") {
		t.Fatalf("builder config should not render invalid BGP auth password:\n%s", conf)
	}
}

func TestFRRConfigBuilder_EmptyAuthPasswordOmitsPassword(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithAuthPassword("").
		WithNICs([]string{"eth0"}).
		WithAddressFamily("ipv4", "unicast")

	conf := b.Build()

	if strings.Contains(conf, " password ") {
		t.Fatalf("builder config should not contain BGP password directive when auth is empty:\n%s", conf)
	}
}

func TestFRRConfigBuilder_NoTimersNoSection(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn")
	conf := b.Build()

	if strings.Contains(conf, "timers bgp") {
		t.Errorf("expected no timers bgp when keepalive/hold are 0:\n%s", conf)
	}
	if strings.Contains(conf, "bfd") {
		t.Errorf("expected no BFD section when not configured:\n%s", conf)
	}
}

func TestFRRConfigBuilder_IPv6Advertise(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("ipv6", "unicast").
		WithAddressFamily("l2vpn", "evpn")
	conf := b.Build()

	if !strings.Contains(conf, "advertise ipv4 unicast") {
		t.Errorf("expected 'advertise ipv4 unicast' in EVPN AF:\n%s", conf)
	}
	if !strings.Contains(conf, "advertise ipv6 unicast") {
		t.Errorf("expected 'advertise ipv6 unicast' in EVPN AF:\n%s", conf)
	}
}

func TestFRRConfigBuilder_IPv4OnlyNoIPv6Advertise(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn")
	conf := b.Build()

	if strings.Contains(conf, "advertise ipv6 unicast") {
		t.Errorf("should not advertise ipv6 when only ipv4 is configured:\n%s", conf)
	}
}

// frrValidator tracks state while scanning FRR config lines.
type frrValidator struct {
	issues           []string
	inRouterBGP      bool
	inAddressFamily  bool
	routerBGPClosed  bool
	inBFD            bool
	afCount          int
	afClosed         int
	peerGroupDefined map[string]bool
	peerGroupUsed    map[string]bool
	bfdProfileDef    map[string]bool
	bfdProfileUsed   map[string]bool
}

func newFRRValidator() *frrValidator {
	return &frrValidator{
		peerGroupDefined: map[string]bool{},
		peerGroupUsed:    map[string]bool{},
		bfdProfileDef:    map[string]bool{},
		bfdProfileUsed:   map[string]bool{},
	}
}

func (v *frrValidator) processBFDLine(trimmed string) {
	if trimmed == "bfd" && !v.inRouterBGP {
		v.inBFD = true
	}
	if !v.inBFD {
		return
	}
	if strings.HasPrefix(trimmed, "profile ") {
		v.bfdProfileDef[strings.TrimPrefix(trimmed, "profile ")] = true
	}
	if trimmed == "exit" {
		v.inBFD = false
	}
}

func (v *frrValidator) processRouterBGPLine(trimmed string, lineNum int) {
	// Peer-group definition: "neighbor X peer-group" (not "interface peer-group").
	if strings.HasSuffix(trimmed, " peer-group") && !strings.Contains(trimmed, "interface peer-group") {
		pg := strings.TrimPrefix(trimmed, "neighbor ")
		pg = strings.TrimSuffix(pg, " peer-group")
		v.peerGroupDefined[pg] = true
	}

	// Peer-group usage: "neighbor X interface peer-group Y".
	if idx := strings.Index(trimmed, "interface peer-group "); idx >= 0 {
		pgName := strings.TrimSpace(trimmed[idx+len("interface peer-group "):])
		v.peerGroupUsed[pgName] = true
	}

	// BFD profile reference inside router bgp.
	if idx := strings.Index(trimmed, "bfd profile "); idx >= 0 {
		v.bfdProfileUsed[strings.TrimSpace(trimmed[idx+len("bfd profile "):])] = true
	}

	// Address-family open/close.
	if strings.HasPrefix(trimmed, "address-family ") {
		v.afCount++
		v.inAddressFamily = true
	}
	if trimmed == "exit-address-family" {
		if !v.inAddressFamily {
			v.issues = append(v.issues, fmt.Sprintf("line %d: 'exit-address-family' without matching 'address-family'", lineNum))
		}
		v.inAddressFamily = false
		v.afClosed++
	}

	// "exit" closes the router bgp block (but not address-family).
	if trimmed == "exit" && !v.inAddressFamily {
		v.inRouterBGP = false
		v.routerBGPClosed = true
	}
}

// validateFRRConfig performs structural validation of generated FRR config text.
// It returns a list of issues found. An empty list means the config is valid.
func validateFRRConfig(conf string) []string {
	v := newFRRValidator()
	lines := strings.Split(conf, "\n")

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		v.processBFDLine(trimmed)

		// Detect router bgp block entry.
		if strings.HasPrefix(trimmed, "router bgp ") && !v.inRouterBGP {
			if v.routerBGPClosed {
				v.issues = append(v.issues, fmt.Sprintf("line %d: multiple 'router bgp' blocks", lineNum))
			}
			v.inRouterBGP = true
		}

		if v.inRouterBGP {
			v.processRouterBGPLine(trimmed, lineNum)
		}
	}

	// Check for unclosed blocks.
	if v.inRouterBGP {
		v.issues = append(v.issues, "router bgp block not closed with 'exit'")
	}
	if v.inAddressFamily {
		v.issues = append(v.issues, "address-family block not closed with 'exit-address-family'")
	}
	if v.afCount != v.afClosed {
		v.issues = append(v.issues, fmt.Sprintf("%d address-family opened but %d closed", v.afCount, v.afClosed))
	}

	// Check peer-group references.
	for pg := range v.peerGroupUsed {
		if !v.peerGroupDefined[pg] {
			v.issues = append(v.issues, fmt.Sprintf("peer-group %q used but never defined with 'neighbor %s peer-group'", pg, pg))
		}
	}

	// Check BFD profile references.
	for prof := range v.bfdProfileUsed {
		if !v.bfdProfileDef[prof] {
			v.issues = append(v.issues, fmt.Sprintf("BFD profile %q referenced but never defined", prof))
		}
	}

	// Must have line vty footer.
	if !strings.Contains(conf, "line vty") {
		v.issues = append(v.issues, "missing 'line vty' footer")
	}

	// Must have frr header.
	if !strings.HasPrefix(conf, "frr version") {
		v.issues = append(v.issues, "missing 'frr version' header")
	}

	return v.issues
}

// TestValidateConfig_AllScenarios runs the config builder through every
// meaningful scenario and validates the output is structurally valid FRR config.
func TestValidateConfig_AllScenarios(t *testing.T) {
	tests := []struct {
		name    string
		builder *FRRConfigBuilder
	}{
		{
			name: "minimal",
			builder: NewFRRConfigBuilder(65000, "10.0.0.1").
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn"),
		},
		{
			name: "single_nic",
			builder: NewFRRConfigBuilder(65000, "10.0.0.1").
				WithNICs([]string{"eth0"}).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn"),
		},
		{
			name: "multi_nic",
			builder: NewFRRConfigBuilder(65000, "10.0.0.1").
				WithNICs([]string{"eth0", "eth1", "eth2", "swp0"}).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn"),
		},
		{
			name: "with_vrf",
			builder: NewFRRConfigBuilder(64497, "10.50.0.42").
				WithVRF("p_zerotrust", 10).
				WithNICs([]string{"eth0"}).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn"),
		},
		{
			name: "with_bfd_and_timers",
			builder: NewFRRConfigBuilder(64497, "10.50.0.42").
				WithVRF("p_zerotrust", 10).
				WithNICs([]string{"swp0", "swp1"}).
				WithBGPTimers(30, 90).
				WithBFDProfile("datacenter", 150, 150).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn"),
		},
		{
			name: "ipv6_dual_stack",
			builder: NewFRRConfigBuilder(64497, "10.50.0.42").
				WithVRF("p_zerotrust", 10).
				WithNICs([]string{"eth0"}).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("ipv6", "unicast").
				WithAddressFamily("l2vpn", "evpn"),
		},
		{
			name: "onefabric",
			builder: NewFRRConfigBuilder(65000, "192.168.4.42").
				WithVRF("Vrf_underlay", 1).
				WithNICs([]string{"eth0"}).
				WithBGPTimers(3, 9).
				WithBFDProfile("datacenter", 300, 300).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn").
				WithOnefabric([]string{"10.0.0.1", "10.0.0.2"}, "10.10.0.0/16", "65000:100"),
		},
		{
			name: "onefabric_single_dcgw",
			builder: NewFRRConfigBuilder(65000, "192.168.4.42").
				WithVRF("Vrf_underlay", 1).
				WithNICs([]string{"eth0", "eth1"}).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("l2vpn", "evpn").
				WithOnefabric([]string{"10.0.0.1"}, "10.10.0.0/16", "65000:100"),
		},
		{
			name: "full_vbm4x",
			builder: NewFRRConfigBuilder(64497, "10.50.0.42").
				WithVRF("p_zerotrust", 10).
				WithNICs([]string{"eth0", "eth1"}).
				WithBGPTimers(30, 90).
				WithBFDProfile("datacenter", 150, 150).
				WithAddressFamily("ipv4", "unicast").
				WithAddressFamily("ipv6", "unicast").
				WithAddressFamily("l2vpn", "evpn").
				WithOnefabric([]string{"10.10.10.1", "10.10.10.2"}, "10.10.0.0/16", "64497:1000"),
		},
		{
			name: "no_evpn",
			builder: NewFRRConfigBuilder(65000, "10.0.0.1").
				WithNICs([]string{"eth0"}).
				WithAddressFamily("ipv4", "unicast"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := tt.builder.Build()
			issues := validateFRRConfig(conf)
			if len(issues) > 0 {
				t.Errorf("config validation failed:\n%s\n\nGenerated config:\n%s",
					strings.Join(issues, "\n"), conf)
			}
		})
	}
}

// TestValidateConfig_PeerGroupBeforeNeighbor checks that the peer-group is
// defined before any neighbor references it.
func TestValidateConfig_PeerGroupBeforeNeighbor(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "10.0.0.1").
		WithNICs([]string{"eth0", "eth1", "eth2"}).
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn")
	conf := b.Build()

	pgIdx := strings.Index(conf, "neighbor fabric peer-group")
	if pgIdx == -1 {
		t.Fatal("peer-group definition not found")
	}

	for _, nic := range []string{"eth0", "eth1", "eth2"} {
		nicLine := fmt.Sprintf("neighbor %s interface peer-group fabric", nic)
		nicIdx := strings.Index(conf, nicLine)
		if nicIdx == -1 {
			t.Errorf("neighbor %s not found in config", nic)
			continue
		}
		if nicIdx < pgIdx {
			t.Errorf("neighbor %s (pos %d) appears before peer-group definition (pos %d):\n%s",
				nic, nicIdx, pgIdx, conf)
		}
	}
}

// TestValidateConfig_OnefabricInsideRouterBGP checks that onefabric DCGW
// neighbors are inside the router bgp block, not after exit.
func TestValidateConfig_OnefabricInsideRouterBGP(t *testing.T) {
	b := NewFRRConfigBuilder(65000, "192.168.4.42").
		WithVRF("Vrf_underlay", 1).
		WithNICs([]string{"eth0"}).
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn").
		WithOnefabric([]string{"10.0.0.1", "10.0.0.2"}, "10.10.0.0/16", "65000:100")
	conf := b.Build()

	// Find the router bgp block boundaries.
	routerIdx := strings.Index(conf, "router bgp ")
	exitIdx := strings.LastIndex(conf, "exit\n!\nline vty")
	if routerIdx == -1 || exitIdx == -1 {
		t.Fatalf("could not find router bgp or exit markers in:\n%s", conf)
	}

	for _, dcgwIP := range []string{"10.0.0.1", "10.0.0.2"} {
		neighborLine := fmt.Sprintf("neighbor %s remote-as internal", dcgwIP)
		neighborIdx := strings.Index(conf, neighborLine)
		if neighborIdx == -1 {
			t.Errorf("DCGW neighbor %s not found", dcgwIP)
			continue
		}
		if neighborIdx < routerIdx || neighborIdx > exitIdx {
			t.Errorf("DCGW neighbor %s (pos %d) is outside router bgp block (%d-%d):\n%s",
				dcgwIP, neighborIdx, routerIdx, exitIdx, conf)
		}
	}
}

// TestValidateConfig_BFDBeforeRouterBGP checks that BFD profile is defined
// before the router bgp block.
func TestValidateConfig_BFDBeforeRouterBGP(t *testing.T) {
	b := NewFRRConfigBuilder(64497, "10.50.0.42").
		WithBFDProfile("datacenter", 150, 150).
		WithNICs([]string{"eth0"}).
		WithAddressFamily("ipv4", "unicast").
		WithAddressFamily("l2vpn", "evpn")
	conf := b.Build()

	bfdIdx := strings.Index(conf, "bfd\n")
	routerIdx := strings.Index(conf, "router bgp ")
	if bfdIdx == -1 {
		t.Fatal("BFD section not found")
	}
	if routerIdx == -1 {
		t.Fatal("router bgp not found")
	}
	if bfdIdx > routerIdx {
		t.Errorf("BFD (pos %d) must appear before router bgp (pos %d):\n%s",
			bfdIdx, routerIdx, conf)
	}
}

// TestValidateConfig_ViaRenderConfig validates configs produced by the
// higher-level RenderConfig function (the one actually used at runtime).
func TestValidateConfig_ViaRenderConfig(t *testing.T) {
	tests := []struct {
		name       string
		cfg        network.Config
		underlayIP string
		overlayIP  string
		nics       []string
	}{
		{
			name:       "basic_ipv4",
			cfg:        network.Config{ASN: 65000, VRFName: "Vrf_underlay"},
			underlayIP: "192.168.4.42",
			overlayIP:  "192.168.4.42",
			nics:       []string{"eth0", "eth1"},
		},
		{
			name: "onefabric",
			cfg: network.Config{
				ASN: 65000, VRFName: "Vrf_underlay",
				DCGWIPs: "10.0.0.1,10.0.0.2", VPNRT: "65000:100",
				OverlayAggregate: "10.10.0.0/16",
			},
			underlayIP: "192.168.4.42",
			overlayIP:  "192.168.4.42",
			nics:       []string{"eth0"},
		},
		{
			name: "dual_stack_with_bfd",
			cfg: network.Config{
				ASN: 64497, VRFName: "p_zerotrust", VRFTableID: 10,
				BGPKeepalive: 30, BGPHold: 90,
				BFDTransmitMS: 150, BFDReceiveMS: 150,
				DCGWIPs: "10.10.10.1", VPNRT: "64497:1000",
			},
			underlayIP: "10.50.0.42",
			overlayIP:  "fd21:0cc2:0981::2a",
			nics:       []string{"eth0", "eth1"},
		},
		{
			name:       "no_nics",
			cfg:        network.Config{ASN: 65000},
			underlayIP: "10.0.0.1",
			overlayIP:  "10.0.0.1",
			nics:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf, err := RenderConfig(&tt.cfg, tt.underlayIP, tt.overlayIP, tt.nics)
			if err != nil {
				t.Fatalf("RenderConfig error: %v", err)
			}
			issues := validateFRRConfig(conf)
			if len(issues) > 0 {
				t.Errorf("config validation failed:\n%s\n\nGenerated config:\n%s",
					strings.Join(issues, "\n"), conf)
			}
		})
	}
}
