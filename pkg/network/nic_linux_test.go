//go:build linux

package network

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// snapshotNICFns captures the current function variables and returns a restore func.
func snapshotNICFns() func() {
	origLinkList := linkList
	origListInterfaces := listInterfaces
	origIfaceAddrs := ifaceAddrs
	origSleepFunc := sleepFunc
	return func() {
		linkList = origLinkList
		listInterfaces = origListInterfaces
		ifaceAddrs = origIfaceAddrs
		sleepFunc = origSleepFunc
	}
}

// fakeLink is a netlink.Dummy with configurable attrs for tests.
func fakeLink(name string, mac net.HardwareAddr) netlink.Link {
	return &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name:         name,
			HardwareAddr: mac,
		},
	}
}

var testMAC = net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}

// ---------------------------------------------------------------------------
// isExcluded
// ---------------------------------------------------------------------------

func Test_isExcluded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{"lo", true},
		{"loopback", true},
		{"docker0", true},
		{"veth123", true},
		{"vxlan0", true},
		{"br-abc", true},
		{"dummy0", true},
		{"virbr0", true},
		{"bond0", true},
		{"tun0", true},
		{"tap0", true},
		{"eth0", false},
		{"ens3", false},
		{"enp1s0", false},
		{"clab-net0", false},
		{"wlan0", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isExcluded(tc.name); got != tc.want {
				t.Errorf("isExcluded(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// detectNICsOnce
// ---------------------------------------------------------------------------

func Test_detectNICsOnce_returnsPhysicalNICs(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	linkList = func() ([]netlink.Link, error) {
		return []netlink.Link{
			fakeLink("eth0", testMAC),
			fakeLink("eth1", testMAC),
			fakeLink("lo", testMAC),    // excluded prefix
			fakeLink("veth0", testMAC), // excluded prefix
		}, nil
	}

	nics, hasTemp, err := detectNICsOnce()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasTemp {
		t.Fatal("hasTemp should be false")
	}
	if len(nics) != 2 {
		t.Fatalf("expected 2 NICs, got %d: %v", len(nics), nics)
	}
}

func Test_detectNICsOnce_detectsClabTemp(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	linkList = func() ([]netlink.Link, error) {
		return []netlink.Link{
			fakeLink("eth0", testMAC),
			fakeLink("clab-net0", testMAC), // temp name
		}, nil
	}

	nics, hasTemp, err := detectNICsOnce()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasTemp {
		t.Fatal("hasTemp should be true when clab-* present")
	}
	// clab link is excluded from nics
	if len(nics) != 1 || nics[0] != "eth0" {
		t.Fatalf("expected [eth0], got %v", nics)
	}
}

func Test_detectNICsOnce_skipsNoMAC(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	linkList = func() ([]netlink.Link, error) {
		return []netlink.Link{
			fakeLink("eth0", nil), // no MAC — should be skipped
			fakeLink("eth1", testMAC),
		}, nil
	}

	nics, _, err := detectNICsOnce()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics) != 1 || nics[0] != "eth1" {
		t.Fatalf("expected [eth1], got %v", nics)
	}
}

func Test_detectNICsOnce_propagatesLinkListError(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	linkList = func() ([]netlink.Link, error) {
		return nil, errors.New("netlink unavailable")
	}

	_, _, err := detectNICsOnce()
	if err == nil {
		t.Fatal("expected error from linkList failure")
	}
}

// ---------------------------------------------------------------------------
// GetIPMIInfo
// ---------------------------------------------------------------------------

func Test_GetIPMIInfo_returnsInterfaceInIPMISubnet(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	mac := net.HardwareAddr{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	iface := net.Interface{
		Name:         "eth0",
		HardwareAddr: mac,
		Flags:        net.FlagUp,
	}

	listInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{iface}, nil
	}
	ifaceAddrs = func(_ net.Interface) ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{
				IP:   net.ParseIP("10.0.0.5"),
				Mask: net.CIDRMask(24, 32),
			},
		}, nil
	}

	gotMAC, gotIP, err := GetIPMIInfo("10.0.0.0/24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMAC != mac.String() {
		t.Errorf("mac = %q, want %q", gotMAC, mac.String())
	}
	if gotIP != "10.0.0.5" {
		t.Errorf("ip = %q, want %q", gotIP, "10.0.0.5")
	}
}

func Test_GetIPMIInfo_skipsLoopback(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	mac := net.HardwareAddr{0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F}
	loopback := net.Interface{
		Name:         "lo",
		HardwareAddr: mac,
		Flags:        net.FlagLoopback | net.FlagUp,
	}
	eth := net.Interface{
		Name:         "eth0",
		HardwareAddr: mac,
		Flags:        net.FlagUp,
	}

	listInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{loopback, eth}, nil
	}
	ifaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		if iface.Name == "eth0" {
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("192.168.1.1"), Mask: net.CIDRMask(24, 32)},
			}, nil
		}
		return nil, nil
	}

	_, gotIP, err := GetIPMIInfo("192.168.1.0/24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIP != "192.168.1.1" {
		t.Errorf("ip = %q, want %q", gotIP, "192.168.1.1")
	}
}

