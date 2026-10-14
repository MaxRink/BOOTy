//go:build e2e && linux

package e2e

// Tests for three previously uncovered pipeline steps:
//   - collect-inventory  (Gap 1)
//   - health-checks      (Gap 5)
//   - setup-nvme-namespaces (Gap 9)

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/provision"
)

// ---------------------------------------------------------------------------
// inventoryTrackingProvider wraps mockProvider and counts ReportInventory calls.
// Defined at package level (not inside a test function) so it satisfies
// config.Provider without the funlen linter penalising a per-function type.
// ---------------------------------------------------------------------------

type inventoryTrackingProvider struct {
	*mockProvider
	calls atomic.Int64
}

func (p *inventoryTrackingProvider) ReportInventory(ctx context.Context, data []byte) error {
	p.calls.Add(1)
	return p.mockProvider.ReportInventory(ctx, data)
}

func newInventoryTrackingProvider(cfg *config.MachineConfig) *inventoryTrackingProvider {
	return &inventoryTrackingProvider{mockProvider: newMockProvider(cfg)}
}

// failingInventoryProvider wraps mockProvider but always returns an error from
// ReportInventory. Used to verify that inventory failures are non-fatal and do
// not propagate to the caller as an inventory error.
type failingInventoryProvider struct {
	*mockProvider
	calls atomic.Int64
}

func (p *failingInventoryProvider) ReportInventory(_ context.Context, _ []byte) error {
	p.calls.Add(1)
	return fmt.Errorf("injected inventory failure")
}

func newFailingInventoryProvider(cfg *config.MachineConfig) *failingInventoryProvider {
	return &failingInventoryProvider{mockProvider: newMockProvider(cfg)}
}

// ---------------------------------------------------------------------------
// Gap 1: collect-inventory
// ---------------------------------------------------------------------------

// TestCollectInventoryDisabledE2E verifies that when InventoryEnabled=false the
// orchestrator skips ReportInventory entirely (zero calls) and still reaches the
// init status.
func TestCollectInventoryDisabledE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "inv-disabled-node",
		Network:  config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:   config.HealthConfig{Enabled: false},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}},
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newInventoryTrackingProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())
	if err != nil && strings.Contains(err.Error(), "inventory") {
		t.Fatalf("inventory step should not cause error when disabled: %v", err)
	}

	if provider.calls.Load() != 0 {
		t.Errorf("ReportInventory called %d time(s), want 0 when inventory is disabled",
			provider.calls.Load())
	}

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected at least one status report")
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
}

// TestCollectInventoryEnabledNonFatalE2E verifies that when InventoryEnabled=true
// the step is non-fatal. inventory.Collect() treats missing sysfs/procfs data as
// best-effort (returns nil error even without hardware), so the pipeline always
// continues past the inventory step.
func TestCollectInventoryEnabledNonFatalE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "inv-nonfatal-node",
		Network:  config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:   config.HealthConfig{Enabled: false},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}},
			Inventory: config.InventoryConfig{Enabled: true},
		},
	}
	provider := newFailingInventoryProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())
	if err != nil && strings.Contains(err.Error(), "inventory") {
		t.Fatalf("inventory failure should be non-fatal, got: %v", err)
	}

	if got := provider.calls.Load(); got != 1 {
		t.Errorf("ReportInventory called %d time(s), want 1", got)
	}

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected at least one status report")
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
	if len(statuses) < 2 {
		t.Error("expected pipeline to progress past collect-inventory (at least 2 status reports)")
	}
}

