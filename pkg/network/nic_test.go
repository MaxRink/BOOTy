//go:build linux

package network

import (
	"errors"
	"net"
	"testing"
)

func mockAddr(t *testing.T, ip string) []net.Addr {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(ip)
	if err != nil {
		t.Fatalf("mockAddr: invalid CIDR %q: %v", ip, err)
	}
	return []net.Addr{ipNet}
}

func makeIface(t *testing.T, name string, mac string, flags net.Flags) net.Interface {
	t.Helper()
	hw, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("makeIface: invalid MAC %q: %v", mac, err)
	}
	return net.Interface{
		Name:         name,
		HardwareAddr: hw,
		Flags:        flags,
	}
}

func addrFor(addrs map[string][]net.Addr) func(net.Interface) ([]net.Addr, error) {
	return func(i net.Interface) ([]net.Addr, error) {
		return addrs[i.Name], nil
	}
}

func addrForWithErr(addrs map[string][]net.Addr, errIface string, errVal error) func(net.Interface) ([]net.Addr, error) {
	return func(i net.Interface) ([]net.Addr, error) {
		if i.Name == errIface {
			return nil, errVal
		}
		return addrs[i.Name], nil
	}
}

func TestSelectIPMIInterface_BMCNameWins(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
		makeIface(t, "ipmi0", "aa:bb:cc:dd:ee:02", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0":  mockAddr(t, "192.168.1.1/24"),
		"ipmi0": mockAddr(t, "192.168.2.1/24"),
	}

	got, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "ipmi0" {
		t.Errorf("got interface %q, want %q", got.Name, "ipmi0")
	}
}

func TestSelectIPMIInterface_BMCBeatsEth(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
		makeIface(t, "bmc0", "aa:bb:cc:dd:ee:02", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mockAddr(t, "10.0.0.1/24"),
		"bmc0": mockAddr(t, "10.0.0.2/24"),
	}

	got, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "bmc0" {
		t.Errorf("got interface %q, want %q", got.Name, "bmc0")
	}
}

// A NIC without a BMC name is not an IPMI NIC. Before, the first addressed
// NIC was used, so a ContainerLab eth0 gave the EVPN router MAC.
func TestSelectIPMIInterface_NoFallbackToDataNIC(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mockAddr(t, "10.0.0.1/24"),
	}

	if _, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil); !errors.Is(err, ErrNoIPMIInterface) {
		t.Fatalf("err = %v, want ErrNoIPMIInterface", err)
	}
	_, other, _ := net.ParseCIDR("172.30.0.0/24")
	if _, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), other); !errors.Is(err, ErrNoIPMIInterface) {
		t.Fatalf("err = %v for a NIC outside the IPMI subnet, want ErrNoIPMIInterface", err)
	}
}

func TestSelectIPMIInterface_IPMISubnetMatch(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
		makeIface(t, "eth1", "aa:bb:cc:dd:ee:02", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mockAddr(t, "10.0.0.1/24"),
		"eth1": mockAddr(t, "172.30.0.5/24"),
	}
	_, ipmiNet, _ := net.ParseCIDR("172.30.0.0/24")

	got, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), ipmiNet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "eth1" {
		t.Errorf("got interface %q, want %q", got.Name, "eth1")
	}
}

func TestSelectIPMIInterface_NoAddressesReturnsError(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{}

	_, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSelectIPMIInterface_MgmtBeatsEth(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
		makeIface(t, "mgmt0", "aa:bb:cc:dd:ee:02", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0":  mockAddr(t, "10.0.0.1/24"),
		"mgmt0": mockAddr(t, "10.0.0.2/24"),
	}

	got, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "mgmt0" {
		t.Errorf("got interface %q, want %q", got.Name, "mgmt0")
	}
}

func TestSelectIPMIInterface_CaseInsensitive(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "IPMI0", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"IPMI0": mockAddr(t, "192.168.0.1/24"),
	}

	got, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "IPMI0" {
		t.Errorf("got interface %q, want %q", got.Name, "IPMI0")
	}
}

func TestSelectIPMIInterface_LoopbackExcluded(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "lo", "00:00:00:00:00:00", net.FlagLoopback),
		makeIface(t, "ipmi0", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"lo":    mockAddr(t, "127.0.0.1/8"),
		"ipmi0": mockAddr(t, "192.168.1.1/24"),
	}

	got, err := selectIPMIInterfaceWith(ifaces, addrFor(addrs), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "ipmi0" {
		t.Errorf("got interface %q, want %q", got.Name, "ipmi0")
	}
}

func TestFilterAddressed_NoMACInterface(t *testing.T) {
	ifaces := []net.Interface{
		{Name: "eth0", HardwareAddr: nil, Flags: net.FlagUp},
		makeIface(t, "eth1", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": mockAddr(t, "10.0.0.1/24"),
		"eth1": mockAddr(t, "10.0.0.2/24"),
	}

	got := filterAddressed(ifaces, addrFor(addrs))
	if len(got) != 1 || got[0].Name != "eth1" {
		t.Errorf("got %v, want only eth1", got)
	}
}

func TestFilterAddressed_ValidIPInLaterEntry(t *testing.T) {
	_, linkLocal, _ := net.ParseCIDR("169.254.0.1/16")
	_, validIP, _ := net.ParseCIDR("10.0.0.1/24")
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": {linkLocal, validIP},
	}

	got := filterAddressed(ifaces, addrFor(addrs))
	if len(got) != 1 || got[0].Name != "eth0" {
		t.Errorf("got %v, want eth0 (valid IP in later addr entry)", got)
	}
}

func TestFilterAddressed_NoValidIPExcluded(t *testing.T) {
	_, linkLocal, _ := net.ParseCIDR("169.254.1.1/16")
	_, loopback, _ := net.ParseCIDR("127.0.0.1/8")
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth0": {linkLocal, loopback},
	}

	got := filterAddressed(ifaces, addrFor(addrs))
	if len(got) != 0 {
		t.Errorf("got %v, want empty (all addrs are link-local or loopback)", got)
	}
}

func TestFilterAddressed_AddrErrorSkipsInterface(t *testing.T) {
	ifaces := []net.Interface{
		makeIface(t, "eth0", "aa:bb:cc:dd:ee:01", net.FlagUp),
		makeIface(t, "eth1", "aa:bb:cc:dd:ee:02", net.FlagUp),
	}
	addrs := map[string][]net.Addr{
		"eth1": mockAddr(t, "10.0.0.2/24"),
	}

	got := filterAddressed(ifaces, addrForWithErr(addrs, "eth0", errors.New("addr enumeration failed")))
	if len(got) != 1 || got[0].Name != "eth1" {
		t.Errorf("got %v, want only eth1 (eth0 skipped due to addr error)", got)
	}
}
