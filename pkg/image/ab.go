//go:build linux

package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"strings"

	exec "github.com/telekom/BOOTy/pkg/executil"
)

const (
	efiSystemPartitionGUID = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	linuxFilesystemGUID    = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	efiSystemMBRType       = "ef"
	linuxFilesystemMBRType = "83"
)

// ABTargets names the already-created target partitions for an A/B image copy.
type ABTargets struct {
	Disk                string
	BootPartition       string
	RootPartition       string
	SourceRootLabel     string
	SourceRootPartition int
}

// RootTarget names an already-created target root partition for custom layouts.
type RootTarget struct {
	RootPartition       string
	SourceRootLabel     string
	SourceRootPartition int
}

type abDirtyTargetsError struct {
	err     error
	targets []string
}

func (e *abDirtyTargetsError) Error() string {
	return e.err.Error()
}

func (e *abDirtyTargetsError) Unwrap() error {
	return e.err
}

func dirtyABTargetsError(err error, targets ...string) error {
	if err == nil {
		return nil
	}
	var existing *abDirtyTargetsError
	if errors.As(err, &existing) {
		targets = append(targets, existing.targets...)
	}
	seen := make(map[string]struct{}, len(targets))
	unique := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		unique = append(unique, target)
	}
	if len(unique) == 0 {
		return err
	}
	return &abDirtyTargetsError{err: err, targets: unique}
}

func invalidateDirtyABTargets(err error) {
	var dirty *abDirtyTargetsError
	if !errors.As(err, &dirty) {
		return
	}
	for _, target := range dirty.targets {
		wipeLeadingSectors(target)
	}
}

func abRangeTargets(ranges []abStreamRange) []string {
	targets := make([]string, 0, len(ranges))
	for _, r := range ranges {
		targets = append(targets, r.dst)
	}
	return targets
}

// StreamAB downloads an OS image and copies its boot/root payload into an
// existing dual-root A/B layout. It does not replace the target partition table.
func StreamAB(ctx context.Context, url string, target ABTargets, opts ...StreamOpts) error {
	if strings.TrimSpace(target.RootPartition) == "" {
		return fmt.Errorf("a/b root partition is required")
	}
	slog.Info("a/b image streaming", "url", RedactURL(url), "disk", target.Disk, "root", target.RootPartition, "boot", target.BootPartition)
	return streamSelectedImagePayload(ctx, url, target, opts...)
}

// StreamRoot downloads an OS image and copies its selected root filesystem
// payload into an already-created target root partition. Partitioned source
// images use the same root selection rules as A/B streaming; plain filesystem
// images without a partition table are copied directly.
func StreamRoot(ctx context.Context, url string, target RootTarget, opts ...StreamOpts) error {
	if strings.TrimSpace(target.RootPartition) == "" {
		return fmt.Errorf("root partition is required")
	}
	slog.Info("root image streaming", "url", RedactURL(url), "root", target.RootPartition)
	return streamSelectedImagePayload(ctx, url, ABTargets{
		RootPartition:       target.RootPartition,
		SourceRootLabel:     target.SourceRootLabel,
		SourceRootPartition: target.SourceRootPartition,
	}, opts...)
}

func streamSelectedImagePayload(ctx context.Context, url string, target ABTargets, opts ...StreamOpts) error {
	var opt StreamOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt, err := withDownloadChecksum(opt)
	if err != nil {
		return err
	}

	src, cleanup, format, err := openAndDecompress(ctx, url, opt.download)
	if err != nil {
		return err
	}
	if format != FormatQCOW2 {
		defer cleanup()
		if err := streamABRaw(ctx, src, target, opt); err != nil {
			invalidateDirtyABTargets(err)
			return err
		}
		return nil
	}
	defer cleanup()
	return streamABViaRamdisk(ctx, src, target, opt)
}

func streamABViaRamdisk(ctx context.Context, src io.Reader, target ABTargets, opt StreamOpts) error {
	if err := setupRamdisk(); err != nil {
		return fmt.Errorf("setting up ramdisk: %w", err)
	}
	defer cleanupRamdisk()

	qcow2Path := ramdiskPath + "/image.qcow2"
	rawPath := ramdiskPath + "/image.raw"

	if err := writeImageToFile(src, qcow2Path); err != nil {
		return fmt.Errorf("writing qcow2 image to ramdisk: %w", err)
	}
	if err := convertQCOW2ToRaw(ctx, qcow2Path, rawPath); err != nil {
		return err
	}
	_ = os.Remove(qcow2Path)

	if err := verifyFileChecksum(rawPath, opt); err != nil {
		return err
	}

	if err := copyABPayload(ctx, rawPath, target); err != nil {
		invalidateDirtyABTargets(err)
		return err
	}
	return nil
}

