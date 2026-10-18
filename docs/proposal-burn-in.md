# Hardware Burn-In Suite

## Status

Proposal. Design only. Nothing in this document is implemented. It expands
the burn-in section of [proposal-disk-lifecycle.md](proposal-disk-lifecycle.md)
into a full suite.

## Priority

P2

## Summary

A `MODE=burnin` that stresses and observes a host for a time budget: memory
test with EDAC and ECC counters, CPU stress with thermal and fan
observation, PCIe AER counters, disk read and write, NIC traffic, GPU
diagnostics (DCGM), and power draw read from the BMC. Results use the same
threshold and report model as the health checks and the cable check.

## Problem and motivation

- New and repaired hosts enter production without stress. Early-life
  failures (DIMMs, GPUs, optics) then hit workloads.
- BOOTy health checks are quick snapshots (`memory-ecc`, `thermal-state`,
  `disk-ioerr`), not stress tests (`pkg/health/`).

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Health checks | Disk presence and I/O errors, memory ECC, min memory and CPU, NIC link, thermal. | `pkg/health/` |
| Inventory | GPU and thermal data. | `pkg/inventory/gpu.go`, `pkg/inventory/thermal.go` |
| Modes | `check` runs health checks. | `docs/caprf-integration.md` |

## Goals

- Repeatable burn-in with a time budget and a pass or fail result.
- Before and after counters: EDAC corrected and uncorrected, PCIe AER,
  NVMe media errors, NIC errors, thermal throttling events.
- Shared thresholds with health checks and the cable check.

## Non-goals

- Vendor acceptance testing replacement; this is an operator-side check.

## Design options

- Memory: A) Go pattern test over most free memory (pure Go, no binary);
  B) `memtester` (GPL, verify); C) firmware memory test through BIOS
  settings (vendor). Recommendation: A plus EDAC counters; C optional.
- CPU: Go workers with mixed integer, floating point and memory bandwidth
  loops; or `stress-ng` (GPL, verify) in a burn-in flavor. Recommendation:
  `stress-ng` in a flavor for coverage, Go fallback.
- GPU: NVIDIA DCGM diagnostics (`dcgmi diag`, proprietary, needs the driver;
  verify licence and size). Only in a GPU flavor.
- Power: Redfish `Power`/`EnvironmentMetrics` from CAPRF during the run, or
  IPMI DCMI power reading in-band (verify).
- Thermals and fans: hwmon sysfs, IPMI SDR.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `MODE` | add `burnin` | |
| `BURNIN_DURATION_MIN` | integer | |
| `BURNIN_TESTS` | list: `memory`, `cpu`, `disk`, `nic`, `gpu`, `pcie` | |
| `BURNIN_THRESHOLDS` | path or JSON | Shared format with health and cable check. |
| `BURNIN_SUCCESS_ACTION` | `poweroff`, `standby` | |

### CAPRF and bm4x impact

- A burn-in step in onboarding (see [proposal-zero-touch-onboarding.md](proposal-zero-touch-onboarding.md))
  and after repair.
- CAPRF samples BMC power and thermal data during the run and adds it to
  the report.
- Host condition `BurnInPassed`.

## Security considerations

- Burn-in destroys data on disks it writes. Same guards as wipes; only on
  hosts that are not in use.
- Vendor binaries only in opt-in flavors.

## Testing strategy

- Unit: threshold evaluation, counter deltas from EDAC, AER and NVMe
  fixtures.
- KVM: short burn-in (1 minute) on QEMU; EDAC injection is not available in
  QEMU (verify), so use fixtures.
- Real hardware: full runs in the HIL lab, including a known-bad DIMM if one
  is available.

## Phased rollout

1. PR: `MODE=burnin` skeleton with counters before and after.
2. PR: Go memory and CPU tests.
3. PR: disk and NIC tests.
4. PR: burn-in flavor with `stress-ng`.
5. PR: GPU flavor with DCGM.
6. CAPRF MR: BMC power sampling and condition.

## Effort estimate

| Phase | Days |
|-------|------|
| Skeleton and counters | 3 to 4 |
| Memory and CPU | 3 to 5 |
| Disk and NIC | 3 to 4 |
| Flavor | 2 |
| GPU | 4 to 6 |
| CAPRF | 3 to 5 |
| **Total** | **18 to 26** |

## Open questions

- Required burn-in duration per hardware class?
- Is DCGM redistribution allowed in our flavor?

## Related proposals

- [proposal-disk-lifecycle.md](proposal-disk-lifecycle.md)
- [proposal-health-checks.md](proposal-health-checks.md)
- [proposal-zero-touch-onboarding.md](proposal-zero-touch-onboarding.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
