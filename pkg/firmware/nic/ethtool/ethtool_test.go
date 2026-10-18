//go:build linux

package ethtool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	netethtool "github.com/telekom/BOOTy/pkg/network/ethtool"
)

// fakeFlags stands in for the ethtool netlink private flags.
type fakeFlags struct {
	flags  []netethtool.Flag
	getErr error
	setErr error
	sets   []string
}

func (f *fakeFlags) io() PrivFlagIO {
	return PrivFlagIO{
		Get: func(string) ([]netethtool.Flag, error) { return f.flags, f.getErr },
		Set: func(iface, name string, on bool) error {
			f.sets = append(f.sets, fmt.Sprintf("%s %s %v", iface, name, on))
			return f.setErr
		},
	}
}

type fakeCommander struct {
	outputs map[string][]byte
	errs    map[string]error
	calls   []string
}

func (f *fakeCommander) CombinedOutput(_ context.Context, cmd string, args ...string) ([]byte, error) {
	key := cmd + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)

	if err, ok := f.errs[key]; ok {
		return f.outputs[key], err
	}
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return nil, nil
}

func TestSupported(t *testing.T) {
	mgr := New(nic.VendorIntel, "0x8086", nil)
	if !mgr.Supported(&nic.Identifier{VendorID: "0x8086"}) {
		t.Fatal("Supported() = false, want true")
	}
	if mgr.Supported(&nic.Identifier{VendorID: "0x15b3"}) {
		t.Fatal("Supported() = true, want false")
	}
}

func TestCaptureParsesInfoAndPrivateFlags(t *testing.T) {
	fake := &fakeCommander{
		outputs: map[string][]byte{
			"ethtool -i eth0": []byte("driver: ixgbe\nfirmware-version: 1.2.3\nbus-info: 0000:01:00.0\n"),
		},
		errs: map[string]error{},
	}
	flags := &fakeFlags{flags: []netethtool.Flag{{Name: "flag_a", On: true}, {Name: "flag_b"}}}
	mgr := NewWithCommander(nic.VendorIntel, "0x8086", nil, fake).WithPrivFlags(flags.io())
	state, err := mgr.Capture(context.Background(), &nic.Identifier{PCIAddress: "0000:01:00.0", Interface: "eth0", VendorID: "0x8086"})
	if err != nil {
		t.Fatalf("Capture() error: %v", err)
	}

	if state.FWVersion != "1.2.3" {
		t.Fatalf("FWVersion = %q, want %q", state.FWVersion, "1.2.3")
	}
	if got := state.Parameters["flag_a"].Current; got != "on" {
		t.Fatalf("flag_a = %q, want %q", got, "on")
	}
	if got := state.Parameters["flag_b"].Current; got != "off" {
		t.Fatalf("flag_b = %q, want %q", got, "off")
	}
}

func TestCaptureIgnoresPrivateFlagsFailure(t *testing.T) {
	fake := &fakeCommander{
		outputs: map[string][]byte{
			"ethtool -i eth0": []byte("driver: ixgbe\nfirmware-version: 1.2.3\n"),
		},
		errs: map[string]error{},
	}
	flags := &fakeFlags{getErr: errors.New("unsupported")}
	mgr := NewWithCommander(nic.VendorIntel, "0x8086", nil, fake).WithPrivFlags(flags.io())
	state, err := mgr.Capture(context.Background(), &nic.Identifier{Interface: "eth0", VendorID: "0x8086"})
	if err != nil {
		t.Fatalf("Capture() error: %v", err)
	}
	if state.FWVersion != "1.2.3" {
		t.Fatalf("FWVersion = %q, want %q", state.FWVersion, "1.2.3")
	}
}

func TestApplyInvokesSetPrivFlags(t *testing.T) {
	fake := &fakeCommander{outputs: map[string][]byte{}, errs: map[string]error{}}
	flags := &fakeFlags{}
	mgr := NewWithCommander(nic.VendorIntel, "0x8086", nil, fake).WithPrivFlags(flags.io())

	err := mgr.Apply(context.Background(), &nic.Identifier{Interface: "eth0", VendorID: "0x8086"},
		[]nic.FlagChange{{Name: "flag_a", Value: "on"}, {Name: "flag_b", Value: "off"}})
	if err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if strings.Join(flags.sets, ";") != "eth0 flag_a true;eth0 flag_b false" || len(fake.calls) != 0 {
		t.Fatalf("netlink sets %v, commands %v; want two netlink sets and no ethtool command", flags.sets, fake.calls)
	}
}

func TestCaptureValidatesInputs(t *testing.T) {
	mgr := New(nic.VendorIntel, "0x8086", nil)
	if _, err := mgr.Capture(context.Background(), nil); err == nil {
		t.Fatal("Capture(nil) error = nil, want non-nil")
	}

	_, err := mgr.Capture(context.Background(), &nic.Identifier{VendorID: "0x8086"})
	if err == nil {
		t.Fatal("Capture() error = nil, want missing interface error")
	}
	if !strings.Contains(err.Error(), "interface name required") {
		t.Fatalf("Capture() error = %q, want interface name error", err.Error())
	}
}

func TestApplyPropagatesCommandFailure(t *testing.T) {
	flags := &fakeFlags{setErr: errors.New("permission denied")}
	mgr := NewWithCommander(nic.VendorIntel, "0x8086", nil, &fakeCommander{}).WithPrivFlags(flags.io())

	err := mgr.Apply(context.Background(), &nic.Identifier{Interface: "eth0", VendorID: "0x8086"}, []nic.FlagChange{{Name: "flag_a", Value: "on"}})
	if err == nil {
		t.Fatal("Apply() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "set flag_a=on") {
		t.Fatalf("Apply() error = %q, want wrapped flag context", err.Error())
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Apply() error = %q, want the netlink error", err.Error())
	}
}

func TestNewWithCommanderDefaults(t *testing.T) {
	mgr := NewWithCommander(nic.VendorBroadcom, "0x14e4", nil, nil)
	if mgr.log == nil {
		t.Fatal("manager logger is nil")
	}

	_, ok := mgr.commander.(OSCommander)
	if !ok {
		t.Fatalf("default commander type = %T, want OSCommander", mgr.commander)
	}
}

func TestApplyViaEthtoolFormatsError(t *testing.T) {
	flags := &fakeFlags{}
	mgr := NewWithCommander(nic.VendorIntel, "0x8086", nil, &fakeCommander{}).WithPrivFlags(flags.io())

	err := mgr.ApplyViaEthtool(context.Background(), "eth0", nic.FlagChange{Name: "foo", Value: "bar"})
	if err == nil || !strings.Contains(err.Error(), "want on or off") || len(flags.sets) != 0 {
		t.Fatalf("ApplyViaEthtool(bar) = %v, sets %v; want a value error and no set", err, flags.sets)
	}
}

func ExampleManager_Supported() {
	mgr := New(nic.VendorIntel, "0x8086", nil)
	fmt.Println(mgr.Supported(&nic.Identifier{VendorID: "0x8086"}))
	// Output: true
}
