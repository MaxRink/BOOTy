package netplan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/network"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- ParseFile tests ---

func TestParseFile_DHCPOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-provision.yaml", `
network:
  version: 2
  ethernets:
    all-en:
      match:
        name: "en*"
      dhcp4: true
    all-eth:
      match:
        name: "eth*"
      dhcp4: true
`)
	cfg, err := ParseFile(filepath.Join(dir, "10-provision.yaml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if cfg.Network.Version != 2 {
		t.Errorf("version = %d, want 2", cfg.Network.Version)
	}
	if len(cfg.Network.Ethernets) != 2 {
		t.Fatalf("ethernets = %d, want 2", len(cfg.Network.Ethernets))
	}
	eth := cfg.Network.Ethernets["all-en"]
	if eth.Match == nil || eth.Match.Name != "en*" {
		t.Errorf("match = %v, want en*", eth.Match)
	}
	if eth.DHCP4 == nil || !*eth.DHCP4 {
		t.Error("dhcp4 should be true")
	}
}

// TestParseFile_BM4XNodeNetwork tests parsing a rendered BM4X 10-node-network.yaml.
func TestParseFile_BM4XNodeNetwork(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-node-network.yaml", `
network:
  version: 2
  bridges:
    cni0:
      mtu: 9000
      interfaces:
        - sriovbond
      parameters:
        stp: true
      accept-ra: false
      ignore-carrier: false
  bonds:
    sriovbond:
      interfaces:
        - ens1f0np0
        - ens1f1np0
      mtu: 9000
      accept-ra: false
      parameters:
        lacp-rate: fast
        mode: 802.3ad
        transmit-hash-policy: layer3+4
        mii-monitor-interval: 100
  ethernets:
    hbn:
      nameservers:
        addresses:
          - 2003:0:af08:1005::1000
      addresses:
        - 10.1.2.3/32
        - fd00::1/128
        - fd00:7:caa5::1/127
      routes:
        - to: default
          via: 169.254.1.0
          from: 10.1.2.3
        - to: default
          via: fd00:7:caa5::0
          from: fd00::1
      mtu: 1500
    ens1f0np0:
      mtu: 9000
      link-local: []
      virtual-function-count: 16
      embedded-switch-mode: switchdev
      delay-virtual-functions-rebind: true
