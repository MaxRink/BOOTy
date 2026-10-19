package dhcp6

import (
	"errors"
	"time"
)

// errTimeout ends a wait without a reply.
var errTimeout = errors.New("no DHCPv6 reply")

// transport sends to All_DHCP_Relay_Agents_and_Servers on one NIC and
// receives the replies.
type transport interface {
	send(msg []byte) error
	receive(timeout time.Duration) ([]byte, error)
	close() error
}
