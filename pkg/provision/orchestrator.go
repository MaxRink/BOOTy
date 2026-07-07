//go:build linux

package provision

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	ociname "github.com/google/go-containerregistry/pkg/name"

	"github.com/telekom/BOOTy/pkg/cloudinit"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/efi"
	"github.com/telekom/BOOTy/pkg/executil"
	"github.com/telekom/BOOTy/pkg/firmware"
	"github.com/telekom/BOOTy/pkg/health"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/inventory"
	"github.com/telekom/BOOTy/pkg/network"
	networkpersist "github.com/telekom/BOOTy/pkg/network/persist"
	"github.com/telekom/BOOTy/pkg/rescue"
	"github.com/telekom/BOOTy/pkg/secureboot"
	"github.com/telekom/BOOTy/pkg/telemetry"
)

var readProcCmdline = func() ([]byte, error) {
	return os.ReadFile("/proc/cmdline")
}

const ociPreflightProbeTimeout = 10 * time.Second

var (
	evalRootSymlinks   = filepath.EvalSymlinks
	collectFirmwareFn  = firmware.Collect
	validateFirmwareFn = firmware.Validate
	loadCheckpointFn   = LoadCheckpoint
	probeOCIReference  = image.ProbeOCIReference
	secureBootRoot     = newroot
	mountBootPart      = func(ctx context.Context, mgr *disk.Manager, device, mountpoint string) error {
		return mgr.MountPartition(ctx, device, mountpoint)
	}
	mountReadOnlyPart = func(ctx context.Context, mgr *disk.Manager, device, mountpoint string) error {
		return mgr.MountPartitionReadOnly(ctx, device, mountpoint)
	}
	mountSharedDataPart = func(ctx context.Context, mgr *disk.Manager, device, mountpoint string) error {
		return mgr.MountPartition(ctx, device, mountpoint)
	}
	unmountSharedDataPart = func(mgr *disk.Manager, mountpoint string) error {
		return mgr.Unmount(mountpoint)
	}
	streamRootImageFn = image.StreamRoot
	sysBlockRoot      = "/sys/class/block"
)

const sharedDataSeedInProgressMarker = ".booty-shared-data-seed-in-progress"
const sharedDataSeedInProgressContent = "BOOTy shared-data seed in progress\n"
const imageSignatureChecksumRequiredMessage = "image signature URL (IMAGE_SIGNATURE_URL) requires image checksum (IMAGE_CHECKSUM) because signature verification and image streaming use separate downloads"

// Step represents a named provisioning step.
type Step struct {
	Name string
	Fn   func(ctx context.Context) error
}

// HealthReporter is an optional provider capability for reporting health check results.
type HealthReporter interface {
	ReportHealthChecks(context.Context, []health.CheckResult) error
}

// Orchestrator runs the full provisioning pipeline.
type Orchestrator struct {
	cfg       *config.MachineConfig
	provider  config.Provider
	disk      *disk.Manager
	config    *Configurator
	collector *telemetry.Collector
	emitter   *telemetry.Emitter
	log       *slog.Logger

	// Runtime state set during provisioning.
	targetDisk       string
	rootPartition    string
	bootPartition    string
	sharedMounts     []string
	bestImageURL     string // resolved by verify-image, reused by stream-image
	nvmeTargetDevice string
	firmwareChanged  bool // true if any step changed firmware values requiring hard reboot
}

// NewOrchestrator creates an Orchestrator with the given dependencies.
func NewOrchestrator(cfg *config.MachineConfig, provider config.Provider, diskMgr *disk.Manager) *Orchestrator {
	return &Orchestrator{
		cfg:       cfg,
		provider:  provider,
		disk:      diskMgr,
		config:    NewConfigurator(diskMgr),
		collector: telemetry.NewCollector(),
		emitter:   telemetry.NewEmitter(),
		log:       slog.Default().With("component", "provision"),
	}
}

// provisionSteps returns the ordered list of provisioning steps.
func (o *Orchestrator) provisionSteps() []Step {
	return []Step{
		{"report-init", o.reportInit},
		{"collect-inventory", o.collectInventory},
		{"collect-firmware", o.collectFirmware},
		{"health-checks", o.runHealthChecks},
		{"validate-provision-inputs", o.validateProvisionInputs},
		{"verify-image", o.verifyImageSignature},
		{"disable-lvm", o.disableLVM},
		{"stop-raid", o.stopRAID},
		{"mount-efivarfs", o.mountEFIVars},
		{"remove-efi-entries", o.removeEFIBootEntries},
		{"setup-mellanox", o.setupMellanox},
		{"setup-nvme-namespaces", o.setupNVMeNamespaces},
		{"setup-raid", o.setupRAID},
		{"detect-disk", o.detectDisk},
		{"wipe-disks", o.wipeOrSecureEraseDisks},
		{"apply-partition-layout", o.applyPartitionLayout},
		{"stream-image", o.streamImage},
		{"partprobe", o.partprobe},
		{"parse-partitions", o.parsePartitions},
		{"check-filesystem", o.checkFilesystem},
		{"enable-lvm", o.enableLVM},
		{"mount-root", o.mountRoot},
		{"mount-boot", o.mountBoot},
		{"mount-shared-data", o.mountSharedData},
		{"set-hostname", o.setHostname},
		{"copy-provisioner-files", o.copyProvisionerFiles},
		{"configure-dns", o.configureDNS},
		{"apply-sysexts", o.applySysexts},
		{"write-fstab", o.writeFstabStep},
		{"setup-chroot-binds", o.setupChrootBinds},
		{"grow-partition", o.growPartition},
		{"resize-filesystem", o.resizeFilesystem},
		{"configure-kubelet", o.configureKubelet},
		{"prepare-oci-prepulls", o.prepareOCIPrePulls},
		{"configure-grub", o.configureGRUB},
		{"install-efi-fallback", o.installEFIFallbackLoader},
		// Validate the final installed boot artifacts after BOOTy has applied
		// GRUB and EFI fallback mutations, before firmware boot entries point to them.
		{"verify-secureboot-chain", o.verifySecureBootChain},
		{"inject-cloudinit", o.injectCloudInit},
		{"copy-machine-files", o.copyMachineFiles},
		{"run-machine-commands", o.runMachineCommands},
		{"run-post-provision-cmds", o.runPostProvisionCmds},
		{"create-efi-boot-entry", o.createEFIBootEntry},
		{"teardown-chroot", o.teardownChroot},
		{"report-success", o.reportSuccess},
	}
}

// Provision runs all provisioning steps sequentially.
func (o *Orchestrator) Provision(ctx context.Context) error {
	// Ensure final telemetry is always flushed before exit/reboot.
	// Uses context.Background() because ctx may be canceled during teardown/error.
	defer o.flushTelemetry(context.Background())
	steps := o.provisionSteps()

	cp, err := o.loadOrCreateCheckpoint()
	if err != nil {
		return err
	}
	o.restoreCheckpointDerivedState(cp)

	// stateSteps must always re-run on resume because they rebuild in-memory
	// runtime fields that later steps depend on (firmwareChanged, bestImageURL,
	// targetDisk, LVM devices, rootPartition/bootPartition, efivarfs, sharedMounts,
	// chroot bind mounts, teardown state) or revalidate safety preconditions
	// before destructive operations.
	stateSteps := resumeStateSteps()

	for i, step := range steps {
		_, mustRun := stateSteps[step.Name]
		if cp.IsCompleted(step.Name) && !mustRun {
			o.log.Info("skipping completed step", "step", step.Name)
			continue
		}
		o.log.Info("provisioning step", "step", step.Name, "index", i+1, "total", len(steps))
		if err := o.executeStep(ctx, step, cp); err != nil {
			return err
		}
	}

	if rmErr := cp.Remove(); rmErr != nil {
		o.log.Warn("failed to remove checkpoint", "error", rmErr)
	}
	return nil
}

func resumeStateSteps() map[string]struct{} {
	return map[string]struct{}{
		"validate-provision-inputs": {},
		"verify-image":              {},
		"mount-efivarfs":            {},
		"setup-mellanox":            {},
		"detect-disk":               {},
		"parse-partitions":          {},
		"enable-lvm":                {},
		"mount-root":                {},
		"mount-boot":                {},
		"mount-shared-data":         {},
		"setup-chroot-binds":        {},
		"teardown-chroot":           {},
	}
}

// loadOrCreateCheckpoint loads an existing checkpoint when BOOTY_RESUME is set,
// or returns a fresh checkpoint. Only checkpoints created via BOOTY_RESUME
// persist to disk; otherwise Save/Remove are no-ops.
func (o *Orchestrator) loadOrCreateCheckpoint() (*Checkpoint, error) {
	if enabled, _ := strconv.ParseBool(os.Getenv("BOOTY_RESUME")); enabled {
		cp, cpErr := loadCheckpointFn()
		if cpErr != nil && !errors.Is(cpErr, ErrNoCheckpoint) {
			o.log.Error("failed to load checkpoint", "error", cpErr)
			return nil, fmt.Errorf("load checkpoint: %w", cpErr)
		}
		if cp != nil {
			return cp, nil
		}
		return &Checkpoint{persist: true}, nil
	}
	return &Checkpoint{}, nil
}

func (o *Orchestrator) restoreCheckpointDerivedState(cp *Checkpoint) {
	if cp == nil || o.cfg.Provision.Disk.Device != "" {
		return
	}
	if len(o.cfg.Provision.Disk.RAID) > 0 && cp.RAIDTargetDevice != "" {
		o.cfg.Provision.Disk.Device = cp.RAIDTargetDevice
		o.targetDisk = cp.RAIDTargetDevice
		o.log.Info("restored raid target device from checkpoint", "device", cp.RAIDTargetDevice)
		return
	}
	if o.cfg.Provision.Disk.NVMeNamespaces != "" && cp.NVMeTargetDevice != "" {
		o.cfg.Provision.Disk.Device = cp.NVMeTargetDevice
		o.nvmeTargetDevice = cp.NVMeTargetDevice
		o.log.Info("restored nvme target device from checkpoint", "device", cp.NVMeTargetDevice)
	}
}

// executeStep runs a single provisioning step with optional retry, updating
// the checkpoint on success or failure.
func (o *Orchestrator) executeStep(ctx context.Context, step Step, cp *Checkpoint) error {
	var err error
	if policy, ok := DefaultPolicies[step.Name]; ok {
		err = WithRetry(ctx, step.Name, policy, step.Fn)
	} else {
		err = step.Fn(ctx)
	}

	if err != nil {
		msg := fmt.Sprintf("step %s failed: %v", step.Name, err)
		o.log.Error("provisioning step failed", "step", step.Name, "error", err)
		cp.Errors = append(cp.Errors, msg)
		cp.FailureCount++
		if saveErr := cp.Save(); saveErr != nil {
			o.log.Warn("failed to save checkpoint", "error", saveErr)
		}
		DumpDebugState(step.Name)
		dumpConfig(o.cfg)
		if reportErr := o.provider.ReportStatus(ctx, config.StatusError, msg); reportErr != nil {
			o.log.Error("failed to report error status", "error", reportErr)
		}
		return fmt.Errorf("provision step %s: %w", step.Name, err)
	}

	o.recordCheckpointDerivedState(step.Name, cp)
	cp.MarkStep(step.Name)
	if saveErr := cp.Save(); saveErr != nil {
		o.log.Error("failed to save checkpoint", "error", saveErr)
		return fmt.Errorf("save checkpoint after step %s: %w", step.Name, saveErr)
	}
	return nil
}

func (o *Orchestrator) recordCheckpointDerivedState(stepName string, cp *Checkpoint) {
	if cp == nil {
		return
	}
	switch stepName {
	case "setup-nvme-namespaces":
		if o.nvmeTargetDevice != "" {
			cp.NVMeTargetDevice = o.nvmeTargetDevice
		}
	case "setup-raid":
		if len(o.cfg.Provision.Disk.RAID) > 0 && o.cfg.Provision.Disk.Device != "" {
			cp.RAIDTargetDevice = o.cfg.Provision.Disk.Device
		}
	}
}

// RescueConfig returns the normalized rescue config derived from machine config.
func (o *Orchestrator) RescueConfig() *rescue.Config {
	cfg := &rescue.Config{Mode: rescue.ModeReboot}
	if o.cfg.Rescue.Mode != "" {
		mode, err := rescue.ParseMode(o.cfg.Rescue.Mode)
		if err != nil {
			o.log.Warn("invalid rescue mode, defaulting to reboot", "mode", o.cfg.Rescue.Mode, "error", err)
		} else {
			cfg.Mode = mode
		}
	}
	if o.cfg.Rescue.SSHPubKey != "" {
		cfg.SSHKeys = []string{o.cfg.Rescue.SSHPubKey}
	}
	cfg.PasswordHash = o.cfg.Rescue.PasswordHash
	if o.cfg.Rescue.Timeout > 0 {
		cfg.ShellTimeout = time.Duration(o.cfg.Rescue.Timeout) * time.Second
	}
	cfg.AutoMountDisks = o.cfg.Rescue.AutoMountDisks
	cfg.ApplyDefaults()
	return cfg
}

// RescueAction returns the rescue action to take after a provisioning failure,
// based on the machine config's RescueMode setting.
func (o *Orchestrator) RescueAction(state *rescue.RetryState) rescue.Action {
	cfg := o.RescueConfig()
	return rescue.Decide(cfg, state)
}

func (o *Orchestrator) reportInit(ctx context.Context) error {
	return o.provider.ReportStatus(ctx, config.StatusInit, "provisioning started")
}

