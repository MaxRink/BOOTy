//go:build linux

package provision

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/serialconsole"
)

const (
	// serialConsoleDropInName is the drop-in BOOTy owns inside the
	// serial-getty instance directory of the provisioned system.
	serialConsoleDropInName = "10-booty-console.conf"
	// serialConsoleArtifactDir is the durable location of the resolution
	// artifact on the provisioned root filesystem.
	serialConsoleArtifactDir = "/var/lib/booty"
	// serialConsoleRunDirDefault is where the artifact is published inside
	// the initramfs so CAPRF can collect it during the provisioning run.
	serialConsoleRunDirDefault = "/run/booty"

	gettyWantsDir       = "/etc/systemd/system/getty.target.wants"
	systemdUnitDir      = "/etc/systemd/system"
	serialGettyTemplate = "serial-getty@.service"
)

// serialGettyTemplateCandidates are the vendor unit locations searched in the
// provisioned root, most specific first.
var serialGettyTemplateCandidates = []string{
	"/usr/lib/systemd/system/" + serialGettyTemplate,
	"/lib/systemd/system/" + serialGettyTemplate,
}

// serialConsoleRunDir is the initramfs directory the artifact is published to
// for CAPRF collection during the provisioning run.
func (c *Configurator) serialConsoleRunDir() string {
	if c.runDir != "" {
		return c.runDir
	}
	return serialConsoleRunDirDefault
}

// resolveSerialConsole resolves the console once per Configurator and caches
// the outcome so the kernel command line and the getty configuration can never
// disagree.
func (c *Configurator) resolveSerialConsole(cfg *config.MachineConfig) (serialconsole.Resolution, error) {
	if c.serialConsoleDone {
		return c.serialConsole, c.serialConsoleErr
	}
	resolver := &serialconsole.Resolver{
		SysRoot:           c.hostSysRoot,
		ProcRoot:          c.hostProcRoot,
		Override:          serialConsoleOverride(cfg),
		ExtraKernelParams: extraKernelParams(cfg),
	}
	resolution, err := resolver.Resolve()
	c.serialConsole, c.serialConsoleErr, c.serialConsoleDone = resolution, err, true
	slog.Info("resolved serial console", resolution.LogAttrs()...)
	return resolution, err
}

func serialConsoleOverride(cfg *config.MachineConfig) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Provision.SerialConsole)
}

func extraKernelParams(cfg *config.MachineConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.Provision.ExtraKernelParams
}

// serialConsoleKernelParam returns the single console= parameter for the
// provisioned kernel command line, or an empty string when no serial console
// must be configured.
func (c *Configurator) serialConsoleKernelParam(cfg *config.MachineConfig) (string, error) {
	resolution, err := c.resolveSerialConsole(cfg)
	if err != nil {
		return "", fmt.Errorf("resolve serial console: %w", err)
	}
	return resolution.KernelParam(), nil
}

// stripConsoleParams removes every console= token from operator-supplied
// kernel parameters. The resolver has already folded operator intent into the
// resolution, so re-appending the original tokens would emit more than one
// console and let the last one silently win.
func stripConsoleParams(params string) (kept string, dropped []string) {
	fields := strings.Fields(params)
	keptFields := make([]string, 0, len(fields))
	for _, field := range fields {
		if strings.HasPrefix(field, "console=") {
			dropped = append(dropped, field)
			continue
		}
		keptFields = append(keptFields, field)
	}
	return strings.Join(keptFields, " "), dropped
}

// ConfigureSerialConsole writes the serial getty configuration of the
// provisioned system and persists the machine-readable resolution artifact.
//
// It fails closed: when the evidence is ambiguous the artifact is still
// written (so CAPRF can see why) and provisioning stops instead of
// configuring a console that may be wrong.
func (c *Configurator) ConfigureSerialConsole(cfg *config.MachineConfig) error {
	resolution, resolveErr := c.resolveSerialConsole(cfg)
	artifact := serialconsole.NewArtifact(&resolution, time.Now(), resolveErr)
	data, marshalErr := serialconsole.MarshalArtifact(&artifact)
	if marshalErr != nil {
		return marshalErr
	}
	if err := c.persistSerialConsoleArtifact(data); err != nil {
		return err
	}
	if resolveErr != nil {
		return fmt.Errorf("resolve serial console: %w", resolveErr)
	}
	if resolution.Console == nil {
		slog.Warn("no serial console configured", resolution.LogAttrs()...)
		return c.pruneSerialGettyUnits("")
	}
	if err := c.writeSerialGettyDropIn(*resolution.Console); err != nil {
		return err
	}
	if err := c.enableSerialGettyUnit(*resolution.Console); err != nil {
		return err
	}
	return c.pruneSerialGettyUnits(resolution.Console.GettyUnit())
}

