//go:build linux

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func withFixtureDisks(t *testing.T, o *Orchestrator, names ...string) {
	t.Helper()
	sysfs := t.TempDir()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(sysfs, "block", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	o.disk.SetSysfsRoot(sysfs)
}

// withPresets applies the config presets such as CAPRF_LEGACY_COMPAT, like
// Validate does for a parsed config.
func withPresets(cfg *config.MachineConfig) *config.MachineConfig {
	cfg.ApplyPresets()
	return cfg
}

// wipeAllCfg returns a config with the native wipe options.
func wipeAllCfg(mode string, wipeAll, discard *bool) *config.MachineConfig {
	return &config.MachineConfig{Mode: mode, Provision: config.ProvisionConfig{
		Disk: config.DiskConfig{WipeAllDisks: wipeAll, DiscardNVMe: discard},
	}}
}

func hasCall(calls []mockCall, name, args string) bool {
	for _, call := range calls {
		if call.name == name && strings.Join(call.args, " ") == args {
			return true
		}
	}
	return false
}

// Legacy evidence: provision.sh calls wipe_all_disks (common.sh:383-417),
// which runs blkdiscard on NVMe disks and wipefs -af on every disk. A
// failure is logged and the run continues. WIPE_ALL_DISKS=true with
// DISCARD_NVME=true selects it, and so does the CAPRF_LEGACY_COMPAT preset.
func TestWipeDisksWipeAllDisksWipesEveryDisk(t *testing.T) {
	for _, mode := range []string{"provision", "deprovision"} {
		configs := map[string]*config.MachineConfig{
			"preset": withPresets(&config.MachineConfig{Mode: mode, LegacyCompat: true}),
			"native": wipeAllCfg(mode, new(true), new(true)),
		}
		for source, cfg := range configs {
			t.Run(mode+"/"+source, func(t *testing.T) {
				o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
				withFixtureDisks(t, o, "nvme0n1", "nvme0c0n1", "sda", "sr0")
				if mode == "provision" {
					o.targetDisk = "/dev/sda"
				}
				cmd.setResult("wipefs -af", nil, errors.New("defect disk"))

				if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
					t.Fatalf("wipeOrSecureEraseDisks() = %v, want nil like the legacy script", err)
				}
				for _, want := range []struct{ name, args string }{
					{"blkdiscard", "/dev/nvme0n1"},
					{"wipefs", "-af /dev/nvme0n1"},
					{"wipefs", "-af /dev/sda"},
				} {
					if !hasCall(cmd.calls, want.name, want.args) {
						t.Errorf("missing %s %s in %v", want.name, want.args, cmd.calls)
					}
				}
				if hasCall(cmd.calls, "blkdiscard", "/dev/nvme0c0n1") || hasCall(cmd.calls, "wipefs", "-af /dev/nvme0c0n1") {
					t.Errorf("hidden NVMe multipath path node was wiped: %v", cmd.calls)
				}
				if hasCall(cmd.calls, "wipefs", "-af /dev/sr0") || hasCall(cmd.calls, "blkdiscard", "/dev/sda") {
					t.Errorf("unexpected wipe of a CD-ROM or discard of a non-NVMe disk: %v", cmd.calls)
				}
			})
		}
	}
}

// WIPE_ALL_DISKS without DISCARD_NVME, or with an explicit DISCARD_NVME=false
// over the preset, wipes every disk without blkdiscard.
func TestWipeDisksWipeAllDisksWithoutDiscard(t *testing.T) {
	presetNoDiscard := &config.MachineConfig{Mode: "provision", LegacyCompat: true}
	presetNoDiscard.Provision.Disk.DiscardNVMe = new(false)
	for name, cfg := range map[string]*config.MachineConfig{
		"native":              wipeAllCfg("provision", new(true), nil),
		"explicit false wins": withPresets(presetNoDiscard),
	} {
		t.Run(name, func(t *testing.T) {
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
			withFixtureDisks(t, o, "nvme0n1", "sda")
			o.targetDisk = "/dev/sda"
			if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
				t.Fatalf("wipeOrSecureEraseDisks() = %v", err)
			}
			if !hasCall(cmd.calls, "wipefs", "-af /dev/nvme0n1") {
				t.Errorf("non-target disk not wiped: %v", cmd.calls)
			}
			if hasCall(cmd.calls, "blkdiscard", "/dev/nvme0n1") {
				t.Errorf("blkdiscard ran without DISCARD_NVME: %v", cmd.calls)
			}
		})
	}
}

