//go:build linux

package gobgp

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/network"
)

func TestNewConfig(t *testing.T) {
	tests := []struct {
		name    string
		netCfg  *network.Config
		wantErr bool
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name: "minimal_unnumbered",
			netCfg: &network.Config{
				UnderlayIP:   "10.0.0.1",
				ASN:          65000,
				ProvisionVNI: 4000,
				BGPPeerMode:  network.PeerModeUnnumbered,
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.RouterID != "10.0.0.1" {
					t.Errorf("RouterID = %q, want 10.0.0.1", cfg.RouterID)
				}
				if cfg.ASN != 65000 {
					t.Errorf("ASN = %d, want 65000", cfg.ASN)
				}
				if cfg.ProvisionVNI != 4000 {
					t.Errorf("ProvisionVNI = %d, want 4000", cfg.ProvisionVNI)
				}
				if cfg.ListenPort != 179 {
					t.Errorf("ListenPort = %d, want 179 (default)", cfg.ListenPort)
				}
				if cfg.MTU != 9000 {
					t.Errorf("MTU = %d, want 9000 (default)", cfg.MTU)
				}
			},
		},
		{
			name: "explicit_vpn_rt",
			netCfg: &network.Config{
				UnderlayIP:   "192.168.4.10",
				ASN:          65100,
				ProvisionVNI: 1000,
				VPNRT:        "65000:1000",
				BGPPeerMode:  network.PeerModeUnnumbered,
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.VPNRT != "65000:1000" {
					t.Errorf("VPNRT = %q, want 65000:1000", cfg.VPNRT)
				}
			},
		},
		{
			name: "invalid_vpn_rt",
			netCfg: &network.Config{
				UnderlayIP:   "192.168.4.10",
				ASN:          65100,
				ProvisionVNI: 1000,
				VPNRT:        "65000",
				BGPPeerMode:  network.PeerModeUnnumbered,
			},
			wantErr: true,
		},
		{
			name: "dual_with_neighbors",
			netCfg: &network.Config{
				UnderlayIP:   "10.0.0.5",
				ASN:          65000,
				ProvisionVNI: 4000,
				BGPPeerMode:  network.PeerModeDual,
				BGPNeighbors: "10.0.0.1,10.0.0.2",
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.PeerMode != network.PeerModeDual {
					t.Errorf("PeerMode = %q, want dual", cfg.PeerMode)
				}
				if len(cfg.NeighborAddrs) != 2 {
					t.Errorf("NeighborAddrs = %d, want 2", len(cfg.NeighborAddrs))
				}
			},
		},
		{
			name: "overlay_same_as_underlay",
			netCfg: &network.Config{
				UnderlayIP:   "192.168.1.10",
				ASN:          65100,
				ProvisionVNI: 100,
				BGPPeerMode:  network.PeerModeUnnumbered,
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.OverlayIP != "192.168.1.10" {
					t.Errorf("OverlayIP = %q, want same as RouterID", cfg.OverlayIP)
				}
			},
		},
		{
			name: "missing_underlay_ip",
			netCfg: &network.Config{
				ASN:          65000,
				ProvisionVNI: 4000,
			},
			wantErr: true,
		},
		{
			name: "missing_asn",
			netCfg: &network.Config{
				UnderlayIP:   "10.0.0.1",
				ProvisionVNI: 4000,
			},
			wantErr: true,
		},
		{
			name: "numbered_without_neighbors",
			netCfg: &network.Config{
				UnderlayIP:   "10.0.0.1",
				ASN:          65000,
				ProvisionVNI: 4000,
				BGPPeerMode:  network.PeerModeNumbered,
			},
			wantErr: true,
		},
		{
			name: "custom_timers",
			netCfg: &network.Config{
				UnderlayIP:   "10.0.0.1",
				ASN:          65000,
				ProvisionVNI: 4000,
				BGPPeerMode:  network.PeerModeUnnumbered,
				BGPKeepalive: 10,
				BGPHold:      30,
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.KeepaliveInterval != 10 {
					t.Errorf("KeepaliveInterval = %d, want 10", cfg.KeepaliveInterval)
				}
				if cfg.HoldTime != 30 {
					t.Errorf("HoldTime = %d, want 30", cfg.HoldTime)
				}
			},
		},
		{
			name: "reject_bfd_transmit_only",
			netCfg: &network.Config{
				UnderlayIP:     "10.0.0.1",
				ASN:            65000,
				ProvisionVNI:   4000,
				BGPPeerMode:    network.PeerModeUnnumbered,
				BFDTransmitMS:  150,
				BGPUnderlayAF:  "ipv4",
				BGPOverlayType: "evpn-vxlan",
			},
			wantErr: true,
		},
		{
			name: "reject_bfd_receive_only",
			netCfg: &network.Config{
				UnderlayIP:     "10.0.0.1",
				ASN:            65000,
				ProvisionVNI:   4000,
				BGPPeerMode:    network.PeerModeUnnumbered,
				BFDReceiveMS:   150,
				BGPUnderlayAF:  "ipv4",
				BGPOverlayType: "evpn-vxlan",
			},
			wantErr: true,
		},
		{
			name: "accept_bfd_pair",
			netCfg: &network.Config{
				UnderlayIP:     "10.0.0.1",
				ASN:            65000,
				ProvisionVNI:   4000,
				BGPPeerMode:    network.PeerModeUnnumbered,
				BFDTransmitMS:  150,
				BFDReceiveMS:   150,
				BGPUnderlayAF:  "ipv4",
				BGPOverlayType: "evpn-vxlan",
				EVPNType5Only:  true,
			},
		},
		{
			name: "supported_underlay_af_and_overlay_type_wired",
			netCfg: &network.Config{
				UnderlayIP:     "10.0.0.1",
				ASN:            65000,
				ProvisionVNI:   4000,
				BGPPeerMode:    network.PeerModeUnnumbered,
				BGPUnderlayAF:  "ipv4",
				BGPOverlayType: "evpn-vxlan",
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.UnderlayAF != "ipv4" {
					t.Errorf("UnderlayAF = %q, want ipv4", cfg.UnderlayAF)
				}
				if cfg.OverlayType != "evpn-vxlan" {
					t.Errorf("OverlayType = %q, want evpn-vxlan", cfg.OverlayType)
				}
			},
		},
		{
			name: "auth_password_flows_from_network_config",
			netCfg: &network.Config{
				UnderlayIP:      "10.0.0.1",
				ASN:             65000,
				ProvisionVNI:    4000,
				BGPPeerMode:     network.PeerModeUnnumbered,
				BGPAuthPassword: "s3cret",
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.AuthPassword != "s3cret" {
					t.Errorf("AuthPassword = %q, want s3cret", cfg.AuthPassword)
				}
			},
		},
		{
			name: "auth_password_empty_by_default",
			netCfg: &network.Config{
				UnderlayIP:   "10.0.0.1",
				ASN:          65000,
				ProvisionVNI: 4000,
				BGPPeerMode:  network.PeerModeUnnumbered,
			},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.AuthPassword != "" {
					t.Errorf("AuthPassword = %q, want empty (no auth)", cfg.AuthPassword)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.netCfg.ProvisionGateway == "" {
				tt.netCfg.ProvisionGateway = testProvisionGateway
			}
			cfg, err := NewConfig(tt.netCfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestNewConfigAllowsMissingProvisionGatewayForType5Only(t *testing.T) {
	cfg, err := NewConfig(&network.Config{
		UnderlayIP:    "10.0.0.1",
		ASN:           65000,
		ProvisionVNI:  4000,
		BGPPeerMode:   network.PeerModeUnnumbered,
		EVPNType5Only: true,
	})
	if err != nil {
		t.Fatalf("NewConfig() error = %v, want Type-5-only config accepted", err)
	}
	if !cfg.Type5Only || !cfg.usesType5Only() {
		t.Fatal("explicit Type-5-only mode was not preserved")
	}
}

func TestNewConfigType5OnlyAndL2AreMutuallyExclusive(t *testing.T) {
	_, err := NewConfig(&network.Config{
		UnderlayIP:       "10.0.0.1",
		ASN:              65000,
		ProvisionVNI:     4000,
		ProvisionGateway: testProvisionGateway,
		BGPPeerMode:      network.PeerModeUnnumbered,
		EVPNL2Enabled:    true,
		EVPNType5Only:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot both be enabled") {
		t.Fatalf("NewConfig() error = %v, want mutually exclusive mode error", err)
	}
}

func TestConfigUsesExplicitType5OnlyMode(t *testing.T) {
	if (&Config{}).usesType5Only() {
		t.Fatal("Type-5-only mode must require an explicit configuration")
	}
	if !(&Config{Type5Only: true}).usesType5Only() {
		t.Fatal("explicit Type5Only configuration was ignored")
	}
}

func TestValidateProvisioningRejectsBoundaryErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "VNI below range", cfg: Config{}},
		{name: "VNI above range", cfg: Config{ProvisionVNI: 16777216}},
		{name: "four octet ASN oversized VNI", cfg: Config{ASN: 65536, ProvisionVNI: 65536}},
		{name: "MTU below VXLAN minimum", cfg: Config{ProvisionVNI: 1, MTU: 625}},
		{name: "legacy gateway omitted", cfg: Config{ProvisionVNI: 1, OverlayType: string(OverlayEVPNVXLAN)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.validateProvisioning(); err == nil {
				t.Fatal("validateProvisioning() error = nil, want error")
			}
		})
	}
	if err := (&Config{ProvisionVNI: 1, Type5Only: true, MTU: 626}).validateProvisioning(); err != nil {
		t.Fatalf("validateProvisioning() error = %v, want explicit Type-5-only config accepted", err)
	}
}

func TestNewConfigAllowsMissingProvisionGatewayWhenOverlayDisabled(t *testing.T) {
	_, err := NewConfig(&network.Config{
		UnderlayIP:     "10.0.0.1",
		ASN:            65000,
		ProvisionVNI:   4000,
		BGPPeerMode:    network.PeerModeUnnumbered,
		BGPOverlayType: string(OverlayNone),
	})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
}

func TestNewConfigRejectsInvalidProvisionGateway(t *testing.T) {
	_, err := NewConfig(&network.Config{
		UnderlayIP:        "10.0.0.1",
		ASN:               65000,
		ProvisionVNI:      4000,
		ProvisionGateway:  "fd00::1",
		BGPPeerMode:       network.PeerModeUnnumbered,
		BGPMinPeers:       1,
		BGPOverlayType:    string(OverlayEVPNVXLAN),
		BGPUnderlayAF:     "ipv4",
		OverlayVRFTableID: 1000,
	})
	if err == nil {
		t.Fatal("expected error for invalid provision gateway")
	}
	if !strings.Contains(err.Error(), "valid IPv4") {
		t.Fatalf("expected IPv4 gateway error, got %v", err)
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()

	if cfg.ListenPort != 179 {
		t.Errorf("ListenPort = %d, want 179", cfg.ListenPort)
	}
	if cfg.KeepaliveInterval != 3 {
		t.Errorf("KeepaliveInterval = %d, want 3", cfg.KeepaliveInterval)
	}
	if cfg.HoldTime != 9 {
		t.Errorf("HoldTime = %d, want 9", cfg.HoldTime)
	}
	if cfg.BridgeName != "br.provision" {
		t.Errorf("BridgeName = %q, want br.provision", cfg.BridgeName)
	}
	if cfg.MTU != 9000 {
		t.Errorf("MTU = %d, want 9000", cfg.MTU)
	}
	if cfg.VRFTableID != 1000 {
		t.Errorf("VRFTableID = %d, want 1000", cfg.VRFTableID)
	}
	if cfg.OverlayVRFTableID != 1000 {
		t.Errorf("OverlayVRFTableID = %d, want 1000", cfg.OverlayVRFTableID)
	}
}

func TestApplyDefaultsVRFTableIDOneOverridden(t *testing.T) {
	cfg := &Config{VRFTableID: 1}
	cfg.ApplyDefaults()

	if cfg.VRFTableID != 1000 {
		t.Errorf("VRFTableID = %d, want 1000 (table 1 conflicts with default)", cfg.VRFTableID)
	}
	if cfg.OverlayVRFTableID != 1000 {
		t.Errorf("OverlayVRFTableID = %d, want 1000", cfg.OverlayVRFTableID)
	}
}

func TestApplyDefaultsPreservesValues(t *testing.T) {
	cfg := &Config{
		ListenPort:        1179,
		KeepaliveInterval: 10,
		HoldTime:          30,
		BridgeName:        "custom-br",
		MTU:               1500,
		VRFTableID:        42,
		OverlayVRFTableID: 43,
	}
	cfg.ApplyDefaults()

	if cfg.ListenPort != 1179 {
		t.Errorf("ListenPort = %d, want 1179", cfg.ListenPort)
	}
	if cfg.KeepaliveInterval != 10 {
		t.Errorf("KeepaliveInterval = %d, want 10", cfg.KeepaliveInterval)
	}
	if cfg.HoldTime != 30 {
		t.Errorf("HoldTime = %d, want 30", cfg.HoldTime)
	}
	if cfg.BridgeName != "custom-br" {
		t.Errorf("BridgeName = %q, want custom-br", cfg.BridgeName)
	}
	if cfg.MTU != 1500 {
		t.Errorf("MTU = %d, want 1500", cfg.MTU)
	}
	if cfg.VRFTableID != 42 {
		t.Errorf("VRFTableID = %d, want 42", cfg.VRFTableID)
	}
	if cfg.OverlayVRFTableID != 43 {
		t.Errorf("OverlayVRFTableID = %d, want 43", cfg.OverlayVRFTableID)
	}
}

func TestApplyDefaultsOverlayVRFTableIDOneUsesUnderlayTable(t *testing.T) {
	cfg := &Config{VRFTableID: 42, OverlayVRFTableID: 1}
	cfg.ApplyDefaults()

	if cfg.OverlayVRFTableID != 42 {
		t.Errorf("OverlayVRFTableID = %d, want 42", cfg.OverlayVRFTableID)
	}
}

func TestNewConfigDefaultsOverlayVRFToUnderlayVRF(t *testing.T) {
	cfg, err := NewConfig(&network.Config{
		ASN:              65000,
		UnderlayIP:       "192.168.4.10",
		ProvisionVNI:     1000,
		ProvisionIP:      "10.200.0.10/24",
		ProvisionGateway: "192.168.4.1",
		VRFName:          "Vrf_underlay",
		BGPPeerMode:      network.PeerModeUnnumbered,
	})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if cfg.OverlayVRFName != "Vrf_underlay" {
		t.Errorf("OverlayVRFName = %q, want Vrf_underlay", cfg.OverlayVRFName)
	}
	if cfg.OverlayVRFTableID != cfg.VRFTableID {
		t.Errorf("OverlayVRFTableID = %d, want underlay table %d", cfg.OverlayVRFTableID, cfg.VRFTableID)
	}
}

func TestNewConfigHonorsExplicitDefaultOverlayVRF(t *testing.T) {
	cfg, err := NewConfig(&network.Config{
		ASN:              65000,
		UnderlayIP:       "192.168.4.10",
		ProvisionVNI:     1000,
		ProvisionIP:      "10.200.0.10/24",
		ProvisionGateway: "192.168.4.1",
		VRFName:          "Vrf_underlay",
		OverlayVRFSet:    true,
		OverlayVRFName:   "",
		BGPPeerMode:      network.PeerModeUnnumbered,
	})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if cfg.OverlayVRFName != "" {
		t.Errorf("OverlayVRFName = %q, want empty default VRF", cfg.OverlayVRFName)
	}
}

func TestNewConfigHonorsOverlayVRFTableID(t *testing.T) {
	cfg, err := NewConfig(&network.Config{
		ASN:               65000,
		UnderlayIP:        "192.168.4.10",
		ProvisionVNI:      1000,
		ProvisionIP:       "10.200.0.10/24",
		ProvisionGateway:  "192.168.4.1",
		VRFName:           "Vrf_underlay",
		VRFTableID:        10,
		OverlayVRFSet:     true,
		OverlayVRFName:    "Vrf_overlay",
		OverlayVRFTableID: 20,
		BGPPeerMode:       network.PeerModeUnnumbered,
	})
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if cfg.VRFTableID != 10 {
		t.Errorf("VRFTableID = %d, want 10", cfg.VRFTableID)
	}
	if cfg.OverlayVRFTableID != 20 {
		t.Errorf("OverlayVRFTableID = %d, want 20", cfg.OverlayVRFTableID)
	}
}

func TestApplyDefaultsPropagatesGracefulRestart(t *testing.T) {
	cfg := &Config{
		GracefulRestart: &GracefulRestartConfig{Enabled: true},
	}
	cfg.ApplyDefaults()

	if cfg.GracefulRestart.RestartTime != 120 {
		t.Errorf("GracefulRestart.RestartTime = %d, want 120", cfg.GracefulRestart.RestartTime)
	}
}

func TestValidateRequiresASN(t *testing.T) {
	cfg := &Config{RouterID: "10.0.0.1"}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for zero ASN")
	}
}

func TestValidateRequiresRouterID(t *testing.T) {
	cfg := &Config{ASN: 65000}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty RouterID")
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "10.0.0.1",
		PeerMode:            network.PeerModeUnnumbered,
		ProvisionVNI:        100,
		ProvisionGateway:    testProvisionGateway,
		MinEstablishedPeers: 1,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateRejectsUnsupportedUnderlayAF(t *testing.T) {
	tests := []struct {
		name     string
		underlay string
	}{
		{"ipv6", "ipv6"},
		{"dual_stack", "dual-stack"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				ASN:                 65000,
				RouterID:            "10.0.0.1",
				PeerMode:            network.PeerModeUnnumbered,
				ProvisionVNI:        100,
				ProvisionGateway:    testProvisionGateway,
				MinEstablishedPeers: 1,
				UnderlayAF:          tt.underlay,
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected unsupported underlay AF error, got nil")
			}
			if !strings.Contains(err.Error(), "not yet implemented") {
				t.Fatalf("expected not yet implemented error, got: %v", err)
			}
		})
	}
}

func TestValidateRejectsNonIPv4RouterID(t *testing.T) {
	tests := []struct {
		name     string
		routerID string
	}{
		{"ipv6", "fd00::1"},
		{"hostname", "router1.example.com"},
		{"garbage", "not-an-ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{ASN: 65000, RouterID: tt.routerID}
			if err := cfg.Validate(); err == nil {
				t.Errorf("expected error for RouterID %q", tt.routerID)
			}
		})
	}
}

