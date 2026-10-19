//go:build linux

package main

import (
	"cmp"
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/crash"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/executil"
	nicfw "github.com/telekom/BOOTy/pkg/firmware/nic"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/kexec"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frr"
	"github.com/telekom/BOOTy/pkg/network/gobgp"
	"github.com/telekom/BOOTy/pkg/network/lldp"
	"github.com/telekom/BOOTy/pkg/network/netplan"
	"github.com/telekom/BOOTy/pkg/network/vlan"
	"github.com/telekom/BOOTy/pkg/oob"
	"github.com/telekom/BOOTy/pkg/provision"
	"github.com/telekom/BOOTy/pkg/realm"
	"github.com/telekom/BOOTy/pkg/runmode"
	"github.com/telekom/BOOTy/pkg/sriov"
	"github.com/telekom/BOOTy/pkg/ux"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
	// stopNetwork, when set, stops the network before the exit after a
	// failed handoff (the network that restoreNetwork set up again).
	stopNetwork func()
}

func main() {
	// "booty lldp ..." is the LLDP diagnosis command of the rescue shell.
	// PID 1 is the agent: the kernel passes unknown command line words to
	// init as arguments.
	if len(os.Args) > 1 && os.Args[1] == "lldp" && os.Getpid() != 1 {
		os.Exit(lldp.CLI(os.Args[2:], os.Stdout, os.Stderr, lldp.CLIDeps{NICs: network.DetectPhysicalNICsContext}))
	}
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
	setPanicReboot()
	loadModules()
	// The disk selection waits for the block devices to settle; count
	// their settle time from here, while the network comes up.
	disk.StartBlockWatch(ctx, "/sys", 250*time.Millisecond)

	// uptime is the seconds since the kernel started: the console has no
	// kernel banner at loglevel=4, so it marks the kernel start.
	slog.Info("starting BOOTy", "version", Version, "build", Build, "uptime", uptimeSeconds())
	logSecureBootState(readKernelLogFn())
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
	watchdogHandoff(false)
	stopLLDPEmit()
	flushObservability()
	realm.Reboot()
}

// The realm handoff requests; tests replace them.
var (
	realmRequestReboot   = realm.RequestReboot
	realmRequestPowerOff = realm.RequestPowerOff
)

func requestReboot() error {
	watchdogHandoff(false)
	stopLLDPEmit()
	flushObservability()
	return realmRequestReboot()
}

func requestPowerOff() error {
	watchdogHandoff(true)
	stopLLDPEmit()
	flushObservability()
	return realmRequestPowerOff()
}

func shell() {
	flushObservability()
	realm.Shell()
}

// The kexec hooks and the installed root; tests replace them.
var (
	flushLogs    = flushObservability
	kexecExecute = kexec.Execute
	kexecLoad    = kexec.Load
	kexecRoot    = installedRootPath
)

// doKexecExecute hands the watchdog over, ships the queued log lines,
// stops the network, then kexecs. The order matters: the log handler needs
// the network, and the switch should see the BGP Cease before the link
// goes quiet.
func doKexecExecute(stopNetwork func()) error {
	watchdogHandoff(false)
	stopLLDPEmit()
	flushLogs()
	stopNetwork()
	return kexecExecute()
}

func runCAPRF(ctx context.Context) {
	startOOBStatus(ctx, nil)
	client, err := caprf.New(varsPath)
	if err != nil {
		slog.Error("failed to create CAPRF client", "error", err)
		provision.DumpDebugState("caprf-init")
		// The vars file did not parse, but it may still ask to wait on
		// failure: then keep the ramdisk running instead of rebooting.
		failRun(ctx, caprf.FailureConfig(varsPath), nil, "config", provision.CodeConfig,
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
	if err := checkPayloads(); err != nil {
		code := provision.CodeOf(provision.WithCode("check-payloads", err))
		failRun(ctx, cfg, client, "check-payloads", code, fmt.Sprintf("ramdisk payload check failed [%s]: %v", code, err), reboot)
		return
	}
	startWatchdog(ctx, cfg)

	// Wire remote log shipping.
	if cfg.Transport.LogURL != "" {
		originalSlogHandler = slog.Default().Handler()
		remoteInner := slog.NewTextHandler(os.Stderr, nil)
		caprfRemoteLogHandler = caprf.NewRemoteHandler(client, remoteInner, slog.LevelInfo, 8192)
		defer flushObservability()
		slog.SetDefault(slog.New(caprfRemoteLogHandler))
	}

	// The console shows only kernel errors (loglevel=4). With DEBUG_URL
	// the server gets the whole kernel log once.
	go shipKernelLog(ctx, client, cfg.Transport.DebugURL)

	slog.Info("CAPRF mode active",
		"hostname", cfg.Hostname,
		"mode", cfg.Mode,
		"image_count", len(cfg.Provision.Image.URLs),
	)
	startOOBStatus(ctx, cfg)
	oob.Phase("network")

	// runCtx stops the active mode when JWT renewal is exhausted and
	// FAILURE_ACTION=wait, like fail() stops the legacy task.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()

	netMode, stopNetwork, err := setupNetworkAndTokenFlow(ctx, cfg, client, stopRun)
	if err != nil {
		failNetworkSetup(ctx, cfg, client, err, stopNetwork)
		return
	}

	diskMgr := disk.NewManager(newDiskCommander())
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
		stopAndReboot(stopNetwork)()
		return
	}

	slog.Info("executing mode", "mode", mode.Name())
	oob.Phase(mode.Name())
	modeErr := mode.Run(runCtx)
	if isTerminalSuccess(modeErr) {
		// A completed task wins over a token failure at the same moment
		// or later: the grace reboot must not cut the handoff.
		tokenTaskDone.Store(true)
		stopTokenGraceReboot()
	}
	if runCtx.Err() != nil && ctx.Err() == nil && !isTerminalSuccess(modeErr) {
		endTokenStoppedRun(ctx, cfg, client, stopNetwork)
		return
	}

	slog.Info("CAPRF run complete")
	endRun(ctx, cfg, client, diskMgr, stopNetwork, mode.Name(), modeErr)
}

// rebootNow and endRunPause are reboot and the pause before the reboot of
// a failed run; tests replace them.
var (
	rebootNow   = reboot
	shellHook   = shell
	endRunPause = 2 * time.Second
)

// endRun ends the run after the mode returned. Every path that leaves the
// ramdisk ships the queued log lines, then stops the network (BGP Cease,
// BFD AdminDown), then hands off; only the rescue shell keeps the network.
func endRun(ctx context.Context, cfg *config.MachineConfig, client provisionStatusReporter,
	diskMgr *disk.Manager, stopNetwork func(), modeName string, modeErr error) {
	leave := stopAndReboot(stopNetwork)

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
		shellHook()
		rebootNow()
		return
	case errors.As(modeErr, &rebootErr):
		leave()
		return
	case errors.As(modeErr, &powerOffErr):
		// Do not reboot when the power off fails: CAPRF has not ejected the
		// ISO yet, so a reboot would run the task again.
		ops := withNetworkStopBeforeHandoff(provisionHandoff, stopNetwork, func() func() { return restoreNetwork(ctx, cfg) })
		requestProvisionHandoff(ctx, client, "power off", ops.powerOff, ops,
			"power off handoff failed after deprovisioning success")
		return
	case errors.As(modeErr, &provisionErr):
		if provisionErr.Cfg != nil {
			// A standby command ran with its own config (payload).
			cfg = provisionErr.Cfg
		}
		finishProvision(ctx, cfg, client, diskMgr, stopNetwork, provisionErr)
		return
	}

	// HealthCheckError is an expected non-fatal exit (checks ran but some failed).
	// Log other unexpected errors at error level.
	var healthErr *runmode.HealthCheckError
	if modeErr != nil && !errors.As(modeErr, &healthErr) {
		slog.Error("mode exited with error", "mode", modeName, "error", modeErr)
	}
	time.Sleep(endRunPause)
	leave()
}

// networkStopTimeout bounds the network teardown before a kexec or reboot;
// tests shorten it.
var networkStopTimeout = 5 * time.Second

// setupNetworkModeFn is setupNetworkMode; tests replace it.
var setupNetworkModeFn = setupNetworkMode

// teardownDone is closed when the teardown of stopNetworkOnce has finished;
// nil before a stop. A teardown that timed out may still run, and it
// removes devices by name, so a restore must wait for it.
var teardownDone atomic.Pointer[chan struct{}]

