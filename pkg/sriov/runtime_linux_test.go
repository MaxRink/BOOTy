//go:build linux

package sriov

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestVFStepsOrderAndSelection(t *testing.T) {
	all := vfSteps(&VFSettings{Trust: ptr(true), SpoofChk: ptr(false), VLAN: ptr(10), MaxTxRate: ptr(5), LinkState: "enable"}, "02:00:00:00:00:01")
	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, s.name)
	}
	if got := strings.Join(names, ","); got != "mac,vlan,rate,spoofchk,state,trust" {
		t.Fatalf("steps %s", got)
	}
	if len(vfSteps(&VFSettings{}, "")) != 0 {
		t.Fatal("no settings must make no calls")
	}
	if len(vfSteps(&VFSettings{QoS: ptr(1)}, "")) != 1 || len(vfSteps(&VFSettings{MinTxRate: ptr(1)}, "")) != 1 {
		t.Fatal("QoS alone and min rate alone must make one call each")
	}
	bad := vfSteps(&VFSettings{}, "zz")
	if err := bad[0].set(&netlink.Dummy{}, 0); err == nil {
		t.Fatal("a broken MAC must fail")
	}
}

func TestLinkStateValue(t *testing.T) {
	if linkStateValue("enable") != netlink.VF_LINK_STATE_ENABLE || linkStateValue("disable") != netlink.VF_LINK_STATE_DISABLE ||
		linkStateValue("auto") != netlink.VF_LINK_STATE_AUTO || deref(nil) != 0 || wrapNL(nil) != nil {
		t.Fatal("helpers")
	}
}

func TestForwardingRoute(t *testing.T) {
	cidr := func(s string) *net.IPNet {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	cases := []struct {
		r    netlink.Route
		want bool
	}{
		{netlink.Route{Type: unix.RTN_UNICAST}, true}, // default route
		{netlink.Route{Type: unix.RTN_UNICAST, Dst: cidr("10.0.0.0/8")}, true},
		{netlink.Route{Type: unix.RTN_UNICAST, Dst: cidr("2001:db8::/64")}, true},
		{netlink.Route{Type: unix.RTN_UNICAST, Dst: cidr("fe80::/64")}, false},
		{netlink.Route{Type: unix.RTN_MULTICAST, Dst: cidr("ff00::/8")}, false},
		{netlink.Route{Type: unix.RTN_LOCAL, Dst: cidr("fe80::1/128")}, false},
	}
	for _, c := range cases {
		if got := forwardingRoute(&c.r); got != c.want {
			t.Errorf("%+v = %v", c.r, got)
		}
	}
	r := &netlink.Route{MultiPath: []*netlink.NexthopInfo{{LinkIndex: 3}, {LinkIndex: 7}}}
	if !routeUses(r, 7) || routeUses(r, 4) || !routeUses(&netlink.Route{LinkIndex: 4}, 4) {
		t.Fatal("routeUses")
	}
}

func TestKernelRuntimeSetNumVFs(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "pci", "devices", "0000:3b:00.0")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	// sysfs has the file already; SetNumVFs only writes it.
	if err := os.WriteFile(filepath.Join(dev, "sriov_numvfs"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := &KernelRuntime{SysBus: dir}
	// The interface name does not matter: the device is found by address.
	if err := k.SetNumVFs(&PF{Interface: "renamed", Bus: "pci", PCIAddress: "0000:3b:00.0"}, 4); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "sriov_numvfs")); string(b) != "4" {
		t.Fatalf("sriov_numvfs = %q", b)
	}
	if err := k.SetNumVFs(&PF{Bus: "pci", PCIAddress: "0000:af:00.0"}, 1); err == nil {
		t.Fatal("missing device must fail")
	}
	if err := k.SetEswitch(&PF{}, "offload"); err == nil {
		t.Fatal("unknown mode must fail")
	}
	if k := NewKernelRuntime(); k.SysBus != "/sys/bus" || k.Sockets == nil {
		t.Fatal("defaults")
	}
}

