package config

import (
	"strings"
	"testing"
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
	} {
		c := &Config{}
		tt.set(c)
		err := c.Validate()
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: Validate = %v, want %q", tt.name, err, tt.want)
		}
	}
}
