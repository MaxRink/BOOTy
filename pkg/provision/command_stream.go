//go:build linux

package provision

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/telekom/BOOTy/pkg/executil"
)

// maxCommandOutputLine bounds one output line of a streamed command, and
// maxCutLineLog bounds what is logged of a longer line.
const (
	maxCommandOutputLine = 64 * 1024
	maxCutLineLog        = 4096
)

// streamWaitDelay bounds how long a finished command may keep its output
// pipes open through background children. Tests shorten it.
var streamWaitDelay = 30 * time.Second

// runStreamingCommand runs argv, inside the chroot root when root is set,
// and calls onLine for every line of its combined stdout and stderr while it
// runs. Tests replace it.
var runStreamingCommand = defaultRunStreamingCommand

// streamLines calls onLine for every line of r. A line longer than
// maxCommandOutputLine is cut, marked and skipped up to its end, and the
// lines after it are still reported.
func streamLines(r io.Reader, onLine func(string)) {
	reader := bufio.NewReaderSize(r, maxCommandOutputLine)
	for {
		line, err := reader.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			onLine(string(line[:min(len(line), maxCutLineLog)]) + " [line cut]")
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
		case len(line) > 0:
			onLine(strings.TrimRight(string(line), "\r\n"))
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func defaultRunStreamingCommand(ctx context.Context, onLine func(string), root string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("run: empty command")
	}
	pr, pw := io.Pipe()
	// executil registers the child, so the PID 1 reaper does not take its
	// exit status. The chroot uses the syscall, so no host chroot binary is
	// needed (the micro initramfs has none), and argv[0] is a path inside
	// root, so skip the host PATH lookup.
	cmd := executil.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Path = argv[0]
	cmd.Err = nil
	// Own process group: cancellation kills the script and its background
	// children, and WaitDelay stops a child that keeps the pipes open from
	// blocking the step after the script exited.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if root != "" {
		cmd.SysProcAttr.Chroot = root
		cmd.Dir = "/"
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// ESRCH: the group already exited, which is not a failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill process group %d: %w", cmd.Process.Pid, err)
		}
		return nil
	}
	cmd.WaitDelay = streamWaitDelay
	cmd.Stdout = pw
	cmd.Stderr = pw

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		streamLines(pr, onLine)
		// Keep draining so the command never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, pr)
	}()

	err := cmd.Run()
	_ = pw.Close()
	<-scanDone
	// ErrWaitDelay only means a background child still held the output
	// pipes after the script exited with status 0.
	// Stop any children left in the group, after success and after failure,
	// so they cannot keep changing the target in later steps.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A canceled step never counts as success, and keeps the context error.
		return fmt.Errorf("run %s: %w", argv[0], errors.Join(ctxErr, err))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// Only children still held the pipes; the script itself succeeded.
		return nil
	}
	if err != nil {
		return fmt.Errorf("run %s: %w", argv[0], err)
	}
	return nil
}
