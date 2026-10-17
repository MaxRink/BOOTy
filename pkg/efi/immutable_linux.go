//go:build linux

package efi

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"
)

// fsImmutableFL is FS_IMMUTABLE_FL of linux/fs.h (x/sys/unix has no name
// for it).
const fsImmutableFL = 0x00000010

// Inode flag ioctls; tests replace them.
var (
	getInodeFlags = func(fd int) (uint32, error) { return unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS) }
	setInodeFlags = func(fd int, flags uint32) error {
		return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(flags))
	}
)

// clearImmutable clears FS_IMMUTABLE_FL of an existing file and returns a
// function that sets it again. A missing file, or a file system without
// inode flags, needs nothing.
func clearImmutable(path string) (func(), error) {
	noop := func() {}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return noop, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(fd) //nolint:errcheck // read-only fd
	flags, err := getInodeFlags(fd)
	if err != nil || flags&fsImmutableFL == 0 {
		return noop, nil //nolint:nilerr // no inode flags: nothing to clear
	}
	if err := setInodeFlags(fd, flags&^fsImmutableFL); err != nil {
		return nil, fmt.Errorf("clear immutable flag of %s: %w", path, err)
	}
	return func() { restoreFlags(path, flags) }, nil
}

func restoreFlags(path string, flags uint32) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err == nil {
		err = setInodeFlags(fd, flags)
		_ = unix.Close(fd)
	}
	if err != nil {
		slog.Warn("restoring the immutable flag failed", "path", path, "error", err)
	}
}
