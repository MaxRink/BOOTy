package linkcheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
	"github.com/telekom/BOOTy/pkg/network/lldp"
)

// Links of a host with three NICs: ens1f0 on the expected port, ens1f1 on
// the wrong port of the second leaf (a Junos ifIndex port ID), eno1 with
// carrier and no neighbor.
func testLinks() []Link {
	return []Link{
		{Ifname: "ens1f0np0", MAC: "3c:fd:fe:00:00:01", Carrier: true, LLDP: &lldp.Neighbor{
			ChassisID: "00:3a:9c:00:00:01", SystemName: "SITE1LSW5K10", PortID: "Ethernet1/12", PortIDSubtype: "ifname"}},
		{Ifname: "ens1f1np1", MAC: "3c:fd:fe:00:00:02", Carrier: true, LLDP: &lldp.Neighbor{
			ChassisID: "40:de:ad:00:00:01", SystemName: "SITE1LSW5K11", PortID: "514", PortIDSubtype: "local", PortDescription: "et-0/0/4"}},
		{Ifname: "eno1", MAC: "3c:fd:fe:00:00:03", Carrier: true},
		{Ifname: "eno2", MAC: "3c:fd:fe:00:00:04"}, // no carrier: needs no neighbor
	}
}

func testExpected() []config.ExpectedLink {
	return []config.ExpectedLink{
		{MAC: "3C:FD:FE:00:00:01", Switch: "site1lsw5k10", Port: "Ethernet1/12"},
		{Interface: "ens1f1np1", Switch: "site1lsw5k11", Port: "et-0/0/3"},
		{MAC: "3c:fd:fe:00:00:99", Switch: "site1lsw5k11", Port: "et-0/0/9"},
	}
}

