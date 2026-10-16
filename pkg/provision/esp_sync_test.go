//go:build linux

package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallESPSyncInstallsOnlyExplicitTargetArtifacts(t *testing.T) {
	root := t.TempDir()
	ramdisk := t.TempDir()
	old := ramdiskRoot
	ramdiskRoot = ramdisk
	t.Cleanup(func() { ramdiskRoot = old })
	asset := filepath.Join(ramdisk, "usr", "lib", "booty")
	if err := os.MkdirAll(asset, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asset, "booty-esp-sync"), []byte("helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewConfigurator(nil)
	c.SetRootDir(root)
	if err := c.InstallESPSync("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Fatal(err)
	}
	helper, err := os.ReadFile(filepath.Join(root, "usr", "local", "libexec", "booty-esp-sync"))
	if err != nil {
		t.Fatal(err)
	}
	if string(helper) != "helper" {
		t.Fatalf("helper = %q", helper)
	}
	unit, err := os.ReadFile(filepath.Join(root, "etc", "systemd", "system", "booty-esp-sync.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "--partuuid-b 22222222-2222-2222-2222-222222222222") {
		t.Fatalf("unit has wrong destination: %s", unit)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc", "systemd", "system", "multi-user.target.wants", "booty-esp-sync.service")); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "etc", "booty", "esp-sync.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "source_partuuid=11111111") {
		t.Fatalf("config missing source: %s", config)
	}
	if err := c.InstallESPSync("same", "same"); err == nil {
		t.Fatal("expected identical PARTUUID rejection")
	}
}
