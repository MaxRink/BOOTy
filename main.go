//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/crash"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/executil"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/kexec"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frr"
	"github.com/telekom/BOOTy/pkg/network/gobgp"
	"github.com/telekom/BOOTy/pkg/network/netplan"
	"github.com/telekom/BOOTy/pkg/network/vlan"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/realm"
	"github.com/telekom/BOOTy/pkg/runmode"
	"github.com/telekom/BOOTy/pkg/ux"
)

// Version and Build are set via -ldflags at build time.
var (
	originalSlogHandler   slog.Handler
	caprfRemoteLogHandler *caprf.RemoteHandler
	Version               = "dev"
	Build                 = "unknown"
)

var (
	setupBondLayer = func(ctx context.Context, cfg *network.Config) (network.Mode, error) {
		return setupBondMode(ctx, cfg, &network.BondMode{})
	}
	setupVLANLayer = func(v network.VLANConfig) (string, error) {
		return vlan.Setup(&vlan.Config{
			ID:      v.ID,
			Parent:  v.Parent,
			Address: v.Address,
			Gateway: v.Gateway,
		})
	}
	teardownVLANLayer = func(v network.VLANConfig) error {
		return vlan.TeardownConfig(&vlan.Config{
			ID:     v.ID,
			Parent: v.Parent,
		})
	}
	provisionHandoff = provisionHandoffOps{
		reboot:   requestReboot,
		powerOff: requestPowerOff,
		shell:    shell,
		exit:     os.Exit,
	}
)

func setupBondMode(ctx context.Context, cfg *network.Config, bond network.Mode) (network.Mode, error) {
	if err := bond.Setup(ctx, cfg); err != nil {
		if cleanupErr := bond.Teardown(ctx); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("bond rollback: %w", cleanupErr))
		}
		return nil, err
	}
	return bond, nil
}

const (
	varsPath          = "/deploy/vars"
	installedRootPath = "/newroot"
)

type provisionStatusReporter interface {
	ReportStatus(ctx context.Context, status config.Status, message string) error
}

type provisionHandoffOps struct {
	reboot   func() error
	powerOff func() error
	shell    func()
	exit     func(int)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Ensure PATH includes standard binary directories. As PID 1 in an
	// initramfs the kernel default may only contain /sbin:/bin; make sure
	// /usr/bin, /usr/sbin, and /usr/local/bin are also reachable.
	ensurePATH("/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin")
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	if err := setupMountsAndDevices(); err != nil {
		slog.Error("early mount/device setup failed", "error", err)
		reboot()
		return
	}
	startPID1ChildReaper(ctx, os.Getpid())
	loadModules()

	slog.Info("starting BOOTy", "version", Version, "build", Build)
	ux.Captain()
	ux.SysInfo()

	slog.Info("beginning provisioning process")
	runCAPRF(ctx)
}

const orphanReapDelay = 500 * time.Millisecond

type waitExitedChildFunc func(int) (int, syscall.WaitStatus, error)

type childReaper struct {
	childPIDs func() ([]int, error)
	managed   func(int) bool
	wait      waitExitedChildFunc
}

type signalNotifyFunc func(chan<- os.Signal, ...os.Signal)
type signalStopFunc func(chan<- os.Signal)

// startPID1ChildReaper reaps daemonized children that get adopted by BOOTy as
// PID 1. It only waits unmanaged child PIDs so normal exec.Cmd owners keep
// exclusive ownership of their child exit statuses.
func startPID1ChildReaper(ctx context.Context, pid int) bool {
	return startPID1ChildReaperWith(
		ctx,
		pid,
		orphanReapDelay,
		signal.Notify,
		signal.Stop,
		reapUnmanagedExitedChildren,
	)
}

func startPID1ChildReaperWith(
	ctx context.Context,
	pid int,
	delay time.Duration,
	notify signalNotifyFunc,
	stop signalStopFunc,
	reap func(),
) bool {
	if pid != 1 {
		return false
	}

	signals := make(chan os.Signal, 1)
	notify(signals, syscall.SIGCHLD)
	go runPID1ChildReaper(ctx, signals, delay, stop, reap)
	return true
}

func runPID1ChildReaper(
	ctx context.Context,
	signals chan os.Signal,
	delay time.Duration,
	stop signalStopFunc,
	reap func(),
) {
	defer stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			waitBeforeReaping(ctx, delay, reap)
		}
	}
}

func waitBeforeReaping(ctx context.Context, delay time.Duration, reap func()) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
		reap()
	}
}

func reapUnmanagedExitedChildren() {
	reapExitedChildrenWith(childReaper{
		childPIDs: listZombieChildPIDs,
		managed:   executil.IsManagedChild,
		wait:      waitExitedChild,
	})
}

func waitExitedChild(pid int) (int, syscall.WaitStatus, error) {
	var status syscall.WaitStatus
	reapedPID, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err != nil {
		return reapedPID, status, fmt.Errorf("wait for exited child %d: %w", pid, err)
	}
	return reapedPID, status, nil
}

func reapExitedChildrenWith(reaper childReaper) {
	pids, err := reaper.childPIDs()
	if err != nil {
		slog.Warn("failed to list child processes", "error", err)
		return
	}

	for _, pid := range pids {
		if reaper.managed(pid) {
			continue
		}
		reapExitedChildWith(pid, reaper.wait)
	}
}

func reapExitedChildWith(pid int, waitChild waitExitedChildFunc) {
	for {
		reapedPID, status, err := waitChild(pid)
		if errors.Is(err, syscall.ECHILD) {
			return
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			slog.Warn("failed to reap child process", "error", err)
			return
		}
		if reapedPID <= 0 {
			return
		}
		slog.Debug("reaped orphan child process", "pid", reapedPID, "status", status)
		return
	}
}

func listZombieChildPIDs() ([]int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, fmt.Errorf("reading task list: %w", err)
	}

	seen := map[int]bool{}
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join(string(os.PathSeparator), "proc", "self", "task", task.Name(), "children"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("reading task children: %w", err)
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return nil, fmt.Errorf("parsing child pid %q: %w", field, err)
			}
			zombie, err := isZombieProcess(pid)
			if err != nil {
				continue
			}
			if zombie {
				seen[pid] = true
			}
		}
	}

	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func isZombieProcess(pid int) (bool, error) {
	data, err := os.ReadFile(filepath.Join(string(os.PathSeparator), "proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false, fmt.Errorf("reading process stat: %w", err)
	}
	state, err := procStatState(string(data))
	if err != nil {
		return false, err
	}
	return state == 'Z', nil
}

func procStatState(stat string) (byte, error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, fmt.Errorf("process stat missing command terminator")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) == 0 || fields[0] == "" {
		return 0, fmt.Errorf("process stat missing state")
	}
	return fields[0][0], nil
}

