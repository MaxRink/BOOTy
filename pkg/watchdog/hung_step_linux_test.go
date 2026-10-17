//go:build linux

package watchdog

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// End to end with a fake device file: a hung step stops the keepalives
// written to the device, and a normal run ends with the magic close.
func TestFakeDeviceHungStepAndCleanHandoff(t *testing.T) {
	path := fakeWatchdogFile(t)
	var requested int
	dev, err := openDevice(path, 30*time.Second, fakeTimeout(&requested, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Unix(1_000_000, 0)}
	s := NewSupervisor(dev, Options{Now: clock.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	// A normal step pets on every tick.
	s.Step("detect-disk")
	for range 3 {
		clock.Advance(10 * time.Second)
		s.Progress()
		s.Tick()
	}
	if got := readFile(t, path); got != "\n\n\n" {
		t.Fatalf("writes after a normal step = %q", got)
	}
	// The next step hangs: the device gets no more writes, so the hardware
	// would reset the host after its timeout.
	s.Step("partprobe")
	for range 6 {
		clock.Advance(10 * time.Second)
		s.Tick()
	}
	// The budget is the 30 s timeout: ticks at 10, 20 and 30 s pet, the
	// ticks after it do not.
	if got := readFile(t, path); got != strings.Repeat("\n", 6) {
		t.Fatalf("writes during a hung step = %q, want three more keepalives within the budget only", got)
	}
	// A run that recovers and hands off disarms the device.
	s.Idle()
	s.Tick()
	if err := s.Handoff(false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.HasSuffix(got, "\nV") {
		t.Fatalf("writes at the handoff = %q, want the magic close last", got)
	}
}
