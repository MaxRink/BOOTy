package gobgp

import (
	"testing"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/telekom/BOOTy/pkg/network"
)

// TestScenarioUnnumberedEVPN validates the configuration for Scenario 1:
// BGP unnumbered (link-local) with IPv4 + L2VPN-EVPN.
//
// Topology: Server ─── leaf switches (eBGP unnumbered)
// All address families (IPv4 unicast + L2VPN-EVPN) are exchanged over
// unnumbered interface sessions.
func TestScenarioUnnumberedEVPN(t *testing.T) {
	cfg := &Config{
		ASN:                 65000,
		RouterID:            "192.168.4.10",
		PeerMode:            network.PeerModeUnnumbered,
		ProvisionVNI:        4000,
		ProvisionIP:         "10.100.0.10/24",
		ProvisionGateway:    testProvisionGateway,
		MTU:                 9000,
		KeepaliveInterval:   3,
		HoldTime:            9,
		BridgeName:          "br.provision",
		MinEstablishedPeers: 1,
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("scenario 1 config validation failed: %v", err)
	}
	if cfg.PeerMode != network.PeerModeUnnumbered {
		t.Errorf("network.PeerMode = %q, want %q", cfg.PeerMode, network.PeerModeUnnumbered)
	}

	// Unnumbered mode must NOT require NeighborAddrs.
	if len(cfg.NeighborAddrs) != 0 {
		t.Error("unnumbered mode should not have NeighborAddrs")
	}

	// Verify allFamilies() returns both IPv4 and EVPN.
	fams := allFamilies()
	if len(fams) != 2 {
		t.Fatalf("allFamilies() returned %d families, want 2", len(fams))
	}
}

// TestScenarioDualUnnumberedAndNumbered validates the configuration for Scenario 2:
// BGP unnumbered (link-local) for IPv4 underlay + numbered iBGP to route
// reflectors (or eBGP to external peers) for L2VPN-EVPN.
//
// Topology:
//
//	Server ─── leaf switches  (unnumbered eBGP, IPv4 unicast only)
//	Server ─── route reflectors  (numbered iBGP, L2VPN-EVPN + IPv4)
func TestScenarioDualUnnumberedAndNumbered(t *testing.T) {
	t.Run("iBGP_to_RR", func(t *testing.T) {
		cfg := &Config{
			ASN:                 65000,
			RouterID:            "192.168.4.10",
			PeerMode:            network.PeerModeDual,
			NeighborAddrs:       []string{"192.168.4.1", "192.168.4.2"},
			RemoteASN:           0, // 0 = same as local = iBGP
			ProvisionVNI:        4000,
			ProvisionIP:         "10.100.0.10/24",
			ProvisionGateway:    testProvisionGateway,
			MTU:                 9000,
			KeepaliveInterval:   3,
			HoldTime:            9,
			BridgeName:          "br.provision",
			MinEstablishedPeers: 1,
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("dual/iBGP config validation failed: %v", err)
		}
		if !cfg.IsiBGP() {
			t.Error("RemoteASN=0 should be treated as iBGP")
		}

		// Unnumbered peers get IPv4 only.
		ipv4 := ipv4Families()
		if len(ipv4) != 1 {
			t.Fatalf("ipv4Families() returned %d families, want 1", len(ipv4))
		}

		// Numbered peers get all families.
		all := allFamilies()
		if len(all) != 2 {
			t.Fatalf("allFamilies() returned %d families, want 2", len(all))
		}
	})

	t.Run("eBGP_to_DCGW", func(t *testing.T) {
		cfg := &Config{
			ASN:                 65000,
			RouterID:            "192.168.4.10",
			PeerMode:            network.PeerModeDual,
			NeighborAddrs:       []string{"10.255.0.1", "10.255.0.2"},
			RemoteASN:           65100, // Different ASN = eBGP
			ProvisionVNI:        4000,
			ProvisionIP:         "10.100.0.10/24",
			ProvisionGateway:    testProvisionGateway,
			MTU:                 9000,
			KeepaliveInterval:   3,
			HoldTime:            9,
			BridgeName:          "br.provision",
			MinEstablishedPeers: 1,
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("dual/eBGP config validation failed: %v", err)
		}
		if cfg.IsiBGP() {
			t.Error("RemoteASN=65100 should be eBGP, not iBGP")
		}
	})
}

