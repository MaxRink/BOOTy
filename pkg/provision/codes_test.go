//go:build linux

package provision

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/telemetry"
	"github.com/telekom/BOOTy/pkg/timesync"
)

var updateDocs = flag.Bool("update", false, "rewrite docs/failure-codes.md")

var failureCodePattern = regexp.MustCompile(`^BOOTY-[A-Z]{2,3}-[0-9]{3}$`)

func TestFailureCodesAreUniqueAndComplete(t *testing.T) {
	classes := []FailureClass{ClassTransient, ClassPermanent, ClassConfig, ClassHardware, ClassEnvironment}
	seen := make(map[string]bool)
	for _, c := range FailureCodes() {
		if !failureCodePattern.MatchString(c.Code) {
			t.Errorf("code %q does not match BOOTY-<AREA>-<NNN>", c.Code)
		}
		if seen[c.Code] {
			t.Errorf("duplicate code %s", c.Code)
		}
		seen[c.Code] = true
		if !containsClass(classes, c.Class) {
			t.Errorf("%s: unknown class %q", c.Code, c.Class)
		}
		if c.Step == "" || c.Text == "" {
			t.Errorf("%s: step and text must be set", c.Code)
		}
	}
	for _, code := range []string{CodeGeneric, CodeCanceled, CodeNetworkSetup, CodeConnectivity, CodeAuth, CodeConfig, CodeNoTime, CodeCertTime} {
		if !seen[code] {
			t.Errorf("constant %s is not in the catalog", code)
		}
	}
	for _, s := range sentinelCodes {
		if !seen[s.code] {
			t.Errorf("sentinel code %s is not in the catalog", s.code)
		}
	}
}

func containsClass(classes []FailureClass, c FailureClass) bool {
	for _, x := range classes {
		if x == c {
			return true
		}
	}
	return false
}

// Every step of every step list has its own code, so a failure never falls
// back to the generic code by accident.
func TestEveryStepHasFailureCode(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	lists := make([][]Step, 0, 4)
	lists = append(lists, o.provisionSteps())
	for _, mode := range []string{"hard", "soft", "decommission"} {
		lists = append(lists, o.deprovisionSteps(mode))
	}
	for _, steps := range lists {
		for _, step := range steps {
			code := codeFor(step.Name, errors.New("boom"))
			if code == CodeGeneric {
				t.Errorf("step %s has no failure code", step.Name)
				continue
			}
			if got := stepOfCode(code); got != step.Name {
				t.Errorf("step %s maps to %s of step %s", step.Name, code, got)
			}
		}
	}
}

func stepOfCode(code string) string {
	for _, c := range failureCodes {
		if c.Code == code {
			return c.Step
		}
	}
	return ""
}

func TestWithCode(t *testing.T) {
	tests := []struct {
		name string
		step string
		err  error
		want string
	}{
		{"step code", "detect-disk", errors.New("no disk"), "BOOTY-DSK-006"},
		{"unknown step", "no-such-step", errors.New("x"), CodeGeneric},
		{"checksum sentinel wins", "stream-image", fmt.Errorf("copy: %w", image.ErrChecksumMismatch), "BOOTY-IMG-003"},
		{"format probe", "stream-image", image.ErrFormatProbe, "BOOTY-IMG-004"},
		{"all disks in use", "wipe-disks", disk.ErrAllDisksInUse, "BOOTY-DSK-010"},
		{"in-use check", "decommission-wipe", disk.ErrInUseCheck, "BOOTY-DSK-010"},
		{"ambiguous root", "parse-partitions", disk.ErrAmbiguousRootPartition, "BOOTY-DSK-012"},
		{"efi vars", "remove-efi-entries", ErrEFIVarsUnavailable, "BOOTY-EFI-002"},
		{"canceled", "stream-image", fmt.Errorf("retry canceled: %w", context.Canceled), CodeCanceled},
		{"deadline", "report-init", context.DeadlineExceeded, CodeCanceled},
		{"existing code wins", "detect-disk", &CodedError{Code: "BOOTY-NET-001", Err: errors.New("x")}, "BOOTY-NET-001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := WithCode(tc.step, tc.err)
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("CodeOf(WithCode(%s, %v)) = %q, want %q", tc.step, tc.err, got, tc.want)
			}
			if err.Error() != tc.err.Error() {
				t.Fatalf("Error() = %q, want the wrapped text %q", err.Error(), tc.err.Error())
			}
			if !errors.Is(err, tc.err) {
				t.Fatal("coded error must unwrap to the original error")
			}
		})
	}
	if WithCode("detect-disk", nil) != nil {
		t.Fatal("WithCode(nil) must stay nil")
	}
	if CodeOf(errors.New("plain")) != "" {
		t.Fatal("CodeOf(plain error) must be empty")
	}
}

