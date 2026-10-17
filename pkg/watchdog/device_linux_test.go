//go:build linux

package watchdog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeWatchdogFile is a plain file that stands in for /dev/watchdog: every
// keepalive and the magic close show up in its content.
func fakeWatchdogFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchdog0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeTimeout records the requested timeout and applies applied (or the
// request when applied is 0).
func fakeTimeout(requested *int, applied int, err error) timeoutSetter {
	return func(_ int, seconds int) (int, error) {
		*requested = seconds
		if applied == 0 {
			applied = seconds
		}
		return applied, err
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFileDeviceKeepaliveAndMagicClose(t *testing.T) {
	path := fakeWatchdogFile(t)
	var requested int
	dev, err := openDevice(path, 300*time.Second, fakeTimeout(&requested, 298, nil))
	if err != nil {
		t.Fatalf("openDevice: %v", err)
	}
	if requested != 300 || dev.Timeout() != 298*time.Second || dev.Name() != path {
		t.Fatalf("requested %d, device timeout %v, name %q", requested, dev.Timeout(), dev.Name())
	}
	for range 3 {
		if err := dev.Keepalive(); err != nil {
			t.Fatalf("Keepalive: %v", err)
		}
	}
	if err := dev.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := readFile(t, path); got != "\n\n\nV" {
		t.Fatalf("device writes = %q, want three keepalives and the magic close", got)
	}
	if err := dev.Keepalive(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Keepalive after Stop = %v, want os.ErrClosed", err)
	}
	if err := dev.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestFileDeviceReleaseLeavesArmed(t *testing.T) {
	path := fakeWatchdogFile(t)
	var requested int
	dev, err := openDevice(path, 60*time.Second, fakeTimeout(&requested, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.Keepalive(); err != nil {
		t.Fatal(err)
	}
	if err := dev.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := readFile(t, path); strings.Contains(got, "V") {
		t.Fatalf("Release wrote the magic close: %q", got)
	}
}

func TestOpenDeviceTimeoutFailureDisarms(t *testing.T) {
	path := fakeWatchdogFile(t)
	var requested int
	if _, err := openDevice(path, 60*time.Second, fakeTimeout(&requested, 0, errors.New("EINVAL"))); err == nil {
		t.Fatal("openDevice succeeded with a failing WDIOC_SETTIMEOUT")
	}
	if got := readFile(t, path); got != "V" {
		t.Fatalf("device writes = %q, want the magic close", got)
	}
}

// A plain file does not support WDIOC_SETTIMEOUT: the real setter fails and
// the device is disarmed again.
func TestOpenDeviceRejectsNonWatchdog(t *testing.T) {
	path := fakeWatchdogFile(t)
	if _, err := OpenDevice(path, 60*time.Second); err == nil || !strings.Contains(err.Error(), "WDIOC_SETTIMEOUT") {
		t.Fatalf("OpenDevice(plain file) = %v, want a WDIOC_SETTIMEOUT error", err)
	}
}

func TestOpenFirstSkipsMissingDevices(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenFirst([]string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}, time.Minute); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("OpenFirst(missing) = %v, want ErrNoDevice", err)
	}
	plain := fakeWatchdogFile(t)
	if _, err := OpenFirst([]string{filepath.Join(dir, "a"), plain}, time.Minute); err == nil || errors.Is(err, ErrNoDevice) {
		t.Fatalf("OpenFirst(plain file) = %v, want the open error", err)
	}
}
