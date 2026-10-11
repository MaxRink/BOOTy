//go:build linux

package kexec

import (
	"errors"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExecuteSyncsBeforeKexec(t *testing.T) {
	origSync, origReboot := syncFilesystems, reboot
	t.Cleanup(func() { syncFilesystems, reboot = origSync, origReboot })

	var calls []string
	syncFilesystems = func() { calls = append(calls, "sync") }
	reboot = func(cmd int) error {
		if cmd != unix.LINUX_REBOOT_CMD_KEXEC {
			t.Errorf("reboot cmd = %#x, want LINUX_REBOOT_CMD_KEXEC", cmd)
		}
		calls = append(calls, "kexec")
		return errors.New("stop")
	}

	if err := Execute(); err == nil {
		t.Fatal("Execute() error = nil, want reboot error")
	}
	if want := []string{"sync", "kexec"}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