func Test_GetIPMIInfo_skipsNoMAC(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	noMAC := net.Interface{Name: "eth0", Flags: net.FlagUp}
	withMAC := net.Interface{
		Name:         "eth1",
		HardwareAddr: testMAC,
		Flags:        net.FlagUp,
	}

	listInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{noMAC, withMAC}, nil
	}
	ifaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("172.16.0.1"), Mask: net.CIDRMask(16, 32)},
		}, nil
	}

	_, gotIP, err := GetIPMIInfo("172.16.0.0/16")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIP != "172.16.0.1" {
		t.Errorf("ip = %q, want %q", gotIP, "172.16.0.1")
	}
}

func Test_GetIPMIInfo_noSuitableInterface(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	listInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "lo", Flags: net.FlagLoopback},
		}, nil
	}
	ifaceAddrs = func(_ net.Interface) ([]net.Addr, error) {
		return nil, nil
	}

	_, _, err := GetIPMIInfo("")
	if err == nil {
		t.Fatal("expected error when no suitable interface found")
	}
}

func Test_GetIPMIInfo_noIPMINICWithoutSubnet(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	listInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", HardwareAddr: testMAC, Flags: net.FlagUp}}, nil
	}
	ifaceAddrs = func(_ net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("172.20.20.2"), Mask: net.CIDRMask(24, 32)}}, nil
	}

	if mac, _, err := GetIPMIInfo(""); !errors.Is(err, ErrNoIPMIInterface) {
		t.Fatalf("GetIPMIInfo() = %q, %v, want ErrNoIPMIInterface", mac, err)
	}
	if _, _, err := GetIPMIInfo("not-a-cidr"); err == nil || errors.Is(err, ErrNoIPMIInterface) {
		t.Fatalf("err = %v for an invalid IPMI subnet, want a configuration error", err)
	}
}

func Test_GetIPMIInfo_listInterfacesError(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	listInterfaces = func() ([]net.Interface, error) {
		return nil, errors.New("permission denied")
	}

	_, _, err := GetIPMIInfo("")
	if err == nil {
		t.Fatal("expected error from listInterfaces failure")
	}
}

// ---------------------------------------------------------------------------
// DetectPhysicalNICs (integration of retry loop with sleepFunc mock)
// ---------------------------------------------------------------------------

func Test_DetectPhysicalNICs_noRetryNeeded(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	sleepFunc = func(time.Duration) {} // no-op: must not block
	callCount := 0
	linkList = func() ([]netlink.Link, error) {
		callCount++
		return []netlink.Link{
			fakeLink("eth0", testMAC),
			fakeLink("eth1", testMAC),
		}, nil
	}

	nics, err := DetectPhysicalNICs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics) != 2 || nics[0] != "eth0" || nics[1] != "eth1" {
		t.Fatalf("expected [eth0 eth1], got %v", nics)
	}
	if callCount != 1 {
		t.Fatalf("expected one scan, got %d", callCount)
	}
}

func Test_DetectPhysicalNICs_waitsForLateFabricLink(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	sleepFunc = func(time.Duration) {} // no-op: must not block
	callCount := 0
	linkList = func() ([]netlink.Link, error) {
		callCount++
		if callCount <= 3 {
			return []netlink.Link{fakeLink("eth0", testMAC)}, nil
		}
		return []netlink.Link{
			fakeLink("eth0", testMAC),
			fakeLink("eth1", testMAC),
		}, nil
	}

	nics, err := DetectPhysicalNICs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics) != 2 || nics[0] != "eth0" || nics[1] != "eth1" {
		t.Fatalf("expected [eth0 eth1], got %v", nics)
	}
	if callCount != 4 {
		t.Fatalf("expected four scans, got %d", callCount)
	}
}

func Test_DetectPhysicalNICs_returnsSingleNICAfterSettleWindow(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	sleepFunc = func(time.Duration) {} // no-op: must not block
	callCount := 0
	linkList = func() ([]netlink.Link, error) {
		callCount++
		return []netlink.Link{fakeLink("eth0", testMAC)}, nil
	}

	nics, err := DetectPhysicalNICs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics) != 1 || nics[0] != "eth0" {
		t.Fatalf("expected [eth0], got %v", nics)
	}
	if callCount != physicalNICSingleNICSettleRetries+1 {
		t.Fatalf("expected %d scans, got %d", physicalNICSingleNICSettleRetries+1, callCount)
	}
}

func Test_DetectPhysicalNICs_stabilizesAfterClab(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	sleepFunc = func(time.Duration) {} // no-op

	callCount := 0
	linkList = func() ([]netlink.Link, error) {
		callCount++
		if callCount == 1 {
			// first call: clab temp name present
			return []netlink.Link{
				fakeLink("clab-net0", testMAC),
				fakeLink("eth0", testMAC),
			}, nil
		}
		// subsequent calls: stable
		return []netlink.Link{fakeLink("eth0", testMAC)}, nil
	}

	nics, err := DetectPhysicalNICs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics) != 1 || nics[0] != "eth0" {
		t.Fatalf("expected [eth0], got %v", nics)
	}
}

func Test_DetectPhysicalNICs_linkListError(t *testing.T) {
	restore := snapshotNICFns()
	defer restore()

	sleepFunc = func(time.Duration) {}
	linkList = func() ([]netlink.Link, error) {
		return nil, errors.New("no netlink")
	}

	_, err := DetectPhysicalNICs()
	if err == nil {
		t.Fatal("expected error from linkList failure")
	}
}