// TestCollectInventoryDoesNotCauseErrorE2E verifies that when InventoryEnabled=true
// and inventory collection succeeds (or is silently skipped in CI), no
// StatusError referencing inventory is ever emitted AND ReportInventory is
// called exactly once.
func TestCollectInventoryDoesNotCauseErrorE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "inv-report-node",
		Network:  config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:   config.HealthConfig{Enabled: false},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}},
			Inventory: config.InventoryConfig{Enabled: true},
		},
	}
	provider := newInventoryTrackingProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	_ = orch.Provision(context.Background())

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports")
	}
	// Inventory error is non-fatal; any failure must come from a later step.
	for _, s := range statuses {
		if s.Status == config.StatusError &&
			strings.Contains(s.Message, "inventory") {
			t.Errorf("inventory step should not report error status: %q", s.Message)
		}
	}
	// Pipeline progressed past collect-inventory when at least two steps ran.
	if len(statuses) < 2 {
		t.Error("expected pipeline to progress past collect-inventory (at least 2 status reports)")
	}
	// Positive assertion: ReportInventory must have been called exactly once.
	if got := provider.calls.Load(); got != 1 {
		t.Errorf("ReportInventory called %d time(s), want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Gap 5: health-checks
// ---------------------------------------------------------------------------

// TestHealthChecksDisabledE2E verifies that when HealthChecksEnabled=false
// the health-check step is skipped entirely and provisioning proceeds.
func TestHealthChecksDisabledE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "health-disabled",
		Network:  config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:   config.HealthConfig{Enabled: false, MinCPUs: 999999},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}},
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newMockProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())
	if err != nil && strings.Contains(err.Error(), "minimum-cpu") {
		t.Fatalf("health check ran despite HealthChecksEnabled=false: %v", err)
	}

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports")
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
	for _, s := range statuses {
		if s.Status == config.StatusError && strings.Contains(s.Message, "minimum-cpu") {
			t.Errorf("health check emitted error status despite being disabled: %q", s.Message)
		}
	}
}

// healthSkipHardware is the set of checks that depend on real kernel sysfs/
// thermal/NIC devices unavailable in a CI environment. Skipping them ensures
// TestHealthChecksPassE2E does not fail due to absent hardware.
const healthSkipHardware = "disk-presence,disk-ioerr,memory-ecc,nic-link-state,thermal-state"

// TestHealthChecksPassE2E verifies that the health check step passes when
// MinCPUs=0 and MinMemory=0 (thresholds disabled) and all hardware-dependent
// checks are skipped via HealthSkipChecks.
func TestHealthChecksPassE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "health-pass",
		Network:  config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health: config.HealthConfig{
			Enabled:     true,
			MinMemoryGB: 0,
			MinCPUs:     0,
			SkipChecks:  healthSkipHardware,
		},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}},
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newMockProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports")
	}
	// With zero thresholds and hardware checks skipped, no critical failure should occur.
	for _, s := range statuses {
		if s.Status == config.StatusError &&
			strings.Contains(s.Message, "critical health check") {
			t.Errorf("health checks should not fail with zero thresholds: %q", s.Message)
		}
	}
	if err != nil && strings.Contains(err.Error(), "health") {
		t.Errorf("health-check step should not cause error with zero thresholds: %v", err)
	}
}

// TestHealthChecksCriticalFailureE2E verifies that when a critical health check
// fails (HealthMinCPUs set impossibly high), the orchestrator returns an error
// and reports StatusError mentioning minimum-cpu.
func TestHealthChecksCriticalFailureE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Network: config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health: config.HealthConfig{
			Enabled:    true,
			MinCPUs:    999999,
			SkipChecks: healthSkipHardware,
		},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newMockProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())
	if err == nil {
		t.Fatal("expected provision to fail due to critical health check failure")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "minimum-cpu") {
		t.Errorf("error should mention minimum-cpu check: %v", err)
	}

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports")
	}
	last := statuses[len(statuses)-1]
	if last.Status != config.StatusError {
		t.Errorf("last status = %q, want error", last.Status)
	}
}

// ---------------------------------------------------------------------------
// Gap 9: setup-nvme-namespaces
// ---------------------------------------------------------------------------