`)
	cfg, err := ParseFile(filepath.Join(dir, "10-node-network.yaml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	bond, ok := cfg.Network.Bonds["sriovbond"]
	if !ok {
		t.Fatal("missing bond sriovbond")
	}
	if len(bond.Interfaces) != 2 {
		t.Errorf("bond interfaces = %v, want 2", bond.Interfaces)
	}
	if bond.Parameters == nil || bond.Parameters.Mode != "802.3ad" {
		t.Error("bond mode should be 802.3ad")
	}
	if bond.Parameters.LACPRate != "fast" {
		t.Errorf("lacp-rate = %q, want fast", bond.Parameters.LACPRate)
	}
	if bond.Parameters.TransmitHashPolicy != "layer3+4" {
		t.Errorf("transmit-hash-policy = %q", bond.Parameters.TransmitHashPolicy)
	}
	br, ok := cfg.Network.Bridges["cni0"]
	if !ok {
		t.Fatal("missing bridge cni0")
	}
	if br.Parameters == nil || br.Parameters.STP == nil || !*br.Parameters.STP {
		t.Error("bridge stp should be true")
	}
	sriov := cfg.Network.Ethernets["ens1f0np0"]
	if sriov.VirtualFunctionCount != 16 {
		t.Errorf("virtual-function-count = %d, want 16", sriov.VirtualFunctionCount)
	}
	if sriov.EmbeddedSwitchMode != "switchdev" {
		t.Errorf("embedded-switch-mode = %q, want switchdev", sriov.EmbeddedSwitchMode)
	}
	hbn := cfg.Network.Ethernets["hbn"]
	if len(hbn.Routes) != 2 {
		t.Errorf("hbn routes = %d, want 2", len(hbn.Routes))
	}
	if hbn.Routes[0].From != "10.1.2.3" {
		t.Errorf("route from = %q, want 10.1.2.3", hbn.Routes[0].From)
	}
}

// TestParseFile_BM4XEVPN tests parsing a rendered BM4X /etc/cra/10-base.yaml.
func TestParseFile_BM4XEVPN(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-base.yaml", `
network:
  version: 2
  ethernets:
    eth0:
      accept-ra: false
      emit-lldp: true
      link-local: [ipv6]
      mtu: 9100
      ipv6-address-generation: eui64
  dummy-devices:
    dum.underlay:
      addresses:
        - 192.168.4.11/32
      mtu: 9100
    lo_calico:
      addresses:
        - 169.254.100.100/32
        - fd00:7:caa5:1::/128
      mtu: 9000
    lo_mgmt:
      addresses:
        - 169.254.1.0/32
      mtu: 9000
  vrfs:
    cluster:
      table: 10
      interfaces:
        - hbn
        - br_cluster
        - lo_calico
        - lo_mgmt
    p_zerotrust:
      table: 11
      interfaces:
        - br_p_zerotrust
  tunnels:
    vx_cluster:
      mode: vxlan
      mtu: 9000
      link-local: []
      link: dum.underlay
      id: 100
      local: '192.168.4.11'
      hairpin: true
      port: 4789
      accept-ra: false
      ignore-carrier: false
      mac-learning: false
      arp-proxy: false
      port-mac-learning: false
    vx_p_zerotrust:
      mode: vxlan
      mtu: 9000
      link-local: []
      link: dum.underlay
      id: 200
      local: '192.168.4.11'
      hairpin: true
      port: 4789
      mac-learning: false
  bridges:
    br_cluster:
      mtu: 9000
      link-local: []
      parameters:
        stp: false
      macaddress: '00:11:22:33:44:55'
      interfaces:
        - vx_cluster
      accept-ra: false
      ignore-carrier: false
    br_p_zerotrust:
      mtu: 9000
      parameters:
        stp: false
      macaddress: '00:11:22:33:44:55'
      interfaces:
        - vx_p_zerotrust
`)
	cfg, err := ParseFile(filepath.Join(dir, "10-base.yaml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	vx := cfg.Network.Tunnels["vx_cluster"]
	if vx.Mode != "vxlan" || vx.ID != 100 || vx.Local != "192.168.4.11" {
		t.Errorf("tunnel = mode=%q id=%d local=%q", vx.Mode, vx.ID, vx.Local)
	}
	if vx.Hairpin == nil || !*vx.Hairpin {
		t.Error("tunnel hairpin should be true")
	}
	if vx.MACLearning == nil || *vx.MACLearning {
		t.Error("tunnel mac-learning should be false")
	}
	if vx.ARPProxy == nil || *vx.ARPProxy {
		t.Error("tunnel arp-proxy should be false")
	}
	br := cfg.Network.Bridges["br_cluster"]
	if br.MACAddress != "00:11:22:33:44:55" {
		t.Errorf("bridge macaddress = %q", br.MACAddress)
	}
	dum := cfg.Network.DummyDevices["dum.underlay"]
	if dum.MTU != 9100 {
		t.Errorf("dummy mtu = %d, want 9100", dum.MTU)
	}
	cluster := cfg.Network.VRFs["cluster"]
	if cluster.Table != 10 || len(cluster.Interfaces) != 4 {
		t.Errorf("cluster VRF = table=%d ifaces=%d", cluster.Table, len(cluster.Interfaces))
	}
	eth0 := cfg.Network.Ethernets["eth0"]
	if eth0.IPv6AddressGeneration != "eui64" {
		t.Errorf("ipv6-address-generation = %q, want eui64", eth0.IPv6AddressGeneration)
	}
	if eth0.EmitLLDP == nil || !*eth0.EmitLLDP {
		t.Error("emit-lldp should be true")
	}
}

// TestParseFile_Netbox tests parsing rendered Netbox network profiles.
func TestParseFile_Netbox(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-a-eth.yaml", `
network:
  version: 2
  ethernets:
    ensbond0np0:
      match:
        name: "ens1f0*"
      dhcp4: false
    ensbond0np1:
      match:
        name: "ens1f1*"
      dhcp4: false
