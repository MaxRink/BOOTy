package telemetry

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type codedErr struct{ code string }

func (e codedErr) Error() string       { return "coded" }
func (e codedErr) FailureCode() string { return e.code }

func TestSummaryStepsFromTracker(t *testing.T) {
	c := NewCollector()
	tr := c.Steps()
	tr.SkipStep("report-init")
	tr.StartStep("detect-disk")
	tr.RecordRetry("detect-disk")
	tr.EndStep("detect-disk", nil)
	tr.StartStep("mount-root")
	tr.EndStep("mount-root", fmt.Errorf("mount: %w", codedErr{"BOOTY-DSK-016"}))

	s := c.Summarize()
	want := []StepMetrics{
		{Name: "report-init", Status: "skipped"},
		{Name: "detect-disk", Status: "done", Retries: 1},
		{Name: "mount-root", Status: "failed", Error: "mount: coded", Code: "BOOTY-DSK-016"},
	}
	if len(s.Steps) != len(want) {
		t.Fatalf("steps = %+v, want %+v", s.Steps, want)
	}
	for i := range want {
		got := s.Steps[i]
		got.DurationMs = 0
		if got != want[i] {
			t.Errorf("step %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestStepErrorWithoutCode(t *testing.T) {
	c := NewCollector()
	c.Steps().StartStep("mount-root")
	c.Steps().EndStep("mount-root", fmt.Errorf("mount failed"))
	if got := c.Summarize().Steps[0]; got.Status != "failed" || got.Code != "" {
		t.Errorf("step = %+v, want failed without code", got)
	}
}

func TestRecordImage(t *testing.T) {
	c := NewCollector()
	c.RecordImage("http://example.com/image.gz", 1024*1024*100, 10*time.Second, true)
	s := c.Summarize()
	if s.Image == nil {
		t.Fatal("image metrics nil")
	}
	if s.Image.SpeedMBps < 9 || s.Image.SpeedMBps > 11 {
		t.Errorf("speed = %f, want ~10", s.Image.SpeedMBps)
	}
}

func TestRecordDisk(t *testing.T) {
	c := NewCollector()
	c.RecordDisk("/dev/sda", 1024*1024*500, 5*time.Second)
	s := c.Summarize()
	if s.Disk == nil {
		t.Fatal("disk metrics nil")
	}
	if s.Disk.SpeedMBps < 99 || s.Disk.SpeedMBps > 101 {
		t.Errorf("speed = %f, want ~100", s.Disk.SpeedMBps)
	}
}

func TestMarshalSummary(t *testing.T) {
	c := NewCollector()
	c.Steps().StartStep("test")
	c.Steps().EndStep("test", codedErr{"BOOTY-RUN-001"})
	data, err := c.MarshalSummary()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status":"failed","error":"coded","code":"BOOTY-RUN-001"`) {
		t.Errorf("summary JSON %s has no step code", data)
	}
}

func TestSummarize_NoPointerAliasing(t *testing.T) {
	c := NewCollector()
	c.RecordImage("http://example.com/img.gz", 1024, time.Second, true)
	c.RecordDisk("/dev/sda", 2048, 2*time.Second)

	s1 := c.Summarize()
	s2 := c.Summarize()

	// Mutating the returned summary should not affect future calls.
	s1.Image.SizeBytes = 999
	s1.Disk.WrittenBytes = 999

	if s2.Image.SizeBytes == 999 {
		t.Error("Summarize returned aliased Image pointer")
	}
	if s2.Disk.WrittenBytes == 999 {
		t.Error("Summarize returned aliased Disk pointer")
	}
}
