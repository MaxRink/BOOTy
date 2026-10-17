package watchdog

import "context"

// Runner runs external commands, like executil.Commander.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ProgressRunner reports progress after every command of Inner: disk
// tools such as wipefs, hdparm, nvme format or sg_sanitize do their work
// inside one command without output, so a finished command is the progress
// of a wipe or erase step.
type ProgressRunner struct {
	Inner Runner
}

// Run runs the command and then reports progress, also when it failed.
func (r ProgressRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.Inner.Run(ctx, name, args...)
	Progress()
	return out, err //nolint:wrapcheck // a transparent wrapper
}
