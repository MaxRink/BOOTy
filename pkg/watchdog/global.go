package watchdog

import "sync/atomic"

// Tracker receives the progress of the run. *Supervisor implements it;
// tests use a recording fake.
type Tracker interface {
	Step(name string)
	Progress()
	Idle()
	Handoff(keep bool) error
}

type trackerHolder struct{ t Tracker }

// active is the tracker of the run, nil when the watchdog is off.
var active atomic.Pointer[trackerHolder]

// Activate makes t the tracker that Step, Progress, Idle and Handoff use.
// Nil deactivates.
func Activate(t Tracker) {
	if t == nil {
		active.Store(nil)
		return
	}
	active.Store(&trackerHolder{t: t})
}

func current() Tracker {
	if h := active.Load(); h != nil {
		return h.t
	}
	return nil
}

// Step marks the start of a provisioning or deprovisioning step. It does
// nothing when the watchdog is off.
func Step(name string) {
	if t := current(); t != nil {
		t.Step(name)
	}
}

// Progress records progress of the active step. It does nothing when the
// watchdog is off.
func Progress() {
	if t := current(); t != nil {
		t.Progress()
	}
}

// Idle marks that no step is active. It does nothing when the watchdog is
// off.
func Idle() {
	if t := current(); t != nil {
		t.Idle()
	}
}

// Handoff ends the supervision before kexec, reboot or power off; see
// Supervisor.Handoff. It does nothing when the watchdog is off.
func Handoff(keep bool) error {
	if t := current(); t != nil {
		return t.Handoff(keep)
	}
	return nil
}
