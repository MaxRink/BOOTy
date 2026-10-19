package dhcp4

import (
	"errors"
	"net"
	"time"
)

// errTimeout ends a wait without a reply.
var errTimeout = errors.New("no DHCP reply")

// transport sends DHCP messages of one NIC and receives the replies.
type transport interface {
	// broadcast sends m to 255.255.255.255:67 as a link broadcast, from src
	// (nil: 0.0.0.0).
	broadcast(src net.IP, m *message) error
	// unicast sends m from src:68 to dst:67.
	unicast(src, dst net.IP, m *message) error
	// receive returns the BOOTP payload of the next UDP datagram to port 68
	// within timeout, or errTimeout.
	receive(timeout time.Duration) ([]byte, error)
	close() error
}
