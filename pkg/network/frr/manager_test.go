//go:build linux

package frr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// mockCommander records calls and returns preset results.
type mockFRRCommander struct {
	calls   []string
	results map[string]mockFRRResult
}

type mockFRRResult struct {
	output []byte
	err    error
}

func newMockFRRCommander() *mockFRRCommander {
	return &mockFRRCommander{results: make(map[string]mockFRRResult)}
}

func (m *mockFRRCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name
	if len(args) > 0 {
		key = name + " " + args[0]
	}
	m.calls = append(m.calls, key)
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}
	return nil, nil
}

func (m *mockFRRCommander) setResult(key string, output []byte, err error) {
	m.results[key] = mockFRRResult{output: output, err: err}
}

func TestNewManager(t *testing.T) {
	mgr := NewManager(nil)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestNewManagerCustomCommander(t *testing.T) {
	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)
	if mgr.commander != cmd {
		t.Fatal("expected custom commander")
	}
}

func TestTeardown(t *testing.T) {
	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)
	mgr.frrStartMethod = frrStartSystemctl
	commands, _ := installDaemonHooks(t, nil, nil)

	if err := mgr.Teardown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := commandBases(*commands); strings.Join(got, ",") != "systemctl" {
		t.Fatalf("commands = %v, want systemctl", got)
	}
}

func TestWaitForConnectivitySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)

	if err := mgr.WaitForConnectivity(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForConnectivityTimeout(t *testing.T) {
	// Server that always fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Close() // close immediately so connections fail

	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)

	err := mgr.WaitForConnectivity(context.Background(), srv.URL, 2*time.Second)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForConnectivityEmptyTarget(t *testing.T) {
	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)

	err := mgr.WaitForConnectivity(context.Background(), "", 1*time.Second)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestWaitForConnectivityContextCancel(t *testing.T) {
	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := mgr.WaitForConnectivity(ctx, "http://192.0.2.1:9999/unreachable", 30*time.Second)
	if err == nil {
		t.Fatal("expected context cancel error")
	}
}

func TestAddBGPPeer(t *testing.T) {
	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)

	err := mgr.addBGPPeer(context.Background(), "Vrf_underlay", 65000, "eth0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(cmd.calls))
	}
}

func TestAddBGPPeerError(t *testing.T) {
	cmd := newMockFRRCommander()
	cmd.setResult("vtysh -c", nil, fmt.Errorf("vtysh failed"))
	mgr := NewManager(cmd)

	err := mgr.addBGPPeer(context.Background(), "Vrf_underlay", 65000, "eth0")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEnsureFRRDirsCreatesRuntimeDirsWithFRROwnership(t *testing.T) {
	base := t.TempDir()
	dirs := []string{
		filepath.Join(base, "run", "frr"),
		filepath.Join(base, "var", "run", "frr"),
		filepath.Join(base, "var", "tmp", "frr"),
		filepath.Join(base, "var", "lib", "frr"),
	}

	origDirs := frrRuntimeDirs
	origLookup := lookupFRRUser
	origChown := chownFRRDir
	frrRuntimeDirs = dirs
	lookupFRRUser = func() frrDirOwner {
		return frrDirOwner{uid: 101, gid: 104, ok: true}
	}

	chowned := make(map[string]bool, len(dirs))
	chownFRRDir = func(path string, uid, gid int) error {
		if uid != 101 || gid != 104 {
			t.Fatalf("chown(%q) owner = %d:%d, want 101:104", path, uid, gid)
		}
		chowned[path] = true
		return nil
	}
	t.Cleanup(func() {
		frrRuntimeDirs = origDirs
		lookupFRRUser = origLookup
		chownFRRDir = origChown
	})

	ensureFRRDirs()

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected FRR dir %q: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("expected FRR path %q to be a directory", dir)
		}
		if !chowned[dir] {
			t.Fatalf("expected FRR dir %q to be chowned", dir)
		}
	}
}

func TestEnsureFRRDirsSkipsOwnershipWhenFRRUserMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "frr")

	origDirs := frrRuntimeDirs
	origLookup := lookupFRRUser
	origChown := chownFRRDir
	frrRuntimeDirs = []string{dir}
	lookupFRRUser = func() frrDirOwner { return frrDirOwner{} }
	chownFRRDir = func(path string, uid, gid int) error {
		t.Fatalf("unexpected chown(%q, %d, %d)", path, uid, gid)
		return nil
	}
	t.Cleanup(func() {
		frrRuntimeDirs = origDirs
		lookupFRRUser = origLookup
		chownFRRDir = origChown
	})

	ensureFRRDirs()

	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("expected FRR dir %q: %v", dir, err)
	} else if !info.IsDir() {
		t.Fatalf("expected FRR path %q to be a directory", dir)
	}
}

