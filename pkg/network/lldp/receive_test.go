//go:build linux

package lldp

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"reflect"
	"strings"
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
	r := &receiver{started: time.Now(), neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: slog.Default()}
	current.Lock()
	current.r = r
	current.Unlock()
	start := time.Now()
	if got := NeighborsWithin(context.Background(), 100*time.Millisecond); len(got) != 0 || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("no neighbor: %+v after %s, want an empty list after the wait", got, time.Since(start))
	}
	time.AfterFunc(20*time.Millisecond, func() {
		r.record(&Neighbor{ChassisID: "leaf01", PortID: "eth1", Interface: "ens1f0np0", TTL: 120}, time.Now())
	})
	r.started = time.Now()
	start = time.Now()
	if got := NeighborsWithin(context.Background(), time.Hour); len(got) != 1 || time.Since(start) > 10*time.Second {
		t.Fatalf("neighbor after 20 ms: %+v after %s", got, time.Since(start))
	}
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
