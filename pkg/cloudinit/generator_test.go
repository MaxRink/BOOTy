package cloudinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerate_Basic(t *testing.T) {
	cfg := &Config{
		Hostname:   "worker-001",
		FQDN:       "worker-001.example.com",
		InstanceID: "SN123456",
		SSHKeys:    []string{"ssh-ed25519 AAAA..."},
		Timezone:   "UTC",
	}

	ud, md, nc := Generate(cfg)

	if ud.Hostname != "worker-001" {
		t.Errorf("hostname = %q, want %q", ud.Hostname, "worker-001")
	}
	if !ud.ManageEtcHosts {
		t.Error("ManageEtcHosts should be true")
	}
	if md.InstanceID != "SN123456" {
		t.Errorf("instance-id = %q, want %q", md.InstanceID, "SN123456")
	}
	if md.Platform != "booty" {
		t.Errorf("platform = %q, want %q", md.Platform, "booty")
	}
	if nc.Version != 2 {
		t.Errorf("network version = %d, want 2", nc.Version)
	}
}

func TestGenerate_StaticIP(t *testing.T) {
	cfg := &Config{
		Hostname:   "node-1",
		InstanceID: "SN1",
		StaticIP:   "10.0.0.5/24",
		Gateway:    "10.0.0.1",
		DNS:        []string{"8.8.8.8", "8.8.4.4"},
	}

	_, _, nc := Generate(cfg)

	eth, ok := nc.Ethernets["id0"]
	if !ok {
		t.Fatal("expected ethernets[id0]")
	}
	if eth.DHCP4 {
		t.Error("DHCP4 should be false for static IP")
	}
	if len(eth.Addresses) != 1 || eth.Addresses[0] != "10.0.0.5/24" {
		t.Errorf("addresses = %v, want [10.0.0.5/24]", eth.Addresses)
	}
}

func TestGenerate_StaticIPv6(t *testing.T) {
	cfg := &Config{
		Hostname:   "node-v6",
		InstanceID: "SNV6",
		StaticIP:   "2001:db8::10/64",
		Gateway:    "2001:db8::1",
		DNS:        []string{"2001:4860:4860::8888"},
	}

	_, _, nc := Generate(cfg)

	eth, ok := nc.Ethernets["id0"]
	if !ok {
		t.Fatal("expected ethernets[id0]")
	}
	if eth.Gateway4 != "" {
		t.Errorf("gateway4 = %q, want empty for IPv6 static config", eth.Gateway4)
	}
	if eth.Gateway6 != "2001:db8::1" {
		t.Errorf("gateway6 = %q, want 2001:db8::1", eth.Gateway6)
	}
	if len(eth.Addresses) != 1 || eth.Addresses[0] != "2001:db8::10/64" {
		t.Errorf("addresses = %v, want [2001:db8::10/64]", eth.Addresses)
	}
}

func TestGenerate_StaticIPUsesConfiguredInterface(t *testing.T) {
	cfg := &Config{
		Hostname:   "node-1",
		InstanceID: "SN1",
		StaticIP:   "10.0.0.5/24",
		Interface:  " eth0 ",
	}

	_, _, nc := Generate(cfg)

	if _, ok := nc.Ethernets["id0"]; ok {
		t.Fatal("unexpected fallback id0 ethernet when interface is configured")
	}
	if _, ok := nc.Ethernets["eth0"]; !ok {
		t.Fatalf("ethernets = %#v, want eth0", nc.Ethernets)
	}
}

func TestGenerate_DHCP(t *testing.T) {
	cfg := &Config{
		Hostname:   "dhcp-node",
		InstanceID: "SN2",
	}

	_, _, nc := Generate(cfg)

	eth, ok := nc.Ethernets["id0"]
	if !ok {
		t.Fatal("expected ethernets[id0]")
	}
	if !eth.DHCP4 {
		t.Error("DHCP4 should be true when no static IP")
	}
}

