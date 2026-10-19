//go:build linux && linux_e2e

package network

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/telekom/BOOTy/pkg/network/dhcp4"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// TestDHCPNetlinkLeaseAppliesTwice applies the same lease twice to a real
// dummy interface, like a network retry does. The second apply finds the
// address already there and must still report the lease as bound. A lease
// without a subnet mask must not panic. Teardown removes the address.
func TestDHCPNetlinkLeaseAppliesTwice(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dhcp-e2e0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })

	d := NewDHCPMode()
	lease := live(&dhcp4.Lease{FixedAddress: net.ParseIP("198.51.100.7")})
	for attempt := 1; attempt <= 2; attempt++ {
		leased := make(chan string, 1)
		d.onBound(link, link.Name, leased)(lease)
		select {
		case <-leased:
		default:
			t.Fatalf("attempt %d: lease was not reported as bound", attempt)
		}
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("list addresses: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IPNet.String() != "198.51.100.7/24" {
		t.Fatalf("addresses = %v, want 198.51.100.7/24", addrs)
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if addrs, err = netlink.AddrList(link, netlink.FAMILY_V4); err != nil || len(addrs) != 0 {
		t.Fatalf("addresses after Teardown = %v (err %v), want none", addrs, err)
	}
}

// TestDHCPNetlinkLeaseReplacesOtherPrefix checks that a lease with the
// same IP but a new mask does not leave the old prefix on the link.
func TestDHCPNetlinkLeaseReplacesOtherPrefix(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dhcp-e2e1"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })
	old, err := netlink.ParseAddr("198.51.100.7/16")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(link, old); err != nil {
		t.Fatalf("add old address: %v", err)
	}

	d := NewDHCPMode()
	lease := live(&dhcp4.Lease{FixedAddress: net.ParseIP("198.51.100.7"), Netmask: net.CIDRMask(24, 32)})
	leased := make(chan string, 1)
	d.onBound(link, link.Name, leased)(lease)
	select {
	case <-leased:
	default:
		t.Fatal("lease was not reported as bound")
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("list addresses: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IPNet.String() != "198.51.100.7/24" {
		t.Fatalf("addresses = %v, want only 198.51.100.7/24", addrs)
	}
}

// TestDHCPNetlinkLeasesOnTwoNICs applies a lease on each of two links, like
// networkd does with "dhcp4: true" on every NIC: both keep their address
// and both default routes stay (proto dhcp, metric 100). A renewal of the
// same lease keeps the route. Teardown removes everything.
func TestDHCPNetlinkLeasesOnTwoNICs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	d := NewDHCPMode()
	for i, net4 := range []string{"198.51.100", "203.0.113"} {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: fmt.Sprintf("dhcp-e2e%d", 2+i)}}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatalf("add dummy: %v", err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("link up: %v", err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(link) })
		lease := live(&dhcp4.Lease{FixedAddress: net.ParseIP(net4 + ".7"), Netmask: net.CIDRMask(24, 32), Router: []net.IP{net.ParseIP(net4 + ".1")}})
		for range 2 {
			leased := make(chan string, 1)
			d.onBound(link, link.Name, leased)(lease)
			if len(leased) != 1 {
				t.Fatalf("%s: lease was not reported as bound", link.Name)
			}
		}
	}

	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	var defaults []string
	for _, r := range routes {
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			defaults = append(defaults, fmt.Sprintf("%s src %s metric %d proto %d", r.Gw, r.Src, r.Priority, r.Protocol))
		}
	}
	slices.Sort(defaults)
	want := []string{"198.51.100.1 src 198.51.100.7 metric 100 proto 16", "203.0.113.1 src 203.0.113.7 metric 100 proto 16"}
	if !slices.Equal(defaults, want) {
		t.Fatalf("default routes = %v, want %v", defaults, want)
	}

	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}
	if routes, err = netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE); err != nil {
		t.Fatalf("list routes: %v", err)
	}
	for _, r := range routes {
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			t.Fatalf("default route %v left after Teardown", r)
		}
	}
}

// TestDHCPNetlinkNewAddressSameSubnet: a renewal with a new address in the
// same subnet adds it as a secondary address. With promote_secondaries=1
// (systemd's default, set by Setup) removing the old primary keeps it and
// its default route; without it the kernel removes the new address too.
func TestDHCPNetlinkNewAddressSameSubnet(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dhcp-e2e9"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	if err := promoteSecondaries(link.Name); err != nil {
		t.Fatalf("promote_secondaries: %v", err)
	}
	d := NewDHCPMode()
	for _, ip := range []string{"198.51.100.7", "198.51.100.8"} {
		d.onBound(link, link.Name, make(chan string, 1))(live(&dhcp4.Lease{
			FixedAddress: net.ParseIP(ip), Netmask: net.CIDRMask(24, 32), Router: []net.IP{net.ParseIP("198.51.100.1")},
		}))
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil || len(addrs) != 1 || addrs[0].IPNet.String() != "198.51.100.8/24" {
		t.Fatalf("addresses = %v (err %v), want only 198.51.100.8/24", addrs, err)
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	var defaults int
	for _, r := range routes {
		if (r.Dst == nil || r.Dst.String() == "0.0.0.0/0") && r.Src.Equal(net.ParseIP("198.51.100.8")) {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("routes = %v, want the default route with source 198.51.100.8", routes)
	}
}

// TestDHCPNetlinkGatewayOutsideSubnet: a /32 lease whose router is outside
// the leased subnet works like in networkd: the on-link host route to the
// gateway comes first, so the default route through it can be added.
func TestDHCPNetlinkGatewayOutsideSubnet(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dhcp-e2e8"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("add dummy: %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	leased := make(chan string, 1)
	NewDHCPMode().onBound(link, link.Name, leased)(live(&dhcp4.Lease{
		FixedAddress: net.ParseIP("198.51.100.7"), Netmask: net.CIDRMask(32, 32), Router: []net.IP{net.ParseIP("192.0.2.1")},
	}))
	if len(leased) != 1 {
		t.Fatal("the /32 lease was not applied")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	var gw, def bool
	for _, r := range routes {
		gw = gw || (r.Dst != nil && r.Dst.String() == "192.0.2.1/32" && r.Scope == netlink.SCOPE_LINK)
		def = def || ((r.Dst == nil || r.Dst.String() == "0.0.0.0/0") && r.Gw.Equal(net.ParseIP("192.0.2.1")))
	}
	if !gw || !def {
		t.Fatalf("routes = %v, want the on-link route to 192.0.2.1 and the default route through it", routes)
	}
}

// enterTestNetns runs the rest of the test in a new network namespace, so
// the host interfaces and routes are never touched.
func enterTestNetns(t *testing.T) {
	t.Helper()
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("get netns: %v", err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatalf("new netns: %v", err)
	}
	t.Cleanup(func() {
		_ = netns.Set(orig)
		_ = ns.Close()
		_ = orig.Close()
		runtime.UnlockOSThread()
	})
}