func TestStartDaemonsDirectFailsWhenRequiredDaemonsMissing(t *testing.T) {
	origDirs := frrDaemonDirs
	frrDaemonDirs = []string{t.TempDir(), t.TempDir()}
	t.Cleanup(func() { frrDaemonDirs = origDirs })

	mgr := NewManager(nil)
	err := mgr.startDaemonsDirect(context.Background())
	if err == nil {
		t.Fatal("expected error for missing required FRR daemons")
	}
	if !strings.Contains(err.Error(), "required FRR daemons not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveFRRDaemonsSupportsInitramfsSbinLayout(t *testing.T) {
	sbin := t.TempDir()
	usrLib := t.TempDir()
	origDirs := frrDaemonDirs
	frrDaemonDirs = []string{usrLib, sbin}
	t.Cleanup(func() { frrDaemonDirs = origDirs })

	for _, name := range []string{"zebra", "bgpd", "bfdd"} {
		path := filepath.Join(sbin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write fake daemon %s: %v", name, err)
		}
	}

	paths, missing, err := resolveFRRDaemons([]frrDaemonSpec{
		{"mgmtd", nil, false},
		{"zebra", nil, true},
		{"bgpd", nil, true},
		{"bfdd", nil, true},
	})
	if err != nil {
		t.Fatalf("resolve daemons: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("unexpected missing daemons: %v", missing)
	}
	for _, name := range []string{"zebra", "bgpd", "bfdd"} {
		if got, want := paths[name], filepath.Join(sbin, name); got != want {
			t.Fatalf("%s path = %q, want %q", name, got, want)
		}
	}
}

func TestResolveFRRDaemonsSurfacesStatErrors(t *testing.T) {
	badDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write bad daemon dir: %v", err)
	}

	origDirs := frrDaemonDirs
	frrDaemonDirs = []string{badDir}
	t.Cleanup(func() { frrDaemonDirs = origDirs })

	_, _, err := resolveFRRDaemons([]frrDaemonSpec{
		{"zebra", nil, true},
	})
	if err == nil {
		t.Fatal("expected stat error")
	}
	if !strings.Contains(err.Error(), "stat FRR daemon") {
		t.Fatalf("error = %q, want stat FRR daemon context", err.Error())
	}
	if strings.Contains(err.Error(), "required FRR daemons not found") {
		t.Fatalf("error flattened stat failure into missing daemon: %v", err)
	}
}

func TestStartDaemonsDirectStopsStartedDaemonsOnRequiredFailure(t *testing.T) {
	setupFakeDaemonDir(t, []string{"mgmtd", "zebra", "staticd", "bgpd", "bfdd"})
	commands, signals := installDaemonHooks(t,
		func(name string) error {
			if filepath.Base(name) == "bgpd" {
				return fmt.Errorf("bgpd failed")
			}
			return nil
		},
		map[string][]int{"mgmtd": {101}, "zebra": {102}, "staticd": {103}},
	)

	mgr := NewManager(nil)
	err := mgr.startDaemonsDirect(context.Background())
	if err == nil {
		t.Fatal("expected bgpd start failure")
	}
	if !strings.Contains(err.Error(), "start FRR daemon bgpd") {
		t.Fatalf("error = %q, want bgpd context", err.Error())
	}

	wantCommands := []string{"mgmtd", "zebra", "staticd", "bgpd"}
	if got := commandBases(*commands); strings.Join(got, ",") != strings.Join(wantCommands, ",") {
		t.Fatalf("commands = %v, want %v", got, wantCommands)
	}
	wantSignals := []string{"staticd:103:terminated", "zebra:102:terminated", "mgmtd:101:terminated"}
	if strings.Join(*signals, ",") != strings.Join(wantSignals, ",") {
		t.Fatalf("signals = %v, want %v", *signals, wantSignals)
	}
	if len(mgr.directDaemonList) != 0 {
		t.Fatalf("directDaemonList = %v, want cleared after rollback", mgr.directDaemonList)
	}
}

func TestTeardownStopsTrackedDirectDaemons(t *testing.T) {
	_, signals := installDaemonHooks(t, nil, map[string][]int{
		"zebra": {201},
		"bgpd":  {202},
		"bfdd":  {203},
	})
	mgr := NewManager(newMockFRRCommander())
	mgr.frrStartMethod = frrStartDirect
	mgr.directDaemonList = []string{"zebra", "bgpd", "bfdd"}

	if err := mgr.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	wantSignals := []string{"bfdd:203:terminated", "bgpd:202:terminated", "zebra:201:terminated"}
	if strings.Join(*signals, ",") != strings.Join(wantSignals, ",") {
		t.Fatalf("signals = %v, want %v", *signals, wantSignals)
	}
	if mgr.frrStartMethod != "" {
		t.Fatalf("frrStartMethod = %q, want cleared", mgr.frrStartMethod)
	}
	if len(mgr.directDaemonList) != 0 {
		t.Fatalf("directDaemonList = %v, want cleared", mgr.directDaemonList)
	}
}

func TestRestartFRRRestartsTrackedDirectDaemons(t *testing.T) {
	setupFakeDaemonDir(t, []string{"zebra", "bgpd", "bfdd"})
	commands, signals := installDaemonHooks(t, nil, map[string][]int{
		"zebra": {301},
		"bgpd":  {302},
		"bfdd":  {303},
	})
	mgr := NewManager(nil)
	mgr.frrStartMethod = frrStartDirect
	mgr.directDaemonList = []string{"zebra", "bgpd", "bfdd"}

	if err := mgr.restartFRR(context.Background()); err != nil {
		t.Fatalf("restartFRR: %v", err)
	}

	wantSignals := []string{"bfdd:303:terminated", "bgpd:302:terminated", "zebra:301:terminated"}
	if strings.Join(*signals, ",") != strings.Join(wantSignals, ",") {
		t.Fatalf("signals = %v, want %v", *signals, wantSignals)
	}
	wantCommands := []string{"zebra", "bgpd", "bfdd"}
	if got := commandBases(*commands); strings.Join(got, ",") != strings.Join(wantCommands, ",") {
		t.Fatalf("commands = %v, want %v", got, wantCommands)
	}
	if mgr.frrStartMethod != frrStartDirect {
		t.Fatalf("frrStartMethod = %q, want direct", mgr.frrStartMethod)
	}
	if got := strings.Join(mgr.directDaemonList, ","); got != "zebra,bgpd,bfdd" {
		t.Fatalf("directDaemonList = %v, want restarted daemons", mgr.directDaemonList)
	}
}

func TestStartStopRestartFRRUsesInitScript(t *testing.T) {
	initPath := filepath.Join(t.TempDir(), "frrinit.sh")
	if err := os.WriteFile(initPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake frrinit.sh: %v", err)
	}
	origInitPath := frrInitScriptPath
	frrInitScriptPath = initPath
	t.Cleanup(func() { frrInitScriptPath = origInitPath })

	commands, _ := installDaemonHooks(t, func(name string) error {
		if filepath.Base(name) == "systemctl" {
			return fmt.Errorf("systemctl unavailable")
		}
		return nil
	}, nil)
	mgr := NewManager(nil)

	if err := mgr.startFRR(context.Background()); err != nil {
		t.Fatalf("startFRR: %v", err)
	}
	if mgr.frrStartMethod != frrStartInit {
		t.Fatalf("frrStartMethod = %q, want init script", mgr.frrStartMethod)
	}
	if err := mgr.stopFRR(context.Background()); err != nil {
		t.Fatalf("stopFRR: %v", err)
	}
	if err := mgr.restartFRR(context.Background()); err != nil {
		t.Fatalf("restartFRR: %v", err)
	}

	wantCommands := []string{"systemctl", "frrinit.sh", "frrinit.sh", "frrinit.sh"}
	if got := commandBases(*commands); strings.Join(got, ",") != strings.Join(wantCommands, ",") {
		t.Fatalf("commands = %v, want %v", got, wantCommands)
	}
}

func TestStopFRRBestEffortWhenStartMethodUnknown(t *testing.T) {
	origInitPath := frrInitScriptPath
	frrInitScriptPath = filepath.Join(t.TempDir(), "missing-frrinit.sh")
	t.Cleanup(func() { frrInitScriptPath = origInitPath })

	commands, _ := installDaemonHooks(t, func(string) error {
		return fmt.Errorf("command unavailable")
	}, nil)
	mgr := NewManager(nil)

	if err := mgr.stopFRR(context.Background()); err != nil {
		t.Fatalf("stopFRR: %v", err)
	}
	if got := commandBases(*commands); strings.Join(got, ",") != "systemctl" {
		t.Fatalf("commands = %v, want best-effort systemctl stop", got)
	}
}

func TestStopDaemonsDirectEscalatesToSIGKILL(t *testing.T) {
	origFind := findFRRDaemonPIDs
	origSignal := signalFRRProcess
	origStopWait := frrDaemonStopWait
	origKillWait := frrDaemonKillWait
	active := true
	signals := []string{}
	findFRRDaemonPIDs = func(names []string) (map[string][]int, error) {
		pids := make(map[string][]int, len(names))
		if active {
			pids["zebra"] = []int{401}
		}
		return pids, nil
	}
	signalFRRProcess = func(pid int, sig syscall.Signal) error {
		signals = append(signals, fmt.Sprintf("zebra:%d:%s", pid, sig))
		if sig == syscall.SIGKILL {
			active = false
		}
		return nil
	}
	frrDaemonStopWait = 0
	frrDaemonKillWait = 0
	t.Cleanup(func() {
		findFRRDaemonPIDs = origFind
		signalFRRProcess = origSignal
		frrDaemonStopWait = origStopWait
		frrDaemonKillWait = origKillWait
	})

	mgr := NewManager(nil)
	mgr.frrStartMethod = frrStartDirect
	mgr.directDaemonList = []string{"zebra"}

	if err := mgr.stopDaemonsDirect(context.Background()); err != nil {
		t.Fatalf("stopDaemonsDirect: %v", err)
	}
	wantSignals := []string{"zebra:401:terminated", "zebra:401:killed"}
	if strings.Join(signals, ",") != strings.Join(wantSignals, ",") {
		t.Fatalf("signals = %v, want %v", signals, wantSignals)
	}
}

func TestRollbackSetupSkipsFRRStateDumpBeforeStart(t *testing.T) {
	cmd := newMockFRRCommander()
	mgr := NewManager(cmd)

	err := mgr.rollbackSetup(context.Background(), fmt.Errorf("setup failed"))
	if err == nil || !strings.Contains(err.Error(), "setup failed") {
		t.Fatalf("rollbackSetup error = %v, want original setup failure", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("commands = %v, want no vtysh dump before FRR start", cmd.calls)
	}
}

func setupFakeDaemonDir(t *testing.T, names []string) {
	t.Helper()
	dir := t.TempDir()
	origDirs := frrDaemonDirs
	frrDaemonDirs = []string{dir}
	t.Cleanup(func() { frrDaemonDirs = origDirs })
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write fake daemon %s: %v", name, err)
		}
	}
}

