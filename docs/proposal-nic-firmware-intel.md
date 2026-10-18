# Proposal: Intel NIC Firmware Management

## Status: Partially implemented — package helpers and unit tests only

Source of truth as of 2026-06-27: the default branch contains the
`pkg/firmware/nic/intel` manager and unit tests. It does **not** wire this
manager into the provisioning state machine and does not run Intel-specific
ContainerLab, KVM, or physical-NIC E2E coverage.

## Priority: P1

## Dependencies: [NIC Firmware Common Framework](proposal-nic-firmware-common.md)

## Summary

Implement Intel E810/X710/X550/I350 NIC firmware parameter management:
capture, baseline, diff, and set/unset individual firmware flags. Preferred
path via Go `devlink` netlink API for ice/i40e drivers; fallback to Intel
`nvmupdate64e` binary for NVM-level operations.

## Motivation

Intel NICs are ubiquitous in data center servers. Key firmware parameters
that affect provisioning and workload behavior:

| Parameter | Driver | Impact |
|-----------|--------|--------|
| SR-IOV VF count | ice, i40e | Max VFs for POD networking |
| LLDP agent (FW-managed) | ice | Conflicts with OS lldpd (must disable in FW) |
| DDP profile | ice | Protocol offload support (GTP, PPPoE, etc.) |
| Link speed cap | ice, i40e | Prevents auto-negotiation to wrong speed |
| Wake-on-LAN | e1000e, igb | Power management for remote boot |
| EEP protect | i40e | NVM write protection |
| Flow Director | i40e, ice | Hardware flow steering |
| ADQ (Application Device Queues) | ice | Per-application queue assignment |

### Existing Support in BOOTy

| Binary / Module | In Initramfs? | Purpose |
|-----------------|--------------|---------|
| `ethtool` | **Yes** | Driver info, link state query |
| `devlink` | **No — add** | Devlink parameter management |
| `nvmupdate64e` | **No — add (optional)** | Intel NVM firmware update/query |
| `e1000e.ko` | **Yes** | Intel 1G driver |
| `igb.ko` | **Yes** | Intel 1G server driver |
| `igc.ko` | **Yes** | Intel I225/I226 2.5G |
| `ixgbe.ko` | **Yes** | Intel 10G driver |
| `i40e.ko` | **Yes** | Intel 10/25/40G driver |
| `ice.ko` | **Yes** | Intel 25/50/100G driver (E810) |
| `iavf.ko` | **Yes** | Intel VF driver |

## Design

### Intel-Specific Firmware Interface

```go
// pkg/firmware/nic/intel/manager.go
package intel

import (
    "context"
    "fmt"
    "log/slog"
    "os"
    "path/filepath"
    "strings"

    "github.com/telekom/BOOTy/pkg/firmware/nic"
)

// Manager implements nic.FirmwareManager for Intel NICs.
type Manager struct {
    log *slog.Logger
}

func New(log *slog.Logger) *Manager {
    return &Manager{log: log}
}

func (m *Manager) Vendor() nic.Vendor { return nic.VendorIntel }

func (m *Manager) Supported(n nic.NICIdentifier) bool {
    return n.VendorID == "8086"
}

func (m *Manager) Capture(ctx context.Context, n nic.NICIdentifier) (*nic.FirmwareState, error) {
    state := &nic.FirmwareState{
        NIC:        n,
        Vendor:     nic.VendorIntel,
        Parameters: make(map[string]nic.Parameter),
    }

    // Devlink params (ice/i40e expose many params since kernel 5.4+)
    if err := m.captureViaDevlink(ctx, n, state); err != nil {
        m.log.Info("devlink capture failed", "nic", n.Interface, "error", err)
    }

    // Sysfs-based capture for SR-IOV and link state
    m.captureViaSysfs(n, state)

    // Ethtool-based capture for FW version and features
    if err := m.captureViaEthtool(ctx, n, state); err != nil {
        m.log.Info("ethtool capture failed", "nic", n.Interface, "error", err)
    }

    return state, nil
}
```

### Intel LLDP Agent Management

The ice driver's firmware has a built-in LLDP agent that conflicts with the
OS-level LLDP agent (BOOTy's own LLDP sender and receiver; the ramdisk no
longer ships `lldpd`). Disabling it is a
critical operation:

