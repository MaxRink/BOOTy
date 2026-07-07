//go:build linux

package provision

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/rescue"
)

type mockCommander struct {
	calls   []mockCall
	results map[string]mockResult
}

type mockCall struct {
	name string
	args []string
}

type mockResult struct {
	output []byte
	err    error
}

func newMockCommander() *mockCommander {
	return &mockCommander{results: make(map[string]mockResult)}
}

func (m *mockCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, mockCall{name: name, args: args})
	key := name
	if len(args) > 0 {
		key = name + " " + args[0]
	}
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}
	return nil, nil
}

func (m *mockCommander) setResult(key string, output []byte, err error) {
	m.results[key] = mockResult{output: output, err: err}
}

type mockProvider struct {
	statuses         []statusReport
	configs          []*config.MachineConfig
	firmwareReports  [][]byte
	reportStatusErr  error
	reportStatusErrs []error
}

type statusReport struct {
	status  config.Status
	message string
}

func (p *mockProvider) GetConfig(_ context.Context) (*config.MachineConfig, error) {
	if len(p.configs) > 0 {
		return p.configs[0], nil
	}
	return &config.MachineConfig{}, nil
}

func (p *mockProvider) ReportStatus(_ context.Context, status config.Status, message string) error {
	p.statuses = append(p.statuses, statusReport{status: status, message: message})
	if len(p.reportStatusErrs) > 0 {
		err := p.reportStatusErrs[0]
		p.reportStatusErrs = p.reportStatusErrs[1:]
		return err
	}
	return p.reportStatusErr
}

func (p *mockProvider) ShipLog(_ context.Context, _ string) error { return nil }
func (p *mockProvider) Heartbeat(_ context.Context) error         { return nil }
func (p *mockProvider) ReportFirmware(_ context.Context, data []byte) error {
	p.firmwareReports = append(p.firmwareReports, append([]byte(nil), data...))
	return nil
}
func (p *mockProvider) FetchCommands(_ context.Context) ([]config.Command, error) {
	return nil, nil
}

func (p *mockProvider) AcknowledgeCommand(_ context.Context, _, _, _ string) error { return nil }

func (p *mockProvider) ReportInventory(_ context.Context, _ []byte) error { return nil }

func newTestConfigurator(t *testing.T, cmd *mockCommander) *Configurator {
	t.Helper()
	root := t.TempDir()
	mgr := disk.NewManager(cmd)
	c := NewConfigurator(mgr)
	c.rootDir = root
	return c
}

func TestSetHostname(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{Hostname: "test-node"}
	if err := c.SetHostname(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "hostname"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "test-node\n" {
		t.Errorf("hostname = %q, want %q", string(data), "test-node\n")
	}
}

func TestSetHostnameEmpty(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{Hostname: ""}
	if err := c.SetHostname(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigureKubeletProviderID(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.OSFamily = "ubuntu"
	cfg.Provision.ProviderID = "redfish://host/sys/1"
	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	confPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := "KUBELET_EXTRA_ARGS=\"--provider-id=redfish://host/sys/1\"\n"
	if string(data) != expected {
		t.Errorf("got %q, want %q", string(data), expected)
	}
}

func TestConfigureKubeletNodeLabels(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.OSFamily = "ubuntu"
	cfg.Provision.FailureDomain = "dc1-az1"
	cfg.Provision.Region = "eu-central"
	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	confPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "--node-labels=topology.kubernetes.io/zone=dc1-az1,topology.kubernetes.io/region=eu-central") {
		t.Fatalf("expected topology labels in kubelet env file, got %q", content)
	}
}

func TestConfigureKubeletNoLabels(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	confPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
	if _, err := os.Stat(confPath); !os.IsNotExist(err) {
		t.Error("kubelet env file should not exist when no args are configured")
	}
}

func TestConfigureKubeletCombinedExtraArgs(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.OSFamily = "ubuntu"
	cfg.Provision.ProviderID = "redfish://host/sys/1"
	cfg.Provision.FailureDomain = "dc1-az1"
	cfg.Provision.Region = "eu-central"

	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	combinedPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
	data, err := os.ReadFile(combinedPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "--provider-id=redfish://host/sys/1") {
		t.Fatalf("combined config missing provider-id: %q", content)
	}
	if !strings.Contains(content, "--node-labels=topology.kubernetes.io/zone=dc1-az1,topology.kubernetes.io/region=eu-central") {
		t.Fatalf("combined config missing node-labels: %q", content)
	}

}

func TestConfigureKubeletRejectsUnsafeValuesBeforeWritingExtraArgs(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.MachineConfig)
		wantErr string
	}{
		{
			name: "provider id whitespace",
			mutate: func(cfg *config.MachineConfig) {
				cfg.Provision.ProviderID = "redfish://host/sys/1 bad"
			},
			wantErr: "provision.providerID",
		},
		{
			name: "failure domain label",
			mutate: func(cfg *config.MachineConfig) {
				cfg.Provision.FailureDomain = "-dc1"
			},
			wantErr: "provision.failureDomain",
		},
		{
			name: "region label",
			mutate: func(cfg *config.MachineConfig) {
				cfg.Provision.Region = "eu central"
			},
			wantErr: "provision.region",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newMockCommander()
			c := newTestConfigurator(t, cmd)
			cfg := &config.MachineConfig{OSFamily: "ubuntu"}
			tc.mutate(cfg)

			err := c.ConfigureKubelet(cfg)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			confPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
			if _, statErr := os.Stat(confPath); !os.IsNotExist(statErr) {
				t.Fatalf("kubelet env file should not be written, stat error: %v", statErr)
			}
		})
	}
}