// The diff has the bm4x ConnectionReport entries for the host side: the
// NIC on the wrong port (with the actual and the expected port) and the
// expected port without a NIC. Switch names compare without case; the
// matching NIC and the NIC without neighbor or expected port have none.
func TestDiff(t *testing.T) {
	links := testLinks()
	unmatched := matchExpected(links, testExpected())
	if links[0].Expected == nil || links[1].Expected == nil || links[2].Expected != nil || links[3].Expected != nil {
		t.Fatalf("expected cabling matched %+v", links)
	}
	got := Diff("site1com5k01", links, unmatched)
	want := []ConnectionEntry{
		{A: Endpoint{"site1com5k01", "ens1f1np1"}, BActual: Endpoint{"SITE1LSW5K11", "et-0/0/4"}, BExpected: Endpoint{"site1lsw5k11", "et-0/0/3"}},
		{A: Endpoint{"site1com5k01", "3c:fd:fe:00:00:99"}, BExpected: Endpoint{"site1lsw5k11", "et-0/0/9"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff\n got  %+v\n want %+v", got, want)
	}

	// A neighbor nobody expected is a diff entry with an empty bexpected.
	links = testLinks()
	got = Diff("h", links, matchExpected(links, testExpected()[:1]))
	if len(got) != 1 || got[0].A.InterfaceName != "ens1f1np1" || got[0].BExpected != (Endpoint{}) {
		t.Fatalf("diff with an unexpected neighbor = %+v", got)
	}
}

func findCheck(t *testing.T, results []health.CheckResult, name string) health.CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", name, results)
	return health.CheckResult{}
}

func TestCablingChecks(t *testing.T) {
	links := testLinks()
	diff := Diff("h", links, matchExpected(links, testExpected()))
	results := cablingChecks(links, diff, true, &config.CheckThresholds{})
	if r := findCheck(t, results, CheckLLDPNeighbor); r.Status != health.StatusFail || r.Severity != health.SeverityWarning || r.Details != "eno1: no LLDP neighbor" {
		t.Errorf("lldp = %+v", r)
	}
	if r := findCheck(t, results, CheckCablingMatch); r.Status != health.StatusFail || r.Severity != health.SeverityCritical ||
		!strings.Contains(r.Details, "ens1f1np1: SITE1LSW5K11 et-0/0/4, want site1lsw5k11 et-0/0/3") {
		t.Errorf("cabling = %+v", r)
	}
	if r := findCheck(t, results, CheckUplinkDualHome); r.Status != health.StatusPass {
		t.Errorf("dual homing with two chassis = %+v", r)
	}

	// Actions: off skips, block makes a missing neighbor critical; no
	// expected cabling skips the cabling match.
	th := &config.CheckThresholds{LLDPMissing: "block", CablingMismatch: "off"}
	results = cablingChecks(links, diff, true, th)
	if r := findCheck(t, results, CheckLLDPNeighbor); r.Severity != health.SeverityCritical {
		t.Errorf("lldp with block = %+v", r)
	}
	if r := findCheck(t, results, CheckCablingMatch); r.Status != health.StatusSkip {
		t.Errorf("cabling with off = %+v", r)
	}
	if r := findCheck(t, cablingChecks(links, nil, false, &config.CheckThresholds{}), CheckCablingMatch); r.Status != health.StatusSkip {
		t.Errorf("cabling without expected cabling = %+v", r)
	}

	// Both uplinks on one switch.
	links[1].LLDP.ChassisID = links[0].LLDP.ChassisID
	if r := findCheck(t, cablingChecks(links, nil, false, &config.CheckThresholds{}), CheckUplinkDualHome); r.Status != health.StatusFail {
		t.Errorf("dual homing on one chassis = %+v", r)
	}
}

func TestFinish(t *testing.T) {
	for _, tt := range []struct {
		checks []health.CheckResult
		status string
		failed bool
	}{
		{[]health.CheckResult{{Status: health.StatusPass}, {Status: health.StatusSkip, Severity: health.SeverityCritical}}, "pass", false},
		{[]health.CheckResult{{Status: health.StatusFail, Severity: health.SeverityWarning}}, "warn", false},
		{[]health.CheckResult{{Status: health.StatusFail, Severity: health.SeverityWarning}, {Status: health.StatusFail, Severity: health.SeverityCritical}}, "fail", true},
		// A later warning does not turn a failed run into a warning.
		{[]health.CheckResult{{Status: health.StatusFail, Severity: health.SeverityCritical}, {Status: health.StatusFail, Severity: health.SeverityWarning}}, "fail", true},
	} {
		d := &Document{Checks: tt.checks}
		if failed := d.Finish(time.Unix(0, 0)); failed != tt.failed || d.Status != tt.status {
			t.Errorf("%+v: status %s failed %v, want %s %v", tt.checks, d.Status, failed, tt.status, tt.failed)
		}
	}
}

type fakeReporter struct {
	result []byte
	checks []health.CheckResult
	err    error
}

func (f *fakeReporter) ReportCheckResult(_ context.Context, data []byte) error {
	f.result = data
	return f.err
}

func (f *fakeReporter) ReportHealthChecks(_ context.Context, r []health.CheckResult) error {
	f.checks = r
	return f.err
}

// Every sink gets the document; the bm4x report has the JSON names of the
// bm4x ConnectionReport; a failing sink does not stop the others.
func TestDeliver(t *testing.T) {
	dir := t.TempDir()
	origResult, origReport := ResultPath, BM4XReportPath
	t.Cleanup(func() { ResultPath, BM4XReportPath = origResult, origReport })
	ResultPath, BM4XReportPath = filepath.Join(dir, "r", "check-result.json"), filepath.Join(dir, "bm4x.json")

	doc := &Document{Schema: Schema, Status: "fail", Checks: []health.CheckResult{{Name: "x", Status: health.StatusFail}},
		Diff: []ConnectionEntry{{A: Endpoint{"h", "ens1f1np1"}, BActual: Endpoint{"leaf2", "et-0/0/4"}, BExpected: Endpoint{"leaf2", "et-0/0/3"}}}}
	cfg := &config.CheckConfig{Sinks: []string{"http", "file", "bm4x-report", "caprf"}}
	r := &fakeReporter{err: errors.New("unreachable")}
	Deliver(context.Background(), doc, cfg, r, slog.New(slog.NewTextHandler(io.Discard, nil)))

	b, err := os.ReadFile(ResultPath)
	var back Document
	if err != nil || json.Unmarshal(b, &back) != nil || back.Status != "fail" || len(back.Diff) != 1 {
		t.Fatalf("result file %s: %v", b, err)
	}
	b, err = os.ReadFile(BM4XReportPath)
	want := `{"diff":[{"a":{"hostname":"h","interfaceName":"ens1f1np1"},"bactual":{"hostname":"leaf2","interfaceName":"et-0/0/4"},"bexpected":{"hostname":"leaf2","interfaceName":"et-0/0/3"}}]}`
	if err != nil || string(b) != want {
		t.Fatalf("bm4x report\n got  %s (%v)\n want %s", b, err, want)
	}
	if len(r.result) == 0 || len(r.checks) != 1 {
		t.Fatalf("reporter got result %d bytes, %d checks", len(r.result), len(r.checks))
	}
}

// Without any expected cabling there is no diff, also not for the links
// with a neighbor; one NIC on one switch is not a dual-homing problem.
func TestDiffWithoutExpectedCabling(t *testing.T) {
	links := []Link{
		{Ifname: "eth0", Carrier: true, LLDP: &lldp.Neighbor{SystemName: "leaf01", ChassisID: "c1", PortDescription: "Ethernet1/1"}},
		{Ifname: "eth1", Carrier: true, LLDP: &lldp.Neighbor{SystemName: "leaf02", ChassisID: "c2", PortDescription: "Ethernet1/1"}},
	}
	if d := Diff("host1", links, nil); d != nil {
		t.Fatalf("diff without expected cabling = %+v", d)
	}
	r := findCheck(t, cablingChecks(links[:1], nil, false, &config.CheckThresholds{}), CheckUplinkDualHome)
	if r.Status != health.StatusPass {
		t.Fatalf("one NIC: %+v", r)
	}
}
