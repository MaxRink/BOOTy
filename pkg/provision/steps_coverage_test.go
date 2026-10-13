//go:build linux

package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

// ---------------------------------------------------------------------------
// set-hostname orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorSetHostname_WritesFile(t *testing.T) {
	cfg := &config.MachineConfig{Hostname: "my-test-node"}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.setHostname(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hostnameFile := filepath.Join(o.config.rootDir, "etc", "hostname")
	data, err := os.ReadFile(hostnameFile)
	if err != nil {
		t.Fatalf("expected hostname file to exist: %v", err)
	}
	if string(data) != "my-test-node\n" {
		t.Errorf("hostname = %q, want %q", string(data), "my-test-node\n")
	}
}

func TestOrchestratorSetHostname_EmptySkips(t *testing.T) {
	cfg := &config.MachineConfig{Hostname: ""}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.setHostname(context.Background()); err != nil {
		t.Fatalf("unexpected error when hostname is empty: %v", err)
	}

	hostnameFile := filepath.Join(o.config.rootDir, "etc", "hostname")
	if _, err := os.Stat(hostnameFile); !os.IsNotExist(err) {
		t.Error("hostname file should not be created when hostname is empty")
	}
}

// ---------------------------------------------------------------------------
// configure-dns orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorConfigureDNS_WritesResolvConf(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "8.8.8.8,8.8.4.4"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := os.MkdirAll(filepath.Join(o.config.rootDir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := o.configureDNS(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(o.config.rootDir, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("expected resolv.conf to exist: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "nameserver 8.8.8.8") {
		t.Errorf("missing nameserver 8.8.8.8 in resolv.conf: %s", content)
	}
	if !strings.Contains(content, "nameserver 8.8.4.4") {
		t.Errorf("missing nameserver 8.8.4.4 in resolv.conf: %s", content)
	}
}

func TestOrchestratorConfigureDNS_EmptySkips(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.configureDNS(context.Background()); err != nil {
		t.Fatalf("unexpected error when no resolvers configured: %v", err)
	}
}