func TestConfigureKubeletRHELUsesSysconfig(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{OSFamily: "rhel"}
	cfg.Provision.ProviderID = "redfish://host/sys/1"

	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "sysconfig", "kubelet"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--provider-id=redfish://host/sys/1") {
		t.Fatalf("provider-id missing from sysconfig kubelet file: %q", string(data))
	}
}

func TestConfigureKubeletMergesExistingArgs(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	confPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `# keep this
KUBELET_EXTRA_ARGS="--v=4 --provider-id=old --node-labels=keep=true,topology.kubernetes.io/zone=old"
`
	if err := os.WriteFile(confPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MachineConfig{}
	cfg.Provision.ProviderID = "redfish://host/sys/1"
	cfg.Provision.FailureDomain = "dc1-az1"

	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{"# keep this", "--v=4", "--provider-id=redfish://host/sys/1", "keep=true", "topology.kubernetes.io/zone=dc1-az1"} {
		if !strings.Contains(content, want) {
			t.Fatalf("merged kubelet env missing %q: %s", want, content)
		}
	}
	if count := strings.Count(content, "--node-labels="); count != 1 {
		t.Fatalf("expected exactly one node-labels flag, got %d: %s", count, content)
	}
	if strings.Contains(content, "--provider-id=old") || strings.Contains(content, "topology.kubernetes.io/zone=old") {
		t.Fatalf("merged kubelet env kept stale BOOTy-owned args: %s", content)
	}
}

func TestConfigureKubeletFlatcarUnsupported(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{OSFamily: "flatcar"}
	cfg.Provision.ProviderID = "redfish://host/sys/1"

	err := c.ConfigureKubelet(cfg)
	if err == nil {
		t.Fatal("expected unsupported flatcar kubelet layout error")
	}
	if !strings.Contains(err.Error(), "unsupported for flatcar") {
		t.Fatalf("expected flatcar unsupported error, got %v", err)
	}
}

func TestConfigureKubeletDetectsOSRelease(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	osReleasePath := filepath.Join(c.rootDir, "etc", "os-release")
	if err := os.MkdirAll(filepath.Dir(osReleasePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(osReleasePath, []byte("ID=fedora\nID_LIKE=\"rhel fedora\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MachineConfig{}
	cfg.Provision.ProviderID = "redfish://host/sys/1"

	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.rootDir, "etc", "sysconfig", "kubelet")); err != nil {
		t.Fatalf("expected sysconfig kubelet file: %v", err)
	}
}

func TestConfigureDNS(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	// Create etc/ directory (in production it exists from the mounted root).
	if err := os.MkdirAll(filepath.Join(c.rootDir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MachineConfig{}
	cfg.Network.DNSResolvers = "8.8.8.8,1.1.1.1"
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if content != "nameserver 8.8.8.8\nnameserver 1.1.1.1\n" {
		t.Errorf("resolv.conf = %q", content)
	}
}

func TestConfigureDNSEmpty(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	path := filepath.Join(c.rootDir, "etc", "resolv.conf")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("resolv.conf should not exist when no resolvers configured")
	}
}

func TestConfigureGRUB(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "quiet splash"
	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty grub config")
	}
	if strings.Contains(string(data), "ds=nocloud") {
		t.Fatalf("default-disabled cloud-init must not force NoCloud datasource in grub config: %s", data)
	}
	if _, err := os.Stat(filepath.Join(c.rootDir, "boot", "grub")); err != nil {
		t.Fatalf("expected boot grub dir: %v", err)
	}
}

func TestConfigureGRUBNoCloudDatasourceEnabled(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Enabled = true

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ds=nocloud") {
		t.Fatalf("expected enabled NoCloud datasource in grub config: %s", data)
	}
}

func TestConfigureGRUBConfigDriveDatasource(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Enabled = true
	cfg.Provision.CloudInit.Datasource = " configDrive "

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "ci.datasource=ConfigDrive") {
		t.Fatalf("expected ConfigDrive datasource in grub config: %s", content)
	}
	if strings.Contains(content, "ds=nocloud") {
		t.Fatalf("ConfigDrive grub config must not force NoCloud: %s", content)
	}
}

func TestConfigureGRUBCloudInitDisabledIgnoresDatasource(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.Provision.CloudInit.Datasource = "configdrive"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "ci.datasource=ConfigDrive") || strings.Contains(content, "ds=nocloud") {
		t.Fatalf("disabled cloud-init must not write datasource params: %s", content)
	}
}

func TestConfigureGRUBABWritesTargetRootPartLabel(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.ActiveSlot = config.ABSlotA
	cfg.Provision.AB.TargetSlot = config.ABTargetInactive
	cfg.Provision.ExtraKernelParams = "quiet splash root=/dev/old"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, "root=PARTLABEL=BOOTY-ROOT-B") {
		t.Fatalf("A/B root partlabel missing from GRUB config: %s", line)
	}
	if strings.LastIndex(line, "root=PARTLABEL=BOOTY-ROOT-B") < strings.LastIndex(line, "root=/dev/old") {
		t.Fatalf("A/B root partlabel must override earlier root args: %s", line)
	}
}