// TestKernelRuntimeNetdev finds the PF netdev of a device after a
// re-register gave it a new name: by phys_port_name, by MAC, or as the only
// port next to representors.
func TestKernelRuntimeNetdev(t *testing.T) {
	dir := t.TempDir()
	mk := func(addr, name, port, mac string) {
		d := filepath.Join(dir, "pci", "devices", addr, "net", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, v := range map[string]string{"phys_port_name": port, "address": mac} {
			if v != "" {
				if err := os.WriteFile(filepath.Join(d, file), []byte(v+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	mk("0000:3b:00.0", "eth7", "p0", "")
	mk("0000:3b:00.0", "eth8", "pf0vf0", "")
	mk("0000:5e:00.0", "eth9", "", "aa:00:00:00:00:01")
	mk("0000:5e:00.0", "eth10", "", "aa:00:00:00:00:02")
	mk("0000:af:00.0", "eth11", "", "")
	k := &KernelRuntime{SysBus: dir}
	for _, c := range []struct {
		pf   PF
		want string
	}{
		{PF{Bus: "pci", PCIAddress: "0000:3b:00.0", PhysPortName: "p0", Interface: "ens1f0np0"}, "eth7"},
		{PF{Bus: "pci", PCIAddress: "0000:5e:00.0", MAC: "aa:00:00:00:00:02"}, "eth10"},
		{PF{Bus: "pci", PCIAddress: "0000:af:00.0"}, "eth11"},
	} {
		if got, err := k.netdev(&c.pf); err != nil || got != c.want {
			t.Errorf("netdev(%+v) = %q, %v; want %q", c.pf, got, err, c.want)
		}
	}
	for _, pf := range []PF{
		{Bus: "pci", PCIAddress: "0000:3b:00.0", PhysPortName: "p1"},
		{Bus: "pci", PCIAddress: "0000:5e:00.0", MAC: "aa:00:00:00:00:09"},
		{Bus: "pci", PCIAddress: "0000:00:00.0"},
	} {
		if got, err := k.netdev(&pf); err == nil {
			t.Errorf("netdev(%+v) = %q, want an error", pf, got)
		}
	}
	if inUse, reason := k.InUse(&PF{Bus: "pci", PCIAddress: "0000:00:00.0"}); !inUse || !strings.Contains(reason, "cannot find the netdev") {
		t.Fatalf("InUse without netdev = %v %q", inUse, reason)
	}
	if err := k.SetVF(&PF{Bus: "pci", PCIAddress: "0000:00:00.0"}, 0, &VFSettings{}, ""); err == nil {
		t.Fatal("SetVF without netdev must fail")
	}
}

func TestSocketOn(t *testing.T) {
	sock := &netlink.Socket{}
	sock.ID.Interface = 7
	sock.ID.SourcePort, sock.ID.DestinationPort = 179, 40000
	k := &KernelRuntime{Sockets: func() ([]*netlink.Socket, error) { return []*netlink.Socket{sock}, nil }}
	if used, reason := k.socketOn(7, "p0"); !used || !strings.Contains(reason, "BGP over IPv6 link-local") {
		t.Fatalf("socketOn(7) = %v %q", used, reason)
	}
	if used, _ := k.socketOn(8, "p1"); used {
		t.Fatal("socket on another interface")
	}
	if used, _ := (&KernelRuntime{}).socketOn(8, "p1"); used {
		t.Fatal("no socket reader")
	}
	// Without sock_diag the signal is missing, not a reason to refuse.
	k.Sockets = func() ([]*netlink.Socket, error) { return nil, errors.New("EPROTONOSUPPORT") }
	if used, _ := k.socketOn(7, "p0"); used {
		t.Fatal("an unreadable socket table must not count as in use")
	}
}

// TestInUseFromNetworkConfig: a port that carries a configured BGP
// session of the ramdisk is in use without any probe of the peer and
// before any kernel query; with BGP on every physical NIC every port is.
func TestInUseFromNetworkConfig(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "pci", "devices", "0000:3b:00.0", "net", "eth7")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "phys_port_name"), []byte("p0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf := &PF{Bus: "pci", PCIAddress: "0000:3b:00.0", PhysPortName: "p0", Interface: "ens1f0np0"}
	sockets := 0
	k := &KernelRuntime{SysBus: dir, Sockets: func() ([]*netlink.Socket, error) { sockets++; return nil, nil }}
	for _, c := range []struct {
		names    []string
		all      bool
		reason   string
		inConfig bool
	}{
		{[]string{"ens1f0np0"}, false, "ramdisk network config uses ens1f0np0", true}, // frr.conf "neighbor ens1f0np0 interface"
		{[]string{"eth7"}, false, "ramdisk network config uses eth7", true},           // the current name after a rename
		{nil, true, "every physical NIC", true},
	} {
		k.Uplinks = func() ([]string, bool, bool) { return c.names, c.all, true }
		inUse, reason := k.InUse(pf)
		if !inUse || !strings.Contains(reason, c.reason) {
			t.Errorf("%v all=%v: %v %q", c.names, c.all, inUse, reason)
		}
	}
	if sockets != 0 {
		t.Fatal("the config decided; no socket or peer query is needed")
	}
	// Not named and not all NICs, or not recorded yet: the config does not decide.
	k.Uplinks = func() ([]string, bool, bool) { return []string{"ens2f0"}, false, true }
	if used, _ := k.configured("eth7", "ens1f0np0"); used {
		t.Fatal("another interface is named")
	}
	k.Uplinks = func() ([]string, bool, bool) { return nil, true, false }
	if used, _ := k.configured("eth7"); used {
		t.Fatal("unknown network config must not decide")
	}
}