// stopNetworkOnce returns a function that tears the network mode down once
// and waits at most networkStopTimeout. GoBGP sends a Cease NOTIFICATION to
// every peer (StopBgp), which ends the session on the switch at once,
// instead of after the BFD detection time or the BGP hold timer, so the next
// OS can peer without waiting. BFD sends AdminDown (bfd.Manager.Close),
// which only keeps the switch from taking the silence as a forwarding
// failure (RFC 5882 3.2).
func stopNetworkOnce(ctx context.Context, netMode network.Mode) func() {
	return sync.OnceFunc(func() {
		// The every-NIC LLDP sender first, so its shutdown LLDPDUs go out
		// while the NICs still have their links.
		stopLLDPEmit()
		if netMode == nil {
			return
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		teardownDone.Store(&finished)
		go func() {
			defer close(finished)
			done <- netMode.Teardown(context.WithoutCancel(ctx))
		}()
		select {
		case err := <-done:
			if err != nil {
				slog.Warn("network teardown before the handoff failed", "error", err)
				return
			}
			slog.Info("network stopped before the handoff")
		case <-time.After(networkStopTimeout):
			slog.Warn("network teardown before the handoff did not finish, going on", "timeout", networkStopTimeout)
		}
	})
}

// finishProvision starts the installed OS after a successful provision:
// kexec, or the handoff (reboot or power off). BGP and BFD stop cleanly
// before each of them, like the legacy `shutdown now` stops FRR before the
// power off.
func finishProvision(ctx context.Context, cfg *config.MachineConfig, client provisionStatusReporter,
	diskMgr *disk.Manager, stopNetwork func(), provisionErr *runmode.ProvisionCompleteError) {
	kexeced := tryKexec(cfg, provisionErr.FirmwareChanged, func() (disk.ReleaseReport, error) {
		return diskMgr.UnmountOrRemountReadOnly(installedRootPath)
	}, stopNetwork)
	if !kexeced && provision.ShouldKeepTargetRootMountedForKexec(cfg, provisionErr.FirmwareChanged) {
		cleanupFailedKexecTarget(diskMgr)
	}
	// No pause here: the handoff closes the CAPRF log handler, and that
	// drains the queued log lines (at most 5 s) before it acts.
	handleProvisionHandoff(ctx, cfg, client, provisionErr, kexeced,
		withNetworkStopBeforeHandoff(provisionHandoff, stopNetwork, func() func() { return restoreNetwork(ctx, cfg) }))
}

// withNetworkStopBeforeHandoff returns ops whose reboot and power off ship
// the queued log lines, then stop the network, then hand off. When the
// handoff fails, restore brings the network back, so that the failure can
// be reported and the rescue shell is reachable, and returns the stop of
// the restored network: the next handoff and the exit after the rescue
// shell stop that one (BGP Cease), the first stop already ran.
func withNetworkStopBeforeHandoff(ops provisionHandoffOps, stopNetwork func(), restore func() func()) provisionHandoffOps {
	var mu sync.Mutex
	current := stopNetwork
	stop := func() {
		mu.Lock()
		s := current
		mu.Unlock()
		s()
	}
	wrap := func(handoff func() error) func() error {
		return func() error {
			flushLogs()
			stop()
			err := handoff()
			if err != nil {
				restored := restore()
				mu.Lock()
				current = restored
				mu.Unlock()
				resumeLLDPEmit() // after the network is back: the host stays up
			}
			return err
		}
	}
	ops.reboot = wrap(ops.reboot)
	ops.powerOff = wrap(ops.powerOff)
	ops.stopNetwork = stop
	return ops
}

// restoreTimeout bounds the network setup and the one connectivity check
// of restoreNetwork; tests shorten it.
var restoreTimeout = time.Minute

// restoreNetwork sets the network up again after a failed handoff, so the
// error report reaches CAPRF; tests replace it. It waits for the teardown
// of the stop first (bounded by networkStopTimeout) and skips the restore
// while it still runs: the teardown removes the VRF, VXLAN and bridge by
// name and would remove the new ones. One setup and one connectivity check
// within restoreTimeout: the report is best effort. It returns the stop of
// the restored network, or a no-op without one.
var restoreNetwork = func(ctx context.Context, cfg *config.MachineConfig) func() {
	if done := teardownDone.Load(); done != nil {
		select {
		case <-*done:
		case <-time.After(networkStopTimeout):
			slog.Error("handoff failed and the network teardown still runs, not setting the network up again")
			return func() {}
		}
	}
	slog.Warn("handoff failed after the network stop, setting the network up again")
	netMode, ok := setupRestoredNetwork(ctx, cfg)
	if !ok {
		return func() {}
	}
	stop := stopNetworkOnce(ctx, netMode)
	target := cfg.Transport.ErrorURL
	if target == "" {
		target = cfg.Transport.InitURL
	}
	if target == "" {
		return stop
	}
	waitCtx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	if err := netMode.WaitForConnectivity(waitCtx, target, restoreTimeout); err != nil {
		slog.Error("no connectivity after the failed handoff", "error", err)
	}
	return stop
}

// setupRestoredNetwork sets the network up with the run context: the mode
// keeps goroutines on it (the GoBGP EVPN route watcher), so it must not be
// a context that ends with the restore. The wait is bounded by
// restoreTimeout instead; a setup that finishes later is torn down.
func setupRestoredNetwork(ctx context.Context, cfg *config.MachineConfig) (network.Mode, bool) {
	type result struct {
		mode network.Mode
		err  error
	}
	done := make(chan result, 1)
	go func() {
		mode, err := setupNetworkModeFn(ctx, cfg)
		done <- result{mode, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			slog.Error("network setup after the failed handoff failed", "error", r.err)
			return nil, false
		}
		return r.mode, true
	case <-time.After(restoreTimeout):
		slog.Error("network setup after the failed handoff did not finish in time", "timeout", restoreTimeout)
		go func() {
			if r := <-done; r.err == nil && r.mode != nil {
				_ = r.mode.Teardown(context.WithoutCancel(ctx))
			}
		}()
		return nil, false
	}
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
		if ops.stopNetwork != nil {
			// The exit of PID 1 ends the ramdisk; the restored network sends
			// its BGP Cease first.
			flushLogs()
			ops.stopNetwork()
		}
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

// setupNetworkAndTokenFlow sets the network and the token up. It returns
// the network mode and the function that stops it once (BGP Cease, BFD
// AdminDown) before the run leaves the ramdisk; the stop is also valid
// when a later setup step fails.
func setupNetworkAndTokenFlow(ctx context.Context, cfg *config.MachineConfig, client *caprf.Client, stopRun func()) (network.Mode, func(), error) {
	if cfg.Mode == "dry-run" {
		slog.Info("dry-run mode: skipping active network setup and token renewal")
		return noopNetworkMode{}, func() {}, nil
	}

	lldp.SetRunMode(cfg.Mode) // for the system description of LLDP_ANNOUNCE

	// Before the network setup: a driver may reset the port when the flag
	// changes, which would drop BGP sessions set up before.
	if cfg.DisableNICFirmwareLLDP() {
		disableNICFirmwareLLDP(ctx, &cfg.Provision.SRIOV)
		lldpPrepareNICs = func(nics []string) { disableNICFirmwareLLDPOn(nics, &cfg.Provision.SRIOV) }
	}

	// Set up networking with retry — if connectivity fails, teardown and
	// rebuild the entire network stack before giving up.
	netMode, err := setupNetworkModeFn(ctx, cfg)
	if err != nil {
		// A known error (a missing FRR payload) keeps its own code.
		return nil, func() {}, provision.WithCode("network-setup", err)
	}
	if lldpReceiveEnabled(cfg) {
		startLLDPReceive(ctx)
	}
	if cfg.Network.LLDP.EmitAllNICs() {
		startLLDPEmitAll(ctx, &cfg.Network.LLDP)
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
			return nil, stopNetworkOnce(ctx, activeMode), &provision.CodedError{Code: provision.CodeConnectivity, Err: err}
		}
		netMode = activeMode
	}
	stopNetwork := stopNetworkOnce(ctx, netMode)

	if err := syncClock(ctx, cfg, connectivityTarget); err != nil {
		provision.DumpDebugState("time-sync")
		return nil, stopNetwork, err
	}

	// Acquire JWT after network is ready so the token endpoint is reachable.
	if err := client.AcquireToken(ctx); err != nil {
		slog.Error("failed to acquire jwt token", "error", err)
		if cfg.Transport.TokenURL != "" {
			provision.DumpDebugState("jwt-acquire")
			return nil, stopNetwork, &provision.CodedError{Code: provision.CodeAuth, Err: err}
		}
	}

	// The grace reboot of the handler holds this first stop, not the stop
	// of a network that restoreNetwork sets up after a failed handoff. That
	// is safe today: a terminal success cancels the grace timer
	// (stopTokenGraceReboot) before any wrapped handoff runs.
	client.SetTokenRenewalFatalHandler(tokenFatalHandler(cfg, stopNetwork, stopRun))

	if err := client.StartTokenRenewal(ctx); err != nil {
		slog.Error("failed to start token renewal", "error", err)
		if cfg.Transport.TokenURL != "" {
			provision.DumpDebugState("jwt-renewal-start")
			return nil, stopNetwork, &provision.CodedError{Code: provision.CodeAuth, Err: err}
		}
	}

	return netMode, stopNetwork, nil
}

// stopAndReboot returns a reboot that ships the queued log lines and stops
// the network first.
func stopAndReboot(stopNetwork func()) func() {
	return func() {
		flushLogs()
		stopNetwork()
		rebootNow()
	}
}

// tokenFatalHandler is the token renewal fatal handler of the run: its
// grace reboot stops the network first.
func tokenFatalHandler(cfg *config.MachineConfig, stopNetwork, stopRun func()) func(error) {
	return func(err error) { tokenRenewalFatal(cfg, err, stopAndReboot(stopNetwork), stopRun) }
}

// failNetworkSetup ends a run whose network or token setup failed; a
// network that came up stops before the reboot.
func failNetworkSetup(ctx context.Context, cfg *config.MachineConfig, reporter provisionStatusReporter, err error, stopNetwork func()) {
	code := provision.CodeOf(err)
	failRun(ctx, cfg, reporter, "network-setup", code, fmt.Sprintf("network or token setup failed [%s]: %v", code, err), stopAndReboot(stopNetwork))
}

// endTokenStoppedRun ends a run that a token failure stopped.
func endTokenStoppedRun(ctx context.Context, cfg *config.MachineConfig, reporter provisionStatusReporter, stopNetwork func()) {
	tokenShutdown(ctx, cfg, reporter, tokenStopMessage(), stopAndReboot(stopNetwork))
}

// failRun ends a run that failed outside a mode in step with the failure
// code: it logs the failure and writes the BOOTY1 failed line. With
// FAILURE_ACTION=wait it reports the error like the legacy fail() and keeps
// the ramdisk running; a reboot would boot the ISO and run the task again.
func failRun(ctx context.Context, cfg *config.MachineConfig, reporter provisionStatusReporter, step, code, message string, rebootFn func()) {
	slog.Error("run failed", "step", step, "code", code, "error", message)
	oob.Fail(step, code)
	if cfg != nil && cfg.WaitOnFailure() {
		slog.Info("keeping the ramdisk running after the failure")
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

// setBGPHostname sets BGP_HOSTNAME as the ramdisk host name before the
// peers start: GoBGP sends the system host name (os.Hostname) as the FQDN
// capability. A failure is only logged, the FQDN is informational.
func setBGPHostname(name string, sethostname func([]byte) error) {
	if name == "" {
		return
	}
	if err := sethostname([]byte(name)); err != nil {
		slog.Warn("setting the BGP hostname failed", "hostname", name, "error", err)
		return
	}
	slog.Info("BGP hostname set", "hostname", name)
}

// isTerminalSuccess reports a mode result that completed its task. Such a
// result wins over a renewal failure that arrives at the same time.
func isTerminalSuccess(modeErr error) bool {
	var provisionErr *runmode.ProvisionCompleteError
	var powerOffErr *runmode.PowerOffRequestedError
	return errors.As(modeErr, &provisionErr) || errors.As(modeErr, &powerOffErr)
}

// tokenFatalReason is the reason of the token renewal failure that stopped
// the run, for tokenShutdown.
var tokenFatalReason atomic.Pointer[string]

// tokenShutdownGrace bounds how long the active task may take to stop after
// a token failure before BOOTy reboots anyway (not with FAILURE_ACTION=wait).
var tokenShutdownGrace = 2 * time.Minute

// tokenRenewalFatal handles a token that can not be renewed or expires: it
// stops the active task (the standby loop or a provision) with stopRun, and
// runCAPRF then reports the reason and ends like a failed run
// (tokenShutdown). Without FAILURE_ACTION=wait a task that does not stop
// within tokenShutdownGrace is ended with a reboot.
func tokenRenewalFatal(cfg *config.MachineConfig, reason error, rebootFn, stopRun func()) {
	if tokenTaskDone.Load() {
		// The task already completed; the handoff owns the end of the run.
		slog.Warn("token can not be renewed after the task completed", "reason", reason)
		return
	}
	provision.DumpDebugState("jwt-renewal-exhausted")
	msg := fmt.Sprintf("token renewal failed, task stopped: %v", reason)
	tokenFatalReason.Store(&msg)
	slog.Error("token can not be renewed, stopping the task", "reason", reason)
	stopRun()
	if cfg == nil || !cfg.WaitOnFailure() {
		tokenGraceTimer.Store(time.AfterFunc(tokenShutdownGrace, rebootFn))
	}
}

// tokenTaskDone is set when the mode completed its task; a later token
// failure then arms no grace reboot.
var tokenTaskDone atomic.Bool

// tokenGraceTimer is the pending grace reboot of tokenRenewalFatal.
var tokenGraceTimer atomic.Pointer[time.Timer]

// stopTokenGraceReboot cancels a pending grace reboot.
func stopTokenGraceReboot() {
	if t := tokenGraceTimer.Load(); t != nil {
		t.Stop()
	}
}

// tokenStopMessage is the reason of a stopped run.
func tokenStopMessage() string {
	if msg := tokenFatalReason.Load(); msg != nil {
		return *msg
	}
	return "token renewal failed, task stopped"
}

// tokenShutdown ends a run that a token failure stopped: it reports the
// reason to ERROR_URL while the token may still work, then waits with
// FAILURE_ACTION=wait, else reboots.
func tokenShutdown(ctx context.Context, cfg *config.MachineConfig, reporter provisionStatusReporter, message string, rebootFn func()) {
	slog.Error("run stopped", "reason", message)
	if reporter != nil {
		reportCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := reporter.ReportStatus(reportCtx, config.StatusError, message); err != nil {
			slog.Warn("failed to report the token failure", "error", err)
		}
		cancel()
	}
	if cfg != nil && cfg.WaitOnFailure() {
		<-ctx.Done()
	}
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
			newMode, setupErr := setupNetworkModeFn(ctx, cfg)
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

// waitForConnectivity runs one connectivity wait. NETWORK_WAIT_MODE=rounds
// bounds the whole call with the timeout: GoBGP otherwise waits for the
// underlay and then for the target, each up to the timeout.
func waitForConnectivity(ctx context.Context, cfg *config.MachineConfig, netMode network.Mode, target string, timeout time.Duration) error {
	if cfg.Network.WaitMode == config.NetworkWaitRounds {
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
// timeout of each. NETWORK_WAIT_MODE=rounds follows wait_for_network in the
// CAPRF common.sh: NETWORK_WAIT_MAX_ATTEMPTS rounds of 20 seconds (default
// 15) on one network setup, and then the run fails.
func networkWait(cfg *config.MachineConfig) (int, time.Duration) {
	if cfg.Network.WaitMode != config.NetworkWaitRounds {
		return 3, 5 * time.Minute
	}
	rounds := 15
	if n := cfg.Network.WaitMaxAttempts; n != nil {
		rounds = *n // an explicit 0 fails at once, like the legacy loop
	}
	return 1, time.Duration(rounds) * 20 * time.Second
}

// lldpReceiveEnabled is LLDP_RECEIVE, on by default.
func lldpReceiveEnabled(cfg *config.MachineConfig) bool {
	return config.BoolOr(cfg.Network.LLDPReceive, true)
}

// startLLDPReceive logs and records the LLDP neighbors of every physical
// NIC for the rest of the run. The legacy ramdisk only sends LLDP. The NIC
// detection runs in the background: with one NIC it waits 3 s for more
// NICs to appear, and the connectivity check must not wait for that.
func startLLDPReceive(ctx context.Context) {
	lldp.ReceiveDetected(ctx, network.DetectPhysicalNICsContext, slog.With("component", "lldp"))
}

// disableNICFirmwareLLDP stops the firmware LLDP agent of the Intel NICs
// (LLDP_DISABLE_FW_AGENT, on by default), which otherwise consumes the
// switch's LLDP frames. The ports whose agent an SR-IOV profile sets are
// left to that profile. A failure is only logged.
func disableNICFirmwareLLDP(ctx context.Context, sriovCfg *sriov.Config) {
	nics, err := detectNICs(ctx)
	if err != nil {
		slog.Warn("firmware LLDP agent not disabled: no physical NICs", "error", err)
		return
	}
	disableNICFirmwareLLDPOn(nics, sriovCfg)
}

// disableNICFirmwareLLDPOn is disableNICFirmwareLLDP for the given NICs,
// also for a NIC that appears later.
func disableNICFirmwareLLDPOn(nics []string, sriovCfg *sriov.Config) {
	if len(nics) == 0 {
		return
	}
	owned, err := sriovCfg.FWLLDPAgentOwners(discoverPFs)
	if err != nil {
		slog.Warn("firmware LLDP agent not disabled: SR-IOV ports unknown", "error", err)
		return
	}
	for _, nic := range nics {
		if slices.Contains(owned, nic) {
			slog.Info("firmware LLDP agent left to the SR-IOV profile", "interface", nic)
			continue
		}
		flag, err := disableFirmwareLLDP(nic)
		switch {
		case err != nil:
			slog.Warn("firmware LLDP agent not disabled", "interface", nic, "flag", flag, "error", err)
		case flag != "":
			slog.Info("firmware LLDP agent disabled", "interface", nic, "flag", flag)
		}
	}
}

// Seams of disableNICFirmwareLLDP.
var (
	detectNICs          = network.DetectPhysicalNICsContext
	discoverPFs         = func() ([]sriov.PF, error) { return sriov.NewInspector().Discover() }
	disableFirmwareLLDP = nicfw.DisableFirmwareLLDP
)

// startLLDPEmitAll sends LLDP frames on every physical NIC with carrier for
// the rest of the run (LLDP_EMIT_ALL), in every network mode.
func startLLDPEmitAll(ctx context.Context, l *config.LLDPConfig) {
	nics, err := network.DetectPhysicalNICsContext(ctx)
	if err != nil || len(nics) == 0 {
		slog.Warn("LLDP not sent: no physical NICs", "error", err)
		return
	}
	info := lldp.SystemInfo()
	info.Profile, info.PortIDFormat, info.Announce = l.Profile, l.PortIDFormat, l.Announce
	info.CarrierOnly = true
	// Its own context: the run context is not canceled before kexec,
	// reboot or power off, so the handoff stops it to send the shutdown
	// LLDPDU (IEEE 802.1AB), like the GoBGP sender at its teardown.
	start := func() func() {
		emitCtx, cancel := context.WithCancel(ctx)
		wait := lldp.Emit(emitCtx, nics, &info, slog.With("component", "lldp"))
		return func() {
			cancel()
			waitLLDPEmit(wait)
		}
	}
	lldpEmitMu.Lock()
	lldpEmitStart = start
	lldpEmitMu.Unlock()
	setLLDPEmitStop(start())
}

// lldpEmitStop stops the every-NIC LLDP sender; nil when none runs.
// lldpEmitStart starts it again and returns its stop function.
// lldpEmitPending is closed when the senders of a stop that timed out have
// ended; nil when none is pending.
var (
	lldpEmitMu      sync.Mutex
	lldpEmitStop    func()
	lldpEmitStart   func() func()
	lldpEmitPending <-chan struct{}
)

// lldpStopTimeout bounds the wait for the shutdown LLDPDUs: BOOTy is PID 1
// right before kexec or power off and must not hang there.
// lldpResumeWait bounds the wait of a restart for senders still alive.
var (
	lldpStopTimeout = 2 * time.Second
	lldpResumeWait  = 500 * time.Millisecond
)

func setLLDPEmitStop(stop func()) {
	lldpEmitMu.Lock()
	defer lldpEmitMu.Unlock()
	lldpEmitStop = stop
}

// waitLLDPEmit waits for the senders, at most lldpStopTimeout. Their done
// channel is pending from the start of the stop, so a restart, also one
// during the wait, does not run next to them.
func waitLLDPEmit(wait func()) {
	done := make(chan struct{})
	lldpEmitMu.Lock()
	lldpEmitPending = done
	lldpEmitMu.Unlock()
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lldpStopTimeout):
		slog.Warn("LLDP senders did not stop in time, handing off without waiting", "timeout", lldpStopTimeout)
	}
}

// stopLLDPEmit stops the every-NIC LLDP sender, which then sends the
// shutdown LLDPDU on each NIC, and waits for it (bounded). The network
// teardown before a reboot and every handoff call it; a second call does
// nothing.
func stopLLDPEmit() {
	lldpEmitMu.Lock()
	stop := lldpEmitStop
	lldpEmitStop = nil
	lldpEmitMu.Unlock()
	if stop != nil {
		stop()
	}
}

// resumeLLDPEmit starts the every-NIC LLDP sender again after a handoff
// request failed and the host stays up; nothing when it runs or was never
// started. The check, the start and the store happen under lldpEmitMu, so
// a concurrent stop or resume cannot leave a sender nobody can stop. When
// the senders of a timed-out stop are still alive after lldpResumeWait, it
// does not restart: their late shutdown LLDPDU (TTL 0) would follow the
// new frames and remove the host from the neighbor tables.
func resumeLLDPEmit() {
	lldpEmitMu.Lock()
	defer lldpEmitMu.Unlock()
	if lldpEmitStart == nil || lldpEmitStop != nil {
		return
	}
	if pending := lldpEmitPending; pending != nil {
		select {
		case <-pending:
			lldpEmitPending = nil
		case <-time.After(lldpResumeWait):
			slog.Warn("LLDP not restarted: the senders of the last stop are still running")
			return
		}
	}
	slog.Info("handoff failed, sending LLDP frames again")
	lldpEmitStop = lldpEmitStart()
}

// setupNetworkMode detects and configures the appropriate network mode.
// defaultNICWaitSeconds is the wait for configured NICs without
// NIC_WAIT_SECONDS.
const defaultNICWaitSeconds = 30

// nicWait is the wait for configured NICs: NIC_WAIT_SECONDS, 0 for the
// default, -1 for none.
func nicWait(seconds int) time.Duration {
	switch {
	case seconds < 0:
		return 0
	case seconds == 0:
		return defaultNICWaitSeconds * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func setupNetworkMode(ctx context.Context, cfg *config.MachineConfig) (_ network.Mode, err error) {
	defer func() {
		// Every failure of the setup is visible in the log, also when the
		// run ends without FAILURE_ACTION=wait.
		if err != nil {
			slog.Error("network setup failed", "error", err)
		}
	}()
	netCfg := networkConfigFromMachineConfig(cfg)

	// Auto-detect netplan configuration files injected by the provisioner.
	// Netplan-derived values override vars-based values, allowing the same
	// config format used by the old deployer to work as a drop-in with BOOTy.
	netplanStrict, frrStrict := inputStrictness(cfg)
	npCfg, err := detectNetplanConfigWith(deployNetplanDir, deployFRRConfPath, netplanStrict, frrStrict, cfg.Network.Devices.Section())
	if err != nil {
		return nil, err
	}
	ignored := setPeerVars(netCfg)
	if npCfg != nil {
		mergeNetplanConfig(netCfg, npCfg)
		// The legacy ramdisk reads these files with udev names on every NIC.
		netCfg.UdevNames = true
	}
	if err := applyNativeSessions(&cfg.Network.BGP, netCfg); err != nil {
		return nil, err
	}
	warnIgnoredPeerVars(netCfg, ignored)
	applyBGPOptions(&cfg.Network.BGP, netCfg)
	keepProvisionerResolvConf(cfg, netCfg)

	// Parse VLAN configuration.
	if cfg.Network.VLAN.Config != "" {
		vlans, err := network.ParseVLANs(cfg.Network.VLAN.Config)
		if err != nil {
			return nil, fmt.Errorf("invalid VLAN configuration: %w", err)
		}
		netCfg.VLANs = vlans
	}

	if config.BoolOr(cfg.Network.PredictableNames, true) {
		netCfg.NICWait = nicWait(cfg.Network.NICWaitSeconds)
		if err := network.ApplyPredictableNames(ctx, netCfg); err != nil {
			return nil, fmt.Errorf("predictable NIC names: %w", err)
		}
	}

	linkCleanup, err := prepareLinkLayers(ctx, netCfg)
	if err != nil {
		return nil, err
	}
	// SR-IOV changes must leave the interfaces of this network alone; the
	// static interface of a netplan match is known from here on.
	network.RecordUsedInterfaces(netCfg)

	// links mode is explicit only: link-local, no DHCP, no BGP.
	if netCfg.IsLinksMode() {
		slog.Info("using links network mode")
		mode := &network.LinksMode{}
		if err := mode.Setup(ctx, netCfg); err != nil {
			if cleanupErr := linkCleanup.Teardown(ctx); cleanupErr != nil {
				slog.Warn("link-layer cleanup after links setup failure failed", "error", cleanupErr)
			}
			return nil, fmt.Errorf("links network setup: %w", err)
		}
		return networkModeWithResolvers(ctx, netCfg, mode, linkCleanup)
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
		if cfg.Network.BGP.HasSessions() {
			slog.Warn("network.bgp.sessions (BGP_SESSIONS) applies to GoBGP mode only; the FRR stack ignores it")
		}
		if err := requireFRRPayload(ctx, linkCleanup); err != nil {
			return nil, err
		}
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
	netCfg := &network.Config{
		UnderlaySubnet:          cfg.Network.EVPN.UnderlaySubnet,
		UnderlayIP:              cfg.Network.EVPN.UnderlayIP,
		OverlaySubnet:           cfg.Network.EVPN.OverlaySubnet,
		IPMISubnet:              cfg.Network.IPMI.Subnet,
		ASN:                     cfg.Network.BGP.ASN,
		ProvisionVNI:            cfg.Network.EVPN.ProvisionVNI,
		ProvisionIP:             cfg.Network.EVPN.ProvisionIP,
		ProvisionGateway:        cfg.Network.EVPN.ProvisionGateway,
		LegacyFRRRecovery:       cfg.Network.WaitMode == config.NetworkWaitRounds,
		DNSResolvers:            cfg.Network.DNSResolvers,
		DCGWIPs:                 cfg.Network.EVPN.DCGWIPs,
		LeafASN:                 cfg.Network.EVPN.LeafASN,
		LocalASN:                cfg.Network.EVPN.LocalASN,
		OverlayAggregate:        cfg.Network.EVPN.OverlayAggregate,
		VPNRT:                   cfg.Network.EVPN.VPNRT,
		StaticIP:                cfg.Network.Static.IP,
		StaticGateway:           cfg.Network.Static.Gateway,
		StaticIface:             cfg.Network.Static.Iface,
		BondInterfaces:          cfg.Network.Bond.Interfaces,
		BondMode:                cfg.Network.Bond.Mode,
		VRFName:                 cfg.Network.VRF.Name,
		VRFTableID:              cfg.Network.VRF.TableID,
		BGPKeepalive:            cfg.Network.BGP.Keepalive,
		BGPHold:                 cfg.Network.BGP.Hold,
		BFDTransmitMS:           cfg.Network.BGP.BFDTransmitMS,
		BFDReceiveMS:            cfg.Network.BGP.BFDReceiveMS,
		BFDDetectMult:           cfg.Network.BGP.BFDDetectMultiplier,
		NetworkMode:             cfg.Network.Mode,
		BGPPeerMode:             network.ParsePeerMode(cfg.Network.BGP.PeerMode),
		BGPInterfaces:           cfg.Network.BGP.Interfaces,
		BGPNeighbors:            cfg.Network.BGP.Neighbors,
		BGPRemoteASN:            cfg.Network.BGP.RemoteASN,
		BGPUnderlayAF:           cfg.Network.BGP.UnderlayAF,
		BGPOverlayType:          cfg.Network.BGP.OverlayType,
		EVPNL2Enabled:           cfg.Network.EVPN.L2Enabled,
		EVPNResolveOverlayIndex: cfg.Network.EVPN.ResolveOverlayIndex,
		EVPNType5Only:           cfg.Network.EVPN.Type5Only != nil && *cfg.Network.EVPN.Type5Only,
		EVPNType5OnlySet:        cfg.Network.EVPN.Type5Only != nil,
		Type5Networks:           splitList(cfg.Network.EVPN.Type5Networks),
		LoopbackAddrs:           splitList(cfg.Network.LoopbackAddresses),
		BlackholeRoutes:         splitList(cfg.Network.BlackholeRoutes),
		BridgeName:              cfg.Network.EVPN.BridgeName,
		VXLANName:               cfg.Network.EVPN.VXLANName,
		BridgeMAC:               cfg.Network.EVPN.BridgeMAC,
		VXLANLink:               cfg.Network.EVPN.VXLANLink,
		UnderlayDummyName:       cfg.Network.EVPN.UnderlayDummyName,
		MTU:                     cfg.Network.MTU,
		OverlayVRFTableID:       cfg.Network.VRF.OverlayTableID,
		BGPLocalAS:              cfg.Network.BGP.LocalAS,
		OverlayDevices:          cfg.Network.Overlay,
		NoAcceptRA:              splitList(cfg.Network.NoAcceptRA),
		EmitLLDP:                splitList(cfg.Network.EmitLLDP),
		LLDPProfile:             cfg.Network.LLDP.Profile,
		LLDPPortIDFormat:        cfg.Network.LLDP.PortIDFormat,
		LLDPAnnounce:            cfg.Network.LLDP.Announce,
		LLDPEmitAll:             cfg.Network.LLDP.EmitAllNICs(),
		IPForwarding:            cfg.Network.IPForwarding,
		IPv6Forwarding:          cfg.Network.IPv6Forwarding,
		BGPRouteMaps:            cfg.Network.BGP.RouteMaps,
		BGPCommunityLists:       cfg.Network.BGP.CommunityLists,
		BGPRouteMapIn:           cfg.Network.BGP.RouteMapIn,
		BGPRouteMapOut:          cfg.Network.BGP.RouteMapOut,
		EVPNImportRTs:           splitList(cfg.Network.EVPN.ImportRouteTargets),
		EVPNExportRTs:           splitList(cfg.Network.EVPN.ExportRouteTargets),
		EVPNRD:                  cfg.Network.EVPN.RD,
		BGPNeighborVRF:          cfg.Network.BGP.NeighborVRF,
		BGPConnectRetry:         cfg.Network.BGP.ConnectRetry,
		BGPAuthPassword:         cfg.Network.BGP.AuthPassword,
		BGPMinPeers:             cfg.Network.BGP.MinPeers,
	}
	// OVERLAY_VRF_NAME=default keeps the bridge in the default VRF.
	if name := cfg.Network.VRF.OverlayName; name != "" {
		netCfg.OverlayVRFSet = true
		if name != "default" {
			netCfg.OverlayVRFName = name
		}
	}
	return netCfg
}

// frrVersionBelow reports whether the "frr version" v is below
// major.minor, like the match_version of FRR's FRR_CFG_DEFAULT_*. An empty
// or unparsable version is the running FRR, so it is not below.
func frrVersionBelow(v string, major, minor int) bool {
	f := strings.SplitN(v, ".", 3)
	if len(f) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(f[0])
	mi, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma < major || ma == major && mi < minor
}

// applyBGPOptions applies the BGP options that frr.conf can also set. With
// any BGP sessions (frr.conf or network.bgp.sessions) the default is FRR's
// (routes installed); the BGP_PEER_MODE peers default it off. fast-external-failover is set in
// applyNativeSessions. An explicit router option wins. The peer options
// (local-as, VRF, connect retry, allowas-in) are defaults for the sessions
// that do not set the value themselves.
func applyBGPOptions(o *config.BGPConfig, n *network.Config) {
	withSessions := len(n.BGPSessions) > 0
	n.BGPInstallRoutes = boolOr(o.InstallRoutes, withSessions)
	n.BGPConnectAtOnce = boolOr(o.ConnectAtOnce, false)
	if o.MultipathRelax != nil {
		n.BGPMultipathRelax = *o.MultipathRelax
	}
	// The "frr defaults" profile: BGP_DEFAULTS wins over frr.conf; without
	// both it is datacenter, the profile of the legacy daemons file.
	if o.Defaults != "" {
		n.BGPDefaults = o.Defaults
	}
	n.BGPDefaults = cmp.Or(n.BGPDefaults, "datacenter")
	n.BGPHostname = o.Hostname
	datacenter := n.BGPDefaults == "datacenter"
	gr := boolOr(o.GracefulRestart, true)
	n.BGPCapabilities = network.BGPCapabilities{
		GracefulRestart:     gr,
		GracefulRestartTime: cmp.Or(o.GracefulRestartTime, 120),
		// FRR BGP_GRACEFUL_NOTIFICATION: off for "frr version" < 8.3.
		GracefulNotification: gr && boolOr(o.GracefulNotification, !frrVersionBelow(n.BGPFRRVersion, 8, 3)),
		AddPathReceive:       boolOr(o.AddPathReceive, true),
		// FRR 10.3 BGP_SOFT_VERSION_CAPABILITY: on for every peer with
		// datacenter, iBGP only otherwise.
		SoftwareVersion: boolOr(o.SoftwareVersion, datacenter),
	}
	if o.AllowASIn != nil {
		n.BGPAllowASIn = *o.AllowASIn
	}
	if o.EBGPRequiresPolicy != nil {
		n.BGPEBGPRequiresPolicy = *o.EBGPRequiresPolicy
	}
	for i := range n.BGPSessions {
		s := &n.BGPSessions[i]
		if o.EBGPRequiresPolicy != nil {
			s.EBGPRequiresPolicy = *o.EBGPRequiresPolicy
		}
		if o.AllowASIn != nil && s.AllowASIn == 0 {
			s.AllowASIn = int(*o.AllowASIn)
		}
		if o.ConnectRetry > 0 && !s.ConnectRetrySet {
			s.ConnectRetry, s.ConnectRetrySet = o.ConnectRetry, true
		}
		if o.LocalAS > 0 && !s.Internal && s.LocalASN == 0 {
			s.LocalASN = o.LocalAS
		}
		if o.NeighborVRF != "" && !s.Interface && s.VRF == "" {
			s.VRF = o.NeighborVRF
		}
		if o.RouteMapIn != "" || o.RouteMapOut != "" {
			s.RouteMaps = explicitRouteMaps(s, o.RouteMapIn, o.RouteMapOut)
		}
		if s.BFD && o.BFDTransmitMS > 0 {
			s.BFDTransmitMS, s.BFDReceiveMS = o.BFDTransmitMS, o.BFDReceiveMS
		}
		if s.BFD && o.BFDDetectMultiplier > 0 {
			s.BFDDetectMult = o.BFDDetectMultiplier
		}
	}
}

// explicitRouteMaps applies BGP_ROUTE_MAP_IN/OUT to every activated family
// of a session; an unset direction keeps the frr.conf route-map.
func explicitRouteMaps(s *network.BGPSession, in, out string) []network.SessionRouteMaps {
	var fams []string
	for f, on := range map[string]bool{"ipv4-unicast": s.IPv4Unicast, "ipv6-unicast": s.IPv6Unicast, "l2vpn-evpn": s.L2VPNEVPN} {
		if on {
			fams = append(fams, f)
		}
	}
	slices.Sort(fams)
	maps := make([]network.SessionRouteMaps, 0, len(fams))
	for _, f := range fams {
		rm := network.SessionRouteMaps{Family: f, In: in, Out: out}
		for _, old := range s.RouteMaps {
			if old.Family == f && in == "" {
				rm.In = old.In
			}
			if old.Family == f && out == "" {
				rm.Out = old.Out
			}
		}
		maps = append(maps, rm)
	}
	return maps
}

func boolOr(v *bool, def bool) bool {
	if v != nil {
		return *v
	}
	return def
}

// keepProvisionerResolvConf leaves the provisioner /etc/resolv.conf in the
// ramdisk alone with PROVISIONER_FILES_TARGET=ramdisk: copy_files puts it in
// place and the legacy scripts never rewrite it.
func keepProvisionerResolvConf(cfg *config.MachineConfig, netCfg *network.Config) {
	if cfg.ProvisionerFilesInRamdisk() && provision.HasProvisionerResolvConf() {
		slog.Info("keeping the provisioner resolv.conf in the ramdisk")
		netCfg.DNSResolvers = ""
		netCfg.KeepResolvConf = true
	}
}

// configureResolvers writes the configured resolvers; tests replace it.
var configureResolvers = network.ConfigureResolvers

func networkModeWithResolvers(ctx context.Context, netCfg *network.Config, mode network.Mode, cleanup *linkLayerCleanup) (network.Mode, error) {
	configure := configureResolvers
	if m, ok := mode.(interface{ ManagesDNS() bool }); ok && m.ManagesDNS() {
		// DHCP mode writes the configured and the lease servers itself:
		// only check the configured ones here, do not overwrite.
		configure = func(r string) error { _, err := network.ResolverLines(r); return err }
	}
	if err := configure(netCfg.DNSResolvers); err != nil {
		if cleanupErr := mode.Teardown(ctx); cleanupErr != nil {
			slog.Warn("network mode cleanup after resolver setup failure failed", "error", cleanupErr)
		}
		if cleanupErr := cleanup.Teardown(ctx); cleanupErr != nil {
			slog.Warn("link-layer cleanup after resolver setup failure failed", "error", cleanupErr)
		}
		return nil, fmt.Errorf("configure initramfs DNS: %w", err)
	}
	// networkd sends LLDP with "emit-lldp: true" whatever the addressing.
	// The GoBGP underlay sends itself; the other modes send from here, after
	// their setup brought the NICs up.
	// With LLDP_EMIT_ALL the all-NIC sender runs instead (no duplicates).
	if (len(netCfg.EmitLLDP) > 0 || len(netCfg.EmitLLDPMatches) > 0) && !netCfg.IsGoBGPMode() && !netCfg.LLDPEmitAll && cleanup != nil {
		cleanup.stopLLDP = startLLDPEmit(netCfg)
	}
	return wrapLinkLayerMode(mode, cleanup), nil
}

// resolveLinkMatch is network.ResolveLinkMatch; tests replace it.
var (
	resolveLinkMatch   = network.ResolveLinkMatch
	resolveLinkMatches = network.ResolveLinkMatches
	startLLDPEmit      = emitLLDPOnNetplanNICs
	lldpLinkSubscribe  = netlink.LinkSubscribeWithOptions
	// lldpPrepareNICs runs for a NIC that appears later before LLDP is sent
	// on it; the network setup sets it from the config.
	lldpPrepareNICs = func([]string) {}
	lldpEmit        = lldp.Emit
)

// followLLDPMatches calls start with the NICs the matches select when a
// NIC that is not known yet appears, until ctx ends.
func followLLDPMatches(ctx context.Context, matches []network.LinkMatch, known func(string) bool, start func([]string), log *slog.Logger) {
	go func() {
		// The subscription ends on a netlink error: subscribe again.
		for ctx.Err() == nil {
			updates := make(chan netlink.LinkUpdate, 16)
			opts := netlink.LinkSubscribeOptions{ErrorCallback: func(err error) {
				log.Warn("link watch for LLDP failed, subscribing again", "error", err)
			}}
			if err := lldpLinkSubscribe(updates, ctx.Done(), opts); err != nil {
				log.Warn("NICs that appear later send no LLDP", "error", err)
				return
			}
			for u := range updates {
				if u.Header.Type != unix.RTM_NEWLINK || u.Link == nil || known(u.Attrs().Name) {
					continue
				}
				names, err := resolveLinkMatches(ctx, matches)
				if err != nil {
					continue
				}
				fresh := slices.DeleteFunc(names, known)
				lldpPrepareNICs(fresh) // the firmware LLDP agent off, like at the start
				start(fresh)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
}

// emitLLDPOnNetplanNICs sends LLDP on the NICs with netplan "emit-lldp:
// true", with the configured profile like startLLDPEmitAll, and on every
// NIC that appears later and a match block selects, like networkd matches
// a link when udev announces it. It returns the stop function.
func emitLLDPOnNetplanNICs(netCfg *network.Config) func() {
	ctx, cancel := context.WithCancel(context.Background())
	info := lldp.SystemInfo()
	info.Profile, info.PortIDFormat, info.Announce = netCfg.LLDPProfile, netCfg.LLDPPortIDFormat, netCfg.LLDPAnnounce
	log := slog.With("component", "lldp")
	var mu sync.Mutex
	var waits []func()
	sending := map[string]bool{}
	start := func(names []string) {
		mu.Lock()
		defer mu.Unlock()
		var add []string
		for _, n := range names {
			if !sending[n] && ctx.Err() == nil {
				sending[n] = true
				add = append(add, n)
			}
		}
		if len(add) > 0 {
			waits = append(waits, lldpEmit(ctx, add, &info, log))
		}
	}
	start(netCfg.EmitLLDP)
	if len(netCfg.EmitLLDPMatches) > 0 {
		known := func(name string) bool { mu.Lock(); defer mu.Unlock(); return sending[name] }
		followLLDPMatches(ctx, netCfg.EmitLLDPMatches, known, start, log)
	}
	return func() {
		cancel()
		mu.Lock()
		defer mu.Unlock()
		for _, w := range waits {
			w()
		}
	}
}

func prepareLinkLayers(ctx context.Context, netCfg *network.Config) (*linkLayerCleanup, error) {
	cleanup := &linkLayerCleanup{}
	if err := network.ApplyLinkRenames(ctx, netCfg.LinkRenames); err != nil {
		return nil, fmt.Errorf("netplan set-name: %w", err)
	}
	if names, err := resolveLinkMatches(ctx, netCfg.EmitLLDPMatches); err != nil {
		slog.Warn("netplan match blocks with emit-lldp not resolved", "error", err)
	} else {
		for _, n := range names {
			if !slices.Contains(netCfg.EmitLLDP, n) {
				netCfg.EmitLLDP = append(netCfg.EmitLLDP, n)
			}
		}
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
	bond     network.Mode
	vlans    []network.VLANConfig
	stopLLDP func()
}

func (c *linkLayerCleanup) empty() bool {
	return c == nil || (c.bond == nil && len(c.vlans) == 0 && c.stopLLDP == nil)
}

func (c *linkLayerCleanup) Teardown(ctx context.Context) error {
	if c.empty() {
		return nil
	}
	c.stopLLDPNow()
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

// Teardown stops the LLDP sender first, so its shutdown frames go out
// before the network mode stops (FRR, within the network stop timeout).
func (m *linkLayerNetworkMode) Teardown(ctx context.Context) error {
	m.cleanup.stopLLDPNow()
	return errors.Join(m.Mode.Teardown(ctx), m.cleanup.Teardown(ctx))
}

// stopLLDPNow stops the LLDP sender of the cleanup, once.
func (c *linkLayerCleanup) stopLLDPNow() {
	if c != nil && c.stopLLDP != nil {
		c.stopLLDP()
		c.stopLLDP = nil
	}
}

// dhcpFallback creates a DHCP mode and attempts setup.
// Returns the mode even if setup fails, so the caller can still proceed.
func dhcpFallback(ctx context.Context, netCfg *network.Config) network.Mode {
	dhcp := network.NewDHCPMode()
	if err := dhcp.Setup(ctx, netCfg); err != nil {
		slog.Error("DHCP setup failed", "error", err)
	}
	// netCfg.DNSResolvers stays: networkModeWithResolvers checks it (DHCP
	// mode writes resolv.conf itself), so a bad resolver fails the run.
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
// bring up a different network than the operator configured. Without
// netplanStrict or frrStrict, a key or statement that BOOTy does not
// implement is logged and skipped instead.
func detectNetplanConfig(netplanDir, frrConfPath string, netplanStrict, frrStrict bool) (*network.Config, error) {
	return detectNetplanConfigWith(netplanDir, frrConfPath, netplanStrict, frrStrict, nil)
}

// detectNetplanConfigWith is detectNetplanConfig with the native devices
// (network.devices), which replace the provisioner netplan files as a whole.
func detectNetplanConfigWith(netplanDir, frrConfPath string, netplanStrict, frrStrict bool, devices *netplan.NetworkSection) (*network.Config, error) {
	found, err := netplan.HasNetplanFiles(netplanDir)
	if err != nil {
		return nil, err //nolint:wrapcheck // already names the dir
	}
	var np *netplan.Config
	switch {
	case devices != nil:
		if found {
			slog.Info("network.devices replaces the provisioner netplan files", "dir", netplanDir)
		}
		np = &netplan.Config{Network: *devices}
		if err := netplan.Check(np); err != nil {
			return nil, fmt.Errorf("network.devices: %w", err)
		}
	case !found:
		return nil, nil //nolint:nilnil // no netplan files means no netplan config, not an error
	default:
		slog.Info("detected netplan configuration files", "dir", netplanDir)
		if np, err = netplan.ParseDirWith(netplanDir, netplanStrict); err != nil {
			return nil, fmt.Errorf("parse netplan files in %s: %w", netplanDir, err)
		}
	}

	var frrParams *netplan.FRRParams
	data, frrErr := os.ReadFile(frrConfPath)
	switch {
	case frrErr == nil:
		parsed, parseErr := netplan.ParseFRRConfigBytesWith(data, frrConfPath, frrStrict)
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
	nativeWins(&dst.BridgeName, src.BridgeName)
	// The netplan MAC belongs to one bridge. Only use it for that bridge.
	if src.BridgeNameOrDefault() == dst.BridgeNameOrDefault() {
		nativeWins(&dst.BridgeMAC, src.BridgeMAC)
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
	mergeInferredEVPNMode(dst, src)
	if src.BondInterfaces != "" {
		dst.BondInterfaces = src.BondInterfaces
	}
	if src.BondMode != "" {
		dst.BondMode = src.BondMode
	}
	if src.VRFTableID > 0 {
		dst.VRFTableID = src.VRFTableID
	}
	nativeWins(&dst.OverlayVRFTableID, src.OverlayVRFTableID)
	if src.VRFName != "" {
		dst.VRFName = src.VRFName
	}
	if src.OverlayVRFSet && !dst.OverlayVRFSet {
		// Preserve an explicitly empty overlay VRF name: it means the overlay
		// should stay in the default namespace, not that the value is unset.
		dst.OverlayVRFName = src.OverlayVRFName
		dst.OverlayVRFSet = true
	}
	nativeWins(&dst.UnderlayDummyName, src.UnderlayDummyName)
	nativeWins(&dst.VXLANLink, src.VXLANLink)
	// Keys that this release adds: a set native value wins.
	nativeWins(&dst.VXLANName, src.VXLANName)
	nativeList("network.loopbackAddresses", &dst.LoopbackAddrs, src.LoopbackAddrs)
	nativeList("network.blackholeRoutes", &dst.BlackholeRoutes, src.BlackholeRoutes)
	nativeList("network.evpn.type5Networks", &dst.Type5Networks, src.Type5Networks)
	nativeWins(&dst.MTU, src.MTU)
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
	if len(src.InterfaceNames) > 0 {
		dst.InterfaceNames = src.InterfaceNames
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
	if len(src.BGPSessions) > 0 {
		dst.BGPSessions = src.BGPSessions
		dst.BGPMultipathRelax = src.BGPMultipathRelax
	}
	if src.BGPDefaults != "" {
		dst.BGPDefaults = src.BGPDefaults
	}
	if src.BGPFRRVersion != "" {
		dst.BGPFRRVersion = src.BGPFRRVersion
	}
	mergeBGPPolicy(dst, src)
	mergeOverlayDevices(&dst.OverlayDevices, &src.OverlayDevices)
	nativeList("network.noAcceptRA", &dst.NoAcceptRA, src.NoAcceptRA)
	if len(dst.EmitLLDP) == 0 {
		dst.EmitLLDPMatches = src.EmitLLDPMatches
	}
	nativeList("network.emitLLDP", &dst.EmitLLDP, src.EmitLLDP)
	// "no ip forwarding" in frr.conf is a default; an explicit option wins.
	if dst.IPForwarding == nil {
		dst.IPForwarding = src.IPForwarding
	}
	if dst.IPv6Forwarding == nil {
		dst.IPv6Forwarding = src.IPv6Forwarding
	}
}

// nativeWins keeps a set native value; an unset one takes the parsed value.
func nativeWins[T comparable](dst *T, parsed T) {
	var zero T
	if *dst == zero {
		*dst = parsed
	}
}

// nativeList keeps a non-empty native list as a whole and logs that it
// replaces the parsed list; an empty one takes the parsed list.
func nativeList[T any](name string, dst *[]T, parsed []T) {
	switch {
	case len(*dst) == 0:
		*dst = parsed
	case len(parsed) > 0:
		slog.Info(name+" replaces the netplan or frr.conf value", "native", len(*dst), "parsed", len(parsed))
	}
}

// mergeInferredEVPNMode applies the EVPN mode inferred from the FRR config.
// A provision VNI that the FRR config maps to a VRF is an L3VNI: FRR
// originates only Type-5 routes for it, so Type-5 only is the default
// unless provision_gateway is set (then the mode and its BUM FDB entry stay
// as before). An explicit EVPN_TYPE5_ONLY (true or false) or
// EVPN_L2_ENABLED=true wins. Otherwise advertise-all-vni enables L2 EVPN
// unless EVPN_TYPE5_ONLY forces Type-5 only. The L3VNI check uses the
// merged provision VNI because the VNI can come from /deploy/vars when the
// netplan files do not set it.
func mergeInferredEVPNMode(dst, src *network.Config) {
	if slices.Contains(src.EVPNL3VNIs, dst.ProvisionVNI) {
		if !dst.EVPNType5OnlySet && !dst.EVPNL2Enabled && dst.ProvisionGateway == "" {
			slog.Info("netplan: provision VNI is an FRR L3VNI, using Type-5 routes only", "vni", dst.ProvisionVNI)
			dst.EVPNType5Only = true
		}
		return
	}
	if src.EVPNL2Enabled && !dst.EVPNType5Only {
		dst.EVPNL2Enabled = true
	}
	// frr.conf "enable-resolve-overlay-index" or the native key turns it on.
	dst.EVPNResolveOverlayIndex = dst.EVPNResolveOverlayIndex || src.EVPNResolveOverlayIndex
}

// mergeBGPPolicy takes the frr.conf route-maps, community-lists and route
// targets. A non-empty native list replaces the frr.conf list as a whole.
// The frr.conf route targets win over the legacy vpn_rt key (as for every
// key that exists on main, and as in FRR); vpn_rt applies only without
// frr.conf and native route targets.
func mergeBGPPolicy(dst, src *network.Config) {
	switch {
	case len(dst.BGPRouteMaps) == 0:
		dst.BGPRouteMaps = src.BGPRouteMaps
	case len(src.BGPRouteMaps) > 0:
		slog.Info("network.bgp.routeMaps replaces the frr.conf route-maps", "native", len(dst.BGPRouteMaps), "frr.conf", len(src.BGPRouteMaps))
	}
	switch {
	case len(dst.BGPCommunityLists) == 0:
		dst.BGPCommunityLists = src.BGPCommunityLists
	case len(src.BGPCommunityLists) > 0:
		slog.Info("network.bgp.communityLists replaces the frr.conf community-lists", "native", len(dst.BGPCommunityLists), "frr.conf", len(src.BGPCommunityLists))
	}
	if len(dst.EVPNImportRTs) == 0 && len(dst.EVPNExportRTs) == 0 {
		dst.EVPNImportRTs, dst.EVPNExportRTs = src.EVPNImportRTs, src.EVPNExportRTs
	}
	dst.EVPNRD = cmp.Or(dst.EVPNRD, src.EVPNRD)
	dst.EVPNRDIndex, dst.BGPInstances = src.EVPNRDIndex, src.BGPInstances
}

// mergeOverlayDevices takes every device setting that netplan sets and the
// native options do not set (native wins).
func mergeOverlayDevices(dst, src *network.OverlayDevices) {
	nativeWins(&dst.VXLANMTU, src.VXLANMTU)
	nativeWins(&dst.BridgeMTU, src.BridgeMTU)
	nativeWins(&dst.VXLANMAC, src.VXLANMAC)
	for _, f := range []struct{ d, s **bool }{
		{&dst.VXLANLearning, &src.VXLANLearning}, {&dst.VXLANARPProxy, &src.VXLANARPProxy},
		{&dst.PortHairpin, &src.PortHairpin}, {&dst.PortLearning, &src.PortLearning},
		{&dst.VXLANLinkLocal, &src.VXLANLinkLocal}, {&dst.BridgeLinkLocal, &src.BridgeLinkLocal},
		{&dst.VXLANAcceptRA, &src.VXLANAcceptRA}, {&dst.BridgeAcceptRA, &src.BridgeAcceptRA},
		{&dst.BridgeSTP, &src.BridgeSTP},
	} {
		nativeWins(f.d, *f.s)
	}
}

// splitList splits a comma-separated vars value and drops empty entries.
func splitList(value string) []string {
	var out []string
	for _, f := range strings.Split(value, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
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
	setBGPHostname(netCfg.BGPHostname, syscall.Sethostname)
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
// target root (busy block device mounts are remounted read-only), so the
// installed OS does not start with filesystems that need journal recovery.
//
// stopNetwork runs after the load, right before the kexec: a failed load
// keeps the network for the reboot fallback and its error report.
func tryKexec(cfg *config.MachineConfig, firmwareChanged bool, releaseTarget func() (disk.ReleaseReport, error), stopNetwork func()) bool {
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

	// The entry that GRUB would boot: default, grubenv, submenus. When only
	// GRUB can tell (BLS, conditional kernel lines, variables set at boot),
	// reboot through the firmware instead of guessing.
	entry, err := kexec.DefaultEntry(kexecRoot)
	if err != nil {
		slog.Warn("no kexec boot entry, rebooting instead", "error", err)
		return false
	}
	slog.Info("kexec boot entry", "entry", entry.Name)

	kernel := resolveKexecPath(kexecRoot, entry.Kernel)
	initrd := resolveKexecPath(kexecRoot, entry.Initramfs)
	slog.Info("attempting kexec", "kernel", kernel, "initrd", initrd)

	if err := kexecLoad(kernel, initrd, entry.KernelArgs); err != nil {
		slog.Warn("kexec load failed, falling back to reboot", "error", err)
		return false
	}
	releaseKexecTarget(cfg, releaseTarget)
	if err := doKexecExecute(stopNetwork); err != nil {
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
	if errors.Is(err, disk.ErrMountsUnknown) {
		slog.Warn("target filesystems unknown at kexec, the installed OS may replay their journal", "root", installedRootPath, "error", err)
		return
	}
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
	return provision.RequiresABKexec(cfg)
}

// inputStrictness returns NETPLAN_STRICT and FRR_CONFIG_STRICT; unset
// means strict.
func inputStrictness(cfg *config.MachineConfig) (netplanStrict, frrStrict bool) {
	strict := func(v *bool) bool { return v == nil || *v }
	return strict(cfg.Network.NetplanStrict), strict(cfg.Network.FRRStrict)
}

// applyNativeSessions resolves network.bgp.sessions with the merged ASN.
// The native list replaces the frr.conf sessions as a whole.
func applyNativeSessions(bgp *config.BGPConfig, netCfg *network.Config) error {
	if v := bgp.FastExternalFailover; v != nil {
		netCfg.BGPFastExternalFailover = v
		for i := range netCfg.BGPSessions {
			netCfg.BGPSessions[i].FastExternalFailover = *v
		}
	}
	if !bgp.HasSessions() {
		return applyNativeBFDProfiles(bgp, netCfg)
	}
	native := *bgp
	native.ASN = netCfg.ASN
	sessions, err := native.ResolveSessions()
	if err != nil {
		return fmt.Errorf("native BGP sessions: %w", err)
	}
	if len(netCfg.BGPSessions) > 0 {
		slog.Info("network.bgp.sessions replaces the frr.conf sessions", "native", len(sessions), "frr.conf", len(netCfg.BGPSessions))
	}
	nativeSessions := netplan.BGPSessions(sessions)
	if bgp.FastExternalFailover == nil {
		keepFastExternalFailover(nativeSessions, netCfg.BGPSessions)
	}
	netCfg.BGPSessions = nativeSessions
	return nil
}

// keepFastExternalFailover keeps a "no bgp fast-external-failover" of
// frr.conf when native sessions replace the frr.conf sessions without
// their own setting: it is a router flag, so it applies to the native
// sessions of the same VRF.
func keepFastExternalFailover(native, parsed []network.BGPSession) {
	off := map[string]bool{}
	for i := range parsed {
		if !parsed[i].FastExternalFailover {
			off[parsed[i].VRF] = true
		}
	}
	for i := range native {
		if off[native[i].VRF] {
			native[i].FastExternalFailover = false
		}
	}
}

// setPeerVars returns the vars keys of the scalar peer setup that are set,
// before the netplan merge changes them.
// applyNativeBFDProfiles gives the frr.conf BFD sessions the timers of the
// native profiles, which replace the frr.conf profiles as a whole.
func applyNativeBFDProfiles(bgp *config.BGPConfig, netCfg *network.Config) error {
	if len(bgp.BFDProfiles) == 0 {
		return nil
	}
	model, err := bgp.BFDModel()
	if err != nil {
		return fmt.Errorf("native BFD profiles: %w", err)
	}
	slog.Info("network.bgp.bfdProfiles replaces the frr.conf BFD profiles", "native", len(bgp.BFDProfiles))
	for i := range netCfg.BGPSessions {
		if s := &netCfg.BGPSessions[i]; s.BFD {
			s.BFDTransmitMS, s.BFDReceiveMS, s.BFDDetectMult = model.BFDTimers(s.BFDProfile)
		}
	}
	return nil
}

func setPeerVars(netCfg *network.Config) []string {
	var set []string
	for _, v := range []struct {
		key string
		on  bool
	}{
		{"BGP_PEER_MODE", netCfg.BGPPeerMode != network.PeerModeUnnumbered},
		{"BGP_INTERFACES", netCfg.BGPInterfaces != ""},
		{"BGP_NEIGHBORS", netCfg.BGPNeighbors != ""},
		{"BGP_REMOTE_ASN", netCfg.BGPRemoteASN != 0},
		{"BGP_KEEPALIVE", netCfg.BGPKeepalive != 0},
		{"BGP_HOLD", netCfg.BGPHold != 0},
	} {
		if v.on {
			set = append(set, v.key)
		}
	}
	return set
}

// warnIgnoredPeerVars logs each scalar peer vars key that the per-session
// setup (frr.conf or network.bgp.sessions) makes ineffective.
func warnIgnoredPeerVars(netCfg *network.Config, keys []string) {
	if len(netCfg.BGPSessions) == 0 {
		return
	}
	for _, k := range keys {
		slog.Warn("vars key ignored: the BGP sessions come from frr.conf or network.bgp.sessions", "key", k)
	}
}