func (o *Orchestrator) collectInventory(ctx context.Context) error {
	if !o.cfg.Provision.Inventory.Enabled {
		o.log.Info("Hardware inventory collection disabled, skipping")
		return nil
	}

	inv, err := inventory.Collect()
	if err != nil {
		o.log.Warn("Hardware inventory collection failed", "error", err)
		return nil // non-fatal
	}

	data, err := json.Marshal(inv)
	if err != nil {
		o.log.Warn("Failed to marshal hardware inventory", "error", err)
		return nil // non-fatal
	}

	o.log.Info("Hardware inventory collected",
		"cpus", len(inv.CPUs),
		"disks", len(inv.Disks),
		"nics", len(inv.NICs),
		"pci_devices", len(inv.PCIDevices))

	if err := o.provider.ReportInventory(ctx, data); err != nil {
		o.log.Warn("Failed to report hardware inventory", "error", err)
	}
	return nil
}

func (o *Orchestrator) collectFirmware(ctx context.Context) error {
	if !o.cfg.Provision.Firmware.Enabled {
		o.log.Info("Firmware reporting disabled, skipping")
		return nil
	}

	report, err := collectFirmwareFn()
	if err != nil {
		// Collection is best-effort: missing sysfs entries are common in
		// virtual environments, so we log and continue provisioning.
		o.log.Warn("Firmware collection failed, continuing", "error", err)
		return nil
	}

	var failures []firmware.ValidationResult
	if o.cfg.Provision.Firmware.MinBIOS != "" || o.cfg.Provision.Firmware.MinBMC != "" {
		policy := firmware.Policy{
			MinBIOSVersion: o.cfg.Provision.Firmware.MinBIOS,
			MinBMCVersion:  o.cfg.Provision.Firmware.MinBMC,
		}
		results := validateFirmwareFn(report, policy)
		for _, r := range results {
			if r.Status == "fail" {
				failures = append(failures, r)
				o.log.Warn("Firmware validation", "name", r.Name, "status", r.Status, "message", r.Message)
			} else {
				o.log.Info("Firmware validation", "name", r.Name, "status", r.Status, "message", r.Message)
			}
		}
	}

	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal firmware report: %w", err)
	}

	if err := o.provider.ReportFirmware(ctx, data); err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("firmware validation failed: %s", formatFirmwareFailures(failures))
	}
	return nil
}

func formatFirmwareFailures(failures []firmware.ValidationResult) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, fmt.Sprintf("%s: %s", failure.Name, failure.Message))
	}
	return strings.Join(parts, "; ")
}

func (o *Orchestrator) setHostname(_ context.Context) error {
	if o.cfg.Hostname == "" {
		return nil
	}
	return o.config.SetHostname(o.cfg)
}

func (o *Orchestrator) copyProvisionerFiles(ctx context.Context) error {
	return o.config.CopyProvisionerFiles(ctx)
}

func (o *Orchestrator) configureDNS(_ context.Context) error {
	if err := o.config.ConfigureDNS(o.cfg); err != nil {
		return err
	}
	return o.persistNetworkConfig()
}

func (o *Orchestrator) persistNetworkConfig() error {
	if !o.cfg.PersistNetwork {
		return nil
	}
	family, err := networkpersist.ParseOSFamily(o.cfg.OSFamily)
	if err != nil {
		return fmt.Errorf("network persistence: %w", err)
	}
	cfg, err := o.targetNetworkConfig()
	if err != nil {
		return fmt.Errorf("network persistence: %w", err)
	}
	if err := networkpersist.Write(o.config.rootDir, family, cfg); err != nil {
		return fmt.Errorf("network persistence: %w", err)
	}
	o.log.Info("persisted target network configuration", "osFamily", family)
	return nil
}

func (o *Orchestrator) targetNetworkConfig() (*networkpersist.NetworkConfig, error) {
	cfg := &networkpersist.NetworkConfig{
		DNS: networkpersist.DNSConfig{
			Servers: splitCommaValues(o.cfg.Network.DNSResolvers),
		},
	}

	addPersistentBond(cfg, o.cfg)
	if err := addPersistentInterface(cfg, o.cfg); err != nil {
		return nil, err
	}
	if err := addPersistentVLANs(cfg, o.cfg.Network.VLAN.Config); err != nil {
		return nil, err
	}
	if len(cfg.Bonds) > 0 && cfg.Bonds[0].Address == "" && len(cfg.VLANs) == 0 {
		return nil, fmt.Errorf("bond network persistence requires static ip or vlan config")
	}
	if len(cfg.Interfaces) == 0 && len(cfg.Bonds) == 0 && len(cfg.VLANs) == 0 {
		return nil, fmt.Errorf("no interface, bond, or vlan configured for target network")
	}
	return cfg, nil
}

func addPersistentBond(target *networkpersist.NetworkConfig, cfg *config.MachineConfig) {
	members := splitCommaValues(cfg.Network.Bond.Interfaces)
	if len(members) == 0 {
		return
	}
	mode := normalizePersistentBondMode(cfg.Network.Bond.Mode)
	address := strings.TrimSpace(cfg.Network.Static.IP)
	gateway := strings.TrimSpace(cfg.Network.Static.Gateway)
	if address == "" {
		gateway = ""
	}
	target.Bonds = append(target.Bonds, networkpersist.BondConfig{
		Name:    "bond0",
		Members: members,
		Mode:    mode,
		Address: address,
		Gateway: gateway,
	})
}

func normalizePersistentBondMode(mode string) string {
	trimmed := strings.TrimSpace(mode)
	switch strings.ToLower(trimmed) {
	case "", "lacp", "802.3ad":
		return "802.3ad"
	case "balance-rr", "active-backup", "balance-xor":
		return strings.ToLower(trimmed)
	default:
		return trimmed
	}
}

func addPersistentInterface(target *networkpersist.NetworkConfig, cfg *config.MachineConfig) error {
	if len(target.Bonds) > 0 {
		return nil
	}
	iface := strings.TrimSpace(cfg.Network.Static.Iface)
	address := strings.TrimSpace(cfg.Network.Static.IP)
	if address != "" && iface == "" {
		return fmt.Errorf("static iface is required when persisting static ip without a bond")
	}
	if iface == "" {
		return nil
	}
	gateway := strings.TrimSpace(cfg.Network.Static.Gateway)
	if address == "" {
		gateway = ""
	}
	target.Interfaces = append(target.Interfaces, networkpersist.InterfaceConfig{
		Name:    iface,
		DHCP:    address == "",
		Address: address,
		Gateway: gateway,
	})
	return nil
}

func addPersistentVLANs(target *networkpersist.NetworkConfig, spec string) error {
	vlans, err := network.ParseVLANs(spec)
	if err != nil {
		return fmt.Errorf("invalid VLAN persistence config: %w", err)
	}
	for _, vlan := range vlans {
		if strings.TrimSpace(vlan.Gateway) != "" {
			return fmt.Errorf("vlan %d on %s has gateway %q, but target network persistence cannot render vlan gateways yet", vlan.ID, vlan.Parent, vlan.Gateway)
		}
		target.VLANs = append(target.VLANs, networkpersist.VLANConfig{
			Parent:  strings.TrimSpace(vlan.Parent),
			ID:      vlan.ID,
			DHCP:    strings.TrimSpace(vlan.Address) == "",
			Address: strings.TrimSpace(vlan.Address),
		})
	}
	return nil
}

func splitCommaValues(raw string) []string {
	var values []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func (o *Orchestrator) stopRAID(ctx context.Context) error {
	return o.disk.StopRAIDArrays(ctx)
}

func (o *Orchestrator) disableLVM(ctx context.Context) error {
	return o.disk.DisableLVM(ctx)
}

func (o *Orchestrator) removeEFIBootEntries(ctx context.Context) error {
	if o.shouldPreserveABBootEntries() {
		o.log.Info("A/B preserveExisting enabled, preserving existing EFI boot entries")
		return nil
	}
	return o.config.RemoveEFIBootEntries(ctx)
}

func (o *Orchestrator) mountEFIVars(ctx context.Context) error {
	return o.config.MountEFIVars(ctx)
}

func (o *Orchestrator) createEFIBootEntry(ctx context.Context) error {
	if o.isABImageMode() {
		o.log.Info("A/B image mode uses removable EFI fallback loader; skipping EFI NVRAM boot entry")
		return nil
	}
	if o.shouldPreserveABBootEntries() {
		o.log.Info("A/B preserveExisting enabled, preserving existing EFI boot entry")
		return nil
	}
	return o.config.CreateEFIBootEntry(ctx, o.targetDisk, o.bootPartition)
}

func (o *Orchestrator) setupMellanox(ctx context.Context) error {
	changed, err := o.config.SetupMellanox(ctx, o.cfg.Provision.Disk.NumVFs)
	if err != nil {
		return err
	}
	if changed {
		o.firmwareChanged = true
	}
	return nil
}

// FirmwareChanged reports whether any provisioning step changed firmware values
// that require a hard reboot (not kexec) to reinitialize.
func (o *Orchestrator) FirmwareChanged() bool {
	return o.firmwareChanged
}

func (o *Orchestrator) setupNVMeNamespaces(ctx context.Context) error {
	if o.cfg.Provision.Disk.NVMeNamespaces == "" {
		return nil
	}
	cfgs, err := disk.ParseNVMeConfig(o.cfg.Provision.Disk.NVMeNamespaces)
	if err != nil {
		return fmt.Errorf("parsing nvme namespace layout: %w", err)
	}
	created, err := o.disk.ApplyNVMeNamespaceLayout(ctx, cfgs)
	if err != nil {
		return err
	}
	// Verify at least one namespace was created across all controllers.
	totalCreated := 0
	for _, nsids := range created {
		totalCreated += len(nsids)
	}
	if totalCreated == 0 {
		return fmt.Errorf("nvme namespace layout applied but no namespaces were created; check controller support and configuration")
	}

	// After namespace creation set DiskDevice to the first created namespace on
	// the first configured controller so DetectDisk targets the intended OS disk.
	if len(cfgs) > 0 && o.cfg.Provision.Disk.Device == "" {
		firstController := cfgs[0].Controller
		nsids := created[firstController]
		if len(nsids) > 0 {
			o.cfg.Provision.Disk.Device = firstController + "n" + nsids[0]
			o.nvmeTargetDevice = o.cfg.Provision.Disk.Device
			o.log.Info("set disk device from nvme namespace layout", "device", o.cfg.Provision.Disk.Device)
		}
	}
	return nil
}

func (o *Orchestrator) setupRAID(ctx context.Context) error {
	raids := normalizedRAIDConfigs(o.cfg.Provision.Disk.RAID)
	if len(raids) == 0 {
		return nil
	}
	if err := validateRAIDNames(raids); err != nil {
		return err
	}
	if err := validateRAIDMemberCounts(raids); err != nil {
		return err
	}
	if err := validateRAIDMemberDevicesDisjoint(raids); err != nil {
		return err
	}
	if err := o.selectRAIDTargetDevice(raids); err != nil {
		return err
	}
	if err := o.wipeRAIDMemberDevices(ctx, raids); err != nil {
		return err
	}
	for _, raid := range raids {
		if err := o.disk.CreateRAIDArray(ctx, raid.Name, raid.Level, raid.Devices); err != nil {
			return fmt.Errorf("create raid array %q: %w", raid.Name, err)
		}
	}
	return nil
}

func (o *Orchestrator) wipeRAIDMemberDevices(ctx context.Context, raids []config.RAIDConfig) error {
	devices := uniqueRAIDMemberDevices(raids)
	secureErase := o.cfg.Provision.Disk.SecureErase
	for _, device := range devices {
		if secureErase {
			o.log.Info("secure erase enabled, erasing raid member", "device", device)
			if err := o.disk.SecureEraseDisk(ctx, device); err != nil {
				return fmt.Errorf("secure erase raid member %s: %w", device, err)
			}
			continue
		}
		if err := o.disk.WipeDisk(ctx, device); err != nil {
			return fmt.Errorf("wipe raid member %s: %w", device, err)
		}
	}
	return nil
}

func normalizedRAIDConfigs(raids []config.RAIDConfig) []config.RAIDConfig {
	normalized := make([]config.RAIDConfig, 0, len(raids))
	for _, raid := range raids {
		raid.Name = strings.TrimSpace(raid.Name)
		raid.Devices = uniqueTrimmedRAIDDevices(raid.Devices)
		normalized = append(normalized, raid)
	}
	return normalized
}

func validateRAIDNames(raids []config.RAIDConfig) error {
	for _, raid := range raids {
		if _, err := raidDevicePath(raid.Name); err != nil {
			return err
		}
	}
	return nil
}

func validateRAIDMemberCounts(raids []config.RAIDConfig) error {
	for _, raid := range raids {
		minDevices, ok := minRAIDDevicesForLevel(raid.Level)
		if !ok {
			return fmt.Errorf("raid array %q has unsupported level %d before destructive storage steps", raid.Name, raid.Level)
		}
		if len(raid.Devices) < minDevices {
			return fmt.Errorf("raid array %q level %d requires at least %d unique member devices, got %d",
				raid.Name, raid.Level, minDevices, len(raid.Devices))
		}
	}
	return nil
}

func validateRAIDMemberDevicesDisjoint(raids []config.RAIDConfig) error {
	owner := make(map[string]string)
	for _, raid := range raids {
		for _, device := range raid.Devices {
			if previous, ok := owner[device]; ok {
				return fmt.Errorf("raid member device %s is configured in both %q and %q before destructive storage steps",
					device, previous, raid.Name)
			}
			owner[device] = raid.Name
		}
	}
	return nil
}

func minRAIDDevicesForLevel(level int) (int, bool) {
	switch level {
	case 0, 1:
		return 2, true
	case 5:
		return 3, true
	case 6, 10:
		return 4, true
	default:
		return 0, false
	}
}

func (o *Orchestrator) selectRAIDTargetDevice(raids []config.RAIDConfig) error {
	if target := strings.TrimSpace(o.cfg.Provision.Disk.Device); target != "" {
		if o.nvmeTargetDevice != "" && target == o.nvmeTargetDevice {
			o.cfg.Provision.Disk.Device = ""
		} else {
			if !raidTargetMatchesConfig(target, raids) {
				return fmt.Errorf("provision.disk.device %s must match one of the configured raid array devices before destructive storage steps", target)
			}
			o.cfg.Provision.Disk.Device = target
			return nil
		}
	}
	if len(raids) != 1 {
		return fmt.Errorf("provision.disk.device is required when %d raid arrays are configured", len(raids))
	}
	target, err := raidDevicePath(raids[0].Name)
	if err != nil {
		return err
	}
	o.cfg.Provision.Disk.Device = target
	o.targetDisk = target
	o.log.Info("set disk device from raid configuration", "device", target)
	return nil
}

func uniqueRAIDMemberDevices(raids []config.RAIDConfig) []string {
	seen := make(map[string]struct{})
	var devices []string
	for _, raid := range raids {
		for _, device := range raid.Devices {
			if _, ok := seen[device]; ok {
				continue
			}
			seen[device] = struct{}{}
			devices = append(devices, device)
		}
	}
	return devices
}

func raidTargetMatchesConfig(target string, raids []config.RAIDConfig) bool {
	for _, raid := range raids {
		device, err := raidDevicePath(raid.Name)
		if err == nil && target == device {
			return true
		}
	}
	return false
}

func uniqueTrimmedRAIDDevices(devices []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(devices))
	for _, device := range devices {
		trimmed := strings.TrimSpace(device)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func raidDevicePath(name string) (string, error) {
	name = strings.TrimSpace(name)
	clean := filepath.Clean(name)
	if name == "" || filepath.IsAbs(name) || clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid raid array name %q before destructive storage steps", name)
	}
	if strings.HasPrefix(clean, "md/") {
		if strings.Count(clean, "/") == 1 && strings.TrimPrefix(clean, "md/") != "" {
			return "/dev/" + clean, nil
		}
		return "", fmt.Errorf("invalid raid array name %q before destructive storage steps", name)
	}
	if strings.HasPrefix(clean, "md") && len(clean) > 2 {
		for _, r := range strings.TrimPrefix(clean, "md") {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("invalid raid array name %q before destructive storage steps", name)
			}
		}
		return "/dev/" + clean, nil
	}
	return "", fmt.Errorf("invalid raid array name %q before destructive storage steps", name)
}

