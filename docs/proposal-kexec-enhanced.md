# Proposal: Enhanced Kexec — Multi-Kernel Support + Chain Loading

## Status: Phase 1 Implemented (PR #52)

## Priority: P1

> The limitations of the kexec handoff (firmware state, measured boot,
> Secure Boot, boot loader semantics, networking, CAPRF interaction) are
> documented in [kexec.md](kexec.md).

## Summary

Extend BOOTy's kexec subsystem with multi-kernel selection (version pinning,
latest detection), kernel verification (signature check), kexec chain loading
for staged boots (BOOTy → rescue → production), debug/rescue mode support,
and kernel command line management. Integrate with the bootloader proposal
for unified kernel selection.

## Motivation

The existing `pkg/kexec/` package provides basic kexec functionality:
GRUB config parsing and kernel/initrd loading. This proposal extends it
for production bare-metal deployments:

| Gap | Impact |
|-----|--------|
| No kernel version selection | Always boots first kernel found |
| No kernel signature verification | Can't verify kernel integrity before kexec |
| No chain loading | Can't stage through rescue → production |
| No rescue mode | No way to boot into recovery |
| No kernel cmdline management | Static cmdline from GRUB config |

### Industry Context

| Tool | Kexec Support |
|------|--------------|
| **Ironic** | deploy-kernel → user-kernel (one kexec) |
| **Tinkerbell** | Hook kernel → target kernel |
| **Flatcar** | kexec for updates (A/B boot) |
| **Talos** | kexec between stages |

## Design

### Kexec Architecture

```
BOOTy (PID 1 in initramfs)
  │
  ├─ Mode 1: Direct kexec
  │   └─ Provision → find kernel → kexec → target OS
  │
  ├─ Mode 2: Chain kexec
  │   ├─ Stage 1: BOOTy → rescue kernel (hardware diagnostics)
  │   └─ Stage 2: rescue → production kernel (kexec from rescue)
  │
  └─ Mode 3: Rescue kexec
      └─ On provisioning failure → kexec into rescue environment
```

### Enhanced Kexec Manager

```go
// pkg/kexec/manager.go
package kexec

import (
    "context"
    "fmt"
    "log/slog"
    "os"
    "path/filepath"
    "sort"
    "strings"
)

// Manager handles kernel selection and kexec execution.
type Manager struct {
    log     *slog.Logger
}

// KexecConfig specifies kernel selection and boot parameters.
type KexecConfig struct {
    KernelVersion   string   `json:"kernelVersion,omitempty"`   // pin to specific version
    KernelPath      string   `json:"kernelPath,omitempty"`      // explicit path override
    InitrdPath      string   `json:"initrdPath,omitempty"`      // explicit initrd override
    Cmdline         string   `json:"cmdline,omitempty"`         // kernel command line
    CmdlineAppend   string   `json:"cmdlineAppend,omitempty"`   // append to existing cmdline
    CmdlineRemove   []string `json:"cmdlineRemove,omitempty"`   // remove specific args
    VerifySignature bool     `json:"verifySignature,omitempty"` // check kernel signature
    Mode            KexecMode `json:"mode,omitempty"`           // "direct", "chain", "rescue"
}

type KexecMode string

const (
    KexecDirect  KexecMode = "direct"
    KexecChain   KexecMode = "chain"
    KexecRescue  KexecMode = "rescue"
)

// SelectKernel finds the appropriate kernel based on config.
func (m *Manager) SelectKernel(rootPath string, cfg KexecConfig) (*KernelInfo, error) {
    if cfg.KernelPath != "" {
        // Explicit path provided
        return &KernelInfo{
            KernelPath: filepath.Join(rootPath, cfg.KernelPath),
            InitrdPath: filepath.Join(rootPath, cfg.InitrdPath),
            Cmdline:    cfg.Cmdline,
        }, nil
    }

    // Scan /boot for available kernels
    kernels, err := m.scanKernels(rootPath)
    if err != nil {
        return nil, fmt.Errorf("scan kernels: %w", err)
    }

    if len(kernels) == 0 {
        return nil, fmt.Errorf("no kernels found in %s/boot", rootPath)
    }

    // Select based on version pin or latest
    var selected *KernelInfo
    if cfg.KernelVersion != "" {
        for i := range kernels {
            if kernels[i].Version == cfg.KernelVersion {
                selected = &kernels[i]
                break
            }
        }
        if selected == nil {
            return nil, fmt.Errorf("kernel version %s not found", cfg.KernelVersion)
        }
    } else {
        // Sort by version (newest first), select latest
        sort.Slice(kernels, func(i, j int) bool {
            return compareVersions(kernels[i].Version, kernels[j].Version) > 0
        })
        selected = &kernels[0]
    }

    // Apply cmdline modifications
    if cfg.CmdlineAppend != "" {
        selected.Cmdline = selected.Cmdline + " " + cfg.CmdlineAppend
    }
    for _, remove := range cfg.CmdlineRemove {
        selected.Cmdline = removeCmdlineArg(selected.Cmdline, remove)
    }

    m.log.Info("selected kernel", "version", selected.Version, "path", selected.KernelPath)
    return selected, nil
}

// KernelInfo holds kernel location and boot parameters.
type KernelInfo struct {
    KernelPath string `json:"kernelPath"`
    InitrdPath string `json:"initrdPath"`
    Cmdline    string `json:"cmdline"`
    Version    string `json:"version"`
}

// scanKernels finds all kernels in /boot.
func (m *Manager) scanKernels(rootPath string) ([]KernelInfo, error) {
    bootPath := filepath.Join(rootPath, "boot")
    entries, err := os.ReadDir(bootPath)
    if err != nil {
        return nil, fmt.Errorf("read /boot: %w", err)
    }

    var kernels []KernelInfo
    for _, entry := range entries {
        name := entry.Name()
        if strings.HasPrefix(name, "vmlinuz-") {
            version := strings.TrimPrefix(name, "vmlinuz-")
            ki := KernelInfo{
                KernelPath: filepath.Join(bootPath, name),
                Version:    version,
            }
            // Find matching initrd
            for _, initrdPrefix := range []string{"initrd.img-", "initramfs-"} {
                initrdPath := filepath.Join(bootPath, initrdPrefix+version)
                if _, err := os.Stat(initrdPath); err == nil {
                    ki.InitrdPath = initrdPath
                    break
                }
            }
            // Read cmdline from GRUB/systemd-boot config
            ki.Cmdline = m.readCmdline(rootPath, version)
            kernels = append(kernels, ki)
        }
    }
    return kernels, nil
}

// Execute performs the kexec system call.
func (m *Manager) Execute(ctx context.Context, ki *KernelInfo) error {
    m.log.Info("executing kexec",
        "kernel", ki.KernelPath,
        "initrd", ki.InitrdPath,
        "version", ki.Version)

    // Load kernel + initrd + cmdline
    if err := kexecLoad(ki.KernelPath, ki.InitrdPath, ki.Cmdline); err != nil {
        return fmt.Errorf("kexec load: %w", err)
    }

    // Execute kexec (doesn't return on success)
    return kexecExecute()
}
```

