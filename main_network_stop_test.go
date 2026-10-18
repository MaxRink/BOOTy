//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/runmode"
)

type teardownMode struct {
	network.Mode
	calls atomic.Int32
	block chan struct{}
}

func (m *teardownMode) Teardown(context.Context) error {
	m.calls.Add(1)
	if m.block != nil {
		<-m.block
	}
	return nil
}

func TestStopNetworkOnceTearsDownOnce(t *testing.T) {
	mode := &teardownMode{}
	stop := stopNetworkOnce(context.Background(), mode)
	stop()
	stop()
	if got := mode.calls.Load(); got != 1 {
		t.Fatalf("Teardown calls = %d, want 1", got)
	}
	stopNetworkOnce(context.Background(), nil)() // no network mode: no panic
}

// A hung teardown must not stop the handoff.
func TestStopNetworkOnceBoundsTheWait(t *testing.T) {
	defer func(d time.Duration) { networkStopTimeout = d }(networkStopTimeout)
	networkStopTimeout = 50 * time.Millisecond
	mode := &teardownMode{block: make(chan struct{})}
	defer close(mode.block)

	start := time.Now()
	stopNetworkOnce(context.Background(), mode)()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stop waited %v, want about %v", d, networkStopTimeout)
	}
}

// recordOrder replaces flushLogs and kexecExecute and records the calls.
func recordOrder(t *testing.T, order *[]string) {
	t.Helper()
	prevFlush, prevExec := flushLogs, kexecExecute
	t.Cleanup(func() { flushLogs, kexecExecute = prevFlush, prevExec })
	flushLogs = func() { *order = append(*order, "flush") }
	kexecExecute = func() error { *order = append(*order, "kexec"); return errors.New("test: no kexec") }
}

// The log lines are shipped while the network is up, then BGP and BFD
// stop, then the kexec runs.
func TestDoKexecExecuteFlushesThenStopsNetwork(t *testing.T) {
	var order []string
	recordOrder(t, &order)
	if err := doKexecExecute(func() { order = append(order, "stop") }); err == nil {
		t.Fatal("kexec error lost")
	}
	if strings.Join(order, ",") != "flush,stop,kexec" {
		t.Fatalf("order = %v, want flush,stop,kexec", order)
	}
}

