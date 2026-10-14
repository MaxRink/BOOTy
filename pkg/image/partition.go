//go:build linux

package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/blockdev"
	exec "github.com/telekom/BOOTy/pkg/executil"
)

// StreamPartitions downloads an image to a tmpfs ramdisk, optionally converts
// qcow2 to raw, then copies each partition individually from the source image
// to the corresponding partition on the target disk.
//
// This preserves the partition table on the target disk and allows the source
// image partitions to differ in size from the target.
func StreamPartitions(ctx context.Context, url, device string, opts ...StreamOpts) error {
	slog.Info("partition-by-partition imaging", "url", RedactURL(url), "device", device)

	var opt StreamOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt, err := withDownloadChecksum(opt)
	if err != nil {
		return err
	}

	if err := setupRamdisk(); err != nil {
		return fmt.Errorf("setting up ramdisk: %w", err)
	}
	defer cleanupRamdisk()

	rawPath, err := downloadAndPrepareRaw(ctx, url, opt.download)
	if err != nil {
		return err
	}

	if err := verifyRawImageChecksum(rawPath, opt); err != nil {
		return err
	}

	return copyPartitions(ctx, rawPath, device)
}

func verifyRawImageChecksum(rawPath string, opt StreamOpts) error {
	if opt.Checksum == "" {
		return opt.download.verify()
	}

	src, err := os.Open(rawPath) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("opening raw image for checksum: %w", err)
	}
	defer func() { _ = src.Close() }()

	checksummed, h, err := wrapChecksum(src, opt)
	if err != nil {
		return err
	}
	buf := make([]byte, imageCopyBufferSize)
	if _, err := io.CopyBuffer(io.Discard, checksummed, buf); err != nil {
		return fmt.Errorf("reading raw image for checksum: %w", err)
	}
	return verifyChecksum(h, opt)
}

// downloadAndPrepareRaw downloads an image to the ramdisk, decompresses it
// if needed, and converts qcow2 to raw. Returns path to the raw image.
func downloadAndPrepareRaw(ctx context.Context, url string, download *downloadChecksum) (string, error) {
	downloadPath := ramdiskPath + "/image.download"

	if err := downloadToFile(ctx, url, downloadPath, download); err != nil {
		return "", fmt.Errorf("downloading image: %w", err)
	}

	// Detect format of the downloaded file.
	f, err := os.Open(downloadPath) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return "", fmt.Errorf("opening downloaded image: %w", err)
	}
	format, _, err := DetectFormat(f)
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("detecting image format: %w", err)
	}
	slog.Info("downloaded image format", "format", format)

	rawPath := ramdiskPath + "/image.raw"

	if isUnsupportedContainerFormat(format) {
		return "", unsupportedImageFormatError(format)
	}

	switch format {
	case FormatQCOW2:
		if err := convertQCOW2ToRaw(ctx, downloadPath, rawPath); err != nil {
			return "", err
		}
		_ = os.Remove(downloadPath)
		return rawPath, nil

	case FormatGzip, FormatZstd, FormatLZ4, FormatXZ, FormatBzip2:
		if err := decompressFile(ctx, downloadPath, rawPath, format); err != nil {
			return "", err
		}
		_ = os.Remove(downloadPath)
		return convertPreparedQCOW2(ctx, rawPath)

	case FormatRaw:
		// Already raw — just rename.
		if err := os.Rename(downloadPath, rawPath); err != nil {
			return "", fmt.Errorf("renaming to raw: %w", err)
		}
		return rawPath, nil
	}

	return downloadPath, nil
}

func convertPreparedQCOW2(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return "", fmt.Errorf("opening prepared image: %w", err)
	}
	format, _, err := DetectFormat(f)
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("detecting prepared image format: %w", err)
	}
	if format != FormatQCOW2 {
		return path, nil
	}

	rawPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".converted.raw"
	if err := convertQCOW2ToRaw(ctx, path, rawPath); err != nil {
		return "", err
	}
	_ = os.Remove(path)
	return rawPath, nil
}

