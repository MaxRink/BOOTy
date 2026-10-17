//go:build linux

package provision

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestPlannedHandoff(t *testing.T) {
	withGrub := t.TempDir()
	if err := os.MkdirAll(filepath.Join(withGrub, "boot", "grub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgFile := "menuentry 'os' {\n linux /boot/vmlinuz root=/dev/sda2\n initrd /boot/initrd.img\n}\n"
	if err := os.WriteFile(filepath.Join(withGrub, "boot", "grub", "grub.cfg"), []byte(cfgFile), 0o644); err != nil {
		t.Fatal(err)
	}
	noGrub := t.TempDir()
	ab := func(c *config.MachineConfig) {
		c.Provision.Image.Mode = config.ImageModeAB
		c.Provision.AB.PreserveExisting = true
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*config.MachineConfig)
		firmware bool
		root     string
		want     string
	}{
		{name: "kexec", root: withGrub, want: HandoffKexec},
		{name: "no grub entry", root: noGrub, want: HandoffReboot},
		{name: "poweroff", mutate: func(c *config.MachineConfig) { c.SuccessAction = config.SuccessActionPowerOff }, root: withGrub, want: HandoffPowerOff},
		{name: "kexec disabled", mutate: func(c *config.MachineConfig) { c.Provision.DisableKexec = true }, root: withGrub, want: HandoffReboot},
		{name: "secure boot re-enable", mutate: func(c *config.MachineConfig) { c.Provision.SecureBoot.ReEnable = true }, root: withGrub, want: HandoffReboot},
		{name: "firmware changed", firmware: true, root: withGrub, want: HandoffReboot},
		{name: "a/b kexec", mutate: ab, root: withGrub, want: HandoffKexec},
		{name: "a/b without entry", mutate: ab, root: noGrub, want: HandoffPowerOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			if got := PlannedHandoff(cfg, tc.firmware, tc.root); got != tc.want {
				t.Fatalf("PlannedHandoff = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeprovisionHandoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.MachineConfig
		want string
	}{
		{name: "reboot", want: HandoffReboot},
		{name: "poweroff", cfg: config.MachineConfig{SuccessAction: config.SuccessActionPowerOff}, want: HandoffPowerOff},
		{name: "standby", cfg: config.MachineConfig{StandbyCommand: true, Agent: config.AgentConfig{
			CommandsURL: "https://caprf/agent/commands", DeprovisionSuccessAction: config.AgentDeprovisionStandby,
		}}, want: HandoffStandby},
		// MODE=deprovision with the agent vars that CAPRF renders into every
		// ISO: BOOTy ends the run, it does not stay in standby.
		{name: "standby option in MODE=deprovision", cfg: config.MachineConfig{Agent: config.AgentConfig{
			CommandsURL: "https://caprf/agent/commands", DeprovisionSuccessAction: config.AgentDeprovisionStandby,
		}}, want: HandoffReboot},
		{name: "standby command without the option", cfg: config.MachineConfig{StandbyCommand: true}, want: HandoffReboot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deprovisionHandoff(&tc.cfg); got != tc.want {
				t.Fatalf("deprovisionHandoff = %q, want %q", got, tc.want)
			}
		})
	}
}
