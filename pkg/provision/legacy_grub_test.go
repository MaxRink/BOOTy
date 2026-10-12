//go:build linux

package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func writeFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// Legacy evidence: provision.sh:452-481 writes
// GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud $CONSOLE_PARAMS $EXTRA_KERNEL_PARAMS"
// to /etc/default/grub.d/10-caprf-kernel-params.cfg, with ttyS1 on Lenovo
// and ttyS0 elsewhere, and runs update-grub only when the image has it.
func TestConfigureGRUBLegacyCompat(t *testing.T) {
	tests := []struct {
		name       string
		vendor     string
		extra      string
		updateGRUB bool
		wantLine   string
	}{
		{
			name:       "default console with extra params",
			vendor:     "HPE\n",
			extra:      "intel_iommu=on console=tty0",
			updateGRUB: true,
			wantLine:   `GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud console=ttyS0,115200n8 intel_iommu=on console=tty0"`,
		},
		{
			name:     "lenovo uses ttyS1",
			vendor:   "LENOVO\n",
			wantLine: `GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud console=ttyS1,115200n8"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			c := newTestConfigurator(t, cmd)
			writeFixture(t, filepath.Join(c.hostSysRoot, "class", "dmi", "id", "sys_vendor"), tt.vendor, 0o644)
			if tt.updateGRUB {
				writeFixture(t, filepath.Join(c.rootDir, "usr", "sbin", "update-grub"), "#!/bin/sh\n", 0o755)
			}
			cfg := &config.MachineConfig{LegacyCompat: true}
			cfg.Provision.ExtraKernelParams = tt.extra

			if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
				t.Fatalf("ConfigureGRUB() = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(data)); got != tt.wantLine {
				t.Fatalf("grub drop-in = %q, want %q", got, tt.wantLine)
			}
			ran := false
			for _, call := range cmd.calls {
				if call.name == "chroot" && strings.Contains(strings.Join(call.args, " "), "/usr/sbin/update-grub") {
					ran = true
				}
			}
			if ran != tt.updateGRUB {
				t.Fatalf("update-grub ran = %v, want %v (calls %v)", ran, tt.updateGRUB, cmd.calls)
			}
		})
	}
}

func TestConfigureGRUBLegacyCompatRejectsUnsafeParams(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	cfg := &config.MachineConfig{LegacyCompat: true}
	cfg.Provision.ExtraKernelParams = `quiet" ; rm -rf /`
	if err := c.ConfigureGRUB(context.Background(), cfg); err == nil {
		t.Fatal("ConfigureGRUB() accepted a quote that breaks the grub drop-in")
	}
}

// The legacy provisioner does not write getty units.
func TestConfigureSerialConsoleSkippedInLegacyCompat(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{LegacyCompat: true}, &mockProvider{})
	if err := o.configureSerialConsole(context.Background()); err != nil {
		t.Fatalf("configureSerialConsole() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.config.rootDir, "etc", "systemd")); !os.IsNotExist(err) {
		t.Fatalf("legacy compat wrote getty configuration: %v", err)
	}
}

func TestFindUpdateGRUB(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{name: "usr sbin", setup: func(t *testing.T, root string) {
			t.Helper()
			writeFixture(t, filepath.Join(root, "usr", "sbin", "update-grub"), "#!/bin/sh\n", 0o755)
		}, want: "/usr/sbin/update-grub"},
		{name: "sbin fallback", setup: func(t *testing.T, root string) {
			t.Helper()
			writeFixture(t, filepath.Join(root, "sbin", "update-grub"), "#!/bin/sh\n", 0o755)
		}, want: "/sbin/update-grub"},
		{name: "not executable", setup: func(t *testing.T, root string) {
			t.Helper()
			writeFixture(t, filepath.Join(root, "usr", "sbin", "update-grub"), "#!/bin/sh\n", 0o644)
		}},
		{name: "neither, no shell in the target", setup: func(t *testing.T, _ string) { t.Helper() }},
		{name: "symlink escaping the root", setup: func(t *testing.T, root string) {
			t.Helper()
			outside := t.TempDir()
			writeFixture(t, filepath.Join(outside, "update-grub"), "#!/bin/sh\n", 0o755)
			if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "usr", "sbin")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			c := newTestConfigurator(t, cmd)
			tt.setup(t, c.rootDir)
			if got := c.findUpdateGRUB(); got != tt.want {
				t.Fatalf("findUpdateGRUB() = %q, want %q", got, tt.want)
			}
			if tt.want == "" {
				cfg := &config.MachineConfig{LegacyCompat: true}
				if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
					t.Fatalf("ConfigureGRUB() = %v, want a skip without update-grub", err)
				}
				if len(cmd.calls) != 0 {
					t.Fatalf("ConfigureGRUB() ran %v without update-grub", cmd.calls)
				}
			}
		})
	}
}
