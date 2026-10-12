//go:build linux

package provision

import (
	"context"
	"fmt"
	"os"

	"github.com/telekom/BOOTy/pkg/disk"
)

// probeRootPartition picks the first Linux filesystem partition that holds
// a root filesystem, like select_root_partition in the CAPRF legacy
// provision.sh: /etc/os-release, or /etc and /usr directories.
func (o *Orchestrator) probeRootPartition(ctx context.Context, parts []disk.Partition, ambiguous error) (*disk.Partition, error) {
	// On resume, mount-root may already have mounted the root. Reuse that
	// choice instead of mounting the active root a second time.
	if source, ok := mountedSource(o.config.rootDir); ok {
		for i := range parts {
			if isLinuxRootPartition(parts[i]) && sameMountSource(source, parts[i].Node) && looksLikeRootFilesystem(o.config.rootDir) {
				o.log.Info("root probe: using the mounted root partition", "partition", parts[i].Node)
				return &parts[i], nil
			}
		}
	}
	inspect, err := os.MkdirTemp("", "booty-root-probe-")
	if err != nil {
		return nil, fmt.Errorf("create root probe mountpoint: %w", err)
	}
	defer func() { _ = os.Remove(inspect) }()

	for i := range parts {
		if !isLinuxRootPartition(parts[i]) {
			continue
		}
		if err := o.disk.MountPartitionInspect(ctx, parts[i].Node, inspect); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("root probe: %w", ctxErr)
			}
			o.log.Info("root probe: cannot mount partition", "partition", parts[i].Node, "error", err)
			continue
		}
		isRoot := looksLikeRootFilesystem(inspect)
		if err := o.disk.Unmount(inspect); err != nil {
			return nil, fmt.Errorf("unmount root probe %s: %w", parts[i].Node, err)
		}
		if isRoot {
			o.log.Info("root probe: selected root partition", "partition", parts[i].Node)
			return &parts[i], nil
		}
	}
	return nil, fmt.Errorf("%w; no candidate holds a root filesystem", ambiguous)
}

func looksLikeRootFilesystem(dir string) bool {
	// os.Root resolves symlinks inside dir only, so a candidate with
	// etc -> /etc is judged by its own content, not by the ramdisk.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close() //nolint:errcheck // read-only handle
	if info, err := root.Stat("etc/os-release"); err == nil && info.Mode().IsRegular() {
		return true
	}
	etc, etcErr := root.Stat("etc")
	usr, usrErr := root.Stat("usr")
	return etcErr == nil && usrErr == nil && etc.IsDir() && usr.IsDir()
}
