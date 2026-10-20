//go:build linux

package lldp

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// StatePath is where the running BOOTy keeps its LLDP state for the
// "booty lldp" diagnosis command; /run is a tmpfs.
var StatePath = "/run/booty/lldp.json"

// State is the LLDP state of the running BOOTy.
type State struct {
	PID       int        `json:"pid"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Transmit  []TxState  `json:"transmit"`
	Neighbors []Neighbor `json:"neighbors"`
}

// TxState is the sender of one NIC.
type TxState struct {
	Interface string    `json:"interface"`
	Profile   string    `json:"profile"`
	Announce  string    `json:"announce"`
	LastTX    time.Time `json:"lastTX,omitzero"`
	Frames    int       `json:"frames"`
	// Error is why the last frame was not sent, "no carrier" when the NIC
	// waits for its link.
	Error string `json:"error,omitempty"`
	// Frame is the last sent frame, decoded.
	Frame *Neighbor `json:"frame,omitempty"`
}

// live is the transmit state of this process.
var live struct {
	sync.Mutex
	tx []TxState
}

// recordTX notes a send attempt and writes the state file. frame is nil
// when the NIC had no carrier. Without a frame and with the error of the
// last record nothing changed, so it does not write the file again.
func recordTX(iface string, info *Info, frame []byte, err error, now time.Time) {
	live.Lock()
	byIface := func(t TxState) bool { return t.Interface == iface }
	i := slices.IndexFunc(live.tx, byIface)
	if i >= 0 && frame == nil && live.tx[i].Error == txError(err) {
		live.Unlock()
		return
	}
	if i < 0 {
		live.tx = append(live.tx, TxState{Interface: iface})
		slices.SortFunc(live.tx, func(a, b TxState) int { return strings.Compare(a.Interface, b.Interface) })
	}
	t := &live.tx[slices.IndexFunc(live.tx, byIface)]
	t.Profile, t.Announce = cmp.Or(info.Profile, ProfileNetworkd), cmp.Or(info.Announce, AnnounceParity)
	if frame == nil {
		t.Error = txError(err)
	} else {
		t.Error, t.LastTX = "", now
		t.Frames++
		if len(frame) > 14 {
			t.Frame = parseLLDP(frame[14:], iface)
		}
	}
	live.Unlock()
	writeState(now)
}

// txError is the Error of a TxState without a frame.
func txError(err error) string {
	if err != nil {
		return err.Error()
	}
	return "no carrier"
}

// stateInterval is the shortest time between two writes of the state
// file: every NIC sends a frame and a busy switch port may send many, but
// the file is only for diagnosis.
var stateInterval = time.Second

// stateRefresh is how often a running receiver rewrites the state file
// without frames.
var stateRefresh = 30 * time.Second

// stateWrite serializes the writers of the state file (senders and
// receivers) and limits the rate: an update within stateInterval of the
// last write is written once that interval has passed.
var stateWrite struct {
	sync.Mutex
	last    time.Time
	pending bool
}

// writeState writes the state file, at most once per stateInterval; now
// is the time it reports.
func writeState(now time.Time) {
	stateWrite.Lock()
	defer stateWrite.Unlock()
	if wait := stateInterval - time.Since(stateWrite.last); wait > 0 {
		if !stateWrite.pending {
			stateWrite.pending = true
			time.AfterFunc(wait, func() {
				stateWrite.Lock()
				stateWrite.pending = false
				stateWrite.Unlock()
				writeState(time.Now())
			})
		}
		return
	}
	stateWrite.last = time.Now()
	writeStateFile(now)
}

// writeStateFile writes the state file atomically. Errors are ignored: the
// file is only for diagnosis.
func writeStateFile(now time.Time) {
	live.Lock()
	s := State{PID: os.Getpid(), UpdatedAt: now.UTC(), Transmit: slices.Clone(live.tx), Neighbors: Neighbors()}
	live.Unlock()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(StatePath), 0o755); err != nil { //nolint:gosec // readable diagnosis
		return
	}
	tmp := StatePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // readable diagnosis
		return
	}
	_ = os.Rename(tmp, StatePath)
}
