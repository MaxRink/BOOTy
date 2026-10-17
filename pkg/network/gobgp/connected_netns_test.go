//go:build linux && linux_e2e

package gobgp

import (
	"net/netip"
	"os"
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// connectedInVRF only counts the networks on the links of the VRF: the
// same address is connected in one VRF and not in another or the default
// VRF. A host address does not count.
func TestConnectedInVRF(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	origin, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	ns, err := netns.New() // also enters it
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	defer netns.Set(origin) //nolint:errcheck // back to the test namespace
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []struct {
		name  string
		table uint32
	}{{"vrf-a", 1001}, {"vrf-b", 1002}} {
		vrf := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: v.name}, Table: v.table}
		must(netlink.LinkAdd(vrf))
		must(netlink.LinkSetUp(vrf))
	}
	for _, d := range []struct{ name, vrf, addr string }{
		{"d-a", "vrf-a", "10.50.0.1/24"},
		{"d-b", "vrf-b", "10.60.0.1/24"},
		{"d-def", "", "10.70.0.1/24"},
		{"d-host", "", "10.80.0.1/32"},
	} {
		dum := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: d.name}}
		must(netlink.LinkAdd(dum))
		if d.vrf != "" {
			vrf, err := netlink.LinkByName(d.vrf)
			must(err)
			must(netlink.LinkSetMaster(dum, vrf))
		}
		addr, err := netlink.ParseAddr(d.addr)
		must(err)
		must(netlink.AddrAdd(dum, addr))
		must(netlink.LinkSetUp(dum))
	}
	for _, tt := range []struct {
		addr, vrf string
		want      bool
	}{
		{"10.50.0.9", "vrf-a", true},
		{"10.50.0.9", "vrf-b", false},
		{"10.50.0.9", "", false},
		{"10.60.0.9", "vrf-b", true},
		{"10.60.0.9", "vrf-a", false},
		{"10.70.0.9", "", true},
		{"10.70.0.9", "vrf-a", false},
		{"10.80.0.1", "", false},
	} {
		if got := connectedInVRF(netip.MustParseAddr(tt.addr), tt.vrf); got != tt.want {
			t.Errorf("connectedInVRF(%s, %q) = %v, want %v", tt.addr, tt.vrf, got, tt.want)
		}
	}
}
