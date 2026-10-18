//go:build linux

package lldp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var cliNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func writeTestState(t *testing.T, s State) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lldp.json")
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func liveState() State {
	return State{
		PID: os.Getpid(), UpdatedAt: cliNow.Add(-3 * time.Second),
		Transmit: []TxState{{Interface: "eth1", Profile: ProfileBM4XCableCheck, Announce: AnnounceParity, Frames: 3, LastTX: cliNow.Add(-5 * time.Second),
			Frame: &Neighbor{ChassisID: "m", ChassisIDSubtype: "local", PortID: "aa:c1:ab:00:00:01", PortIDSubtype: "mac", SystemName: "bare-minimal"}},
			{Interface: "eth2", Profile: ProfileNetworkd, Announce: AnnounceStandard, Error: "no carrier"}},
		Neighbors: []Neighbor{{
			Interface: "eth1", ChassisID: "00:3a:9c:5a:10:01", ChassisIDSubtype: "mac", PortID: "Ethernet1/12", PortIDSubtype: "ifname",
			PortDescription: "Ethernet1/12", SystemName: "leaf01", Description: "NX-OS", Capabilities: []string{"bridge", "router"},
			EnabledCapabilities: []string{"router"}, MgmtAddresses: []string{"192.0.2.1"}, VLAN: 1,
			MACPHY: &MACPHY{AutonegSupported: true, MAUType: 30}, MaxFrameSize: 9216, TTL: 120,
			LastSeen: cliNow.Add(-12 * time.Second),
		}},
	}
}

func runCLI(t *testing.T, deps CLIDeps, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	if deps.Now == nil {
		deps.Now = func() time.Time { return cliNow }
	}
	code = CLI(args, &o, &e, deps)
	return code, o.String(), e.String()
}

func TestCLINeighborsLive(t *testing.T) {
	path := writeTestState(t, liveState())
	code, out, _ := runCLI(t, CLIDeps{}, "neighbors", "--state", path)
	for _, want := range []string{
		"Interface: eth1 (last frame 12s ago, TTL 120 s, changes 0)",
		"  Chassis ID:    mac 00:3a:9c:5a:10:01",
		"  Port ID:       ifname Ethernet1/12",
		"  Port descr:    Ethernet1/12",
		"  System name:   leaf01",
		"  Capabilities:  bridge, router (enabled: router)",
		"  Mgmt address:  192.0.2.1",
		"  Port VLAN:     1",
		"  MAC/PHY:       autoneg supported true, enabled false, MAU type 30",
		"  Max frame:     9216",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	code, out, _ = runCLI(t, CLIDeps{}, "neighbors", "--json", "--state", path)
	var got []Neighbor
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0].SystemName != "leaf01" {
		t.Fatalf("JSON (exit %d): %s", code, out)
	}
}