func streamABRaw(ctx context.Context, src io.Reader, target ABTargets, opt StreamOpts) error {
	prefix, err := readStreamPrefix(src, streamABPrefixBytes)
	if err != nil {
		return err
	}

	stream, checksum, err := wrapChecksum(io.MultiReader(bytes.NewReader(prefix), src), opt)
	if err != nil {
		return err
	}

	parts, err := parseStreamPartitions(prefix)
	if errors.Is(err, errNoPartitionTable) {
		return streamABRootFilesystem(ctx, stream, checksum, target, opt)
	}
	if err != nil {
		return err
	}

	var ranges []abStreamRange
	if target.BootPartition != "" {
		if boot, ok := selectSourceBootPartition(parts); ok {
			ranges = append(ranges, abStreamRange{
				name:  "boot",
				start: int64(gptSectorSize) * boot.Start,
				size:  int64(gptSectorSize) * boot.Size,
				dst:   target.BootPartition,
			})
		} else {
			slog.Warn("source image has no EFI partition; leaving shared boot partition unchanged")
		}
	}

	root, err := selectSourceRootPartition(parts, target.SourceRootLabel, target.SourceRootPartition)
	if err != nil {
		return err
	}
	ranges = append(ranges, abStreamRange{
		name:  "root",
		start: int64(gptSectorSize) * root.Start,
		size:  int64(gptSectorSize) * root.Size,
		dst:   target.RootPartition,
	})

	// A download-scope checksum does not decompress the rest: verify()
	// hashes the remaining raw download. The zstd decoder runs without a
	// read-ahead goroutine (Decompressor), so nothing else reads the body.
	if err := copyABStreamRanges(ctx, stream, ranges, checksum != nil); err != nil {
		return err
	}
	if err := verifyChecksum(checksum, opt); err != nil {
		return dirtyABTargetsError(err, abRangeTargets(ranges)...)
	}
	return nil
}

func streamABRootFilesystem(ctx context.Context, stream io.Reader, checksum hash.Hash, target ABTargets, opt StreamOpts) error {
	slog.Info("source image has no partition table; copying as root filesystem")
	if err := streamReaderToDevice(ctx, stream, target.RootPartition); err != nil {
		return err
	}
	if err := verifyChecksum(checksum, opt); err != nil {
		return dirtyABTargetsError(err, target.RootPartition)
	}
	return nil
}