func (o *Orchestrator) validateProvisionInputs(ctx context.Context) error {
	if err := config.ValidateRequiredProvisionTargetOS(o.cfg.Provision.TargetOS); err != nil {
		return fmt.Errorf("rejected before destructive storage steps: %w", err)
	}
	o.cfg.Provision.TargetOS = config.NormalizeProvisionTargetOS(o.cfg.Provision.TargetOS)
	osFamily := strings.ToLower(strings.TrimSpace(o.cfg.OSFamily))
	if osFamily == "rhel" {
		return fmt.Errorf("osFamily=%q is not supported for provisioning: rhel-like target bootloader support is not implemented: native GRUB2/BLS/vendor EFI paths are required before destructive storage steps", osFamily)
	}
	if err := o.validatePartitionLayoutModeCompatibility(); err != nil {
		return err
	}
	if err := o.validatePartitionLayoutRuntimeSupport(); err != nil {
		return err
	}
	o.cfg.Provision.Image.Checksum = strings.TrimSpace(o.cfg.Provision.Image.Checksum)
	checksumConfigured := o.cfg.Provision.Image.Checksum != ""
	imageSources := make([]string, 0, len(o.cfg.Provision.Image.URLs))
	hasSource := false
	for i, source := range o.cfg.Provision.Image.URLs {
		source = strings.TrimSpace(source)
		o.cfg.Provision.Image.URLs[i] = source
		if source == "" {
			continue
		}
		hasSource = true
		imageSources = append(imageSources, source)
		if err := validateProvisionImageSource(source, checksumConfigured, o.cfg.Provision.Image.AllowInsecureHTTP); err != nil {
			return err
		}
	}
	if !hasSource {
		return fmt.Errorf("provision image source required before destructive storage steps: no image URLs configured")
	}
	o.cfg.Provision.Image.SignatureURL = strings.TrimSpace(o.cfg.Provision.Image.SignatureURL)
	if o.cfg.Provision.Image.SignatureURL != "" && !checksumConfigured {
		return errors.New(imageSignatureChecksumRequiredMessage)
	}

	return o.validateImageStreamingPrerequisites(ctx, imageSources)
}

func (o *Orchestrator) validateImageStreamingPrerequisites(ctx context.Context, imageSources []string) error {
	bestURL := o.bestImageURL
	if bestURL == "" {
		var err error
		bestURL, err = image.SelectBestSource(ctx, imageSources)
		if err != nil {
			return fmt.Errorf("selecting image source before destructive storage: %w", err)
		}
		o.bestImageURL = bestURL
	}
	if image.IsOCIReference(bestURL) {
		return nil
	}
	format, err := image.ValidateStreamingPrerequisites(ctx, bestURL)
	if err != nil {
		if image.IsTransientFormatProbeError(err) {
			o.log.Warn("unable to probe image prerequisites before destructive storage",
				"url", image.RedactURL(bestURL), "error", err)
			return nil
		}
		return fmt.Errorf("validating image prerequisites before destructive storage: %w", err)
	}
	o.log.Info("image prerequisites validated", "url", image.RedactURL(bestURL), "format", format)
	return nil
}

func validateProvisionImageSource(source string, checksumConfigured, allowInsecureHTTP bool) error {
	if image.IsOCIReference(source) {
		return validateProvisionOCIImageSource(source, checksumConfigured)
	}

	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("invalid image source %q: %w", image.RedactURL(source), redactedProvisionSourceError(err, source))
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
		if scheme == "http" && !allowInsecureHTTP {
			return fmt.Errorf("provision image source %q: plain HTTP sources require provision.image.allowInsecureHTTP=true (or IMAGE_ALLOW_INSECURE_HTTP=true via vars)", image.RedactURL(source))
		}
		if u.Hostname() == "" {
			return fmt.Errorf("invalid image source %q: missing host", image.RedactURL(source))
		}
		return nil
	case "":
		return fmt.Errorf("unsupported image source without scheme: %q", image.RedactURL(source))
	default:
		return fmt.Errorf("unsupported image source scheme %q for %q", scheme, image.RedactURL(source))
	}
}

func validateProvisionOCIImageSource(source string, checksumConfigured bool) error {
	ref := strings.TrimSpace(image.TrimOCIScheme(source))
	if ref == "" {
		return fmt.Errorf("invalid OCI image source: missing reference")
	}
	parsedRef, err := ociname.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("invalid OCI image source %q: %w", image.RedactOCIRef(ref), redactedProvisionOCIReferenceError(err, ref))
	}
	_, digestRef := parsedRef.(ociname.Digest)
	if !checksumConfigured && !digestRef {
		return fmt.Errorf("provision OCI image source must use a digest reference or IMAGE_CHECKSUM before destructive storage steps: %s", image.RedactURL(source))
	}
	return nil
}

func redactOCIReferenceError(err error, ref string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	redactedRef := image.RedactOCIRef(ref)
	msg = strings.ReplaceAll(msg, ref, redactedRef)
	msg = strings.ReplaceAll(msg, "oci://"+ref, "oci://"+redactedRef)
	return msg
}

type redactedProvisionSourceErr struct {
	source string
	err    error
}

func redactedProvisionSourceError(err error, source string) error {
	if err == nil {
		return nil
	}
	return &redactedProvisionSourceErr{source: source, err: err}
}

func (e *redactedProvisionSourceErr) Error() string {
	return image.RedactSourceError(e.err, e.source)
}

func (e *redactedProvisionSourceErr) Unwrap() error {
	return e.err
}

type redactedProvisionOCIReferenceErr struct {
	ref string
	err error
}

func redactedProvisionOCIReferenceError(err error, ref string) error {
	if err == nil {
		return nil
	}
	return &redactedProvisionOCIReferenceErr{ref: ref, err: err}
}

func (e *redactedProvisionOCIReferenceErr) Error() string {
	return redactOCIReferenceError(e.err, e.ref)
}

func (e *redactedProvisionOCIReferenceErr) Unwrap() error {
	return e.err
}

func (o *Orchestrator) wipeOrSecureEraseDisks(ctx context.Context) error {
	if err := o.ensureABPartitionLayout(); err != nil {
		return err
	}
	if !config.IsDeprovisionMode(o.cfg.Mode) {
		if err := o.disk.RequireLVMTools(ctx, o.cfg.Provision.Disk.PartitionLayout); err != nil {
			return err
		}
	}
	if err := o.validatePartitionLayoutConfig(); err != nil {
		return err
	}
	if o.isABImageMode() && o.cfg.Provision.AB.PreserveExisting {
		o.log.Info("A/B preserveExisting enabled, skipping whole-disk wipe")
		return nil
	}

	// In deprovision modes use the deprovision-specific SecureErase setting.
	// In all other modes (provision, dry-run) use the provision setting.
	secureErase := o.cfg.Provision.Disk.SecureErase
	mode := o.cfg.Mode
	deprovisionMode := config.IsDeprovisionMode(mode)
	if deprovisionMode {
		secureErase = o.cfg.Deprovision.SecureErase
	}
	targetDisk := strings.TrimSpace(o.targetDisk)
	if targetDisk == "" {
		if allowsWholeDiskDeprovisionWipe(mode) {
			if secureErase {
				o.log.Info("secure erase enabled, performing hardware-level erase on all disks")
				return o.disk.SecureEraseAllDisks(ctx)
			}
			return o.disk.WipeAllDisks(ctx)
		}
		return fmt.Errorf("target disk is required before wipe-disks")
	}
	if len(o.cfg.Provision.Disk.RAID) > 0 && !deprovisionMode &&
		raidTargetMatchesConfig(targetDisk, normalizedRAIDConfigs(o.cfg.Provision.Disk.RAID)) {
		o.log.Info("wiping raid target device after array creation", "device", targetDisk)
		return o.disk.WipeDisk(ctx, targetDisk)
	}
	if secureErase {
		o.log.Info("secure erase enabled, performing hardware-level erase")
		return o.disk.SecureEraseDisk(ctx, targetDisk)
	}
	return o.disk.WipeDisk(ctx, targetDisk)
}

func allowsWholeDiskDeprovisionWipe(mode string) bool {
	return mode == "deprovision" || mode == "hard"
}

func (o *Orchestrator) validatePartitionLayoutModeCompatibility() error {
	if o.cfg.Provision.Disk.PartitionLayout == nil {
		return nil
	}

	// Deprovisioning is allowed to wipe disks even when PARTITION_LAYOUT is set.
	if config.IsDeprovisionMode(o.cfg.Mode) {
		return nil
	}
	if o.isABImageMode() {
		return nil
	}

	mode := strings.ToLower(strings.TrimSpace(o.cfg.Provision.Image.Mode))
	if mode == "" || mode == config.ImageModeWholeDisk {
		return nil
	}

	return fmt.Errorf("provision.image.mode=%q cannot be combined with partition layout; use whole-disk mode to stream the selected root filesystem into the declared root partition", mode)
}

func (o *Orchestrator) validatePartitionLayoutConfig() error {
	if o.cfg.Provision.Disk.PartitionLayout == nil {
		return nil
	}

	layoutDevice := strings.TrimSpace(o.cfg.Provision.Disk.PartitionLayout.Device)
	o.cfg.Provision.Disk.PartitionLayout.Device = layoutDevice

	// Check device conflicts before mode compatibility so that
	// configuration errors surface immediately.
	if layoutDevice != "" && o.cfg.Provision.Disk.Device != "" && o.cfg.Provision.Disk.Device != layoutDevice {
		return fmt.Errorf("disk device conflict: DISK_DEVICE=%q differs from PARTITION_LAYOUT.device=%q", o.cfg.Provision.Disk.Device, layoutDevice)
	}

	if err := o.validatePartitionLayoutModeCompatibility(); err != nil {
		return err
	}
	if err := o.validatePartitionLayoutRuntimeSupport(); err != nil {
		return err
	}

	if layoutDevice == "" {
		return nil
	}

	info, err := os.Stat(layoutDevice)
	if err != nil {
		return fmt.Errorf("partition layout device %q: %w", layoutDevice, err)
	}
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("partition layout device %q is not a block device", layoutDevice)
	}

	return nil
}