// ensurePATH adds each dir to PATH if not already present, preserving any
// directories the build environment or initramfs may have set.
func ensurePATH(dirs ...string) {
	existing := os.Getenv("PATH")
	have := make(map[string]bool)
	for _, d := range strings.Split(existing, ":") {
		have[d] = true
	}
	for _, d := range dirs {
		if !have[d] {
			if existing != "" {
				existing += ":"
			}
			existing += d
			have[d] = true
		}
	}
	if err := os.Setenv("PATH", existing); err != nil {
		slog.Warn("failed to set PATH", "error", err)
	}
}

type earlyInitMountManager interface {
	GetMount(name string) *realm.Mount
	CreateFolder() error
	MountNamed(name string, remove bool) error
	MountAll() error
}

type earlyInitDeviceManager interface {
	CreateDevice() error
}

// setupMountsAndDevices performs early init: mount filesystems and create devices.
func setupMountsAndDevices() error {
	return setupMountsAndDevicesWith(realm.DefaultMounts(), realm.DefaultDevices())
}

func setupMountsAndDevicesWith(m earlyInitMountManager, d earlyInitDeviceManager) error {
	for _, name := range []string{"dev", "proc", "run", "tmp", "sys"} {
		mt := m.GetMount(name)
		if mt == nil {
			return fmt.Errorf("required early mount %q not configured", name)
		}
		mt.CreateMount = true
		mt.EnableMount = true
	}

	if err := m.CreateFolder(); err != nil {
		return fmt.Errorf("create early mount folders: %w", err)
	}
	if err := m.MountNamed("dev", true); err != nil {
		return fmt.Errorf("mount early /dev: %w", err)
	}
	if err := d.CreateDevice(); err != nil {
		return fmt.Errorf("create early devices: %w", err)
	}
	if err := m.MountAll(); err != nil {
		return fmt.Errorf("mount early filesystems: %w", err)
	}
	return nil
}

// loadModules loads kernel modules from /modules/ for server NICs and storage controllers.
// Uses the finit_module syscall directly instead of shelling out to insmod.
// Errors are non-fatal: modules may already be built-in or not needed.
//
// Module dependencies are resolved by retrying: modules that fail on the
// first pass (e.g. virtio_net depends on virtio_pci → virtio → virtio_ring)
// succeed on subsequent passes once their dependencies have been loaded.
func loadModules() {
	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		slog.Debug("no kernel modules directory, skipping", "path", moduleDir)
		return
	}

	// Collect all module paths.
	var pending []string
	for _, entry := range entries {
		if !entry.IsDir() {
			pending = append(pending, entry.Name())
		}
	}

	// Retry up to 5 passes to resolve dependency ordering.
	const maxPasses = 5
	for pass := range maxPasses {
		var failed []string
		for _, name := range pending {
			ko := filepath.Join(moduleDir, name)
			if err := loadModuleFn(ko); err != nil {
				failed = append(failed, name)
				if pass == maxPasses-1 {
					// Visible at the default level: a missing driver often
					// shows up later as "no disk" or "DHCP failed".
					slog.Warn("kernel module failed to load", "module", name, "error", err)
				}
				continue
			}
			slog.Info("loaded kernel module", "module", name)
		}
		if len(failed) == 0 {
			break
		}
		pending = failed
	}
}

// Test seams for loadModules.
var (
	moduleDir    = "/modules"
	loadModuleFn = loadModule
)

// loadModule loads a single kernel module via the finit_module syscall.
// Returns nil when the module is already loaded or built-in (EEXIST).
func loadModule(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open module %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("finit_module %s: %w", filepath.Base(path), err)
	}
	return nil
}

// runCAPRF runs the CAPRF provisioning flow (ISO-based, /deploy/vars config).
func flushObservability() {
	if originalSlogHandler != nil {
		slog.SetDefault(slog.New(originalSlogHandler))
		originalSlogHandler = nil
	}
	if caprfRemoteLogHandler != nil {
		caprfRemoteLogHandler.Close()
		caprfRemoteLogHandler = nil
	}
}

func reboot() {
	flushObservability()
	realm.Reboot()
}

func requestReboot() error {
	flushObservability()
	return realm.RequestReboot()
}

func requestPowerOff() error {
	flushObservability()
	return realm.RequestPowerOff()
}

func shell() {
	flushObservability()
	realm.Shell()
}

func doKexecExecute() error {
	flushObservability()
	return kexec.Execute()
}

