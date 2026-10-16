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
