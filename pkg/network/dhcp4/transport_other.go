//go:build !linux

package dhcp4

import (
	"errors"
	"net"
)

func openTransport(*net.Interface) (transport, error) {
	return nil, errors.New("DHCP needs Linux packet sockets")
}
