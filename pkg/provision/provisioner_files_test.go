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

func withProvisionerFiles(t *testing.T, files map[string]os.FileMode) string {
	t.Helper()
	src := t.TempDir()
	for rel, mode := range files {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel+"\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	old := provisionerFilesDir
	provisionerFilesDir = src
	t.Cleanup(func() { provisionerFilesDir = old })
	return src
}

// Legacy evidence: common.sh copy_files copies deploy/file-system into the
// ramdisk root before the network starts and sets netplan files to 0600.
func TestApplyProvisionerFilesToRamdisk(t *testing.T) {
	withProvisionerFiles(t, map[string]os.FileMode{
		"etc/netplan/50-provision.yaml": 0o644,
		"etc/frr/frr.conf":              0o640,
		"etc/resolv.conf":               0o644,
	})
	root := t.TempDir()
	oldRoot := ramdiskRoot
	ramdiskRoot = root
	t.Cleanup(func() { ramdiskRoot = oldRoot })

	if err := ApplyProvisionerFilesToRamdisk(context.Background()); err != nil {
		t.Fatalf("ApplyProvisionerFilesToRamdisk() = %v", err)
	}
	for rel, wantMode := range map[string]os.FileMode{
		"etc/netplan/50-provision.yaml": 0o600,
		"etc/frr/frr.conf":              0o640,
		"etc/resolv.conf":               0o644,
	} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s not applied: %v", rel, err)
		}
		if info.Mode().Perm() != wantMode {
			t.Errorf("%s mode = %v, want %v", rel, info.Mode().Perm(), wantMode)
		}
	}
}