func runCAPRF(ctx context.Context) {
	client, err := caprf.New(varsPath)
	if err != nil {
		slog.Error("failed to create CAPRF client", "error", err)
		provision.DumpDebugState("caprf-init")
		// The vars file did not parse, but it may still ask to wait on
		// failure: then keep the ramdisk running instead of rebooting.
		failRun(ctx, caprf.FailureConfig(varsPath), nil,
			"invalid vars file", reboot)
		return
	}

	cfg, err := client.GetConfig(ctx)
	if err != nil {
		slog.Error("failed to get CAPRF config", "error", err)
		provision.DumpDebugState("config-fetch")
		reboot()
	}
	network.SetSkipTLSVerify(config.BoolOr(cfg.Transport.SkipTLSVerify, false))
	image.RefreshHTTPClients()

	if cfg.ProvisionerFilesInRamdisk() {
		// Like copy_files in the legacy common.sh: before any request, so
		// files such as /etc/hosts also apply to the remote log shipping.
		if err := provision.ApplyProvisionerFilesToRamdisk(ctx); err != nil {
			// The legacy script stops before the network starts when
			// copy_files fails, and the ramdisk keeps running. Do not go on
			// with a partly applied file set.
			slog.Error("applying provisioner files to the ramdisk failed, waiting", "error", err)
			provision.DumpDebugState("apply-provisioner-files")
			<-ctx.Done()
			reboot()
			return
		}
	}
	if config.BoolOr(cfg.RamdiskHostname, false) {
		setRamdiskHostname(cfg, syscall.Sethostname)
	}

	// DRY_RUN=true overrides mode before logging.
	if cfg.DryRun {
		cfg.Mode = "dry-run"
	}

	// Wire remote log shipping.
	if cfg.Transport.LogURL != "" {
		originalSlogHandler = slog.Default().Handler()
		remoteInner := slog.NewTextHandler(os.Stderr, nil)
		caprfRemoteLogHandler = caprf.NewRemoteHandler(client, remoteInner, slog.LevelInfo, 256)
		defer flushObservability()
		slog.SetDefault(slog.New(caprfRemoteLogHandler))
	}

	slog.Info("CAPRF mode active",
		"hostname", cfg.Hostname,
		"mode", cfg.Mode,
		"image_count", len(cfg.Provision.Image.URLs),
	)

	// runCtx stops the active mode when JWT renewal is exhausted and
	// FAILURE_ACTION=wait, like fail() stops the legacy task.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()

	netMode, err := setupNetworkAndTokenFlow(ctx, cfg, client, stopRun)
	if err != nil {
		failRun(ctx, cfg, client, fmt.Sprintf("network or token setup failed: %v", err), reboot)
		return
	}

	diskMgr := disk.NewManager(nil)
	diskMgr.SetLegacyCAPRFSelection(cfg.Provision.Disk.Selection == config.DiskSelectionLsblk)
	if result, inspectErr := crash.InspectStartup(ctx, cfg, diskMgr, client, crash.InspectOptions{}); inspectErr != nil {
		slog.Warn("startup crash artifact inspection failed", "error", inspectErr)
	} else if result != nil && result.Ran {
		slog.Info("startup crash artifact inspection complete",
			"evidence", result.EvidenceFound,
			"uploaded", result.Uploaded,
			"skip_reason", result.SkipReason)
	}

	// Resolve and run the operating mode.
	mode, err := runmode.Resolve(runmode.Deps{
		Cfg:     cfg,
		Client:  client,
		DiskMgr: diskMgr,
		NetMode: netMode,
	})
	if err != nil {
		slog.Error("unknown operating mode", "mode", cfg.Mode, "error", err)
		if netMode != nil {
			if tearErr := netMode.Teardown(ctx); tearErr != nil {
				slog.Warn("network teardown error", "error", tearErr)
			}
		}
		reboot()
		return
	}

	slog.Info("executing mode", "mode", mode.Name())
	modeErr := mode.Run(runCtx)
	if cfg.WaitOnFailure() && runCtx.Err() != nil && ctx.Err() == nil && !isTerminalSuccess(modeErr) {
		failRun(ctx, cfg, client, "token renewal exhausted, task stopped", reboot)
		return
	}

	slog.Info("CAPRF run complete")

	// Handle mode-specific exit behavior before network teardown,
	// so rescue shell SSH access remains available.
	var rescueErr *runmode.RescueShellError
	var rebootErr *runmode.RebootRequestedError
	var powerOffErr *runmode.PowerOffRequestedError
	var provisionErr *runmode.ProvisionCompleteError
	switch {
	case errors.As(modeErr, &rescueErr):
		// Network teardown is intentionally skipped here so SSH access
		// remains available in the rescue shell.
		shell()
		reboot()
		return
	case errors.As(modeErr, &rebootErr):
		if netMode != nil {
			if err := netMode.Teardown(ctx); err != nil {
				slog.Warn("network teardown error", "error", err)
			}
		}
		reboot()
		return
	case errors.As(modeErr, &powerOffErr):
		// Keep the network up, like the provision handoff, so a failed power
		// off can still be reported. Do not reboot when the power off fails:
		// CAPRF has not ejected the ISO yet, so a reboot would run the task
		// again.
		requestProvisionHandoff(ctx, client, "power off", provisionHandoff.powerOff, provisionHandoff,
			"power off handoff failed after deprovisioning success")
		return
	case errors.As(modeErr, &provisionErr):
		if provisionErr.Cfg != nil {
			// A standby command ran with its own config (payload).
			cfg = provisionErr.Cfg
		}
		kexeced := tryKexec(cfg, provisionErr.FirmwareChanged, func() (disk.ReleaseReport, error) {
			return diskMgr.UnmountOrRemountReadOnly(installedRootPath)
		})
		if !kexeced && provision.ShouldKeepTargetRootMountedForKexec(cfg, provisionErr.FirmwareChanged) {
			cleanupFailedKexecTarget(diskMgr)
		}
		time.Sleep(2 * time.Second)
		handleProvisionHandoff(ctx, cfg, client, provisionErr, kexeced, provisionHandoff)
		return
	}

	// HealthCheckError is an expected non-fatal exit (checks ran but some failed).
	// Log other unexpected errors at error level.
	var healthErr *runmode.HealthCheckError
	if modeErr != nil && !errors.As(modeErr, &healthErr) {
		slog.Error("mode exited with error", "mode", mode.Name(), "error", modeErr)
	}
	if netMode != nil {
		if err := netMode.Teardown(ctx); err != nil {
			slog.Warn("network teardown error", "error", err)
		}
	}
	time.Sleep(2 * time.Second)
	reboot()
}

func handleProvisionHandoff(
	ctx context.Context,
	cfg *config.MachineConfig,
	reporter provisionStatusReporter,
	provisionErr *runmode.ProvisionCompleteError,
	kexeced bool,
	ops provisionHandoffOps,
) {
	if requiresABKexec(cfg) && !kexeced {
		msg := "a/b preserveExisting requires kexec; refusing normal reboot because firmware boot state still points at the active slot"
		slog.Error(msg)
		requestProvisionHandoff(ctx, reporter, "power off", ops.powerOff, ops, msg)
		return
	}

	if provisionErr.PowerOff {
		slog.Info("provisioning succeeded, powering off for orchestrator to manage boot")
		requestProvisionHandoff(ctx, reporter, "power off", ops.powerOff, ops, "power off handoff failed after provisioning success")
		return
	}

	requestProvisionHandoff(ctx, reporter, "reboot", ops.reboot, ops, "reboot handoff failed after provisioning success")
}

func requestProvisionHandoff(
	ctx context.Context,
	reporter provisionStatusReporter,
	action string,
	request func() error,
	ops provisionHandoffOps,
	message string,
) {
	if err := request(); err != nil {
		statusMessage := fmt.Sprintf("%s: %v", message, err)
		slog.Error("provisioning handoff failed after success report", "action", action, "error", err)
		reportProvisionHandoffError(ctx, reporter, statusMessage)
		ops.shell()
		ops.exit(1)
		return
	}
	ops.exit(0)
}

func reportProvisionHandoffError(ctx context.Context, reporter provisionStatusReporter, message string) {
	if reporter == nil {
		return
	}
	if err := reporter.ReportStatus(ctx, config.StatusError, message); err != nil {
		slog.Warn("failed to report provisioning handoff error", "error", err)
	}
}

