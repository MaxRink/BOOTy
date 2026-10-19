//go:build linux

package image

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
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
// in process, and streams the result to the target device.
//
// The flow is: download → tmpfs → qcow2 to raw → dd to device.
// The ramdisk is cleaned up after conversion.
func ConvertQCOW2(ctx context.Context, url, device string, opts ...StreamOpts) error {
	var opt StreamOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt, err := withDownloadChecksum(opt)
	if err != nil {
		return err
	}
	body, err := openSource(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	src := opt.download.verifyAtEOF(opt.download.source(body))
	return ConvertQCOW2FromReader(ctx, src, url, device, opt)
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

	if err := checkQCOW2FitsDevice(qcow2Path, device); err != nil {
		return err
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

// downloadToFile downloads a URL to a local file path. A non-nil download
// checksum is verified before the download counts as complete.
func downloadToFile(ctx context.Context, url, dest string, download *downloadChecksum) error {
	body, err := openSource(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	return writeImageToFile(download.verifyAtEOF(download.source(body)), dest)
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

// convertQCOW2ToRaw converts the qcow2 image src to the raw file dst in
// process (qcow2raw.go), like `qemu-img convert -f qcow2 -O raw`.
func convertQCOW2ToRaw(ctx context.Context, src, dst string) error {
	slog.Info("converting qcow2 to raw", "src", src, "dst", dst)
	in, err := os.Open(src) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("create raw image: %w", err)
	}
	if err := qcow2ToRaw(ctx, in, out); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("qcow2 to raw: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close raw image: %w", err)
	}
	slog.Info("qcow2 conversion complete")
	return nil
}

// checkQCOW2FitsDevice fails, before the conversion, when the virtual size
// of the qcow2 image is larger than the target device. A regular file as
// target (tests) has no fixed size and is not checked.
func checkQCOW2FitsDevice(qcow2Path, device string) error {
	fi, err := os.Stat(device)
	if err != nil || fi.Mode()&os.ModeDevice == 0 {
		return nil //nolint:nilerr // the write reports a missing device
	}
	src, err := os.Open(qcow2Path) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer func() { _ = src.Close() }()
	h, err := readQCOW2Header(src)
	if err != nil {
		return err
	}
	dev, err := os.Open(device) //nolint:gosec // device from config
	if err != nil {
		return fmt.Errorf("open target device %s: %w", device, err)
	}
	defer func() { _ = dev.Close() }()
	size, err := dev.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size of target device %s: %w", device, err)
	}
	if h.size > uint64(size) { //nolint:gosec // a size is not negative
		return fmt.Errorf("qcow2 virtual size %d bytes is larger than the target device %s (%d bytes)", h.size, device, size)
	}
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
