package config

import (
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/network/netplan"
)

func TestValidateNetworkPrefixLists(t *testing.T) {
	c := &Config{}
	c.Network.LoopbackAddresses = "2001:db8::17/128, 192.0.2.1/32"
	c.Network.BlackholeRoutes = "2001:db8::/64"
	c.Network.EVPN.Type5Networks = "2001:db8::17/128,2001:db8::/64"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	c.Network.BlackholeRoutes = "2001:db8::/64,2001:db8::1"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `network.blackholeRoutes: "2001:db8::1" is not a CIDR`) {
		t.Fatalf("Validate = %v, want a CIDR error for network.blackholeRoutes", err)
	}
}

// The native peer and device options are checked with their ranges.
func TestValidateNetworkOptionRanges(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(*Config)
		want string
	}{
		{"allowASIn 11", func(c *Config) { c.Network.BGP.AllowASIn = new(uint32(11)) }, "allowASIn 11 must be 0 to 10"},
		{"allowASIn 10", func(c *Config) { c.Network.BGP.AllowASIn = new(uint32(10)) }, ""},
		{"bad bridgeMAC", func(c *Config) { c.Network.EVPN.BridgeMAC = "02:00:00:00:10" }, "is not a 48-bit MAC address"},
		{"bridgeMAC", func(c *Config) { c.Network.EVPN.BridgeMAC = "02:00:00:00:10:01" }, ""},
		{"vxlanMTU 575", func(c *Config) { c.Network.Overlay.VXLANMTU = 575 }, "network.overlay.vxlanMTU 575 must be 576 to 65535"},
		{"vxlanMTU 576", func(c *Config) { c.Network.Overlay.VXLANMTU = 576 }, ""},
		{"nicWaitSeconds -2", func(c *Config) { c.Network.NICWaitSeconds = -2 }, "NIC_WAIT_SECONDS) must be -1"},
		{"nicWaitSeconds -1", func(c *Config) { c.Network.NICWaitSeconds = -1 }, ""},
		{"nicWaitSeconds 3600", func(c *Config) { c.Network.NICWaitSeconds = 3600 }, ""},
		{"nicWaitSeconds 3601", func(c *Config) { c.Network.NICWaitSeconds = 3601 }, "NIC_WAIT_SECONDS) must be -1"},
		{"bridgeMTU 575", func(c *Config) { c.Network.Overlay.BridgeMTU = 575 }, "network.overlay.bridgeMTU 575 must be 576 to 65535"},
		{"devices route metric", func(c *Config) {
			c.Network.Devices = &NetworkDevices{Ethernets: map[string]netplan.EthernetConfig{
				"eth0": {Routes: []netplan.RouteConfig{{To: "default", Via: "192.0.2.1", Metric: 50}}},
			}}
		}, "network.devices: "},
		{"devices", func(c *Config) {
			c.Network.Devices = &NetworkDevices{Ethernets: map[string]netplan.EthernetConfig{
				"eth0": {Routes: []netplan.RouteConfig{{To: "default", Via: "192.0.2.1"}}},
			}}
		}, ""},
		{"gracefulRestartTime 4096", func(c *Config) { c.Network.BGP.GracefulRestartTime = 4096 }, "gracefulRestartTime 4096 must be 1 to 4095"},
		{"gracefulRestartTime 4095", func(c *Config) { c.Network.BGP.GracefulRestartTime = 4095 }, ""},
	} {
		c := &Config{}
		tt.set(c)
		err := c.Validate()
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: Validate = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestValidateOverlayDevices(t *testing.T) {
	c := &Config{}
	c.Network.Overlay.VXLANMAC = "02:54:c0:00:02:13"
	c.Network.Overlay.VXLANMTU = 9000
	c.Network.Overlay.BridgeMTU = 576
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	c.Network.Overlay.VXLANMAC = "02:54:c0"
	c.Network.Overlay.BridgeMTU = 70000
	err := c.Validate()
	for _, want := range []string{`network.overlay.vxlanMAC "02:54:c0"`, "network.overlay.bridgeMTU 70000"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate = %v, want %q", err, want)
		}
	}
}