func TestConfigureGRUBSystemABWritesReadOnlyRoot(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.Scheme = config.ABSchemeSystemAB
	cfg.Provision.AB.TargetSlot = config.ABSlotA
	cfg.Provision.ExtraKernelParams = "quiet rw"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, "root=PARTLABEL=BOOTY-ROOT-A ro") {
		t.Fatalf("system-ab root ro params missing from GRUB config: %s", line)
	}
	if strings.LastIndex(line, " ro") < strings.LastIndex(line, " rw") {
		t.Fatalf("system-ab ro param must override earlier rw args: %s", line)
	}
}

func TestABRootKernelParamRejectsInvalidTarget(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.Mode = config.ImageModeAB
	cfg.Provision.AB.TargetSlot = "next"

	if _, err := abRootKernelParam(cfg); err == nil {
		t.Fatal("expected invalid target slot error")
	}
}

func TestCopyProvisionerFilesNotExist(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	if err := c.CopyProvisionerFiles(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCopyMachineFilesNotExist(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	if err := c.CopyMachineFiles(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunMachineCommandsNotExist(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	if err := c.RunMachineCommands(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetupMellanoxNoNICsFailsWhenRequested(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return false, nil })
	defer restore()
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected error when NUM_VFS is requested without Mellanox NICs")
	}
	if !strings.Contains(err.Error(), "no Mellanox NICs found") {
		t.Fatalf("error = %v, want no Mellanox NICs found", err)
	}
	if changed {
		t.Error("expected no firmware change")
	}
}

func TestSetupMellanoxPCIDetectionFailureFailsWhenRequested(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return false, fmt.Errorf("sysfs not available") })
	defer restore()
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected PCI detection error")
	}
	if !strings.Contains(err.Error(), "detect Mellanox PCI devices") {
		t.Fatalf("error = %v, want PCI detection context", err)
	}
	if changed {
		t.Error("expected no firmware change on PCI detection failure")
	}
}

type sequentialCommander struct {
	results []mockResult
	idx     int
	calls   []mockCall
}

func (s *sequentialCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, mockCall{name: name, args: args})
	if s.idx >= len(s.results) {
		return nil, nil
	}
	r := s.results[s.idx]
	s.idx++
	return r.output, r.err
}

func TestSetupMellanoxZeroVFsSkipsFirmwareChange(t *testing.T) {
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	cmd := &sequentialCommander{
		results: []mockResult{
			{output: []byte("mt4125_pciconf0\n"), err: nil},
			{output: []byte("Applied"), err: nil},
		},
	}
	mgr := disk.NewManager(cmd)
	c := &Configurator{disk: mgr, rootDir: t.TempDir()}

	changed, err := c.SetupMellanox(context.Background(), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatal("expected NUM_VFS=0 to skip firmware changes")
	}
	if cmd.idx != 0 {
		t.Fatalf("expected no mst device enumeration when NUM_VFS=0, got %d command calls", cmd.idx)
	}
}

func TestSetupMellanoxFirmwareChanged(t *testing.T) {
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	cmd := &sequentialCommander{
		results: []mockResult{
			{output: []byte("mt4125_pciconf0\n"), err: nil},
			{output: []byte("Applied"), err: nil},
		},
	}
	mgr := disk.NewManager(cmd)
	c := &Configurator{disk: mgr, rootDir: t.TempDir()}
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Error("expected firmware change when mstconfig succeeds")
	}
	if len(cmd.calls) != 2 {
		t.Fatalf("expected list and mstconfig calls, got %d", len(cmd.calls))
	}
	got := chrootShellCommand(t, cmd.calls[1])
	if !strings.Contains(got, "SRIOV_EN=True") || !strings.Contains(got, "NUM_OF_VFS=32") {
		t.Fatalf("mstconfig command = %q, want SRIOV_EN=True and NUM_OF_VFS=32", got)
	}
}

func chrootShellCommand(t *testing.T, call mockCall) string {
	t.Helper()
	if call.name != "chroot" || len(call.args) < 4 {
		t.Fatalf("call = %#v, want chroot root /bin/bash -c command", call)
	}
	return call.args[3]
}

func TestSetupMellanoxMissingMstDevicesFailsWhenRequested(t *testing.T) {
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	mgr := disk.NewManager(&sequentialCommander{
		results: []mockResult{
			{output: []byte("mstflint\n"), err: nil},
		},
	})
	c := &Configurator{disk: mgr, rootDir: t.TempDir()}
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected missing pciconf device error")
	}
	if !strings.Contains(err.Error(), "no Mellanox mst pciconf devices found") {
		t.Fatalf("error = %v, want missing pciconf context", err)
	}
	if changed {
		t.Error("expected no firmware change when no mst pciconf devices exist")
	}
}

func TestSetupMellanoxListMstFailsWhenRequested(t *testing.T) {
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	mgr := disk.NewManager(&sequentialCommander{
		results: []mockResult{
			{output: []byte("missing"), err: fmt.Errorf("ls: no such file or directory")},
		},
	})
	c := &Configurator{disk: mgr, rootDir: t.TempDir()}
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected /dev/mst listing error")
	}
	if !strings.Contains(err.Error(), "list Mellanox mst devices") {
		t.Fatalf("error = %v, want list Mellanox mst devices context", err)
	}
	if changed {
		t.Error("expected no firmware change when /dev/mst cannot be listed")
	}
}