func setupNetworkAndTokenFlow(ctx context.Context, cfg *config.MachineConfig, client *caprf.Client, stopRun func()) (network.Mode, error) {
	if cfg.Mode == "dry-run" {
		slog.Info("dry-run mode: skipping active network setup and token renewal")
		return noopNetworkMode{}, nil
	}

	// Set up networking with retry — if connectivity fails, teardown and
	// rebuild the entire network stack before giving up.
	netMode, err := setupNetworkMode(ctx, cfg)
	if err != nil {
		return nil, err
	}
	connectivityTarget := cfg.Transport.InitURL
	if connectivityTarget == "" {
		connectivityTarget = cfg.Transport.SuccessURL
	}
	if connectivityTarget == "" && cfg.Provision.CrashArtifacts.Enabled {
		connectivityTarget = cfg.Provision.CrashArtifacts.PrepareURL
		if connectivityTarget == "" {
			connectivityTarget = cfg.Provision.CrashArtifacts.UploadURL
		}
	}
	if connectivityTarget != "" {
		activeMode, err := ensureNetworkConnectivity(ctx, cfg, netMode, connectivityTarget)
		if err != nil {
			return nil, err
		}
		netMode = activeMode
	}

	// Acquire JWT after network is ready so the token endpoint is reachable.
	if err := client.AcquireToken(ctx); err != nil {
		slog.Error("failed to acquire jwt token", "error", err)
		if cfg.Transport.TokenURL != "" {
			provision.DumpDebugState("jwt-acquire")
			return nil, err
		}
	}

	client.SetTokenRenewalFatalHandler(func() { tokenRenewalFatal(cfg, reboot, stopRun) })

	if err := client.StartTokenRenewal(ctx); err != nil {
		slog.Error("failed to start token renewal", "error", err)
		if cfg.Transport.TokenURL != "" {
			provision.DumpDebugState("jwt-renewal-start")
			return nil, err
		}
	}

	return netMode, nil
}

// failRun ends a run that failed outside a mode. With FAILURE_ACTION=wait it
// reports the error like the legacy fail() and keeps the ramdisk running; a
// reboot would boot the ISO and run the task again.
func failRun(ctx context.Context, cfg *config.MachineConfig, reporter provisionStatusReporter, message string, rebootFn func()) {
	if cfg != nil && cfg.WaitOnFailure() {
		slog.Error("run failed, keeping the ramdisk running", "error", message)
		if reporter != nil {
			reportCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := reporter.ReportStatus(reportCtx, config.StatusError, message); err != nil {
				slog.Warn("failed to report the run failure", "error", err)
			}
			cancel()
		}
		<-ctx.Done()
	}
	rebootFn()
}

// setRamdiskHostname names the ramdisk after the machine (RAMDISK_HOSTNAME),
// like `hostname "$HOSTNAME"` at the start of the CAPRF legacy provision.sh,
// so FRR, syslog and DHCP use the machine name. A failure is only logged.
func setRamdiskHostname(cfg *config.MachineConfig, sethostname func([]byte) error) {
	if cfg.DryRun || (cfg.Mode != "provision" && cfg.Mode != "") || cfg.Hostname == "" {
		return
	}
	if err := sethostname([]byte(cfg.Hostname)); err != nil {
		slog.Warn("setting the ramdisk hostname failed", "hostname", cfg.Hostname, "error", err)
		return
	}
	slog.Info("ramdisk hostname set", "hostname", cfg.Hostname)
}

// isTerminalSuccess reports a mode result that completed its task. Such a
// result wins over a renewal failure that arrives at the same time.
func isTerminalSuccess(modeErr error) bool {
	var provisionErr *runmode.ProvisionCompleteError
	var powerOffErr *runmode.PowerOffRequestedError
	return errors.As(modeErr, &provisionErr) || errors.As(modeErr, &powerOffErr)
}

// tokenRenewalFatal handles exhausted JWT renewal. FAILURE_ACTION=wait stops
// the active task with stopRun and then waits instead of rebooting into the
// ISO.
func tokenRenewalFatal(cfg *config.MachineConfig, rebootFn, stopRun func()) {
	provision.DumpDebugState("jwt-renewal-exhausted")
	if cfg != nil && cfg.WaitOnFailure() {
		slog.Error("token renewal exhausted, stopping the task")
		stopRun()
		return
	}
	slog.Error("token renewal exhausted, rebooting")
	rebootFn()
}

type noopNetworkMode struct{}

func (noopNetworkMode) Setup(context.Context, *network.Config) error { return nil }

func (noopNetworkMode) WaitForConnectivity(context.Context, string, time.Duration) error { return nil }

func (noopNetworkMode) Teardown(context.Context) error { return nil }

// ensureNetworkConnectivity retries network setup up to 3 times on connectivity failure.
// It returns the active network mode so callers can tear down the latest instance.
func ensureNetworkConnectivity(ctx context.Context, cfg *config.MachineConfig, netMode network.Mode, target string) (network.Mode, error) {
	maxRetries, timeout := networkWait(cfg)
	logTarget := network.RedactHTTPURLForLog(target)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		slog.Info("waiting for network connectivity", "target", logTarget, "attempt", attempt, "timeout", timeout)
		if err := waitForConnectivity(ctx, cfg, netMode, target, timeout); err == nil {
			slog.Info("network connectivity established", "target", logTarget)
			return netMode, nil
		}
		slog.Error("network connectivity timeout", "attempt", attempt)
		if attempt < maxRetries {
			slog.Info("tearing down network for retry", "attempt", attempt)
			if tErr := netMode.Teardown(ctx); tErr != nil {
				slog.Warn("network teardown failed", "error", tErr)
			}
			newMode, setupErr := setupNetworkMode(ctx, cfg)
			if setupErr != nil {
				return nil, fmt.Errorf("network retry setup: %w", setupErr)
			}
			netMode = newMode
		}
	}
	slog.Error("network connectivity failed after all retries", "attempts", maxRetries)
	if maxRetries == 1 {
		return netMode, fmt.Errorf("network connectivity timeout after %s", timeout)
	}
	return netMode, fmt.Errorf("network connectivity timeout after %d attempts", maxRetries)
}

