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

// wipeAllCfg returns a config with the native options of the legacy wipe.
// WIPE_DEPTH=quick, because the thorough wipe discards every disk anyway.
func wipeAllCfg(mode string, wipeAll, discard *bool) *config.MachineConfig {
	return &config.MachineConfig{Mode: mode, Provision: config.ProvisionConfig{
		Disk: config.DiskConfig{WipeAllDisks: wipeAll, DiscardNVMe: discard, WipeDepth: config.WipeDepthQuick},
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

// Legacy evidence: provision.sh calls clear_mdadm_superblocks
// (common.sh:545-567) before the wipe. It stops every array and zeroes the
// superblock of every linux_raid_member, whole disks and partitions, because
// the quick wipe leaves metadata 1.0 superblocks at the partition ends.
// RAID_SUPERBLOCK_CLEAR=all (preset by CAPRF_LEGACY_COMPAT) does that in
// stop-raid; the default "wiped" zeroes only the disks that wipe-disks wipes.
func TestRAIDSuperblockClear(t *testing.T) {
	const all = "-ln -o PATH,FSTYPE"
	lsblk := []byte("/dev/sda \n/dev/sda1 vfat\n/dev/sda2 linux_raid_member\n/dev/sdb linux_raid_member\n/dev/sdc \n")
	for _, tt := range []struct {
		name     string
		mode     string
		clear    string
		legacy   bool
		wipeAll  bool
		target   string
		mirror   string
		wantStop string // lsblk args in stop-raid, "" for none
		wantWipe string // lsblk args in wipe-disks, "" for none
	}{
		{name: "provision wiped: target and mirror", mode: "provision", target: "/dev/sda", mirror: "/dev/sdb",
			wantWipe: all + " /dev/sda /dev/sdb"},
		{name: "provision wiped: single target", mode: "provision", clear: "wiped", target: "/dev/sda",
			wantWipe: all + " /dev/sda"},
		{name: "provision wiped: wipe all disks", mode: "provision", wipeAll: true, target: "/dev/sda", wantWipe: all},
		{name: "provision all", mode: "provision", clear: "all", target: "/dev/sda", wantStop: all},
		{name: "provision legacy preset", mode: "provision", legacy: true, target: "/dev/sda", wantStop: all},
		{name: "provision legacy with explicit wiped", mode: "provision", legacy: true, clear: "wiped", target: "/dev/sda", wantWipe: all + " /dev/sda"},
		{name: "hard deprovision wiped without override", mode: "hard", clear: "wiped", wantStop: all},
		{name: "hard deprovision all without override", mode: "hard", clear: "all", wantStop: all},
		{name: "hard deprovision wiped with override", mode: "hard", clear: "wiped", target: "/dev/sda", mirror: "/dev/sdb",
			wantStop: all + " /dev/sda /dev/sdb"},
		{name: "hard deprovision all with override", mode: "hard", clear: "all", target: "/dev/sda", mirror: "/dev/sdb",
			wantStop: all + " /dev/sda /dev/sdb"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.MachineConfig{Mode: tt.mode, LegacyCompat: tt.legacy}
			cfg.Provision.Disk.RAIDSuperblockClear = tt.clear
			cfg.Provision.Disk.WipeAllDisks = new(tt.wipeAll)
			cfg.Provision.Disk.WipeDepth = config.WipeDepthQuick
			cfg.Provision.Disk.DiscardNVMe = new(false)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
			withFixtureDisks(t, o, "sda", "sdb", "sdc")
			o.targetDisk, o.mirrorDisk = tt.target, tt.mirror
			cmd.setResult("lsblk -ln", lsblk, nil)
			var steps []Step
			if tt.mode == "provision" {
				steps = o.provisionSteps()
			} else {
				steps = o.deprovisionSteps(tt.mode)
			}
			step := func(name string) func(context.Context) error {
				for _, s := range steps {
					if s.Name == name {
						return s.Fn
					}
				}
				t.Fatalf("no %s step in %s", name, tt.mode)
				return nil
			}

			if err := step("stop-raid")(context.Background()); err != nil {
				t.Fatalf("stop-raid error = %v", err)
			}
			if commandIndex(cmd.calls, "mdadm", "--stop", "--scan") != 0 {
				t.Fatalf("stop-raid must stop the arrays first: %v", cmd.calls)
			}
			assertRAIDClear(t, "stop-raid", cmd.calls, tt.wantStop)

			cmd.calls = nil
			// The wipe itself runs too, so wipe the superblocks before it.
			if tt.mode == "provision" {
				if err := o.wipeOrSecureEraseDisks(context.Background()); err != nil {
					t.Fatalf("wipe-disks error = %v", err)
				}
				assertRAIDClear(t, "wipe-disks", cmd.calls, tt.wantWipe)
				if tt.wantWipe != "" && commandIndex(cmd.calls, "lsblk", strings.Fields(tt.wantWipe)...) >
					commandIndex(cmd.calls, "wipefs", "-af", "/dev/sda") {
					t.Fatalf("superblocks zeroed after the wipe: %v", cmd.calls)
				}
			}
		})
	}
}

// assertRAIDClear checks that calls list superblocks with exactly the lsblk
// args want, or not at all for an empty want, and zero only the members.
func assertRAIDClear(t *testing.T, step string, calls []mockCall, want string) {
	t.Helper()
	var lsblkCalls []string
	for _, c := range calls {
		if c.name == "lsblk" && len(c.args) > 0 && c.args[0] == "-ln" {
			lsblkCalls = append(lsblkCalls, strings.Join(c.args, " "))
		}
	}
	if want == "" {
		if len(lsblkCalls) != 0 || hasCommandName(calls, "mdadm") && commandIndex(calls, "mdadm", "--zero-superblock", "/dev/sdb") >= 0 {
			t.Fatalf("%s zeroed RAID superblocks: %v", step, calls)
		}
		return
	}
	if len(lsblkCalls) != 1 || lsblkCalls[0] != want {
		t.Fatalf("%s listed %v, want lsblk %s", step, lsblkCalls, want)
	}
	for _, dev := range []string{"/dev/sda2", "/dev/sdb"} {
		if !hasCall(calls, "mdadm", "--zero-superblock "+dev) {
			t.Fatalf("%s did not zero %s: %v", step, dev, calls)
		}
	}
	if hasCall(calls, "mdadm", "--zero-superblock /dev/sda1") {
		t.Fatalf("%s zeroed a device that is no RAID member: %v", step, calls)
	}
}
