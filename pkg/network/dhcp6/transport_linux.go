//go:build linux

package dhcp6

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// allServers is All_DHCP_Relay_Agents_and_Servers (RFC 8415 section 7.1).
var allServers = net.ParseIP("ff02::1:2")

// udpTransport is a UDP socket on port 546 of the NIC; the kernel sends
// from the link-local address.
type udpTransport struct {
	iface *net.Interface
	conn  net.PacketConn
}

func openTransport(iface *net.Interface) (transport, error) {
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) {
			for _, o := range []int{unix.SO_REUSEADDR, unix.SO_REUSEPORT} {
				if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, o, 1); serr != nil {
					return
				}
			}
			serr = unix.BindToDevice(int(fd), iface.Name)
		})
		return errors.Join(err, serr)
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp6", "[::]:546")
	if err != nil {
		return nil, fmt.Errorf("DHCPv6 socket on %s: %w", iface.Name, err)
	}
	return &udpTransport{iface: iface, conn: conn}, nil
}

func (t *udpTransport) send(msg []byte) error {
	if _, err := t.conn.WriteTo(msg, &net.UDPAddr{IP: allServers, Port: 547, Zone: t.iface.Name}); err != nil {
		return fmt.Errorf("send DHCPv6 on %s: %w", t.iface.Name, err)
	}
	return nil
}

func (t *udpTransport) receive(timeout time.Duration) ([]byte, error) {
	if err := t.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("read deadline: %w", err)
	}
	buf := make([]byte, 65535)
	n, _, err := t.conn.ReadFrom(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, errTimeout
	}
	if err != nil {
		return nil, fmt.Errorf("receive DHCPv6: %w", err)
	}
	return buf[:n], nil
}

func (t *udpTransport) close() error {
	return t.conn.Close() //nolint:wrapcheck // closing
}
