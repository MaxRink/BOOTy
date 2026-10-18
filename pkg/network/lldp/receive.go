//go:build linux

package lldp

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// receiver keeps the latest LLDP neighbor of each interface until its TTL
// ends, like the receive side of lldpd. A frame with TTL 0 (shutdown)
// removes the neighbor at once. It keeps one neighbor per interface: a
// second chassis on the same link (a hub, or two agents of one switch)
// replaces the first; the fabric has one switch port per NIC.
type receiver struct {
	started   time.Time
	mu        sync.Mutex
	neighbors map[string]Neighbor
	expires   map[string]time.Time
	log       *slog.Logger
}

// current is the receiver of the process, read by Neighbors for the
// inventory report.
var current struct {
	sync.Mutex
	r *receiver
}

// Receive listens for LLDP frames on every interface until ctx ends and
// logs each new or changed neighbor. Neighbors returns what it learned.
func Receive(ctx context.Context, ifaces []string, log *slog.Logger) {
	r := &receiver{started: time.Now(), neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: log}
	current.Lock()
	current.r = r
	current.Unlock()
	for _, iface := range ifaces {
		go r.receiveOn(ctx, iface)
	}
}

// Neighbors returns the current LLDP neighbors, sorted by interface.
func Neighbors() []Neighbor {
	current.Lock()
	r := current.r
	current.Unlock()
	if r == nil {
		return nil
	}
	return r.list(time.Now())
}

// NeighborsWithin returns the current LLDP neighbors. While none is known
// yet, it waits until Receive has run for wait (one transmit interval of
// the switch, 30 s with lldpd and networkd), so a report soon after the
// start does not miss them. Without Receive it returns at once.
func NeighborsWithin(ctx context.Context, wait time.Duration) []Neighbor {
	current.Lock()
	r := current.r
	current.Unlock()
	if r == nil {
		return nil
	}
	for {
		n := r.list(time.Now())
		if len(n) > 0 || time.Since(r.started) >= wait {
			return n
		}
		select {
		case <-ctx.Done():
			return n
		case <-time.After(neighborPoll):
		}
	}
}

// neighborPoll is the time between two looks of NeighborsWithin.
var neighborPoll = 500 * time.Millisecond

func (r *receiver) receiveOn(ctx context.Context, iface string) {
	fd, err := openRX(iface)
	if err != nil {
		r.log.Warn("LLDP receive not started", "interface", iface, "error", err)
		return
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, 1600)
	for ctx.Err() == nil {
		if nb := readNeighbor(fd, buf, iface); nb != nil {
			r.record(nb, time.Now())
		}
	}
}

// record stores a neighbor and logs it when it is new or changed.
func (r *receiver) record(n *Neighbor, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, known := r.neighbors[n.Interface]
	if n.TTL == 0 {
		if known {
			r.log.Info("LLDP neighbor left", "interface", n.Interface, "chassisID", old.ChassisID, "portID", old.PortID)
		}
		delete(r.neighbors, n.Interface)
		delete(r.expires, n.Interface)
		return
	}
	if !known || r.expires[n.Interface].Before(now) || old.ChassisID != n.ChassisID || old.PortID != n.PortID ||
		old.SystemName != n.SystemName {
		r.log.Info("LLDP neighbor", "interface", n.Interface, "chassisID", n.ChassisID, "portID", n.PortID,
			"systemName", n.SystemName, "ttl", n.TTL)
	}
	r.neighbors[n.Interface] = *n
	r.expires[n.Interface] = now.Add(time.Duration(n.TTL) * time.Second)
}

func (r *receiver) list(now time.Time) []Neighbor {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Neighbor
	for iface, n := range r.neighbors {
		if r.expires[iface].After(now) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b Neighbor) int { return strings.Compare(a.Interface, b.Interface) })
	return out
}
