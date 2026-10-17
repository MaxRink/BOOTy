package network

import (
	"strings"
	"testing"
)

func TestDHCPNTPServers(t *testing.T) {
	t.Cleanup(func() { setDHCPNTPServers(nil) })
	setDHCPNTPServers([]byte{192, 0, 2, 1, 0, 0, 0, 0, 198, 51, 100, 7})
	if got := strings.Join(DHCPNTPServers(), ","); got != "192.0.2.1,198.51.100.7" {
		t.Fatalf("DHCPNTPServers = %q", got)
	}
	// The caller gets a copy.
	DHCPNTPServers()[0] = "changed"
	if DHCPNTPServers()[0] != "192.0.2.1" {
		t.Fatal("DHCPNTPServers returned the internal slice")
	}
	setDHCPNTPServers([]byte{192, 0, 2})
	if got := DHCPNTPServers(); got != nil {
		t.Fatalf("malformed option 42 = %v, want nil", got)
	}
}
