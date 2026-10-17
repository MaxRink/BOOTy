package watchdog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// recorder is a Tracker that records the calls.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(c string)     { r.mu.Lock(); r.calls = append(r.calls, c); r.mu.Unlock() }
func (r *recorder) Step(name string) { r.add("step " + name) }
func (r *recorder) Progress()        { r.add("progress") }
func (r *recorder) Idle()            { r.add("idle") }
func (r *recorder) Handoff(bool) error {
	r.add("handoff")
	return nil
}

type stubRunner struct{ err error }

func (s stubRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte("out"), s.err
}

func TestProgressRunnerReportsEveryCommand(t *testing.T) {
	r := &recorder{}
	Activate(r)
	t.Cleanup(func() { Activate(nil) })
	if out, err := (ProgressRunner{Inner: stubRunner{}}).Run(context.Background(), "wipefs", "-a", "/dev/sda"); err != nil || string(out) != "out" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	failed := errors.New("exit 1")
	if _, err := (ProgressRunner{Inner: stubRunner{err: failed}}).Run(context.Background(), "hdparm"); !errors.Is(err, failed) {
		t.Fatalf("Run error = %v", err)
	}
	if got := strings.Join(r.calls, ","); got != "progress,progress" {
		t.Fatalf("calls = %q", got)
	}
}