type metricsProvider struct {
	mockProvider
	metrics []byte
}

func (p *metricsProvider) ReportMetrics(_ context.Context, data []byte) error {
	p.metrics = data
	return nil
}

func lastSummary(t *testing.T, p *metricsProvider) telemetry.Summary {
	t.Helper()
	var s telemetry.Summary
	if err := json.Unmarshal(p.metrics, &s); err != nil {
		t.Fatalf("metrics summary %q: %v", p.metrics, err)
	}
	return s
}

// A resumed run skips report-init and fails in release-stale-mounts: the
// ERROR_URL body and the METRICS_URL summary carry the code, and the summary
// has every step the run reached.
func TestProvisionFailureCodeInErrorBodyAndSummary(t *testing.T) {
	oldUnmount, oldLoad := unmountTargetTree, loadCheckpointFn
	t.Cleanup(func() { unmountTargetTree, loadCheckpointFn = oldUnmount, oldLoad })
	unmountTargetTree = func(*disk.Manager, string) error { return errors.New("umount /newroot: device busy") }
	t.Setenv("BOOTY_RESUME", "true")
	loadCheckpointFn = func() (*Checkpoint, error) {
		return &Checkpoint{CompletedSteps: []string{"report-init"}}, nil
	}

	provider := &metricsProvider{}
	o := newTestOrchestrator(t, &config.MachineConfig{}, &provider.mockProvider)
	o.provider = provider
	err := o.Provision(context.Background())
	if CodeOf(err) != "BOOTY-DSK-001" {
		t.Fatalf("Provision() code = %q (%v), want BOOTY-DSK-001", CodeOf(err), err)
	}
	if len(provider.statuses) != 1 || provider.statuses[0].status != config.StatusError {
		t.Fatalf("statuses = %+v, want one error", provider.statuses)
	}
	wantBody := "step release-stale-mounts failed [BOOTY-DSK-001]: unmounting target root"
	if !strings.HasPrefix(provider.statuses[0].message, wantBody) {
		t.Fatalf("error body = %q, want prefix %q", provider.statuses[0].message, wantBody)
	}

	s := lastSummary(t, provider)
	want := []telemetry.StepMetrics{
		{Name: "report-init", Status: "skipped"},
		{Name: "release-stale-mounts", Status: "failed", Code: "BOOTY-DSK-001"},
	}
	if len(s.Steps) != len(want) {
		t.Fatalf("summary steps = %+v, want %+v", s.Steps, want)
	}
	for i, w := range want {
		got := s.Steps[i]
		if got.Name != w.Name || got.Status != w.Status || got.Code != w.Code {
			t.Errorf("step %d = %+v, want %+v", i, got, w)
		}
	}
	if !strings.Contains(s.Steps[1].Error, "device busy") {
		t.Errorf("failed step error = %q", s.Steps[1].Error)
	}
}

// executeStep records every executed step with its retries in the summary.
func TestExecuteStepFeedsStepTracker(t *testing.T) {
	DefaultPolicies["test-retry"] = RetryPolicy{MaxRetries: 3, Transient: true}
	t.Cleanup(func() { delete(DefaultPolicies, "test-retry") })

	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	cp := &Checkpoint{}
	calls := 0
	steps := []Step{
		{"set-hostname", func(context.Context) error { return nil }},
		{"test-retry", func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("flaky")
			}
			return nil
		}},
		{"detect-disk", func(context.Context) error {
			time.Sleep(time.Millisecond)
			return errors.New("no disk")
		}},
	}
	for _, step := range steps {
		_ = o.executeStep(context.Background(), step, cp)
	}

	s := o.collector.Summarize()
	if len(s.Steps) != len(steps) {
		t.Fatalf("summary steps = %+v, want %d", s.Steps, len(steps))
	}
	for i, step := range steps {
		if s.Steps[i].Name != step.Name {
			t.Errorf("step %d = %s, want %s", i, s.Steps[i].Name, step.Name)
		}
	}
	if s.Steps[0].Status != "done" || s.Steps[0].Code != "" {
		t.Errorf("set-hostname = %+v, want done without code", s.Steps[0])
	}
	if s.Steps[1].Status != "done" || s.Steps[1].Retries != 2 {
		t.Errorf("test-retry = %+v, want done with 2 retries", s.Steps[1])
	}
	if s.Steps[2].Status != "failed" || s.Steps[2].Code != "BOOTY-DSK-006" || s.Steps[2].DurationMs < 1 {
		t.Errorf("detect-disk = %+v, want failed, BOOTY-DSK-006, duration >= 1ms", s.Steps[2])
	}
}

