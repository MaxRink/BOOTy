//go:build linux && linux_e2e

// Kernel tests of the ethtool netlink requests. They need CAP_NET_ADMIN and
// run in the privileged Linux E2E job.
package ethtool

import (
	"testing"

	"github.com/vishvananda/netlink"
)

// On a veth pair the kernel reports 10000 Mb/s full duplex and the link
// state.
func TestLinkKernel(t *testing.T) {
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "bootyeth0"}, PeerName: "bootyeth1"}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(veth) })
	for _, name := range []string{"bootyeth0", "bootyeth1"} {
		l, _ := netlink.LinkByName(name)
		_ = netlink.LinkSetUp(l)
	}
	m, err := GetLinkModes("bootyeth0")
	if err != nil || m.SpeedMbps != 10000 || m.Duplex != "full" {
		t.Fatalf("veth link modes = %+v, %v", m, err)
	}
	if _, err := GetLinkState("bootyeth0"); err != nil {
		t.Fatalf("veth link state: %v", err)
	}
	if _, err := GetFEC("bootyeth0"); !IsNotSupported(err) {
		t.Fatalf("veth FEC = %v, want EOPNOTSUPP", err)
	}
}

// The kernel accepts the request: lo has no module, so a well formed
// request ends with EOPNOTSUPP, a malformed one with EINVAL. The kernel
// allows module reads only with CAP_NET_ADMIN.
func TestModuleEEPROMKernel(t *testing.T) {
	if _, err := ReadModuleEEPROM("lo", 0x50, 0, 0, 128); !IsNotSupported(err) {
		t.Fatalf("ReadModuleEEPROM(lo) = %v, want EOPNOTSUPP", err)
	}
}