// The wiring of tryKexec: after the load, the network stops between the log
// flush and the kexec.
func TestTryKexecStopsNetworkBeforeExecute(t *testing.T) {
	var order []string
	recordOrder(t, &order)
	root := t.TempDir()
	for _, f := range []string{"boot/vmlinuz", "boot/initrd.img"} {
		mustWrite(t, filepath.Join(root, f))
	}
	if err := os.MkdirAll(filepath.Join(root, "boot", "grub"), 0o755); err != nil {
		t.Fatal(err)
	}
	grub := "menuentry 'os' {\n linux /boot/vmlinuz root=/dev/sda2\n initrd /boot/initrd.img\n}\n"
	if err := os.WriteFile(filepath.Join(root, "boot", "grub", "grub.cfg"), []byte(grub), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot, prevLoad := kexecRoot, kexecLoad
	t.Cleanup(func() { kexecRoot, kexecLoad = prevRoot, prevLoad })
	kexecRoot = root
	kexecLoad = func(string, string, string) error { order = append(order, "load"); return nil }
	keep := false
	cfg := &config.MachineConfig{}
	cfg.Provision.KexecUnmountTarget = &keep

	if tryKexec(cfg, false, nil, func() { order = append(order, "stop") }) {
		t.Fatal("tryKexec reported a kexec although execute failed")
	}
	if strings.Join(order, ",") != "load,flush,stop,kexec" {
		t.Fatalf("order = %v, want load,flush,stop,kexec", order)
	}
}

// Reboot and power off both ship the log lines and stop the network first,
// like the legacy `shutdown now`; a failed handoff brings the network back.
func TestWithNetworkStopBeforeHandoff(t *testing.T) {
	var order []string
	recordOrder(t, &order)
	ops := provisionHandoffOps{
		reboot:   func() error { order = append(order, "reboot"); return errors.New("x") },
		powerOff: func() error { order = append(order, "poweroff"); return nil },
	}
	ops = withNetworkStopBeforeHandoff(ops, func() { order = append(order, "stop") },
		func() func() {
			order = append(order, "restore")
			return func() { order = append(order, "stop-restored") }
		})
	if err := ops.powerOff(); err != nil {
		t.Fatal(err)
	}
	if err := ops.reboot(); err == nil {
		t.Fatal("reboot error lost")
	}
	want := "flush,stop,poweroff,flush,stop,reboot,restore"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	// After the restore, the next handoff and the exit stop the restored
	// network: the first stop already ran.
	order = nil
	_ = ops.reboot()
	ops.stopNetwork()
	want = "flush,stop-restored,reboot,restore,stop-restored"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

// recordEndRun replaces the reboot, the pause and the handoff ops of endRun.
func recordEndRun(t *testing.T, order *[]string, powerOffErr error) {
	t.Helper()
	recordOrder(t, order)
	prevReboot, prevPause, prevOps, prevRestore := rebootNow, endRunPause, provisionHandoff, restoreNetwork
	t.Cleanup(func() {
		rebootNow, endRunPause, provisionHandoff, restoreNetwork = prevReboot, prevPause, prevOps, prevRestore
	})
	rebootNow = func() { *order = append(*order, "reboot") }
	endRunPause = 0
	restoreNetwork = func(context.Context, *config.MachineConfig) func() {
		*order = append(*order, "restore")
		return func() { *order = append(*order, "stop-restored") }
	}
	provisionHandoff = provisionHandoffOps{
		reboot:   func() error { *order = append(*order, "reboot"); return nil },
		powerOff: func() error { *order = append(*order, "poweroff"); return powerOffErr },
		shell:    func() { *order = append(*order, "shell") },
		exit:     func(int) {},
	}
}

type errReporter struct{ order *[]string }

func (r errReporter) ReportStatus(_ context.Context, status config.Status, _ string) error {
	*r.order = append(*r.order, "report-"+string(status))
	return nil
}

// The wiring of endRun: every way out of the ramdisk stops the network
// after the log flush and before the handoff.
func TestEndRunStopsNetworkBeforeTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		modeErr     error
		powerOffErr error
		want        string
	}{
		{"reboot requested", &runmode.RebootRequestedError{}, nil, "flush,stop,reboot"},
		{"failed mode", errors.New("mode failed"), nil, "flush,stop,reboot"},
		{"no error", nil, nil, "flush,stop,reboot"},
		{"power off", &runmode.PowerOffRequestedError{}, nil, "flush,stop,poweroff"},
		{"failed power off", &runmode.PowerOffRequestedError{}, errors.New("x"), "flush,stop,poweroff,restore,report-error,shell,flush,stop-restored"},
		{"rescue shell keeps the network", &runmode.RescueShellError{}, nil, "reboot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			recordEndRun(t, &order, tc.powerOffErr)
			prevShell := shellHook
			t.Cleanup(func() { shellHook = prevShell })
			shellHook = func() {}
			endRun(context.Background(), &config.MachineConfig{}, errReporter{&order}, nil,
				func() { order = append(order, "stop") }, "test", tc.modeErr)
			if got := strings.Join(order, ","); got != tc.want {
				t.Fatalf("order = %v, want %s", got, tc.want)
			}
		})
	}
}

// The wiring of runCAPRF: a provision that reboots stops the network once,
// after the log flush and before the reboot.
func TestFinishProvisionStopsNetworkBeforeReboot(t *testing.T) {
	var order []string
	recordOrder(t, &order)
	prevOps := provisionHandoff
	t.Cleanup(func() { provisionHandoff = prevOps })
	provisionHandoff = provisionHandoffOps{
		reboot:   func() error { order = append(order, "reboot"); return nil },
		powerOff: func() error { order = append(order, "poweroff"); return nil },
		shell:    func() {},
		exit:     func(int) {},
	}
	mode := &orderMode{order: &order}
	cfg := &config.MachineConfig{}
	cfg.Provision.DisableKexec = true
	finishProvision(context.Background(), cfg, nil, nil, stopNetworkOnce(context.Background(), mode), &runmode.ProvisionCompleteError{})
	if strings.Join(order, ",") != "flush,teardown,reboot" {
		t.Fatalf("order = %v, want flush,teardown,reboot", order)
	}
}

