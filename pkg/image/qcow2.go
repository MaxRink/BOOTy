//go:build linux

package image

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	exec "github.com/telekom/BOOTy/pkg/executil"
)

const (
	// ramdiskPath is the tmpfs mount point for qcow2 conversion scratch space.
	ramdiskPath = "/tmp/booty-ramdisk"
	// ramdiskSizeOpt is the tmpfs size option — bounded to avoid OOM.
	ramdiskSizeOpt = "size=4G"
)

func init() {
	convertQCOW2Hook = func(ctx context.Context, src io.Reader, sourceName, device string, opt StreamOpts) error {
		return ConvertQCOW2FromReader(ctx, src, sourceName, device, opt)
	}
}

// ConvertQCOW2 downloads a qcow2 image to a tmpfs ramdisk, converts it to raw
// using qemu-img, and streams the result to the target device.
//
// The flow is: download → tmpfs → qemu-img convert -f qcow2 -O raw → dd to device.
// The ramdisk is cleaned up after conversion.
func ConvertQCOW2(ctx context.Context, url, device string, opts ...StreamOpts) error {
	body, err := openSource(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	return ConvertQCOW2FromReader(ctx, body, url, device, opts...)
}

// ConvertQCOW2FromReader writes an already-open qcow2 stream to a tmpfs
// ramdisk, converts it to raw, and streams the result to the target device.
func ConvertQCOW2FromReader(ctx context.Context, src io.Reader, sourceName, device string, opts ...StreamOpts) error {
	slog.Info("qcow2 image detected, writing to ramdisk for conversion",
		"source", redactQCOW2SourceForLog(sourceName), "device", device)
	var opt StreamOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	if err := setupRamdisk(); err != nil {
		return fmt.Errorf("setting up ramdisk: %w", err)
	}
	defer cleanupRamdisk()

	qcow2Path := filepath.Join(ramdiskPath, "image.qcow2")
	rawPath := filepath.Join(ramdiskPath, "image.raw")

	if err := writeImageToFile(src, qcow2Path); err != nil {
		return fmt.Errorf("writing qcow2 image: %w", err)
	}

	if err := convertQCOW2ToRaw(ctx, qcow2Path, rawPath); err != nil {
		return fmt.Errorf("converting qcow2 to raw: %w", err)
	}

	// Remove qcow2 to free ramdisk space before streaming.
	_ = os.Remove(qcow2Path)

	if err := streamRawToDevice(rawPath, device, opt); err != nil {
		return fmt.Errorf("streaming raw image to device: %w", err)
	}

	return nil
}

func redactQCOW2SourceForLog(sourceName string) string {
	return RedactURL(sourceName)
}

// setupRamdisk creates and mounts a tmpfs at ramdiskPath.
func setupRamdisk() error {
	if err := os.MkdirAll(ramdiskPath, 0o700); err != nil {
		return fmt.Errorf("creating ramdisk dir: %w", err)
	}
	if err := syscall.Mount("tmpfs", ramdiskPath, "tmpfs", 0, ramdiskSizeOpt); err != nil {
		return fmt.Errorf("mounting tmpfs at %s: %w", ramdiskPath, err)
	}
	slog.Info("ramdisk mounted", "path", ramdiskPath)
	return nil
}

// cleanupRamdisk unmounts and removes the tmpfs ramdisk.
func cleanupRamdisk() {
	if err := syscall.Unmount(ramdiskPath, 0); err != nil {
		slog.Warn("failed to unmount ramdisk", "error", err)
	}
	_ = os.RemoveAll(ramdiskPath)
}

// downloadToFile downloads a URL to a local file path.
func downloadToFile(ctx context.Context, url, dest string) error {
	body, err := openSource(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	return writeImageToFile(body, dest)
}

func writeImageToFile(src io.Reader, dest string) error {
	f, err := os.Create(dest) //nolint:gosec // dest is a controlled ramdisk path
	if err != nil {
		return fmt.Errorf("creating file %s: %w", dest, err)
	}
	defer func() { _ = f.Close() }()

	counter := &WriteCounter{}
	stopProgress := startProgressTicker(counter)

	buf := make([]byte, imageCopyBufferSize)
	written, err := io.CopyBuffer(f, io.TeeReader(src, counter), buf)
	stopProgress()
	if err != nil {
		return fmt.Errorf("writing to file: %w", err)
	}
	fmt.Println()
	slog.Info("image written to ramdisk", "bytes", written)
	return nil
}

func requireQCOW2Converter() (string, error) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return "", fmt.Errorf("qemu-img not found (install qemu-utils): %w", err)
	}
	return qemuImg, nil
}