func (o *Orchestrator) validatePartitionLayoutRuntimeSupport() error {
	layout := o.cfg.Provision.Disk.PartitionLayout
	if layout == nil || o.isABImageMode() || config.IsDeprovisionMode(o.cfg.Mode) {
		return nil
	}
	for _, part := range layout.Partitions {
		if err := validateSupportedLayoutMountpoint(part.Mountpoint); err != nil {
			return err
		}
	}
	if layout.LVM != nil {
		for _, vol := range layout.LVM.Volumes {
			if strings.TrimSpace(vol.Mountpoint) == "/boot/efi" {
				return fmt.Errorf("partition layout lvm volume mountpoint \"/boot/efi\" is not supported; declare /boot/efi as a regular partition")
			}
			if err := validateSupportedLayoutMountpoint(vol.Mountpoint); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSupportedLayoutMountpoint(mountpoint string) error {
	switch strings.TrimSpace(mountpoint) {
	case "", "/", "/boot/efi":
		return nil
	default:
		return fmt.Errorf("partition layout mountpoint %q is not supported in non-A/B layout provisioning; only / and /boot/efi are mounted during provisioning", mountpoint)
	}
}

func (o *Orchestrator) detectDisk(ctx context.Context) error {
	if o.cfg.Provision.Disk.PartitionLayout != nil {
		layoutDevice := strings.TrimSpace(o.cfg.Provision.Disk.PartitionLayout.Device)
		o.cfg.Provision.Disk.PartitionLayout.Device = layoutDevice
		if layoutDevice != "" {
			o.targetDisk = layoutDevice
			o.log.Info("using partition layout device override", "device", o.targetDisk)
			return nil
		}
	}

	// If a specific disk device is configured, validate and use it directly.
	if o.cfg.Provision.Disk.Device != "" {
		info, err := os.Stat(o.cfg.Provision.Disk.Device)
		if err != nil {
			return fmt.Errorf("configured disk device %s: %w", o.cfg.Provision.Disk.Device, err)
		}
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("configured disk device %s is not a block device", o.cfg.Provision.Disk.Device)
		}
		o.log.Info("using configured disk device", "device", o.cfg.Provision.Disk.Device)
		o.targetDisk = o.cfg.Provision.Disk.Device
		return nil
	}

	serial := strings.TrimSpace(o.cfg.Provision.Disk.SerialNumber)
	o.cfg.Provision.Disk.SerialNumber = serial
	if serial != "" {
		d, err := o.disk.FindDiskBySerial(ctx, serial, o.cfg.Provision.Disk.MinSizeGB)
		if err != nil {
			return err
		}
		o.log.Info("using configured disk serial", "serial", serial, "device", d)
		o.targetDisk = d
		return nil
	}

	d, err := o.disk.DetectDisk(ctx, o.cfg.Provision.Disk.MinSizeGB)
	if err != nil {
		return err
	}
	o.targetDisk = d
	return nil
}

func (o *Orchestrator) applyPartitionLayout(ctx context.Context) error {
	if err := o.ensureABPartitionLayout(); err != nil {
		return err
	}
	if o.cfg.Provision.Disk.PartitionLayout == nil {
		return nil
	}
	if o.isABImageMode() && o.cfg.Provision.AB.PreserveExisting {
		o.log.Info("A/B preserveExisting enabled, reusing existing partition layout")
		return nil
	}

	device := o.targetDisk
	layoutDevice := strings.TrimSpace(o.cfg.Provision.Disk.PartitionLayout.Device)
	o.cfg.Provision.Disk.PartitionLayout.Device = layoutDevice
	if layoutDevice != "" {
		device = layoutDevice
		o.targetDisk = device
	}

	// Validate the target device exists and is a block device.
	if info, err := os.Stat(device); err != nil {
		return fmt.Errorf("partition layout device %q: %w", device, err)
	} else if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("partition layout device %q is not a block device", device)
	}

	o.log.Info("applying custom partition layout", "device", device, "partitions", len(o.cfg.Provision.Disk.PartitionLayout.Partitions))

	if err := o.disk.ApplyPartitionLayout(ctx, device, o.cfg.Provision.Disk.PartitionLayout); err != nil {
		return fmt.Errorf("apply partition layout: %w", err)
	}

	// Apply LVM if configured.
	if o.cfg.Provision.Disk.PartitionLayout.LVM != nil {
		if err := o.disk.ApplyLVMConfig(ctx, device, o.cfg.Provision.Disk.PartitionLayout); err != nil {
			return fmt.Errorf("apply LVM config: %w", err)
		}
	}

	o.log.Info("custom partition layout applied")
	return nil
}

// writeFstab generates and writes fstab after root is mounted.
func (o *Orchestrator) writeFstabStep(ctx context.Context) error {
	if err := o.writeFstab(ctx); err != nil {
		return err
	}
	return o.writeABSlotState()
}

func (o *Orchestrator) writeFstab(ctx context.Context) error {
	if o.cfg.Provision.Disk.PartitionLayout == nil {
		return nil
	}
	device := o.targetDisk
	layout := o.cfg.Provision.Disk.PartitionLayout
	if o.shouldDetectPartitionLayoutRootFilesystem() {
		var err error
		layout, err = o.partitionLayoutWithDetectedRootFilesystem(ctx, layout)
		if err != nil {
			return err
		}
	}
	fstab := disk.GenerateFstab(layout, device)
	if layout.LVM != nil {
		fstab += disk.GenerateLVMFstab(layout.LVM)
	}

	fstabPath := filepath.Join(o.config.rootDir, "etc", "fstab")
	if err := os.MkdirAll(filepath.Dir(fstabPath), 0o755); err != nil {
		return fmt.Errorf("creating fstab directory: %w", err)
	}
	if err := os.WriteFile(fstabPath, []byte(fstab), 0o644); err != nil {
		return fmt.Errorf("writing fstab: %w", err)
	}
	o.log.Info("generated fstab for custom layout")
	return nil
}

func (o *Orchestrator) shouldDetectPartitionLayoutRootFilesystem() bool {
	return o.cfg.Provision.Disk.PartitionLayout != nil && !o.isABImageMode()
}

func (o *Orchestrator) partitionLayoutWithDetectedRootFilesystem(
	ctx context.Context,
	layout *config.PartitionLayout,
) (*config.PartitionLayout, error) {
	rootFS, err := o.disk.FilesystemType(ctx, o.rootPartition)
	if err != nil {
		return nil, fmt.Errorf("detecting streamed root filesystem for fstab: %w", err)
	}
	if rootFS == "" {
		return nil, fmt.Errorf("detecting streamed root filesystem for fstab: empty filesystem type for %s", o.rootPartition)
	}

	updated := copyPartitionLayout(layout)
	if setLayoutRootFilesystem(updated, rootFS) {
		return updated, nil
	}
	return nil, fmt.Errorf("partition layout has no root mountpoint for fstab")
}

func copyPartitionLayout(layout *config.PartitionLayout) *config.PartitionLayout {
	if layout == nil {
		return nil
	}
	updated := *layout
	updated.Partitions = append([]config.Partition(nil), layout.Partitions...)
	if layout.LVM != nil {
		lvm := *layout.LVM
		lvm.Volumes = append([]config.LVVolume(nil), layout.LVM.Volumes...)
		updated.LVM = &lvm
	}
	return &updated
}

func setLayoutRootFilesystem(layout *config.PartitionLayout, fsType string) bool {
	for i := range layout.Partitions {
		if strings.TrimSpace(layout.Partitions[i].Mountpoint) == "/" {
			layout.Partitions[i].Filesystem = fsType
			return true
		}
	}
	if layout.LVM == nil {
		return false
	}
	for i := range layout.LVM.Volumes {
		if strings.TrimSpace(layout.LVM.Volumes[i].Mountpoint) == "/" {
			layout.LVM.Volumes[i].Filesystem = fsType
			return true
		}
	}
	return false
}

func (o *Orchestrator) streamImage(ctx context.Context) error {
	bestURL := o.bestImageURL
	if bestURL == "" {
		// verify-image may have skipped URL resolution; resolve it now.
		var err error
		bestURL, err = image.SelectBestSource(ctx, o.cfg.Provision.Image.URLs)
		if err != nil {
			return fmt.Errorf("selecting image source: %w", err)
		}
	}

	var opts []image.StreamOpts
	if o.cfg.Provision.Image.Checksum != "" {
		opts = append(opts, image.StreamOpts{
			Checksum:     o.cfg.Provision.Image.Checksum,
			ChecksumType: o.cfg.Provision.Image.ChecksumType,
		})
	}

	if o.cfg.Provision.Disk.PartitionLayout != nil && !o.isABImageMode() {
		return o.streamPartitionLayoutRootImage(ctx, bestURL, opts)
	}

	// Partition-by-partition mode: wipe first to ensure a clean slate on any
	// retry attempt, then download and copy each partition individually.
	if strings.EqualFold(o.cfg.Provision.Image.Mode, "partition") {
		o.log.Info("Streaming image partition-by-partition", "url", image.RedactURL(bestURL), "disk", o.targetDisk)
		if err := o.disk.WipeDisk(ctx, o.targetDisk); err != nil {
			return fmt.Errorf("wiping disk before partition stream: %w", err)
		}
		if err := image.StreamPartitions(ctx, bestURL, o.targetDisk, opts...); err != nil {
			return classifyImageStreamError(bestURL, err)
		}
		return nil
	}

	if o.isABImageMode() {
		return o.streamABImage(ctx, bestURL, opts)
	}

	// Default whole-disk mode.
	o.log.Info("Streaming image", "url", image.RedactURL(bestURL), "disk", o.targetDisk)
	if err := image.Stream(ctx, bestURL, o.targetDisk, opts...); err != nil {
		return classifyImageStreamError(bestURL, err)
	}
	return nil
}

func (o *Orchestrator) streamPartitionLayoutRootImage(ctx context.Context, bestURL string, opts []image.StreamOpts) error {
	if err := o.validatePartitionLayoutModeCompatibility(); err != nil {
		return err
	}
	if err := o.parsePartitionsFromLayout(ctx); err != nil {
		return err
	}
	o.log.Info("streaming image into partition layout root", "url", image.RedactURL(bestURL), "disk", o.targetDisk, "root", o.rootPartition)
	if err := streamRootImageFn(ctx, bestURL, image.RootTarget{
		RootPartition:       o.rootPartition,
		SourceRootLabel:     o.cfg.Provision.Image.SourceRootLabel,
		SourceRootPartition: o.cfg.Provision.Image.SourceRootPartition,
	}, opts...); err != nil {
		return classifyImageStreamError(bestURL, err)
	}
	return nil
}

func classifyImageStreamError(imageURL string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := &redactedImageStreamError{
		msg: fmt.Sprintf("streaming %s: %s", image.RedactURL(imageURL), image.RedactSourceError(err, imageURL)),
		err: err,
	}
	if strings.Contains(strings.ToLower(err.Error()), "checksum mismatch") {
		return &PermanentError{Err: wrapped}
	}
	return wrapped
}

type redactedImageStreamError struct {
	msg string
	err error
}

func (e *redactedImageStreamError) Error() string {
	return e.msg
}

func (e *redactedImageStreamError) Unwrap() error {
	return e.err
}

func (o *Orchestrator) streamABImage(ctx context.Context, bestURL string, opts []image.StreamOpts) error {
	if err := o.ensureABPartitionLayout(); err != nil {
		return err
	}
	if err := o.parsePartitionsFromLayout(ctx); err != nil {
		return err
	}
	if err := o.validateABPreserveExistingLayout(ctx); err != nil {
		return err
	}
	if err := o.prepareABTargetSlot(ctx); err != nil {
		return err
	}

	targets := o.abStreamTargets()
	o.log.Info("Streaming image into A/B target slot", "url", image.RedactURL(bestURL), "disk", targets.Disk, "root", targets.RootPartition, "boot", targets.BootPartition)
	if err := image.StreamAB(ctx, bestURL, targets, opts...); err != nil {
		return classifyImageStreamError(bestURL, err)
	}
	return nil
}

func (o *Orchestrator) abStreamTargets() image.ABTargets {
	bootPartition := o.bootPartition
	if o.cfg.Provision.AB.PreserveExisting {
		// PreserveExisting updates only the target root slot. The shared EFI
		// partition stays untouched so rollback keeps the previous boot assets.
		bootPartition = ""
	}
	return image.ABTargets{
		Disk:                o.targetDisk,
		BootPartition:       bootPartition,
		RootPartition:       o.rootPartition,
		SourceRootLabel:     o.cfg.Provision.AB.SourceRootLabel,
		SourceRootPartition: o.cfg.Provision.AB.SourceRootPartition,
	}
}

func (o *Orchestrator) prepareABTargetSlot(ctx context.Context) error {
	if !o.cfg.Provision.AB.PreserveExisting {
		return nil
	}
	if err := o.disk.WipeFilesystemSignatures(ctx, o.rootPartition); err != nil {
		return fmt.Errorf("wiping A/B target slot before stream: %w", err)
	}
	return nil
}

func (o *Orchestrator) validateABPreserveExistingLayout(ctx context.Context) error {
	if !o.cfg.Provision.AB.PreserveExisting {
		return nil
	}
	layout := o.cfg.Provision.Disk.PartitionLayout
	if layout == nil {
		return fmt.Errorf("A/B preserveExisting requires generated partition layout")
	}
	actual, err := o.disk.ParsePartitions(ctx, o.targetDisk)
	if err != nil {
		return fmt.Errorf("validate existing A/B partition layout: %w", err)
	}
	if len(actual) < len(layout.Partitions) {
		return fmt.Errorf("existing A/B partition layout has %d partitions, want at least %d",
			len(actual), len(layout.Partitions))
	}
	for i := range layout.Partitions {
		expected := &layout.Partitions[i]
		if err := validateABPreservePartition(o.targetDisk, i, expected, actual[i]); err != nil {
			return err
		}
	}
	if err := o.validateABActiveSlotState(ctx); err != nil {
		return err
	}
	return nil
}

func (o *Orchestrator) validateABActiveSlotState(ctx context.Context) error {
	ab := o.cfg.Provision.AB.WithDefaults()
	if ab.ActiveSlot == "" {
		return fmt.Errorf("A/B preserveExisting requires provision.ab.activeSlot")
	}
	targetSlot, err := ab.ResolvedTargetSlot()
	if err != nil {
		return err
	}
	if targetSlot == ab.ActiveSlot {
		return fmt.Errorf("A/B target slot %q equals active slot", targetSlot)
	}

	activePartition, err := abSlotPartitionDevice(o.targetDisk, ab.ActiveSlot)
	if err != nil {
		return err
	}
	if activePartition == o.rootPartition {
		return fmt.Errorf("A/B target root %s resolves to active slot %q", o.rootPartition, ab.ActiveSlot)
	}

	if err := validateABBootedSlotSignal(o.targetDisk, ab.ActiveSlot); err != nil {
		return err
	}

	if _, err := os.Stat(activePartition); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("active A/B partition device %s is not present", activePartition)
		}
		return fmt.Errorf("stat active A/B partition %s: %w", activePartition, err)
	}

	mountpoint, err := os.MkdirTemp("", "booty-active-slot-*")
	if err != nil {
		return fmt.Errorf("creating active A/B slot mountpoint: %w", err)
	}
	defer func() { _ = os.RemoveAll(mountpoint) }()

	if err := mountReadOnlyPart(ctx, o.disk, activePartition, mountpoint); err != nil {
		return fmt.Errorf("mounting declared active A/B slot %q (%s): %w", ab.ActiveSlot, activePartition, err)
	}
	defer func() {
		if err := o.disk.Unmount(mountpoint); err != nil {
			o.log.Warn("failed to unmount active A/B slot", "mountpoint", mountpoint, "error", err)
		}
	}()

	state, err := readABSlotStateFile(filepath.Join(mountpoint, "etc", "booty", "ab-slot.env"))
	if err != nil {
		return fmt.Errorf("reading active A/B slot state from %s: %w", activePartition, err)
	}
	stateBootedSlot := normalizeABStateSlot(state["BOOTY_AB_BOOTED_SLOT"])
	if stateBootedSlot == "" {
		stateBootedSlot = normalizeABStateSlot(state["BOOTY_AB_TARGET_SLOT"])
	}
	if stateBootedSlot != ab.ActiveSlot {
		return fmt.Errorf("active A/B slot state on %s reports slot %q, config declares %q", activePartition, stateBootedSlot, ab.ActiveSlot)
	}
	return nil
}

