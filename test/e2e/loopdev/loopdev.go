// Package loopdev waits for the partition nodes of loop devices in the e2e
// tests. `losetup --partscan` returns before the kernel and udev have
// created /dev/loopNpM, and in containers without udev the node is never
// created at all, so a plain stat loop fails now and then.
package loopdev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var partitionRe = regexp.MustCompile(`^(/dev/loop\d+)p(\d+)$`)

// SplitPartition returns the loop device and the partition number of a
// loop partition path such as /dev/loop3p2.
func SplitPartition(path string) (dev string, part int, ok bool) {
	m := partitionRe.FindStringSubmatch(path)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], n, true
}

// Attach attaches image to a free loop device with partition scanning and
// waits for partitions 1..parts. It detaches the device again on error.
func Attach(image string, parts int) (string, error) {
	ctx := context.Background()
	out, err := exec.CommandContext(ctx, "losetup", "--find", "--show", "--partscan", image).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("losetup %s: %w: %s", image, err, out)
	}
	dev := strings.TrimSpace(string(out))
	if err := WaitPartitions(dev, parts, 30*time.Second); err != nil {
		_ = exec.CommandContext(ctx, "losetup", "--detach", dev).Run()
		return "", err
	}
	return dev, nil
}

// nodeGrace is how long WaitPartitions leaves a partition that sysfs lists
// to udev or devtmpfs before it creates the /dev node itself. A node that
// WaitPartitions creates is not removed by devtmpfs on detach.
const nodeGrace = time.Second

// WaitPartitions waits until dev has usable block device nodes for
// partitions 1..parts. While a partition is missing in sysfs it asks the
// kernel to re-read the partition table. When sysfs lists a partition but
// /dev has no matching node after nodeGrace (no udev, or a node left from an
// earlier loop device), it creates the node from the sysfs major:minor.
func WaitPartitions(dev string, parts int, timeout time.Duration) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		missing, lastErr := missingPartitions(dev, parts, time.Since(start) >= nodeGrace)
		if len(missing) == 0 {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("partitions %v of %s not ready within %s: %w", missing, dev, timeout, lastErr)
		}
		if _, err := exec.LookPath("partx"); err == nil {
			_ = exec.CommandContext(ctx, "partx", "--update", dev).Run()
		}
		_ = exec.CommandContext(ctx, "partprobe", dev).Run()
		if _, err := exec.LookPath("udevadm"); err == nil {
			_ = exec.CommandContext(ctx, "udevadm", "settle", "--timeout=2").Run()
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// missingPartitions returns the partitions that are not ready and the last
// reason.
func missingPartitions(dev string, parts int, mknod bool) ([]int, error) {
	var missing []int
	var lastErr error
	for i := 1; i <= parts; i++ {
		if err := ensureNode(fmt.Sprintf("%sp%d", dev, i), mknod); err != nil {
			missing = append(missing, i)
			lastErr = err
		}
	}
	return missing, lastErr
}

// ensureNode checks that node matches the sysfs major:minor and can be
// read. With mknod it replaces a missing or stale node.
func ensureNode(node string, mknod bool) error {
	data, err := os.ReadFile("/sys/class/block/" + filepath.Base(node) + "/dev")
	if err != nil {
		return fmt.Errorf("read sysfs dev of %s: %w", node, err)
	}
	var major, minor uint32
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d:%d", &major, &minor); err != nil {
		return fmt.Errorf("parse sysfs dev of %s: %w", node, err)
	}
	want := unix.Mkdev(major, minor)
	var st unix.Stat_t
	statErr := unix.Stat(node, &st)
	if statErr == nil && st.Mode&unix.S_IFMT == unix.S_IFBLK && uint64(st.Rdev) == want { //nolint:unconvert // Rdev is int32 on darwin
		return usable(node)
	}
	if !mknod {
		return fmt.Errorf("%s has no node for %d:%d yet", node, major, minor)
	}
	if statErr == nil {
		if err := os.Remove(node); err != nil {
			return fmt.Errorf("remove stale %s: %w", node, err)
		}
	}
	if err := unix.Mknod(node, unix.S_IFBLK|0o660, int(want)); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("mknod %s: %w", node, err)
	}
	return usable(node)
}

// usable opens node and reads its size. A partition that the kernel is
// still removing or adding fails here, so the caller tries again.
func usable(node string) error {
	f, err := os.Open(node)
	if err != nil {
		return fmt.Errorf("open %s: %w", node, err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size of %s: %w", node, err)
	}
	if size <= 0 {
		return fmt.Errorf("%s has no size", node)
	}
	return nil
}
