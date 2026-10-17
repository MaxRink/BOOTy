// Package telemetry provides provisioning metrics, step timing, and event emission.
package telemetry

import (
	"errors"
	"log/slog"
	"sync"
	"time"
)

// StepStatus is the status of a provisioning step.
type StepStatus string

// Step status constants.
const (
	StatusPending StepStatus = "pending"
	StatusRunning StepStatus = "running"
	StatusDone    StepStatus = "done"
	StatusFailed  StepStatus = "failed"
	StatusSkipped StepStatus = "skipped"
)

// StepRecord records execution details for a provisioning step.
type StepRecord struct {
	Name      string        `json:"name"`
	Status    StepStatus    `json:"status"`
	StartTime time.Time     `json:"startTime,omitempty"`
	EndTime   time.Time     `json:"endTime,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
	Retries   int           `json:"retries,omitempty"`
	Error     string        `json:"error,omitempty"`
	Code      string        `json:"code,omitempty"`
}

// codedError is an error that carries a stable failure code
// (pkg/provision.CodedError).
type codedError interface {
	error
	FailureCode() string
}

// failureCode returns the failure code that err carries, or "".
func failureCode(err error) string {
	if coded, ok := errors.AsType[codedError](err); ok {
		return coded.FailureCode()
	}
	return ""
}

// StepTracker tracks provisioning step execution.
type StepTracker struct {
	mu      sync.Mutex
	steps   []StepRecord
	metrics *Metrics
	log     *slog.Logger
}

// NewStepTracker creates a new step tracker.
func NewStepTracker(metrics *Metrics, log *slog.Logger) *StepTracker {
	return &StepTracker{
		metrics: metrics,
		log:     log,
	}
}

// StartStep marks a step as running.
func (t *StepTracker) StartStep(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.steps = append(t.steps, StepRecord{
		Name:      name,
		Status:    StatusRunning,
		StartTime: time.Now(),
	})

	if t.log != nil {
		t.log.Info("step started", "step", name)
	}
}

// EndStep marks the current step as done or failed.
func (t *StepTracker) EndStep(name string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := len(t.steps) - 1; i >= 0; i-- {
		if t.steps[i].Name != name || t.steps[i].Status != StatusRunning {
			continue
		}
		t.steps[i].EndTime = time.Now()
		t.steps[i].Duration = t.steps[i].EndTime.Sub(t.steps[i].StartTime)

		if err != nil {
			t.steps[i].Status = StatusFailed
			t.steps[i].Error = err.Error()
			t.steps[i].Code = failureCode(err)
			if t.metrics != nil {
				t.metrics.StepErrors.Inc()
			}
		} else {
			t.steps[i].Status = StatusDone
		}

		if t.metrics != nil {
			t.metrics.StepDuration.Observe(t.steps[i].Duration.Seconds())
		}

		if t.log != nil {
			t.log.Info("step ended",
				"step", name,
				"status", string(t.steps[i].Status),
				"duration", t.steps[i].Duration)
		}
		return
	}

	if t.log != nil {
		t.log.Warn("step end called for unknown or non-running step", "step", name)
	}
}

// SkipStep records a skipped step.
func (t *StepTracker) SkipStep(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, StepRecord{
		Name:   name,
		Status: StatusSkipped,
	})
}

// RecordRetry increments the retry count for a running step.
func (t *StepTracker) RecordRetry(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := len(t.steps) - 1; i >= 0; i-- {
		if t.steps[i].Name != name || t.steps[i].Status != StatusRunning {
			continue
		}
		t.steps[i].Retries++
		if t.metrics != nil {
			t.metrics.StepRetries.Inc()
		}
		return
	}

	if t.log != nil {
		t.log.Warn("retry recorded for unknown step", "step", name)
	}
}

// Steps returns a copy of all step records.
func (t *StepTracker) Steps() []StepRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]StepRecord, len(t.steps))
	copy(out, t.steps)
	return out
}

// TotalDuration returns the sum of durations across all steps
// (including zero-duration for pending or skipped steps).
func (t *StepTracker) TotalDuration() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var total time.Duration
	for i := range t.steps {
		total += t.steps[i].Duration
	}
	return total
}
