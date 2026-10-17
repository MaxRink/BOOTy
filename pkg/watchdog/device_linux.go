//go:build linux

package watchdog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultDevices are tried in order when WATCHDOG_DEVICE is empty.
var DefaultDevices = []string{"/dev/watchdog0", "/dev/watchdog"}

// ErrNoDevice is returned by OpenFirst when no watchdog device exists.
var ErrNoDevice = errors.New("no watchdog device")

// magicClose is the character that disarms the watchdog when it is the
// last one written before close (Documentation/watchdog/watchdog-api.rst).
const magicClose = "V"

// keepaliveByte pets the watchdog: every write does.
const keepaliveByte = "\n"

// timeoutSetter sets the timeout of an open watchdog and returns the value
// that the driver applied. Tests replace it for a plain file.
type timeoutSetter func(fd int, seconds int) (int, error)

// ioctlTimeout sets the timeout with WDIOC_SETTIMEOUT and reads the applied
// value back with WDIOC_GETTIMEOUT: drivers round to their resolution.
func ioctlTimeout(fd, seconds int) (int, error) {
	if err := unix.IoctlSetPointerInt(fd, unix.WDIOC_SETTIMEOUT, seconds); err != nil {
		return 0, fmt.Errorf("WDIOC_SETTIMEOUT %d: %w", seconds, err)
	}
	applied, err := unix.IoctlGetInt(fd, unix.WDIOC_GETTIMEOUT)
	if err != nil {
		return seconds, nil //nolint:nilerr // the set worked; some drivers can not report it
	}
	return applied, nil
}

// fileDevice is a Linux watchdog character device. Opening it arms the
// watchdog; a write pets it; "V" before close disarms it.
type fileDevice struct {
	path    string
	timeout time.Duration

	mu sync.Mutex
	f  *os.File
}

// OpenDevice opens and arms the watchdog at path with the timeout.
func OpenDevice(path string, timeout time.Duration) (Device, error) {
	return openDevice(path, timeout, ioctlTimeout)
}

func openDevice(path string, timeout time.Duration, setTimeout timeoutSetter) (*fileDevice, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open watchdog %s: %w", path, err)
	}
	applied, err := setTimeout(int(f.Fd()), int(timeout/time.Second))
	if err != nil || applied <= 0 {
		// The device is armed with its old timeout: disarm it again.
		_, _ = f.WriteString(magicClose)
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("driver reports timeout %d", applied)
		}
		return nil, fmt.Errorf("set watchdog %s timeout: %w", path, err)
	}
	return &fileDevice{path: path, timeout: time.Duration(applied) * time.Second, f: f}, nil
}

// OpenFirst opens the first device of paths that exists.
func OpenFirst(paths []string, timeout time.Duration) (Device, error) {
	var errs []error
	for _, path := range paths {
		dev, err := OpenDevice(path, timeout)
		if err == nil {
			return dev, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, ErrNoDevice
	}
	return nil, errors.Join(errs...)
}

func (d *fileDevice) Name() string           { return d.path }
func (d *fileDevice) Timeout() time.Duration { return d.timeout }

func (d *fileDevice) Keepalive() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return os.ErrClosed
	}
	if _, err := d.f.WriteString(keepaliveByte); err != nil {
		return fmt.Errorf("pet watchdog %s: %w", d.path, err)
	}
	return nil
}

// Stop writes the magic close character and closes the device. A kernel
// with nowayout keeps the watchdog running anyway.
func (d *fileDevice) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	_, werr := d.f.WriteString(magicClose)
	cerr := d.f.Close()
	d.f = nil
	if err := errors.Join(werr, cerr); err != nil {
		return fmt.Errorf("disarm watchdog %s: %w", d.path, err)
	}
	return nil
}

// Release closes the device without the magic character, so the watchdog
// stays armed.
func (d *fileDevice) Release() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	if err != nil {
		return fmt.Errorf("release watchdog %s: %w", d.path, err)
	}
	return nil
}