// waitForConnectivity runs one connectivity wait. Legacy compat bounds the
// whole call with the timeout: GoBGP otherwise waits for the underlay and
// then for the target, each up to the timeout.
func waitForConnectivity(ctx context.Context, cfg *config.MachineConfig, netMode network.Mode, target string, timeout time.Duration) error {
	if cfg.LegacyCompat {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := netMode.WaitForConnectivity(ctx, target, timeout); err != nil {
		return fmt.Errorf("wait for %s: %w", network.RedactHTTPURLForLog(target), err)
	}
	return nil
}

// networkWait returns the number of network setups and the connectivity
// timeout of each. Legacy compat follows wait_for_network in the CAPRF
// common.sh: NETWORK_WAIT_MAX_ATTEMPTS rounds of 20 seconds (default 15)
// on one network setup, and then the run fails.
func networkWait(cfg *config.MachineConfig) (int, time.Duration) {
	if !cfg.LegacyCompat {
		return 3, 5 * time.Minute
	}
	rounds := 15
	if n := cfg.Network.WaitMaxAttempts; n != nil {
		rounds = *n // an explicit 0 fails at once, like the legacy loop
	}
	return 1, time.Duration(rounds) * 20 * time.Second
}

// setupNetworkMode detects and configures the appropriate network mode.
func setupNetworkMode(ctx context.Context, cfg *config.MachineConfig) (network.Mode, error) {
	netCfg := networkConfigFromMachineConfig(cfg)

	// Auto-detect netplan configuration files injected by the provisioner.
	// Netplan-derived values override vars-based values, allowing the same
	// config format used by the old deployer to work as a drop-in with BOOTy.
	npCfg, err := detectNetplanConfig(deployNetplanDir, deployFRRConfPath)
	if err != nil {
		return nil, err
	}
	if npCfg != nil {
		mergeNetplanConfig(netCfg, npCfg)
	}
	keepProvisionerResolvConf(cfg, netCfg)

	// Parse VLAN configuration.
	if cfg.Network.VLAN.Config != "" {
		vlans, err := network.ParseVLANs(cfg.Network.VLAN.Config)
		if err != nil {
			return nil, fmt.Errorf("invalid VLAN configuration: %w", err)
		}
		netCfg.VLANs = vlans
	}

	linkCleanup, err := prepareLinkLayers(ctx, netCfg)
	if err != nil {
		return nil, err
	}

	// Priority: GoBGP > FRR > Static > DHCP.
	if netCfg.IsGoBGPMode() {
		detectIPMI(netCfg)
		slog.Info("using GoBGP/EVPN network mode", "asn", cfg.Network.BGP.ASN)
		stack, err := setupGoBGPStack(ctx, netCfg)
		if err != nil {
			slog.Error("gobgp network setup failed", "error", err)
			if cleanupErr := linkCleanup.Teardown(ctx); cleanupErr != nil {
				slog.Warn("link-layer cleanup after GoBGP setup failure failed", "error", cleanupErr)
			}
			return nil, fmt.Errorf("gobgp network setup: %w", err)
		}
		return networkModeWithResolvers(ctx, netCfg, stack, linkCleanup)
	}

	if netCfg.IsFRRMode() {
		detectIPMI(netCfg)
		slog.Info("using FRR/EVPN network mode", "asn", cfg.Network.BGP.ASN)
		mgr := frr.NewManager(nil)
		if err := mgr.Setup(ctx, netCfg); err != nil {
			slog.Error("FRR network setup failed, falling back to DHCP", "error", err)
			return networkModeWithResolvers(ctx, netCfg, dhcpFallback(ctx, netCfg), linkCleanup)
		}
		return networkModeWithResolvers(ctx, netCfg, mgr, linkCleanup)
	}

	if netCfg.IsStaticMode() {
		slog.Info("using static network mode", "ip", netCfg.StaticIP)
		mode := &network.StaticMode{}
		if err := mode.Setup(ctx, netCfg); err != nil {
			slog.Error("static network setup failed, falling back to DHCP", "error", err)
			if tearErr := mode.Teardown(ctx); tearErr != nil {
				slog.Warn("static teardown failed during DHCP fallback", "error", tearErr)
			}
			return networkModeWithResolvers(ctx, netCfg, dhcpFallback(ctx, netCfg), linkCleanup)
		}
		return networkModeWithResolvers(ctx, netCfg, mode, linkCleanup)
	}

	slog.Info("using DHCP network mode")
	return networkModeWithResolvers(ctx, netCfg, dhcpFallback(ctx, netCfg), linkCleanup)
}

func networkConfigFromMachineConfig(cfg *config.MachineConfig) *network.Config {
	return &network.Config{
		UnderlaySubnet:    cfg.Network.EVPN.UnderlaySubnet,
		UnderlayIP:        cfg.Network.EVPN.UnderlayIP,
		OverlaySubnet:     cfg.Network.EVPN.OverlaySubnet,
		IPMISubnet:        cfg.Network.IPMI.Subnet,
		ASN:               cfg.Network.BGP.ASN,
		ProvisionVNI:      cfg.Network.EVPN.ProvisionVNI,
		ProvisionIP:       cfg.Network.EVPN.ProvisionIP,
		ProvisionGateway:  cfg.Network.EVPN.ProvisionGateway,
		LegacyFRRRecovery: cfg.LegacyCompat,
		DNSResolvers:      cfg.Network.DNSResolvers,
		DCGWIPs:           cfg.Network.EVPN.DCGWIPs,
		LeafASN:           cfg.Network.EVPN.LeafASN,
		LocalASN:          cfg.Network.EVPN.LocalASN,
		OverlayAggregate:  cfg.Network.EVPN.OverlayAggregate,
		VPNRT:             cfg.Network.EVPN.VPNRT,
		StaticIP:          cfg.Network.Static.IP,
		StaticGateway:     cfg.Network.Static.Gateway,
		StaticIface:       cfg.Network.Static.Iface,
		BondInterfaces:    cfg.Network.Bond.Interfaces,
		BondMode:          cfg.Network.Bond.Mode,
		VRFName:           cfg.Network.VRF.Name,
		VRFTableID:        cfg.Network.VRF.TableID,
		BGPKeepalive:      cfg.Network.BGP.Keepalive,
		BGPHold:           cfg.Network.BGP.Hold,
		BFDTransmitMS:     cfg.Network.BGP.BFDTransmitMS,
		BFDReceiveMS:      cfg.Network.BGP.BFDReceiveMS,
		NetworkMode:       cfg.Network.Mode,
		BGPPeerMode:       network.ParsePeerMode(cfg.Network.BGP.PeerMode),
		BGPInterfaces:     cfg.Network.BGP.Interfaces,
		BGPNeighbors:      cfg.Network.BGP.Neighbors,
		BGPRemoteASN:      cfg.Network.BGP.RemoteASN,
		BGPUnderlayAF:     cfg.Network.BGP.UnderlayAF,
		BGPOverlayType:    cfg.Network.BGP.OverlayType,
		EVPNL2Enabled:     cfg.Network.EVPN.L2Enabled,
		EVPNType5Only:     cfg.Network.EVPN.Type5Only,
		BGPAuthPassword:   cfg.Network.BGP.AuthPassword,
		BGPMinPeers:       cfg.Network.BGP.MinPeers,
	}
}

// keepProvisionerResolvConf leaves the provisioner /etc/resolv.conf in the
// ramdisk alone with PROVISIONER_FILES_TARGET=ramdisk: copy_files puts it in
// place and the legacy scripts never rewrite it.
func keepProvisionerResolvConf(cfg *config.MachineConfig, netCfg *network.Config) {
	if cfg.ProvisionerFilesInRamdisk() && provision.HasProvisionerResolvConf() {
		slog.Info("keeping the provisioner resolv.conf in the ramdisk")
		netCfg.DNSResolvers = ""
	}
}

func networkModeWithResolvers(ctx context.Context, netCfg *network.Config, mode network.Mode, cleanup *linkLayerCleanup) (network.Mode, error) {
	if err := network.ConfigureResolvers(netCfg.DNSResolvers); err != nil {
		if cleanupErr := mode.Teardown(ctx); cleanupErr != nil {
			slog.Warn("network mode cleanup after resolver setup failure failed", "error", cleanupErr)
		}
		if cleanupErr := cleanup.Teardown(ctx); cleanupErr != nil {
			slog.Warn("link-layer cleanup after resolver setup failure failed", "error", cleanupErr)
		}
		return nil, fmt.Errorf("configure initramfs DNS: %w", err)
	}
	return wrapLinkLayerMode(mode, cleanup), nil
}

// resolveLinkMatch is network.ResolveLinkMatch; tests replace it.
var resolveLinkMatch = network.ResolveLinkMatch

func prepareLinkLayers(ctx context.Context, netCfg *network.Config) (*linkLayerCleanup, error) {
	cleanup := &linkLayerCleanup{}
	if err := network.ApplyLinkRenames(ctx, netCfg.LinkRenames); err != nil {
		return nil, fmt.Errorf("netplan set-name: %w", err)
	}
	if netCfg.StaticIface == "" && netCfg.StaticMatch != nil {
		name, err := resolveLinkMatch(ctx, *netCfg.StaticMatch)
		if err != nil {
			slog.Warn("netplan match for the static address did not select one NIC, using the first NIC", "error", err)
		} else {
			netCfg.StaticIface = name
		}
	}
	if netCfg.IsBondMode() {
		slog.Info("setting up LACP bond")
		bondMode, err := setupBondLayer(ctx, netCfg)
		if err != nil {
			return nil, fmt.Errorf("bond setup: %w", err)
		}
		cleanup.bond = bondMode
		if netCfg.StaticIface == "" && !netCfg.IsVLANMode() {
			netCfg.StaticIface = "bond0"
		}
	}

	if netCfg.IsVLANMode() {
		if err := setupVLANLayers(ctx, netCfg, cleanup); err != nil {
			return nil, err
		}
	}

	return cleanup, nil
}

func setupVLANLayers(ctx context.Context, netCfg *network.Config, cleanup *linkLayerCleanup) error {
	slog.Info("setting up VLAN interfaces", "count", len(netCfg.VLANs))
	var vlanErrs []error
	for _, v := range netCfg.VLANs {
		name, err := setupVLANLayer(v)
		if err != nil {
			slog.Error("vlan setup failed", "vlan", v.ID, "parent", v.Parent, "error", err)
			vlanErrs = append(vlanErrs, fmt.Errorf("vlan %d on %s: %w", v.ID, v.Parent, err))
			continue
		}
		if netCfg.StaticIface == "" {
			netCfg.StaticIface = name
		}
		cleanup.vlans = append(cleanup.vlans, v)
	}
	if err := errors.Join(vlanErrs...); err != nil {
		if cleanupErr := cleanup.Teardown(ctx); cleanupErr != nil {
			return errors.Join(fmt.Errorf("vlan setup: %w", err), fmt.Errorf("link-layer rollback: %w", cleanupErr))
		}
		return fmt.Errorf("vlan setup: %w", err)
	}
	return nil
}

type linkLayerCleanup struct {
	bond  network.Mode
	vlans []network.VLANConfig
}

func (c *linkLayerCleanup) empty() bool {
	return c == nil || (c.bond == nil && len(c.vlans) == 0)
}

func (c *linkLayerCleanup) Teardown(ctx context.Context) error {
	if c.empty() {
		return nil
	}
	var errs []error
	for i := len(c.vlans) - 1; i >= 0; i-- {
		v := c.vlans[i]
		if err := teardownVLANLayer(v); err != nil {
			errs = append(errs, fmt.Errorf("teardown vlan %d on %s: %w", v.ID, v.Parent, err))
		}
	}
	if c.bond != nil {
		if err := c.bond.Teardown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("teardown bond: %w", err))
		}
	}
	return errors.Join(errs...)
}

