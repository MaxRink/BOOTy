//go:build linux

package ipmi

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// recordingRunner records every ipmitool call.
type recordingRunner struct{ calls []string }

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

func TestBMCWatchdogCommands(t *testing.T) {
	r := &recordingRunner{}
	m := &Manager{deviceNum: "0", runner: r, log: slog.Default()}
	ctx := context.Background()
	if err := m.SetWatchdog(ctx, 300*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.ResetWatchdog(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.StopWatchdog(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		// 300 s = 3000 * 100 ms = 0x0bb8, LSB first.
		"ipmitool -I open -d 0 raw 0x06 0x24 0x04 0x01 0x00 0x10 0xb8 0x0b",
		"ipmitool -I open -d 0 raw 0x06 0x22",
		"ipmitool -I open -d 0 raw 0x06 0x22",
		"ipmitool -I open -d 0 raw 0x06 0x24 0x04 0x00 0x00 0x10 0xff 0xff",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}
	for _, bad := range []time.Duration{0, MaxWatchdogTimeout + time.Second} {
		if err := m.SetWatchdog(ctx, bad); err == nil {
			t.Errorf("SetWatchdog(%s) succeeded", bad)
		}
	}
}
