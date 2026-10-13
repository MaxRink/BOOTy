//go:build linux

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func withPCIDevices(t *testing.T, devices map[string][3]string) {
	t.Helper()
	root := t.TempDir()
	for addr, attrs := range devices {
		dir := filepath.Join(root, addr)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"vendor", "device", "class"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(attrs[i]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	old, oldLookPath := pciDevicesRoot, lookPath
	pciDevicesRoot = root
	lookPath = func(name string) (string, error) { return "/bin/" + name, nil }
	t.Cleanup(func() { pciDevicesRoot, lookPath = old, oldLookPath })
}

func TestSetupMellanoxLegacyWithoutMstconfig(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x1017", "0x020000"}})
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	changed, err := c.setupMellanoxLegacy(context.Background(), 0)
	if err != nil || changed || len(cmd.calls) != 0 {
		t.Fatalf("setupMellanoxLegacy() = %v, %v, calls %v; want a skip like the legacy script", changed, err, cmd.calls)
	}
}

func mstCalls(calls []mockCall) []string {
	var out []string
	for _, call := range calls {
		if call.name == "mstconfig" {
			out = append(out, strings.Join(call.args, " "))
		}
	}
	return out
}

// Legacy evidence: provision.sh setup_mellanox_num_of_vfs sets NUM_OF_VFS=32
// on every "Ethernet ... Mellanox ... [ConnectX-4..7 (Dx/Lx)]" function whose
// current value is lower.
func TestSetupMellanoxLegacy(t *testing.T) {
	withPCIDevices(t, map[string][3]string{
		"0000:3b:00.0": {"0x15b3", "0x1017", "0x020000"}, // ConnectX-5 port 1
		"0001:3b:00.0": {"0x15b3", "0x1017", "0x020000"}, // ConnectX-5 in PCI domain 1
		"0000:5e:00.0": {"0x15b3", "0x1015", "0x020000"}, // ConnectX-4 Lx: not matched
		"0000:af:00.0": {"0x15b3", "0x101b", "0x020700"}, // ConnectX-6 in IB mode
		"0000:00:1f.6": {"0x8086", "0x15bb", "0x020000"}, // Intel NIC
	})
	cmd := newMockCommander()
	cmd.setResult("mstconfig -d", []byte("Configurations:     Next Boot\n         NUM_OF_VFS          8\n"), nil)
	c := newTestConfigurator(t, cmd)

	changed, err := c.setupMellanoxLegacy(context.Background(), 0)
	if err != nil || !changed {
		t.Fatalf("setupMellanoxLegacy() = %v, %v; want changed", changed, err)
	}
	want := []string{
		"-d 0000:3b:00.0 q", "-y -d 0000:3b:00.0 s NUM_OF_VFS=32",
		"-d 0001:3b:00.0 q", "-y -d 0001:3b:00.0 s NUM_OF_VFS=32",
	}
	if got := mstCalls(cmd.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("mstconfig calls = %v, want %v", got, want)
	}
}

func TestSetupMellanoxLegacyKeepsHigherValues(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x101d", "0x020000"}})
	cmd := newMockCommander()
	cmd.setResult("mstconfig -d", []byte("NUM_OF_VFS 64\n"), nil)
	c := newTestConfigurator(t, cmd)

	changed, err := c.setupMellanoxLegacy(context.Background(), 0)
	if err != nil || changed {
		t.Fatalf("setupMellanoxLegacy() = %v, %v; want unchanged", changed, err)
	}
}

func TestSetupMellanoxLegacyWithoutNICs(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:00:1f.6": {"0x8086", "0x15bb", "0x020000"}})
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	changed, err := c.setupMellanoxLegacy(context.Background(), 0)
	if err != nil || changed || len(cmd.calls) != 0 {
		t.Fatalf("setupMellanoxLegacy() = %v, %v, calls %v; want a silent skip", changed, err, cmd.calls)
	}
}

func TestSetupMellanoxLegacyQueryFailure(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x1021", "0x020000"}})
	cmd := newMockCommander()
	cmd.setResult("mstconfig -d", nil, errors.New("no device"))
	c := newTestConfigurator(t, cmd)
	if _, err := c.setupMellanoxLegacy(context.Background(), 0); err == nil {
		t.Fatal("setupMellanoxLegacy() = nil, want the query failure like the legacy set -e")
	}
}

// Without legacy compat BOOTy keeps NUM_VFS opt-in.
func TestSetupMellanoxStepDefaultIsOptIn(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x1017", "0x020000"}})
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{}, &mockProvider{})
	if err := o.setupMellanox(context.Background()); err != nil {
		t.Fatalf("setupMellanox() = %v", err)
	}
	if len(mstCalls(cmd.calls)) != 0 {
		t.Fatalf("default mode ran mstconfig: %v", cmd.calls)
	}
}

// The orchestrator step takes the legacy path with NUM_VFS_MODE=raise, set
// natively or by the preset, and the default path with an explicit set.
func TestSetupMellanoxStepNumVFsMode(t *testing.T) {
	explicitSet := &config.MachineConfig{LegacyCompat: true}
	explicitSet.Provision.Firmware.NumVFsMode = config.NumVFsModeSet
	native := &config.MachineConfig{}
	native.Provision.Firmware.NumVFsMode = config.NumVFsModeRaise
	for name, tt := range map[string]struct {
		cfg   *config.MachineConfig
		raise bool
	}{
		"native raise":                  {cfg: native, raise: true},
		"preset":                        {cfg: withPresets(&config.MachineConfig{LegacyCompat: true}), raise: true},
		"explicit set wins over preset": {cfg: withPresets(explicitSet)},
	} {
		t.Run(name, func(t *testing.T) {
			withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x1017", "0x020000"}})
			o, cmd := newTestOrchestratorWithCommander(t, tt.cfg, &mockProvider{})
			cmd.setResult("mstconfig -d", []byte("NUM_OF_VFS 8\n"), nil)

			if err := o.setupMellanox(context.Background()); err != nil {
				t.Fatalf("setupMellanox() = %v", err)
			}
			var want []string
			if tt.raise {
				want = []string{"-d 0000:3b:00.0 q", "-y -d 0000:3b:00.0 s NUM_OF_VFS=32"}
			}
			if got := mstCalls(cmd.calls); !reflect.DeepEqual(got, want) {
				t.Fatalf("mstconfig calls = %v, want %v", got, want)
			}
			if o.FirmwareChanged() != tt.raise {
				t.Fatalf("FirmwareChanged() = %v, want %v", o.FirmwareChanged(), tt.raise)
			}
		})
	}
}

func TestSetupMellanoxLegacyNumVFsOverride(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x1017", "0x020000"}})
	cmd := newMockCommander()
	cmd.setResult("mstconfig -d", []byte("NUM_OF_VFS 8\n"), nil)
	c := newTestConfigurator(t, cmd)

	changed, err := c.setupMellanoxLegacy(context.Background(), 16)
	if err != nil || !changed {
		t.Fatalf("setupMellanoxLegacy(16) = %v, %v; want changed", changed, err)
	}
	want := []string{"-d 0000:3b:00.0 q", "-y -d 0000:3b:00.0 s NUM_OF_VFS=16"}
	if got := mstCalls(cmd.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("mstconfig calls = %v, want %v", got, want)
	}

	cmd = newMockCommander()
	cmd.setResult("mstconfig -d", []byte("NUM_OF_VFS 16\n"), nil)
	c = newTestConfigurator(t, cmd)
	if changed, err := c.setupMellanoxLegacy(context.Background(), 16); err != nil || changed {
		t.Fatalf("setupMellanoxLegacy(16) with 16 configured = %v, %v; want unchanged", changed, err)
	}
}

func TestSetupMellanoxLegacySetFailure(t *testing.T) {
	withPCIDevices(t, map[string][3]string{"0000:3b:00.0": {"0x15b3", "0x1017", "0x020000"}})
	cmd := newMockCommander()
	cmd.setResult("mstconfig -d", []byte("NUM_OF_VFS 8\n"), nil)
	cmd.setResult("mstconfig -y", []byte("write failed"), errors.New("exit status 3"))
	c := newTestConfigurator(t, cmd)

	changed, err := c.setupMellanoxLegacy(context.Background(), 0)
	if err == nil || changed {
		t.Fatalf("setupMellanoxLegacy() = %v, %v; want the set failure and no change", changed, err)
	}
}