func validateABBootedSlotSignal(diskDevice, activeSlot string) error {
	bootedSlot, err := detectBootedABSlotFromCmdline(diskDevice)
	if err != nil {
		return fmt.Errorf("determine booted A/B slot from kernel cmdline: %w", err)
	}
	if bootedSlot == "" {
		return nil
	}
	if bootedSlot != activeSlot {
		return fmt.Errorf("kernel cmdline reports booted A/B slot %q, config declares active slot %q", bootedSlot, activeSlot)
	}
	return nil
}

func detectBootedABSlotFromCmdline(diskDevice string) (string, error) {
	data, err := readProcCmdline()
	if err != nil {
		return "", err
	}
	bootedSlot := ""
	for _, field := range strings.Fields(string(data)) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key != "root" {
			continue
		}
		if slot := abSlotFromRootSpec(value, diskDevice); slot != "" {
			bootedSlot = slot
		}
	}
	return bootedSlot, nil
}

func abSlotFromRootSpec(rootSpec, diskDevice string) string {
	rootSpec = strings.Trim(strings.TrimSpace(rootSpec), `"'`)
	switch {
	case strings.EqualFold(rootSpec, "PARTLABEL=BOOTY-ROOT-A"),
		strings.EqualFold(rootSpec, "LABEL=BOOTY-ROOT-A"),
		strings.EqualFold(rootSpec, "/dev/disk/by-label/BOOTY-ROOT-A"),
		strings.EqualFold(rootSpec, "/dev/disk/by-partlabel/BOOTY-ROOT-A"):
		return config.ABSlotA
	case strings.EqualFold(rootSpec, "PARTLABEL=BOOTY-ROOT-B"),
		strings.EqualFold(rootSpec, "LABEL=BOOTY-ROOT-B"),
		strings.EqualFold(rootSpec, "/dev/disk/by-label/BOOTY-ROOT-B"),
		strings.EqualFold(rootSpec, "/dev/disk/by-partlabel/BOOTY-ROOT-B"):
		return config.ABSlotB
	}
	rootSpec = resolveRootSpecDevice(rootSpec)
	if slotA, err := abSlotPartitionDevice(diskDevice, config.ABSlotA); err == nil && rootSpec == slotA {
		return config.ABSlotA
	}
	if slotB, err := abSlotPartitionDevice(diskDevice, config.ABSlotB); err == nil && rootSpec == slotB {
		return config.ABSlotB
	}
	return ""
}

func resolveRootSpecDevice(rootSpec string) string {
	var devicePath string
	if key, value, ok := strings.Cut(rootSpec, "="); ok {
		switch strings.ToUpper(key) {
		case "UUID":
			devicePath = "/dev/disk/by-uuid/" + value
		case "PARTUUID":
			devicePath = "/dev/disk/by-partuuid/" + value
		case "LABEL":
			devicePath = "/dev/disk/by-label/" + value
		case "PARTLABEL":
			devicePath = "/dev/disk/by-partlabel/" + value
		}
	} else if strings.HasPrefix(rootSpec, "/dev/") {
		devicePath = rootSpec
	}
	if devicePath == "" {
		return rootSpec
	}
	if resolved, err := evalRootSymlinks(devicePath); err == nil && strings.HasPrefix(resolved, "/dev/") {
		devicePath = resolved
	}
	return resolveBlockSlaveDevice(devicePath)
}

func resolveBlockSlaveDevice(devicePath string) string {
	base := filepath.Base(devicePath)
	entries, err := os.ReadDir(filepath.Join(sysBlockRoot, base, "slaves"))
	if err != nil || len(entries) != 1 {
		return devicePath
	}
	name := strings.TrimSpace(entries[0].Name())
	if name == "" {
		return devicePath
	}
	return "/dev/" + name
}

func abSlotPartitionDevice(diskDevice, slot string) (string, error) {
	switch normalizeABStateSlot(slot) {
	case config.ABSlotA:
		return disk.PartitionDevicePath(diskDevice, 2), nil
	case config.ABSlotB:
		return disk.PartitionDevicePath(diskDevice, 3), nil
	default:
		return "", fmt.Errorf("invalid A/B slot %q", slot)
	}
}

func readABSlotStateFile(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is inside a read-only mounted root partition
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return values, nil
}

func normalizeABStateSlot(slot string) string {
	return strings.ToLower(strings.TrimSpace(slot))
}

func validateABPreservePartition(diskDevice string, index int, expected *config.Partition, actual disk.Partition) error {
	partNum := index + 1
	expectedNode := disk.PartitionDevicePath(diskDevice, partNum)
	if actual.Node != expectedNode {
		return fmt.Errorf("existing A/B partition %d node = %q, want %q", partNum, actual.Node, expectedNode)
	}
	if strings.TrimSpace(actual.Name) != expected.Label {
		return fmt.Errorf("existing A/B partition %d label = %q, want %q", partNum, actual.Name, expected.Label)
	}
	expectedType := expectedABPartitionType(expected)
	if !strings.EqualFold(actual.Type, expectedType) {
		return fmt.Errorf("existing A/B partition %d (%s) type = %q, want %q",
			partNum, expected.Label, actual.Type, expectedType)
	}
	return nil
}

func expectedABPartitionType(part *config.Partition) string {
	if strings.EqualFold(part.Filesystem, "vfat") || part.Mountpoint == "/boot/efi" {
		return disk.EFISystemPartitionGUID
	}
	return disk.LinuxFilesystemGUID
}

func (o *Orchestrator) isABImageMode() bool {
	return strings.EqualFold(strings.TrimSpace(o.cfg.Provision.Image.Mode), config.ImageModeAB)
}

func (o *Orchestrator) shouldPreserveABBootEntries() bool {
	return o.isABImageMode() && o.cfg.Provision.AB.PreserveExisting
}

func (o *Orchestrator) ensureABPartitionLayout() error {
	if !o.isABImageMode() {
		return nil
	}
	device := strings.TrimSpace(o.targetDisk)
	if device == "" {
		device = strings.TrimSpace(o.cfg.Provision.Disk.Device)
	}
	layout, err := o.cfg.Provision.AB.PartitionLayout(device)
	if err != nil {
		return fmt.Errorf("A/B partition layout: %w", err)
	}
	o.cfg.Provision.Disk.PartitionLayout = layout
	return nil
}

func (o *Orchestrator) writeABSlotState() error {
	if !o.isABImageMode() {
		return nil
	}
	ab := o.cfg.Provision.AB.WithDefaults()
	targetSlot, err := ab.ResolvedTargetSlot()
	if err != nil {
		return err
	}
	statePath := filepath.Join(o.config.rootDir, "etc", "booty", "ab-slot.env")
	stateDir := filepath.Dir(statePath)
	if err := ensureWithinRoot(o.config.rootDir, statePath); err != nil {
		return fmt.Errorf("a/b slot state path: %w", err)
	}
	if err := ensureTargetParentWithinRoot(o.config.rootDir, stateDir); err != nil {
		return fmt.Errorf("a/b slot state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating A/B state directory: %w", err)
	}
	if err := ensureTargetParentWithinRoot(o.config.rootDir, stateDir); err != nil {
		return fmt.Errorf("a/b slot state directory: %w", err)
	}
	content := fmt.Sprintf(
		"BOOTY_AB_SCHEME=%s\nBOOTY_AB_TARGET_SLOT=%s\nBOOTY_AB_BOOTED_SLOT=%s\nBOOTY_AB_ACTIVE_SLOT=%s\nBOOTY_AB_PRESERVE_EXISTING=%t\nBOOTY_AB_ROOT_PARTITION=%s\n",
		ab.Scheme,
		targetSlot,
		targetSlot,
		ab.ActiveSlot,
		ab.PreserveExisting,
		o.rootPartition,
	)
	if err := writeFileAtomic(statePath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing A/B slot state: %w", err)
	}
	return nil
}

func (o *Orchestrator) verifyImageSignature(ctx context.Context) error {
	// Resolve the best image URL so stream-image reuses the same source.
	// NOTE: This step streams the image content for GPG verification. The
	// subsequent stream-image step downloads the same URL again. This is an
	// intentional tradeoff: signature verification must complete before
	// writing to disk, and piping the same stream into both GPG and the
	// block device would require buffering multi-GB images in memory.
	bestURL := o.bestImageURL
	if bestURL == "" {
		var err error
		bestURL, err = image.SelectBestSource(ctx, o.cfg.Provision.Image.URLs)
		if err != nil {
			return fmt.Errorf("selecting image source: %w", err)
		}
		o.bestImageURL = bestURL
	}

	if image.IsOCIReference(bestURL) {
		// Probe OCI sources even without GPG verification so bad registry
		// references fail before destructive storage steps begin.
		ref := image.TrimOCIScheme(bestURL)
		probeCtx, cancel := context.WithTimeout(ctx, ociPreflightProbeTimeout)
		defer cancel()
		if err := probeOCIReference(probeCtx, ref); err != nil {
			return fmt.Errorf("probing OCI image source: %w", err)
		}
	}

	if o.cfg.Provision.Image.SignatureURL == "" {
		o.log.Info("no image signature URL configured, skipping verification")
		return nil
	}
	if o.cfg.Provision.Image.GPGPubKey == "" {
		return fmt.Errorf("image signature URL set but no GPG public key path configured")
	}

	return image.VerifyGPGSignature(ctx, bestURL, o.cfg.Provision.Image.SignatureURL, o.cfg.Provision.Image.GPGPubKey)
}

func (o *Orchestrator) partprobe(ctx context.Context) error {
	return o.disk.PartProbe(ctx, o.targetDisk)
}

func (o *Orchestrator) parsePartitions(ctx context.Context) error {
	if err := o.ensureABPartitionLayout(); err != nil {
		return err
	}

	// With a custom partition layout, derive root from the layout definition
	// rather than scanning by GUID (which can pick the wrong partition when
	// multiple Linux-type partitions exist).
	if o.cfg.Provision.Disk.PartitionLayout != nil {
		return o.parsePartitionsFromLayout(ctx)
	}

	parts, err := o.disk.ParsePartitions(ctx, o.targetDisk)
	if err != nil {
		return err
	}

	boot, err := o.disk.FindBootPartition(parts)
	if err != nil {
		o.log.Warn("No EFI partition found", "error", err)
	} else {
		o.bootPartition = boot.Node
	}

	root, err := o.findRootPartition(parts)
	if err != nil {
		return err
	}
	o.rootPartition = root.Node
	return nil
}

func (o *Orchestrator) findRootPartition(parts []disk.Partition) (*disk.Partition, error) {
	label := strings.TrimSpace(o.cfg.Provision.Disk.RootPartitionLabel)
	if label != "" {
		return findRootPartitionByLabel(parts, label)
	}
	number := o.cfg.Provision.Disk.RootPartitionNumber
	if number > 0 {
		return findRootPartitionByNumber(parts, o.targetDisk, number)
	}
	return o.disk.FindRootPartition(parts)
}

func findRootPartitionByLabel(parts []disk.Partition, label string) (*disk.Partition, error) {
	var candidate *disk.Partition
	matches := 0
	for i := range parts {
		if !strings.EqualFold(strings.TrimSpace(parts[i].Name), label) {
			continue
		}
		if !isLinuxRootPartition(parts[i]) {
			return nil, fmt.Errorf("root partition label %q matched %s with non-Linux partition type %q", label, parts[i].Node, parts[i].Type)
		}
		candidate = &parts[i]
		matches++
	}
	if matches > 1 {
		return nil, fmt.Errorf("root partition label %q matched %d Linux partitions; set provision.disk.rootPartitionNumber to disambiguate", label, matches)
	}
	if candidate != nil {
		return candidate, nil
	}
	return nil, fmt.Errorf("root partition label %q not found", label)
}

func findRootPartitionByNumber(parts []disk.Partition, targetDisk string, number int) (*disk.Partition, error) {
	for i := range parts {
		if partitionNumberForSelector(parts[i].Node, targetDisk) != number {
			continue
		}
		if !isLinuxRootPartition(parts[i]) {
			return nil, fmt.Errorf("root partition number %d matched %s with non-Linux partition type %q", number, parts[i].Node, parts[i].Type)
		}
		return &parts[i], nil
	}
	return nil, fmt.Errorf("root partition number %d not found", number)
}