func TestDeprovisionFailureCodeInErrorBodyAndSummary(t *testing.T) {
	provider := &metricsProvider{}
	cfg := &config.MachineConfig{Mode: "soft"}
	o := newTestOrchestrator(t, cfg, &provider.mockProvider)
	o.provider = provider
	provider.reportStatusErrs = []error{errors.New("init callback down")}

	err := o.Deprovision(context.Background())
	if CodeOf(err) != "BOOTY-RPT-001" {
		t.Fatalf("Deprovision() code = %q (%v), want BOOTY-RPT-001", CodeOf(err), err)
	}
	last := provider.statuses[len(provider.statuses)-1]
	if last.status != config.StatusError || !strings.HasPrefix(last.message, "step report-init failed [BOOTY-RPT-001]: ") {
		t.Fatalf("error report = %+v", last)
	}
	s := lastSummary(t, provider)
	if len(s.Steps) != 1 || s.Steps[0].Name != "report-init" || s.Steps[0].Code != "BOOTY-RPT-001" {
		t.Fatalf("summary steps = %+v", s.Steps)
	}
}

const failureCodesDoc = "../../docs/failure-codes.md"

// TestFailureCodeCatalogDoc keeps docs/failure-codes.md equal to the table.
// Run with -update after a change to the table.
func TestFailureCodeCatalogDoc(t *testing.T) {
	got := renderFailureCodeDoc()
	if *updateDocs {
		if err := os.WriteFile(failureCodesDoc, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(failureCodesDoc)
	if err != nil {
		t.Fatalf("read %s (run with -update): %v", failureCodesDoc, err)
	}
	if string(want) != got {
		t.Fatalf("%s is out of date: go test ./pkg/provision -run TestFailureCodeCatalogDoc -update", failureCodesDoc)
	}
}

func renderFailureCodeDoc() string {
	var b strings.Builder
	b.WriteString(`# Failure codes

<!-- Generated from pkg/provision/codes.go by TestFailureCodeCatalogDoc. Do not edit. -->

BOOTy attaches a stable code ` + "`BOOTY-<AREA>-<NNN>`" + ` to every step failure.
The code is in the ` + "`ERROR_URL`" + ` body
(` + "`step <name> failed [<code>]: <error>`" + `) and in the failed step of the
` + "`METRICS_URL`" + ` summary. A code never changes its meaning. A retired
code stays reserved.

A known error (for example a checksum mismatch) gets its own code in every
step (step ` + "`*`" + `). Any other error gets the code of its step.

| Code | Class | Step | Meaning |
|------|-------|------|---------|
`)
	for _, c := range failureCodes {
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s |\n", c.Code, c.Class, c.Step, c.Text)
	}
	return b.String()
}

// The time sync errors keep their codes in every step, also behind
// wrappers such as the redacted image download error.
func TestTimeSyncFailureCodes(t *testing.T) {
	skew := timesync.ExplainCertTime(x509.CertificateInvalidError{
		Cert: &x509.Certificate{NotBefore: time.Now().Add(time.Hour), NotAfter: time.Now().Add(2 * time.Hour)}, Reason: x509.Expired,
	}, time.Now())
	if got := CodeOf(WithCode("stream-image", fmt.Errorf("fetching image: %w", skew))); got != CodeCertTime {
		t.Errorf("clock skew in stream-image: code %q, want %s", got, CodeCertTime)
	}
	if got := CodeOf(WithCode("report-init", fmt.Errorf("post: %w", timesync.ErrClockSkew))); got != CodeCertTime {
		t.Errorf("clock skew sentinel in report-init: code %q, want %s", got, CodeCertTime)
	}
	if got := CodeOf(WithCode("report-init", timesync.ErrNoTime)); got != CodeNoTime {
		t.Errorf("no time: code %q, want %s", got, CodeNoTime)
	}
}