### Cmdline Management

```go
// pkg/kexec/cmdline.go
package kexec

import "strings"

// MergeCmdline combines base cmdline with additions and removals.
func MergeCmdline(base, append string, remove []string) string {
    result := base
    if append != "" {
        result += " " + append
    }
    for _, r := range remove {
        result = removeCmdlineArg(result, r)
    }
    return strings.TrimSpace(result)
}

func removeCmdlineArg(cmdline, arg string) string {
    parts := strings.Fields(cmdline)
    var filtered []string
    for _, p := range parts {
        // Remove exact match or key= match
        key := strings.SplitN(p, "=", 2)[0]
        if p != arg && key != arg {
            filtered = append(filtered, p)
        }
    }
    return strings.Join(filtered, " ")
}
```

### Required Binaries in Initramfs

| Binary | Package | Purpose | Initramfs Flavor | Already Present? |
|--------|---------|---------|-----------------|-----------------|
| `kexec` | `kexec-tools` | kexec system call wrapper | full, gobgp | **Yes** (via busybox or standalone) |

**Note**: BOOTy uses Go `syscall.Kexec` for the kexec system call directly.
The `kexec` binary is a backup. No new binaries needed.

### Configuration

```bash
# /deploy/vars
export KEXEC_ENABLED="true"
export KEXEC_KERNEL_VERSION=""           # pin; empty = latest
export KEXEC_CMDLINE_APPEND="console=ttyS0,115200"
export KEXEC_CMDLINE_REMOVE="quiet splash"
export KEXEC_VERIFY="true"               # verify kernel signature before kexec
export KEXEC_MODE="direct"               # "direct", "chain", "rescue"
```

## Files Changed

| File | Change |
|------|--------|
| `pkg/kexec/manager.go` | Enhanced kexec manager |
| `pkg/kexec/cmdline.go` | Cmdline management |
| `pkg/kexec/scan.go` | Kernel scanning |
| `pkg/kexec/grub.go` | Enhanced GRUB config parsing (existing) |
| `pkg/kexec/verify.go` | Kernel signature verification |
| `pkg/provision/orchestrator.go` | Enhanced kexec step |
| `pkg/config/provider.go` | Kexec config fields |

## Testing

### Unit Tests

- `kexec/cmdline_test.go` — Table-driven cmdline manipulation. Cases:
  append, remove key=value, remove bare key, empty base, duplicate removal.
- `kexec/manager_test.go` — Kernel scanning with mock /boot directory.
  Cases: single kernel, multiple versions, version pinning, no kernel found.
- `kexec/scan_test.go` — Version comparison and sorting.

### E2E / KVM Tests

- **KVM matrix** (`kvm-matrix.yml`, tag `e2e_kvm`):
  - QEMU → BOOTy provisions → kexec direct into provisioned kernel
  - Verify: correct kernel version selected when multiple installed
  - Verify: cmdline modifications applied correctly
  - Verify: kexec to rescue kernel when provisioning fails

## Risks

| Risk | Mitigation |
|------|------------|
| kexec fails with certain kernels | Compatibility list; fall back to reboot |
| initrd too large for kexec memory | Check available memory before load |
| Cmdline modification breaks boot | Validate against known-good patterns |

## Effort Estimate

5–8 engineering days (multi-kernel + cmdline + chain loading + verification
+ KVM tests).