func TestValidatePeerModeUnnumbered(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "10.0.0.1",
		PeerMode:            network.PeerModeUnnumbered,
		ProvisionVNI:        100,
		ProvisionGateway:    testProvisionGateway,
		MinEstablishedPeers: 1,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("unnumbered mode should not require neighbors: %v", err)
	}
}

func TestValidatePeerModeDualRequiresNeighbors(t *testing.T) {
	cfg := &Config{
		ASN:      65000,
		RouterID: "10.0.0.1",
		PeerMode: network.PeerModeDual,
	}
	if err := cfg.Validate(); err == nil {
		t.Error("dual mode without neighbors should fail validation")
	}
}

func TestValidatePeerModeNumberedRequiresNeighbors(t *testing.T) {
	cfg := &Config{
		ASN:      65000,
		RouterID: "10.0.0.1",
		PeerMode: network.PeerModeNumbered,
	}
	if err := cfg.Validate(); err == nil {
		t.Error("numbered mode without neighbors should fail validation")
	}
}

func TestValidatePeerModeDualWithNeighbors(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "10.0.0.1",
		PeerMode:            network.PeerModeDual,
		NeighborAddrs:       []string{"10.0.0.100", "10.0.0.101"},
		ProvisionVNI:        100,
		ProvisionGateway:    testProvisionGateway,
		MinEstablishedPeers: 1,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("dual mode with valid neighbors should pass: %v", err)
	}
}

