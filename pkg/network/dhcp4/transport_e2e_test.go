//go:build linux && linux_e2e

package dhcp4

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// TestTransportOnVeth runs the client with its real sockets against a small
// server on the other end of a veth pair in a new network namespace: DORA
// over the packet socket (BPF filter, broadcasts from 0.0.0.0), then a
// RENEWING REQUEST through the unicast UDP socket.
func TestTransportOnVeth(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	// The client and server sockets are made on many threads, so the whole
	// test process runs in a new network namespace.
	if os.Getenv("DHCP4_TEST_NETNS") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTransportOnVeth$", "-test.v")
		cmd.Env = append(os.Environ(), "DHCP4_TEST_NETNS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("in a new network namespace: %v\n%s", err, out)
		}
		return
	}

	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "dhcpc0"}, PeerName: "dhcps0"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	cli, _ := netlink.LinkByName("dhcpc0")
	if err := netlink.LinkSetUp(cli); err != nil {
		t.Fatal(err)
	}
	// The server end lives in its own namespace: with both ends in one,
	// the kernel takes the unicast through lo and drops the broadcast from
	// the lease address as a local source.
	requests := make(chan *message, 8)
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // this thread stays in the server namespace
		orig, _ := netns.Get()
		srvNS, err := netns.New()
		if err != nil {
			ready <- err
			return
		}
		_ = netns.Set(orig)
		srv, _ := netlink.LinkByName("dhcps0")
		if err := netlink.LinkSetNsFd(srv, int(srvNS)); err != nil {
			ready <- err
			return
		}
		_ = netns.Set(srvNS)
		srv, _ = netlink.LinkByName("dhcps0")
		srvAddr, _ := netlink.ParseAddr("192.0.2.1/24")
		if err := netlink.AddrAdd(srv, srvAddr); err != nil {
			ready <- err
			return
		}
		if err := netlink.LinkSetUp(srv); err != nil {
			ready <- err
			return
		}
		ready <- nil
		serveDHCP(t, "dhcps0", requests)
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}

	ifi, _ := net.InterfaceByName("dhcpc0")
	bound := make(chan *Lease, 4)
	c := &Client{Iface: ifi, Hostname: "veth-test", Params: []layers.DHCPOpt{1, 3}, OnBound: func(l *Lease) {
		// Like BOOTy: the address is on the NIC before the renewal.
		_ = netlink.AddrReplace(cli, &netlink.Addr{IPNet: &net.IPNet{IP: l.FixedAddress, Mask: l.Netmask}})
		bound <- l
	}}
	c.Start()
	defer c.Stop()
	select {
	case l := <-bound:
		if !l.FixedAddress.Equal(net.IPv4(192, 0, 2, 10)) {
			t.Fatalf("lease = %+v", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no lease over the veth")
	}
	// The server sets T1 to 2 s: the renewal comes as unicast from the lease
	// address.
	select {
	case <-bound:
	case <-time.After(30 * time.Second):
		t.Fatal("no renewal")
	}
	var renewal *message
	for len(requests) > 0 {
		if m := <-requests; m.typ == layers.DHCPMsgTypeRequest && m.ciaddr != nil {
			renewal = m
		}
	}
	if renewal == nil {
		t.Fatal("the server saw no RENEWING REQUEST with ciaddr")
	}
}

// serveDHCP answers DISCOVER with OFFER and REQUEST with ACK (lease 10 s,
// T1 2 s), broadcast before the client has an address, unicast after.
func serveDHCP(t *testing.T, ifname string, seen chan<- *message) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		t.Error(err)
		return
	}
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	_ = unix.BindToDevice(fd, ifname)
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: 67}); err != nil {
		t.Error(err)
		return
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return
		}
		var d layers.DHCPv4
		if d.DecodeFromBytes(buf[:n], nil) != nil {
			continue
		}
		m := &message{xid: d.Xid, mac: d.ClientHWAddr}
		if !d.ClientIP.IsUnspecified() {
			m.ciaddr = d.ClientIP
		}
		for _, o := range d.Options {
			if o.Type == layers.DHCPOptMessageType {
				m.typ = layers.DHCPMsgType(o.Data[0])
			}
		}
		seen <- m
		typ := layers.DHCPMsgTypeOffer
		if m.typ == layers.DHCPMsgTypeRequest {
			typ = layers.DHCPMsgTypeAck
		}
		reply := testReply(m, typ)
		to := &unix.SockaddrInet4{Port: 68, Addr: [4]byte{255, 255, 255, 255}}
		if m.ciaddr != nil {
			copy(to.Addr[:], m.ciaddr.To4())
		}
		_ = unix.Sendto(fd, reply, 0, to)
	}
}

func testReply(m *message, typ layers.DHCPMsgType) []byte {
	d := &layers.DHCPv4{
		Operation: layers.DHCPOpReply, HardwareType: layers.LinkTypeEthernet, HardwareLen: 6,
		Xid: m.xid, ClientHWAddr: m.mac, YourClientIP: net.IPv4(192, 0, 2, 10).To4(), ClientIP: net.IPv4zero,
		NextServerIP: net.IPv4zero, RelayAgentIP: net.IPv4zero,
		Options: []layers.DHCPOption{
			layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(typ)}),
			layers.NewDHCPOption(layers.DHCPOptServerID, net.IPv4(192, 0, 2, 1).To4()),
			layers.NewDHCPOption(layers.DHCPOptLeaseTime, []byte{0, 0, 0, 10}),
			layers.NewDHCPOption(layers.DHCPOptT1, []byte{0, 0, 0, 2}),
			layers.NewDHCPOption(layers.DHCPOptSubnetMask, []byte{255, 255, 255, 0}),
		},
	}
	buf := gopacket.NewSerializeBuffer()
	_ = d.SerializeTo(buf, gopacket.SerializeOptions{})
	return buf.Bytes()
}