// decompressFile decompresses a file on the ramdisk to a new output file.
func decompressFile(_ context.Context, src, dst string, format Format) error {
	slog.Info("decompressing image on ramdisk", "format", format, "src", src)

	in, err := os.Open(src) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("opening compressed file: %w", err)
	}
	defer func() { _ = in.Close() }()

	reader, closer, err := Decompressor(in, format)
	if err != nil {
		return fmt.Errorf("creating decompressor: %w", err)
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}

	out, err := os.Create(dst) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer func() { _ = out.Close() }()

	written, err := io.Copy(out, reader)
	if err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}
	slog.Info("decompression complete", "bytes", written)
	return nil
}

// copyPartitions sets up a loop device for the source raw image, reads its
// partition table, then copies each partition's data to the matching partition
// on the target disk.
func copyPartitions(ctx context.Context, rawPath, targetDisk string) error {
	// Set up loop device for the raw image.
	loopDev, err := setupLoopDevice(ctx, rawPath)
	if err != nil {
		return fmt.Errorf("setting up loop device: %w", err)
	}
	defer teardownLoopDevice(ctx, loopDev)

	// Force kernel to re-read partition table on loop device.
	if err := settlePartitionTable(ctx, loopDev); err != nil {
		return fmt.Errorf("settling loop device partitions: %w", err)
	}

	// Copy partition table from source to target.
	slog.Info("copying partition table to target disk", "target", targetDisk)
	if err := copyPartitionTable(ctx, loopDev, targetDisk); err != nil {
		return fmt.Errorf("copying partition table: %w", err)
	}

	// Re-read target partition table and trigger partition device creation.
	if err := settlePartitionTable(ctx, targetDisk); err != nil {
		return fmt.Errorf("settling target partitions: %w", err)
	}

	// Read partitions from source loop device.
	srcParts, err := readSfdiskPartitions(ctx, loopDev)
	if err != nil {
		return fmt.Errorf("reading source partitions: %w", err)
	}

	if len(srcParts) == 0 {
		return fmt.Errorf("source image has no partitions")
	}

	// Copy each partition.
	for i, sp := range srcParts {
		srcNode := sp.Node
		tgtNode, partNum := targetPartitionNodeForSource(targetDisk, sp, i+1)
		slog.Info("copying partition",
			"source_partition", partNum, "src", srcNode, "dst", tgtNode,
			"type", sp.Type, "sectors", sp.Size)

		if err := waitForPartitionDevice(ctx, srcNode); err != nil {
			return fmt.Errorf("source partition %s not ready: %w", srcNode, err)
		}
		if err := waitForPartitionDevice(ctx, tgtNode); err != nil {
			return fmt.Errorf("target partition %s not ready: %w", tgtNode, err)
		}
		if err := ddPartition(ctx, srcNode, tgtNode); err != nil {
			return fmt.Errorf("copying partition %d (%s -> %s): %w", partNum, srcNode, tgtNode, err)
		}
	}

	slog.Info("partition-by-partition imaging complete", "partitions", len(srcParts))
	return nil
}

