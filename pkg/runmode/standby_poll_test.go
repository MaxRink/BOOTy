//go:build linux

package runmode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

// queuedProvider returns one batch of commands per poll, then none.
type queuedProvider struct {
	standbyProvider
	batches [][]config.Command
}

func (p *queuedProvider) FetchCommands(context.Context) ([]config.Command, error) {
	p.fetches++
	if len(p.batches) == 0 {
		return nil, nil
	}
	b := p.batches[0]
	p.batches = p.batches[1:]
	return b, nil
}

// After a deprovision command that returns to standby, the next poll starts
// after repollAfterCommands, not after the poll interval: CAPRF queues the
// provision command right after the ACK.
func TestStandbyPollsAgainAtOnceAfterCommands(t *testing.T) {
	defer func(d time.Duration) { pollInterval = d }(pollInterval)
	pollInterval = time.Hour

	cfg := standbyConfig()
	cfg.Agent.DeprovisionSuccessAction = config.AgentDeprovisionStandby
	provider := &queuedProvider{batches: [][]config.Command{
		{{ID: "d1", Type: "deprovision"}},
		{{ID: "r1", Type: "reboot"}},
	}}
	mode := &StandbyMode{
		deps: Deps{Cfg: cfg, Client: provider},
		newDeprovisioner: func(*config.MachineConfig) standbyDeprovisioner {
			return standbyDeprovisionerFunc(func(context.Context) error { return nil })
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reboot *RebootRequestedError
	if err := mode.Run(ctx); !errors.As(err, &reboot) {
		t.Fatalf("Run error = %v, want the reboot command without waiting for the poll interval", err)
	}
	if provider.fetches != 2 {
		t.Fatalf("fetches = %d, want 2", provider.fetches)
	}
}

func TestNextPollDelay(t *testing.T) {
	defer func(d time.Duration) { pollInterval = d }(pollInterval)
	pollInterval = 10 * time.Second

	if d := nextPollDelay(time.Now(), true); d != repollAfterCommands {
		t.Errorf("after commands: delay = %v, want %v", d, repollAfterCommands)
	}
	// A long poll that the server held for 20 s: poll again at once.
	if d := nextPollDelay(time.Now().Add(-20*time.Second), false); d != 0 {
		t.Errorf("after a long poll: delay = %v, want 0", d)
	}
	// A server that answers at once: keep the interval.
	if d := nextPollDelay(time.Now(), false); d < 9*time.Second {
		t.Errorf("after a short poll: delay = %v, want about 10s", d)
	}
}

func TestMemInfoMiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	data := "MemTotal:        2048000 kB\nMemFree:          100000 kB\nMemAvailable:    1024000 kB\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if total, available := memInfoMiB(path); total != 2000 || available != 1000 {
		t.Fatalf("memInfoMiB = %d, %d, want 2000, 1000", total, available)
	}
	if total, available := memInfoMiB(filepath.Join(t.TempDir(), "missing")); total != 0 || available != 0 {
		t.Fatalf("missing file: %d, %d, want zeros", total, available)
	}
}

// heartbeatProvider signals every heartbeat on beats and counts them. Its
// FetchCommands (with hold) holds the first loop poll, like a CAPRF long
// poll, until it has seen three more heartbeats, then returns a reboot.
type heartbeatProvider struct {
	standbyProvider
	beats  chan struct{}
	count  atomic.Int32
	hold   bool
	polls  int
	failed error
	// batches are the commands of the polls after the readiness poll.
	batches [][]config.Command
}

func newHeartbeatProvider() *heartbeatProvider {
	return &heartbeatProvider{beats: make(chan struct{}, 1024)}
}

func (p *heartbeatProvider) Heartbeat(context.Context) error {
	p.count.Add(1)
	select {
	case p.beats <- struct{}{}:
	default:
	}
	return nil
}

func (p *heartbeatProvider) FetchCommands(ctx context.Context) ([]config.Command, error) {
	p.polls++
	if p.polls == 1 { // readiness poll
		return nil, nil
	}
	if p.hold {
		p.failed = p.awaitHeartbeats(ctx, 3)
		return []config.Command{{ID: "r1", Type: "reboot"}}, nil
	}
	if len(p.batches) == 0 {
		return []config.Command{{ID: "r1", Type: "reboot"}}, nil
	}
	b := p.batches[0]
	p.batches = p.batches[1:]
	return b, nil
}

