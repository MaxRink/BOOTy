//go:build linux

package lldp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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
	mu        sync.Mutex
	started   time.Time // when the sockets open; zero before, guarded by mu
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
	newReceiver(log).listen(ctx, ifaces)
}

// ReceiveDetected is Receive on the interfaces that detect returns. It
// registers the receiver at once, so NeighborsWithin waits for the
// detection, but detect runs in the background: it may wait seconds for
// the NIC set to settle, and the boot must not wait for a passive
// receiver. A failed detection unregisters the receiver again.
func ReceiveDetected(ctx context.Context, detect func(context.Context) ([]string, error), log *slog.Logger) {
	r := newReceiver(log)
	go func() {
		ifaces, err := detect(ctx)
		if err != nil || len(ifaces) == 0 {
			log.Warn("LLDP receive not started: no physical NICs", "error", err)
			current.Lock()
			if current.r == r {
				current.r = nil
			}
			current.Unlock()
			return
		}
		r.listen(ctx, ifaces)
	}()
}

// newReceiver makes the receiver of the process.
func newReceiver(log *slog.Logger) *receiver {
	r := &receiver{neighbors: map[string]Neighbor{}, expires: map[string]time.Time{}, log: log}
	current.Lock()
	current.r = r
	current.Unlock()
	return r
}

// listen starts a receive on every interface. The wait of NeighborsWithin
// counts from here, not from the registration.
func (r *receiver) listen(ctx context.Context, ifaces []string) {
	r.mu.Lock()
	r.started = time.Now()
	r.mu.Unlock()
	// The state file exists from the start and stays fresh without frames,
	// so "booty lldp status" tells "no neighbor yet" from "not running"
	// (a file older than 75 s).
	writeState(time.Now())
	refresh := stateRefresh
	go func() {
		tick := time.NewTicker(refresh)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				writeState(now)
			}
		}
	}()
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
// yet, it waits until Receive has listened for wait (one transmit interval
// of the switch, 30 s with lldpd and networkd), so a report soon after the
// start does not miss them. While the NIC detection of ReceiveDetected
// runs, it waits for it, bounded by ctx. Without a receiver, or when the
// detection failed, it returns at once.
func NeighborsWithin(ctx context.Context, wait time.Duration) []Neighbor {
	current.Lock()
	r := current.r
	current.Unlock()
	if r == nil {
		return nil
	}
	for {
		n := r.list(time.Now())
		if len(n) > 0 {
			return n
		}
		current.Lock()
		registered := current.r == r
		current.Unlock()
		r.mu.Lock()
		started := r.started
		r.mu.Unlock()
		if !registered || (!started.IsZero() && time.Since(started) >= wait) {
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
		if nb := r.read(fd, buf, iface); nb != nil {
			r.record(nb, time.Now())
			writeState(time.Now())
		}
	}
}

// read reads one frame. A decoder panic drops that frame only: receive is
// on by default and runs in PID 1, and the frames come from the wire.
func (r *receiver) read(fd int, buf []byte, iface string) (nb *Neighbor) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Warn("LLDP frame dropped: decoder panic", "interface", iface, "panic", fmt.Sprint(p))
			nb = nil
		}
	}()
	return readFrame(fd, buf, iface)
}

// readFrame reads and decodes one frame; a test seam.
var readFrame = readNeighbor

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
	expired := known && r.expires[n.Interface].Before(now)
	switch {
	case known && !expired && sameNeighbor(&old, n):
		n.FirstSeen, n.Changes = old.FirstSeen, old.Changes
	default:
		n.FirstSeen = now
		if known {
			n.Changes = old.Changes + 1
		}
		r.log.Info("LLDP neighbor", "interface", n.Interface, "chassisID", n.ChassisID,
			"portID", n.PortID, "portIDSubtype", n.PortIDSubtype, "portDescription", n.PortDescription,
			"systemName", n.SystemName, "mgmtAddresses", n.MgmtAddresses, "vlan", n.VLAN,
			"maxFrameSize", n.MaxFrameSize, "ttl", n.TTL, "changes", n.Changes)
	}
	n.LastSeen = now
	r.neighbors[n.Interface] = *n
	r.expires[n.Interface] = now.Add(time.Duration(n.TTL) * time.Second)
}

func (r *receiver) list(now time.Time) []Neighbor {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Neighbor
	for iface := range r.neighbors {
		if r.expires[iface].After(now) {
			out = append(out, r.neighbors[iface])
		}
	}
	slices.SortFunc(out, func(a, b Neighbor) int { return strings.Compare(a.Interface, b.Interface) })
	return out
}

// carrierSettle is how long WaitForNeighbors waits for a first carrier;
// waitTick is its poll interval.
var (
	carrierSettle = 15 * time.Second
	waitTick      = time.Second
)

// hasCarrier reports whether a NIC has carrier; a test seam.
var hasCarrier = func(iface string) bool {
	ifi, err := net.InterfaceByName(iface)
	return err == nil && ifi.Flags&net.FlagRunning != 0
}

// WaitForNeighbors waits until every NIC with carrier has an LLDP
// neighbor, or until timeout. It returns the NICs with carrier that have
// none. A switch sends every 30 s, so a timeout of 90 s covers a lost
// frame. Links come up a few seconds after the network setup, so while a
// NIC has no carrier it waits up to carrierSettle (at most the timeout)
// before it judges; a port that stays down only costs that settle time.
func WaitForNeighbors(ctx context.Context, ifaces []string, timeout time.Duration) []string {
	start := time.Now()
	settle := min(timeout, carrierSettle)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(waitTick)
	defer tick.Stop()
	for {
		missing := missingNeighbors(ifaces, Neighbors())
		allUp := !slices.ContainsFunc(ifaces, func(i string) bool { return !hasCarrier(i) })
		settled := allUp || time.Since(start) >= settle
		if len(missing) == 0 && settled || ctx.Err() != nil {
			return missing
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

// missingNeighbors lists the NICs with carrier and no neighbor.
func missingNeighbors(ifaces []string, neighbors []Neighbor) []string {
	var missing []string
	for _, iface := range ifaces {
		if !hasCarrier(iface) {
			continue
		}
		if !slices.ContainsFunc(neighbors, func(n Neighbor) bool { return n.Interface == iface }) {
			missing = append(missing, iface)
		}
	}
	return missing
}