`)
	writeFile(t, dir, "10-b-bond0.yaml", `
network:
  version: 2
  bonds:
    bond0:
      mtu: 9000
      interfaces:
        - ensbond0np0
        - ensbond0np1
      parameters:
        mode: "active-backup"
        mii-monitor-interval: 100
  vlans:
    bond0.10:
      addresses:
        - "10.1.1.5/24"
      nameservers:
        addresses:
          - 10.235.119.37
          - 10.235.119.38
      dhcp4: false
      mtu: 9000
      routes:
        - to: "100.64.0.0/10"
          via: "10.1.1.254"
      id: 10
      link: "bond0"
    bond0.200:
      addresses:
        - "10.2.2.5/24"
      nameservers:
        addresses:
          - 10.235.119.37
          - 10.235.119.38
      routes:
        - to: "default"
          via: "10.2.2.1"
      dhcp4: false
      mtu: 9000
      id: 200
      link: "bond0"
`)
	cfg, err := ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	if len(cfg.Network.Ethernets) != 2 {
		t.Errorf("ethernets = %d, want 2", len(cfg.Network.Ethernets))
	}
	bond := cfg.Network.Bonds["bond0"]
	if bond.Parameters.Mode != "active-backup" {
		t.Errorf("mode = %q", bond.Parameters.Mode)
	}
	if bond.Parameters.MIIMonitorInterval != 100 {
		t.Errorf("mii = %d, want 100", bond.Parameters.MIIMonitorInterval)
	}
	if len(cfg.Network.VLANs) != 2 {
		t.Errorf("vlans = %d, want 2", len(cfg.Network.VLANs))
	}
	oam := cfg.Network.VLANs["bond0.10"]
	if oam.ID != 10 || oam.Link != "bond0" {
		t.Errorf("oam = id=%d link=%q", oam.ID, oam.Link)
	}
	if oam.Nameservers == nil || len(oam.Nameservers.Addresses) != 2 {
		t.Error("oam should have 2 nameservers")
	}
	dth := cfg.Network.VLANs["bond0.200"]
	if dth.Routes[0].To != "default" || dth.Routes[0].Via != "10.2.2.1" {
		t.Errorf("dth route = %+v", dth.Routes[0])
	}
}

// --- ParseDir tests ---

func TestParseDir_Merge(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "10-underlay.yaml", `
network:
  version: 2
  ethernets:
    eth0:
      link-local: [ipv6]
      mtu: 9100
`)
	writeFile(t, dir, "20-overlay.yaml", `
network:
  version: 2
  tunnels:
    vx100:
      mode: vxlan
      id: 100
      local: "10.0.0.1"
      port: 4789
  bridges:
    br100:
      interfaces:
        - vx100
      addresses:
        - 10.100.0.20/24
