> **Note:** the `pkg/bootloader` package that this proposal describes,
> including `SystemdBoot.Configure`, was removed from `main` in #463
> ("cleanup dead bootloader") as unused scaffolding. Provisioning hard-codes
> GRUB paths. No systemd-boot code exists on `main`.
> The current design for systemd-boot is
> [proposal-systemd-boot.md](proposal-systemd-boot.md).

# Proposal: Bootloader Management — GRUB Enhancement + systemd-boot

## Status: Partially implemented

## Priority: P2

## Summary

Unified bootloader management via a `Bootloader` interface. The current code
contains bootloader detection helpers and GRUB-oriented provisioning support,
but the active provisioning pipeline does not yet use systemd-boot as a source
of truth for target configuration, kexec parsing, or EFI boot-entry creation.
`SystemdBoot.Configure` is currently a no-op and systemd-boot first boot is not
CI-proven.

The design sections below describe intended coverage and note current gaps.
When a snippet reflects current code, it uses the actual exported API; planned
systemd-boot entry generation is called out separately.

## Motivation

BOOTy currently has basic GRUB config parsing in `pkg/kexec/grub.go` for
kexec kernel loading, but doesn't manage the bootloader installation or
configuration in the provisioned OS. Fedora uses BLS-style boot entries with
GRUB2 on most architectures, while several systemd-first distributions such as
Arch can use systemd-boot instead of GRUB.

| Gap | Impact |
|-----|--------|
| No GRUB installation management | Manual GRUB setup needed post-provision |
| No multi-kernel support | Can't select between kernel versions |
| No active systemd-boot provisioning support | Can't claim systemd-boot distro provisioning |
| No provisioning use of bootloader auto-detection | Provisioning still follows GRUB-oriented paths |

### Industry Context

| Tool | Bootloader Management |
|------|----------------------|
| **Ironic** | Minimal — relies on image having bootloader pre-configured |
| **MAAS** | Full GRUB management via curtin; no systemd-boot |
| **Tinkerbell** | No built-in bootloader management |
| **Flatcar** | Uses systemd-boot + GRUB fallback |
| **Talos** | Uses systemd-boot exclusively |

## Design

### Common Interface

```go
// pkg/bootloader/bootloader.go
package bootloader

import "context"

type Bootloader interface {
    // Install sets up the bootloader on the target disk.
    Install(ctx context.Context, rootPath, diskDevice string) error
    // Configure sets kernel parameters and default entry.
    Configure(ctx context.Context, rootPath string, cfg BootConfig) error
    // ListEntries returns the available boot entries.
    ListEntries(ctx context.Context, rootPath string) ([]BootEntry, error)
    // SetDefault sets the default boot entry by title.
    SetDefault(ctx context.Context, rootPath, title string) error
}

type BootConfig struct {
    KernelPath   string
    InitrdPath   string
    Cmdline      string
    DefaultEntry string
}

type BootEntry struct {
    Title  string
    Kernel string
    Initrd string
    Args   string
}
```

### GRUB Manager

```go
// pkg/bootloader/grub.go
package bootloader

import (
    "context"
    "fmt"
    "os/exec"
    "strings"
)

type GRUB struct{}

func (g *GRUB) Install(ctx context.Context, rootPath, diskDevice string) error {
    out, err := exec.CommandContext(ctx, "chroot", rootPath, "grub-install", diskDevice).CombinedOutput()
    if err != nil {
        return fmt.Errorf("grub-install: %s: %w", strings.TrimSpace(string(out)), err)
    }
    return nil
}
```

### systemd-boot Manager

Current implementation is helper-only:

- `SystemdBoot.Install` runs `bootctl install` inside the target chroot.
- `SystemdBoot.Configure` is a no-op and relies on existing Type #1 BLS
  entries in the target image.
- BOOTy does not generate `loader/entries/*.conf` or `loader.conf` during the
  active provisioning pipeline.

Direct loader-entry generation and loader configuration remain planned work.
Current unit tests cover detection, `bootctl` output parsing, and the no-op
`Configure` behavior. They do not validate loader-entry or `loader.conf`
generation because that generation is not implemented.

### Auto-Detection

```go
// pkg/bootloader/detect.go
package bootloader

import (
    "os"
    "path/filepath"
)

// DetectBootloader examines rootPath and returns the appropriate Bootloader.
func DetectBootloader(rootPath string) Bootloader {
    sdBoot := filepath.Join(rootPath, "boot", "efi", "EFI", "systemd", "systemd-bootx64.efi")
    if stat, err := os.Stat(sdBoot); err == nil && !stat.IsDir() {
        return &SystemdBoot{}
    }
    return &GRUB{}
}
```

### Required Binaries in Initramfs

| Binary | Package | Purpose | Initramfs Flavor | Already Present? |
|--------|---------|---------|-----------------|-----------------|
| `efibootmgr` | `efibootmgr` | EFI boot entry management | all | **Yes** |
| `bootctl` | target OS systemd | systemd-boot installation helper | target chroot | **Not bundled** |
| `grub-install` | — | Runs in chroot of provisioned OS | N/A (from image) | N/A |

**Note**: `grub-install`, `update-grub`, and the current `bootctl` helper run
inside the chroot of the provisioned OS image. `bootctl` is not bundled in
BOOTy's initramfs, and the active Go implementation does not generate
systemd-boot entry files directly.

## Files Changed

| File | Change |
|------|--------|
| `pkg/bootloader/bootloader.go` | Common `Bootloader` interface |
| `pkg/bootloader/detect.go` | Auto-detection logic |
| `pkg/bootloader/grub.go` | GRUB manager |
| `pkg/bootloader/systemdboot.go` | systemd-boot manager |
| `pkg/kexec/grub.go` | Enhanced GRUB config parsing |
| `pkg/provision/orchestrator.go` | Still uses GRUB-oriented provisioning paths |
| `initrd.Dockerfile` | No `bootctl` bundle in the current implementation |

## Testing

### Unit Tests

- `bootloader/detect_test.go` — Auto-detection with mock filesystem trees.
  Covered cases: systemd-boot binary, GRUB fallback, and directory-not-file
  fallback.
- `bootloader/bootloader_test.go` — GRUB entry parsing and missing-file
  behavior; systemd-boot no-op `Configure`; `bootctl` output parsing.

### E2E / KVM Tests

The current KVM matrix proves BOOTy startup through QEMU direct kernel/initrd
boot and synthetic A/B flows. It does not prove firmware handoff through the
target disk's GRUB or systemd-boot bootloader, first boot of a real
systemd-boot image, systemd-boot loader-entry generation, or kexec from
systemd-boot configuration. Add those tests before marking systemd-boot
provisioning support as implemented.

## Risks

| Risk | Mitigation |
|------|------------|
| bootctl version mismatch with target OS | Run bootctl from chroot when possible |
| GRUB path differences (grub vs grub2) | Active provisioning still uses `update-grub` and `/boot/grub/grub.cfg`; add `/boot/grub2` and `grub2-mkconfig` support before claiming RHEL/SUSE GRUB2 handoff |
| EFI partition layout varies | Configurable ESP path |

## Effort Estimate

8–12 engineering days (interface + GRUB manager + systemd-boot manager +
auto-detection + kexec integration + KVM tests).
