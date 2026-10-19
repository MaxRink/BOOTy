//go:build linux && linux_e2e

package dhcp6

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/ipv6"
)

// TestTransportOnVeth runs SOLICIT with rapid commit over a veth with the
// real socket against a server in another network namespace.
func TestTransportOnVeth(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	if os.Getenv("DHCP6_TEST_NETNS") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTransportOnVeth$", "-test.v")
		cmd.Env = append(os.Environ(), "DHCP6_TEST_NETNS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("in a new network namespace: %v\n%s", err, out)
		}
		return
	}
	for _, k := range []string{"/proc/sys/net/ipv6/conf/all/accept_dad", "/proc/sys/net/ipv6/conf/default/accept_dad"} {
		_ = os.WriteFile(k, []byte("0"), 0o644) //nolint:gosec // test sysctl
	}
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "dhcp6c0"}, PeerName: "dhcp6s0"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	cli, _ := netlink.LinkByName("dhcp6c0")
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, _ := netns.Get()
		srvNS, err := netns.New()
		if err != nil {
			ready <- err
			return
		}
		_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/accept_dad", []byte("0"), 0o644) //nolint:gosec // test sysctl
		_ = netns.Set(orig)
		srv, _ := netlink.LinkByName("dhcp6s0")
		if err := netlink.LinkSetNsFd(srv, int(srvNS)); err != nil {
			ready <- err
			return
		}
		_ = netns.Set(srvNS)
		srv, _ = netlink.LinkByName("dhcp6s0")
		if err := netlink.LinkSetUp(srv); err != nil {
			ready <- err
			return
		}
		conn, err := net.ListenPacket("udp6", "[::]:547")
		if err != nil {
			ready <- err
			return
		}
		ifi, _ := net.InterfaceByName("dhcp6s0")
		if err := ipv6.NewPacketConn(conn).JoinGroup(ifi, &net.UDPAddr{IP: allServers}); err != nil {
			ready <- err
			return
		}
		ready <- nil
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if buf[0] == msgSolicit {
				_, _ = conn.WriteTo(serverReply(buf[:n], msgReply, true), from)
			}
		}
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(cli); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // link-local address
	ifi, _ := net.InterfaceByName("dhcp6c0")
	bound := make(chan *Lease, 1)
	c := &Client{Iface: ifi, DUID: testDUID, IAID: 7, Stateful: true, Hostname: "veth-test", OnBound: func(l *Lease) { bound <- l }}
	c.Start()
	defer c.Stop()
	select {
	case l := <-bound:
		if !l.Addr.Equal(testAddr) {
			t.Fatalf("lease = %+v", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no DHCPv6 lease over the veth")
	}
}