```go
// pkg/firmware/nic/intel/lldp.go
package intel

import (
    "context"
    "fmt"
    "os/exec"
)

// DisableFWLLDP disables the firmware-managed LLDP agent on an ice NIC.
// This is required when using OS-level lldpd for LLDP neighbor discovery.
func (m *Manager) DisableFWLLDP(ctx context.Context, n nic.NICIdentifier) error {
    if n.Driver != "ice" {
        return nil // only ice has FW LLDP agent
    }
    // devlink dev param set pci/<addr> name enable_iwarp value false cmode runtime
    // For LLDP: ethtool --set-priv-flags <iface> disable-fw-lldp on
    cmd := exec.CommandContext(ctx, "ethtool", "--set-priv-flags", n.Interface, "disable-fw-lldp", "on")
    if out, err := cmd.CombinedOutput(); err != nil {
        return fmt.Errorf("disable FW LLDP on %s: %s: %w", n.Interface, string(out), err)
    }
    return nil
}
```

### DDP Profile Management

```go
// pkg/firmware/nic/intel/ddp.go
package intel

// DDPProfile represents a loaded Dynamic Device Personalization profile.
type DDPProfile struct {
    Name    string `json:"name"`
    Version string `json:"version"`
    TrackID string `json:"trackId"`
}

// GetDDPProfile reads the currently loaded DDP profile from sysfs.
func (m *Manager) GetDDPProfile(n nic.NICIdentifier) (*DDPProfile, error) {
    // /sys/class/net/<iface>/device/ddp_cfg (ice driver)
    // Format: "ICE OS Default Package version X.X.X.X"
    // ...
    return nil, nil
}
```

### Key Intel Parameters

```go
var criticalParams = []string{
    // ice driver devlink params
    "enable_iwarp",           // iWARP RDMA support
    "enable_roce",            // RoCE RDMA support
    "fw_lldp_agent",          // FW-managed LLDP (disable for OS lldpd)
    "safe_mode_support",      // Safe mode on FW error
    // i40e devlink params
    "msix_vec_per_pf_max",    // MSI-X vectors per PF
    "msix_vec_per_pf_min",    // MSI-X vectors per PF (minimum)
    // sysfs SR-IOV
    "sriov_totalvfs",         // Max VFs (read-only)
    "sriov_numvfs",           // Current VF count
    // ethtool private flags
    "disable-fw-lldp",        // Disable FW LLDP agent
    "channel-inline-flow-director", // Flow Director
    "link-down-on-close",     // Link down on interface close
}
```

### Required Binaries in Initramfs

| Binary | Package | Initramfs Flavor | Already Present? |
|--------|---------|-----------------|-----------------|
| `ethtool` | `ethtool` | all | **Yes** |
| `devlink` | `iproute2` | full, gobgp | **No — add** (shared with common framework) |
| `nvmupdate64e` | Intel download | full only | **No — optional add** |

**Note**: `nvmupdate64e` is Intel's proprietary NVM update tool. It is
**optional** — only needed for NVM-level firmware updates, not for parameter
management. The Go devlink + ethtool approach covers all parameter operations.

**Dockerfile change** for optional nvmupdate64e:

```dockerfile
# Intel NVM update tool (optional, for NVM-level firmware operations)
# Download from Intel support site and place in build context
# COPY nvmupdate64e bin/nvmupdate64e
```

## Files Changed

| File | Change |
|------|--------|
| `pkg/firmware/nic/intel/manager.go` | `Manager` implementing `FirmwareManager` |
| `pkg/firmware/nic/intel/lldp.go` | FW LLDP agent management |
| `pkg/firmware/nic/intel/ddp.go` | DDP profile detection |
| `pkg/firmware/nic/intel/params.go` | Critical parameter list |
| `pkg/firmware/nic/intel/manager_test.go` | Unit tests |
| `initrd.Dockerfile` | Add `devlink` binary |

## Testing

### Unit Tests

- `intel/manager_test.go`:
  - Table-driven `Capture()` with mock sysfs for E810/X710/I350
  - LLDP agent disable with mock `exec.CommandContext`
  - DDP profile parsing from mock sysfs
  - Ethtool private flag parsing
  - Devlink parameter set/get with mock netlink

### E2E / KVM Tests

No current E2E test exercises the Intel NIC firmware manager. QEMU/KVM and
`linux_e2e` coverage for this proposal still requires a runtime integration
that proves BOOTy invokes the manager during provisioning and handles missing
or unsupported driver parameters deterministically.

## Risks

| Risk | Mitigation |
|------|------------|
| ice/i40e devlink params vary by kernel version | Version-aware param list; skip missing params |
| nvmupdate64e is proprietary binary | Optional; Go devlink covers most parameters |
| DDP profile names not standardized | Parse known formats; raw value in `Raw` map |
| ethtool private flags are driver-specific | Per-driver flag lists |

## Effort Estimate

5–8 engineering days (devlink capture + ethtool integration + LLDP +
DDP + tests).
