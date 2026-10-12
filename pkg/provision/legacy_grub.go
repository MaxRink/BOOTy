//go:build linux

package provision

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
)

// legacyGRUBUnsafe holds characters that would break the double-quoted
// GRUB_CMDLINE_LINUX value of the legacy drop-in.
const legacyGRUBUnsafe = "\"\\$`\n\r"

// findUpdateGRUB returns /usr/sbin/update-grub or /sbin/update-grub when
// it is an executable file in the target, like the `-x` checks of the legacy
// provision.sh. os.Root resolves image symlinks inside the target root and
// never against the ramdisk, and no target shell is needed for the check.
func (c *Configurator) findUpdateGRUB() string {
	root, err := os.OpenRoot(c.rootDir)
	if err != nil {
		return ""
	}
	defer root.Close() //nolint:errcheck // read-only handle
	for _, candidate := range []string{"/usr/sbin/update-grub", "/sbin/update-grub"} {
		info, err := root.Stat(strings.TrimPrefix(candidate, "/"))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// configureGRUBLegacy writes the kernel parameters like provision.sh of the
// CAPRF legacy provisioner: it appends "ds=nocloud <console> <extra>" to the
// image GRUB_CMDLINE_LINUX and runs update-grub when the image has it.
func (c *Configurator) configureGRUBLegacy(ctx context.Context, cfg *config.MachineConfig) error {
	extra := strings.TrimSpace(cfg.Provision.ExtraKernelParams)
	if strings.ContainsAny(extra, legacyGRUBUnsafe) {
		return fmt.Errorf("unsafe characters in ExtraKernelParams: %q", extra)
	}
	abRoot, err := abRootKernelParam(cfg)
	if err != nil {
		return err
	}
	params := strings.Join(strings.Fields("ds=nocloud "+c.legacyConsoleParam()+" "+extra+" "+abRoot), " ")
	line := fmt.Sprintf("GRUB_CMDLINE_LINUX=\"$GRUB_CMDLINE_LINUX %s\"\n", params)

	// targetPath refuses paths that leave the target root through symlinks.
	if _, err := c.targetPath(path.Join("/boot/grub", "grub.cfg")); err != nil {
		return fmt.Errorf("creating boot grub dir: %w", err)
	}
	grubPath, err := c.targetPath("/etc/default/grub.d/10-caprf-kernel-params.cfg")
	if err != nil {
		return fmt.Errorf("grub drop-in path: %w", err)
	}
	slog.Info("writing GRUB config like the legacy CAPRF provisioner", "path", grubPath)
	if err := writeFileAtomic(grubPath, []byte(line), 0o644); err != nil {
		return fmt.Errorf("writing grub config: %w", err)
	}

	updateGRUB := c.findUpdateGRUB()
	if updateGRUB == "" {
		slog.Info("update-grub not found in the target, skipping like the legacy CAPRF provisioner")
		return nil
	}
	out, err := c.disk.ChrootRun(ctx, c.rootDir, updateGRUB)
	if err != nil {
		return fmt.Errorf("update-grub: %s: %w", string(out), err)
	}
	return nil
}

// legacyConsoleParam returns console=ttyS0,115200n8, or ttyS1 on Lenovo
// hosts, like provision.sh of the CAPRF legacy provisioner.
func (c *Configurator) legacyConsoleParam() string {
	sysRoot := c.hostSysRoot
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	vendor, err := os.ReadFile(filepath.Join(sysRoot, "class", "dmi", "id", "sys_vendor"))
	if err == nil && strings.EqualFold(strings.TrimSpace(string(vendor)), "lenovo") {
		return "console=ttyS1,115200n8"
	}
	return "console=ttyS0,115200n8"
}
