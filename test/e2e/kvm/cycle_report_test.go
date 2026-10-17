package kvm

// The cycle report turns the host-timestamped serial lines of the
// deprovision and provision VMs into phase durations, groups them and
// compares them with a baseline report of main. TestAgentCycleTimingQEMU
// (cycle_timing_test.go) writes it. This file has no build tag, so the unit
// tests below run with the normal unit tests.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// timedLine is one serial console line and the time since the VM started.
type timedLine struct {
	at   time.Duration
	text string
}

type cyclePhase struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
}

// cycleRun is one measured cycle: "cold" (two ISO boots) or "agent" (one
// standby boot, commands only), or the one-time "agent-boot".
type cycleRun struct {
	Name         string       `json:"name"`
	TotalSeconds float64      `json:"totalSeconds"`
	Phases       []cyclePhase `json:"phases"`
}

type cycleReport struct {
	Accel  string     `json:"accel"`
	Commit string     `json:"commit,omitempty"`
	Cycles []cycleRun `json:"cycles"`
}

var stepRE = regexp.MustCompile(`msg="(provisioning|deprovisioning) step".* step=([a-z0-9-]+)`)
var commandRE = regexp.MustCompile(`msg="received command".* type=([a-z-]+)`)

// boundary returns the name of the phase that a line starts, or "".
func boundary(text string) string {
	if m := stepRE.FindStringSubmatch(text); m != nil {
		return strings.TrimSuffix(m[1], "ing") + "/" + m[2]
	}
	if m := commandRE.FindStringSubmatch(text); m != nil {
		return "command/" + m[1]
	}
	for _, b := range []struct{ marker, name string }{
		{"Linux version", "kernel-boot"},
		{"starting BOOTy", "network-bring-up"},
		{"network connectivity established", "pre-task"},
		{"entering standby mode", "standby/idle"},
		{"returning to standby", "standby/idle"},
		{`msg="CAPRF run complete"`, "handoff"},
	} {
		if strings.Contains(text, b.marker) {
			return b.name
		}
	}
	return ""
}

// phasesBetween splits [from, to] at the phase boundaries of lines. The time
// before the first boundary is "firmware-iso-load", the time from VM start
// until the kernel prints its banner.
func phasesBetween(lines []timedLine, from, to time.Duration) []cyclePhase {
	name, start := "firmware-iso-load", time.Duration(0)
	var phases []cyclePhase
	add := func(end time.Duration) {
		s, e := max(start, from), min(end, to)
		if e > s {
			phases = append(phases, cyclePhase{Name: name, Seconds: (e - s).Seconds()})
		}
	}
	for _, l := range lines {
		next := boundary(l.text)
		if next == "" {
			continue
		}
		add(l.at)
		name, start = next, l.at
	}
	add(to)
	return phases
}

// phaseGroup maps a phase to the row of the summary table.
func phaseGroup(name string) string {
	switch {
	case name == "firmware-iso-load" || name == "kernel-boot":
		return "boot: firmware, ISO, kernel"
	case name == "network-bring-up":
		return "network bring-up"
	case name == "pre-task":
		return "pre-task (mode, crash check)"
	case strings.HasPrefix(name, "standby/") || strings.HasPrefix(name, "command/"):
		return "agent: wait for command"
	case strings.HasSuffix(name, "/wipe-disks"):
		return "disk wipe"
	case strings.HasSuffix(name, "/stream-image"):
		return "image stream"
	case strings.HasPrefix(name, "deprovision/"):
		return "deprovision steps (other)"
	case strings.HasPrefix(name, "provision/"):
		return "provision steps (other)"
	default:
		return "handoff (power off)"
	}
}

// groupOrder is the row order of the summary table.
var groupOrder = []string{
	"boot: firmware, ISO, kernel", "network bring-up", "pre-task (mode, crash check)",
	"agent: wait for command", "disk wipe", "deprovision steps (other)",
	"image stream", "provision steps (other)", "handoff (power off)",
}

func groups(r cycleRun) map[string]float64 {
	g := map[string]float64{}
	for _, p := range r.Phases {
		g[phaseGroup(p.Name)] += p.Seconds
	}
	return g
}

func (r cycleReport) cycle(name string) (cycleRun, bool) {
	for _, c := range r.Cycles {
		if c.Name == name {
			return c, true
		}
	}
	return cycleRun{}, false
}

// regressionPolicy says when a cycle total counts as a regression: when it
// grows by more than Percent and by more than MinSeconds over the baseline.
// The cold cycle has its own MinSeconds: it is two boots and noisier, the
// agent cycles are short, so a few seconds already matter there.
type regressionPolicy struct {
	Percent        float64
	MinSeconds     float64 // cold cycle
	AgentMinSecond float64 // agent and agent-boot cycles
}

func (p regressionPolicy) minSeconds(cycle string) float64 {
	if cycle == "cold" {
		return p.MinSeconds
	}
	return p.AgentMinSecond
}

// comparable reports whether base can be the baseline of cur: a run under
// TCG is several times slower than under KVM, so other accelerators are
// not compared.
func comparable(cur, base cycleReport) bool {
	return cur.Accel == base.Accel
}