func TestCLIStatus(t *testing.T) {
	path := writeTestState(t, liveState())
	code, out, _ := runCLI(t, CLIDeps{}, "status", "--state", path)
	for _, want := range []string{
		fmt.Sprintf("BOOTy LLDP (pid %d, state 3s old)", os.Getpid()),
		"Interface: eth1 (profile bm4x-cablecheck, announce parity, 3 frames, last 5s ago)",
		"  Port ID:       mac aa:c1:ab:00:00:01",
		"  System name:   bare-minimal",
		"Interface: eth2 (profile networkd, announce standard, 0 frames, last never)",
		"  Not sent:      no carrier",
		"Neighbors: 1 (booty lldp neighbors)",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Without a live state (no file, an old one, a gone BOOTy) neighbors
// captures on the NICs and status fails.
func TestCLINotRunning(t *testing.T) {
	stale := liveState()
	stale.UpdatedAt = cliNow.Add(-10 * time.Minute)
	gone := liveState()
	gone.PID = 1 << 30
	captured := []Neighbor{{Interface: "eth1", SystemName: "leaf01", PortID: "eth1", PortIDSubtype: "ifname",
		LastSeen: cliNow.Add(-4 * time.Second)}}
	deps := CLIDeps{
		NICs: func(context.Context) ([]string, error) { return []string{"eth1", "eth2"}, nil },
		Capture: func(_ context.Context, nics []string, timeout time.Duration) []Neighbor {
			if len(nics) != 2 || timeout != 5*time.Second {
				t.Errorf("capture on %v for %s", nics, timeout)
			}
			return append([]Neighbor(nil), captured...)
		},
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "none.json"), writeTestState(t, stale), writeTestState(t, gone)} {
		code, out, errOut := runCLI(t, deps, "neighbors", "--capture", "5s", "--state", path)
		if code != 0 || !strings.Contains(errOut, "BOOTy is not running; listening for LLDP for 5s on eth1 eth2") ||
			!strings.Contains(out, "Interface: eth1 (last frame 4s ago") || !strings.Contains(out, "  System name:   leaf01\n") {
			t.Errorf("%s: exit %d\n%s%s", path, code, errOut, out)
		}
		if code, _, errOut := runCLI(t, deps, "status", "--state", path); code != 1 || !strings.Contains(errOut, "BOOTy is not sending LLDP") {
			t.Errorf("%s: status exit %d: %s", path, code, errOut)
		}
	}
	deps.NICs = func(context.Context) ([]string, error) { return nil, errors.New("no sysfs") }
	if code, _, _ := runCLI(t, deps, "neighbors", "--state", "/nonexistent"); code != 1 {
		t.Errorf("capture without NICs: exit %d", code)
	}
}

func TestCLIUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"show"}, {"neighbors", "--bogus"}} {
		if code, _, _ := runCLI(t, CLIDeps{}, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// The senders record each frame (decoded) or why they did not send, and
// the state file has the transmit state and the neighbors.
func TestRecordTX(t *testing.T) {
	testState(t, 0)
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	frame, err := Frame("eth1", mac, &Info{MachineID: "m", Profile: ProfileBM4XCableCheck})
	if err != nil {
		t.Fatal(err)
	}
	bm4x := &Info{Profile: ProfileBM4XCableCheck, Announce: AnnounceFull}
	recordTX("eth1", bm4x, frame, nil, cliNow)
	recordTX("eth1", bm4x, frame, nil, cliNow.Add(time.Second))
	recordTX("eth0", &Info{}, nil, nil, cliNow.Add(time.Second))
	recordTX("eth2", &Info{}, nil, errors.New("network is down"), cliNow.Add(time.Second))
	s, ok := readState(StatePath, cliNow.Add(2*time.Second))
	if !ok || len(s.Transmit) != 3 {
		t.Fatalf("state live %v: %+v", ok, s)
	}
	eth0, eth1, eth2 := s.Transmit[0], s.Transmit[1], s.Transmit[2]
	if eth1.Frames != 2 || eth1.Announce != AnnounceFull || !eth1.LastTX.Equal(cliNow.Add(time.Second)) || eth1.Frame == nil || eth1.Frame.SystemName != "bare-minimal" ||
		eth1.Frame.PortID != "02:00:00:00:00:01" {
		t.Errorf("eth1 = %+v", eth1)
	}
	if eth0.Interface != "eth0" || eth0.Error != "no carrier" || eth0.Profile != ProfileNetworkd || eth0.Announce != AnnounceParity ||
		eth2.Error != "network is down" {
		t.Errorf("eth0 = %+v, eth2 = %+v", eth0, eth2)
	}
}

// The state file is written at most once per interval; the last update
// within an interval is written when it ends. Receive writes it at once,
// before any frame.
func TestWriteStateRate(t *testing.T) {
	testState(t, 300*time.Millisecond)
	t.Cleanup(func() {
		current.Lock()
		current.r = nil
		current.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Receive(ctx, nil, slog.Default())
	if _, err := os.Stat(StatePath); err != nil {
		t.Fatalf("no state file after Receive started: %v", err)
	}
	for range 5 {
		recordTX("eth0", &Info{}, nil, nil, time.Now())
	}
	if s, _ := readState(StatePath, time.Now()); len(s.Transmit) != 0 {
		t.Fatalf("written within the interval: %+v", s.Transmit)
	}
	time.Sleep(600 * time.Millisecond)
	if s, _ := readState(StatePath, time.Now()); len(s.Transmit) != 1 || s.Transmit[0].Error != "no carrier" {
		t.Fatalf("last update not written after the interval: %+v", s.Transmit)
	}
}

// Without frames a running receiver keeps the state file fresh.
func TestReceiveRefreshesState(t *testing.T) {
	testState(t, 0)
	origRefresh := stateRefresh
	stateRefresh = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		stateRefresh = origRefresh
		current.Lock()
		current.r = nil
		current.Unlock()
	})
	Receive(ctx, nil, slog.Default())
	first, _ := readState(StatePath, time.Now())
	time.Sleep(300 * time.Millisecond)
	later, _ := readState(StatePath, time.Now())
	if !later.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("state not refreshed: %v, then %v", first.UpdatedAt, later.UpdatedAt)
	}
}

// testState points the state file to a temporary path with an interval,
// under the writer lock that the delayed writes take too.
func testState(t *testing.T, interval time.Duration) {
	t.Helper()
	stateWrite.Lock()
	origPath, origInterval := StatePath, stateInterval
	StatePath, stateInterval, stateWrite.last = filepath.Join(t.TempDir(), "booty", "lldp.json"), interval, time.Time{}
	stateWrite.Unlock()
	t.Cleanup(func() {
		stateWrite.Lock()
		StatePath, stateInterval = origPath, origInterval
		stateWrite.Unlock()
		live.Lock()
		live.tx = nil
		live.Unlock()
	})
}
