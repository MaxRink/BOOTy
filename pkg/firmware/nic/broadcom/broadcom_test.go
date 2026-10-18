//go:build linux

package broadcom

import (
	"context"
	"testing"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	"github.com/telekom/BOOTy/pkg/firmware/nic/ethtool"
	netethtool "github.com/telekom/BOOTy/pkg/network/ethtool"
)

// netlinkFlags fakes the ethtool netlink private flags; sets collects the
// flags set.
func netlinkFlags(flags []netethtool.Flag, sets *[]string) ethtool.PrivFlagIO {
	return ethtool.PrivFlagIO{
		Get: func(string) ([]netethtool.Flag, error) { return flags, nil },
		Set: func(iface, name string, on bool) error {
			*sets = append(*sets, iface+" "+name)
			return nil
		},
	}
}

func TestSupported(t *testing.T) {
	m := New(nil)
	if !m.Supported(&nic.Identifier{VendorID: "0x14e4"}) {
		t.Error("should support Broadcom NIC")
	}
	if m.Supported(&nic.Identifier{VendorID: "0x8086"}) {
		t.Error("should not support Intel NIC")
	}
}

func TestVendor(t *testing.T) {
	m := New(nil)
	if m.Vendor() != nic.VendorBroadcom {
		t.Errorf("vendor = %q", m.Vendor())
	}
}

func TestDriverType(t *testing.T) {
	tests := []struct {
		driver   string
		expected string
	}{
		{"tg3", "tg3"},
		{"bnxt_en", "bnxt_en"},
		{"other", "unknown"},
		{"", "unknown"},
		{" ", "unknown"},
	}

	for _, tc := range tests {
		got := DriverType(&nic.Identifier{Driver: tc.driver})
		if got != tc.expected {
			t.Errorf("DriverType(%q) = %q, want %q", tc.driver, got, tc.expected)
		}
	}
}

func TestDriverType_Nil(t *testing.T) {
	got := DriverType(nil)
	if got != "unknown" {
		t.Errorf("DriverType(nil) = %q, want unknown", got)
	}
}

// MockCommander returns pre-canned outputs for testing.
type MockCommander struct {
	ethtoolOut   string
	privFlagsOut string
	err          error
}

func (m *MockCommander) CombinedOutput(_ context.Context, cmd string, args ...string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	if len(args) > 0 && args[0] == "--show-priv-flags" {
		return []byte(m.privFlagsOut), nil
	}
	return []byte(m.ethtoolOut), nil
}

func TestCapture_Success(t *testing.T) {
	ethtoolOutput := `driver: bnxt_en
version: 204.0.19.0/800c
firmware-version: bnxt_en_driver_216.0.57.0/bnxt_phy_driver_216_53_57
`
	privFlagsOutput := `Private flags for eth0:
disable-rrc         : off
gre-tunnel-lro      : on
hds                 : off
`

	mock := &MockCommander{
		ethtoolOut:   ethtoolOutput,
		privFlagsOut: privFlagsOutput,
	}
	m := NewWithCommander(nil, mock)
	var sets []string
	m.WithPrivFlags(netlinkFlags([]netethtool.Flag{{Name: "disable-rrc"}, {Name: "gre-tunnel-lro", On: true}, {Name: "hds"}}, &sets))

	nicID := &nic.Identifier{
		PCIAddress: "0000:01:00.0",
		Interface:  "eth0",
		VendorID:   "0x14e4",
		DeviceID:   "0x1604",
		Driver:     "bnxt_en",
	}

	state, err := m.Capture(context.Background(), nicID)
	if err != nil {
		t.Fatalf("Capture failed: %v", err)
	}
	if state == nil {
		t.Fatal("state is nil")
		return
	}

	// Check that firmware version was captured
	if state.FWVersion != "bnxt_en_driver_216.0.57.0/bnxt_phy_driver_216_53_57" {
		t.Errorf("firmware version = %q", state.FWVersion)
	}

	// Check that both read-only and modifiable parameters were captured
	if param, ok := state.Parameters["driver"]; !ok || param.Current != "bnxt_en" {
		t.Errorf("driver parameter missing or wrong")
	}
	if param, ok := state.Parameters["disable-rrc"]; !ok || param.Current != "off" || param.ReadOnly {
		t.Errorf("disable-rrc flag not captured or marked as read-only")
	}
	if param, ok := state.Parameters["gre-tunnel-lro"]; !ok || param.Current != "on" || param.ReadOnly {
		t.Errorf("gre-tunnel-lro flag not captured or marked as read-only")
	}

	// Verify NIC identity is preserved
	if state.NIC.PCIAddress != "0000:01:00.0" {
		t.Errorf("NIC identity lost")
	}
}

func TestCapture_NoInterface(t *testing.T) {
	m := New(nil)
	nicID := &nic.Identifier{
		PCIAddress: "0000:01:00.0",
		VendorID:   "0x14e4",
	}

	_, err := m.Capture(context.Background(), nicID)
	if err == nil {
		t.Fatal("Capture should fail without interface name")
	}
}

func TestApply_Success(t *testing.T) {
	var appliedArgs []string
	mock := &MockCommander{
		ethtoolOut:   "",
		privFlagsOut: "Private flags for eth0:\ndisable-rrc: off\n",
	}
	// Override CombinedOutput to track set-priv-flags calls.
	origMock := mock
	trackMock := &trackingCommander{delegate: origMock}

	m := NewWithCommander(nil, trackMock)
	var sets []string
	m.WithPrivFlags(netlinkFlags(nil, &sets))

	nicID := &nic.Identifier{
		PCIAddress: "0000:01:00.0",
		Interface:  "eth0",
		VendorID:   "0x14e4",
	}

	changes := []nic.FlagChange{
		{Name: "disable-rrc", Value: "on"},
	}

	err := m.Apply(context.Background(), nicID, changes)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	appliedArgs = trackMock.args
	if len(sets) != 1 || sets[0] != "eth0 disable-rrc" || len(appliedArgs) != 0 {
		t.Fatalf("netlink sets %v, ethtool args %v; want the flag set over netlink only", sets, appliedArgs)
	}
}

// trackingCommander records args from CombinedOutput calls.
type trackingCommander struct {
	delegate ethtool.Commander
	args     []string
}

func (t *trackingCommander) CombinedOutput(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	t.args = append(t.args, args...)
	return t.delegate.CombinedOutput(ctx, cmd, args...)
}
