//go:build !linux

package dhcp6

import (
	"errors"
	"net"
)

func openTransport(*net.Interface) (transport, error) {
	return nil, errors.New("DHCPv6 needs Linux")
}
