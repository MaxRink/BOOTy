//go:build linux

package crash

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

const defaultMountPoint = "/run/booty/crash-root"

// InspectOptions controls startup inspection.
type InspectOptions struct {
	MountPoint string
	OutputDir  string
	PstorePath string
	Log        *slog.Logger
}

// ShouldInspect reports whether crash inspection should run for cfg.
func ShouldInspect(cfg *config.MachineConfig) (ok bool, reason string) {
	if cfg == nil || !cfg.Provision.CrashArtifacts.Enabled {
		return false, "disabled"
	}
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		mode = "provision"
	}
	switch {
	case mode == "provision" || config.IsDeprovisionMode(mode):
		return true, ""
	case mode == "dry-run":
		return false, "dry-run"
	case mode == "standby":
		return false, "standby"
	default:
		return false, "unsupported-mode"
	}
}

// InspectStartup scans the existing target OS for crash artifacts and uploads them.
func InspectStartup(ctx context.Context, cfg *config.MachineConfig, diskMgr *disk.Manager, uploader Uploader, opts InspectOptions) (*InspectResult, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default().With("component", "crash")
	}
	if ok, reason := ShouldInspect(cfg); !ok {
		return &InspectResult{SkipReason: reason}, nil
	}
	if !hasUploadEndpoint(cfg) {
		return &InspectResult{Ran: true, SkipReason: "missing-upload-url"}, nil
	}
	if diskMgr == nil {
		return &InspectResult{Ran: true, SkipReason: "missing-disk-manager"}, nil
	}
	if uploader == nil {
		return &InspectResult{Ran: true, SkipReason: "missing-uploader"}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("crash inspection canceled: %w", err)
	}

	targetDisk, root, skipReason, err := resolveRootPartition(ctx, cfg, diskMgr)
	if err != nil {
		log.Warn("crash artifact root partition resolution failed", "reason", skipReason, "error", err)
		return &InspectResult{Ran: true, SkipReason: skipReason}, nil
	}

	mountPoint := opts.MountPoint
	if mountPoint == "" {
		mountPoint = defaultMountPoint
	}
	if err := os.MkdirAll(filepath.Dir(mountPoint), 0o755); err != nil {
		return &InspectResult{Ran: true, SkipReason: "mountpoint-create-failed"}, nil
	}
	if err := diskMgr.MountPartitionReadOnly(ctx, root.Node, mountPoint); err != nil {
		log.Warn("crash artifact read-only mount failed", "partition", root.Node, "error", err)
		return &InspectResult{Ran: true, SkipReason: "mount-failed"}, nil
	}
	defer func() {
		if err := diskMgr.Unmount(mountPoint); err != nil {
			log.Warn("crash artifact unmount failed", "mountpoint", mountPoint, "error", err)
		}
	}()

	collectResult, err := collectMountedRoot(ctx, cfg, targetDisk, root.Node, mountPoint, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		log.Warn("crash artifact collection failed", "error", err)
		return &InspectResult{Ran: true, SkipReason: "collection-failed"}, nil
	}
	return uploadCollectedCrash(ctx, uploader, collectResult, log), nil
}

func hasUploadEndpoint(cfg *config.MachineConfig) bool {
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.Provision.CrashArtifacts.PrepareURL) != "" || strings.TrimSpace(cfg.Provision.CrashArtifacts.UploadURL) != ""
}

func resolveRootPartition(ctx context.Context, cfg *config.MachineConfig, diskMgr *disk.Manager) (targetDisk string, root *disk.Partition, skipReason string, err error) {
	targetDisk, err = resolveTargetDisk(ctx, cfg, diskMgr)
	if err != nil {
		return "", nil, "disk-detection-failed", err
	}
	parts, err := diskMgr.ParsePartitions(ctx, targetDisk)
	if err != nil {
		return targetDisk, nil, "partition-parse-failed", err
	}
	root, err = diskMgr.FindRootPartition(parts)
	if err != nil {
		return targetDisk, nil, unsupportedPartitionReason(parts), err
	}
	return targetDisk, root, "", nil
}

func collectMountedRoot(ctx context.Context, cfg *config.MachineConfig, targetDisk, rootNode, mountPoint string, opts InspectOptions) (*CollectResult, error) {
	maxMB := cfg.Provision.CrashArtifacts.MaxMB
	if maxMB <= 0 {
		maxMB = config.DefaultCrashArtifactsMaxMB
	}
	return Collect(ctx, &CollectOptions{
		RootPath:      mountPoint,
		PstorePath:    opts.PstorePath,
		OutputDir:     opts.OutputDir,
		TargetDisk:    targetDisk,
		RootPartition: rootNode,
		MountPoint:    mountPoint,
		MaxBytes:      int64(maxMB) * 1024 * 1024,
		Config:        cfg,
	})
}

func uploadCollectedCrash(ctx context.Context, uploader Uploader, collectResult *CollectResult, log *slog.Logger) *InspectResult {
	result := &InspectResult{Ran: true, EvidenceFound: collectResult.EvidenceFound, ArchivePath: collectResult.ArchivePath, Manifest: &collectResult.Manifest}
	if !collectResult.EvidenceFound {
		result.SkipReason = "no-evidence"
		return result
	}

	req := &PrepareRequest{
		Manifest:      collectResult.Manifest,
		ArchiveBytes:  collectResult.Manifest.Scan.ArchiveBytes,
		ArtifactCount: len(collectResult.Manifest.Artifacts),
		TotalBytes:    collectResult.Manifest.Scan.TotalBytes,
	}
	if err := uploader.ReportCrashArtifacts(ctx, req, collectResult.ArchivePath); err != nil {
		if errors.Is(err, ErrNoUploadURL) {
			result.SkipReason = "missing-upload-url"
			return result
		}
		log.Warn("crash artifact upload failed", "error", err)
		result.UploadError = err
		return result
	}
	result.Uploaded = true
	return result
}

func resolveTargetDisk(ctx context.Context, cfg *config.MachineConfig, diskMgr *disk.Manager) (string, error) {
	if cfg != nil && cfg.Provision.Disk.PartitionLayout != nil {
		if device := strings.TrimSpace(cfg.Provision.Disk.PartitionLayout.Device); device != "" {
			return device, nil
		}
	}
	if cfg != nil && strings.TrimSpace(cfg.Provision.Disk.Device) != "" {
		return strings.TrimSpace(cfg.Provision.Disk.Device), nil
	}
	minSize := 0
	if cfg != nil {
		minSize = cfg.Provision.Disk.MinSizeGB
	}
	return diskMgr.DetectDisk(ctx, minSize)
}

func unsupportedPartitionReason(parts []disk.Partition) string {
	for _, part := range parts {
		if strings.EqualFold(part.Type, disk.LinuxLVMGUID) {
			return "lvm_root_unsupported"
		}
	}
	return "root-partition-not-found"
}