func isLinuxRootPartition(part disk.Partition) bool {
	return strings.EqualFold(part.Type, disk.LinuxFilesystemGUID) ||
		strings.EqualFold(part.Type, disk.LinuxFilesystemMBRType)
}

func partitionNumberForSelector(node, targetDisk string) int {
	if targetDisk != "" && strings.HasPrefix(node, targetDisk) {
		return disk.PartitionNumber(node, targetDisk)
	}
	base := filepath.Base(node)
	end := len(base)
	start := end
	for start > 0 && base[start-1] >= '0' && base[start-1] <= '9' {
		start--
	}
	if start == end {
		return 0
	}
	n, err := strconv.Atoi(base[start:end])
	if err != nil {
		return 0
	}
	return n
}

// parsePartitionsFromLayout determines boot/root partitions from the declared
// partition layout instead of scanning GPT type GUIDs.
func (o *Orchestrator) parsePartitionsFromLayout(_ context.Context) error {
	layout := o.cfg.Provision.Disk.PartitionLayout

	if err := o.resolveRootFromLayout(layout); err != nil {
		return err
	}

	// Find boot/EFI partition from the layout.
	// Require an explicit /boot/efi mountpoint to avoid choosing the wrong
	// partition in layouts with multiple vfat filesystems.
	espIdx := -1
	for i, part := range layout.Partitions {
		if part.Mountpoint == "/boot/efi" {
			espIdx = i
			break
		}
	}
	if espIdx == -1 {
		o.log.Warn("no /boot/efi mountpoint found in partition layout; efi boot entry creation may be skipped")
		return nil
	}
	o.bootPartition = disk.PartitionDevicePath(o.targetDisk, espIdx+1)
	o.log.Info("boot partition from layout", "device", o.bootPartition)

	return nil
}

func (o *Orchestrator) resolveRootFromLayout(layout *config.PartitionLayout) error {
	if layout == nil {
		return fmt.Errorf("partition layout is nil")
	}

	// When LVM is configured, use the LV with mountpoint "/" as root.
	if layout.LVM != nil {
		for _, vol := range layout.LVM.Volumes {
			if vol.Mountpoint == "/" {
				o.rootPartition = fmt.Sprintf("/dev/%s/%s", layout.LVM.VolumeGroup, vol.Name)
				o.log.Info("root from lvm", "device", o.rootPartition)
				return nil
			}
		}
	}

	// Find the partition with mountpoint "/" from the layout definition.
	for i, part := range layout.Partitions {
		if part.Mountpoint == "/" {
			o.rootPartition = disk.PartitionDevicePath(o.targetDisk, i+1)
			o.log.Info("root from partition layout", "device", o.rootPartition)
			return nil
		}
	}
	return fmt.Errorf("partition layout has no mountpoint \"/\" in partitions or lvm volumes")
}

func (o *Orchestrator) checkFilesystem(ctx context.Context) error {
	return o.disk.CheckFilesystem(ctx, o.rootPartition)
}

func (o *Orchestrator) enableLVM(ctx context.Context) error {
	return o.disk.EnableLVM(ctx)
}

func (o *Orchestrator) mountRoot(ctx context.Context) error {
	if isMountPoint(newroot) {
		source, err := mountedFromExpectedSource(newroot, o.rootPartition, "root partition")
		if err != nil {
			return err
		}
		o.log.Info("root partition already mounted", "mountpoint", newroot, "source", source)
		return nil
	}
	return o.disk.MountPartition(ctx, o.rootPartition, newroot)
}

func mountedFromExpectedSource(mountpoint, expected, description string) (string, error) {
	source, ok := mountedSource(mountpoint)
	if !ok {
		return "", fmt.Errorf("%s is already mounted but mount source could not be resolved", mountpoint)
	}
	if !sameMountSource(source, expected) {
		return "", fmt.Errorf("%s is already mounted from %s, expected %s %s", mountpoint, source, description, expected)
	}
	return source, nil
}

func sameMountSource(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if actual == expected {
		return true
	}
	actualResolved, actualErr := evalRootSymlinks(actual)
	expectedResolved, expectedErr := evalRootSymlinks(expected)
	return actualErr == nil && expectedErr == nil && actualResolved == expectedResolved
}

func bootEFIMountPoint() string {
	return filepath.Join(newroot, "boot", "efi")
}

func (o *Orchestrator) mountBoot(ctx context.Context) error {
	if strings.TrimSpace(o.bootPartition) == "" {
		o.log.Info("skipping boot partition mount; no boot partition detected")
		return nil
	}
	mountpoint := bootEFIMountPoint()
	if isMountPoint(mountpoint) {
		source, err := mountedFromExpectedSource(mountpoint, o.bootPartition, "boot partition")
		if err != nil {
			return err
		}
		o.log.Info("boot partition already mounted", "mountpoint", mountpoint, "source", source)
		return nil
	}
	if err := mountBootPart(ctx, o.disk, o.bootPartition, mountpoint); err != nil {
		if isUnsupportedBootFilesystemError(err) {
			if ok, reason := efiRuntimeReady(); !ok {
				o.log.Warn("skipping boot partition mount; partition is not a usable ESP and EFI runtime is unavailable",
					"partition", o.bootPartition, "reason", reason, "error", err)
				return nil
			}
		}
		return fmt.Errorf("mounting boot partition %s at %s: %w", o.bootPartition, mountpoint, err)
	}
	return nil
}

func (o *Orchestrator) verifySecureBootChain(_ context.Context) error {
	pins := o.cfg.Provision.SecureBoot.PinnedDigests
	if len(pins) == 0 {
		return nil
	}
	result, err := secureboot.NewChainVerifier(efi.NewEFIVarReader("")).
		WithRoot(secureBootRoot).
		WithPinnedDigests(pins).
		Verify()
	if err != nil {
		return fmt.Errorf("verifying secure boot chain: %w", err)
	}
	var failures []string
	for _, component := range result.Components {
		if component.Error != "" {
			failures = append(failures, fmt.Sprintf("%s: %s", component.Name, component.Error))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("secure boot chain verification failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

type sharedDataMount struct {
	device     string
	mountpoint string
	label      string
}

type copiedPathMetadata struct {
	path     string
	mode     os.FileMode
	modTime  time.Time
	uid      int
	gid      int
	hasOwner bool
}

func (o *Orchestrator) mountSharedData(ctx context.Context) error {
	if !o.isSystemABMode() {
		return nil
	}
	mounts := o.sharedDataMountsFromLayout()
	for _, m := range mounts {
		target := filepath.Join(newroot, strings.TrimPrefix(m.mountpoint, "/"))
		if err := ensureWithinRoot(newroot, target); err != nil {
			return fmt.Errorf("shared data mount %s: %w", m.mountpoint, err)
		}
		if isMountPoint(target) {
			source, err := mountedFromExpectedSource(target, m.device, "shared data partition")
			if err != nil {
				return err
			}
			o.log.Info("shared data partition already mounted", "label", m.label, "mountpoint", target, "source", source)
			o.recordSharedMount(target)
			continue
		}
		if !o.cfg.Provision.AB.PreserveExisting {
			if err := o.seedSharedDataPartition(ctx, m.device, target); err != nil {
				return fmt.Errorf("seed shared data partition %s: %w", m.label, err)
			}
		}
		if err := mountSharedDataPart(ctx, o.disk, m.device, target); err != nil {
			return fmt.Errorf("mount shared data partition %s at %s: %w", m.device, target, err)
		}
		o.recordSharedMount(target)
		o.log.Info("mounted shared data partition", "label", m.label, "device", m.device, "mountpoint", target)
	}
	return nil
}

func (o *Orchestrator) isSystemABMode() bool {
	return o.isABImageMode() && o.cfg.Provision.AB.WithDefaults().Scheme == config.ABSchemeSystemAB
}

func (o *Orchestrator) sharedDataMountsFromLayout() []sharedDataMount {
	layout := o.cfg.Provision.Disk.PartitionLayout
	if layout == nil {
		return nil
	}
	var mounts []sharedDataMount
	for i := range layout.Partitions {
		part := &layout.Partitions[i]
		if !isSharedDataPartition(part) {
			continue
		}
		mounts = append(mounts, sharedDataMount{
			device:     disk.PartitionDevicePath(o.targetDisk, i+1),
			mountpoint: part.Mountpoint,
			label:      part.Label,
		})
	}
	return mounts
}

func isSharedDataPartition(part *config.Partition) bool {
	mountpoint := strings.TrimSpace(part.Mountpoint)
	if mountpoint == "" || mountpoint == "/" || mountpoint == "/boot/efi" {
		return false
	}
	return !strings.EqualFold(part.Filesystem, "swap")
}

func (o *Orchestrator) recordSharedMount(mountpoint string) {
	for _, existing := range o.sharedMounts {
		if existing == mountpoint {
			return
		}
	}
	o.sharedMounts = append(o.sharedMounts, mountpoint)
}

func (o *Orchestrator) seedSharedDataPartition(ctx context.Context, device, target string) error {
	if exists, err := validateSharedDataSeedSource(target); err != nil {
		return err
	} else if !exists {
		return nil
	}
	seedMount, err := os.MkdirTemp(newroot, ".booty-data-seed-*")
	if err != nil {
		return fmt.Errorf("create seed mountpoint: %w", err)
	}
	mounted := false
	defer func() { o.cleanupSharedDataSeedMount(seedMount, mounted) }()
	if err := mountSharedDataPart(ctx, o.disk, device, seedMount); err != nil {
		return fmt.Errorf("mount seed target %s: %w", device, err)
	}
	mounted = true
	state, err := seedDirectoryState(seedMount)
	if err != nil {
		return err
	}
	switch state {
	case seedStateExistingContent:
		o.log.Info("shared data partition already has content, skipping seed", "device", device)
		return nil
	case seedStateInProgress:
		o.log.Warn("shared data seed was interrupted, cleaning and retrying", "device", device)
		if err := cleanInterruptedSeed(seedMount); err != nil {
			return err
		}
	case seedStateEmpty:
	}
	if err := writeSeedInProgressMarker(seedMount); err != nil {
		return err
	}
	if err := copyTreeWithSymlinks(ctx, target, seedMount); err != nil {
		return err
	}
	return removeSeedInProgressMarker(seedMount)
}

func (o *Orchestrator) cleanupSharedDataSeedMount(seedMount string, mounted bool) {
	if mounted {
		if err := unmountSharedDataPart(o.disk, seedMount); err != nil {
			o.log.Warn("failed to unmount shared data seed target; leaving mountpoint intact", "mountpoint", seedMount, "error", err)
			return
		}
	}
	if err := os.RemoveAll(seedMount); err != nil {
		o.log.Warn("failed to remove shared data seed mountpoint", "mountpoint", seedMount, "error", err)
	}
}

type seedState int

const (
	seedStateEmpty seedState = iota
	seedStateExistingContent
	seedStateInProgress
)

func directoryEmptyForSeed(path string) (bool, error) {
	state, err := seedDirectoryState(path)
	if err != nil {
		return false, err
	}
	return state == seedStateEmpty, nil
}

func seedDirectoryState(path string) (seedState, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return seedStateExistingContent, fmt.Errorf("read seed directory %s: %w", path, err)
	}
	hasExistingContent := false
	hasInProgressMarker := false
	for _, entry := range entries {
		if entry.Name() == "lost+found" {
			continue
		}
		if entry.Name() == sharedDataSeedInProgressMarker {
			markerValid, err := validSeedInProgressMarker(filepath.Join(path, entry.Name()))
			if err != nil {
				return seedStateExistingContent, err
			}
			if markerValid {
				hasInProgressMarker = true
				continue
			}
		}
		hasExistingContent = true
	}
	if hasInProgressMarker {
		return seedStateInProgress, nil
	}
	if hasExistingContent {
		return seedStateExistingContent, nil
	}
	return seedStateEmpty, nil
}

func validSeedInProgressMarker(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("stat seed marker %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read seed marker %s: %w", path, err)
	}
	return string(data) == sharedDataSeedInProgressContent, nil
}

func cleanInterruptedSeed(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read interrupted seed directory %s: %w", path, err)
	}
	for _, entry := range entries {
		if entry.Name() == "lost+found" {
			continue
		}
		entryPath := filepath.Join(path, entry.Name())
		if err := os.RemoveAll(entryPath); err != nil {
			return fmt.Errorf("remove interrupted seed entry %s: %w", entryPath, err)
		}
	}
	return nil
}

func writeSeedInProgressMarker(path string) error {
	markerPath := filepath.Join(path, sharedDataSeedInProgressMarker)
	if err := os.WriteFile(markerPath, []byte(sharedDataSeedInProgressContent), 0o600); err != nil {
		return fmt.Errorf("write shared data seed marker %s: %w", markerPath, err)
	}
	return nil
}

func removeSeedInProgressMarker(path string) error {
	markerPath := filepath.Join(path, sharedDataSeedInProgressMarker)
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove shared data seed marker %s: %w", markerPath, err)
	}
	return nil
}

