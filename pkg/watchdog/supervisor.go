// Package watchdog arms a hardware watchdog and pets it while the run makes
// progress. A provisioning step that makes no progress for its budget stops
// the petting, and the hardware resets the host, also when the kernel hangs.
package watchdog

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// CodeStalled is the failure code logged when a step stalls and BOOTy stops
// petting the watchdog.
const CodeStalled = "BOOTY-WDT-001"

// Device is an armed hardware watchdog.
type Device interface {
	// Name identifies the device in logs.
	Name() string
	// Timeout is the hardware timeout in effect.
	Timeout() time.Duration
	// Keepalive pets the watchdog.
	Keepalive() error
	// Stop disarms the watchdog and releases it.
	Stop() error
	// Release releases the watchdog and leaves it armed.
	Release() error
}

// DefaultStepBudgets are the built-in budgets of steps that can run long
// without reporting progress. WATCHDOG_STEP_BUDGETS overrides them.
//
// Wipe and erase steps report progress after every disk command (see
// ProgressRunner), but one firmware overwrite or sanitize of a large hard
// disk is one command: about 22 h for 20 TB. Their budget is 48 h per
// command.
var DefaultStepBudgets = map[string]time.Duration{
	"wipe-disks":        48 * time.Hour,
	"decommission-wipe": 48 * time.Hour,
	// Firmware reads and NIC firmware changes, RAID and NVMe namespace
	// setup, partitioning and partition growth wait for devices and tools.
	"collect-firmware":       time.Hour,
	"setup-mellanox":         time.Hour,
	"setup-raid":             time.Hour,
	"setup-nvme-namespaces":  time.Hour,
	"apply-partition-layout": time.Hour,
	"grow-partition":         time.Hour,
	// Image writes with dd (A/B modes), mirroring, file system checks and
	// operator commands.
	"stream-image":            time.Hour,
	"mirror-root":             time.Hour,
	"check-filesystem":        time.Hour,
	"resize-filesystem":       time.Hour,
	"run-machine-commands":    time.Hour,
	"run-post-provision-cmds": time.Hour,
}

// Supervisor pets a Device while the active step makes progress. Outside a
// step (network setup, standby, waiting after a failure) it pets as long as
// the process runs, so the hardware still resets a hung kernel.
type Supervisor struct {
	dev           Device
	interval      time.Duration
	defaultBudget time.Duration
	budgets       map[string]time.Duration
	now           func() time.Time
	log           *slog.Logger
	onStall       func(step string)

	done chan struct{}

	mu           sync.Mutex
	step         string
	lastProgress time.Time
	stalled      bool
	closed       bool
}

// Options configure a Supervisor.
type Options struct {
	// DefaultBudget is the time a step may run without progress when it has
	// no budget of its own. Zero uses the device timeout.
	DefaultBudget time.Duration
	// Budgets override DefaultStepBudgets by step name.
	Budgets map[string]time.Duration
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Log is the logger; nil uses slog.Default.
	Log *slog.Logger
	// OnStall runs once when a step first exceeds its budget, before the
	// hardware resets the host: BOOTy writes the out-of-band record there.
	OnStall func(step string)
}

// NewSupervisor returns a supervisor for dev. It pets every third of the
// device timeout.
func NewSupervisor(dev Device, opts Options) *Supervisor {
	s := &Supervisor{
		dev:           dev,
		interval:      max(dev.Timeout()/3, time.Second),
		defaultBudget: opts.DefaultBudget,
		budgets:       make(map[string]time.Duration, len(DefaultStepBudgets)+len(opts.Budgets)),
		now:           opts.Now,
		log:           opts.Log,
		onStall:       opts.OnStall,
		done:          make(chan struct{}),
	}
	if s.defaultBudget <= 0 {
		s.defaultBudget = dev.Timeout()
	}
	for step, budget := range DefaultStepBudgets {
		s.budgets[step] = budget
	}
	for step, budget := range opts.Budgets {
		s.budgets[step] = budget
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With("component", "watchdog", "device", dev.Name())
	return s
}

// Interval is the time between two keepalives.
func (s *Supervisor) Interval() time.Duration { return s.interval }

// Step marks the start of a step. It counts as progress.
func (s *Supervisor) Step(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.step = name
	s.lastProgress = s.now()
}

// Progress records progress of the active step, such as written bytes.
func (s *Supervisor) Progress() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProgress = s.now()
}

// Idle marks that no step is active.
func (s *Supervisor) Idle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.step = ""
	s.lastProgress = s.now()
}

// budget returns the stall budget of step.
func (s *Supervisor) budget(step string) time.Duration {
	if b, ok := s.budgets[step]; ok {
		return b
	}
	return s.defaultBudget
}

// Tick pets the watchdog unless the active step exceeded its budget. It
// reports whether it pet.
func (s *Supervisor) Tick() bool {
	pet, stalledStep := s.check()
	if stalledStep != "" && s.onStall != nil {
		s.onStall(stalledStep)
	}
	if !pet {
		return false
	}
	if err := s.dev.Keepalive(); err != nil {
		s.log.Warn("watchdog keepalive failed", "error", err)
		return false
	}
	return true
}

// check decides under the lock whether to pet. stalledStep is set once, when
// a step first exceeds its budget.
func (s *Supervisor) check() (pet bool, stalledStep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ""
	}
	if s.step != "" {
		budget := s.budget(s.step)
		if idle := s.now().Sub(s.lastProgress); idle > budget {
			if s.stalled {
				return false, ""
			}
			s.stalled = true
			s.log.Error("step made no progress within its budget, stopping the watchdog keepalive; the hardware resets the host",
				"step", s.step, "idle", idle.Round(time.Second), "budget", budget,
				"reset_within", s.dev.Timeout(), "code", CodeStalled)
			return false, s.step
		}
	}
	if s.stalled {
		s.stalled = false
		s.log.Warn("progress resumed, petting the watchdog again", "step", s.step)
	}
	return true, ""
}

// Run pets the watchdog every Interval until ctx ends or Handoff runs.
func (s *Supervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			s.Tick()
		}
	}
}

// Handoff ends the supervision before kexec, reboot or power off. keep
// leaves the watchdog armed for the target OS; otherwise it is disarmed.
// Later calls do nothing.
func (s *Supervisor) Handoff(keep bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.done)
	if keep {
		s.log.Info("leaving the watchdog armed for the target OS", "timeout", s.dev.Timeout())
		return s.dev.Release()
	}
	s.log.Info("disarming the watchdog")
	return s.dev.Stop()
}