func TestGenerate_DHCPUsesConfiguredInterface(t *testing.T) {
	cfg := &Config{
		Hostname:   "dhcp-node",
		InstanceID: "SN2",
		Interface:  "ens3",
	}

	_, _, nc := Generate(cfg)

	if _, ok := nc.Ethernets["ens3"]; !ok {
		t.Fatalf("ethernets = %#v, want ens3", nc.Ethernets)
	}
}

func TestGenerate_Bond(t *testing.T) {
	cfg := &Config{
		Hostname:   "bond-node",
		InstanceID: "SN3",
		BondIfaces: []string{"eth0", "eth1"},
		BondMode:   "802.3ad",
		StaticIP:   "10.0.0.10/24",
	}

	_, _, nc := Generate(cfg)

	if len(nc.Ethernets) != 0 {
		t.Error("ethernets should be empty when using bonds")
	}
	bond, ok := nc.Bonds["bond0"]
	if !ok {
		t.Fatal("expected bonds[bond0]")
	}
	if len(bond.Interfaces) != 2 {
		t.Errorf("bond interfaces = %d, want 2", len(bond.Interfaces))
	}
	if bond.Parameters.Mode != "802.3ad" {
		t.Errorf("bond mode = %q, want %q", bond.Parameters.Mode, "802.3ad")
	}
}

func TestGenerate_BondIPv6(t *testing.T) {
	cfg := &Config{
		Hostname:   "bond-v6",
		InstanceID: "SNB6",
		BondIfaces: []string{"eth0", "eth1"},
		StaticIP:   "2001:db8:1::10/64",
		Gateway:    "2001:db8:1::1",
	}

	_, _, nc := Generate(cfg)

	bond, ok := nc.Bonds["bond0"]
	if !ok {
		t.Fatal("expected bonds[bond0]")
	}
	if bond.Gateway4 != "" {
		t.Errorf("gateway4 = %q, want empty for IPv6 bond", bond.Gateway4)
	}
	if bond.Gateway6 != "2001:db8:1::1" {
		t.Errorf("gateway6 = %q, want 2001:db8:1::1", bond.Gateway6)
	}
}

func TestGenerate_VLANs(t *testing.T) {
	cfg := &Config{
		Hostname:   "vlan-node",
		InstanceID: "SNVLAN",
		VLANs: []VLANInput{
			{ID: 200, Parent: "eno1", Address: "10.200.0.42/24", Gateway: "10.200.0.1"},
			{ID: 300, Parent: "eno2"},
		},
		DNS: []string{"1.1.1.1"},
	}

	_, _, nc := Generate(cfg)

	if _, ok := nc.Ethernets["id0"]; ok {
		t.Fatalf("unexpected fallback id0 ethernet for VLAN-only config: %#v", nc.Ethernets)
	}
	if _, ok := nc.Ethernets["eno1"]; !ok {
		t.Fatalf("missing parent ethernet eno1: %#v", nc.Ethernets)
	}
	staticVLAN, ok := nc.VLANs["eno1.200"]
	if !ok {
		t.Fatalf("missing static VLAN: %#v", nc.VLANs)
	}
	if staticVLAN.ID != 200 || staticVLAN.Link != "eno1" || staticVLAN.Gateway4 != "10.200.0.1" {
		t.Fatalf("unexpected static VLAN config: %#v", staticVLAN)
	}
	if staticVLAN.Nameservers == nil || staticVLAN.Nameservers.Addresses[0] != "1.1.1.1" {
		t.Fatalf("unexpected VLAN nameservers: %#v", staticVLAN.Nameservers)
	}
	dhcpVLAN, ok := nc.VLANs["eno2.300"]
	if !ok {
		t.Fatalf("missing DHCP VLAN: %#v", nc.VLANs)
	}
	if !dhcpVLAN.DHCP4 || len(dhcpVLAN.Addresses) != 0 {
		t.Fatalf("unexpected DHCP VLAN config: %#v", dhcpVLAN)
	}
}

