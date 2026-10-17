//go:build linux

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/oob"
)

// installTestConsole installs a reporter that writes to a temp file and
// returns a function that reads it.
func installTestConsole(t *testing.T, phase string) func() string {
	t.Helper()
	console := filepath.Join(t.TempDir(), "ttyS0")
	if err := os.WriteFile(console, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := oob.NewReporter(&oob.Options{Serial: true, Consoles: []string{console}, Run: "0a1b2c3d"})
	r.Phase(phase)
	oob.Install(r)
	t.Cleanup(func() { oob.Install(nil) })
	return func() string {
		data, err := os.ReadFile(console)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

// A successful run ends with the success line.
func TestDeprovisionWritesSuccessStatusLine(t *testing.T) {
	oldCommand := hostCommandCombinedOutput
	hostCommandCombinedOutput = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no efi") }
	t.Cleanup(func() { hostCommandCombinedOutput = oldCommand })
	withEFIFirmware(t, true)
	oldMount := mountEFIVarFS
	mountEFIVarFS = func(string, string, string, uintptr, string) error { return syscall.ENODEV }
	t.Cleanup(func() { mountEFIVarFS = oldMount })
	read := installTestConsole(t, "deprovision")

	o, _ := newTestOrchestratorWithCommander(t, withPresets(&config.MachineConfig{Mode: "decommission", LegacyCompat: true}), &mockProvider{})
	o.disk.SetSysfsRoot(t.TempDir())
	if err := o.Deprovision(context.Background()); err != nil {
		t.Fatalf("Deprovision() = %v", err)
	}
	want := "BOOTY1 run=0a1b2c3d phase=deprovision code=- step=- result=running\n" +
		"BOOTY1 run=0a1b2c3d phase=deprovision code=- step=- result=success\n"
	if got := read(); got != want {
		t.Fatalf("console = %q, want %q", got, want)
	}
}

// A failed step writes the failure line with its code and step.
func TestDeprovisionWritesFailureStatusLine(t *testing.T) {
	read := installTestConsole(t, "deprovision")
	provider := &mockProvider{reportStatusErrs: []error{errors.New("init callback down")}}
	o := newTestOrchestrator(t, &config.MachineConfig{Mode: "soft"}, provider)
	if err := o.Deprovision(context.Background()); err == nil {
		t.Fatal("Deprovision() = nil, want the report-init failure")
	}
	want := "BOOTY1 run=0a1b2c3d phase=deprovision code=BOOTY-RPT-001 step=report-init result=failed\n"
	if got := read(); got != "BOOTY1 run=0a1b2c3d phase=deprovision code=- step=- result=running\n"+want {
		t.Fatalf("console = %q, want the failure line %q", got, want)
	}
}