func TestSetupMellanoxUnsafeMstDeviceFailsWhenRequested(t *testing.T) {
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	mgr := disk.NewManager(&sequentialCommander{
		results: []mockResult{
			{output: []byte("mt4125_pciconf0;touch\n"), err: nil},
		},
	})
	c := &Configurator{disk: mgr, rootDir: t.TempDir()}
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected invalid mst device error")
	}
	if !strings.Contains(err.Error(), "invalid mst device name") {
		t.Fatalf("error = %v, want invalid mst device context", err)
	}
	if changed {
		t.Error("expected no firmware change for unsafe mst device")
	}
}

func TestSetupMellanoxMstconfigFails(t *testing.T) {
	restore := SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	mgr := disk.NewManager(&sequentialCommander{
		results: []mockResult{
			{output: []byte("mt4125_pciconf0\n"), err: nil},
			{output: []byte("failed"), err: fmt.Errorf("mstconfig: exit 1")},
		},
	})
	c := &Configurator{disk: mgr, rootDir: t.TempDir()}
	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected mstconfig failure")
	}
	if !strings.Contains(err.Error(), "mstconfig /dev/mst/mt4125_pciconf0 failed") {
		t.Fatalf("error = %v, want mstconfig failure context", err)
	}
	if changed {
		t.Error("expected no firmware change when mstconfig fails")
	}
}

func TestFormatMstconfigErrorCompactsOutput(t *testing.T) {
	out := []byte(strings.Repeat("line one\nline two\t", 20))
	got := formatMstconfigError("/dev/mst/mt4125_pciconf0", out, fmt.Errorf("exit status 1"))
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Fatalf("error output was not normalized: %q", got)
	}
	if !strings.Contains(got, "...(truncated)") {
		t.Fatalf("error output was not truncated: %q", got)
	}
	if !strings.Contains(got, "exit status 1") {
		t.Fatalf("error is missing command failure: %q", got)
	}
}

func TestRemoveEFIBootEntriesGracefulOnMissing(t *testing.T) {
	oldCommand := hostCommandCombinedOutput
	hostCommandCombinedOutput = func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { hostCommandCombinedOutput = oldCommand })

	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	if err := c.RemoveEFIBootEntries(context.Background()); err != nil {
		t.Fatalf("expected nil error when efibootmgr unavailable, got: %v", err)
	}
}

func TestMountEFIVarsReturnsNilOnHost(t *testing.T) {
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

	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	err := c.MountEFIVars(context.Background())
	if err != nil {
		t.Fatalf("expected nil error on non-EFI test system, got: %v", err)
	}
}

func TestIsMountPoint(t *testing.T) {
	// /proc should be a mount point on Linux.
	if !isMountPoint("/proc") {
		t.Error("/proc should be a mount point")
	}
	// Nonexistent path should not be a mount point.
	if isMountPoint("/nonexistent/path/12345") {
		t.Error("/nonexistent/path/12345 should not be a mount point")
	}
}

func TestPartNumberFromDevice(t *testing.T) {
	tests := []struct {
		dev  string
		want string
	}{
		{"/dev/sda1", "1"},
		{"/dev/sda2", "2"},
		{"/dev/sda15", "15"},
		{"/dev/nvme0n1p1", "1"},
		{"/dev/nvme0n1p2", "2"},
		{"/dev/nvme0n1p15", "15"},
		{"/dev/vda3", "3"},
	}
	for _, tt := range tests {
		t.Run(tt.dev, func(t *testing.T) {
			if got := partNumberFromDevice(tt.dev); got != tt.want {
				t.Errorf("partNumberFromDevice(%q) = %q, want %q", tt.dev, got, tt.want)
			}
		})
	}
}

