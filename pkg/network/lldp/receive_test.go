//go:build linux

package lldp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// A neighbor stays until its TTL ends, a newer frame replaces it, and TTL
// 0 removes it at once.
func TestReceiverRecord(t *testing.T) {
	r := &receiver{neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: slog.Default()}
	now := time.Unix(1000, 0)
	a := Neighbor{ChassisID: "leaf01", PortID: "eth1", Interface: "ens1f0np0", TTL: 120}
	b := Neighbor{ChassisID: "leaf02", PortID: "eth1", Interface: "ens1f1np1", TTL: 10}
	r.record(&a, now)
	r.record(&b, now)
	if got := r.list(now.Add(5 * time.Second)); !reflect.DeepEqual(got, []Neighbor{a, b}) {
		t.Fatalf("neighbors = %+v", got)
	}
	if got := r.list(now.Add(11 * time.Second)); !reflect.DeepEqual(got, []Neighbor{a}) {
		t.Fatalf("after the TTL of %s: %+v", b.Interface, got)
	}
	a2 := a
	a2.PortID = "eth9"
	r.record(&a2, now.Add(20*time.Second))
	if got := r.list(now.Add(130 * time.Second)); !reflect.DeepEqual(got, []Neighbor{a2}) {
		t.Fatalf("after a newer frame: %+v", got)
	}
	gone := a2
	gone.TTL = 0
	r.record(&gone, now.Add(21*time.Second))
	if got := r.list(now.Add(22 * time.Second)); len(got) != 0 {
		t.Fatalf("after TTL 0: %+v", got)
	}
}

// A TTL 0 frame logs that the neighbor left, a changed neighbor is logged
// again, and Neighbors reads the receiver that Receive started.
func TestReceiverLogsAndCurrent(t *testing.T) {
	var buf bytes.Buffer
	r := &receiver{started: time.Now(), neighbors: map[string]Neighbor{}, expires: map[string]time.Time{},
		log: slog.New(slog.NewTextHandler(&buf, nil))}
	now := time.Now()
	a := Neighbor{ChassisID: "leaf01", PortID: "eth1", Interface: "ens1f0np0", TTL: 120}
	r.record(&a, now)
	moved := a
	moved.PortID = "eth9"
	r.record(&moved, now.Add(time.Second))
	if strings.Count(buf.String(), `msg="LLDP neighbor"`) != 2 || !strings.Contains(buf.String(), "portID=eth9") {
		t.Fatalf("log = %s, want the new and the changed neighbor", buf.String())
	}
	gone := moved
	gone.TTL = 0
	r.record(&gone, now.Add(2*time.Second))
	if !strings.Contains(buf.String(), `msg="LLDP neighbor left" interface=ens1f0np0`) {
		t.Fatalf("log = %s, want the neighbor that left", buf.String())
	}

	r.record(&a, now)
	current.Lock()
	prev := current.r
	current.r = r
	current.Unlock()
	t.Cleanup(func() { current.Lock(); current.r = prev; current.Unlock() })
	if got := Neighbors(); len(got) != 1 || got[0].ChassisID != "leaf01" {
		t.Fatalf("Neighbors = %+v, want the receiver's neighbor", got)
	}
}

// NeighborsWithin returns at once with a known neighbor or without a
// receiver, and otherwise waits until the receiver has run for wait.
func TestNeighborsWithin(t *testing.T) {
	current.Lock()
	prev := current.r
	current.r = nil
	current.Unlock()
	t.Cleanup(func() { current.Lock(); current.r = prev; current.Unlock() })
	if got := NeighborsWithin(context.Background(), time.Hour); got != nil {
		t.Fatalf("without a receiver: %+v", got)
	}
	poll := neighborPoll
	neighborPoll = time.Millisecond
	t.Cleanup(func() { neighborPoll = poll })
	// The wait counts from started, so the check does too: a start time
	// taken after started (late under -race) made the test flaky.
	started := time.Now()
	r := &receiver{started: started, neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: slog.Default()}
	current.Lock()
	current.r = r
	current.Unlock()
	if got := NeighborsWithin(context.Background(), 100*time.Millisecond); len(got) != 0 || time.Since(started) < 100*time.Millisecond {
		t.Fatalf("no neighbor: %+v after %s, want an empty list after the wait", got, time.Since(started))
	}
	// A neighbor that arrives during an hour-long wait ends it.
	r.mu.Lock()
	r.started = time.Now()
	r.mu.Unlock()
	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		r.record(&Neighbor{ChassisID: "leaf01", PortID: "eth1", Interface: "ens1f0np0", TTL: 120}, time.Now())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got := NeighborsWithin(ctx, time.Hour); len(got) != 1 {
		t.Fatalf("neighbor recorded during the wait: got %+v, ctx %v", got, ctx.Err())
	}
	<-recorded
}