// persistSerialConsoleArtifact writes the artifact to the provisioned root and
// to the initramfs run directory consumed by CAPRF.
func (c *Configurator) persistSerialConsoleArtifact(data []byte) error {
	targetPath, err := c.targetPath(path.Join(serialConsoleArtifactDir, serialconsole.ArtifactFileName))
	if err != nil {
		return fmt.Errorf("serial console artifact path: %w", err)
	}
	if err := writeFileAtomic(targetPath, data, 0o644); err != nil {
		return fmt.Errorf("write serial console artifact: %w", err)
	}
	runDir := c.serialConsoleRunDir()
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		slog.Warn("cannot create serial console run directory", "path", runDir, "error", err)
		return nil
	}
	runPath := filepath.Join(runDir, serialconsole.ArtifactFileName)
	if err := writeFileAtomic(runPath, data, 0o644); err != nil {
		slog.Warn("cannot publish serial console artifact for CAPRF", "path", runPath, "error", err)
	}
	return nil
}

// writeSerialGettyDropIn pins agetty to the resolved 8N1 framing and baud so
// the getty matches the kernel console exactly.
func (c *Configurator) writeSerialGettyDropIn(spec serialconsole.Spec) error {
	dropInDir := path.Join(systemdUnitDir, spec.GettyUnit()+".d")
	dropInPath, err := c.targetPath(path.Join(dropInDir, serialConsoleDropInName))
	if err != nil {
		return fmt.Errorf("serial getty drop-in path: %w", err)
	}
	if err := writeFileAtomic(dropInPath, []byte(serialGettyDropIn(spec)), 0o644); err != nil {
		return fmt.Errorf("write serial getty drop-in: %w", err)
	}
	slog.Info("wrote serial getty drop-in", "path", dropInPath, "device", spec.Device, "baud", spec.Baud)
	return nil
}

func serialGettyDropIn(spec serialconsole.Spec) string {
	var b strings.Builder
	b.WriteString("# Managed by BOOTy. Generated from the resolved serial console.\n")
	b.WriteString("# Resolution evidence: " + path.Join(serialConsoleArtifactDir, serialconsole.ArtifactFileName) + "\n")
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=\n")
	fmt.Fprintf(&b, "ExecStart=-/sbin/agetty -8 -o '-p -- \\\\u' --keep-baud %d %%I $TERM\n", spec.Baud)
	return b.String()
}

// enableSerialGettyUnit creates the getty.target.wants symlink systemd would
// create for `systemctl enable`, without entering the target root.
func (c *Configurator) enableSerialGettyUnit(spec serialconsole.Spec) error {
	wantsDir, err := c.targetPath(gettyWantsDir)
	if err != nil {
		return fmt.Errorf("serial getty wants dir: %w", err)
	}
	if info, err := os.Lstat(wantsDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to use symlinked serial getty wants dir: %s", wantsDir)
	}
	if err := os.MkdirAll(wantsDir, 0o755); err != nil {
		return fmt.Errorf("create serial getty wants dir: %w", err)
	}
	if err := ensureTargetDirWithinRoot(c.rootDir, wantsDir); err != nil {
		return fmt.Errorf("serial getty wants dir: %w", err)
	}
	linkPath := filepath.Join(wantsDir, spec.GettyUnit())
	if info, err := os.Lstat(linkPath); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("serial getty unit path already exists: %s", linkPath)
		}
		target, readErr := os.Readlink(linkPath)
		if readErr != nil || filepath.Clean(target) != filepath.Clean(c.serialGettyTemplatePath()) {
			return fmt.Errorf("serial getty unit path has unexpected symlink: %s", linkPath)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect serial getty unit path: %w", err)
	}
	if err := os.Symlink(c.serialGettyTemplatePath(), linkPath); err != nil {
		return fmt.Errorf("enable serial getty unit: %w", err)
	}
	slog.Info("enabled serial getty unit", "unit", spec.GettyUnit(), "path", linkPath)
	return nil
}

// serialGettyTemplatePath picks the vendor template unit present in the
// provisioned root, falling back to the most common location.
func (c *Configurator) serialGettyTemplatePath() string {
	for _, candidate := range serialGettyTemplateCandidates {
		hostPath := filepath.Join(c.rootDir, strings.TrimPrefix(candidate, "/"))
		if _, err := os.Lstat(hostPath); err == nil {
			return candidate
		}
	}
	return serialGettyTemplateCandidates[len(serialGettyTemplateCandidates)-1]
}