func TestGenerate_BondVLANDoesNotDHCPUntaggedBond(t *testing.T) {
	cfg := &Config{
		Hostname:   "bond-vlan",
		InstanceID: "SNBV",
		BondIfaces: []string{"eth0", "eth1"},
		VLANs:      []VLANInput{{ID: 200, Parent: "bond0", Address: "10.200.0.42/24"}},
	}

	_, _, nc := Generate(cfg)

	bond, ok := nc.Bonds["bond0"]
	if !ok {
		t.Fatal("expected bonds[bond0]")
	}
	if bond.DHCP4 {
		t.Fatalf("bond should not DHCP on untagged bond when bond VLAN is configured: %#v", bond)
	}
	vlan, ok := nc.VLANs["bond0.200"]
	if !ok {
		t.Fatalf("missing bond VLAN: %#v", nc.VLANs)
	}
	if vlan.Link != "bond0" || len(nc.Ethernets) != 0 {
		t.Fatalf("unexpected bond VLAN topology: vlan=%#v ethernets=%#v", vlan, nc.Ethernets)
	}
}

func TestGenerate_WithUsers(t *testing.T) {
	cfg := &Config{
		Hostname:   "user-node",
		InstanceID: "SN4",
		Users: []User{
			{Name: "admin", Groups: "sudo", Shell: "/bin/bash", Sudo: "ALL=(ALL) NOPASSWD:ALL"},
		},
	}

	ud, _, _ := Generate(cfg)

	if len(ud.Users) != 1 {
		t.Fatalf("users count = %d, want 1", len(ud.Users))
	}
	if ud.Users[0].Name != "admin" {
		t.Errorf("user name = %q, want %q", ud.Users[0].Name, "admin")
	}
}

func TestUserData_Render(t *testing.T) {
	ud := &UserData{
		Hostname: "test-host",
		Timezone: "Europe/Berlin",
	}

	data, err := ud.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	s := string(data)
	if !strings.HasPrefix(s, "#cloud-config\n") {
		t.Error("user-data should start with #cloud-config header")
	}
	if !strings.Contains(s, "hostname: test-host") {
		t.Error("user-data should contain hostname")
	}
}

func TestMetaData_Render(t *testing.T) {
	md := &MetaData{
		InstanceID:    "i-12345",
		LocalHostname: "test-host",
		Platform:      "booty",
	}

	data, err := md.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "instance-id: i-12345") {
		t.Error("meta-data should contain instance-id")
	}
}

func TestNetworkConfig_Render(t *testing.T) {
	nc := &NetworkConfig{
		Version: 2,
		Ethernets: map[string]EthConfig{
			"eth0": {DHCP4: true},
		},
	}

	data, err := nc.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "version: 2") {
		t.Error("network-config should contain version: 2")
	}
}

