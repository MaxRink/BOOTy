package image

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/watchdog"
)

func TestWriteCounter(t *testing.T) {
	wc := &WriteCounter{}
	data := []byte("hello world")
	n, err := wc.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected %d bytes written, got %d", len(data), n)
	}
	if wc.Total.Load() != uint64(len(data)) {
		t.Errorf("expected Total=%d, got %d", len(data), wc.Total.Load())
	}

	// Write more data
	n2, err := wc.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n2 != len(data) {
		t.Errorf("expected %d bytes written, got %d", len(data), n2)
	}
	if wc.Total.Load() != uint64(2*len(data)) {
		t.Errorf("expected Total=%d, got %d", 2*len(data), wc.Total.Load())
	}
}

// progressCounter counts watchdog progress reports.
type progressCounter struct{ n atomic.Int32 }

func (p *progressCounter) Step(string)        {}
func (p *progressCounter) Progress()          { p.n.Add(1) }
func (p *progressCounter) Idle()              {}
func (p *progressCounter) Handoff(bool) error { return nil }

// Written image bytes are watchdog progress; a stalled download is not.
func TestProgressTickerReportsWatchdogProgress(t *testing.T) {
	oldInterval := progressTickInterval
	progressTickInterval = 5 * time.Millisecond
	t.Cleanup(func() { progressTickInterval = oldInterval })
	p := &progressCounter{}
	watchdog.Activate(p)
	t.Cleanup(func() { watchdog.Activate(nil) })

	counter := &WriteCounter{}
	stop := startProgressTicker(counter)
	defer stop()
	waitFor := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("progress reports = %d", p.n.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}
	_, _ = counter.Write(make([]byte, 4096))
	waitFor(func() bool { return p.n.Load() == 1 })
	// No new bytes: several ticks without a report.
	time.Sleep(50 * time.Millisecond)
	if n := p.n.Load(); n != 1 {
		t.Fatalf("progress reports without new bytes = %d, want 1", n)
	}
	_, _ = counter.Write(make([]byte, 4096))
	waitFor(func() bool { return p.n.Load() == 2 })
}