// awaitHeartbeats waits for n heartbeats after the call, or fails after a
// timeout that only a missing heartbeat goroutine reaches.
func (p *heartbeatProvider) awaitHeartbeats(ctx context.Context, n int) error {
	for drained := false; !drained; {
		select {
		case <-p.beats:
		default:
			drained = true
		}
	}
	timeout := time.After(5 * time.Second)
	for range n {
		select {
		case <-p.beats:
		case <-timeout:
			return errors.New("no heartbeat while the poll was held")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// A held poll must not hold the heartbeats.
func TestStandbyHeartbeatsDoNotWaitForTheLongPoll(t *testing.T) {
	defer func(h, p time.Duration) { heartbeatInterval, pollInterval = h, p }(heartbeatInterval, pollInterval)
	heartbeatInterval, pollInterval = 10*time.Millisecond, 0

	provider := newHeartbeatProvider()
	provider.hold = true
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var reboot *RebootRequestedError
	if err := mode.Run(ctx); !errors.As(err, &reboot) {
		t.Fatalf("Run error = %v, want the reboot command", err)
	}
	if provider.failed != nil {
		t.Fatal(provider.failed)
	}
}

// slowDeprovisioner blocks until the test lets it end.
type slowDeprovisioner struct {
	started chan struct{}
	release chan struct{}
}

func (d *slowDeprovisioner) Deprovision(context.Context) error {
	close(d.started)
	<-d.release
	return nil
}

// Heartbeats pause while a command runs; one already started may still
// arrive.
func TestStandbyPausesHeartbeatsWhileACommandRuns(t *testing.T) {
	defer func(h, p time.Duration) { heartbeatInterval, pollInterval = h, p }(heartbeatInterval, pollInterval)
	heartbeatInterval, pollInterval = 5*time.Millisecond, 0

	cfg := standbyConfig()
	cfg.Agent.DeprovisionSuccessAction = config.AgentDeprovisionStandby
	provider := newHeartbeatProvider()
	provider.batches = [][]config.Command{{{ID: "d1", Type: "deprovision"}}}
	deprov := &slowDeprovisioner{started: make(chan struct{}), release: make(chan struct{})}
	mode := &StandbyMode{
		deps:             Deps{Cfg: cfg, Client: provider},
		newDeprovisioner: func(*config.MachineConfig) standbyDeprovisioner { return deprov },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- mode.Run(ctx) }()
	<-deprov.started
	before := provider.count.Load()
	// Sixty heartbeat intervals.
	time.Sleep(300 * time.Millisecond)
	during := provider.count.Load() - before
	close(deprov.release)

	var reboot *RebootRequestedError
	if err := <-done; !errors.As(err, &reboot) {
		t.Fatalf("Run error = %v, want the reboot command after the deprovision", err)
	}
	if during > 1 {
		t.Fatalf("%d heartbeats while the command ran, want at most the one already started", during)
	}
}

// failOnceProvider fails the first loop poll and then returns a reboot.
type failOnceProvider struct {
	standbyProvider
}

func (p *failOnceProvider) FetchCommands(context.Context) ([]config.Command, error) {
	p.fetches++
	switch p.fetches {
	case 1: // readiness poll
		return nil, nil
	case 2:
		return nil, errors.New("poll failed")
	default:
		return []config.Command{{ID: "r1", Type: "reboot"}}, nil
	}
}

// A failed poll schedules the next one.
func TestStandbyPollsAgainAfterAFailedPoll(t *testing.T) {
	defer func(d time.Duration) { pollInterval = d }(pollInterval)
	pollInterval = 10 * time.Millisecond

	provider := &failOnceProvider{}
	mode := &StandbyMode{deps: Deps{Cfg: standbyConfig(), Client: provider}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reboot *RebootRequestedError
	if err := mode.Run(ctx); !errors.As(err, &reboot) {
		t.Fatalf("Run error = %v, want the reboot command of the poll after the failed one", err)
	}
	if provider.fetches != 3 {
		t.Fatalf("fetches = %d, want 3", provider.fetches)
	}
}