// regressions lists the cycles of cur that are slower than in base.
func regressions(cur, base cycleReport, p regressionPolicy) []string {
	if !comparable(cur, base) {
		return nil
	}
	var out []string
	for _, c := range cur.Cycles {
		b, ok := base.cycle(c.Name)
		if !ok || b.TotalSeconds <= 0 {
			continue
		}
		delta, minSec := c.TotalSeconds-b.TotalSeconds, p.minSeconds(c.Name)
		if delta > minSec && delta/b.TotalSeconds*100 > p.Percent {
			out = append(out, fmt.Sprintf("%s cycle %.1fs vs %.1fs on main (+%.0f%%, limit %.0f%% and %.0fs)",
				c.Name, c.TotalSeconds, b.TotalSeconds, delta/b.TotalSeconds*100, p.Percent, minSec))
		}
	}
	return out
}

// markdown renders the job summary: one column per cycle, and the baseline
// delta when base is not nil.
func markdown(cur cycleReport, base *cycleReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "### Deprovision + provision cycle (accel %s)\n\n", cur.Accel)
	sb.WriteString("| Phase |")
	for _, c := range cur.Cycles {
		fmt.Fprintf(&sb, " %s (s) |", c.Name)
	}
	sb.WriteString("\n|---|" + strings.Repeat("---:|", len(cur.Cycles)) + "\n")
	rows := append(append([]string{}, groupOrder...), "total")
	for _, row := range rows {
		line := "| " + row + " |"
		seen := false
		for _, c := range cur.Cycles {
			v, b := groups(c)[row], 0.0
			if row == "total" {
				v = c.TotalSeconds
			}
			if base != nil {
				if bc, ok := base.cycle(c.Name); ok {
					b = groups(bc)[row]
					if row == "total" {
						b = bc.TotalSeconds
					}
				}
			}
			seen = seen || v > 0 || b > 0
			line += " " + cell(v, b, base != nil) + " |"
		}
		if seen {
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}

func cell(v, base float64, withBase bool) string {
	if !withBase || base == 0 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.1f (%+.1f)", v, v-base)
}

func TestCyclePhasesBetween(t *testing.T) {
	s := time.Second
	lines := []timedLine{
		{2 * s, "Linux version 6.8"},
		{3 * s, `time=x level=INFO msg="starting BOOTy" version=dev`},
		{5 * s, `time=x level=INFO msg="network connectivity established"`},
		{6 * s, `time=x level=INFO msg="entering standby mode"`},
		{10 * s, `time=x level=INFO msg="received command" id=d1 type=deprovision`},
		{11 * s, `time=x level=INFO msg="deprovisioning step" component=provision step=wipe-disks index=8 total=9`},
		{14 * s, `time=x level=INFO msg="deprovisioning complete, returning to standby"`},
		{15 * s, `time=x level=INFO msg="received command" id=p1 type=provision`},
		{16 * s, `time=x level=INFO msg="provisioning step" component=provision step=stream-image index=18 total=51`},
		{20 * s, `time=x level=INFO msg="CAPRF run complete"`},
	}
	got := phasesBetween(lines, 8*s, 21*s)
	want := []cyclePhase{
		{"standby/idle", 2}, {"command/deprovision", 1}, {"deprovision/wipe-disks", 3},
		{"standby/idle", 1}, {"command/provision", 1}, {"provision/stream-image", 4}, {"handoff", 1},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	all := phasesBetween(lines, 0, 3*s)
	if fmt.Sprint(all) != fmt.Sprint([]cyclePhase{{"firmware-iso-load", 2}, {"kernel-boot", 1}}) {
		t.Fatalf("boot phases = %v", all)
	}
}

func TestCycleRegressions(t *testing.T) {
	base := cycleReport{Cycles: []cycleRun{{Name: "cold", TotalSeconds: 100}, {Name: "agent", TotalSeconds: 20}}}
	cur := cycleReport{Cycles: []cycleRun{{Name: "cold", TotalSeconds: 130}, {Name: "agent", TotalSeconds: 26}, {Name: "new", TotalSeconds: 5}}}
	p := regressionPolicy{Percent: 20, MinSeconds: 10, AgentMinSecond: 2}
	got := regressions(cur, base, p)
	if len(got) != 2 || !strings.HasPrefix(got[0], "cold cycle 130.0s vs 100.0s") || !strings.HasPrefix(got[1], "agent cycle 26.0s") {
		t.Fatalf("regressions = %q, want cold and agent", got)
	}
	// The agent cycle grew 30 % by 6 s: above its 2 s, below the cold 10 s.
	if got := regressions(cur, base, regressionPolicy{Percent: 20, MinSeconds: 10, AgentMinSecond: 10}); len(got) != 1 {
		t.Fatalf("regressions with a 10 s agent minimum = %q, want only cold", got)
	}
	// A TCG run is not compared with a KVM baseline.
	tcg := cur
	tcg.Accel = "tcg"
	base.Accel = "kvm"
	cur.Accel = "kvm"
	if got := regressions(tcg, base, p); got != nil {
		t.Fatalf("regressions across accelerators = %q, want none", got)
	}
	if got := regressions(cur, base, p); len(got) != 2 {
		t.Fatalf("regressions with the same accelerator = %q", got)
	}
	md := markdown(cur, &base)
	if !strings.Contains(md, "| total | 130.0 (+30.0) | 26.0 (+6.0) | 5.0 |") {
		t.Fatalf("markdown total row missing:\n%s", md)
	}
}
