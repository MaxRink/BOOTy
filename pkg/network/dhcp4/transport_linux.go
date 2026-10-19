//go:build linux

package dhcp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mdlayher/packet"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// rawTransport sends broadcasts and receives replies on a packet socket of
// the NIC, which works before the NIC has an address; unicasts go through
// a UDP socket bound to the lease address and the NIC.
type rawTransport struct {
	iface *net.Interface
	conn  *packet.Conn

	// mu guards udp and closed: Stop and Reboot close the transport from
	// another goroutine while the client sends.
	mu     sync.Mutex
	closed bool
	// absorb is a socket on 0.0.0.0:68 of the NIC for the whole exchange:
	// a server answers an INIT-REBOOT or a DISCOVER with option 50 unicast
	// to the address the NIC may still have, and without a socket the
	// kernel answers with ICMP port unreachable. Nothing reads it.
	absorb int
	// udp is the socket on lease-address:68 of the exchange. It stays open
	// until the exchange ends, so the server's unicast reply (to a RENEW,
	// or to a REBIND with ciaddr) finds a socket; without one the kernel
	// answers with ICMP port unreachable. networkd keeps the same socket.
	udp int
}

// dhcpReplies passes IPv4 UDP datagrams to port 68 that are not fragments.
var dhcpReplies = []bpf.Instruction{
	bpf.LoadAbsolute{Off: 23, Size: 1},
	bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: unix.IPPROTO_UDP, SkipTrue: 6},
	bpf.LoadAbsolute{Off: 20, Size: 2},
	bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 0x1fff, SkipTrue: 4},
	bpf.LoadMemShift{Off: 14},
	bpf.LoadIndirect{Off: 16, Size: 2},
	bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 68, SkipTrue: 1},
	bpf.RetConstant{Val: 0xffff},
	bpf.RetConstant{Val: 0},
}

func openTransport(iface *net.Interface) (transport, error) {
	filter, err := bpf.Assemble(dhcpReplies)
	if err != nil {
		return nil, fmt.Errorf("assemble DHCP filter: %w", err)
	}
	conn, err := packet.Listen(iface, packet.Raw, unix.ETH_P_IP, &packet.Config{Filter: filter})
	if err != nil {
		return nil, fmt.Errorf("packet socket on %s: %w", iface.Name, err)
	}
	// PACKET_AUXDATA tells a frame whose checksum the NIC or the kernel did
	// not fill in yet (offload), like networkd reads it.
	rc, err := conn.SyscallConn()
	if err == nil {
		var serr error
		err = rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_PACKET, unix.PACKET_AUXDATA, 1) })
		err = errors.Join(err, serr)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("PACKET_AUXDATA on %s: %w", iface.Name, err)
	}
	t := &rawTransport{iface: iface, conn: conn, udp: -1, absorb: -1}
	if fd, err := t.openUDP(net.IPv4zero); err == nil {
		t.absorb = fd
	} else if t.udp >= 0 {
		_ = unix.Close(t.udp)
	}
	t.udp = -1
	return t, nil
}

// broadcast sends with the IP TOS of networkd (CS6). A message with ciaddr
// (REBINDING) also opens the socket on ciaddr:68 for the unicast reply.
func (t *rawTransport) broadcast(src net.IP, m *message) error {
	payload := m.encode()
	if ip := m.ciaddr.To4(); ip != nil && !ip.IsUnspecified() {
		t.mu.Lock()
		_, _ = t.openUDP(ip) // without the address on the NIC the reply finds no socket either way
		t.mu.Unlock()
	}
	frame := udpFrame(t.iface.HardwareAddr, ipOrZero(src), net.IPv4bcast, 68, 67, 0xc0, payload)
	if _, err := t.conn.WriteTo(frame, &packet.Addr{HardwareAddr: net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}); err != nil {
		return fmt.Errorf("send DHCP %s on %s: %w", m.typ, t.iface.Name, err)
	}
	return nil
}

// openUDP replaces the socket of the exchange with one on src:68 of the
// NIC. The caller holds t.mu.
func (t *rawTransport) openUDP(src net.IP) (int, error) {
	if t.closed {
		return -1, net.ErrClosed
	}
	if t.udp >= 0 {
		_ = unix.Close(t.udp)
		t.udp = -1
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return -1, fmt.Errorf("UDP socket: %w", err)
	}
	t.udp = fd
	for _, o := range []struct{ level, opt, val int }{
		{unix.SOL_SOCKET, unix.SO_REUSEADDR, 1}, {unix.SOL_SOCKET, unix.SO_REUSEPORT, 1}, {unix.IPPROTO_IP, unix.IP_TOS, 0xc0},
	} {
		if err := unix.SetsockoptInt(fd, o.level, o.opt, o.val); err != nil {
			return -1, fmt.Errorf("setsockopt: %w", err)
		}
	}
	if err := unix.BindToDevice(fd, t.iface.Name); err != nil {
		return -1, fmt.Errorf("bind to %s: %w", t.iface.Name, err)
	}
	var from unix.SockaddrInet4
	from.Port = 68
	copy(from.Addr[:], src.To4())
	if err := unix.Bind(fd, &from); err != nil {
		return -1, fmt.Errorf("bind %s:68: %w", src, err)
	}
	return fd, nil
}

func (t *rawTransport) unicast(src, dst net.IP, m *message) error {
	payload := m.encode()
	t.mu.Lock()
	defer t.mu.Unlock()
	fd, err := t.openUDP(src)
	if err != nil {
		return err
	}
	var to unix.SockaddrInet4
	to.Port = 67
	copy(to.Addr[:], dst.To4())
	if err := unix.Sendto(fd, payload, 0, &to); err != nil {
		return fmt.Errorf("send DHCP %s to %s: %w", m.typ, dst, err)
	}
	return nil
}

func (t *rawTransport) receive(timeout time.Duration) ([]byte, error) {
	if err := t.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("read deadline: %w", err)
	}
	rc, err := t.conn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("receive DHCP: %w", err)
	}
	buf := make([]byte, 65535) // jumbo frames: a reply can be larger than 1500
	oob := make([]byte, unix.CmsgSpace(tpacketAuxdataLen))
	for {
		var n, oobn int
		var rerr error
		err := rc.Read(func(fd uintptr) bool {
			n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf, oob, 0)
			return !errors.Is(rerr, unix.EAGAIN)
		})
		if err == nil {
			err = rerr
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, errTimeout
			}
			return nil, fmt.Errorf("receive DHCP: %w", err)
		}
		if payload, ok := udpPayload(buf[:n], !checksumNotReady(oob[:oobn])); ok {
			return payload, nil
		}
	}
}

// tpacketAuxdataLen is the size of struct tpacket_auxdata.
const tpacketAuxdataLen = 20

// checksumNotReady tells a frame whose checksum is not filled in yet
// (TP_STATUS_CSUMNOTREADY in the PACKET_AUXDATA).
func checksumNotReady(oob []byte) bool {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.Header.Level == unix.SOL_PACKET && m.Header.Type == unix.PACKET_AUXDATA && len(m.Data) >= 4 {
			return binary.NativeEndian.Uint32(m.Data)&unix.TP_STATUS_CSUMNOTREADY != 0
		}
	}
	return false
}

func (t *rawTransport) close() error {
	t.mu.Lock()
	t.closed = true
	for _, fd := range []*int{&t.udp, &t.absorb} {
		if *fd >= 0 {
			_ = unix.Close(*fd)
			*fd = -1
		}
	}
	t.mu.Unlock()
	return t.conn.Close() //nolint:wrapcheck // closing
}