// A frame from one of our NICs or with our chassis ID is ours.
func TestOwnFrame(t *testing.T) {
	prev := ownMachineID
	t.Cleanup(func() { ownMachineID = prev })
	ownMachineID = func() string { return "c0ffee" }
	if !ownFrame(net.HardwareAddr{2, 0, 0, 0, 0x99, 0x99}, "c0ffee") {
		t.Fatal("our chassis ID is not ours")
	}
	if ownFrame(net.HardwareAddr{2, 0, 0, 0, 0x99, 0x99}, "leaf01") {
		t.Fatal("a foreign frame is ours")
	}
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if len(ifi.HardwareAddr) == 6 {
			if !ownFrame(ifi.HardwareAddr, "leaf01") || !ownFrame(net.HardwareAddr{2, 0, 0, 0, 0x99, 0x99}, ifi.HardwareAddr.String()) {
				t.Fatalf("a frame from %s or with its MAC as chassis is not ours", ifi.Name)
			}
			return
		}
	}
}

// FirstSeen stays while the same neighbor sends; a different neighbor on
// the interface, or the same one after its TTL ran out, counts as a change.
func TestReceiverChanges(t *testing.T) {
	r := &receiver{neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: slog.Default()}
	t0 := time.Unix(1000, 0)
	n := Neighbor{ChassisID: "leaf01", PortID: "eth1", Interface: "ens1f0np0", TTL: 120}
	for i, step := range []struct {
		at        time.Duration
		chassis   string
		firstSeen time.Duration
		changes   int
	}{
		{0, "leaf01", 0, 0},
		{30 * time.Second, "leaf01", 0, 0},                  // same neighbor
		{60 * time.Second, "leaf02", 60 * time.Second, 1},   // cable moved
		{400 * time.Second, "leaf02", 400 * time.Second, 2}, // back after the TTL
	} {
		f := n
		f.ChassisID = step.chassis
		r.record(&f, t0.Add(step.at))
		got := r.neighbors["ens1f0np0"]
		if !got.FirstSeen.Equal(t0.Add(step.firstSeen)) || got.Changes != step.changes || !got.LastSeen.Equal(t0.Add(step.at)) {
			t.Fatalf("step %d: first %v last %v changes %d", i, got.FirstSeen.Sub(t0), got.LastSeen.Sub(t0), got.Changes)
		}
	}
}

// Only NICs with carrier need a neighbor.
func TestMissingNeighbors(t *testing.T) {
	orig := hasCarrier
	t.Cleanup(func() { hasCarrier = orig })
	hasCarrier = func(iface string) bool { return iface != "eno1" }
	got := missingNeighbors([]string{"eno1", "ens1f0np0", "ens1f1np1"}, []Neighbor{{Interface: "ens1f0np0"}})
	if !reflect.DeepEqual(got, []string{"ens1f1np1"}) {
		t.Fatalf("missing = %v, want [ens1f1np1]", got)
	}
}

// A decoder panic drops the frame and keeps the receiver running.
func TestReceiverReadRecovers(t *testing.T) {
	orig := readFrame
	t.Cleanup(func() { readFrame = orig })
	readFrame = func(int, []byte, string) *Neighbor { panic("index out of range") }
	r := &receiver{neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: slog.Default()}
	if nb := r.read(-1, nil, "eth0"); nb != nil {
		t.Fatalf("read = %+v, want nil", nb)
	}
}

// WaitForNeighbors does not judge before the first carrier: links come up
// after the network setup.
func TestWaitForNeighborsWaitsForCarrier(t *testing.T) {
	origCarrier, origSettle, origTick := hasCarrier, carrierSettle, waitTick
	current.Lock()
	origR := current.r
	current.r = nil
	current.Unlock()
	t.Cleanup(func() {
		hasCarrier, carrierSettle, waitTick = origCarrier, origSettle, origTick
		current.Lock()
		current.r = origR
		current.Unlock()
	})
	carrierSettle, waitTick = 5*time.Second, 10*time.Millisecond
	up := time.Now().Add(200 * time.Millisecond)
	hasCarrier = func(string) bool { return time.Now().After(up) }
	got := WaitForNeighbors(context.Background(), []string{"eth0"}, 300*time.Millisecond)
	if !reflect.DeepEqual(got, []string{"eth0"}) {
		t.Fatalf("missing = %v, want [eth0] (carrier came up after 200 ms)", got)
	}
	// Without any carrier it returns after the settle time, not the timeout.
	hasCarrier = func(string) bool { return false }
	carrierSettle = 50 * time.Millisecond
	start := time.Now()
	if got := WaitForNeighbors(context.Background(), []string{"eth0"}, time.Minute); len(got) != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("no carrier: missing %v after %v", got, time.Since(start))
	}
}

