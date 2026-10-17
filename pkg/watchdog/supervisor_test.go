package watchdog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDevice counts keepalives and records the handoff.
type fakeDevice struct {
	mu         sync.Mutex
	timeout    time.Duration
	keepalives int
	stopped    bool
	released   bool
	petErr     error
}

func (d *fakeDevice) Name() string           { return "fake" }
func (d *fakeDevice) Timeout() time.Duration { return d.timeout }

func (d *fakeDevice) Keepalive() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.petErr != nil {
		return d.petErr
	}
	d.keepalives++
	return nil
}

func (d *fakeDevice) Stop() error    { d.mu.Lock(); defer d.mu.Unlock(); d.stopped = true; return nil }
func (d *fakeDevice) Release() error { d.mu.Lock(); defer d.mu.Unlock(); d.released = true; return nil }

func (d *fakeDevice) pets() int { d.mu.Lock(); defer d.mu.Unlock(); return d.keepalives }

// testClock is a manual clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newTestSupervisor(dev Device, clock *testClock, budgets map[string]time.Duration) *Supervisor {
	return NewSupervisor(dev, Options{
		Budgets: budgets,
		Now:     clock.Now,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestSupervisorPetsWhileStepsProgress(t *testing.T) {
	dev := &fakeDevice{timeout: 300 * time.Second}
	clock := &testClock{now: time.Unix(1_000_000, 0)}
	s := newTestSupervisor(dev, clock, nil)
	if s.Interval() != 100*time.Second {
		t.Fatalf("interval = %v, want a third of the timeout", s.Interval())
	}

	// Outside a step BOOTy pets for as long as it runs.
	clock.Advance(time.Hour)
	if !s.Tick() {
		t.Fatal("no keepalive outside a step")
	}
	s.Step("detect-disk")
	clock.Advance(299 * time.Second)
	if !s.Tick() {
		t.Fatal("no keepalive within the step budget")
	}
	// The step hangs: no progress for longer than the budget.
	clock.Advance(2 * time.Second)
	for range 2 {
		if s.Tick() {
			t.Fatal("keepalive for a stalled step")
		}
	}
	// Progress resumes before the hardware fired.
	s.Progress()
	if !s.Tick() {
		t.Fatal("no keepalive after the progress resumed")
	}
	s.Step("partprobe")
	clock.Advance(10 * time.Minute)
	if s.Tick() {
		t.Fatal("keepalive for a stalled second step")
	}
	s.Idle()
	clock.Advance(time.Hour)
	if !s.Tick() || dev.pets() != 4 {
		t.Fatalf("keepalives = %d, want 4", dev.pets())
	}
}

func TestSupervisorStepBudgets(t *testing.T) {
	dev := &fakeDevice{timeout: 60 * time.Second}
	clock := &testClock{now: time.Unix(1_000_000, 0)}
	s := newTestSupervisor(dev, clock, map[string]time.Duration{"run-machine-commands": 5 * time.Minute, "custom": 90 * time.Second})

	for _, tt := range []struct {
		step    string
		idle    time.Duration
		wantPet bool
	}{
		{"wipe-disks", 47 * time.Hour, true}, // built-in 48 h
		{"wipe-disks", 49 * time.Hour, false},
		{"setup-raid", 50 * time.Minute, true}, // built-in 1 h, longer than the timeout
		{"setup-raid", 61 * time.Minute, false},
		{"run-machine-commands", 4 * time.Minute, true}, // override of the built-in 1 h
		{"run-machine-commands", 6 * time.Minute, false},
		{"custom", 80 * time.Second, true},
		{"custom", 100 * time.Second, false},
		{"detect-disk", 59 * time.Second, true}, // default: the device timeout
		{"detect-disk", 61 * time.Second, false},
	} {
		s.Step(tt.step)
		clock.Advance(tt.idle)
		if got := s.Tick(); got != tt.wantPet {
			t.Errorf("%s idle %v: pet = %v, want %v", tt.step, tt.idle, got, tt.wantPet)
		}
	}
}

func TestSupervisorKeepaliveError(t *testing.T) {
	dev := &fakeDevice{timeout: time.Minute, petErr: errors.New("EIO")}
	s := newTestSupervisor(dev, &testClock{now: time.Unix(0, 0)}, nil)
	if s.Tick() {
		t.Fatal("Tick reported a failed keepalive as pet")
	}
}

func TestSupervisorHandoff(t *testing.T) {
	for _, keep := range []bool{false, true} {
		dev := &fakeDevice{timeout: time.Minute}
		s := newTestSupervisor(dev, &testClock{now: time.Unix(0, 0)}, nil)
		if err := s.Handoff(keep); err != nil {
			t.Fatal(err)
		}
		if dev.stopped == keep || dev.released != keep {
			t.Fatalf("keep=%v: stopped %v, released %v", keep, dev.stopped, dev.released)
		}
		if s.Tick() || dev.pets() != 0 {
			t.Fatalf("keep=%v: keepalive after the handoff", keep)
		}
		// A second handoff (reboot after a failed kexec) does nothing.
		dev.stopped, dev.released = false, false
		if err := s.Handoff(!keep); err != nil || dev.stopped || dev.released {
			t.Fatalf("second handoff touched the device")
		}
	}
}

func TestSupervisorRunStopsAtHandoff(t *testing.T) {
	dev := &fakeDevice{timeout: 3 * time.Second} // keepalive every second
	s := NewSupervisor(dev, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	deadline := time.After(10 * time.Second)
	for dev.pets() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not pet the device")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := s.Handoff(false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the handoff")
	}
}

func TestGlobalHooksWithoutSupervisor(t *testing.T) {
	Activate(nil)
	Step("x")
	Progress()
	Idle()
	if err := Handoff(true); err != nil {
		t.Fatal(err)
	}
	dev := &fakeDevice{timeout: time.Minute}
	clock := &testClock{now: time.Unix(0, 0)}
	s := newTestSupervisor(dev, clock, nil)
	Activate(s)
	t.Cleanup(func() { Activate(nil) })
	Step("detect-disk")
	clock.Advance(2 * time.Minute)
	if s.Tick() {
		t.Fatal("the global Step did not reach the supervisor")
	}
	Progress()
	if !s.Tick() {
		t.Fatal("the global Progress did not reach the supervisor")
	}
	if err := Handoff(false); err != nil || !dev.stopped {
		t.Fatalf("global Handoff: %v, stopped %v", err, dev.stopped)
	}
}

func TestSupervisorOnStallRunsOncePerStall(t *testing.T) {
	dev := &fakeDevice{timeout: time.Minute}
	clock := &testClock{now: time.Unix(0, 0)}
	var stalled []string
	s := NewSupervisor(dev, Options{Now: clock.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnStall: func(step string) { stalled = append(stalled, step) }})
	s.Step("partprobe")
	clock.Advance(2 * time.Minute)
	s.Tick()
	s.Tick()
	s.Progress()
	s.Tick()
	clock.Advance(2 * time.Minute)
	s.Tick()
	if strings.Join(stalled, ",") != "partprobe,partprobe" {
		t.Fatalf("OnStall calls = %v, want one per stall", stalled)
	}
}