func TestOrchestratorConfigureDNS_TrimsWhitespace(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "  1.1.1.1 ,  9.9.9.9  "
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := os.MkdirAll(filepath.Join(o.config.rootDir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := o.configureDNS(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(o.config.rootDir, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("expected resolv.conf: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "nameserver 1.1.1.1") {
		t.Errorf("missing trimmed nameserver 1.1.1.1: %s", content)
	}
	if !strings.Contains(content, "nameserver 9.9.9.9") {
		t.Errorf("missing trimmed nameserver 9.9.9.9: %s", content)
	}
}

// ---------------------------------------------------------------------------
// configure-grub orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorConfigureGRUB_WritesConfig(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, provider)

	if err := o.configureGRUB(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	grubPath := filepath.Join(o.config.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg")
	data, err := os.ReadFile(grubPath)
	if err != nil {
		t.Fatalf("expected grub config to exist: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "GRUB_CMDLINE_LINUX=") {
		t.Errorf("expected GRUB_CMDLINE_LINUX in grub config: %s", content)
	}
	if strings.Contains(content, "ds=nocloud") {
		t.Errorf("default-disabled cloud-init must not force NoCloud in grub config: %s", content)
	}
}

func TestOrchestratorConfigureGRUB_NoCloudDatasourceEnabled(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Enabled = true
	provider := &mockProvider{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, provider)

	if err := o.configureGRUB(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	grubPath := filepath.Join(o.config.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg")
	data, err := os.ReadFile(grubPath)
	if err != nil {
		t.Fatalf("expected grub config to exist: %v", err)
	}
	if !strings.Contains(string(data), "ds=nocloud") {
		t.Errorf("expected enabled NoCloud datasource in grub config: %s", data)
	}
}

func TestOrchestratorConfigureGRUB_ConfigDriveDatasource(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = "configdrive"
	provider := &mockProvider{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, provider)

	if err := o.configureGRUB(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	grubPath := filepath.Join(o.config.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg")
	data, err := os.ReadFile(grubPath)
	if err != nil {
		t.Fatalf("expected grub config to exist: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "ci.datasource=ConfigDrive") {
		t.Errorf("expected ConfigDrive datasource in grub config: %s", content)
	}
	if strings.Contains(content, "ds=nocloud") {
		t.Errorf("ConfigDrive grub config must not force NoCloud: %s", content)
	}
}

func TestOrchestratorConfigureGRUB_WithExtraParams(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "quiet splash"
	provider := &mockProvider{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, provider)

	if err := o.configureGRUB(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	grubPath := filepath.Join(o.config.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg")
	data, err := os.ReadFile(grubPath)
	if err != nil {
		t.Fatalf("expected grub config: %v", err)
	}
	if !strings.Contains(string(data), "quiet splash") {
		t.Errorf("extra kernel params not written: %s", data)
	}
}

func TestOrchestratorConfigureGRUB_UnsafeParamsRejected(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "evil; rm -rf /"
	provider := &mockProvider{}
	o, _ := newTestOrchestratorWithCommander(t, cfg, provider)

	err := o.configureGRUB(context.Background())
	if err == nil {
		t.Fatal("expected error for unsafe kernel params")
	}
	if !strings.Contains(err.Error(), "unsafe characters") {
		t.Errorf("expected 'unsafe characters' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// run-machine-commands orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorRunMachineCommands_NoDirNoOp(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.runMachineCommands(context.Background()); err != nil {
		t.Fatalf("unexpected error when machine-commands dir absent: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mount-efivarfs orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorMountEFIVars_ReturnsNilOnNonEFIHost(t *testing.T) {
	oldCommand := hostCommandCombinedOutput
	hostCommandCombinedOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("modprobe unavailable"), os.ErrNotExist
	}
	t.Cleanup(func() { hostCommandCombinedOutput = oldCommand })
	oldFirmwarePath := efiFirmwarePath
	oldVarsPath := efiVarsPath
	efiFirmwarePath = filepath.Join(t.TempDir(), "missing-efi")
	efiVarsPath = filepath.Join(efiFirmwarePath, "efivars")
	t.Cleanup(func() {
		efiFirmwarePath = oldFirmwarePath
		efiVarsPath = oldVarsPath
	})

	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.mountEFIVars(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// remove-efi-entries orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorRemoveEFIBootEntries_GracefulWhenMissing(t *testing.T) {
	oldCommand := hostCommandCombinedOutput
	hostCommandCombinedOutput = func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { hostCommandCombinedOutput = oldCommand })

	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.removeEFIBootEntries(context.Background()); err != nil {
		t.Fatalf("unexpected error when efibootmgr unavailable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// create-efi-boot-entry orchestrator step
// ---------------------------------------------------------------------------

func TestOrchestratorCreateEFIBootEntry_SkipsWhenNoBootPartition(t *testing.T) {
	cfg := &config.MachineConfig{}
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)
	if err := o.createEFIBootEntry(context.Background()); err != nil {
		t.Fatalf("unexpected error when boot partition is empty: %v", err)
	}
}

// ---------------------------------------------------------------------------
// inject-cloudinit orchestrator step — instance-id fallback to "booty"
// ---------------------------------------------------------------------------

func TestOrchestratorInjectCloudInit_FallbackInstanceID(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Network.Static.Iface = "eth0"
	provider := &mockProvider{}
	o := newTestOrchestrator(t, cfg, provider)

	if err := o.injectCloudInit(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	metaPath := filepath.Join(o.config.rootDir, "var", "lib", "cloud", "seed", "nocloud", "meta-data")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("expected meta-data file: %v", err)
	}
	if !strings.Contains(string(data), "instance-id: booty") {
		t.Errorf("expected fallback instance-id 'booty', got: %s", data)
	}
}

// ---------------------------------------------------------------------------
// validateProvisionCommand
// ---------------------------------------------------------------------------

func TestValidateProvisionCommand_SemicolonBlocked(t *testing.T) {
	err := validateProvisionCommand("echo hello; rm -rf /")
	if err == nil {
		t.Fatal("expected error for semicolon in command")
	}
}

func TestValidateProvisionCommand_PipeBlocked(t *testing.T) {
	err := validateProvisionCommand("cat /etc/passwd | grep root")
	if err == nil {
		t.Fatal("expected error for pipe in command")
	}
}

func TestValidateProvisionCommand_BacktickBlocked(t *testing.T) {
	err := validateProvisionCommand("echo `whoami`")
	if err == nil {
		t.Fatal("expected error for backtick in command")
	}
}

func TestValidateProvisionCommand_SubshellBlocked(t *testing.T) {
	err := validateProvisionCommand("run $(evil)")
	if err == nil {
		t.Fatal("expected error for subshell substitution in command")
	}
}

func TestValidateProvisionCommand_SafeCommandAccepted(t *testing.T) {
	safeCmds := []string{
		"apt-get install -y curl",
		"systemctl enable kubelet",
		"grub-install /dev/sda",
		"update-grub",
	}
	for _, cmd := range safeCmds {
		if err := validateProvisionCommand(cmd); err != nil {
			t.Errorf("validateProvisionCommand(%q) unexpected error: %v", cmd, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Configurator.SetHostname — additional edge cases
// ---------------------------------------------------------------------------

func TestConfiguratorSetHostname_CreatesParentDir(t *testing.T) {
	root := t.TempDir()
	mgr := disk.NewManager(newMockCommander())
	c := NewConfigurator(mgr)
	c.rootDir = root

	cfg := &config.MachineConfig{Hostname: "created-dir-host"}
	if err := c.SetHostname(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "etc", "hostname"))
	if err != nil {
		t.Fatalf("expected hostname file: %v", err)
	}
	if string(data) != "created-dir-host\n" {
		t.Errorf("hostname = %q, want %q", string(data), "created-dir-host\n")
	}
}

func TestConfiguratorSetHostname_OverwritesExisting(t *testing.T) {
	root := t.TempDir()
	etcDir := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etcDir, "hostname"), []byte("old-host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := disk.NewManager(newMockCommander())
	c := NewConfigurator(mgr)
	c.rootDir = root

	cfg := &config.MachineConfig{Hostname: "new-host"}
	if err := c.SetHostname(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(etcDir, "hostname"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-host\n" {
		t.Errorf("hostname = %q, want new-host", string(data))
	}
}

// ---------------------------------------------------------------------------
// Configurator.ConfigureGRUB — grub.d directory creation
// ---------------------------------------------------------------------------

func TestConfiguratorConfigureGRUB_CreatesGrubDir(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	cfg := &config.MachineConfig{}
	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	grubDir := filepath.Join(c.rootDir, "etc", "default", "grub.d")
	if _, err := os.Stat(grubDir); err != nil {
		t.Errorf("expected grub.d to be created: %v", err)
	}
	cfgPath := filepath.Join(grubDir, "10-caprf-kernel-params.cfg")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("expected grub config file: %v", err)
	}
}

func TestConfiguratorConfigureGRUB_UnsafeParamsRejected(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "quiet; rm -rf /"
	err := c.ConfigureGRUB(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unsafe ExtraKernelParams")
	}
	if !strings.Contains(err.Error(), "unsafe characters") {
		t.Errorf("expected 'unsafe characters' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Configurator.RunMachineCommands — with actual command files
// ---------------------------------------------------------------------------

func TestConfiguratorRunMachineCommands_NoDirNoOp(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	if err := c.RunMachineCommands(context.Background(), false); err != nil {
		t.Fatalf("expected nil when machine-commands dir absent: %v", err)
	}
}
