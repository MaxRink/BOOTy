//go:build linux

package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefaultRunStreamingCommandStreamsLines(t *testing.T) {
	var lines []string
	err := defaultRunStreamingCommand(context.Background(), func(line string) { lines = append(lines, line) },
		"", []string{"/bin/sh", "-c", "echo one; echo two >&2; printf three"})
	if err != nil {
		t.Fatalf("defaultRunStreamingCommand() = %v", err)
	}
	if !reflect.DeepEqual(lines, []string{"one", "two", "three"}) {
		t.Fatalf("lines = %q", lines)
	}
}

func TestDefaultRunStreamingCommandReportsFailure(t *testing.T) {
	err := defaultRunStreamingCommand(context.Background(), func(string) {}, "", []string{"/bin/sh", "-c", "exit 3"})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("defaultRunStreamingCommand() = %v, want exit status 3", err)
	}
}

func TestDefaultRunStreamingCommandDropsOverlongLines(t *testing.T) {
	var lines []string
	err := defaultRunStreamingCommand(context.Background(), func(line string) { lines = append(lines, line) },
		"", []string{"/bin/sh", "-c", "head -c 70000 /dev/zero | tr '\\0' a; echo; echo after"})
	if err != nil {
		t.Fatalf("defaultRunStreamingCommand() = %v", err)
	}
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "[line cut]") || lines[1] != "after" {
		t.Fatalf("got %d lines, want the cut long line and then %q", len(lines), "after")
	}
}

// A background child that keeps stdout open must not block the step after
// the script itself exited.
func TestDefaultRunStreamingCommandDoesNotWaitForBackgroundChildren(t *testing.T) {
	old := streamWaitDelay
	streamWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { streamWaitDelay = old })

	start := time.Now()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	err := defaultRunStreamingCommand(context.Background(), func(string) {}, "",
		[]string{"/bin/sh", "-c", "sleep 30 & echo $! > " + pidFile + "; echo started"})
	if err != nil {
		t.Fatalf("defaultRunStreamingCommand() = %v, want nil: the script itself succeeded", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	waitProcessGone(t, pid)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("command returned after %s, the background child held it", elapsed)
	}
}

// Cancellation kills the script and its background children.
func TestDefaultRunStreamingCommandCancelKillsGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := defaultRunStreamingCommand(ctx, func(string) {}, "", []string{"/bin/sh", "-c", "sleep 30 & sleep 30"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("defaultRunStreamingCommand() = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

// A failing script must not leave its background children running.
func TestDefaultRunStreamingCommandKillsChildrenAfterFailure(t *testing.T) {
	old := streamWaitDelay
	streamWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { streamWaitDelay = old })

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	err := defaultRunStreamingCommand(context.Background(), func(string) {}, "",
		[]string{"/bin/sh", "-c", "sleep 30 & echo $! > " + pidFile + "; exit 3"})
	if err == nil {
		t.Fatal("failing script returned nil")
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	waitProcessGone(t, pid)
}

// waitProcessGone fails the test when pid still runs after 5 seconds. A
// killed child stays a zombie until its parent reaps it, which counts as gone.
func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if state, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err != nil || strings.Contains(string(state), ") Z ") {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("process %d still runs", pid)
		}
	}
}
