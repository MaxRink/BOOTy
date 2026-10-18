//go:build linux_e2e

package lldp

import (
	"bytes"
	"net"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// openRX joins the three LLDP groups (/proc/net/dev_mcast lists them on
// the device; the lab veths pass LLDP without them, so only this test
// covers them), and readNeighbor drops our own frames that come back (a
// bridge reflects them): our source MAC or our chassis ID, while a
// foreign frame is read. Needs root.
func TestOpenRXGroupsAndOwnFrames(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("needs root")
	}
	la := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "lldptsta"}, PeerName: "lldptstb"}
	_ = netlink.LinkDel(la)
	if err := netlink.LinkAdd(la); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(la) })
	a, _ := netlink.LinkByName("lldptsta")
	b, _ := netlink.LinkByName("lldptstb")
	// udev on CI runners gives new veths another MAC after creation
	// (MACAddressPolicy); set fixed ones after it and read them back.
	time.Sleep(500 * time.Millisecond)
	for i, l := range []netlink.Link{a, b} {
		if err := netlink.LinkSetHardwareAddr(l, net.HardwareAddr{2, 0, 0, 0, 0x11, byte(i)}); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := openRX("lldptsta")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	mc, err := os.ReadFile("/proc/net/dev_mcast")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"0180c200000e", "0180c2000003", "0180c2000000"} {
		if !bytes.Contains(mc, []byte("lldptsta")) || !bytes.Contains(mc, []byte(g)) {
			t.Fatalf("group %s not joined on lldptsta:\n%s", g, mc)
		}
	}

	prev := ownMachineID
	t.Cleanup(func() { ownMachineID = prev })
	ownMachineID = func() string { return "our-machine" }
	tx, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(tx) })
	addr := &unix.SockaddrLinklayer{Protocol: htons(0x88cc), Ifindex: b.Attrs().Index, Halen: 6}
	copy(addr.Addr[:], nearestBridge)
	sendFrom := func(src net.HardwareAddr, chassis string) {
		t.Helper()
		frame, err := Frame("lldptstb", src, Info{MachineID: chassis, SystemName: chassis})
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Sendto(tx, frame, 0, addr); err != nil {
			t.Fatal(err)
		}
	}
	read := func() *Neighbor {
		buf := make([]byte, 1600)
		deadline := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if nb := readNeighbor(fd, buf, "lldptsta"); nb != nil {
				return nb
			}
		}
		return nil
	}
	foreign := net.HardwareAddr{2, 0, 0, 0, 0x99, 0x99}
	own, err := net.InterfaceByName("lldptsta")
	if err != nil || own.HardwareAddr.String() != "02:00:00:00:11:00" {
		t.Fatalf("lldptsta MAC = %v (%v), want 02:00:00:00:11:00", own, err)
	}
	sendFrom(own.HardwareAddr, "other-host") // reflected: our MAC
	sendFrom(foreign, "our-machine")         // our chassis ID
	if nb := read(); nb != nil {
		t.Fatalf("own frame read as neighbor %+v", nb)
	}
	sendFrom(foreign, "leaf01")
	if nb := read(); nb == nil || nb.ChassisID != "leaf01" {
		t.Fatalf("foreign frame = %+v, want leaf01", nb)
	}
}