func TestValidatePeerModeNumberedWithNeighbors(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "10.0.0.1",
		PeerMode:            network.PeerModeNumbered,
		NeighborAddrs:       []string{"10.0.0.50"},
		RemoteASN:           65001,
		ProvisionVNI:        100,
		ProvisionGateway:    testProvisionGateway,
		MinEstablishedPeers: 1,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("numbered mode with valid neighbors should pass: %v", err)
	}
}

func TestValidateRejectsInvalidNeighborAddr(t *testing.T) {
	cfg := &Config{
		ASN:           65000,
		RouterID:      "10.0.0.1",
		PeerMode:      network.PeerModeNumbered,
		NeighborAddrs: []string{"not-an-ip"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("invalid neighbor address should fail validation")
	}
}

func TestValidateRejectsUnknownPeerMode(t *testing.T) {
	cfg := &Config{
		ASN:      65000,
		RouterID: "10.0.0.1",
		PeerMode: "invalid",
	}
	if err := cfg.Validate(); err == nil {
		t.Error("unknown peer mode should fail validation")
	}
}

func TestValidateRejectsUnsupportedOverlayType(t *testing.T) {
	cfg := &Config{
		ASN:              65000,
		RouterID:         "10.0.0.1",
		PeerMode:         network.PeerModeUnnumbered,
		ProvisionVNI:     100,
		ProvisionGateway: testProvisionGateway,
		OverlayType:      string(OverlayL3VPN),
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("l3vpn overlay type should fail validation")
	}
	if !strings.Contains(err.Error(), "overlay type") {
		t.Errorf("expected error to mention \"overlay type\", got: %v", err)
	}
}

func TestValidateRejectsHoldTimeLessThan3(t *testing.T) {
	cfg := &Config{ASN: 65000, RouterID: "10.0.0.1", PeerMode: network.PeerModeUnnumbered, ProvisionVNI: 100, HoldTime: 2, KeepaliveInterval: 1}
	if err := cfg.Validate(); err == nil {
		t.Error("HoldTime < 3 should fail RFC 4271 validation")
	}
}

func TestValidateAcceptsHoldTimeZero(t *testing.T) {
	cfg := &Config{
		ASN:              65000,
		RouterID:         "10.0.0.1",
		PeerMode:         network.PeerModeUnnumbered,
		ProvisionVNI:     100,
		ProvisionGateway: testProvisionGateway,
		HoldTime:         0,
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Errorf("HoldTime 0 (defaulted) should pass: %v", err)
	}
}

func TestValidateRejectsKeepaliveExceedingHoldTime(t *testing.T) {
	cfg := &Config{ASN: 65000, RouterID: "10.0.0.1", PeerMode: network.PeerModeUnnumbered, ProvisionVNI: 100, HoldTime: 9, KeepaliveInterval: 4}
	if err := cfg.Validate(); err == nil {
		t.Error("KeepaliveInterval > HoldTime/3 should fail validation")
	}
}

func TestParsePeerMode(t *testing.T) {
	tests := []struct {
		input string
		want  network.PeerMode
	}{
		{"", network.PeerModeUnnumbered},
		{"unnumbered", network.PeerModeUnnumbered},
		{"UNNUMBERED", network.PeerModeUnnumbered},
		{"dual", network.PeerModeDual},
		{"Dual", network.PeerModeDual},
		{"numbered", network.PeerModeNumbered},
		{"NUMBERED", network.PeerModeNumbered},
		{"unknown", network.PeerModeUnnumbered},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := network.ParsePeerMode(tt.input)
			if got != tt.want {
				t.Errorf("ParsePeerMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseNeighborAddrs(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"10.0.0.1", 1},
		{"10.0.0.1,10.0.0.2", 2},
		{"10.0.0.1, 10.0.0.2, 10.0.0.3", 3},
		{",,,", 0},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseNeighborAddrs(tt.input)
			if len(got) != tt.want {
				t.Errorf("parseNeighborAddrs(%q) = %d addrs, want %d", tt.input, len(got), tt.want)
			}
		})
	}
}

func TestApplyDefaultsSetsMinEstablishedPeersToOne(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()

	if cfg.MinEstablishedPeers != 1 {
		t.Errorf("MinEstablishedPeers = %d, want 1 (default)", cfg.MinEstablishedPeers)
	}
}

func TestApplyDefaultsPreservesNonZeroMinEstablishedPeers(t *testing.T) {
	cfg := &Config{MinEstablishedPeers: 2}
	cfg.ApplyDefaults()

	if cfg.MinEstablishedPeers != 2 {
		t.Errorf("MinEstablishedPeers = %d, want 2 (preserved)", cfg.MinEstablishedPeers)
	}
}

func TestValidateRejectsZeroMinEstablishedPeers(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "10.0.0.1",
		PeerMode:            network.PeerModeUnnumbered,
		ProvisionVNI:        100,
		MinEstablishedPeers: 0,
	}
	if err := cfg.Validate(); err == nil {
		t.Error("MinEstablishedPeers=0 should fail validation")
	}
}

