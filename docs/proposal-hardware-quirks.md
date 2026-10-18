# Hardware Quirks Database

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

Move per-model firmware and driver workarounds out of code branches into a
data-driven quirks database: match rules (vendor, model, BIOS or BMC version,
PCI IDs, disk models) and actions (kernel module options, delays, console
port, disabled features, known-bad firmware). BOOTy evaluates the database
at start and logs every applied quirk.

## Problem and motivation

- Workarounds live in code: Lenovo `ttyS1` in the legacy console path,
  ConnectX-specific SR-IOV steps, frozen ATA drives, disk settle waits for
  HBAs. Each new quirk needs a code change and a release.
- The vendor matrix (see [proposal-hardware-vendor-matrix.md](proposal-hardware-vendor-matrix.md))
  lists more quirks that need a home.

## Current state

| Quirk | Where | Reference |
|-------|-------|-----------|
| Lenovo console `ttyS1` | Code condition | `pkg/provision/legacy_grub.go` |
| Disk settle for async HBA scans | Config key `DISK_SETTLE_TIMEOUT_SEC` | `docs/configuration.md` |
| ConnectX functions for `NUM_VFS_MODE=raise` | Code list | `pkg/provision/legacy_mellanox.go` |
| Frozen ATA drives | Code | `pkg/disk/manager.go` |
| Kernel modules | Module loading from `/modules` | `pkg/drivers/` |

## Goals

- One YAML database, shipped in the ramdisk and overridable from `/deploy`.
- Match on DMI, PCI IDs, NVMe and ATA model and firmware, BMC version.
- A fixed set of action types, each implemented once in code.
- Every applied quirk visible in logs, metrics and inventory.

## Non-goals

- Arbitrary scripts as actions. Actions are typed.

## Design options

- A: Keep code branches. Rejected.
- B: Typed YAML database with Go action handlers. Recommendation.
- C: Rego or CEL rules. More power than needed; maybe for matching later.

Example:

```yaml
- id: lenovo-console-ttyS1
  match: {dmi: {sys_vendor: Lenovo}}
  action: {type: console, value: ttyS1}
  evidence: pkg/provision/legacy_grub.go
- id: hba-slow-scan
  match: {pci: {vendor: "1000", device: "10e2"}}
  action: {type: settleTimeout, seconds: 90}
  evidence: verify
```

Action types: `moduleOption`, `moduleBlocklist`, `kernelArg` (target),
`console`, `settleTimeout`, `disableFeature`, `firmwareDenylist`,
`eraseMethodOverride`.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `QUIRKS` | `on`, `off` | Default `on` once the database exists. |
| `QUIRKS_FILE` | ramdisk path | Extra or replacement entries. |
| `QUIRKS_DISABLE` | list of IDs | |

### CAPRF and bm4x impact

- Inventory reports applied quirk IDs; CAPRF shows them on the host.
- The firmware denylist feeds the policy gate in
  [proposal-fleet-rollout-control.md](proposal-fleet-rollout-control.md).

## Security considerations

- A `/deploy` quirks file can disable safety features. Restrict which action
  types a `/deploy` file may use (no `disableFeature` for security checks).

## Testing strategy

- Unit: matching on fixture DMI and PCI trees; each action handler.
- KVM: a quirk that matches QEMU's DMI strings and changes the console.
- Real hardware: quirks verified in the HIL lab and marked in `evidence`.

## Phased rollout

1. PR: database format, matcher, `console` and `settleTimeout` actions;
   migrate the Lenovo rule without behaviour change.
2. PR: module and kernel argument actions.
3. PR: firmware denylist and erase method override.
4. PR: inventory and metrics reporting.

## Effort estimate

| Phase | Days |
|-------|------|
| Format, matcher, two actions | 4 to 5 |
| Module and kernel actions | 2 to 3 |
| Denylist and erase override | 2 to 3 |
| Reporting | 1 to 2 |
| **Total** | **9 to 13** |

## Open questions

- Shared database for BOOTy and CAPRF (BMC quirks), or one each?

## Related proposals

- [proposal-hardware-vendor-matrix.md](proposal-hardware-vendor-matrix.md)
- [proposal-kernel-drivers.md](proposal-kernel-drivers.md)
- [proposal-fleet-rollout-control.md](proposal-fleet-rollout-control.md)