// pruneSerialGettyUnits removes every serial getty instance BOOTy did not
// select, so the provisioned system starts exactly one serial getty.
func (c *Configurator) pruneSerialGettyUnits(keepUnit string) error {
	if err := c.pruneSerialGettyLinks(keepUnit); err != nil {
		return err
	}
	return c.pruneSerialGettyDropIns(keepUnit)
}

func (c *Configurator) pruneSerialGettyLinks(keepUnit string) error {
	wantsDir := filepath.Join(c.rootDir, strings.TrimPrefix(gettyWantsDir, "/"))
	// Machine files may keep symlinks, so an ancestor such as etc/systemd
	// can point out of the target root on a resumed run.
	if err := ensureTargetParentWithinRoot(c.rootDir, wantsDir); err != nil {
		return fmt.Errorf("check serial getty wants dir: %w", err)
	}
	if info, err := os.Lstat(wantsDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to traverse symlinked serial getty wants dir: %s", wantsDir)
	}
	entries, err := os.ReadDir(wantsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list serial getty wants dir: %w", err)
	}
	for _, dirEntry := range entries {
		entry := dirEntry.Name()
		if !isSerialGettyInstance(entry) || entry == keepUnit {
			continue
		}
		entryPath := filepath.Join(wantsDir, entry)
		info, statErr := os.Lstat(entryPath)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("inspect stale serial getty unit %s: %w", entry, statErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to remove non-symlink serial getty unit: %s", entryPath)
		}
		if err := os.Remove(entryPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale serial getty unit %s: %w", entry, err)
		}
		slog.Info("removed conflicting serial getty unit", "unit", entry)
	}
	return nil
}

//nolint:cyclop // bounded filesystem cleanup with explicit symlink safety checks
func (c *Configurator) pruneSerialGettyDropIns(keepUnit string) error {
	unitDir := filepath.Join(c.rootDir, strings.TrimPrefix(systemdUnitDir, "/"))
	if err := ensureTargetParentWithinRoot(c.rootDir, unitDir); err != nil {
		return fmt.Errorf("check systemd unit dir: %w", err)
	}
	if info, err := os.Lstat(unitDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to traverse symlinked systemd unit dir: %s", unitDir)
	}
	entries, err := os.ReadDir(unitDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list systemd unit dir: %w", err)
	}
	for _, dirEntry := range entries {
		entry := dirEntry.Name()
		unit, ok := strings.CutSuffix(entry, ".d")
		if !ok || !isSerialGettyInstance(unit) || unit == keepUnit {
			continue
		}
		dropIn := filepath.Join(unitDir, entry, serialConsoleDropInName)
		entryPath := filepath.Join(unitDir, entry)
		if info, err := os.Lstat(entryPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to traverse symlinked serial getty drop-in dir: %s", entryPath)
		}
		info, statErr := os.Lstat(dropIn)
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect stale serial getty drop-in for %s: %w", unit, statErr)
		}
		if statErr == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to remove non-regular serial getty drop-in: %s", dropIn)
		}
		if err := os.Remove(dropIn); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale serial getty drop-in for %s: %w", unit, err)
		}
		// Only succeeds when BOOTy owned the sole drop-in.
		_ = os.Remove(filepath.Join(unitDir, entry))
	}
	return nil
}

func isSerialGettyInstance(name string) bool {
	instance, ok := strings.CutPrefix(name, "serial-getty@")
	if !ok {
		return false
	}
	device, ok := strings.CutSuffix(instance, ".service")
	return ok && serialconsole.ValidateDeviceName(device) == nil
}

// targetPath maps an absolute path inside the provisioned system onto the
// mounted target root, refusing anything that escapes it.
func (c *Configurator) targetPath(imagePath string) (string, error) {
	cleanPath := path.Clean("/" + strings.TrimSpace(imagePath))
	if cleanPath == "/" {
		return "", fmt.Errorf("target path must not be root")
	}
	hostPath := filepath.Join(c.rootDir, strings.TrimPrefix(filepath.FromSlash(cleanPath), string(filepath.Separator)))
	if err := ensureWithinRoot(c.rootDir, hostPath); err != nil {
		return "", err
	}
	if err := ensureTargetParentWithinRoot(c.rootDir, filepath.Dir(hostPath)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return "", fmt.Errorf("create target parent directory: %w", err)
	}
	if err := ensureTargetDirWithinRoot(c.rootDir, filepath.Dir(hostPath)); err != nil {
		return "", err
	}
	return hostPath, nil
}