func copyTreeWithSymlinks(ctx context.Context, srcBase, destRoot string) error {
	if exists, err := validateSharedDataSeedSource(srcBase); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("seed source %s does not exist", srcBase)
	}
	cleanSrc, err := filepath.Abs(srcBase)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}
	cleanDest, err := filepath.Abs(destRoot)
	if err != nil {
		return fmt.Errorf("resolve dest root: %w", err)
	}
	var dirs []copiedPathMetadata
	if err := filepath.WalkDir(cleanSrc, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy tree canceled: %w", err)
		}
		relPath, err := filepath.Rel(cleanSrc, path)
		if err != nil {
			return fmt.Errorf("resolve shared data seed path %s relative to %s: %w", path, cleanSrc, err)
		}
		destPath := filepath.Join(cleanDest, relPath)
		if err := ensureWithinRoot(cleanDest, destPath); err != nil {
			return fmt.Errorf("shared data seed path %s: %w", relPath, err)
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			dirs = append(dirs, copiedPathMetadataFromInfo(destPath, info))
		}
		return copyTreeEntry(ctx, path, destPath, d)
	}); err != nil {
		return fmt.Errorf("copy shared data tree %s -> %s: %w", srcBase, destRoot, err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := applyPathMetadata(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateSharedDataSeedSource(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat seed source %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("seed source %s must be a directory, got symlink", path)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("seed source %s must be a directory", path)
	}
	return true, nil
}

func copyTreeEntry(ctx context.Context, src, dst string, d os.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	switch mode := info.Mode(); {
	case mode&os.ModeSymlink != 0:
		return copySymlink(src, dst)
	case d.IsDir():
		if err := os.MkdirAll(dst, metadataMode(mode)); err != nil {
			return fmt.Errorf("create directory %s: %w", dst, err)
		}
		if err := os.Chmod(dst, metadataMode(mode)); err != nil {
			return fmt.Errorf("set directory mode %s: %w", dst, err)
		}
		return nil
	case mode.IsRegular():
		return copyFile(ctx, src, dst)
	default:
		slog.Warn("skipping unsupported shared data seed entry", "path", src, "mode", mode.String())
		return nil
	}
}

func metadataMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func copiedPathMetadataFromInfo(path string, info os.FileInfo) copiedPathMetadata {
	meta := copiedPathMetadata{
		path:    path,
		mode:    metadataMode(info.Mode()),
		modTime: info.ModTime(),
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		meta.uid = int(stat.Uid)
		meta.gid = int(stat.Gid)
		meta.hasOwner = true
	}
	return meta
}

func applyPathMetadata(meta copiedPathMetadata) error {
	if meta.hasOwner {
		if err := os.Chown(meta.path, meta.uid, meta.gid); err != nil {
			return fmt.Errorf("set owner %s: %w", meta.path, err)
		}
	}
	if err := os.Chmod(meta.path, meta.mode); err != nil {
		return fmt.Errorf("set mode %s: %w", meta.path, err)
	}
	if err := os.Chtimes(meta.path, meta.modTime, meta.modTime); err != nil {
		return fmt.Errorf("set timestamps %s: %w", meta.path, err)
	}
	return nil
}

func copySymlink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("read symlink %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create symlink parent %s: %w", dst, err)
	}
	if existing, err := os.Lstat(dst); err == nil {
		if existing.IsDir() {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("replace symlink destination %s: %w", dst, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat symlink destination %s: %w", dst, err)
	}
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("create symlink %s: %w", dst, err)
	}
	return nil
}

func isUnsupportedBootFilesystemError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, ": tried ") &&
		(strings.Contains(msg, "invalid argument") || strings.Contains(msg, "wrong fs type") || strings.Contains(msg, "no such device"))
}

func (o *Orchestrator) applySysexts(ctx context.Context) error {
	return o.config.ApplySysexts(ctx, &o.cfg.Provision.Sysext)
}

func (o *Orchestrator) setupChrootBinds(_ context.Context) error {
	return o.disk.SetupChrootBindMounts(newroot)
}

func (o *Orchestrator) growPartition(ctx context.Context) error {
	if o.cfg.Provision.Disk.PartitionLayout != nil {
		o.log.Info("skipping grow-partition for declarative partition layout")
		return nil
	}

	partNum, err := disk.PartitionNumberChecked(o.rootPartition, o.targetDisk)
	if err != nil {
		return fmt.Errorf("determine root partition number: %w", err)
	}
	return o.disk.GrowPartition(ctx, o.targetDisk, partNum)
}

func (o *Orchestrator) resizeFilesystem(ctx context.Context) error {
	return o.disk.ResizeFilesystem(ctx, o.rootPartition, newroot)
}

func (o *Orchestrator) configureKubelet(_ context.Context) error {
	return o.config.ConfigureKubelet(o.cfg)
}

func (o *Orchestrator) configureGRUB(ctx context.Context) error {
	return o.config.ConfigureGRUB(ctx, o.cfg)
}

func (o *Orchestrator) installEFIFallbackLoader(ctx context.Context) error {
	if !o.isABImageMode() {
		return nil
	}
	if o.cfg.Provision.AB.PreserveExisting {
		o.log.Info("A/B preserveExisting enabled, preserving existing EFI fallback loader")
		return nil
	}
	if strings.TrimSpace(o.bootPartition) == "" {
		o.log.Info("skipping EFI fallback loader installation; no boot partition detected")
		return nil
	}
	mountpoint := bootEFIMountPoint()
	if !isMountPoint(mountpoint) {
		return fmt.Errorf("cannot install EFI fallback loader: boot partition %s is not mounted at %s", o.bootPartition, mountpoint)
	}
	return o.config.InstallEFIFallbackLoader(ctx, o.targetDisk, o.rootPartition)
}

func (o *Orchestrator) injectCloudInit(_ context.Context) error {
	if !o.cfg.Provision.CloudInit.Enabled {
		return nil
	}

	ds, err := cloudInitDatasource(o.cfg.Provision.CloudInit.Datasource)
	if err != nil {
		return err
	}

	bondIfaces := splitTrimmedCSV(o.cfg.Network.Bond.Interfaces)
	vlans, err := cloudInitVLANInputs(o.cfg.Network.VLAN.Config)
	if err != nil {
		return err
	}
	if err := validateCloudInitNetworkTarget(o.cfg.Network.Static.Iface, bondIfaces, vlans); err != nil {
		return err
	}

	// Cloud-init expects a stable, non-empty instance-id for first-boot identity.
	instanceID := strings.TrimSpace(o.cfg.Provision.ProviderID)
	if instanceID == "" {
		instanceID = strings.TrimSpace(o.cfg.Hostname)
	}
	if instanceID == "" {
		instanceID = "booty"
	}

	ciCfg := &cloudinit.Config{
		Hostname:   o.cfg.Hostname,
		InstanceID: instanceID,
		StaticIP:   o.cfg.Network.Static.IP,
		Interface:  o.cfg.Network.Static.Iface,
		Gateway:    o.cfg.Network.Static.Gateway,
		BondIfaces: bondIfaces,
		BondMode:   o.cfg.Network.Bond.Mode,
		DNS:        splitTrimmedCSV(o.cfg.Network.DNSResolvers),
		VLANs:      vlans,
	}

	ud, md, nc := cloudinit.Generate(ciCfg)
	rootPath := o.config.rootDir

	var injectErr error
	switch ds {
	case "nocloud":
		injectErr = cloudinit.InjectNoCloud(rootPath, ud, md, nc)
	case "configdrive":
		injectErr = cloudinit.InjectConfigDrive(rootPath, ud, md, nc)
	}
	if injectErr != nil {
		return fmt.Errorf("inject cloud-init: %w", injectErr)
	}
	o.log.Info("cloud-init seed injected", "datasource", ds, "root", rootPath)
	return nil
}

func cloudInitVLANInputs(spec string) ([]cloudinit.VLANInput, error) {
	vlans, err := network.ParseVLANs(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid cloud-init VLAN config: %w", err)
	}
	inputs := make([]cloudinit.VLANInput, 0, len(vlans))
	for _, vlan := range vlans {
		inputs = append(inputs, cloudinit.VLANInput{
			ID:      vlan.ID,
			Parent:  vlan.Parent,
			Address: vlan.Address,
			Gateway: vlan.Gateway,
		})
	}
	return inputs, nil
}

func cloudInitDatasource(raw string) (string, error) {
	ds := strings.ToLower(strings.TrimSpace(raw))
	if ds == "" {
		return "nocloud", nil
	}
	if ds != "nocloud" && ds != "configdrive" {
		return "", fmt.Errorf("unsupported cloud-init datasource %q", ds)
	}
	return ds, nil
}

func validateCloudInitNetworkTarget(iface string, bondIfaces []string, vlans []cloudinit.VLANInput) error {
	if len(bondIfaces) == 0 && strings.TrimSpace(iface) == "" && len(vlans) == 0 {
		return fmt.Errorf("cloud-init network config requires STATIC_IFACE when no bond interfaces are configured")
	}
	return nil
}