// TestScenarioNumberedOnly validates the configuration for Scenario 3:
// DHCP or static underlay with BGP numbered (iBGP or eBGP) for L2VPN-EVPN.
//
// Topology:
//
//	Server ─── (DHCP/static) ─── underlay  (no BGP unnumbered)
//	Server ─── BGP peers  (numbered sessions, all families)
func TestScenarioNumberedOnly(t *testing.T) {
	t.Run("iBGP_numbered", func(t *testing.T) {
		cfg := &Config{
			ASN:                 65000,
			RouterID:            "10.0.0.5",
			PeerMode:            network.PeerModeNumbered,
			NeighborAddrs:       []string{"10.0.0.1", "10.0.0.2"},
			RemoteASN:           65000, // Same ASN = iBGP
			ProvisionVNI:        4000,
			ProvisionIP:         "10.100.0.10/24",
			ProvisionGateway:    testProvisionGateway,
			MTU:                 9000,
			KeepaliveInterval:   3,
			HoldTime:            9,
			BridgeName:          "br.provision",
			MinEstablishedPeers: 1,
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("numbered/iBGP config validation failed: %v", err)
		}
		if !cfg.IsiBGP() {
			t.Error("RemoteASN=65000 with ASN=65000 should be iBGP")
		}
		if len(cfg.NeighborAddrs) != 2 {
			t.Errorf("NeighborAddrs = %d, want 2", len(cfg.NeighborAddrs))
		}
	})

	t.Run("eBGP_numbered", func(t *testing.T) {
		cfg := &Config{
			ASN:                 65010,
			RouterID:            "10.0.0.5",
			PeerMode:            network.PeerModeNumbered,
			NeighborAddrs:       []string{"10.0.0.1"},
			RemoteASN:           65020, // Different ASN = eBGP
			ProvisionVNI:        4000,
			ProvisionIP:         "10.100.0.10/24",
			ProvisionGateway:    testProvisionGateway,
			MTU:                 9000,
			KeepaliveInterval:   3,
			HoldTime:            9,
			BridgeName:          "br.provision",
			MinEstablishedPeers: 1,
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("numbered/eBGP config validation failed: %v", err)
		}
		if cfg.IsiBGP() {
			t.Error("RemoteASN=65020 with ASN=65010 should be eBGP")
		}
	})

	t.Run("numbered_without_neighbors_fails", func(t *testing.T) {
		cfg := &Config{
			ASN:      65000,
			RouterID: "10.0.0.5",
			PeerMode: network.PeerModeNumbered,
		}

		if err := cfg.Validate(); err == nil {
			t.Error("numbered mode without neighbors should fail validation")
		}
	})

	t.Run("numbered_with_ipv6_peer", func(t *testing.T) {
		cfg := &Config{
			ASN:                 65000,
			RouterID:            "10.0.0.5",
			PeerMode:            network.PeerModeNumbered,
			NeighborAddrs:       []string{"fd00::1"},
			RemoteASN:           65000,
			ProvisionVNI:        100,
			ProvisionGateway:    testProvisionGateway,
			MinEstablishedPeers: 1,
		}

		if err := cfg.Validate(); err == nil {
			t.Fatal("numbered mode with IPv6 neighbor should fail validation")
		}
	})
}

// TestScenarioFamilyAssignment verifies that each peer mode assigns the
// correct address families to the correct peer types.
func TestScenarioFamilyAssignment(t *testing.T) {
	t.Run("unnumbered_gets_all_families", func(t *testing.T) {
		fams := allFamilies()
		if len(fams) != 2 {
			t.Fatalf("want 2 families, got %d", len(fams))
		}
		// First should be IPv4 unicast.
		if f := fams[0].Config.Family; f.Afi != 1 || f.Safi != 1 { // AFI_IP=1, SAFI_UNICAST=1
			t.Errorf("family[0] = AFI %d / SAFI %d, want IPv4 unicast", f.Afi, f.Safi)
		}
		// Second should be L2VPN-EVPN.
		if f := fams[1].Config.Family; f.Afi != 25 || f.Safi != 70 { // AFI_L2VPN=25, SAFI_EVPN=70
			t.Errorf("family[1] = AFI %d / SAFI %d, want L2VPN-EVPN", f.Afi, f.Safi)
		}
	})

	t.Run("dual_unnumbered_gets_ipv4_only", func(t *testing.T) {
		fams := ipv4Families()
		if len(fams) != 1 {
			t.Fatalf("want 1 family for unnumbered in dual mode, got %d", len(fams))
		}
		if f := fams[0].Config.Family; f.Afi != 1 || f.Safi != 1 {
			t.Errorf("family = AFI %d / SAFI %d, want IPv4 unicast", f.Afi, f.Safi)
		}
	})

	t.Run("dual_numbered_gets_all_families", func(t *testing.T) {
		fams := allFamilies()
		if len(fams) != 2 {
			t.Fatalf("want 2 families for numbered in dual mode, got %d", len(fams))
		}
	})
}

// TestScenarioBGPTimers verifies BGP timer configuration for each scenario.
func TestScenarioBGPTimers(t *testing.T) {
	cfg := &Config{
		KeepaliveInterval: 3,
		HoldTime:          9,
	}
	timers := bgpTimers(cfg)
	if timers.Config.KeepaliveInterval != 3 {
		t.Errorf("keepalive = %d, want 3", timers.Config.KeepaliveInterval)
	}
	if timers.Config.HoldTime != 9 {
		t.Errorf("hold = %d, want 9", timers.Config.HoldTime)
	}
}