type orderMode struct {
	network.Mode
	order *[]string
}

func (m *orderMode) Teardown(context.Context) error {
	*m.order = append(*m.order, "teardown")
	return nil
}

// recordStop replaces the reboot and the log flush and records them.
func recordStop(t *testing.T, order *[]string) func() {
	t.Helper()
	recordOrder(t, order)
	prev := rebootNow
	t.Cleanup(func() { rebootNow = prev })
	rebootNow = func() { *order = append(*order, "reboot") }
	return func() { *order = append(*order, "stop") }
}

// The ends of a run outside a mode stop the network before the reboot:
// a failed network or token setup, a token failure, and the grace reboot of
// the token fatal handler.
func TestRunEndsOutsideAModeStopTheNetwork(t *testing.T) {
	const want = "flush,stop,reboot"
	t.Run("failed setup", func(t *testing.T) {
		var order []string
		stop := recordStop(t, &order)
		failNetworkSetup(context.Background(), &config.MachineConfig{}, nil, errors.New("token"), stop)
		if got := strings.Join(order, ","); got != want {
			t.Fatalf("order = %s, want %s", got, want)
		}
	})
	t.Run("token stop", func(t *testing.T) {
		var order []string
		stop := recordStop(t, &order)
		endTokenStoppedRun(context.Background(), &config.MachineConfig{}, nil, stop)
		if got := strings.Join(order, ","); got != want {
			t.Fatalf("order = %s, want %s", got, want)
		}
	})
	t.Run("token grace reboot", func(t *testing.T) {
		var order []string
		var mu sync.Mutex
		prevFlush, prevReboot, prevGrace := flushLogs, rebootNow, tokenShutdownGrace
		t.Cleanup(func() {
			flushLogs, rebootNow, tokenShutdownGrace = prevFlush, prevReboot, prevGrace
			tokenTaskDone.Store(false)
		})
		tokenTaskDone.Store(false)
		tokenShutdownGrace = time.Millisecond
		rebooted := make(chan struct{})
		add := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
		flushLogs = func() { add("flush") }
		rebootNow = func() { add("reboot"); close(rebooted) }
		tokenFatalHandler(&config.MachineConfig{}, func() { add("stop") }, func() {})(errors.New("expired"))
		select {
		case <-rebooted:
		case <-time.After(5 * time.Second):
			t.Fatal("no grace reboot")
		}
		mu.Lock()
		defer mu.Unlock()
		if got := strings.Join(order, ","); got != want {
			t.Fatalf("order = %s, want %s", got, want)
		}
	})
}

// connectivityMode fails the connectivity check and records the teardown.
type connectivityMode struct {
	network.Mode
	teardowns atomic.Int32
	waits     atomic.Int32
}

func (m *connectivityMode) WaitForConnectivity(context.Context, string, time.Duration) error {
	m.waits.Add(1)
	return errors.New("no route")
}

func (m *connectivityMode) Teardown(context.Context) error {
	m.teardowns.Add(1)
	return nil
}

// A connectivity failure returns the stop of the network that came up, so
// the failed run still sends the BGP Cease.
func TestSetupNetworkConnectivityFailureReturnsTheStop(t *testing.T) {
	mode := &connectivityMode{}
	prev := setupNetworkModeFn
	t.Cleanup(func() { setupNetworkModeFn = prev })
	setupNetworkModeFn = func(context.Context, *config.MachineConfig) (network.Mode, error) { return mode, nil }
	cfg := &config.MachineConfig{}
	cfg.Transport.InitURL = "https://caprf.example.invalid/status/init"
	cfg.Network.WaitMode = config.NetworkWaitRounds
	zero := 0
	cfg.Network.WaitMaxAttempts = &zero

	_, stop, err := setupNetworkAndTokenFlow(context.Background(), cfg, nil, func() {})
	if err == nil {
		t.Fatal("connectivity failure lost")
	}
	stop()
	if got := mode.teardowns.Load(); got != 1 {
		t.Fatalf("teardowns = %d, want 1 from the returned stop", got)
	}
}

