package network

import (
	"net"
	"sync"
)

// dhcpNTP holds the NTP servers (option 42) of the applied DHCP lease.
var dhcpNTP struct {
	mu      sync.Mutex
	servers []string
}

// DHCPNTPServers returns the NTP servers of the applied DHCP lease (option
// 42), or nil when DHCP did not run or the lease had none.
func DHCPNTPServers() []string {
	dhcpNTP.mu.Lock()
	defer dhcpNTP.mu.Unlock()
	return append([]string(nil), dhcpNTP.servers...)
}

// setDHCPNTPServers records the option 42 payload of a lease: IPv4
// addresses, four bytes each. A malformed payload clears the list.
func setDHCPNTPServers(data []byte) {
	var servers []string
	if len(data) > 0 && len(data)%net.IPv4len == 0 {
		for i := 0; i < len(data); i += net.IPv4len {
			ip := net.IP(data[i : i+net.IPv4len])
			if ip.IsUnspecified() {
				continue
			}
			servers = append(servers, ip.String())
		}
	}
	dhcpNTP.mu.Lock()
	dhcpNTP.servers = servers
	dhcpNTP.mu.Unlock()
}