// sfdiskPartition mirrors the sfdisk JSON partition entry.
type sfdiskPartition struct {
	Node   string `json:"node"`
	Start  int64  `json:"start"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Number int    `json:"-"`
}

// readSfdiskPartitions reads partition entries from a disk/loop device.
func readSfdiskPartitions(ctx context.Context, dev string) ([]sfdiskPartition, error) {
	out, err := runCmd(ctx, "sfdisk", "--json", dev)
	if err != nil {
		return nil, fmt.Errorf("sfdisk --json %s: %w", dev, err)
	}

	jsonBytes := out
	if idx := bytes.IndexByte(out, '{'); idx > 0 {
		jsonBytes = out[idx:]
	}

	var result struct {
		PartitionTable struct {
			Partitions []sfdiskPartition `json:"partitions"`
		} `json:"partitiontable"`
	}
	if err := json.Unmarshal(jsonBytes, &result); err != nil {
		return nil, fmt.Errorf("parsing sfdisk output: %w", err)
	}
	for i := range result.PartitionTable.Partitions {
		result.PartitionTable.Partitions[i].Number = partitionNumberFromNode(result.PartitionTable.Partitions[i].Node, i+1)
	}
	return result.PartitionTable.Partitions, nil
}

func partitionNumberFromNode(node string, fallback int) int {
	end := len(node)
	start := end
	for start > 0 && node[start-1] >= '0' && node[start-1] <= '9' {
		start--
	}
	if start == end {
		return fallback
	}
	n, err := strconv.Atoi(node[start:end])
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func targetPartitionNodeForSource(targetDisk string, part sfdiskPartition, fallback int) (node string, number int) {
	partNum := part.Number
	if partNum <= 0 {
		partNum = fallback
	}
	return targetPartitionNode(targetDisk, partNum), partNum
}

// copyPartitionTable copies the GPT/MBR partition table from src to dst using sfdisk.
func copyPartitionTable(ctx context.Context, src, dst string) error {
	dump, err := runCmd(ctx, "sfdisk", "--dump", src)
	if err != nil {
		return fmt.Errorf("dumping partition table: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sfdisk", "--force", dst) //nolint:gosec // controlled device path
	cmd.Stdin = bytes.NewReader(dump)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("applying partition table: %w\n%s", err, string(out))
	}
	return nil
}

// setupLoopDevice attaches a raw image to a loop device and returns the device path.
func setupLoopDevice(ctx context.Context, imagePath string) (string, error) {
	out, err := runCmd(ctx, "losetup", "--find", "--show", "--partscan", imagePath)
	if err != nil {
		return "", fmt.Errorf("losetup: %w", err)
	}
	dev := strings.TrimSpace(string(out))
	slog.Info("loop device attached", "device", dev, "image", imagePath)
	return dev, nil
}

// teardownLoopDevice detaches a loop device.
func teardownLoopDevice(ctx context.Context, dev string) {
	if _, err := runCmd(ctx, "losetup", "--detach", dev); err != nil {
		slog.Warn("failed to detach loop device", "device", dev, "error", err)
	}
}

// ddPartition copies data from src partition device to dst partition device.
func ddPartition(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "dd", //nolint:gosec // controlled device paths from partition table
		"if="+src, "of="+dst, "bs=4M", "conv=fsync", "status=progress",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dd %s to %s: %w", src, dst, err)
	}
	return nil
}

func settlePartitionTable(ctx context.Context, disk string) error {
	if out, err := runCmd(ctx, "sync"); err != nil {
		slog.Warn("sync before partition settle failed", "disk", disk, "error", err, "output", string(out))
	}

	out, err := runCmd(ctx, "partprobe", disk)
	if err != nil {
		slog.Warn("partprobe failed, trying blockdev --rereadpt", "disk", disk, "error", err, "output", string(out))
		out, err = runCmd(ctx, "blockdev", "--rereadpt", disk)
		if err != nil {
			return fmt.Errorf("re-read partition table %s: %s: %w", disk, string(out), err)
		}
	}

	if out, err := runCmd(ctx, "mdev", "-s"); err != nil {
		slog.Warn("mdev -s failed after partition settle", "disk", disk, "error", err, "output", string(out))
	}
	return nil
}

const (
	partitionDeviceWaitInterval = 500 * time.Millisecond
	partitionDeviceWaitAttempts = 20
)

func waitForPartitionDevice(ctx context.Context, device string) error {
	ticker := time.NewTicker(partitionDeviceWaitInterval)
	defer ticker.Stop()

	for i := range partitionDeviceWaitAttempts {
		if err := statPartitionDevice(device); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if out, err := runCmd(ctx, "mdev", "-s"); err != nil {
			slog.Debug("mdev -s failed while waiting for partition device", "device", device, "error", err, "output", string(out))
		}
		if err := statPartitionDevice(device); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		slog.Debug("waiting for partition device", "device", device, "iteration", i)
		if i == partitionDeviceWaitAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait canceled: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return fmt.Errorf("device node did not appear after %s", partitionDeviceWaitInterval*partitionDeviceWaitAttempts)
}

func statPartitionDevice(device string) error {
	if _, err := statPath(device); err != nil {
		if os.IsNotExist(err) {
			return err
		}
		return fmt.Errorf("stat partition device %s: %w", device, err)
	}
	return nil
}

// targetPartitionNode derives the partition device node for a given disk and
// partition number using the same device naming rules as the disk package.
func targetPartitionNode(disk string, partNum int) string {
	return blockdev.PartitionDevicePath(disk, partNum)
}

var runCmd = runCommand
var statPath = os.Stat

// runCommand executes a command and returns its combined output.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // controlled arguments
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