func TestCreateEFIBootEntry(t *testing.T) {
	old := efiRuntimeReady
	efiRuntimeReady = func() (bool, string) { return true, "" }
	t.Cleanup(func() { efiRuntimeReady = old })

	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	// Create the arch-specific shim EFI loader file so it's detected.
	efiDir := filepath.Join(c.rootDir, "boot", "efi", "EFI", "ubuntu")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shimName, _, err := efiLoaderNames(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(efiDir, shimName), []byte("shim"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd.setResult("efibootmgr -c", []byte("Boot0001* ubuntu"), nil)

	err = c.CreateEFIBootEntry(context.Background(), "/dev/sda", "/dev/sda1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasCommandName(cmd.calls, "chroot") {
		t.Fatalf("EFI boot entry creation should not chroot, calls=%#v", cmd.calls)
	}
	if !hasCommandCall(cmd.calls, "efibootmgr", "-c", "-d", "/dev/sda", "-p", "1", "-L", "ubuntu", "-l", `\EFI\ubuntu\`+shimName) {
		t.Fatalf("expected host efibootmgr create command, calls=%#v", cmd.calls)
	}
}

func TestCreateEFIBootEntryDebian(t *testing.T) {
	old := efiRuntimeReady
	efiRuntimeReady = func() (bool, string) { return true, "" }
	t.Cleanup(func() { efiRuntimeReady = old })

	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	efiDir := filepath.Join(c.rootDir, "boot", "efi", "EFI", "debian")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, grubName, err := efiLoaderNames(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(efiDir, grubName), []byte("grub"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd.setResult("efibootmgr -c", []byte("Boot0001* debian"), nil)

	err = c.CreateEFIBootEntry(context.Background(), "/dev/sda", "/dev/sda1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCommandCall(cmd.calls, "efibootmgr", "-c", "-d", "/dev/sda", "-p", "1", "-L", "debian", "-l", `\EFI\debian\`+grubName) {
		t.Fatalf("expected Debian efibootmgr create command, calls=%#v", cmd.calls)
	}
}

func TestCreateEFIBootEntryEmptyPartition(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	// Empty boot partition should skip without error.
	err := c.CreateEFIBootEntry(context.Background(), "/dev/sda", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateEFIBootEntryRejectsUnsafeDevicePaths(t *testing.T) {
	old := efiRuntimeReady
	efiRuntimeReady = func() (bool, string) { return true, "" }
	t.Cleanup(func() { efiRuntimeReady = old })

	tests := []struct {
		name     string
		diskDev  string
		bootPart string
		want     string
	}{
		{
			name:     "unsafe disk",
			diskDev:  "/dev/sda;touch /tmp/pwned",
			bootPart: "/dev/sda1",
			want:     "unsafe EFI boot disk device",
		},
		{
			name:     "unsafe partition",
			diskDev:  "/dev/sda",
			bootPart: "/dev/sda1 $(touch /tmp/pwned)",
			want:     "unsafe EFI boot partition device",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newMockCommander()
			c := newTestConfigurator(t, cmd)

			err := c.CreateEFIBootEntry(context.Background(), tt.diskDev, tt.bootPart)
			if err == nil {
				t.Fatal("CreateEFIBootEntry() error = nil, want unsafe device error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if hasCommandName(cmd.calls, "efibootmgr") {
				t.Fatalf("unsafe device path reached efibootmgr command, calls=%#v", cmd.calls)
			}
		})
	}
}

func TestEFILoaderPathUsesRemovableFallback(t *testing.T) {
	root := t.TempDir()
	loaderName, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	efiBootDir := filepath.Join(root, "boot", "efi", "EFI", "BOOT")
	if err := os.MkdirAll(efiBootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(efiBootDir, loaderName), []byte("efi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := efiLoaderPath(root, runtime.GOARCH)
	if err != nil {
		t.Fatalf("efiLoaderPath(): %v", err)
	}
	if want := `\EFI\BOOT\` + loaderName; got != want {
		t.Fatalf("efiLoaderPath() = %q, want %q", got, want)
	}
}

func TestGrubEFITarget(t *testing.T) {
	tests := []struct {
		arch string
		want string
	}{
		{arch: "amd64", want: "x86_64-efi"},
		{arch: "arm64", want: "arm64-efi"},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			got, err := grubEFITarget(tt.arch)
			if err != nil {
				t.Fatalf("grubEFITarget(%q): %v", tt.arch, err)
			}
			if got != tt.want {
				t.Fatalf("grubEFITarget(%q) = %q, want %q", tt.arch, got, tt.want)
			}
		})
	}
}

func TestGrubEFITargetRejectsUnsupportedArch(t *testing.T) {
	if _, err := grubEFITarget("riscv64"); err == nil {
		t.Fatal("grubEFITarget accepted unsupported arch")
	}
}

func TestInstallEFIFallbackLoader(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	target, err := grubEFITarget(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	wantCmd := fmt.Sprintf(
		"grub-install --target=%s --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --no-nvram --recheck /dev/sda",
		target,
	)

	if err := c.InstallEFIFallbackLoader(context.Background(), "/dev/sda", "/dev/sda2"); err != nil {
		t.Fatalf("InstallEFIFallbackLoader() error = %v", err)
	}
	if !hasCommandCall(cmd.calls, "chroot", c.rootDir, "/bin/bash", "-c", wantCmd) {
		t.Fatalf("missing fallback grub-install command, calls=%#v", cmd.calls)
	}
}

func TestInstallEFIFallbackLoaderUsesDebianBootloaderID(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	oldAssetDir := efiFallbackAssetDirectory
	efiFallbackAssetDirectory = t.TempDir()
	t.Cleanup(func() { efiFallbackAssetDirectory = oldAssetDir })

	target, err := grubEFITarget(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	efiDir := filepath.Join(c.rootDir, "boot", "efi", "EFI", "debian")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCmd := fmt.Sprintf(
		"grub-install --target=%s --efi-directory=/boot/efi --bootloader-id=debian --removable --no-nvram --recheck /dev/sda",
		target,
	)

	if err := c.InstallEFIFallbackLoader(context.Background(), "/dev/sda", "/dev/sda2"); err != nil {
		t.Fatalf("InstallEFIFallbackLoader() error = %v", err)
	}
	if !hasCommandCall(cmd.calls, "chroot", c.rootDir, "/bin/bash", "-c", wantCmd) {
		t.Fatalf("missing Debian fallback grub-install command, calls=%#v", cmd.calls)
	}
}

func TestInstallEFIFallbackLoaderReportsChrootFailure(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)
	cmd.setResult("chroot "+c.rootDir, []byte("grub missing"), fmt.Errorf("exit status 1"))

	err := c.InstallEFIFallbackLoader(context.Background(), "/dev/sda", "/dev/sda2")
	if err == nil {
		t.Fatal("InstallEFIFallbackLoader() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "install EFI fallback loader: grub missing") {
		t.Fatalf("error = %v, want grub output", err)
	}
}

func TestInstallEFIFallbackLoaderRejectsUnsafeDiskDevice(t *testing.T) {
	for _, diskDev := range []string{
		"/dev/sda;touch /tmp/pwned",
		"/dev/sda $(touch /tmp/pwned)",
		" /dev/sda",
		"/dev/../tmp/fake",
	} {
		t.Run(diskDev, func(t *testing.T) {
			cmd := newMockCommander()
			c := newTestConfigurator(t, cmd)

			err := c.InstallEFIFallbackLoader(context.Background(), diskDev, "/dev/sda2")
			if err == nil {
				t.Fatal("InstallEFIFallbackLoader() error = nil, want unsafe device error")
			}
			if !strings.Contains(err.Error(), "unsafe EFI fallback disk device") {
				t.Fatalf("error = %v, want unsafe disk-device message", err)
			}
			if hasCommandName(cmd.calls, "chroot") {
				t.Fatalf("unsafe disk device reached chroot command, calls=%#v", cmd.calls)
			}
		})
	}
}

func TestInstallEFIFallbackLoaderUsesBundledAsset(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	assetDir := t.TempDir()
	loaderName, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, loaderName), []byte("efi-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldAssetDir := efiFallbackAssetDirectory
	efiFallbackAssetDirectory = assetDir
	t.Cleanup(func() { efiFallbackAssetDirectory = oldAssetDir })
	oldHandoffID := newEFIFallbackHandoffID
	newEFIFallbackHandoffID = func() (string, error) { return "abcdef0123456789", nil }
	t.Cleanup(func() { newEFIFallbackHandoffID = oldHandoffID })

	grubDir := filepath.Join(c.rootDir, "boot", "grub")
	if err := os.MkdirAll(grubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grubDir, "grub.cfg"), []byte("menuentry test {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.InstallEFIFallbackLoader(context.Background(), "/dev/sda", "/dev/sda2"); err != nil {
		t.Fatalf("InstallEFIFallbackLoader() error = %v", err)
	}
	if hasCommandName(cmd.calls, "chroot") {
		t.Fatalf("bundled fallback path should not chroot, calls=%#v", cmd.calls)
	}
	efiBootDir := filepath.Join(c.rootDir, "boot", "efi", "EFI", "BOOT")
	if got, err := os.ReadFile(filepath.Join(efiBootDir, loaderName)); err != nil || string(got) != "efi-binary" {
		t.Fatalf("fallback loader = %q, %v; want copied bundled asset", string(got), err)
	}
	grubConfig, err := os.ReadFile(filepath.Join(efiBootDir, "grub.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	grubText := string(grubConfig)
	if !strings.Contains(grubText, "search --no-floppy --set=booty_root --file /etc/booty/grub-target-abcdef0123456789") {
		t.Fatalf("ESP grub.cfg missing marker search:\n%s", grubText)
	}
	if !strings.Contains(grubText, "configfile ($booty_root)/boot/grub/grub.cfg") {
		t.Fatalf("ESP grub.cfg missing target grub handoff:\n%s", grubText)
	}
	marker, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "booty", "grub-target-abcdef0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "/dev/sda2\n" {
		t.Fatalf("marker = %q, want root device", string(marker))
	}
}

func TestInstallEFIFallbackLoaderWithBundledAssetRequiresTargetGrubConfig(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	assetDir := t.TempDir()
	loaderName, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, loaderName), []byte("efi-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldAssetDir := efiFallbackAssetDirectory
	efiFallbackAssetDirectory = assetDir
	t.Cleanup(func() { efiFallbackAssetDirectory = oldAssetDir })

	err = c.InstallEFIFallbackLoader(context.Background(), "/dev/sda", "/dev/sda2")
	if err == nil {
		t.Fatal("InstallEFIFallbackLoader() error = nil, want missing target grub config error")
	}
	if !strings.Contains(err.Error(), "target GRUB config") {
		t.Fatalf("error = %v, want target GRUB config", err)
	}
	if hasCommandName(cmd.calls, "chroot") {
		t.Fatalf("missing target grub should not fall back to chroot, calls=%#v", cmd.calls)
	}
}

func devicePathCaseName(prefix, value string) string {
	if value == "" {
		return prefix + "_empty"
	}
	name := strings.NewReplacer("/", "_", "\n", `\n`, " ", "_").Replace(value)
	return prefix + "_" + name
}

func TestValidateChrootDevicePath(t *testing.T) {
	valid := []string{
		"/dev/sda",
		"/dev/sda1",
		"/dev/nvme0n1",
		"/dev/nvme0n1p1",
		"/dev/disk/by-id/nvme-eui.0000000000000001",
		"/dev/mapper/vg-root",
	}
	for _, value := range valid {
		t.Run(devicePathCaseName("valid", value), func(t *testing.T) {
			got, err := validateChrootDevicePath("device", value)
			if err != nil {
				t.Fatalf("validateChrootDevicePath(%q) error = %v", value, err)
			}
			if got != value {
				t.Fatalf("validateChrootDevicePath(%q) = %q", value, got)
			}
		})
	}

	invalid := []string{
		"",
		"/tmp/sda",
		"/dev/sda;reboot",
		"/dev/sda $(reboot)",
		"/dev/../tmp/sda",
		"/dev/disk/by-id/bad path",
		"/dev/sda\n/dev/sdb",
	}
	for _, value := range invalid {
		t.Run(devicePathCaseName("invalid", value), func(t *testing.T) {
			if _, err := validateChrootDevicePath("device", value); err == nil {
				t.Fatalf("validateChrootDevicePath(%q) error = nil, want rejection", value)
			}
		})
	}
}

func TestRunPostProvisionCmds(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	cmd.setResult("chroot "+c.rootDir, []byte("ok"), nil)

	cmds := []string{"apt update", "systemctl enable foo", ""}
	err := c.RunPostProvisionCmds(context.Background(), cmds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunPostProvisionCmdsError(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	cmd.setResult("chroot "+c.rootDir, nil, fmt.Errorf("command failed"))

	cmds := []string{"failing-command"}
	err := c.RunPostProvisionCmds(context.Background(), cmds)
	if err == nil {
		t.Fatal("expected error when command fails")
	}
}

func TestRunPostProvisionCmdsRejectsUnsafeCommand(t *testing.T) {
	cmd := newMockCommander()
	c := newTestConfigurator(t, cmd)

	cmds := []string{"echo ok && rm -rf /"}
	err := c.RunPostProvisionCmds(context.Background(), cmds)
	if err == nil {
		t.Fatal("expected validation error for unsafe command")
	}
	if !strings.Contains(err.Error(), "blocked shell token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewOrchestrator(t *testing.T) {
	cfg := &config.MachineConfig{Mode: "provision"}
	provider := &mockProvider{}
	mgr := disk.NewManager(newMockCommander())
	orch := NewOrchestrator(cfg, provider, mgr)
	if orch == nil {
		t.Fatal("expected non-nil orchestrator")
	}
}

func TestProvisionStepsContainEFIVars(t *testing.T) {
	cfg := &config.MachineConfig{}
	orch := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(newMockCommander()))
	steps := orch.provisionSteps()

	// Verify mount-efivarfs appears before remove-efi-entries.
	mountIdx, removeIdx := -1, -1
	for i, step := range steps {
		switch step.Name {
		case "mount-efivarfs":
			mountIdx = i
		case "remove-efi-entries":
			removeIdx = i
		}
	}
	if mountIdx == -1 || removeIdx == -1 {
		t.Fatal("mount-efivarfs and remove-efi-entries not found in steps")
	}
	if mountIdx >= removeIdx {
		t.Errorf("mount-efivarfs (idx %d) must come before remove-efi-entries (idx %d)", mountIdx, removeIdx)
	}

	// Verify total step count includes early provisioning input validation,
	// setup-nvme-namespaces, setup-raid, mount-boot, mount-shared-data, apply-sysexts,
	// OCI pre-pulls, EFI fallback install, and secureboot chain verification.
	if len(steps) != 44 {
		t.Errorf("expected 44 provisioning steps, got %d", len(steps))
	}
}

func TestProvisionStepsConfigureMountedRootBeforeSysexts(t *testing.T) {
	cfg := &config.MachineConfig{}
	orch := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(newMockCommander()))
	steps := orch.provisionSteps()

	mountIdx, bootIdx := -1, -1
	hostnameIdx, filesIdx, dnsIdx := -1, -1, -1
	sysextIdx, fstabIdx := -1, -1
	for i, step := range steps {
		switch step.Name {
		case "mount-root":
			mountIdx = i
		case "mount-boot":
			bootIdx = i
		case "set-hostname":
			hostnameIdx = i
		case "copy-provisioner-files":
			filesIdx = i
		case "configure-dns":
			dnsIdx = i
		case "apply-sysexts":
			sysextIdx = i
		case "write-fstab":
			fstabIdx = i
		}
	}
	if mountIdx == -1 || bootIdx == -1 || hostnameIdx == -1 || filesIdx == -1 || dnsIdx == -1 || sysextIdx == -1 || fstabIdx == -1 {
		t.Fatalf("missing mounted-root configuration steps")
	}
	if mountIdx >= bootIdx ||
		bootIdx >= hostnameIdx ||
		hostnameIdx >= filesIdx ||
		filesIdx >= dnsIdx ||
		dnsIdx >= sysextIdx ||
		sysextIdx >= fstabIdx {
		t.Fatalf(
			"unexpected mounted-root step order: mount=%d boot=%d hostname=%d files=%d dns=%d sysext=%d fstab=%d",
			mountIdx,
			bootIdx,
			hostnameIdx,
			filesIdx,
			dnsIdx,
			sysextIdx,
			fstabIdx,
		)
	}
}

func TestFirmwareChangedDefault(t *testing.T) {
	cfg := &config.MachineConfig{}
	orch := NewOrchestrator(cfg, &mockProvider{}, disk.NewManager(newMockCommander()))
	if orch.FirmwareChanged() {
		t.Error("expected firmwareChanged=false by default")
	}
}

func TestDeprovisionReportsInit(t *testing.T) {
	provider := &mockProvider{}
	cmd := newMockCommander()
	cfg := &config.MachineConfig{Mode: "hard"}
	orch := NewOrchestrator(cfg, provider, disk.NewManager(cmd))
	_ = orch.Deprovision(context.Background())
	if len(provider.statuses) == 0 {
		t.Fatal("expected at least one status report")
	}
	if provider.statuses[0].status != config.StatusInit {
		t.Errorf("expected first status to be init, got %s", provider.statuses[0].status)
	}
}

func TestDeprovisionSoftMode(t *testing.T) {
	provider := &mockProvider{}
	cmd := newMockCommander()
	cfg := &config.MachineConfig{Mode: "soft"}
	orch := NewOrchestrator(cfg, provider, disk.NewManager(cmd))
	err := orch.Deprovision(context.Background())
	if err == nil {
		return
	}
	found := false
	for _, s := range provider.statuses {
		if s.status == config.StatusError {
			found = true
		}
	}
	if !found {
		t.Error("expected error status to be reported on failure")
	}
}

func TestRedactURLs(t *testing.T) {
	tests := []struct {
		name string
		urls []string
		want []string
	}{
		{
			name: "no credentials",
			urls: []string{"http://example.com/image.gz"},
			want: []string{"http://example.com/image.gz"},
		},
		{
			name: "with credentials",
			urls: []string{"http://user:pass@registry.example.com/image:tag"},
			want: []string{"http://registry.example.com/image:tag"},
		},
		{
			name: "oci with credentials",
			urls: []string{"oci://user:pass@registry/repo:v1"},
			want: []string{"oci://registry/repo:v1"},
		},
		{
			name: "with query and fragment",
			urls: []string{"https://registry.example.com/image.raw?token=abc#frag"},
			want: []string{"https://registry.example.com/image.raw"},
		},
		{
			name: "empty",
			urls: []string{},
			want: []string{},
		},
		{
			name: "mixed",
			urls: []string{
				"http://example.com/plain.gz",
				"http://admin:secret@example.com/private.gz",
				"https://example.com/query.gz?token=abc#frag",
			},
			want: []string{
				"http://example.com/plain.gz",
				"http://example.com/private.gz",
				"https://example.com/query.gz",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURLs(tc.urls)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestRescueAction(t *testing.T) {
	tests := []struct {
		name       string
		rescueMode string
		wantType   rescue.Mode
	}{
		{"empty defaults to reboot", "", rescue.ModeReboot},
		{"invalid defaults to reboot", "bogus", rescue.ModeReboot},
		{"reboot mode", "reboot", rescue.ModeReboot},
		{"retry mode", "retry", rescue.ModeRetry},
		{"shell mode", "shell", rescue.ModeShell},
		{"wait mode", "wait", rescue.ModeWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Rescue.Mode = tc.rescueMode
			orch := &Orchestrator{
				cfg: cfg,
				log: slog.Default(),
			}
			state := &rescue.RetryState{}
			action := orch.RescueAction(state)
			if action.Type != tc.wantType {
				t.Errorf("RescueAction() type = %q, want %q", action.Type, tc.wantType)
			}
		})
	}
}

func TestIsSafeDeviceName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"sda", true},
		{"nvme0n1", true},
		{"vd.a", true},
		{"disk-0", true},
		{"DISK_A", true},
		{"", false},
		{"sd a", false},
		{"/dev/sda", false},
		{"sda;rm", false},
		{"sd$a", false},
		{"name`cmd`", false},
		{"../etc", false},
		{"sda\n", false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			if got := isSafeDeviceName(tc.name); got != tc.want {
				t.Errorf("isSafeDeviceName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestCopyTreeNormalFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create a simple file tree: dir/file.txt
	subDir := filepath.Join(src, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(context.Background(), src, dst); err != nil {
		t.Fatalf("copyTree() unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "sub", "file.txt"))
	if err != nil {
		t.Fatalf("expected copied file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("file content = %q, want %q", data, "hello")
	}
}

func TestCopyTreeSymlinksSkipped(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create a real file and a symlink pointing to it.
	realFile := filepath.Join(src, "real.txt")
	if err := os.WriteFile(realFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realFile, filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(context.Background(), src, dst); err != nil {
		t.Fatalf("copyTree() unexpected error: %v", err)
	}

	// Real file should exist, symlink should be skipped.
	if _, err := os.Stat(filepath.Join(dst, "real.txt")); err != nil {
		t.Errorf("expected real.txt to be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "link.txt")); !os.IsNotExist(err) {
		t.Error("expected symlink to be skipped, but it exists")
	}
}

func TestCopyTreePathTraversal(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create a normal file under src.
	if err := os.WriteFile(filepath.Join(src, "ok.txt"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Normal copy should succeed.
	if err := copyTree(context.Background(), src, dst); err != nil {
		t.Fatalf("copyTree() on clean tree should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "ok.txt")); err != nil {
		t.Errorf("expected ok.txt to be copied: %v", err)
	}

	// Create a symlink inside src that points outside.
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("escape"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}

	dst2 := t.TempDir()
	// Copy should skip the symlink (no error, but symlink not followed).
	if err := copyTree(context.Background(), src, dst2); err != nil {
		t.Fatalf("copyTree() should skip symlinks without error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst2, "escape")); !os.IsNotExist(err) {
		t.Error("expected symlink target to not be copied")
	}
}

func TestRescueAction_RetryExhausted(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Rescue.Mode = "retry"
	orch := &Orchestrator{
		cfg: cfg,
		log: slog.Default(),
	}
	state := &rescue.RetryState{Attempts: 3, MaxRetries: 3}
	action := orch.RescueAction(state)
	if action.Type != rescue.ModeReboot {
		t.Errorf("exhausted retry should reboot, got %q", action.Type)
	}
}
