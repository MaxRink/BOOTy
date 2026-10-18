//go:build linux

package linkcheck

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/telekom/BOOTy/pkg/network/ethtool"
)

// linkState combines the ethtool replies, the netdev counters and the
// carrier losses from sysfs; a driver without FEC has no FEC part.
func TestLinkStateAndPCIe(t *testing.T) {
	dir := t.TempDir()
	origSys, origModes, origState, origFEC, origCounters := sysClassNet, getLinkModes, getLinkState, getFEC, getCounters
	t.Cleanup(func() {
		sysClassNet, getLinkModes, getLinkState, getFEC, getCounters = origSys, origModes, origState, origFEC, origCounters
	})
	sysClassNet = dir
	dev := filepath.Join(dir, "ens1f0np0", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"ens1f0np0/carrier_down_count": "4\n", "ens1f0np0/device/current_link_speed": "8.0 GT/s PCIe\n",
		"ens1f0np0/device/max_link_speed": "16.0 GT/s PCIe\n", "ens1f0np0/device/current_link_width": "8\n",
		"ens1f0np0/device/max_link_width": "16\n",
		// The parent of the device directory is the slot (upstream port).
		"ens1f0np0/max_link_speed": "8.0 GT/s PCIe\n", "ens1f0np0/max_link_width": "8\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	getLinkModes = func(string) (ethtool.LinkModes, error) {
		return ethtool.LinkModes{SpeedMbps: 25000, Duplex: "full", Autoneg: true}, nil
	}
	getLinkState = func(string) (ethtool.LinkState, error) { return ethtool.LinkState{Up: true}, nil }
	getFEC = func(string) (ethtool.FEC, error) { return ethtool.FEC{}, errors.New("EOPNOTSUPP") }
	getCounters = func(string) *Counters { return &Counters{RxErrors: 1} }

	got := linkState("ens1f0np0")
	want := &LinkState{SpeedMbps: 25000, Duplex: "full", Autoneg: true, Flaps: 4, Errors: &Counters{RxErrors: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("link state = %+v, want %+v", got, want)
	}
	if p := pcie("ens1f0np0"); !reflect.DeepEqual(p, &PCIe{Speed: "8.0 GT/s PCIe", Width: 8, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16,
		SlotMaxSpeed: "8.0 GT/s PCIe", SlotMaxWidth: 8}) {
		t.Fatalf("pcie = %+v", p)
	}
	if p := pcie("veth0"); p != nil {
		t.Fatalf("pcie of a virtual NIC = %+v", p)
	}
	getFEC = func(string) (ethtool.FEC, error) { return ethtool.FEC{Active: "rs", Corrected: 7, Stats: true}, nil }
	if f := linkState("ens1f0np0").FEC; f == nil || *f != (FEC{Active: "rs", Corrected: 7, Stats: true}) {
		t.Fatalf("FEC = %+v", f)
	}
}

// A NIC without a module has none; a module is decoded through the
// ethtool reader.
func TestModule(t *testing.T) {
	orig := readModule
	t.Cleanup(func() { readModule = orig })
	readModule = func(string, int, int, int, int) ([]byte, error) { return nil, errors.New("EOPNOTSUPP") }
	if m := module("eth0"); m != nil {
		t.Fatalf("module without a cage = %+v", m)
	}
	readModule = func(_ string, addr, page, offset, length int) ([]byte, error) {
		b := make([]byte, length)
		if addr == 0x50 && page == 0 && offset == 0 {
			b[0], b[2] = 0x03, 0x07 // SFP, LC, no diagnostics
		}
		return b, nil
	}
	if m := module("ens1f0np0"); m == nil || m.Type != "SFP" || m.Connector != "LC" || m.DOM {
		t.Fatalf("module = %+v", m)
	}
}