func TestInjectNoCloud(t *testing.T) {
	root := t.TempDir()

	ud := &UserData{Hostname: "inject-test"}
	md := &MetaData{InstanceID: "test-id", LocalHostname: "inject-test", Platform: "booty"}
	nc := &NetworkConfig{Version: 2, Ethernets: map[string]EthConfig{"eth0": {DHCP4: true}}}

	if err := InjectNoCloud(root, ud, md, nc); err != nil {
		t.Fatalf("InjectNoCloud: %v", err)
	}

	seedDir := filepath.Join(root, "var", "lib", "cloud", "seed", "nocloud")
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		data, err := os.ReadFile(filepath.Join(seedDir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	udData, _ := os.ReadFile(filepath.Join(seedDir, "user-data"))
	if !strings.HasPrefix(string(udData), "#cloud-config\n") {
		t.Error("user-data should start with #cloud-config")
	}
}

func TestInjectConfigDrive(t *testing.T) {
	root := t.TempDir()

	ud := &UserData{Hostname: "configdrive-test"}
	md := &MetaData{InstanceID: "test-id", LocalHostname: "configdrive-test", Platform: "booty"}
	nc := &NetworkConfig{
		Version: 2,
		Ethernets: map[string]EthConfig{
			"eth0": {
				Addresses:   []string{"10.0.0.5/24"},
				Gateway4:    "10.0.0.1",
				Nameservers: &NSConfig{Addresses: []string{"1.1.1.1"}},
			},
		},
	}

	if err := InjectConfigDrive(root, ud, md, nc); err != nil {
		t.Fatalf("InjectConfigDrive: %v", err)
	}

	seedDir := filepath.Join(root, "var", "lib", "cloud", "seed", "config_drive", "openstack", "latest")
	for _, name := range []string{"user_data", "meta_data.json", "network_data.json"} {
		data, err := os.ReadFile(filepath.Join(seedDir, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	assertConfigDriveMetadata(t, filepath.Join(seedDir, "meta_data.json"))
	assertConfigDriveNetworkData(t, filepath.Join(seedDir, "network_data.json"))
}

func TestConfigDriveNetworkDataIPv6AndVLAN(t *testing.T) {
	nc := &NetworkConfig{
		Version: 2,
		Ethernets: map[string]EthConfig{
			"eth0": {},
		},
		VLANs: map[string]VLANConfig{
			"eth0.200": {
				ID:          200,
				Link:        "eth0",
				Addresses:   []string{"2001:db8:200::42/64"},
				Gateway6:    "2001:db8:200::1",
				Nameservers: &NSConfig{Addresses: []string{"2001:4860:4860::8888"}},
			},
		},
	}

	networkData, err := configDriveNetworkData(nc)
	if err != nil {
		t.Fatalf("configDriveNetworkData: %v", err)
	}

	vlanLink := findOpenStackLink(networkData.Links, "eth0.200")
	if vlanLink == nil {
		t.Fatalf("missing VLAN link: %+v", networkData.Links)
	}
	if vlanLink.Type != "vlan" || vlanLink.VLANID == nil || *vlanLink.VLANID != 200 || vlanLink.VLANLink != "eth0" {
		t.Fatalf("unexpected VLAN link: %+v", *vlanLink)
	}
	if vlanLink.VLANMACAddress == nil {
		t.Fatalf("VLAN link must include vlan_mac_address for cloud-init OpenStack conversion: %+v", *vlanLink)
	}

	v6Network := findOpenStackNetwork(networkData.Networks, "eth0.200")
	if v6Network == nil {
		t.Fatalf("missing VLAN IPv6 network: %+v", networkData.Networks)
	}
	if v6Network.Type != "ipv6" || v6Network.IPAddress != "2001:db8:200::42" || v6Network.Netmask != "64" {
		t.Fatalf("unexpected IPv6 network: %+v", *v6Network)
	}
	if v6Network.Gateway != "2001:db8:200::1" || len(v6Network.Routes) != 1 || v6Network.Routes[0].Network != "::/0" {
		t.Fatalf("unexpected IPv6 gateway/routes: %+v", *v6Network)
	}
	if len(v6Network.Services) != 1 || v6Network.Services[0].Address != "2001:4860:4860::8888" {
		t.Fatalf("unexpected network services: %+v", v6Network.Services)
	}
}

func TestInjectConfigDriveBondNetworkData(t *testing.T) {
	root := t.TempDir()
	nc := &NetworkConfig{
		Version: 2,
		Ethernets: map[string]EthConfig{
			"eth0": {Match: &MatchConfig{MACAddress: "aa:bb:cc:00:00:01"}},
			"eth1": {Match: &MatchConfig{MACAddress: "aa:bb:cc:00:00:02"}},
		},
		Bonds: map[string]BondConfig{
			"bond0": {
				Interfaces: []string{"eth0", "eth1"},
				Parameters: &BondParams{Mode: "802.3ad"},
				Addresses:  []string{"10.10.0.5/24"},
				Gateway4:   "10.10.0.1",
			},
		},
	}
	if err := InjectConfigDrive(root, &UserData{}, &MetaData{InstanceID: "bond-test"}, nc); err != nil {
		t.Fatalf("InjectConfigDrive: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "var", "lib", "cloud", "seed", "config_drive", "openstack", "latest", "network_data.json"))
	if err != nil {
		t.Fatalf("read network_data.json: %v", err)
	}
	var networkData openStackNetworkData
	if err := json.Unmarshal(raw, &networkData); err != nil {
		t.Fatalf("unmarshal network_data.json: %v", err)
	}

	if len(networkData.Links) != 3 {
		t.Fatalf("links = %+v, want eth0, eth1 and bond0 once each", networkData.Links)
	}
	for id, mac := range map[string]string{"eth0": "aa:bb:cc:00:00:01", "eth1": "aa:bb:cc:00:00:02"} {
		link := findOpenStackLink(networkData.Links, id)
		if link == nil || link.Type != "phy" || link.EthernetMACAddress != mac {
			t.Fatalf("member link %s = %+v, want phy link with MAC %s", id, link, mac)
		}
	}
	bond := findOpenStackLink(networkData.Links, "bond0")
	if bond == nil {
		t.Fatalf("missing bond0 link: %+v", networkData.Links)
	}
	if bond.Type != "bond" || bond.BondMode != "802.3ad" || strings.Join(bond.BondLinks, ",") != "eth0,eth1" {
		t.Fatalf("bond0 link = %+v, want type bond, mode 802.3ad, bond_links [eth0 eth1]", *bond)
	}

	if len(networkData.Networks) != 1 {
		t.Fatalf("networks = %+v, want one static network on bond0", networkData.Networks)
	}
	network := networkData.Networks[0]
	if network.Link != "bond0" || network.Type != "ipv4" || network.IPAddress != "10.10.0.5" ||
		network.Netmask != "255.255.255.0" || network.Gateway != "10.10.0.1" {
		t.Fatalf("bond0 network = %+v, want ipv4 10.10.0.5/255.255.255.0 via 10.10.0.1 on bond0", network)
	}
}

func assertConfigDriveMetadata(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meta_data.json: %v", err)
	}

	var md openStackMetaData
	if err := json.Unmarshal(data, &md); err != nil {
		t.Fatalf("unmarshal meta_data.json: %v", err)
	}
	if md.UUID != "test-id" || md.Hostname != "configdrive-test" || md.Name != "configdrive-test" {
		t.Fatalf("unexpected metadata: %+v", md)
	}
}

func assertConfigDriveNetworkData(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read network_data.json: %v", err)
	}

	var networkData openStackNetworkData
	if err := json.Unmarshal(data, &networkData); err != nil {
		t.Fatalf("unmarshal network_data.json: %v", err)
	}
	if len(networkData.Links) != 1 || networkData.Links[0].ID != "eth0" {
		t.Fatalf("unexpected links: %+v", networkData.Links)
	}
	if len(networkData.Networks) != 1 || networkData.Networks[0].IPAddress != "10.0.0.5" {
		t.Fatalf("unexpected networks: %+v", networkData.Networks)
	}
	if len(networkData.Networks[0].Services) != 1 || networkData.Networks[0].Services[0].Address != "1.1.1.1" {
		t.Fatalf("unexpected network services: %+v", networkData.Networks[0].Services)
	}
	if len(networkData.Services) != 1 || networkData.Services[0].Address != "1.1.1.1" {
		t.Fatalf("unexpected services: %+v", networkData.Services)
	}
}

func findOpenStackLink(links []openStackLink, id string) *openStackLink {
	for i := range links {
		if links[i].ID == id {
			return &links[i]
		}
	}
	return nil
}

func findOpenStackNetwork(networks []openStackNetwork, link string) *openStackNetwork {
	for i := range networks {
		if networks[i].Link == link {
			return &networks[i]
		}
	}
	return nil
}

func TestAddressList(t *testing.T) {
	if got := addressList(""); got != nil {
		t.Errorf("addressList empty = %v, want nil", got)
	}
	got := addressList("10.0.0.1/24")
	if len(got) != 1 || got[0] != "10.0.0.1/24" {
		t.Errorf("addressList ip = %v", got)
	}
}

func TestGenerate_BondIfaces_EmptyStringsFiltered(t *testing.T) {
	cfg := &Config{
		Hostname:   "node-1",
		InstanceID: "SN1",
		BondIfaces: []string{"", " ", ""},
	}

	_, _, nc := Generate(cfg)

	// All empty/whitespace ifaces should be filtered out, producing DHCP config.
	if len(nc.Bonds) != 0 {
		t.Error("bonds should be empty when all BondIfaces are empty strings")
	}
	eth, ok := nc.Ethernets["id0"]
	if !ok {
		t.Fatal("expected ethernets[id0]")
	}
	if !eth.DHCP4 {
		t.Error("DHCP4 should be true when bond ifaces are all empty")
	}
}

func TestGenerate_StaticIP_NoDNS_NoNameservers(t *testing.T) {
	cfg := &Config{
		Hostname:   "node-1",
		InstanceID: "SN1",
		StaticIP:   "10.0.0.5/24",
		Gateway:    "10.0.0.1",
		DNS:        nil,
	}

	_, _, nc := Generate(cfg)

	eth := nc.Ethernets["id0"]
	if eth.Nameservers != nil {
		t.Error("Nameservers should be nil when DNS is empty")
	}
}

func TestGenerate_BondDHCP_NoGateway(t *testing.T) {
	cfg := &Config{
		Hostname:   "bond-dhcp",
		InstanceID: "SN5",
		BondIfaces: []string{"eth0", "eth1"},
		Gateway:    "10.0.0.1",
	}

	_, _, nc := Generate(cfg)

	bond, ok := nc.Bonds["bond0"]
	if !ok {
		t.Fatal("expected bonds[bond0]")
	}
	if !bond.DHCP4 {
		t.Error("DHCP4 should be true when static IP is not set")
	}
	if bond.Gateway4 != "" {
		t.Errorf("gateway4 = %q, want empty for DHCP bond", bond.Gateway4)
	}
}

func TestInjectNoCloud_NilInput(t *testing.T) {
	root := t.TempDir()
	err := InjectNoCloud(root, nil, &MetaData{}, &NetworkConfig{})
	if err == nil {
		t.Error("expected error for nil user-data")
	}
}

func TestInjectNoCloud_InvalidRootPath(t *testing.T) {
	ud := &UserData{Hostname: "test"}
	md := &MetaData{InstanceID: "id", LocalHostname: "test", Platform: "booty"}
	nc := &NetworkConfig{Version: 2}

	if err := InjectNoCloud("", ud, md, nc); err == nil {
		t.Error("expected error for empty rootPath")
	}
	if err := InjectNoCloud("relative/path", ud, md, nc); err == nil {
		t.Error("expected error for relative rootPath")
	}
}

func TestInjectConfigDrive_InvalidStaticAddress(t *testing.T) {
	root := t.TempDir()
	ud := &UserData{Hostname: "test"}
	md := &MetaData{InstanceID: "id", LocalHostname: "test", Platform: "booty"}
	nc := &NetworkConfig{
		Version: 2,
		Ethernets: map[string]EthConfig{
			"eth0": {Addresses: []string{"not-a-cidr"}},
		},
	}

	if err := InjectConfigDrive(root, ud, md, nc); err == nil {
		t.Fatal("expected error for invalid static CIDR")
	}
}

func TestInjectNoCloud_AtomicWriteCleanup(t *testing.T) {
	root := t.TempDir()
	ud := &UserData{Hostname: "test"}
	md := &MetaData{InstanceID: "id", LocalHostname: "test", Platform: "booty"}
	nc := &NetworkConfig{Version: 2}

	if err := InjectNoCloud(root, ud, md, nc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify seed files exist after atomic write.
	seedDir := filepath.Join(root, "var", "lib", "cloud", "seed", "nocloud")
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		if _, err := os.Stat(filepath.Join(seedDir, name)); err != nil {
			t.Errorf("seed file %s missing: %v", name, err)
		}
		// Verify no .tmp files were left behind.
		if _, err := os.Stat(filepath.Join(seedDir, name+".tmp")); err == nil {
			t.Errorf("temp file %s.tmp should not exist after successful write", name)
		}
	}
}