// TestSetupNVMeNamespacesSkippedWhenEmptyE2E verifies that when
// NVMeNamespaces="" the step is skipped and no nvme commands are run.
func TestSetupNVMeNamespacesSkippedWhenEmptyE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "nvme-skip",
		Network:  config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:   config.HealthConfig{Enabled: false},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}},
			Disk:      config.DiskConfig{NVMeNamespaces: ""},
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newMockProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports — provisioning may not have started")
	}
	// Pipeline must have at least reached init, confirming the nvme step was
	// encountered (and skipped) rather than the pipeline aborting before it.
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init — pipeline did not progress to nvme step", statuses[0].Status)
	}
	// The pipeline must have progressed past setup-nvme-namespaces into detect-disk
	// (or beyond). An error from setup-nvme-namespaces itself would mean the skip
	// logic did not work.
	if err != nil && strings.Contains(err.Error(), "setup-nvme-namespaces") {
		t.Errorf("pipeline should have skipped setup-nvme-namespaces, got: %v", err)
	}
	for _, c := range cmd.getCalls() {
		if c.Name == "nvme" {
			t.Errorf("nvme should NOT be called when NVMeNamespaces is empty: %s", c)
		}
	}
}

// TestSetupNVMeNamespacesInvalidConfigE2E verifies that a malformed JSON config
// causes a parse error at the nvme namespace layout step, propagating as a
// fatal provisioning failure with the specific error text.
func TestSetupNVMeNamespacesInvalidConfigE2E(t *testing.T) {
	t.Cleanup(provision.SetEFIFirmwarePathForTest(filepath.Join(t.TempDir(), "no-efi"))) // BIOS host: never touch the runner NVRAM
	cfg := &config.MachineConfig{
		Network: config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:  config.HealthConfig{Enabled: false},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}, AllowInsecureHTTP: new(true)},
			Disk:      config.DiskConfig{NVMeNamespaces: "{not valid json"},
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newMockProvider(cfg)
	cmd := newMockCommander()
	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))

	err := orch.Provision(context.Background())
	if err == nil {
		t.Fatal("expected error from invalid NVMe namespace config")
	}
	errMsg := err.Error()
	// The orchestrator wraps the JSON parse error with "parsing nvme namespace layout".
	if !strings.Contains(errMsg, "parsing nvme namespace layout") {
		t.Errorf("error should reference NVMe namespace layout parsing, got: %v", err)
	}

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports")
	}
	last := statuses[len(statuses)-1]
	if last.Status != config.StatusError {
		t.Errorf("last status = %q, want error", last.Status)
	}
}

