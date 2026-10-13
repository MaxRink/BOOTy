//go:build linux

package runmode

import (
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestResolveProvision(t *testing.T) {
	tests := []struct {
		mode     string
		wantName string
		wantErr  bool
	}{
		{mode: "", wantName: "provision"},
		{mode: "provision", wantName: "provision"},
		{mode: "dry-run", wantName: "dry-run"},
		{mode: "deprovision", wantName: "deprovision"},
		{mode: "soft-deprovision", wantName: "deprovision"},
		{mode: "decommission", wantName: "deprovision"},
		{mode: "standby", wantName: "standby"},
		{mode: "check", wantName: "check"},
		{mode: "invalid-mode", wantErr: true},
	}

	for _, tt := range tests {
		name := tt.mode
		if name == "" {
			name = "(default)"
		}
		t.Run(name, func(t *testing.T) {
			deps := Deps{Cfg: &config.MachineConfig{Mode: tt.mode}}
			m, err := Resolve(deps)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.Name() != tt.wantName {
				t.Errorf("Name() = %q, want %q", m.Name(), tt.wantName)
			}
		})
	}
}

func TestProvisionModeInitialState(t *testing.T) {
	deps := Deps{Cfg: &config.MachineConfig{}}
	m, _ := Resolve(deps)
	pm := m.(*ProvisionMode)
	if pm.Succeeded() {
		t.Error("Succeeded() should be false before Run")
	}
	if pm.FirmwareChanged() {
		t.Error("FirmwareChanged() should be false before Run")
	}
}

func TestProvisionCompleteRequestsRebootFallback(t *testing.T) {
	err := provisionComplete(true, false)
	if !err.FirmwareChanged {
		t.Error("FirmwareChanged should be preserved")
	}
	if err.PowerOff {
		t.Error("direct provision completion should reboot when kexec is skipped or fails")
	}
}

func TestProvisionCompletePowersOffWithSuccessActionPowerOff(t *testing.T) {
	err := provisionComplete(false, true)
	if !err.PowerOff {
		t.Error("SUCCESS_ACTION=poweroff provision completion must power off like the legacy CAPRF provisioner")
	}
}