func installDaemonHooks(
	t *testing.T,
	runErr func(string) error,
	pids map[string][]int,
) (*[]string, *[]string) {
	t.Helper()
	origRun := runFRRDaemonCommand
	origDelay := frrDaemonStartDelay
	origFind := findFRRDaemonPIDs
	origSignal := signalFRRProcess
	commands := []string{}
	signals := []string{}
	activePIDs, pidNames := copyPIDMap(pids)
	runFRRDaemonCommand = func(_ context.Context, name string, _ ...string) error {
		commands = append(commands, name)
		if runErr != nil {
			return runErr(name)
		}
		return nil
	}
	frrDaemonStartDelay = 0
	findFRRDaemonPIDs = func(names []string) (map[string][]int, error) {
		result := make(map[string][]int, len(names))
		for _, name := range names {
			result[name] = append([]int(nil), activePIDs[name]...)
		}
		return result, nil
	}
	signalFRRProcess = func(pid int, sig syscall.Signal) error {
		name := pidNames[pid]
		signals = append(signals, fmt.Sprintf("%s:%d:%s", name, pid, sig))
		activePIDs[name] = removePID(activePIDs[name], pid)
		return nil
	}
	t.Cleanup(func() {
		runFRRDaemonCommand = origRun
		frrDaemonStartDelay = origDelay
		findFRRDaemonPIDs = origFind
		signalFRRProcess = origSignal
	})
	return &commands, &signals
}

func copyPIDMap(pids map[string][]int) (map[string][]int, map[int]string) {
	active := make(map[string][]int, len(pids))
	names := make(map[int]string)
	for name, ids := range pids {
		active[name] = append([]int(nil), ids...)
		for _, pid := range ids {
			names[pid] = name
		}
	}
	return active, names
}

func removePID(pids []int, remove int) []int {
	out := pids[:0]
	for _, pid := range pids {
		if pid != remove {
			out = append(out, pid)
		}
	}
	return out
}

func commandBases(commands []string) []string {
	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, filepath.Base(cmd))
	}
	return out
}

func TestWaitForHTTPWithFRRRestartsOnFailure(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The function should succeed after a few retries.
	err := waitForHTTPWithFRR(context.Background(), srv.URL, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
