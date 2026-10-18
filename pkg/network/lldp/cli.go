//go:build linux

package lldp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// CLIDeps are what the lldp command needs from outside the package.
type CLIDeps struct {
	// NICs lists the physical NICs for a capture.
	NICs func(context.Context) ([]string, error)
	// Capture listens on the NICs; nil uses DiscoverAll.
	Capture func(ctx context.Context, nics []string, timeout time.Duration) []Neighbor
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// staleAfter: the running BOOTy writes its state at least every 30 s
// (one frame per NIC); an older file is from a BOOTy that stopped.
const staleAfter = 75 * time.Second

const cliUsage = `usage: booty lldp neighbors|status [--json] [--capture DURATION] [--state FILE]

  neighbors  the LLDP neighbor of each NIC (like lldpcli show neighbors detail)
  status     what BOOTy sends on each NIC (like lldpcli show interfaces)

The command reads the live state of the running BOOTy (--state, default
/run/booty/lldp.json). Without it, neighbors listens on every physical NIC
for --capture (default 35s; switches send every 30 s).
`

// CLI runs "booty lldp ..." and returns the exit code.
func CLI(args []string, out, errOut io.Writer, deps CLIDeps) int {
	if len(args) == 0 || (args[0] != "neighbors" && args[0] != "status") {
		_, _ = fmt.Fprint(errOut, cliUsage)
		return 2
	}
	fs := flag.NewFlagSet("lldp "+args[0], flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print JSON")
	capture := fs.Duration("capture", 35*time.Second, "capture time without a running BOOTy")
	path := fs.String("state", StatePath, "state file of the running BOOTy")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	now := time.Now
	if deps.Now != nil {
		now = deps.Now
	}
	state, live := readState(*path, now())
	if args[0] == "status" {
		if !live {
			_, _ = fmt.Fprintf(errOut, "BOOTy is not sending LLDP: no live state in %s\n", *path)
			return 1
		}
		return output(out, *asJSON, state, func() { printStatus(out, &state, now()) })
	}
	neighbors := state.Neighbors
	if !live {
		var err error
		if neighbors, err = captureNeighbors(errOut, deps, *capture, now()); err != nil {
			_, _ = fmt.Fprintln(errOut, err)
			return 1
		}
	}
	return output(out, *asJSON, neighbors, func() { printNeighbors(out, neighbors, now()) })
}

func output(out io.Writer, asJSON bool, v any, text func()) int {
	if !asJSON {
		text()
		return 0
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return 1
	}
	return 0
}

// readState reads the state file; live is false without a file, when its
// BOOTy is gone or when it is older than staleAfter.
func readState(path string, now time.Time) (State, bool) {
	var s State
	b, err := os.ReadFile(path) //nolint:gosec // diagnosis input
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, false
	}
	if s.PID <= 0 {
		return s, false
	}
	err = syscall.Kill(s.PID, 0) // signal 0: does the process exist
	alive := err == nil || errors.Is(err, syscall.EPERM)
	return s, alive && now.Sub(s.UpdatedAt) < staleAfter
}

func captureNeighbors(errOut io.Writer, deps CLIDeps, timeout time.Duration, now time.Time) ([]Neighbor, error) {
	if deps.NICs == nil {
		return nil, errors.New("no NIC lister")
	}
	nics, err := deps.NICs(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list NICs: %w", err)
	}
	_, _ = fmt.Fprintf(errOut, "BOOTy is not running; listening for LLDP for %s on %s\n", timeout, strings.Join(nics, " "))
	capture := deps.Capture
	if capture == nil {
		capture = DiscoverAll
	}
	neighbors := capture(context.Background(), nics, timeout)
	for i := range neighbors {
		if neighbors[i].LastSeen.IsZero() { // a capture without the frame time
			neighbors[i].FirstSeen, neighbors[i].LastSeen = now, now
		}
	}
	return neighbors, nil
}

func printNeighbors(out io.Writer, neighbors []Neighbor, now time.Time) {
	if len(neighbors) == 0 {
		_, _ = fmt.Fprintln(out, "no LLDP neighbors")
		return
	}
	for i := range neighbors {
		n := &neighbors[i]
		age := now.Sub(n.LastSeen).Truncate(time.Second)
		_, _ = fmt.Fprintf(out, "Interface: %s (last frame %s ago, TTL %d s, changes %d)\n", n.Interface, age, n.TTL, n.Changes)
		printFrame(out, n)
	}
}

// printFrame prints the TLVs of a neighbor or of a sent frame.
func printFrame(out io.Writer, n *Neighbor) {
	line := func(k, v string) {
		if v != "" {
			_, _ = fmt.Fprintf(out, "  %-14s %s\n", k+":", v)
		}
	}
	line("Chassis ID", strings.TrimSpace(n.ChassisIDSubtype+" "+n.ChassisID))
	line("Port ID", strings.TrimSpace(n.PortIDSubtype+" "+n.PortID))
	line("Port descr", n.PortDescription)
	line("System name", n.SystemName)
	line("System descr", n.Description)
	if len(n.Capabilities) > 0 {
		line("Capabilities", strings.Join(n.Capabilities, ", ")+" (enabled: "+strings.Join(n.EnabledCapabilities, ", ")+")")
	}
	line("Mgmt address", strings.Join(n.MgmtAddresses, ", "))
	if n.VLAN != 0 {
		line("Port VLAN", fmt.Sprint(n.VLAN))
	}
	line("VLAN names", strings.Join(n.VLANNames, ", "))
	if a := n.LinkAggregation; a != nil {
		line("Aggregation", fmt.Sprintf("supported %v, enabled %v, port %d", a.Supported, a.Enabled, a.PortID))
	}
	if m := n.MACPHY; m != nil {
		line("MAC/PHY", fmt.Sprintf("autoneg supported %v, enabled %v, MAU type %d", m.AutonegSupported, m.AutonegEnabled, m.MAUType))
	}
	if n.MaxFrameSize != 0 {
		line("Max frame", fmt.Sprint(n.MaxFrameSize))
	}
	if inv := n.Inventory; inv != nil {
		line("MED inventory", strings.Join(nonEmptyPairs(
			"hw", inv.HardwareRevision, "fw", inv.FirmwareRevision, "sw", inv.SoftwareRevision, "serial", inv.SerialNumber,
			"vendor", inv.Manufacturer, "model", inv.Model, "asset", inv.AssetID), ", "))
	}
}

func nonEmptyPairs(kv ...string) []string {
	var out []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			out = append(out, kv[i]+"="+kv[i+1])
		}
	}
	return out
}

func printStatus(out io.Writer, s *State, now time.Time) {
	_, _ = fmt.Fprintf(out, "BOOTy LLDP (pid %d, state %s old)\n", s.PID, now.Sub(s.UpdatedAt).Truncate(time.Second))
	if len(s.Transmit) == 0 {
		_, _ = fmt.Fprintln(out, "not sending on any NIC")
	}
	for i := range s.Transmit {
		t := &s.Transmit[i]
		last := "never"
		if !t.LastTX.IsZero() {
			last = now.Sub(t.LastTX).Truncate(time.Second).String() + " ago"
		}
		_, _ = fmt.Fprintf(out, "Interface: %s (profile %s, announce %s, %d frames, last %s)\n", t.Interface, t.Profile, t.Announce, t.Frames, last)
		if t.Error != "" {
			_, _ = fmt.Fprintf(out, "  %-14s %s\n", "Not sent:", t.Error)
		}
		if t.Frame != nil {
			printFrame(out, t.Frame)
		}
	}
	_, _ = fmt.Fprintf(out, "Neighbors: %d (booty lldp neighbors)\n", len(s.Neighbors))
}