func TestBGPAuthNumberedPeer(t *testing.T) {
	families := allFamilies()

	t.Run("auth_set_when_configured", func(t *testing.T) {
		cfg := &Config{
			ASN:          65000,
			RemoteASN:    65001,
			AuthPassword: "hunter2",
		}
		peer := buildNumberedPeer(cfg, "10.0.0.1", families)
		if peer.Conf.AuthPassword != "hunter2" {
			t.Errorf("AuthPassword = %q, want hunter2", peer.Conf.AuthPassword)
		}
		if peer.Conf.NeighborAddress != "10.0.0.1" {
			t.Errorf("NeighborAddress = %q, want 10.0.0.1", peer.Conf.NeighborAddress)
		}
		if peer.Conf.PeerAsn != 65001 {
			t.Errorf("PeerAsn = %d, want 65001", peer.Conf.PeerAsn)
		}
	})

	t.Run("no_auth_when_password_empty", func(t *testing.T) {
		cfg := &Config{
			ASN:          65000,
			RemoteASN:    65001,
			AuthPassword: "",
		}
		peer := buildNumberedPeer(cfg, "10.0.0.1", families)
		if peer.Conf.AuthPassword != "" {
			t.Errorf("AuthPassword = %q, want empty", peer.Conf.AuthPassword)
		}
	})

	t.Run("ibgp_uses_local_asn_when_remote_zero", func(t *testing.T) {
		cfg := &Config{
			ASN:          65000,
			RemoteASN:    0,
			AuthPassword: "secret",
		}
		peer := buildNumberedPeer(cfg, "10.0.0.2", families)
		if peer.Conf.PeerAsn != 65000 {
			t.Errorf("PeerAsn = %d, want 65000 (iBGP)", peer.Conf.PeerAsn)
		}
		if peer.Conf.AuthPassword != "secret" {
			t.Errorf("AuthPassword = %q, want secret", peer.Conf.AuthPassword)
		}
	})
}

func TestBGPAuthInterfacePeer(t *testing.T) {
	families := allFamilies()
	const iface = "eth0"
	const addr = "fe80::1%eth0"

	t.Run("auth_set_when_configured", func(t *testing.T) {
		cfg := &Config{
			ASN:          65000,
			AuthPassword: "hunter2",
		}
		peer := buildInterfacePeer(cfg, iface, addr, families)
		if peer.Conf.AuthPassword != "hunter2" {
			t.Errorf("AuthPassword = %q, want hunter2", peer.Conf.AuthPassword)
		}
		if peer.Transport.BindInterface != iface {
			t.Errorf("BindInterface = %q, want %q", peer.Transport.BindInterface, iface)
		}
		if peer.Transport.RemoteAddress != addr {
			t.Errorf("RemoteAddress = %q, want %q", peer.Transport.RemoteAddress, addr)
		}
		if peer.Conf.PeerAsn != 0 {
			t.Errorf("PeerAsn = %d, want 0 (learned via open)", peer.Conf.PeerAsn)
		}
	})

	t.Run("no_auth_when_password_empty", func(t *testing.T) {
		cfg := &Config{
			ASN:          65000,
			AuthPassword: "",
		}
		peer := buildInterfacePeer(cfg, iface, addr, families)
		if peer.Conf.AuthPassword != "" {
			t.Errorf("AuthPassword = %q, want empty", peer.Conf.AuthPassword)
		}
	})
}

// GoBGP v4.6.0 added native single-hop BFD, which a peer turns on with
// Peer.Bfd. BOOTy runs its own BFD (pkg/network/bfd: multihop, the GTSM
// receive check, VRF-aware lookup), so the peers must leave the GoBGP BFD
// off, also with BFD configured, or both would run on one session.
func TestPeersLeaveGoBGPBFDOff(t *testing.T) {
	cfg := &Config{ASN: 65000, BFDTransmitMS: 300, BFDReceiveMS: 300, BFDDetectMult: 3}
	s := &network.BGPSession{Name: "eth1", Interface: true, RemoteASN: 65100, IPv4Unicast: true,
		BFD: true, BFDTransmitMS: 300, BFDReceiveMS: 300, BFDDetectMult: 3}
	for _, p := range []*apipb.Peer{
		buildNumberedPeer(cfg, "192.0.2.1", ipv4Families()),
		buildInterfacePeer(cfg, "eth1", "fe80::1", ipv4Families()),
		buildSessionPeer(cfg, s, "fe80::1"),
	} {
		if p.GetBfd() != nil {
			t.Errorf("peer %s sets the GoBGP BFD config %v", p.GetConf().GetNeighborAddress(), p.GetBfd())
		}
	}
}