`)
	cfg, err := ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	if len(cfg.Network.Ethernets) != 1 {
		t.Errorf("ethernets = %d, want 1", len(cfg.Network.Ethernets))
	}
	if len(cfg.Network.Tunnels) != 1 {
		t.Errorf("tunnels = %d, want 1", len(cfg.Network.Tunnels))
	}
}

func TestParseDir_EmptyDir(t *testing.T) {
	_, err := ParseDir(t.TempDir())
	if err == nil {
		t.Error("expected error for empty directory")
	}
}

// --- ToNetworkConfig tests ---

func TestToNetworkConfig_DHCP(t *testing.T) {
	dhcp := true
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"all-en": {Match: &MatchConfig{Name: "en*"}, DHCP4: &dhcp},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty", netCfg.NetworkMode)
	}
}

func TestToNetworkConfig_BM4XEVPN(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"eth0": {LinkLocal: []string{"ipv6"}, MTU: 9100},
				"hbn":  {MTU: 1500},
			},
			DummyDevices: map[string]DummyConfig{
				"dum.underlay": {Addresses: []string{"192.168.4.11/32"}},
			},
			Tunnels: map[string]TunnelConfig{
				"vx_cluster":     {Mode: "vxlan", ID: 100, Local: "192.168.4.11", Port: 4789},
				"vx_p_zerotrust": {Mode: "vxlan", ID: 200, Local: "192.168.4.11", Port: 4789},
			},
			Bridges: map[string]BridgeConfig{
				"br_cluster":     {Interfaces: []string{"vx_cluster"}, Addresses: []string{"10.100.0.11/32"}},
				"br_p_zerotrust": {Interfaces: []string{"vx_p_zerotrust"}, Addresses: []string{"10.200.0.11/32"}},
			},
			VRFs: map[string]VRFConfig{
				"cluster":     {Table: 10, Interfaces: []string{"hbn", "br_cluster"}},
				"p_zerotrust": {Table: 11, Interfaces: []string{"br_p_zerotrust"}},
			},
		},
	}
	frr := &FRRParams{
		ASN:             65501,
		LocalASN:        65501,
		RouterID:        "192.168.4.11",
		UnnumberedPeers: []string{"eth0"},
		EVPN:            true,
		AdvertiseAllVNI: true,
	}
	netCfg := ToNetworkConfig(np, frr)
	if netCfg.NetworkMode != "gobgp" {
		t.Errorf("NetworkMode = %q, want gobgp", netCfg.NetworkMode)
	}
	if netCfg.ASN != 65501 {
		t.Errorf("ASN = %d, want 65501", netCfg.ASN)
	}
	if netCfg.LocalASN != 65501 {
		t.Errorf("LocalASN = %d, want 65501", netCfg.LocalASN)
	}
	if netCfg.ProvisionVNI == 0 {
		t.Error("ProvisionVNI should be > 0")
	}
	if netCfg.UnderlayIP != "192.168.4.11" {
		t.Errorf("UnderlayIP = %q", netCfg.UnderlayIP)
	}
	if !netCfg.EVPNL2Enabled {
		t.Error("EVPNL2Enabled should be true")
	}
	if netCfg.BGPPeerMode != network.PeerModeUnnumbered {
		t.Errorf("BGPPeerMode = %q, want unnumbered", netCfg.BGPPeerMode)
	}
	if netCfg.VRFTableID == 0 {
		t.Error("VRFTableID should be > 0")
	}
	if netCfg.BridgeName != "br_cluster" {
		t.Errorf("BridgeName = %q, want br_cluster", netCfg.BridgeName)
	}
	if !netCfg.OverlayVRFSet {
		t.Error("OverlayVRFSet should be true")
	}
	if netCfg.OverlayVRFName != "cluster" {
		t.Errorf("OverlayVRFName = %q, want cluster", netCfg.OverlayVRFName)
	}
	if netCfg.OverlayVRFTableID != 10 {
		t.Errorf("OverlayVRFTableID = %d, want 10", netCfg.OverlayVRFTableID)
	}
	if netCfg.MTU != 9100 {
		t.Errorf("MTU = %d, want 9100", netCfg.MTU)
	}
}

func TestToNetworkConfig_PureType5DoesNotEnableL2(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"hbn": {LinkLocal: []string{"ipv6"}, MTU: 9100},
			},
			DummyDevices: map[string]DummyConfig{
				"dum.underlay": {Addresses: []string{"192.168.4.10/32"}},
			},
			Tunnels: map[string]TunnelConfig{
				"vx.1000": {
					Mode: "vxlan", ID: 1000, Local: "192.168.4.10",
					Port: 4789, Link: "dum.underlay",
				},
			},
			Bridges: map[string]BridgeConfig{
				"br.provision": {Interfaces: []string{"vx.1000"}, Addresses: []string{"10.200.0.10/32"}},
			},
		},
	}
	frr := &FRRParams{
		ASN:             65100,
		RouterID:        "192.168.4.10",
		UnnumberedPeers: []string{"hbn"},
		EVPN:            true,
		AdvertiseAllVNI: false,
	}

	netCfg := ToNetworkConfig(np, frr)
	if netCfg.NetworkMode != "gobgp" {
		t.Errorf("NetworkMode = %q, want gobgp", netCfg.NetworkMode)
	}
	if netCfg.EVPNL2Enabled {
		t.Error("EVPNL2Enabled should be false for pure Type-5 without advertise-all-vni")
	}
	if netCfg.BGPPeerMode != network.PeerModeUnnumbered {
		t.Errorf("BGPPeerMode = %q, want unnumbered", netCfg.BGPPeerMode)
	}
}

func TestToNetworkConfig_ProductionUnderlayVRFOverlayDefault(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			DummyDevices: map[string]DummyConfig{
				"dum.underlay": {Addresses: []string{"192.168.4.10/32"}},
			},
			Tunnels: map[string]TunnelConfig{
				"vx.1000": {
					Mode: "vxlan", ID: 1000, Local: "192.168.4.10",
					Port: 4789, Link: "dum.underlay",
				},
			},
			Bridges: map[string]BridgeConfig{
				"br.provision": {Interfaces: []string{"vx.1000"}, Addresses: []string{"10.200.0.10/32"}},
			},
			VRFs: map[string]VRFConfig{
				"Vrf_underlay": {Table: 10, Interfaces: []string{"hbn", "dum.underlay"}},
			},
		},
	}

	netCfg := ToNetworkConfig(np, &FRRParams{ASN: 65100, RouterID: "192.168.4.10", EVPN: true})
	if netCfg.VRFName != "Vrf_underlay" {
		t.Errorf("VRFName = %q, want Vrf_underlay", netCfg.VRFName)
	}
	if netCfg.UnderlayDummyName != "dum.underlay" {
		t.Errorf("UnderlayDummyName = %q, want dum.underlay", netCfg.UnderlayDummyName)
	}
	if netCfg.VXLANLink != "dum.underlay" {
		t.Errorf("VXLANLink = %q, want dum.underlay", netCfg.VXLANLink)
	}
	if netCfg.BridgeName != "br.provision" {
		t.Errorf("BridgeName = %q, want br.provision", netCfg.BridgeName)
	}
	if !netCfg.OverlayVRFSet {
		t.Fatal("OverlayVRFSet should be true for netplan-derived bridge placement")
	}
	if netCfg.OverlayVRFName != "" {
		t.Errorf("OverlayVRFName = %q, want empty default VRF", netCfg.OverlayVRFName)
	}
	if netCfg.OverlayVRFTableID != 0 {
		t.Errorf("OverlayVRFTableID = %d, want 0", netCfg.OverlayVRFTableID)
	}
}

func TestToNetworkConfig_VXLANLinkComesFromProvisionTunnel(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			DummyDevices: map[string]DummyConfig{
				"dum.underlay": {Addresses: []string{"192.168.4.10/32"}},
			},
			Tunnels: map[string]TunnelConfig{
				// First by name, carries the VNI, but has no link.
				"vx.1000": {
					Mode: "vxlan", ID: 1000, Local: "192.168.4.10", Port: 4789,
				},
				// Later tunnel with a link that must not be used.
				"vx.2000": {
					Mode: "vxlan", ID: 2000, Local: "192.168.4.11",
					Port: 4789, Link: "dum.other",
				},
			},
		},
	}

	netCfg := ToNetworkConfig(np, &FRRParams{ASN: 65100, RouterID: "192.168.4.10", EVPN: true})
	if netCfg.ProvisionVNI != 1000 {
		t.Errorf("ProvisionVNI = %d, want 1000", netCfg.ProvisionVNI)
	}
	if netCfg.VXLANLink != "" {
		t.Errorf("VXLANLink = %q, want empty", netCfg.VXLANLink)
	}
	if netCfg.UnderlayIP != "192.168.4.10" {
		t.Errorf("UnderlayIP = %q, want 192.168.4.10", netCfg.UnderlayIP)
	}
}

func TestToNetworkConfig_OverlayVRFCanDifferFromUnderlayVRF(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			DummyDevices: map[string]DummyConfig{
				"dum.underlay": {Addresses: []string{"192.168.4.10/32"}},
			},
			Tunnels: map[string]TunnelConfig{
				"vx.1000": {
					Mode: "vxlan", ID: 1000, Local: "192.168.4.10",
					Port: 4789, Link: "dum.underlay",
				},
			},
			Bridges: map[string]BridgeConfig{
				"br.provision": {Interfaces: []string{"vx.1000"}, Addresses: []string{"10.200.0.10/32"}},
			},
			VRFs: map[string]VRFConfig{
				"a-underlay": {Table: 10, Interfaces: []string{"hbn", "dum.underlay"}},
				"z-overlay":  {Table: 20, Interfaces: []string{"br.provision"}},
			},
		},
	}

	netCfg := ToNetworkConfig(np, &FRRParams{ASN: 65100, RouterID: "192.168.4.10", EVPN: true})
	if netCfg.VRFName != "a-underlay" {
		t.Errorf("VRFName = %q, want a-underlay", netCfg.VRFName)
	}
	if !netCfg.OverlayVRFSet {
		t.Fatal("OverlayVRFSet should be true")
	}
	if netCfg.OverlayVRFName != "z-overlay" {
		t.Errorf("OverlayVRFName = %q, want z-overlay", netCfg.OverlayVRFName)
	}
	if netCfg.OverlayVRFTableID != 20 {
		t.Errorf("OverlayVRFTableID = %d, want 20", netCfg.OverlayVRFTableID)
	}
}

func TestToNetworkConfig_Netbox(t *testing.T) {
	dhcpFalse := false
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"np0": {Match: &MatchConfig{Name: "ens1f0*"}, DHCP4: &dhcpFalse},
				"np1": {Match: &MatchConfig{Name: "ens1f1*"}, DHCP4: &dhcpFalse},
			},
			Bonds: map[string]BondConfig{
				"bond0": {
					Interfaces: []string{"np0", "np1"}, MTU: 9000,
					Parameters: &BondParameters{Mode: "active-backup", MIIMonitorInterval: 100},
				},
			},
			VLANs: map[string]VLANConfig{
				"bond0.10": {
					ID: 10, Link: "bond0",
					Addresses:   []string{"10.1.1.5/24"},
					Nameservers: &DNSConfig{Addresses: []string{"10.235.119.37", "10.235.119.38"}},
					Routes:      []RouteConfig{{To: "100.64.0.0/10", Via: "10.1.1.254"}},
				},
				"bond0.200": {
					ID: 200, Link: "bond0",
					Addresses:   []string{"10.2.2.5/24"},
					Nameservers: &DNSConfig{Addresses: []string{"10.235.119.37", "10.235.119.38"}},
					Routes:      []RouteConfig{{To: "default", Via: "10.2.2.1"}},
				},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.BondInterfaces != "np0,np1" {
		t.Errorf("BondInterfaces = %q", netCfg.BondInterfaces)
	}
	if netCfg.BondMode != "active-backup" {
		t.Errorf("BondMode = %q", netCfg.BondMode)
	}
	if len(netCfg.VLANs) != 2 {
		t.Errorf("VLANs = %d, want 2", len(netCfg.VLANs))
	}
	foundDTH := false
	for _, v := range netCfg.VLANs {
		if v.ID == 200 && v.Gateway == "10.2.2.1" {
			foundDTH = true
		}
	}
	if !foundDTH {
		t.Error("DTH VLAN should have gateway 10.2.2.1")
	}
	if netCfg.DNSResolvers != "10.235.119.37,10.235.119.38" {
		t.Errorf("DNSResolvers = %q", netCfg.DNSResolvers)
	}
	if netCfg.MTU != 9000 {
		t.Errorf("MTU = %d, want 9000", netCfg.MTU)
	}
	if netCfg.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty", netCfg.NetworkMode)
	}
}

func TestToNetworkConfig_VRFName(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			VRFs: map[string]VRFConfig{
				"cluster": {Table: 10, Interfaces: []string{"br_cluster"}},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.VRFName != "cluster" {
		t.Errorf("VRFName = %q, want cluster", netCfg.VRFName)
	}
}

func TestToNetworkConfig_StaticGateway(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"hbn": {Routes: []RouteConfig{{To: "default", Via: "169.254.1.0", From: "10.1.2.3"}}},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.StaticGateway != "169.254.1.0" {
		t.Errorf("StaticGateway = %q, want 169.254.1.0", netCfg.StaticGateway)
	}
}

func TestToNetworkConfig_StaticEthernetAddress(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"eno1": {
					Addresses: []string{"10.1.2.3/24"},
					Routes:    []RouteConfig{{To: "default", Via: "10.1.2.1"}},
				},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.StaticIP != "10.1.2.3/24" {
		t.Errorf("StaticIP = %q, want 10.1.2.3/24", netCfg.StaticIP)
	}
	if netCfg.StaticIface != "eno1" {
		t.Errorf("StaticIface = %q, want eno1", netCfg.StaticIface)
	}
	if netCfg.StaticGateway != "10.1.2.1" {
		t.Errorf("StaticGateway = %q, want 10.1.2.1", netCfg.StaticGateway)
	}
}

func TestToNetworkConfig_StaticEthernetWildcardAutoDetect(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"all-en": {
					Match:     &MatchConfig{Name: "en*"},
					Addresses: []string{"10.1.2.3/24"},
				},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.StaticIP != "10.1.2.3/24" {
		t.Errorf("StaticIP = %q, want 10.1.2.3/24", netCfg.StaticIP)
	}
	if netCfg.StaticIface != "" {
		t.Errorf("StaticIface = %q, want auto-detect", netCfg.StaticIface)
	}
}

func TestToNetworkConfig_StaticEthernetMACMatchAutoDetect(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"provision-uplink": {
					Match:     &MatchConfig{MAC: "00:11:22:33:44:55"},
					Addresses: []string{"10.1.2.3/24"},
				},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.StaticIP != "10.1.2.3/24" {
		t.Errorf("StaticIP = %q, want 10.1.2.3/24", netCfg.StaticIP)
	}
	if netCfg.StaticIface != "" {
		t.Errorf("StaticIface = %q, want auto-detect", netCfg.StaticIface)
	}
}

func TestToNetworkConfig_StaticBondAddress(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"eno1": {},
				"eno2": {},
			},
			Bonds: map[string]BondConfig{
				"bond0": {
					Interfaces: []string{"eno1", "eno2"},
					Addresses:  []string{"10.5.0.10/24"},
					Parameters: &BondParameters{Mode: "active-backup"},
				},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.StaticIP != "10.5.0.10/24" {
		t.Errorf("StaticIP = %q, want 10.5.0.10/24", netCfg.StaticIP)
	}
	if netCfg.StaticIface != "bond0" {
		t.Errorf("StaticIface = %q, want bond0", netCfg.StaticIface)
	}
	if netCfg.BondInterfaces != "eno1,eno2" {
		t.Errorf("BondInterfaces = %q, want eno1,eno2", netCfg.BondInterfaces)
	}
}

func TestToNetworkConfig_StaticBondAddressUsesCreatedBondName(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"eno1": {},
				"eno2": {},
			},
			Bonds: map[string]BondConfig{
				"provision-bond": {
					Interfaces: []string{"eno1", "eno2"},
					Addresses:  []string{"10.5.0.10/24"},
				},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.StaticIP != "10.5.0.10/24" {
		t.Errorf("StaticIP = %q, want 10.5.0.10/24", netCfg.StaticIP)
	}
	if netCfg.StaticIface != "bond0" {
		t.Errorf("StaticIface = %q, want bond0", netCfg.StaticIface)
	}
}

func TestToNetworkConfig_DNSDedup(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Ethernets: map[string]EthernetConfig{
				"eth0": {Nameservers: &DNSConfig{Addresses: []string{"8.8.8.8"}}},
			},
			VLANs: map[string]VLANConfig{
				"v10": {ID: 10, Nameservers: &DNSConfig{Addresses: []string{"8.8.8.8", "8.8.4.4"}}},
			},
		},
	}
	netCfg := ToNetworkConfig(np, nil)
	if netCfg.DNSResolvers != "8.8.8.8,8.8.4.4" {
		t.Errorf("DNSResolvers = %q, want 8.8.8.8,8.8.4.4", netCfg.DNSResolvers)
	}
}

// --- HasNetplanFiles tests ---

func TestToNetworkConfig_PeerModeDual(t *testing.T) {
	np := &Config{
		Network: NetworkSection{
			Tunnels: map[string]TunnelConfig{
				"vx100": {Mode: "vxlan", ID: 100, Local: "10.0.0.1"},
			},
		},
	}
	frr := &FRRParams{
		ASN:             65001,
		UnnumberedPeers: []string{"eth0"},
		NumberedPeers:   []string{"10.0.0.2"},
		EVPN:            true,
	}
	netCfg := ToNetworkConfig(np, frr)
	if netCfg.BGPPeerMode != network.PeerModeDual {
		t.Errorf("BGPPeerMode = %q, want dual", netCfg.BGPPeerMode)
	}
	if netCfg.BGPNeighbors != "10.0.0.2" {
		t.Errorf("BGPNeighbors = %q, want 10.0.0.2", netCfg.BGPNeighbors)
	}
}

func TestToNetworkConfig_BridgeMAC(t *testing.T) {
	tunnels := map[string]TunnelConfig{"vx.1000": {Mode: "vxlan", ID: 1000, Local: "192.168.4.10"}}
	tests := []struct {
		name    string
		bridges map[string]BridgeConfig
		want    string
	}{
		{"no_macaddress", map[string]BridgeConfig{
			"br.provision": {Interfaces: []string{"vx.1000"}},
		}, ""},
		{"provision_bridge_without_addresses", map[string]BridgeConfig{
			"br.provision": {Interfaces: []string{"vx.1000"}, MACAddress: "02:00:00:00:10:01"},
		}, "02:00:00:00:10:01"},
		{"other_bridge_ignored", map[string]BridgeConfig{
			"br.other":     {Interfaces: []string{"eth1"}, MACAddress: "02:00:00:00:10:02"},
			"br.provision": {Interfaces: []string{"vx.1000"}, Addresses: []string{"10.100.0.20/32"}},
		}, ""},
		{"selected_bridge_mac", map[string]BridgeConfig{
			"br.a": {Interfaces: []string{"vx.1000"}, MACAddress: "02:00:00:00:10:03"},
			"br.b": {Interfaces: []string{"vx.1000"}, Addresses: []string{"10.100.0.20/32"}, MACAddress: "02:00:00:00:10:04"},
		}, "02:00:00:00:10:04"},
		{"addressless_bridge_not_selected", map[string]BridgeConfig{
			"br.a": {Interfaces: []string{"vx.1000"}, MACAddress: "02:00:00:00:10:03"},
			"br.b": {Interfaces: []string{"vx.1000"}, Addresses: []string{"10.100.0.20/32"}},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			np := &Config{Network: NetworkSection{Tunnels: tunnels, Bridges: tt.bridges}}
			if got := ToNetworkConfig(np, nil).BridgeMAC; got != tt.want {
				t.Errorf("BridgeMAC = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasNetplanFiles(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"has yaml", map[string]string{"10-test.yaml": "x"}, true},
		{"has yml", map[string]string{"10-test.yml": "x"}, true},
		{"no yaml", map[string]string{"readme.txt": "x"}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for n, c := range tt.files {
				writeFile(t, dir, n, c)
			}
			if got := HasNetplanFiles(dir); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasNetplanFiles_NonexistentDir(t *testing.T) {
	if HasNetplanFiles("/nonexistent/path") {
		t.Error("expected false")
	}
}
