//go:build linux

package realm

import (
	"errors"
	"slices"
	"syscall"
	"testing"
)

func TestRequestRebootAndPowerOffSyncFirst(t *testing.T) {
	origSync, origReboot := syncFilesystems, rebootSyscall
	t.Cleanup(func() { syncFilesystems, rebootSyscall = origSync, origReboot })
	t.Setenv("BOOTY_NO_REBOOT", "")

	for _, tt := range []struct {
		name string
		call func() error
		cmd  int
	}{
		{"reboot", RequestReboot, syscall.LINUX_REBOOT_CMD_RESTART},
		{"power off", RequestPowerOff, syscall.LINUX_REBOOT_CMD_POWER_OFF},
	} {
		var calls []string
		syncFilesystems = func() { calls = append(calls, "sync") }
		rebootSyscall = func(cmd int) error {
			if cmd != tt.cmd {
				t.Errorf("%s: reboot cmd = %#x, want %#x", tt.name, cmd, tt.cmd)
			}
			calls = append(calls, "reboot")
			return errors.New("stop")
		}
		if err := tt.call(); err == nil {
			t.Fatalf("%s: error = nil, want the reboot error", tt.name)
		}
		if want := []string{"sync", "reboot"}; !slices.Equal(calls, want) {
			t.Fatalf("%s: calls = %v, want %v", tt.name, calls, want)
		}
	}
}