// convertQCOW2ToRaw runs qemu-img convert to transform qcow2 to raw.
func convertQCOW2ToRaw(ctx context.Context, src, dst string) error {
	qemuImg, err := requireQCOW2Converter()
	if err != nil {
		return err
	}

	if err := rejectQCOW2ExternalFiles(src); err != nil {
		return err
	}

	slog.Info("converting qcow2 to raw", "src", src, "dst", dst)
	cmd := exec.CommandContext(ctx, qemuImg, "convert", "-f", "qcow2", "-O", "raw", src, dst) //nolint:gosec // controlled paths
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert: %w", err)
	}

	slog.Info("qcow2 conversion complete")
	return nil
}

// qcow2Magic starts every qcow2 image ("QFI\xfb").
var qcow2Magic = []byte{0x51, 0x46, 0x49, 0xfb}

// qcow2HeaderLen covers the qcow2 v3 header fields up to
// incompatible_features.
const qcow2HeaderLen = 80

// qcow2IncompatExternalData is the incompatible feature bit for an external
// data file.
const qcow2IncompatExternalData = 1 << 2

// rejectQCOW2ExternalFiles reads the qcow2 header and fails for an image
// that names a backing file or an external data file. qemu-img convert, and
// qemu-img info in the QEMU that BOOTy ships, would open that file from the
// initramfs or the network and copy it into the raw image. The header is
// parsed in process, so nothing is opened before the check.
func rejectQCOW2ExternalFiles(path string) error {
	f, err := os.Open(path) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer func() { _ = f.Close() }()

	header := make([]byte, qcow2HeaderLen)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read qcow2 header: %w", err)
	}
	return checkQCOW2Header(header[:n])
}

func checkQCOW2Header(header []byte) error {
	if len(header) < 72 || !bytes.Equal(header[:4], qcow2Magic) {
		return fmt.Errorf("not a valid qcow2 header")
	}
	if binary.BigEndian.Uint64(header[8:16]) != 0 {
		return fmt.Errorf("qcow2 image has a backing file; only self-contained images are supported")
	}
	if binary.BigEndian.Uint32(header[4:8]) >= 3 {
		if len(header) < qcow2HeaderLen {
			return fmt.Errorf("qcow2 v3 header is truncated")
		}
		if binary.BigEndian.Uint64(header[72:80])&qcow2IncompatExternalData != 0 {
			return fmt.Errorf("qcow2 image has an external data file; only self-contained images are supported")
		}
	}
	return nil
}

// streamRawToDevice streams a raw image file to a block device.
func streamRawToDevice(rawPath, device string, opt StreamOpts) error {
	slog.Info("streaming raw image to device", "src", rawPath, "device", device)

	src, err := os.Open(rawPath) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("opening raw image: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(device, os.O_WRONLY, 0) //nolint:gosec // device from config
	if err != nil {
		return fmt.Errorf("opening device %s: %w", device, err)
	}
	defer func() { _ = dst.Close() }()

	counter := &WriteCounter{}
	stopProgress := startProgressTicker(counter)

	checksummed, h, err := wrapChecksum(src, opt)
	if err != nil {
		stopProgress()
		return err
	}

	buf := make([]byte, imageCopyBufferSize)
	written, err := io.CopyBuffer(dst, io.TeeReader(checksummed, counter), buf)
	stopProgress()
	if err != nil {
		slog.Error("qcow2 write failed: wiping partial image", "device", device, "error", err)
		wipeLeadingSectors(device)
		return fmt.Errorf("writing raw to device: %w", err)
	}

	if err := verifyChecksum(h, opt); err != nil {
		slog.Error("checksum mismatch: wiping partition table to prevent booting corrupt image",
			"device", device, "error", err)
		wipeLeadingSectors(device)
		return err
	}

	if err := syncImageTarget(dst, device); err != nil {
		slog.Error("qcow2 image sync failed: attempting to wipe partial image", "device", device, "error", err)
		wipeLeadingSectors(device)
		return err
	}

	fmt.Println()
	slog.Info("raw image written to device", "bytes", written)
	return nil
}