func TestValidateAcceptsMinEstablishedPeersGreaterThanOne(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "10.0.0.1",
		PeerMode:            network.PeerModeUnnumbered,
		ProvisionVNI:        100,
		ProvisionGateway:    testProvisionGateway,
		MinEstablishedPeers: 2,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("MinEstablishedPeers=2 should pass validation: %v", err)
	}
}

func TestValidatePeerModeRejectsIPv6Neighbors(t *testing.T) {
	for _, mode := range []network.PeerMode{network.PeerModeDual, network.PeerModeNumbered} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := &Config{
				ASN:                 65000,
				RouterID:            "10.0.0.1",
				PeerMode:            mode,
				NeighborAddrs:       []string{"fd00::1"},
				ProvisionVNI:        4000,
				MinEstablishedPeers: 1,
			}

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() accepted IPv6 BGP_NEIGHBORS")
			}
			if !strings.Contains(err.Error(), "must be IPv4") {
				t.Fatalf("Validate() error = %v, want IPv4 diagnostic", err)
			}
		})
	}
}

func TestNewConfigMapsMinEstablishedPeers(t *testing.T) {
	netCfg := &network.Config{
		UnderlayIP:       "10.0.0.1",
		ASN:              65000,
		ProvisionVNI:     4000,
		ProvisionGateway: testProvisionGateway,
		BGPPeerMode:      network.PeerModeUnnumbered,
		BGPMinPeers:      2,
	}
	cfg, err := NewConfig(netCfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinEstablishedPeers != 2 {
		t.Errorf("MinEstablishedPeers = %d, want 2", cfg.MinEstablishedPeers)
	}
}

func TestNewConfigMapsBGPInterfaces(t *testing.T) {
	netCfg := &network.Config{
		UnderlayIP:       "10.0.0.1",
		ASN:              65000,
		ProvisionVNI:     4000,
		ProvisionGateway: testProvisionGateway,
		BGPPeerMode:      network.PeerModeUnnumbered,
		BGPInterfaces:    "eth1, eth2",
	}
	cfg, err := NewConfig(netCfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(cfg.Interfaces, ","); got != "eth1,eth2" {
		t.Errorf("Interfaces = %q, want eth1,eth2", got)
	}
}

func TestNewConfigMapsNetplanVXLANBinding(t *testing.T) {
	netCfg := &network.Config{
		UnderlayIP:        "192.168.4.10",
		ASN:               65000,
		ProvisionVNI:      1000,
		ProvisionGateway:  testProvisionGateway,
		BGPPeerMode:       network.PeerModeUnnumbered,
		UnderlayDummyName: "dum.underlay",
		VXLANLink:         "dum.underlay",
	}

	cfg, err := NewConfig(netCfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.UnderlayDummyName != "dum.underlay" {
		t.Errorf("UnderlayDummyName = %q, want dum.underlay", cfg.UnderlayDummyName)
	}
	if cfg.VXLANLink != "dum.underlay" {
		t.Errorf("VXLANLink = %q, want dum.underlay", cfg.VXLANLink)
	}
}

func TestNewConfigDefaultsMinEstablishedPeersToOne(t *testing.T) {
	netCfg := &network.Config{
		UnderlayIP:       "10.0.0.1",
		ASN:              65000,
		ProvisionVNI:     4000,
		ProvisionGateway: testProvisionGateway,
		BGPPeerMode:      network.PeerModeUnnumbered,
	}
	cfg, err := NewConfig(netCfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MinEstablishedPeers != 1 {
		t.Errorf("MinEstablishedPeers = %d, want 1 (default when BGPMinPeers=0)", cfg.MinEstablishedPeers)
	}
}

func TestIsiBGP(t *testing.T) {
	tests := []struct {
		name      string
		asn       uint32
		remoteASN uint32
		want      bool
	}{
		{"zero remote = iBGP", 65000, 0, true},
		{"same ASN = iBGP", 65000, 65000, true},
		{"different ASN = eBGP", 65000, 65001, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{ASN: tt.asn, RemoteASN: tt.remoteASN}
			if got := cfg.IsiBGP(); got != tt.want {
				t.Errorf("IsiBGP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyDefaultsNormalizesUnderlayAFAndOverlayType(t *testing.T) {
	tests := []struct {
		name        string
		underlayAF  string
		overlayType string
		wantAF      string
		wantOT      string
	}{
		{"empty defaults", "", "", "ipv4", "evpn-vxlan"},
		{"uppercase AF", "IPV4", "EVPN-VXLAN", "ipv4", "evpn-vxlan"},
		{"mixed case", "DualStack", "L3VPN", "dual-stack", "l3vpn"},
		{"already canonical", "ipv6", "none", "ipv6", "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{UnderlayAF: tt.underlayAF, OverlayType: tt.overlayType}
			cfg.ApplyDefaults()
			if cfg.UnderlayAF != tt.wantAF {
				t.Errorf("UnderlayAF = %q, want %q", cfg.UnderlayAF, tt.wantAF)
			}
			if cfg.OverlayType != tt.wantOT {
				t.Errorf("OverlayType = %q, want %q", cfg.OverlayType, tt.wantOT)
			}
		})
	}
}

func TestUnderlayTierReady(t *testing.T) {
	makeUnderlay := func(minPeers int, countFn func(context.Context) int) *UnderlayTier {
		cfg := &Config{MinEstablishedPeers: minPeers}
		u := NewUnderlayTier(cfg)
		u.peerCountFn = countFn
		u.pollInterval = time.Millisecond
		return u
	}

	t.Run("succeeds_when_threshold_met_immediately", func(t *testing.T) {
		u := makeUnderlay(1, func(_ context.Context) int { return 1 })
		ctx := context.Background()
		if err := u.Ready(ctx, 5*time.Second); err != nil {
			t.Fatalf("Ready() = %v, want nil", err)
		}
	})

	t.Run("succeeds_when_threshold_met_on_second_tick", func(t *testing.T) {
		calls := 0
		u := makeUnderlay(2, func(_ context.Context) int {
			calls++
			if calls >= 2 {
				return 2
			}
			return 0
		})
		ctx := context.Background()
		if err := u.Ready(ctx, 5*time.Second); err != nil {
			t.Fatalf("Ready() = %v, want nil", err)
		}
	})

	t.Run("returns_error_on_timeout", func(t *testing.T) {
		u := makeUnderlay(3, func(_ context.Context) int { return 0 })
		ctx := context.Background()
		err := u.Ready(ctx, 50*time.Millisecond)
		if err == nil {
			t.Fatal("Ready() = nil, want timeout error")
		}
	})

	t.Run("returns_error_on_context_cancel", func(t *testing.T) {
		u := makeUnderlay(1, func(_ context.Context) int { return 0 })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := u.Ready(ctx, 5*time.Second)
		if err == nil {
			t.Fatal("Ready() = nil, want context error")
		}
	})

	t.Run("min_peers_greater_than_one", func(t *testing.T) {
		u := makeUnderlay(3, func(_ context.Context) int { return 3 })
		ctx := context.Background()
		if err := u.Ready(ctx, 5*time.Second); err != nil {
			t.Fatalf("Ready() = %v, want nil", err)
		}
	})

	t.Run("count_below_threshold_does_not_succeed", func(t *testing.T) {
		u := makeUnderlay(3, func(_ context.Context) int { return 2 })
		ctx := context.Background()
		err := u.Ready(ctx, 50*time.Millisecond)
		if err == nil {
			t.Fatal("Ready() = nil, want timeout error when count < min")
		}
	})
}

// When every probe finishes before the result is read, done and result are
// ready together. The reachable peer must still win (CI flake on #647).
func TestFirstReachableBGPPeerAllProbesFinished(t *testing.T) {
	probe := bgpProbe
	t.Cleanup(func() { bgpProbe = probe })
	bgpProbe = func(_ context.Context, addr, _ string) bool { return addr == "::1" }
	for range 200 {
		if got := firstReachableBGPPeerOnPort(context.Background(), []string{"2001:db8::1", "::1"}, "179"); got != "::1" {
			t.Fatalf("firstReachableBGPPeer() = %q, want ::1", got)
		}
	}
}

func TestFirstReachableBGPPeer(t *testing.T) {
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(context.Background(), "tcp6", "[::1]:0")
	if err != nil {
		if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
			t.Fatalf("IPv6 loopback listener unavailable: %v", err)
		}
		t.Skipf("IPv6 loopback listener unavailable: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
		close(accepted)
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := firstReachableBGPPeerOnPort(ctx, []string{"2001:db8::1", "::1"}, port)
	if got != "::1" {
		t.Fatalf("firstReachableBGPPeer() = %q, want ::1", got)
	}

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("listener did not observe BGP probe")
	}
}

// The relax and fast-external-failover settings reach the GoBGP config;
// fast-external-failover is on unless set off.
func TestNewConfigMapsRelaxAndFailover(t *testing.T) {
	base := func() *network.Config {
		return &network.Config{ASN: 65000, UnderlayIP: "192.0.2.1", ProvisionVNI: 4000, ProvisionGateway: testProvisionGateway, BGPMultipathRelax: true,
			BGPSessions: []network.BGPSession{{Name: "192.0.2.9", RemoteASN: 65001, KeepaliveTime: 3, HoldTime: 9, IPv4Unicast: true}}}
	}
	cfg, err := NewConfig(base())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MultipathRelax || !cfg.FastExternalFailover {
		t.Fatalf("MultipathRelax %v FastExternalFailover %v, want both on", cfg.MultipathRelax, cfg.FastExternalFailover)
	}
	off := base()
	off.BGPFastExternalFailover = new(false)
	if cfg, err = NewConfig(off); err != nil || cfg.FastExternalFailover {
		t.Fatalf("FastExternalFailover = %v (%v), want off", cfg.FastExternalFailover, err)
	}
}

// The native peers carry the options that frr.conf sessions carry per peer.
func TestNativePeerOptions(t *testing.T) {
	cfg := &Config{ASN: 4210000017, LocalAS: 65501, AllowASIn: 3, NeighborVRF: "Vrf_underlay"}
	ifPeer := buildInterfacePeer(cfg, "eth1", "fe80::1%eth1", ipv4Families())
	if ifPeer.Conf.LocalAsn != 65501 || ifPeer.Conf.AllowOwnAsn != 3 || ifPeer.Transport.BindInterface != "eth1" {
		t.Fatalf("interface peer conf %+v transport %+v", ifPeer.Conf, ifPeer.Transport)
	}
	numPeer := buildNumberedPeer(cfg, "198.51.100.1", allFamilies())
	if numPeer.Conf.PeerAsn != 4210000017 || numPeer.Conf.LocalAsn != 0 || numPeer.Conf.AllowOwnAsn != 3 ||
		numPeer.Transport.BindInterface != "Vrf_underlay" {
		t.Fatalf("numbered peer conf %+v transport %+v", numPeer.Conf, numPeer.Transport)
	}
	u := &UnderlayTier{cfg: cfg}
	if !u.hasVRFSession() {
		t.Fatal("BGP_NEIGHBOR_VRF must count as a VRF session (FIB table, tcp_l3mdev_accept)")
	}
	u.cfg = &Config{ASN: 65000}
	if u.hasVRFSession() {
		t.Fatal("no VRF without BGP_NEIGHBOR_VRF")
	}
	// Like FRR, unnumbered peers on the VRF_NAME NICs are in that VRF, so
	// the leaf may connect into it (tcp_l3mdev_accept).
	for _, mode := range []network.PeerMode{"", network.PeerModeUnnumbered, network.PeerModeDual} {
		u.cfg = &Config{ASN: 65000, VRFName: "Vrf_underlay", PeerMode: mode}
		if !u.hasVRFSession() {
			t.Fatalf("peer mode %q: unnumbered peers in VRF_NAME must count as a VRF session", mode)
		}
	}
	u.cfg = &Config{ASN: 65000, VRFName: "Vrf_underlay", PeerMode: network.PeerModeNumbered}
	if u.hasVRFSession() {
		t.Fatal("numbered peer mode: VRF_NAME alone has no session in a VRF")
	}
}

// With LLDP_EMIT_ALL the caller sends on every NIC, so the underlay sends
// on none and no NIC gets two senders.
func TestEmitLLDPWithEmitAll(t *testing.T) {
	netCfg := &network.Config{EmitLLDP: []string{"ens1f0np0"}}
	if got := emitLLDP(netCfg); len(got) != 1 || got[0] != "ens1f0np0" {
		t.Fatalf("emitLLDP = %v, want the emit-lldp NICs", got)
	}
	netCfg.LLDPEmitAll = true
	if got := emitLLDP(netCfg); got != nil {
		t.Fatalf("emitLLDP with emit-all = %v, want none", got)
	}
}
