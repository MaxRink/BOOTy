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

func TestDecommissionStepsMatchLegacyScript(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{Mode: "decommission"}, &mockProvider{})
	steps := o.deprovisionSteps("decommission")
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Name)
	}
	want := []string{
		"report-init", "copy-provisioner-files", "configure-dns",
		"decommission-wipe", "mount-efivarfs", "remove-all-efi-entries",
		"report-success",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("decommission steps = %v, want %v", names, want)
	}
}

// Legacy deprovision.sh decommission_wipe never fails on a defect disk or a
// missing EFI runtime. It always reports success.
func TestDecommissionReportsSuccessWhenWipeAndEFIFail(t *testing.T) {
	var efiCalls []string
	oldCommand := hostCommandCombinedOutput
	hostCommandCombinedOutput = func(_ context.Context, name string, args ...string) ([]byte, error) {
		efiCalls = append(efiCalls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		if name == "efibootmgr" && len(args) == 0 {
			return []byte("BootCurrent: 0001\nBootOrder: 0001,0003\nBoot0001* ubuntu\nBoot0003* PXE\n"), nil
		}
		return nil, errors.New("efi failure")
	}
	t.Cleanup(func() { hostCommandCombinedOutput = oldCommand })
	// Never mount the efivarfs of the machine running the tests.
	oldFirmwarePath, oldVarsPath := efiFirmwarePath, efiVarsPath
	efiFirmwarePath = filepath.Join(t.TempDir(), "missing-efi")
	efiVarsPath = filepath.Join(efiFirmwarePath, "efivars")
	t.Cleanup(func() { efiFirmwarePath, efiVarsPath = oldFirmwarePath, oldVarsPath })

	provider := &mockProvider{}
	o, cmd := newTestOrchestratorWithCommander(t, &config.MachineConfig{Mode: "decommission"}, provider)
	sysfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfs, "block", "sda"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.disk.SetSysfsRoot(sysfs)
	cmd.setResult("wipefs -af", nil, errors.New("defect disk"))
	cmd.setResult("mdadm --stop", nil, errors.New("no arrays"))

	if err := o.Deprovision(context.Background()); err != nil {
		t.Fatalf("Deprovision(decommission) = %v, want nil", err)
	}
	wiped := false
	for _, call := range cmd.calls {
		if call.name == "wipefs" && strings.Join(call.args, " ") == "-af /dev/sda" {
			wiped = true
		}
	}
	if !wiped {
		t.Fatalf("decommission did not try to wipe /dev/sda: %v", cmd.calls)
	}
	got := make([]config.Status, 0, len(provider.statuses))
	for _, s := range provider.statuses {
		got = append(got, s.status)
	}
	if !reflect.DeepEqual(got, []config.Status{config.StatusInit, config.StatusSuccess}) {
		t.Fatalf("statuses = %v, want init then success", got)
	}
	for _, want := range []string{"efibootmgr -b 0001 -B", "efibootmgr -b 0003 -B"} {
		found := false
		for _, call := range efiCalls {
			if call == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %v: decommission removes every BootOrder entry", want, efiCalls)
		}
	}
}

func TestEFIBootOrder(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{name: "entries", out: "BootCurrent: 0002\nTimeout: 1 seconds\nBootOrder: 0002,0000,000A\nBoot0000* PXE\n", want: []string{"0002", "0000", "000A"}},
		{name: "no boot order", out: "BootCurrent: 0002\n", want: nil},
		{name: "empty boot order", out: "BootOrder: \n", want: nil},
		{name: "invalid entries skipped", out: "BootOrder: 0001,zz;rm,00012\n", want: []string{"0001"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := efiBootOrder(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("efiBootOrder() = %v, want %v", got, tt.want)
			}
		})
	}
}
