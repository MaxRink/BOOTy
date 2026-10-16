//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/telekom/BOOTy/pkg/espsync"
	"golang.org/x/sys/unix"
)

func main() {
	source := flag.String("source", "/boot/efi", "mounted source ESP")
	firstPartUUID := flag.String("partuuid-a", "", "first mirrored ESP PARTUUID")
	secondPartUUID := flag.String("partuuid-b", "", "second mirrored ESP PARTUUID")
	sourcePartUUID := flag.String("source-partuuid", "", "legacy fixed source ESP PARTUUID")
	destination := flag.String("destination", "", "destination ESP block device or mounted directory")
	mountpoint := flag.String("mountpoint", "/run/booty/esp-sync", "private destination mountpoint")
	lockPath := flag.String("lock", "/run/lock/booty-esp-sync.lock", "synchronization lock")
	marker := flag.String("marker", "/run/booty/esp-sync.ready", "optional marker written after a successful synchronization")
	once := flag.Bool("once", false, "synchronize once and exit")
	flag.Parse()
	if (*firstPartUUID == "") != (*secondPartUUID == "") {
		fatal(errors.New("partuuid-a and partuuid-b must be provided together"))
	}
	if *destination == "" && *firstPartUUID == "" {
		fatal(errors.New("destination is required"))
	}
	if *once {
		runOnce(*source, *sourcePartUUID, *firstPartUUID, *secondPartUUID, *destination, *mountpoint, *lockPath, *marker)
		return
	}
	if err := runService(*source, *sourcePartUUID, *firstPartUUID, *secondPartUUID, *destination, *mountpoint, *lockPath, *marker); err != nil {
		fatal(err)
	}
}

func runOnce(source, sourcePartUUID, firstPartUUID, secondPartUUID, destination, mountpoint, lockPath, marker string) {
	if firstPartUUID != "" {
		pair := &pairSync{source: source, firstPartUUID: firstPartUUID, secondPartUUID: secondPartUUID,
			mountpoint: mountpoint, lockPath: lockPath, marker: marker}
		if err := pair.sync(); err != nil {
			fatal(err)
		}
		return
	}
	mounted, err := mountDestination(destination, mountpoint)
	if err != nil {
		fatal(err)
	}
	if mounted {
		defer unmount(mountpoint)
	}
	if sourcePartUUID != "" {
		if err := espsync.ValidateMountedSource(source, sourcePartUUID); err != nil {
			fatal(err)
		}
	}
	if err := espsync.RunOnce(source, mountpointOrDestination(mounted, mountpoint, destination), lockPath); err != nil {
		fatal(err)
	}
	if err := espsync.WriteMarker(marker); err != nil {
		fatal(err)
	}
}

func runService(source, sourcePartUUID, firstPartUUID, secondPartUUID, destination, mountpoint, lockPath, marker string) error {
	if firstPartUUID != "" {
		pair := &pairSync{source: source, firstPartUUID: firstPartUUID, secondPartUUID: secondPartUUID,
			mountpoint: mountpoint, lockPath: lockPath, marker: marker}
		if err := pair.sync(); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		// pair.sync writes the marker itself, only after a real copy.
		return espsync.WatchFunc(source, pair.sync, ctx.Done())
	}
	mounted, err := mountDestination(destination, mountpoint)
	if err != nil {
		return err
	}
	if mounted {
		defer unmount(mountpoint)
	}
	target := mountpointOrDestination(mounted, mountpoint, destination)
	if sourcePartUUID != "" {
		if err := espsync.ValidateMountedSource(source, sourcePartUUID); err != nil {
			return err
		}
	}
	if err := espsync.RunOnce(source, target, lockPath); err != nil {
		return err
	}
	if err := espsync.WriteMarker(marker); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return espsync.WatchWithMarker(source, target, lockPath, marker, ctx.Done())
}

// peerDevice resolves the other ESP of the mirrored pair; tests replace it.
var peerDevice = espsync.PeerDevice

// pairSync copies the mounted ESP to the other ESP of a mirrored pair.
type pairSync struct {
	source, firstPartUUID, secondPartUUID, mountpoint, lockPath, marker string
	// degraded is true while the peer ESP is missing, so the degraded
	// state is logged once and not on every change of the source.
	degraded bool
}

// sync copies the source ESP to its peer. When the peer partition is missing,
// for example after a boot from one disk of a degraded mirror, there is
// nothing to synchronize: it logs that once and returns nil, so the service
// keeps watching instead of exiting and restarting in a loop.
func (p *pairSync) sync() error {
	return espsync.WithLock(p.lockPath, func() error {
		destination, err := peerDevice(p.source, p.firstPartUUID, p.secondPartUUID)
		if err != nil {
			return err
		}
		if _, err := os.Stat(destination); errors.Is(err, fs.ErrNotExist) {
			if !p.degraded {
				slog.Warn("peer EFI system partition is missing, the mirror is degraded; nothing to synchronize",
					"source", p.source, "peer", destination)
				p.degraded = true
			}
			return nil
		}
		if p.degraded {
			slog.Info("peer EFI system partition is back", "peer", destination)
			p.degraded = false
		}
		mounted, err := mountDestination(destination, p.mountpoint)
		if err != nil {
			return err
		}
		if mounted {
			defer unmount(p.mountpoint)
		}
		target := mountpointOrDestination(mounted, p.mountpoint, destination)
		slog.Info("synchronizing EFI system partition", "source", p.source, "destination", target)
		if err := espsync.CopyTree(p.source, target); err != nil {
			return err
		}
		return espsync.WriteMarker(p.marker)
	})
}

func mountDestination(device, mountpoint string) (bool, error) {
	info, err := os.Stat(device)
	if err == nil && info.IsDir() {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("stat ESP device: %w", err)
	}
	if !filepath.IsAbs(mountpoint) {
		return false, fmt.Errorf("mountpoint must be absolute")
	}
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		return false, fmt.Errorf("create ESP mountpoint: %w", err)
	}
	if err := unix.Mount(device, mountpoint, "vfat", unix.MS_NODEV|unix.MS_NOSUID|unix.MS_NOEXEC, "umask=0077"); err != nil {
		return false, fmt.Errorf("mount destination ESP: %w", err)
	}
	return true, nil
}

func unmount(path string) {
	if err := unix.Unmount(path, 0); err != nil {
		slog.Warn("unmount destination ESP", "path", path, "error", err)
	}
}
func mountpointOrDestination(mounted bool, mountpoint, destination string) string {
	if mounted {
		return mountpoint
	}
	return destination
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