// On a host with two NICs, one with carrier and a neighbor does not end the
// wait while the other one is still coming up.
func TestWaitForNeighborsWaitsForEveryCarrier(t *testing.T) {
	origCarrier, origSettle, origTick := hasCarrier, carrierSettle, waitTick
	r := &receiver{started: time.Now(), neighbors: map[string]Neighbor{"eth0": {Interface: "eth0"}},
		expires: map[string]time.Time{"eth0": time.Now().Add(time.Hour)}, log: slog.Default()}
	current.Lock()
	origR := current.r
	current.r = r
	current.Unlock()
	t.Cleanup(func() {
		hasCarrier, carrierSettle, waitTick = origCarrier, origSettle, origTick
		current.Lock()
		current.r = origR
		current.Unlock()
	})
	carrierSettle, waitTick = 5*time.Second, 10*time.Millisecond
	up := time.Now().Add(200 * time.Millisecond)
	hasCarrier = func(iface string) bool { return iface == "eth0" || time.Now().After(up) }
	got := WaitForNeighbors(context.Background(), []string{"eth0", "eth1"}, 300*time.Millisecond)
	if !reflect.DeepEqual(got, []string{"eth1"}) {
		t.Fatalf("missing = %v, want [eth1] (its carrier came up after 200 ms)", got)
	}
}

// ReceiveDetected returns before the NIC detection ends and registers its
// receiver at once, so NeighborsWithin waits for it. A failed detection is
// logged.
func TestReceiveDetectedDoesNotWaitForDetection(t *testing.T) {
	current.Lock()
	prev := current.r
	current.Unlock()
	t.Cleanup(func() { current.Lock(); current.r = prev; current.Unlock() })
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	release := make(chan struct{})
	detected := make(chan struct{})
	ReceiveDetected(context.Background(), func(context.Context) ([]string, error) {
		<-release
		defer close(detected)
		return nil, errors.New("no NICs")
	}, log)

	current.Lock()
	r := current.r
	current.Unlock()
	if r == nil || r == prev {
		t.Fatal("ReceiveDetected did not register its receiver before the detection ended")
	}
	close(release)
	<-detected
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		logged := strings.Contains(buf.String(), "LLDP receive not started")
		mu.Unlock()
		if logged {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("failed detection not logged")
		}
		time.Sleep(time.Millisecond)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// fakeDetect makes a detect function that returns ifaces and err once
// release is closed.
func fakeDetect(release <-chan struct{}, ifaces []string, err error) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		select {
		case <-release:
			return ifaces, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func keepCurrent(t *testing.T) {
	t.Helper()
	current.Lock()
	prev := current.r
	current.Unlock()
	t.Cleanup(func() { current.Lock(); current.r = prev; current.Unlock() })
	poll := neighborPoll
	neighborPoll = time.Millisecond
	t.Cleanup(func() { neighborPoll = poll })
}

// The wait of NeighborsWithin counts from when the sockets open, not from
// the registration: a slow NIC detection does not shorten it.
func TestNeighborsWithinCountsFromListen(t *testing.T) {
	keepCurrent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	release := make(chan struct{})
	// No such interface: the receive logs a warning and ends.
	ReceiveDetected(ctx, fakeDetect(release, []string{"booty-none0"}, nil), slog.New(slog.DiscardHandler))
	const detectTime, wait = 200 * time.Millisecond, 50 * time.Millisecond
	time.AfterFunc(detectTime, func() { close(release) })
	start := time.Now()
	NeighborsWithin(context.Background(), wait)
	if got := time.Since(start); got < detectTime+wait {
		t.Fatalf("NeighborsWithin returned after %s, want at least the detection plus the wait (%s)", got, detectTime+wait)
	}
}

// A failed or empty NIC detection unregisters the receiver, so
// NeighborsWithin returns at once instead of waiting the transmit interval.
func TestNeighborsWithinReturnsAfterFailedDetection(t *testing.T) {
	for name, tc := range map[string]struct {
		ifaces []string
		err    error
	}{
		"error":   {err: errors.New("no NICs")},
		"no NICs": {},
	} {
		t.Run(name, func(t *testing.T) {
			keepCurrent(t)
			release := make(chan struct{})
			ReceiveDetected(context.Background(), fakeDetect(release, tc.ifaces, tc.err), slog.New(slog.DiscardHandler))
			time.AfterFunc(20*time.Millisecond, func() { close(release) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			if got := NeighborsWithin(ctx, time.Hour); len(got) != 0 {
				t.Fatalf("neighbors = %+v", got)
			}
			if got := time.Since(start); got > 2*time.Second {
				t.Fatalf("NeighborsWithin waited %s after a failed detection", got)
			}
			current.Lock()
			r := current.r
			current.Unlock()
			if r != nil {
				t.Fatal("the receiver of a failed detection is still registered")
			}
		})
	}
}

// While the detection runs, ctx bounds the wait.
func TestNeighborsWithinDuringDetectionHonoursContext(t *testing.T) {
	keepCurrent(t)
	detectCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	ReceiveDetected(detectCtx, fakeDetect(make(chan struct{}), nil, nil), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	NeighborsWithin(ctx, time.Millisecond)
	if got := time.Since(start); got < 20*time.Millisecond || got > 2*time.Second {
		t.Fatalf("NeighborsWithin returned after %s, want at the context deadline (20 ms)", got)
	}
}