// The restore waits for a teardown that still runs and skips while it
// does; afterwards it sets the network up once with one connectivity
// check.
func TestRestoreNetworkWaitsForTheTeardown(t *testing.T) {
	prevSetup, prevStop, prevRestore := setupNetworkModeFn, networkStopTimeout, restoreTimeout
	t.Cleanup(func() {
		setupNetworkModeFn, networkStopTimeout, restoreTimeout = prevSetup, prevStop, prevRestore
		teardownDone.Store(nil)
	})
	networkStopTimeout, restoreTimeout = 20*time.Millisecond, time.Second
	restored := &connectivityMode{}
	setups := 0
	setupNetworkModeFn = func(context.Context, *config.MachineConfig) (network.Mode, error) { setups++; return restored, nil }
	cfg := &config.MachineConfig{}
	cfg.Transport.ErrorURL = "https://caprf.example.invalid/status/error"

	hung := &teardownMode{block: make(chan struct{})}
	stopNetworkOnce(context.Background(), hung)() // times out, teardown still runs
	restoreNetwork(context.Background(), cfg)
	if setups != 0 {
		t.Fatal("restored while the teardown still runs")
	}
	close(hung.block)
	stop := restoreNetwork(context.Background(), cfg)
	if setups != 1 || restored.waits.Load() != 1 {
		t.Fatalf("setups %d, connectivity checks %d; want one each", setups, restored.waits.Load())
	}
	// The restored network is kept, and its stop tears it down.
	if restored.teardowns.Load() != 0 {
		t.Fatal("restored network torn down at once")
	}
	stop()
	if got := restored.teardowns.Load(); got != 1 {
		t.Fatalf("teardowns of the restored network = %d, want 1 from the returned stop", got)
	}
}

// The restored network is set up with the run context: the context that
// Setup got stays live after restoreNetwork returns, so goroutines of the
// mode (the GoBGP EVPN route watcher) keep running.
func TestRestoreNetworkSetupContextOutlivesTheRestore(t *testing.T) {
	prevSetup, prevRestore := setupNetworkModeFn, restoreTimeout
	t.Cleanup(func() {
		setupNetworkModeFn, restoreTimeout = prevSetup, prevRestore
		teardownDone.Store(nil)
	})
	teardownDone.Store(nil)
	restoreTimeout = 50 * time.Millisecond
	var setupCtx context.Context
	setupNetworkModeFn = func(ctx context.Context, _ *config.MachineConfig) (network.Mode, error) {
		setupCtx = ctx
		return &connectivityMode{}, nil
	}
	cfg := &config.MachineConfig{}
	cfg.Transport.ErrorURL = "https://caprf.example.invalid/status/error"

	restoreNetwork(context.Background(), cfg)
	if setupCtx == nil || setupCtx.Err() != nil {
		t.Fatalf("setup context after the restore: %v, want live", setupCtx)
	}
}

// A setup that does not finish within restoreTimeout is given up, not
// canceled, and torn down when it finishes.
func TestRestoreNetworkGivesUpASlowSetup(t *testing.T) {
	prevSetup, prevRestore := setupNetworkModeFn, restoreTimeout
	t.Cleanup(func() {
		setupNetworkModeFn, restoreTimeout = prevSetup, prevRestore
		teardownDone.Store(nil)
	})
	teardownDone.Store(nil)
	restoreTimeout = 20 * time.Millisecond
	release := make(chan struct{})
	late := &connectivityMode{}
	setupNetworkModeFn = func(context.Context, *config.MachineConfig) (network.Mode, error) {
		<-release
		return late, nil
	}

	restoreNetwork(context.Background(), &config.MachineConfig{})()
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for late.teardowns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := late.teardowns.Load(); got != 1 {
		t.Fatalf("teardowns of the late network = %d, want 1", got)
	}
}