// copyTree skips symlinks, so a linked resolv.conf or etc directory is
// never applied and must not count as a provisioner resolv.conf.
func TestHasProvisionerResolvConfIgnoresSymlinks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(target, []byte("nameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := withProvisionerFiles(t, map[string]os.FileMode{"etc/resolv.conf": 0o644})
	if !HasProvisionerResolvConf() {
		t.Fatal("HasProvisionerResolvConf() = false for a regular file")
	}
	if err := os.Remove(filepath.Join(src, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(src, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if HasProvisionerResolvConf() {
		t.Error("HasProvisionerResolvConf() = true for a symlinked resolv.conf")
	}
	if err := os.RemoveAll(filepath.Join(src, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(target), filepath.Join(src, "etc")); err != nil {
		t.Fatal(err)
	}
	if HasProvisionerResolvConf() {
		t.Error("HasProvisionerResolvConf() = true for a symlinked etc directory")
	}
}

func TestApplyProvisionerFilesToRamdiskWithoutFiles(t *testing.T) {
	old := provisionerFilesDir
	provisionerFilesDir = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { provisionerFilesDir = old })
	if err := ApplyProvisionerFilesToRamdisk(context.Background()); err != nil {
		t.Fatalf("ApplyProvisionerFilesToRamdisk() = %v, want nil without files", err)
	}
}

func TestCopyProvisionerFilesToTarget(t *testing.T) {
	withProvisionerFiles(t, map[string]os.FileMode{"etc/netplan/50-provision.yaml": 0o600})
	explicitTarget := &config.MachineConfig{LegacyCompat: true, ProvisionerFilesTarget: config.ProvisionerFilesTarget}
	for _, tt := range []struct {
		name       string
		cfg        *config.MachineConfig
		wantCopied bool
	}{
		{name: "default copies into the target", cfg: &config.MachineConfig{}, wantCopied: true},
		{name: "ramdisk keeps the target clean", cfg: &config.MachineConfig{ProvisionerFilesTarget: config.ProvisionerFilesRamdisk}},
		{name: "preset keeps the target clean", cfg: withPresets(&config.MachineConfig{LegacyCompat: true})},
		{name: "explicit target wins over preset", cfg: withPresets(explicitTarget), wantCopied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := newTestOrchestrator(t, tt.cfg, &mockProvider{})
			if err := o.copyProvisionerFiles(context.Background()); err != nil {
				t.Fatalf("copyProvisionerFiles() = %v", err)
			}
			_, err := os.Stat(filepath.Join(o.config.rootDir, "etc", "netplan", "50-provision.yaml"))
			if copied := err == nil; copied != tt.wantCopied {
				t.Fatalf("netplan copied to target = %v, want %v", copied, tt.wantCopied)
			}
		})
	}
}

// Legacy evidence: the legacy scripts never write the target resolv.conf.
// WRITE_RESOLV_CONF=false (preset by CAPRF_LEGACY_COMPAT) keeps it.
func TestConfigureDNSWriteResolvConfFalseKeepsTargetResolvConf(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.WriteResolvConf = new(false)
	cfg.Network.DNSResolvers = "10.0.0.53"
	o := newTestOrchestrator(t, cfg, &mockProvider{})
	etc := filepath.Join(o.config.rootDir, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "../run/systemd/resolve/stub-resolv.conf"
	if err := os.Symlink(stub, filepath.Join(etc, "resolv.conf")); err != nil {
		t.Fatal(err)
	}

	if err := o.configureDNS(context.Background()); err != nil {
		t.Fatalf("configureDNS() = %v", err)
	}
	got, err := os.Readlink(filepath.Join(etc, "resolv.conf"))
	if err != nil || got != stub {
		t.Fatalf("target resolv.conf changed: link %q, err %v", got, err)
	}
}

// The ramdisk root is "/". The old prefix check built "//" and rejected
// every path below it.
func TestEnsureWithinRoot(t *testing.T) {
	tests := []struct {
		root, target string
		ok           bool
	}{
		{"/", "/etc/hosts", true},
		{"/", "/", true},
		{"/newroot", "/newroot/etc/hosts", true},
		{"/newroot", "/newroot", true},
		{"/newroot", "/newroot/../etc/hosts", false},
		{"/newroot", "/newrootx/etc", false},
		{"/newroot", "/etc", false},
	}
	for _, tt := range tests {
		err := ensureWithinRoot(tt.root, tt.target)
		if (err == nil) != tt.ok {
			t.Errorf("ensureWithinRoot(%q, %q) = %v, want ok %v", tt.root, tt.target, err, tt.ok)
		}
	}
}

func TestHasProvisionerResolvConf(t *testing.T) {
	withProvisionerFiles(t, map[string]os.FileMode{"etc/hosts": 0o644})
	if HasProvisionerResolvConf() {
		t.Fatal("HasProvisionerResolvConf() = true without the file")
	}
	withProvisionerFiles(t, map[string]os.FileMode{"etc/resolv.conf": 0o644})
	if !HasProvisionerResolvConf() {
		t.Fatal("HasProvisionerResolvConf() = false with the file")
	}
}

func TestConfigureDNSWriteResolvConfPrecedence(t *testing.T) {
	explicitWrite := &config.MachineConfig{LegacyCompat: true}
	explicitWrite.Provision.WriteResolvConf = new(true)
	for _, tt := range []struct {
		name      string
		cfg       *config.MachineConfig
		wantWrite bool
	}{
		{name: "default writes", cfg: &config.MachineConfig{}, wantWrite: true},
		{name: "preset keeps", cfg: withPresets(&config.MachineConfig{LegacyCompat: true})},
		{name: "explicit true wins over preset", cfg: withPresets(explicitWrite), wantWrite: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.Network.DNSResolvers = "10.0.0.53"
			o := newTestOrchestrator(t, tt.cfg, &mockProvider{})
			if err := os.MkdirAll(filepath.Join(o.config.rootDir, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := o.configureDNS(context.Background()); err != nil {
				t.Fatalf("configureDNS() = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(o.config.rootDir, "etc", "resolv.conf"))
			if written := err == nil && strings.Contains(string(data), "10.0.0.53"); written != tt.wantWrite {
				t.Fatalf("resolv.conf written = %v, want %v (content %q, err %v)", written, tt.wantWrite, data, err)
			}
		})
	}
}

// The files of a standby command go into the ramdisk without etc/frr and
// etc/netplan: the running network keeps the settings of the boot.
func TestApplyCommandProvisionerFilesToRamdiskKeepsTheNetwork(t *testing.T) {
	withProvisionerFiles(t, map[string]os.FileMode{
		"etc/hosts":                             0o644,
		"usr/local/share/ca-certificates/x.crt": 0o644,
		"etc/frr/frr.conf":                      0o640,
		"etc/frr2/x":                            0o644,
		"etc/netplan/50-provision.yaml":         0o600,
	})
	root := t.TempDir()
	oldRoot := ramdiskRoot
	ramdiskRoot = root
	t.Cleanup(func() { ramdiskRoot = oldRoot })

	if err := ApplyCommandProvisionerFilesToRamdisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"etc/hosts": true, "usr/local/share/ca-certificates/x.crt": true,
		"etc/frr/frr.conf": false, "etc/netplan/50-provision.yaml": false,
		"etc/frr2/x": true, // the skip matches whole directories, not prefixes
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); (err == nil) != want {
			t.Fatalf("%s in the ramdisk = %t, want %t", rel, err == nil, want)
		}
	}
}