type linkLayerNetworkMode struct {
	network.Mode
	cleanup *linkLayerCleanup
}

func wrapLinkLayerMode(mode network.Mode, cleanup *linkLayerCleanup) network.Mode {
	if cleanup.empty() {
		return mode
	}
	return &linkLayerNetworkMode{Mode: mode, cleanup: cleanup}
}

func (m *linkLayerNetworkMode) Teardown(ctx context.Context) error {
	return errors.Join(m.Mode.Teardown(ctx), m.cleanup.Teardown(ctx))
}

// dhcpFallback creates a DHCP mode and attempts setup.
// Returns the mode even if setup fails, so the caller can still proceed.
func dhcpFallback(ctx context.Context, netCfg *network.Config) network.Mode {
	dhcp := network.NewDHCPMode()
	if err := dhcp.Setup(ctx, netCfg); err != nil {
		slog.Error("DHCP setup failed", "error", err)
	}
	return dhcp
}

// Paths of the netplan and FRR files in the provisioner's file-system overlay.
const (
	deployNetplanDir  = "/deploy/file-system/etc/netplan"
	deployFRRConfPath = "/deploy/file-system/etc/frr/frr.conf"
)

// detectNetplanConfig checks for netplan YAML files in the provisioner's
// file-system overlay. If found, it parses them (and any FRR config) into
// a network.Config. Returns nil if no netplan files are present. Files that
// are present but cannot be parsed are an error: falling back to vars would
// bring up a different network than the operator configured.
func detectNetplanConfig(netplanDir, frrConfPath string) (*network.Config, error) {
	found, err := netplan.HasNetplanFiles(netplanDir)
	if err != nil {
		return nil, err //nolint:wrapcheck // already names the dir
	}
	if !found {
		return nil, nil //nolint:nilnil // no netplan files means no netplan config, not an error
	}
	slog.Info("detected netplan configuration files", "dir", netplanDir)

	np, err := netplan.ParseDir(netplanDir)
	if err != nil {
		return nil, fmt.Errorf("parse netplan files in %s: %w", netplanDir, err)
	}

	var frrParams *netplan.FRRParams
	data, frrErr := os.ReadFile(frrConfPath)
	switch {
	case frrErr == nil:
		parsed, parseErr := netplan.ParseFRRConfigBytes(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parse FRR config %s: %w", frrConfPath, parseErr)
		}
		frrParams = parsed
		slog.Info("parsed FRR configuration", "asn", frrParams.ASN, "evpn", frrParams.EVPN)
	case !errors.Is(frrErr, os.ErrNotExist):
		return nil, fmt.Errorf("read FRR config %s: %w", frrConfPath, frrErr)
	}

	cfg := netplan.ToNetworkConfig(np, frrParams)
	slog.Info("netplan config loaded",
		"mode", cfg.NetworkMode, "asn", cfg.ASN,
		"vni", cfg.ProvisionVNI, "underlay", cfg.UnderlayIP,
	)
	return cfg, nil
}