func verifyFileChecksum(path string, opt StreamOpts) error {
	if opt.Checksum == "" {
		return nil
	}
	opt, err := normalizeChecksumOpt(opt)
	if err != nil {
		return err
	}
	h, err := newHash(opt.ChecksumType)
	if err != nil {
		return err
	}
	f, err := os.Open(path) //nolint:gosec // controlled ramdisk path
	if err != nil {
		return fmt.Errorf("opening image for checksum: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only best-effort close
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing image: %w", err)
	}
	return verifyChecksum(h, opt)
}

func copyABPayload(ctx context.Context, rawPath string, target ABTargets) error {
	loopDev, err := setupLoopDevice(ctx, rawPath)
	if err != nil {
		slog.Info("source image is not loop-mountable as a disk image; copying as root filesystem", "error", err)
		return dirtyABTargetsError(ddFileToDevice(ctx, rawPath, target.RootPartition), target.RootPartition)
	}
	defer teardownLoopDevice(ctx, loopDev)

	srcParts, err := readSfdiskPartitions(ctx, loopDev)
	if err != nil || len(srcParts) == 0 {
		if err != nil {
			slog.Info("source image has no partition table; copying as root filesystem", "error", err)
		}
		return dirtyABTargetsError(ddFileToDevice(ctx, rawPath, target.RootPartition), target.RootPartition)
	}

	if _, err := runCmd(ctx, "partprobe", loopDev); err != nil {
		return fmt.Errorf("partprobe source image: %w", err)
	}

	var boot *sfdiskPartition
	if target.BootPartition != "" {
		if selected, ok := selectSourceBootPartition(srcParts); ok {
			boot = &selected
		} else {
			slog.Warn("source image has no EFI partition; leaving shared boot partition unchanged")
		}
	}

	root, err := selectSourceRootPartition(srcParts, target.SourceRootLabel, target.SourceRootPartition)
	if err != nil {
		return err
	}

	var dirtyTargets []string
	if boot != nil {
		slog.Info("copying source boot partition to shared A/B boot partition", "src", boot.Node, "dst", target.BootPartition)
		if err := ddPartition(ctx, boot.Node, target.BootPartition); err != nil {
			return dirtyABTargetsError(fmt.Errorf("copying boot partition: %w", err), target.BootPartition)
		}
		dirtyTargets = append(dirtyTargets, target.BootPartition)
	}

	slog.Info("copying source root partition to A/B target slot", "src", root.Node, "dst", target.RootPartition)
	if err := ddPartition(ctx, root.Node, target.RootPartition); err != nil {
		return dirtyABTargetsError(fmt.Errorf("copying root partition: %w", err), append(dirtyTargets, target.RootPartition)...)
	}
	return nil
}

func selectSourceBootPartition(parts []sfdiskPartition) (sfdiskPartition, bool) {
	for _, part := range parts {
		if isEFISystemPartition(part) {
			return part, true
		}
	}
	return sfdiskPartition{}, false
}

var commonSourceRootLabels = map[string]struct{}{
	"root":              {},
	"root-a":            {},
	"root-b":            {},
	"rootfs":            {},
	"cloudimg-rootfs":   {},
	"booty-root-a":      {},
	"booty-root-b":      {},
	"ubuntu-root":       {},
	"debian-root":       {},
	"fedora-root":       {},
	"opensuse-root":     {},
	"opensuse-rootfs":   {},
	"flatcar-root":      {},
	"bottlerocket-root": {},
}

func selectSourceRootPartition(parts []sfdiskPartition, label string, number int) (sfdiskPartition, error) {
	if number > 0 {
		for _, part := range parts {
			if part.Number == number {
				if isEFISystemPartition(part) {
					return sfdiskPartition{}, fmt.Errorf("source image partition number %d is EFI, not a root partition", number)
				}
				return part, nil
			}
		}
		return sfdiskPartition{}, fmt.Errorf("source image has no partition number %d", number)
	}

	if strings.TrimSpace(label) != "" {
		return selectSingleSourceRootCandidate(parts, func(part sfdiskPartition) bool {
			return strings.EqualFold(strings.TrimSpace(part.Name), strings.TrimSpace(label))
		}, fmt.Sprintf("label %q", strings.TrimSpace(label)))
	}

	if hasFlatcarUSRSlot(parts) {
		return sfdiskPartition{}, fmt.Errorf(
			"source image has Flatcar-like USR-A/USR-B slots; %s",
			sourceRootSelectorHint,
		)
	}

	if part, err := selectSingleSourceRootCandidate(parts, hasCommonSourceRootLabel, "common root partition label"); err == nil {
		return part, nil
	} else if !errors.Is(err, errNoSourceRootCandidate) {
		return sfdiskPartition{}, err
	}

	if part, err := selectSingleSourceRootCandidate(parts, isLinuxFilesystemPartition, "Linux filesystem partition"); err == nil {
		return part, nil
	} else if !errors.Is(err, errNoSourceRootCandidate) {
		return sfdiskPartition{}, err
	}

	return sfdiskPartition{}, fmt.Errorf(
		"source image has no Linux root partition candidate; %s for supported Linux images without a standard root label or partition type",
		sourceRootSelectorHint,
	)
}

var errNoSourceRootCandidate = errors.New("source image has no root partition candidate")

const sourceRootSelectorHint = "set provision.image.sourceRootLabel/provision.image.sourceRootPartition for partition-layout root streaming, or provision.ab.sourceRootLabel/provision.ab.sourceRootPartition for A/B streaming"

func selectSingleSourceRootCandidate(parts []sfdiskPartition, match func(sfdiskPartition) bool, reason string) (sfdiskPartition, error) {
	var candidate sfdiskPartition
	count := 0
	for _, part := range parts {
		if !match(part) {
			continue
		}
		candidate = part
		count++
	}
	switch count {
	case 0:
		return sfdiskPartition{}, errNoSourceRootCandidate
	case 1:
		return candidate, nil
	default:
		return sfdiskPartition{}, fmt.Errorf("source image has %d %s candidates; %s", count, reason, sourceRootSelectorHint)
	}
}

func hasCommonSourceRootLabel(part sfdiskPartition) bool {
	if isEFISystemPartition(part) {
		return false
	}
	_, ok := commonSourceRootLabels[normalizeSourceRootLabel(part.Name)]
	return ok
}

func hasFlatcarUSRSlot(parts []sfdiskPartition) bool {
	for _, part := range parts {
		if isFlatcarUSRSlotLabel(part.Name) {
			return true
		}
	}
	return false
}

func isFlatcarUSRSlotLabel(label string) bool {
	switch normalizeSourceRootLabel(label) {
	case "usr-a", "usr-b":
		return true
	default:
		return false
	}
}

func normalizeSourceRootLabel(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	label = strings.ReplaceAll(label, "_", "-")
	return label
}

func isLinuxFilesystemPartition(part sfdiskPartition) bool {
	return strings.EqualFold(part.Type, linuxFilesystemGUID) ||
		strings.EqualFold(part.Type, linuxFilesystemMBRType)
}

func isEFISystemPartition(part sfdiskPartition) bool {
	return strings.EqualFold(part.Type, efiSystemPartitionGUID) ||
		strings.EqualFold(part.Type, efiSystemMBRType)
}

func ddFileToDevice(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "dd", //nolint:gosec // controlled ramdisk path and target partition
		"if="+src, "of="+dst, "bs=4M", "conv=fsync", "status=progress",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dd %s to %s: %w", src, dst, err)
	}
	return nil
}