func splitTrimmedCSV(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func (o *Orchestrator) copyMachineFiles(ctx context.Context) error {
	return o.config.CopyMachineFiles(ctx)
}

func (o *Orchestrator) runMachineCommands(ctx context.Context) error {
	return o.config.RunMachineCommands(ctx)
}

func (o *Orchestrator) runPostProvisionCmds(ctx context.Context) error {
	if len(o.cfg.Provision.PostProvisionCmds) == 0 {
		return nil
	}
	return o.config.RunPostProvisionCmds(ctx, o.cfg.Provision.PostProvisionCmds)
}

func (o *Orchestrator) prepareOCIPrePulls(ctx context.Context) error {
	return o.config.PrepareOCIPrePulls(ctx, &o.cfg.Provision.OCIPrePulls)
}

func (o *Orchestrator) teardownChroot(_ context.Context) error {
	if o.shouldKeepChrootMountedForKexec() {
		o.log.Info("keeping target root mounted for kexec", "root", newroot)
		return nil
	}
	bindErr := o.disk.TeardownChrootBindMounts(newroot)
	bootErr := o.unmountBoot()
	sharedErr := o.unmountSharedData()
	unmountErr := o.disk.Unmount(newroot)
	return errors.Join(bindErr, bootErr, sharedErr, unmountErr)
}

func (o *Orchestrator) shouldKeepChrootMountedForKexec() bool {
	return ShouldKeepTargetRootMountedForKexec(o.cfg, o.firmwareChanged)
}

// ShouldKeepTargetRootMountedForKexec mirrors the main kexec gates so teardown
// only preserves /newroot when the following exit path can attempt kexec.
func ShouldKeepTargetRootMountedForKexec(cfg *config.MachineConfig, firmwareChanged bool) bool {
	if cfg == nil {
		return false
	}
	return !cfg.Provision.DisableKexec &&
		!cfg.Provision.SecureBoot.ReEnable &&
		!firmwareChanged
}

func (o *Orchestrator) unmountBoot() error {
	if strings.TrimSpace(o.bootPartition) == "" {
		return nil
	}
	mountpoint := bootEFIMountPoint()
	if !isMountPoint(mountpoint) {
		return nil
	}
	if err := o.disk.Unmount(mountpoint); err != nil {
		return fmt.Errorf("unmount boot mountpoint %s: %w", mountpoint, err)
	}
	return nil
}

func (o *Orchestrator) unmountSharedData() error {
	var errs []error
	for i := len(o.sharedMounts) - 1; i >= 0; i-- {
		mountpoint := o.sharedMounts[i]
		if !isMountPoint(mountpoint) {
			continue
		}
		if err := o.disk.Unmount(mountpoint); err != nil {
			errs = append(errs, fmt.Errorf("unmount shared data mountpoint %s: %w", mountpoint, err))
		}
	}
	return errors.Join(errs...)
}

func (o *Orchestrator) runHealthChecks(ctx context.Context) error {
	if !o.cfg.Health.Enabled {
		o.log.Info("Health checks disabled, skipping")
		return nil
	}

	results, critical := health.RunAll(ctx, o.healthChecks(), o.cfg.Health.SkipChecks)

	o.logHealthCheckResults(results)

	// Best-effort report to server.
	if reporter, ok := o.provider.(HealthReporter); ok {
		if err := reporter.ReportHealthChecks(ctx, results); err != nil {
			o.log.Warn("failed to report health checks", "error", err)
		}
	}

	if critical {
		var failed []string
		for _, r := range results {
			if r.Status == health.StatusFail && r.Severity == health.SeverityCritical {
				failed = append(failed, r.Name)
			}
		}
		return fmt.Errorf("critical health check(s) failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (o *Orchestrator) healthChecks() []health.Check {
	return []health.Check{
		&health.DiskPresenceCheck{},
		&health.DiskIOErrorCheck{},
		&health.MemoryECCCheck{},
		&health.MinimumMemoryCheck{MinGiB: o.cfg.Health.MinMemoryGB},
		&health.MinimumCPUCheck{MinCPUs: o.cfg.Health.MinCPUs},
		&health.NICLinkStateCheck{},
		&health.ThermalStateCheck{},
	}
}

func (o *Orchestrator) logHealthCheckResults(results []health.CheckResult) {
	for _, r := range results {
		logAttrs := []any{
			"check", r.Name,
			"status", r.Status,
			"severity", r.Severity,
			"message", r.Message,
		}
		if r.Details != "" {
			logAttrs = append(logAttrs, "details", r.Details)
		}
		o.log.Info("Health check result", logAttrs...)
	}
}

func (o *Orchestrator) reportSuccess(ctx context.Context) error {
	return o.reportTerminalSuccess(ctx, "provisioning", successStatusMessage(o.cfg))
}

func (o *Orchestrator) reportTerminalSuccess(ctx context.Context, operation, message string) error {
	if err := o.provider.ReportStatus(ctx, config.StatusSuccess, message); err != nil {
		return fmt.Errorf("failed to report terminal success status for %s: %w", operation, err)
	}
	return nil
}

func successStatusMessage(cfg *config.MachineConfig) string {
	message := "provisioning complete"
	if cfg != nil && cfg.Provision.SecureBoot.ReEnable {
		message += " SECUREBOOT_REENABLE=true"
	}
	return message
}

// DumpDebugState logs system state useful for diagnosing failures.
// BOOTy runs as PID 1 in an initramfs — this dump is the only diagnostic
// data available before reboot, so it must be comprehensive.
// Step-specific debug commands are run first, followed by comprehensive dump.
// Automatically detects whether FRR (vtysh) or GoBGP is in use and runs
// the appropriate network diagnostics.
func DumpDebugState(failedStep string) {
	slog.Warn("=== DEBUG DUMP START ===", "failedStep", failedStep)

	// PATH and available binaries — critical for diagnosing missing-binary issues.
	executil.DumpPATH()

	// Shared library availability — dynamically-linked tools fail silently without these.
	runDebugCmd("shared libs", "ls -la /lib64/ld-linux-x86-64.so* /lib/ld-linux-x86-64.so* /lib/x86_64-linux-gnu/lib*.so* /usr/lib/x86_64-linux-gnu/lib*.so* /lib64/ld-linux-aarch64.so* /lib/ld-linux-aarch64.so* /lib/aarch64-linux-gnu/lib*.so* /usr/lib/aarch64-linux-gnu/lib*.so* 2>/dev/null | head -40 || echo 'no shared libs found'")
	runDebugCmd("ld.so.cache", "ldconfig -p 2>/dev/null | head -20 || echo 'ldconfig not available'")

	// Step-specific commands run first for targeted diagnostics.
	stepCmds := stepDebugCmds(failedStep)
	for _, dc := range stepCmds {
		runDebugCmd(dc.label, dc.cmd)
	}

	debugCmds := []struct {
		label string
		cmd   string
	}{
		// Block devices & disk subsystem.
		{"block devices", "lsblk -a"},
		{"mounts", "cat /proc/mounts"},
		{"memory", "cat /proc/meminfo"},
		{"disk partitions", "cat /proc/partitions"},
		{"mdstat", "cat /proc/mdstat"},
		{"df", "df -h"},
		{"pvs", "pvs"},
		{"lvs", "lvs"},

		// Kernel messages.
		{"dmesg tail", "dmesg | tail -100"},

		// Network interfaces & routes (IPv4 + IPv6).
		{"network interfaces", "ip -br addr"},
		{"interface stats", "ip -s link"},
		{"routes v4", "ip route"},
		{"routes v6", "ip -6 route"},
		{"bridge fdb", "bridge fdb show"},
		{"vxlan interfaces", "ip link show type vxlan"},
	}

	for _, dc := range debugCmds {
		runDebugCmd(dc.label, dc.cmd)
	}

	// Network mode–specific diagnostics: FRR (vtysh) vs GoBGP (in-process).
	if hasBinary("vtysh") {
		frrDebugCmds(failedStep)
	} else {
		gobgpDebugCmds()
	}

	// Log environment (redact sensitive values).
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "BOOTY_") || strings.HasPrefix(env, "MODE=") ||
			strings.HasPrefix(env, "NETWORK_MODE=") {
			key, _, _ := strings.Cut(env, "=")
			if isSensitiveEnvKey(key) {
				slog.Warn("debug env", "var", key+"=REDACTED")
			} else {
				slog.Warn("debug env", "var", env)
			}
		}
	}

	slog.Warn("=== DEBUG DUMP END ===", "failedStep", failedStep)
}

// hasBinary reports whether the named binary exists in PATH.
func hasBinary(name string) bool {
	_, err := executil.LookPath(name)
	return err == nil
}

// isSensitiveEnvKey returns true if the key likely contains credentials.
func isSensitiveEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, s := range []string{"TOKEN", "SECRET", "PASSWORD", "KEY", "CREDENTIAL"} {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}

// frrDebugCmds dumps FRR-specific state using vtysh.
func frrDebugCmds(_ string) {
	cmds := []debugCmd{
		{"frr config", "cat /etc/frr/frr.conf"},
		{"frr daemons", "pgrep -la 'bgpd|zebra|bfdd|mgmtd|staticd'"},
		{"frr log tail", "tail -100 /var/log/frr/frr.log"},
		{"bgp summary", "vtysh -c 'show bgp summary'"},
		{"bgp ipv4", "vtysh -c 'show bgp ipv4 unicast'"},
		{"bgp ipv6", "vtysh -c 'show bgp ipv6 unicast'"},
		{"bgp l2vpn evpn", "vtysh -c 'show bgp l2vpn evpn'"},
		{"bfd peers", "vtysh -c 'show bfd peers'"},
		{"frr interfaces", "vtysh -c 'show interface brief'"},
	}
	for _, dc := range cmds {
		runDebugCmd(dc.label, dc.cmd)
	}
}

// gobgpDebugCmds dumps network state available without FRR/vtysh.
// GoBGP runs in-process so there is no CLI to query; instead we dump
// kernel state that reflects what the GoBGP stack has programmed.
func gobgpDebugCmds() {
	slog.Warn("debug", "label", "network-mode", "data", "GoBGP (in-process, no vtysh)")
	cmds := []debugCmd{
		// VRF state — GoBGP programs routes into a VRF.
		{"vrf devices", "ip -d link show type vrf"},
		{"vrf routes", "ip route show vrf provision 2>/dev/null || ip route show table all"},
		{"vrf neighbors", "ip neigh show vrf provision 2>/dev/null || ip neigh show"},
		// VXLAN tunnel state.
		{"vxlan details", "ip -d link show type vxlan"},
		{"vxlan fdb", "bridge fdb show dev vx100 2>/dev/null || true"},
		// Bridge state.
		{"bridge links", "ip -d link show type bridge"},
		{"bridge vlan", "bridge vlan show 2>/dev/null || true"},
		// General neighbor/ARP state.
		{"arp table", "ip neigh show"},
		// Routing tables.
		{"all routes", "ip route show table all 2>/dev/null | head -80"},
	}
	for _, dc := range cmds {
		runDebugCmd(dc.label, dc.cmd)
	}
}

// debugCtx returns a context with a 10-second timeout for debug commands,
// preventing them from blocking shutdown indefinitely.
func debugCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second) //nolint:mnd // fixed debug timeout
}

// runDebugCmd executes a single debug command and logs its output.
func runDebugCmd(label, cmd string) {
	ctx, cancel := debugCtx()
	defer cancel()
	out, err := executil.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput() //nolint:gosec // debug cmds are hardcoded
	trimmed := strings.TrimSpace(string(out))
	if trimmed != "" {
		for _, line := range strings.Split(trimmed, "\n") {
			if line != "" {
				slog.Warn("debug", "label", label, "data", redactDebugData(line))
			}
		}
	}
	if err != nil {
		slog.Warn("debug command failed", "label", label, "cmd", cmd, "error", err)
	}
}

type debugCmd struct {
	label string
	cmd   string
}

// stepDebugCmds returns step-specific debug commands for targeted diagnostics.
func stepDebugCmds(step string) []debugCmd {
	switch step {
	case "detect-disk":
		return []debugCmd{
			{"sysblock entries", "ls -la /sys/block/"},
			{"sysblock sizes", "for d in /sys/block/*/size; do echo \"$d: $(cat $d)\"; done"},
			{"dev devices", "ls -la /dev/sd* /dev/nvme* /dev/vd* /dev/loop* 2>/dev/null || true"},
			{"loaded modules", "lsmod 2>/dev/null || cat /proc/modules | head -30"},
			{"scsi devices", "cat /proc/scsi/scsi 2>/dev/null || echo 'no SCSI info'"},
		}
	case "wipe-disks":
		return []debugCmd{
			{"wipefs version", "wipefs --version 2>&1 || echo 'wipefs not available'"},
			{"sgdisk version", "sgdisk --version 2>&1 || echo 'sgdisk not available'"},
			{"shared libs wipefs", "ldd $(which wipefs 2>/dev/null) 2>&1 || echo 'ldd/wipefs not found'"},
			{"shared libs sgdisk", "ldd $(which sgdisk 2>/dev/null) 2>&1 || echo 'ldd/sgdisk not found'"},
			{"ld.so check", "ls -la /lib64/ld-linux-x86-64.so.2 /lib/ld-linux-x86-64.so.2 /lib64/ld-linux-aarch64.so.1 /lib/ld-linux-aarch64.so.1 2>/dev/null || echo 'dynamic linker not found'"},
			{"dev devices", "ls -la /dev/sd* /dev/nvme* /dev/vd* 2>/dev/null || true"},
		}
	case "setup-raid":
		return []debugCmd{
			{"mdadm version", "mdadm --version 2>&1 || echo 'mdadm not available'"},
			{"mdstat", "cat /proc/mdstat 2>/dev/null || echo 'no mdstat'"},
			{"mdadm scan", "mdadm --detail --scan 2>&1 || true"},
			{"dev md devices", "ls -la /dev/md* /dev/md/* 2>/dev/null || true"},
			{"candidate disks", "ls -la /dev/sd* /dev/nvme* /dev/vd* 2>/dev/null || true"},
		}
	case "parse-partitions", "apply-partition-layout":
		return []debugCmd{
			{"sfdisk version", "sfdisk --version 2>&1 || echo 'sfdisk not found'"},
			{"sfdisk raw", "for d in /dev/sd[a-z] /dev/nvme*n1 /dev/vd[a-z]; do if [ -b \"$d\" ]; then sfdisk --json \"$d\"; break; fi; done 2>&1 | head -30 || true"},
			{"fdisk list", "fdisk -l 2>/dev/null | head -40 || true"},
			{"partitions", "cat /proc/partitions"},
			{"shared libs sfdisk", "ldd $(which sfdisk) 2>&1 || echo 'ldd not found'"},
		}
	case "stream-image":
		return []debugCmd{
			{"target disk info", "fdisk -l 2>/dev/null | head -30 || true"},
			{"disk space", "df -h"},
			{"partitions", "cat /proc/partitions"},
		}
	case "mount-root", "mount-boot", "apply-sysexts", "setup-chroot-binds":
		return []debugCmd{
			{"proc mounts", "cat /proc/mounts"},
			{"newroot contents", "ls -la /newroot/ 2>/dev/null || echo '/newroot not found'"},
		}
	case "configure-grub", "run-machine-commands", "run-post-provision-cmds", "configure-kubelet":
		return []debugCmd{
			{"chroot bin", "ls /newroot/bin/ /newroot/usr/bin/ 2>/dev/null | head -50 || true"},
			{"chroot boot", "ls -la /newroot/boot/ 2>/dev/null || echo '/newroot/boot not found'"},
			{"chroot mounts", "cat /proc/mounts | grep newroot || true"},
		}
	case "remove-efi-entries", "create-efi-boot-entry", "mount-efivarfs":
		return []debugCmd{
			{"efivarfs", "ls /sys/firmware/efi/efivars/ 2>/dev/null | head -20 || echo 'no EFI'"},
			{"efibootmgr", "efibootmgr -v 2>/dev/null || echo 'efibootmgr not available'"},
			{"proc mounts efi", "grep efi /proc/mounts 2>/dev/null || echo 'no efi mounts'"},
		}
	default:
		return nil
	}
}

// dumpConfig logs the parsed machine configuration on failure.
// Token is excluded to avoid leaking credentials.
func dumpConfig(cfg *config.MachineConfig) {
	if cfg == nil {
		return
	}
	slog.Warn("=== CONFIG DUMP ===",
		"hostname", cfg.Hostname,
		"mode", cfg.Mode,
		"images", redactURLs(cfg.Provision.Image.URLs),
		"asn", cfg.Network.BGP.ASN,
		"provision_vni", cfg.Network.EVPN.ProvisionVNI,
		"underlay_subnet", cfg.Network.EVPN.UnderlaySubnet,
		"underlay_ip", cfg.Network.EVPN.UnderlayIP,
		"overlay_subnet", cfg.Network.EVPN.OverlaySubnet,
		"ipmi_subnet", cfg.Network.IPMI.Subnet,
		"dcgw_ips", cfg.Network.EVPN.DCGWIPs,
		"leaf_asn", cfg.Network.EVPN.LeafASN,
		"local_asn", cfg.Network.EVPN.LocalASN,
		"vpn_rt", cfg.Network.EVPN.VPNRT,
		"overlay_aggregate", cfg.Network.EVPN.OverlayAggregate,
		"provision_ip", cfg.Network.EVPN.ProvisionIP,
		"dns_resolver", cfg.Network.DNSResolvers,
		"vrf_table_id", cfg.Network.VRF.TableID,
		"bgp_keepalive", cfg.Network.BGP.Keepalive,
		"bgp_hold", cfg.Network.BGP.Hold,
		"bgp_interfaces", cfg.Network.BGP.Interfaces,
		"bfd_transmit_ms", cfg.Network.BGP.BFDTransmitMS,
		"bfd_receive_ms", cfg.Network.BGP.BFDReceiveMS,
		"static_ip", cfg.Network.Static.IP,
		"static_gateway", cfg.Network.Static.Gateway,
		"bond_interfaces", cfg.Network.Bond.Interfaces,
		"min_disk_size_gb", cfg.Provision.Disk.MinSizeGB,
	)
}

// redactURLs strips embedded credentials, query parameters, and fragments from
// image URLs before they are written to debug logs.
func redactURLs(urls []string) []string {
	redacted := make([]string, len(urls))
	for i, raw := range urls {
		redacted[i] = image.RedactURL(raw)
	}
	return redacted
}

func (o *Orchestrator) flushTelemetry(ctx context.Context) {
	o.flushMetrics(ctx)
	o.flushLastEvent(ctx)
}

func (o *Orchestrator) flushMetrics(ctx context.Context) {
	if tr, ok := o.provider.(config.TelemetryReporter); ok {
		if o.collector != nil {
			if data, err := o.collector.MarshalSummary(); err == nil {
				_ = tr.ReportMetrics(ctx, data)
			}
		}
	}
}

func (o *Orchestrator) flushLastEvent(ctx context.Context) {
	er, ok := o.provider.(config.EventReporter)
	if !ok || o.emitter == nil {
		return
	}
	ev := o.emitter.LastEvent()
	if ev == nil {
		return
	}
	if data, err := telemetry.MarshalEvent(ev); err == nil {
		_ = er.SendEvent(ctx, data)
	}
}