// mergeNetplanConfig overrides dst fields with values from the netplan-derived
// src config. Fields that netplan doesn't provide (zero/empty) are left
// unchanged so vars-based operational parameters are preserved.
func mergeNetplanConfig(dst, src *network.Config) {
	if src.ASN > 0 {
		dst.ASN = src.ASN
	}
	if src.ProvisionVNI > 0 {
		dst.ProvisionVNI = src.ProvisionVNI
	}
	if src.UnderlayIP != "" {
		dst.UnderlayIP = src.UnderlayIP
	}
	// The netplan MAC belongs to one bridge. Only use it for that bridge.
	if src.BridgeMAC != "" && src.BridgeNameOrDefault() == dst.BridgeNameOrDefault() {
		dst.BridgeMAC = src.BridgeMAC
	}
	if src.ProvisionIP != "" {
		dst.ProvisionIP = mergeProvisionIP(dst.ProvisionIP, src.ProvisionIP)
	}
	if src.NetworkMode != "" {
		dst.NetworkMode = src.NetworkMode
	}
	if src.BGPPeerMode != "" {
		dst.BGPPeerMode = src.BGPPeerMode
	}
	if src.BGPNeighbors != "" {
		// The FRR config sets the numbered peers and their remote ASN
		// together; 0 means iBGP and must replace a vars value too.
		dst.BGPNeighbors = src.BGPNeighbors
		dst.BGPRemoteASN = src.BGPRemoteASN
	}
	mergeInferredEVPNL2(dst, src)
	if src.BondInterfaces != "" {
		dst.BondInterfaces = src.BondInterfaces
	}
	if src.BondMode != "" {
		dst.BondMode = src.BondMode
	}
	if src.VRFTableID > 0 {
		dst.VRFTableID = src.VRFTableID
	}
	if src.OverlayVRFTableID > 0 {
		dst.OverlayVRFTableID = src.OverlayVRFTableID
	}
	if src.VRFName != "" {
		dst.VRFName = src.VRFName
	}
	if src.OverlayVRFSet {
		// Preserve an explicitly empty overlay VRF name: it means the overlay
		// should stay in the default namespace, not that the value is unset.
		dst.OverlayVRFName = src.OverlayVRFName
		dst.OverlayVRFSet = true
	}
	if src.UnderlayDummyName != "" {
		dst.UnderlayDummyName = src.UnderlayDummyName
	}
	if src.VXLANLink != "" {
		dst.VXLANLink = src.VXLANLink
	}
	if src.MTU > 0 {
		dst.MTU = src.MTU
	}
	if src.DNSResolvers != "" {
		dst.DNSResolvers = src.DNSResolvers
	}
	if src.StaticIP != "" {
		dst.StaticIP = src.StaticIP
		dst.StaticIface = src.StaticIface
		dst.StaticMatch = src.StaticMatch
	}
	if len(src.LinkRenames) > 0 {
		dst.LinkRenames = src.LinkRenames
	}
	if src.StaticGateway != "" {
		dst.StaticGateway = src.StaticGateway
	}
	if len(src.VLANs) > 0 {
		dst.VLANs = src.VLANs
	}
	if len(src.Interfaces) > 0 {
		dst.Interfaces = src.Interfaces
	}
}

// mergeInferredEVPNL2 applies the EVPN mode inferred from the FRR config.
// An explicit EVPN_TYPE5_ONLY wins, so operators can force Type-5 only
// routing. The L3VNI check is repeated with the merged provision VNI because
// the VNI can come from /deploy/vars when the netplan files do not set it.
func mergeInferredEVPNL2(dst, src *network.Config) {
	if dst.EVPNType5Only {
		return
	}
	if slices.Contains(src.EVPNL3VNIs, dst.ProvisionVNI) {
		// An L3VNI has no L2 gateway, like FRR. Without provision_gateway
		// (the bm4x IPv6 provisioning network) that is Type-5 only. With
		// a gateway the mode stays as before, so its BUM FDB entry stays.
		if !dst.EVPNL2Enabled && dst.ProvisionGateway == "" {
			dst.EVPNType5Only = true
		}
		slog.Info("netplan: provision VNI is an FRR L3VNI, using Type-5 routes only", "vni", dst.ProvisionVNI)
		return
	}
	if src.EVPNL2Enabled {
		dst.EVPNL2Enabled = true
	}
}

func mergeProvisionIP(existing, detected string) string {
	if detected == "" || existing == "" {
		if detected != "" {
			return detected
		}
		return existing
	}
	if shouldPreserveProvisionPrefix(existing, detected) {
		return existing
	}
	return detected
}

func shouldPreserveProvisionPrefix(existing, detected string) bool {
	existingIP, _, existingBits, err := parseCIDRBits(existing)
	if err != nil {
		return false
	}
	detectedIP, _, detectedBits, err := parseCIDRBits(detected)
	if err != nil {
		return false
	}
	if !existingIP.Equal(detectedIP) {
		return false
	}
	if detectedBits.ones != detectedBits.bits {
		return false
	}
	return existingBits.ones < detectedBits.ones
}

type cidrBits struct {
	ones int
	bits int
}

func parseCIDRBits(value string) (net.IP, *net.IPNet, cidrBits, error) {
	ip, ipNet, err := net.ParseCIDR(value)
	if err != nil {
		return nil, nil, cidrBits{}, fmt.Errorf("parse CIDR %q: %w", value, err)
	}
	ones, bits := ipNet.Mask.Size()
	return ip, ipNet, cidrBits{ones: ones, bits: bits}, nil
}

// setupGoBGPStack creates and sets up a GoBGP/EVPN network stack.
func setupGoBGPStack(ctx context.Context, netCfg *network.Config) (*gobgp.Stack, error) {
	bgpCfg, err := gobgp.NewConfig(netCfg)
	if err != nil {
		return nil, fmt.Errorf("gobgp config: %w", err)
	}
	stack := gobgp.NewStack(bgpCfg)
	if err := stack.Setup(ctx, netCfg); err != nil {
		// Clean up partially created network state before returning.
		_ = stack.Teardown(ctx)
		return nil, fmt.Errorf("gobgp setup: %w", err)
	}
	return stack, nil
}