func TestWipeDisksDefaultWipesOnlyTheTarget(t *testing.T) {
	presetOff := &config.MachineConfig{Mode: "provision", LegacyCompat: true}
	presetOff.Provision.Disk.WipeAllDisks = new(false)
	for name, cfg := range map[string]*config.MachineConfig{
		"default":             {Mode: "provision"},
		"explicit false wins": withPresets(presetOff),
	} {
		t.Run(name, func(t *testing.T) {
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
			withFixtureDisks(t, o, "nvme0n1", "sda")
			o.targetDisk = "/dev/sda"

			if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
				t.Fatalf("wipeOrSecureEraseDisks() = %v", err)
			}
			if hasCall(cmd.calls, "wipefs", "-af /dev/nvme0n1") || hasCall(cmd.calls, "blkdiscard", "/dev/nvme0n1") {
				t.Fatalf("touched a non-target disk: %v", cmd.calls)
			}
		})
	}
}

func TestUsesWipeAllDisks(t *testing.T) {
	tests := []struct {
		name   string
		cfg    config.MachineConfig
		target string
		want   bool
	}{
		{name: "default", cfg: config.MachineConfig{Mode: "provision"}},
		{name: "native provision", cfg: *wipeAllCfg("provision", new(true), nil), target: "/dev/sda", want: true},
		{name: "native false", cfg: *wipeAllCfg("provision", new(false), nil), target: "/dev/sda"},
		{name: "compat provision", cfg: config.MachineConfig{Mode: "provision", LegacyCompat: true}, target: "/dev/sda", want: true},
		{name: "compat hard deprovision", cfg: config.MachineConfig{Mode: "deprovision", LegacyCompat: true}, want: true},
		{name: "compat deprovision with disk override", cfg: config.MachineConfig{Mode: "deprovision", LegacyCompat: true}, target: "/dev/sdb"},
		{name: "compat soft deprovision", cfg: config.MachineConfig{Mode: "soft", LegacyCompat: true}},
		{name: "compat dry run", cfg: config.MachineConfig{Mode: "dry-run", LegacyCompat: true}},
		{
			name: "compat with explicit RAID",
			cfg: config.MachineConfig{Mode: "provision", LegacyCompat: true, Provision: config.ProvisionConfig{
				Disk: config.DiskConfig{RAID: []config.RAIDConfig{{Name: "md0", Level: 1}}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			o := newTestOrchestrator(t, withPresets(&cfg), &mockProvider{})
			o.targetDisk = tt.target
			if got := o.usesWipeAllDisks(); got != tt.want {
				t.Fatalf("usesWipeAllDisks() = %v, want %v", got, tt.want)
			}
		})
	}
}

// wipe_all_disks logs a failed blkdiscard and goes on with the next disk
// and the signature wipe.
func TestWipeDisksWipeAllDisksContinuesAfterDiscardFailure(t *testing.T) {
	cfg := wipeAllCfg("provision", new(true), new(true))
	o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
	withFixtureDisks(t, o, "nvme0n1", "nvme1n1")
	o.targetDisk = "/dev/nvme0n1"
	cmd.setResult("blkdiscard /dev/nvme0n1", nil, errors.New("discard not supported"))

	if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
		t.Fatalf("wipeOrSecureEraseDisks() = %v, want nil", err)
	}
	for _, want := range []struct{ name, args string }{
		{"blkdiscard", "/dev/nvme1n1"},
		{"wipefs", "-af /dev/nvme0n1"},
		{"wipefs", "-af /dev/nvme1n1"},
	} {
		if !hasCall(cmd.calls, want.name, want.args) {
			t.Errorf("missing %s %s after a failed discard: %v", want.name, want.args, cmd.calls)
		}
	}
}
