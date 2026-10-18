//go:build linux

package intel

import (
	"context"
	"fmt"
	"testing"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	"github.com/telekom/BOOTy/pkg/firmware/nic/ethtool"
	netethtool "github.com/telekom/BOOTy/pkg/network/ethtool"
)

// netlinkFlags fakes the ethtool netlink private flags.
func netlinkFlags(flags []netethtool.Flag, setErr error, sets *[]string) ethtool.PrivFlagIO {
	return ethtool.PrivFlagIO{
		Get: func(string) ([]netethtool.Flag, error) { return flags, nil },
		Set: func(iface, name string, on bool) error {
			*sets = append(*sets, fmt.Sprintf("%s %s %v", iface, name, on))
			return setErr
		},
	}
}

func TestSupported(t *testing.T) {
	m := New(nil)
	if !m.Supported(&nic.Identifier{VendorID: "0x8086"}) {
		t.Error("should support Intel NIC")
	}
	if m.Supported(&nic.Identifier{VendorID: "0x15b3"}) {
		t.Error("should not support Mellanox NIC")
	}
}

func TestVendor(t *testing.T) {
	m := New(nil)
	if m.Vendor() != nic.VendorIntel {
		t.Errorf("vendor = %q", m.Vendor())
	}
}

func TestCriticalParams(t *testing.T) {
	params := CriticalParams()
	if len(params) == 0 {
		t.Error("CriticalParams should not be empty")
	}
	found := false
	for _, p := range params {
		if p == "disable-fw-lldp" {
			found = true
		}
	}
	if !found {
		t.Error("CriticalParams should contain disable-fw-lldp")
	}
}

// MockCommander returns pre-canned outputs for testing.
type MockCommander struct {
	ethtoolOut   string
	privFlagsOut string
	err          error
	called       []string
}

func (m *MockCommander) CombinedOutput(_ context.Context, cmd string, args ...string) ([]byte, error) {
	m.called = append(m.called, cmd)
	if m.err != nil {
		return nil, m.err
	}
	if len(args) > 0 && args[0] == "--show-priv-flags" {
		return []byte(m.privFlagsOut), nil
	}
	if len(args) > 0 && args[0] == "--set-priv-flags" {
		return nil, nil
	}
	return []byte(m.ethtoolOut), nil
}

func TestCapture_Success(t *testing.T) {
	mock := &MockCommander{
		ethtoolOut:   "driver: ice\nfirmware-version: 4.40 0x8001c967 1.3534.0\n",
		privFlagsOut: "Private flags for eth0:\ndisable-fw-lldp : off\nchannel-inline-flow-director : on\n",
	}
	m := NewWithCommander(nil, mock)
	var sets []string
	m.WithPrivFlags(netlinkFlags([]netethtool.Flag{{Name: "disable-fw-lldp"}, {Name: "channel-inline-flow-director", On: true}}, nil, &sets))

	id := &nic.Identifier{
		PCIAddress: "0000:01:00.0",
		Interface:  "eth0",
		VendorID:   "0x8086",
	}

	state, err := m.Capture(context.Background(), id)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if state.FWVersion != "4.40 0x8001c967 1.3534.0" {
		t.Errorf("fw version = %q", state.FWVersion)
	}
	if p, ok := state.Parameters["disable-fw-lldp"]; !ok || p.Current != "off" {
		t.Error("disable-fw-lldp not captured")
	}
}

func TestCapture_NilNIC(t *testing.T) {
	m := New(nil)
	if _, err := m.Capture(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil NIC")
	}
}

func TestCapture_NoInterface(t *testing.T) {
	m := New(nil)
	if _, err := m.Capture(context.Background(), &nic.Identifier{VendorID: "0x8086"}); err == nil {
		t.Fatal("expected error without interface")
	}
}

func TestCapture_EthtoolError(t *testing.T) {
	mock := &MockCommander{err: fmt.Errorf("ethtool not found")}
	m := NewWithCommander(nil, mock)

	id := &nic.Identifier{PCIAddress: "0000:01:00.0", Interface: "eth0", VendorID: "0x8086"}
	if _, err := m.Capture(context.Background(), id); err == nil {
		t.Fatal("expected error")
	}
}

func TestApply_Success(t *testing.T) {
	mock := &MockCommander{}
	m := NewWithCommander(nil, mock)
	var sets []string
	m.WithPrivFlags(netlinkFlags(nil, nil, &sets))

	id := &nic.Identifier{PCIAddress: "0000:01:00.0", Interface: "eth0", VendorID: "0x8086"}
	changes := []nic.FlagChange{{Name: "disable-fw-lldp", Value: "on"}}

	if err := m.Apply(context.Background(), id, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(sets) != 1 || sets[0] != "eth0 disable-fw-lldp true" || len(mock.called) != 0 {
		t.Fatalf("netlink sets %v, ethtool calls %v", sets, mock.called)
	}
}

func TestApply_Error(t *testing.T) {
	mock := &MockCommander{}
	m := NewWithCommander(nil, mock)
	var sets []string
	m.WithPrivFlags(netlinkFlags(nil, fmt.Errorf("permission denied"), &sets))

	id := &nic.Identifier{PCIAddress: "0000:01:00.0", Interface: "eth0", VendorID: "0x8086"}
	changes := []nic.FlagChange{{Name: "x", Value: "on"}}

	if err := m.Apply(context.Background(), id, changes); err == nil {
		t.Fatal("expected error")
	}
}

func TestApply_NilNIC(t *testing.T) {
	m := New(nil)
	if err := m.Apply(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error for nil NIC")
	}
}

func TestApply_NoInterface(t *testing.T) {
	m := New(nil)
	if err := m.Apply(context.Background(), &nic.Identifier{VendorID: "0x8086"}, nil); err == nil {
		t.Fatal("expected error without interface")
	}
}

// DisableFWLLDP goes through nic.DisableFirmwareLLDP (tested there with
// the i40e and ice flags): a NIC without an Intel driver is left alone,
// and no ethtool command runs.
func TestDisableFWLLDP(t *testing.T) {
	mock := &MockCommander{}
	m := NewWithCommander(nil, mock)

	if err := m.DisableFWLLDP(context.Background(), ""); err == nil {
		t.Fatal("DisableFWLLDP without an interface succeeded")
	}
	if err := m.DisableFWLLDP(context.Background(), "booty-no-such-nic"); err != nil || len(mock.called) != 0 {
		t.Fatalf("DisableFWLLDP: %v, ethtool calls %v", err, mock.called)
	}
}