// TestSetupNVMeNamespacesCommandsCalledE2E verifies that with a valid NVMe
// namespace config the orchestrator calls nvme id-ctrl, list-ns, create-ns and
// attach-ns against the configured controller.
func TestSetupNVMeNamespacesCommandsCalledE2E(t *testing.T) {
	t.Cleanup(provision.SetEFIFirmwarePathForTest(filepath.Join(t.TempDir(), "no-efi"))) // BIOS host: never touch the runner NVRAM
	// Minimal valid config: one controller, one 100% namespace.
	nvmeCfg := `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`

	cfg := &config.MachineConfig{
		Network: config.NetworkConfig{DNSResolvers: "8.8.8.8"},
		Health:  config.HealthConfig{Enabled: false},
		Provision: config.ProvisionConfig{
			TargetOS:  config.TargetOSLinux,
			Image:     config.ImageConfig{URLs: []string{startE2ERawImageServer(t)}, AllowInsecureHTTP: new(true)},
			Disk:      config.DiskConfig{NVMeNamespaces: nvmeCfg},
			Inventory: config.InventoryConfig{Enabled: false},
		},
	}
	provider := newMockProvider(cfg)
	cmd := newMockCommander()

	// id-ctrl: report nn>1 (multi-namespace) and tnvmcap (1 TiB).
	cmd.set("nvme id-ctrl", []byte(
		`{"mn":"Mock NVMe","cntlid":1,"nn":32,"tnvmcap":1000204886016}`,
	), nil)
	// list-ns: no existing namespaces (clean slate).
	cmd.set("nvme list-ns", []byte(`{"nsid_list":[]}`), nil)
	// create-ns: return nsid 1.
	cmd.set("nvme create-ns", []byte("create-ns: Success, created nsid:1\n"), nil)
	// attach-ns: success.
	cmd.set("nvme attach-ns", []byte(""), nil)

	orch := provision.NewOrchestrator(cfg, provider, disk.NewManager(cmd))
	err := orch.Provision(context.Background())
	if err != nil && strings.Contains(err.Error(), "nvme namespace") {
		t.Fatalf("pipeline failed at the NVMe namespace step itself: %v", err)
	}

	calls := cmd.getCalls()
	var idCtrlCalled, listNSCalled, createNSCalled, attachNSCalled bool
	for _, c := range calls {
		if c.Name != "nvme" || len(c.Args) == 0 {
			continue
		}
		switch c.Args[0] {
		case "id-ctrl":
			idCtrlCalled = true
		case "list-ns":
			listNSCalled = true
			if !strings.Contains(strings.Join(c.Args, " "), "--all") {
				t.Errorf("list-ns for the reset must include inactive namespaces: %v", c.Args)
			}
		case "create-ns":
			createNSCalled = true
		case "attach-ns":
			attachNSCalled = true
			if !strings.HasSuffix(strings.Join(c.Args, " "), "-c 1") {
				t.Errorf("attach-ns must use the controller ID from id-ctrl: %v", c.Args)
			}
		}
	}
	if !idCtrlCalled {
		t.Error("expected nvme id-ctrl to be called")
	}
	if !listNSCalled {
		t.Error("expected nvme list-ns to be called")
	}
	if !createNSCalled {
		t.Error("expected nvme create-ns to be called")
	}
	if !attachNSCalled {
		t.Error("expected nvme attach-ns to be called")
	}

	assertNVMeOrder(t, calls)
	assertNVMeControllerRef(t, calls, "/dev/nvme0")

	// DiskDevice must be set to the first created namespace.
	if cfg.Provision.Disk.Device == "" {
		t.Error("expected cfg.Provision.Disk.Device to be set after namespace creation")
	}
	if !strings.HasPrefix(cfg.Provision.Disk.Device, "/dev/nvme0n") {
		t.Errorf("Provision.Disk.Device = %q, want /dev/nvme0n<N>", cfg.Provision.Disk.Device)
	}
}

// assertNVMeOrder verifies that NVMe subcommands appear in the expected
// provisioning sequence: id-ctrl → list-ns → create-ns → attach-ns.
func assertNVMeOrder(t *testing.T, calls []cmdCall) {
	t.Helper()
	var nvmeSubs []string
	for _, c := range calls {
		if c.Name == "nvme" && len(c.Args) > 0 {
			nvmeSubs = append(nvmeSubs, c.Args[0])
		}
	}
	want := []string{"id-ctrl", "list-ns", "create-ns", "attach-ns"}
	idx := 0
	for _, sub := range nvmeSubs {
		if idx < len(want) && sub == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("NVMe commands not in expected order; got %v, want subsequence %v", nvmeSubs, want)
	}
}

// assertNVMeControllerRef verifies that id-ctrl, create-ns and attach-ns each
// reference the configured controller path in their arguments.
func assertNVMeControllerRef(t *testing.T, calls []cmdCall, controller string) {
	t.Helper()
	checkSubs := map[string]bool{"id-ctrl": true, "create-ns": true, "attach-ns": true}
	for _, c := range calls {
		if c.Name != "nvme" || len(c.Args) == 0 || !checkSubs[c.Args[0]] {
			continue
		}
		found := false
		for _, arg := range c.Args {
			if strings.HasPrefix(arg, controller) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("nvme %s: expected argument referencing %s, got %v", c.Args[0], controller, c.Args)
		}
	}
}