// detectIPMI auto-detects IPMI MAC/IP from system if not provided.
func detectIPMI(netCfg *network.Config) {
	if netCfg.IPMIMAC != "" && netCfg.IPMIIP != "" {
		return
	}
	mac, ip, err := network.GetIPMIInfo(netCfg.IPMISubnet)
	if err != nil {
		if errors.Is(err, network.ErrNoIPMIInterface) {
			slog.Info("no IPMI interface found, using the underlay IP rules")
			return
		}
		slog.Warn("failed to detect IPMI info", "error", err)
		return
	}
	if netCfg.IPMIMAC == "" {
		netCfg.IPMIMAC = mac
	}
	if netCfg.IPMIIP == "" {
		netCfg.IPMIIP = ip
	}
	slog.Info("detected IPMI info", "mac", mac, "ip", ip)
}

// tryKexec attempts to kexec into the installed kernel.
// Returns false on failure so the caller can decide whether a normal reboot is safe.
// Skips kexec when disabled by config toggle, when firmware was changed during
// provisioning (e.g. Mellanox SR-IOV), or when Secure Boot must be re-enabled,
// since those paths require a firmware-enforced reboot.
//
// After the kernel and initramfs are loaded, releaseTarget unmounts the
// target root (busy mounts are remounted read-only), so the installed OS
// does not start with filesystems that need journal recovery.
func tryKexec(cfg *config.MachineConfig, firmwareChanged bool, releaseTarget func() (disk.ReleaseReport, error)) bool {
	if cfg.KexecDisabled() {
		slog.Info("kexec disabled by configuration, skipping", "successAction", cfg.SuccessAction)
		return false
	}

	if cfg.Provision.SecureBoot.ReEnable {
		slog.Info("secure boot re-enable requested, hard reboot required; skipping kexec")
		return false
	}

	if firmwareChanged {
		slog.Info("firmware values changed during provisioning, hard reboot required — skipping kexec")
		return false
	}

	const grubPath = installedRootPath + "/boot/grub/grub.cfg"
	f, err := os.Open(grubPath)
	if err != nil {
		slog.Warn("cannot open grub.cfg, skipping kexec", "error", err)
		return false
	}
	entries, err := kexec.ParseGrubCfg(f)
	// Close grub.cfg now: an open file keeps its filesystem busy, and the
	// target is unmounted before kexec.
	_ = f.Close()
	if err != nil {
		slog.Warn("failed to parse grub.cfg", "error", err)
		return false
	}
	entry, err := kexec.GetDefaultEntry(entries)
	if err != nil {
		slog.Warn("no default boot entry found", "error", err)
		return false
	}

	kernel := resolveKexecPath(installedRootPath, entry.Kernel)
	initrd := resolveKexecPath(installedRootPath, entry.Initramfs)
	slog.Info("attempting kexec", "kernel", kernel, "initrd", initrd)

	if err := kexec.Load(kernel, initrd, entry.KernelArgs); err != nil {
		slog.Warn("kexec load failed, falling back to reboot", "error", err)
		return false
	}
	releaseKexecTarget(cfg, releaseTarget)
	if err := doKexecExecute(); err != nil {
		slog.Warn("kexec execute failed, falling back to reboot", "error", err)
		return false
	}
	return true
}

// releaseKexecTarget runs releaseTarget unless KEXEC_UNMOUNT_TARGET=false
// and logs what happened to the target mounts. Failures only warn: the data
// is synced, and kexec still beats a firmware reboot.
func releaseKexecTarget(cfg *config.MachineConfig, releaseTarget func() (disk.ReleaseReport, error)) {
	if !config.BoolOr(cfg.Provision.KexecUnmountTarget, true) {
		slog.Info("KEXEC_UNMOUNT_TARGET=false, keeping the target mounted for kexec", "root", installedRootPath)
		return
	}
	r, err := releaseTarget()
	slog.Info("released target filesystems before kexec", "root", installedRootPath,
		"unmounted", r.Unmounted, "readOnly", r.ReadOnly, "leftAlone", r.LeftAlone)
	if err != nil {
		slog.Warn("target filesystems are still writable at kexec, the installed OS may replay their journal",
			"writable", r.StillWrite, "error", err)
	}
}

func resolveKexecPath(root, grubPath string) string {
	path := strings.TrimSpace(grubPath)
	if path == "" {
		return ""
	}
	candidates := []string{pathInRoot(root, path)}
	if isRootRelativeBootArtifact(path) {
		candidates = append(candidates, pathInRoot(filepath.Join(root, "boot"), path))
	}
	for _, candidate := range candidates {
		resolved, ok, unsafe := resolveKexecCandidate(root, candidate)
		if ok {
			return resolved
		}
		if unsafe {
			return ""
		}
	}
	return candidates[0]
}

func resolveKexecCandidate(root, candidate string) (resolved string, ok, unsafe bool) {
	if !pathIsWithinRoot(root, candidate) {
		return "", false, true
	}
	if _, err := os.Lstat(candidate); err != nil { //nolint:gosec // candidate is normalized and checked to stay under the mounted target root before lstat.
		return "", false, !os.IsNotExist(err)
	}
	resolved, err := filepath.EvalSymlinks(candidate) //nolint:gosec // resolved symlink target is rechecked against the mounted target root before use.
	if err != nil {
		return "", false, true
	}
	if !pathIsWithinRoot(root, resolved) {
		return "", false, true
	}
	info, err := os.Stat(resolved) //nolint:gosec // resolved has been evaluated and checked to stay under the mounted target root.
	if err == nil && info.Mode().IsRegular() {
		return resolved, true, false
	}
	return "", false, false
}

func pathInRoot(root, path string) string {
	cleaned := filepath.Clean("/" + strings.TrimSpace(path))
	return filepath.Join(root, strings.TrimPrefix(cleaned, "/"))
}

func pathIsWithinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isRootRelativeBootArtifact(path string) bool {
	trimmed := strings.TrimPrefix(path, "/")
	if strings.HasPrefix(trimmed, "boot/") {
		return false
	}
	name := filepath.Base(trimmed)
	return strings.HasPrefix(name, "vmlinuz") ||
		strings.HasPrefix(name, "initrd") ||
		strings.HasPrefix(name, "initramfs")
}

func cleanupFailedKexecTarget(diskMgr *disk.Manager) {
	if err := diskMgr.UnmountRecursive(installedRootPath); err != nil {
		slog.Warn("failed to clean up target root after unsuccessful kexec", "root", installedRootPath, "error", err)
		return
	}
	slog.Info("cleaned up target root after unsuccessful kexec", "root", installedRootPath)
}

func requiresABKexec(cfg *config.MachineConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Provision.Image.Mode), config.ImageModeAB) &&
		cfg.Provision.AB.PreserveExisting
}
