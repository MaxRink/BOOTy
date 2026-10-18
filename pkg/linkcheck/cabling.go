package linkcheck

import (
	"fmt"
	"slices"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
)

// Check names.
const (
	CheckLLDPNeighbor   = "lldp-neighbor-present"
	CheckCablingMatch   = "cabling-match"
	CheckUplinkDualHome = "uplink-dual-homed"
)

// action returns a threshold action, or def when it is not set.
func action(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// result turns the failures of a check into its result: skipped when the
// action is off, failed with a warning or critical severity otherwise.
func result(name, act string, failures []string) health.CheckResult {
	r := health.CheckResult{Name: name, Status: health.StatusPass, Severity: health.SeverityWarning}
	if act == config.CheckActionBlock {
		r.Severity = health.SeverityCritical
	}
	switch {
	case act == config.CheckActionOff:
		r.Status, r.Severity, r.Message = health.StatusSkip, health.SeverityInfo, "off in check.thresholds"
	case len(failures) > 0:
		r.Status = health.StatusFail
		r.Message = fmt.Sprintf("%d problem(s)", len(failures))
		r.Details = strings.Join(failures, "; ")
	}
	return r
}

// matchExpected sets the expected cabling of each link, by MAC or by
// interface name, and returns the expected links that match no NIC.
func matchExpected(links []Link, expected []config.ExpectedLink) []config.ExpectedLink {
	var unmatched []config.ExpectedLink
	for i := range expected {
		e := &expected[i]
		found := false
		for j := range links {
			if e.MAC != "" && strings.EqualFold(e.MAC, links[j].MAC) || e.MAC == "" && e.Interface == links[j].Ifname {
				links[j].Expected, found = e, true
			}
		}
		if !found {
			unmatched = append(unmatched, *e)
		}
	}
	return unmatched
}

// actual is the switch port that LLDP shows for a link, or none.
func actual(l *Link) Endpoint {
	if l.LLDP == nil {
		return Endpoint{}
	}
	return Endpoint{Hostname: l.LLDP.SystemName, InterfaceName: l.LLDP.SwitchPort()}
}

// Diff compares the LLDP neighbors with the expected cabling, like the
// bm4x connection check from the host side: one entry per NIC whose
// switch port differs from the expected one, per NIC with a neighbor but
// no expected port, and per expected port without a NIC. Switch names
// compare without case. Without any expected cabling there is nothing to
// compare, so the diff is empty.
func Diff(host string, links []Link, unmatched []config.ExpectedLink) []ConnectionEntry {
	if len(unmatched) == 0 && !slices.ContainsFunc(links, func(l Link) bool { return l.Expected != nil }) {
		return nil
	}
	var diff []ConnectionEntry
	for i := range links {
		l := &links[i]
		got := actual(l)
		var want Endpoint
		if l.Expected != nil {
			want = Endpoint{Hostname: l.Expected.Switch, InterfaceName: l.Expected.Port}
		} else if l.LLDP == nil {
			continue
		}
		if !strings.EqualFold(got.Hostname, want.Hostname) || got.InterfaceName != want.InterfaceName {
			diff = append(diff, ConnectionEntry{A: Endpoint{Hostname: host, InterfaceName: l.Ifname}, BActual: got, BExpected: want})
		}
	}
	for _, e := range unmatched {
		name := e.Interface
		if name == "" {
			name = e.MAC
		}
		diff = append(diff, ConnectionEntry{A: Endpoint{Hostname: host, InterfaceName: name},
			BExpected: Endpoint{Hostname: e.Switch, InterfaceName: e.Port}})
	}
	return diff
}

// cablingChecks runs the LLDP and cabling checks.
func cablingChecks(links []Link, diff []ConnectionEntry, haveExpected bool, t *config.CheckThresholds) []health.CheckResult {
	var missing []string
	chassis := map[string]bool{}
	var withNeighbor []string
	for i := range links {
		l := &links[i]
		if l.Carrier && l.LLDP == nil {
			missing = append(missing, l.Ifname+": no LLDP neighbor")
		}
		if l.LLDP != nil {
			chassis[l.LLDP.ChassisID] = true
			withNeighbor = append(withNeighbor, l.Ifname)
		}
	}
	results := []health.CheckResult{result(CheckLLDPNeighbor, action(t.LLDPMissing, config.CheckActionWarn), missing)}

	mismatch := make([]string, 0, len(diff))
	for _, d := range diff {
		mismatch = append(mismatch, fmt.Sprintf("%s: %s %s, want %s %s", d.A.InterfaceName,
			orNone(d.BActual.Hostname), d.BActual.InterfaceName, orNone(d.BExpected.Hostname), d.BExpected.InterfaceName))
	}
	r := result(CheckCablingMatch, action(t.CablingMismatch, config.CheckActionBlock), mismatch)
	if !haveExpected && r.Status != health.StatusSkip {
		r.Status, r.Severity, r.Message = health.StatusSkip, health.SeverityInfo, "no expected cabling"
	}
	results = append(results, r)

	var single []string
	if len(withNeighbor) > 1 && len(chassis) == 1 {
		single = append(single, fmt.Sprintf("%s all reach one switch", strings.Join(withNeighbor, ", ")))
	}
	return append(results, result(CheckUplinkDualHome, action(t.DualHoming, config.CheckActionWarn), single))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
