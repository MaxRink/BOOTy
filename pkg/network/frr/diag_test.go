//go:build linux

package frr

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitForHTTPWithFRRRedactsTargetInLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var logs bytes.Buffer
	mgr := NewManager(newMockFRRCommander())
	mgr.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The first target is unreachable, so the failure path logs too.
	if err := waitForHTTPWithFRR(context.Background(), "http://127.0.0.1:1/?token=SECRET1", 1500*time.Millisecond, mgr); err == nil {
		t.Fatal("expected timeout for unreachable target")
	}
	if err := waitForHTTPWithFRR(context.Background(), srv.URL+"/?token=SECRET2", 5*time.Second, mgr); err != nil {
		t.Fatalf("waitForHTTPWithFRR() = %v", err)
	}
	for _, secret := range []string{"SECRET1", "SECRET2"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leak %s:\n%s", secret, logs.String())
		}
	}
}

// blockingCommander blocks until its context is done.
type blockingCommander struct{ calls int }

func (b *blockingCommander) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	b.calls++
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDumpFRRStateBoundsEachCommand(t *testing.T) {
	orig := frrDumpCommandTimeout
	t.Cleanup(func() { frrDumpCommandTimeout = orig })
	frrDumpCommandTimeout = 20 * time.Millisecond

	cmd := &blockingCommander{}
	mgr := NewManager(cmd)
	done := make(chan struct{})
	go func() {
		mgr.DumpFRRState()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DumpFRRState blocked on a wedged vtysh")
	}
	if cmd.calls != 6 {
		t.Fatalf("vtysh calls = %d, want 6", cmd.calls)
	}
}
