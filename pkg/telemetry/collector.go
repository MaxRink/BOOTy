package telemetry

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// StepMetrics captures timing and status for a single provisioning step.
type StepMetrics struct {
	Name       string `json:"name"`
	DurationMs int64  `json:"durationMs"`
	// Status is a StepStatus: done, failed, skipped or running.
	Status  string `json:"status"`
	Retries int    `json:"retries,omitempty"`
	Error   string `json:"error,omitempty"`
	// Code is the stable failure code (BOOTY-<AREA>-<NNN>) of a failed step.
	Code string `json:"code,omitempty"`
}

// ImageMetrics captures image download performance.
type ImageMetrics struct {
	URL           string  `json:"url"`
	SizeBytes     int64   `json:"sizeBytes"`
	DurationMs    int64   `json:"durationMs"`
	SpeedMBps     float64 `json:"speedMBps"`
	ChecksumMatch bool    `json:"checksumMatch"`
}

// DiskMetrics captures disk write performance.
type DiskMetrics struct {
	DevicePath   string  `json:"devicePath"`
	WrittenBytes int64   `json:"writtenBytes"`
	DurationMs   int64   `json:"durationMs"`
	SpeedMBps    float64 `json:"speedMBps"`
}

// Summary holds all metrics collected during a provisioning run.
type Summary struct {
	StartTime  time.Time     `json:"startTime"`
	EndTime    time.Time     `json:"endTime"`
	TotalDurMs int64         `json:"totalDurationMs"`
	Steps      []StepMetrics `json:"steps"`
	Image      *ImageMetrics `json:"image,omitempty"`
	Disk       *DiskMetrics  `json:"disk,omitempty"`
}

// Collector accumulates metrics during provisioning.
type Collector struct {
	mu        sync.Mutex
	startTime time.Time
	steps     *StepTracker
	image     *ImageMetrics
	disk      *DiskMetrics
}

// NewCollector creates a Collector and records the start time.
func NewCollector() *Collector {
	return &Collector{startTime: time.Now(), steps: NewStepTracker(nil, nil)}
}

// Steps returns the step tracker. It is the only source of the step data
// in the summary.
func (c *Collector) Steps() *StepTracker {
	return c.steps
}

// RecordImage records image download metrics.
func (c *Collector) RecordImage(url string, sizeBytes int64, duration time.Duration, checksumOK bool) {
	speedMBps := 0.0
	if duration > 0 && sizeBytes > 0 {
		speedMBps = float64(sizeBytes) / duration.Seconds() / (1024 * 1024)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.image = &ImageMetrics{
		URL: url, SizeBytes: sizeBytes, DurationMs: duration.Milliseconds(),
		SpeedMBps: speedMBps, ChecksumMatch: checksumOK,
	}
}

// RecordDisk records disk write metrics.
func (c *Collector) RecordDisk(devicePath string, writtenBytes int64, duration time.Duration) {
	speedMBps := 0.0
	if duration > 0 && writtenBytes > 0 {
		speedMBps = float64(writtenBytes) / duration.Seconds() / (1024 * 1024)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disk = &DiskMetrics{
		DevicePath: devicePath, WrittenBytes: writtenBytes,
		DurationMs: duration.Milliseconds(), SpeedMBps: speedMBps,
	}
}

// Summarize returns the collected metrics as a Summary.
// Returns copies of internal state to prevent aliasing.
func (c *Collector) Summarize() Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	records := c.steps.Steps()
	steps := make([]StepMetrics, len(records))
	for i := range records {
		r := &records[i]
		steps[i] = StepMetrics{
			Name: r.Name, DurationMs: r.Duration.Milliseconds(), Status: string(r.Status),
			Retries: r.Retries, Error: r.Error, Code: r.Code,
		}
	}
	s := Summary{
		StartTime: c.startTime, EndTime: now, TotalDurMs: now.Sub(c.startTime).Milliseconds(),
		Steps: steps,
	}
	if c.image != nil {
		img := *c.image
		s.Image = &img
	}
	if c.disk != nil {
		d := *c.disk
		s.Disk = &d
	}
	return s
}

// MarshalSummary returns the metrics summary as JSON bytes.
func (c *Collector) MarshalSummary() ([]byte, error) {
	s := c.Summarize()
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshaling metrics: %w", err)
	}
	return data, nil
}

// Fixes issue #441
