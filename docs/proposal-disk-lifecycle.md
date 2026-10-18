# Disk Lifecycle: Secure Erase per Media, Health Gating and Burn-In

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1 for erase correctness and evidence. P2 for SMART gating. P3 for burn-in.

## Summary

Make disk sanitization media-aware, verifiable and reportable, gate
provisioning on disk health (SMART and NVMe health logs), and add an
optional hardware burn-in mode for new or repaired hosts. The output is an
erase certificate per disk that a decommission process can keep as evidence.

## Problem and motivation

- `SECURE_ERASE` applies only to provisioning. Deprovisioning, decommission
  and `WIPE_ALL_DISKS=true` ignore it (`docs/configuration.md`). The config
  field `deprovision.secureErase` has no vars key.
- `secureEraseNVMe` runs `nvme format --ses=1`. When it fails, BOOTy falls
  back to `wipefs` and logs a warning. `secureEraseSATA` falls back to
  `wipefs` when ATA security is not supported or the drive is frozen
  (`pkg/disk/manager.go`). A requested secure erase can end as a signature
  wipe, and the run still succeeds. For decommission this is a compliance
  gap.
- Update: #704 removed the silent fallback; a requested secure erase now
  fails or reports the fallback instead of succeeding quietly.
- SAS and SCSI disks (`sd*` behind a SAS HBA) take the SATA path with
  `hdparm`. SAS drives need SCSI `SANITIZE` or `FORMAT UNIT` (`sg_sanitize`,
  `sg_format`, verify).
- NVMe `sanitize` (block erase, crypto erase, overwrite) is not used. It
  covers all namespaces and caches, which `format` may not (verify per
  controller).
- Self-encrypting drives (TCG Opal) support a crypto erase by key revert.
  BOOTy does not use it.
- Health checks look at `ioerr_cnt` and presence only (`pkg/health/disk.go`).
  They do not read SMART or the NVMe health log (critical warnings, media
  errors, percentage used, spare).
- New or repaired hosts go into production without a stress test.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Wipe | `thorough` and `quick` depths, LVM, dm-crypt, MD teardown, `wipefs`, zero edges, `sgdisk --zap-all`, `blkdiscard`. | `pkg/disk/wipe.go`, `docs/disk.md` |
| Secure erase | NVMe `format --ses=1`, ATA Security Erase with `hdparm`, `wipefs` fallbacks. | `pkg/disk/manager.go` (`SecureEraseDisk`, `secureEraseNVMe`, `secureEraseSATA`) |
| In-use guard | `CheckNotInUse` before erase. | `pkg/disk/inuse.go` |
| Health | `disk-presence`, `disk-ioerr` (warning), memory ECC, CPU, NIC link, thermal. | `pkg/health/` |
| Decommission | Wipes all disks and EFI entries; failures logged, not fatal. | `docs/caprf-integration.md` (Modes) |

## Goals

- One erase API that picks the strongest supported method per device and
  reports the method used.
- No silent downgrade. A fallback must be allowed by policy and visible in the
  result.
- Verification after erase (sample reads, or full read for the strict level).
- Erase in all modes where the operator asks for it, including decommission.
- SMART and NVMe health gating before destructive steps.
- An optional burn-in mode.

## Non-goals

- Physical destruction workflows.
- Certification against a specific standard. The design follows the NIST
  SP 800-88 method categories (Clear, Purge) as a reference (verify the
  current revision).

## Design

### Erase methods per media

| Media | Purge methods (strongest first) | Clear method | Detection |
|-------|---------------------------------|--------------|-----------|
| NVMe | `sanitize` crypto erase, `sanitize` block erase, `format --ses=2` (crypto), `format --ses=1` | overwrite or `format --ses=0` plus discard | `id-ctrl` `sanicap`, `fna` (verify fields) |
| SATA SSD | ATA `SANITIZE` crypto scramble or block erase, ATA Security Erase Enhanced | ATA Security Erase, overwrite | IDENTIFY words for sanitize and security; frozen state |
| SATA HDD | ATA `SANITIZE` overwrite, ATA Security Erase | overwrite | same |
| SAS | SCSI `SANITIZE` (crypto, block, overwrite), `FORMAT UNIT` | overwrite | `REPORT SUPPORTED OPERATION CODES` |
| TCG Opal SED | `RevertSP` or PSID revert (crypto erase) | n/a | Level 0 discovery |
| Virtual or thin LUN | n/a | `blkdiscard` plus overwrite of metadata areas | `/sys/block/*/queue/discard_*` |
| HW RAID logical disk | Erase the member disks through the controller (vendor or Redfish) | Overwrite | controller type |

Frozen ATA drives: a suspend and resume cycle unfreezes many drives (verify
that the ramdisk can do this safely), otherwise the result is "not
supported" and policy decides.

Implementation: direct ioctls (`NVME_IOCTL_ADMIN_CMD`, `SG_IO`) in Go, which
fits the "no shell-outs" rule and removes the `nvme` and `hdparm` dependencies.
Keep the CLI path as a fallback during migration.

### Policy and result

- `ERASE_LEVEL`: `none`, `clear`, `purge`.
- `ERASE_FALLBACK`: `fail` (default for `purge`), `clear`.
- Result per disk: device, model, serial, media type, method, start and end
  time, verification result, fallback used, error. Send it as an `EraseReport`
  event (see [proposal-capi-integration.md](proposal-capi-integration.md))
  and sign it with the TPM AK when attestation is available (see
  [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)).

### Verification

- `sample`: read N random blocks and check that they are zero or not equal to
  a pre-erase sample (crypto erase returns random data).
- `full`: read the whole device. Slow. For decommission only.

### Health gating

- Read the NVMe SMART / health log (critical warning, available spare,
  percentage used, media errors) and ATA SMART attributes (reallocated,
  pending, uncorrectable sectors) through ioctls.
- Thresholds in a baseline. Results go into the existing health check
  framework as new checks: `disk-smart` and `nvme-health`, critical by default
  for the target disk, warning for other disks.
- Run self-tests (`short`) optionally before provisioning.

### Burn-in

- New `MODE=burnin` with a time budget: CPU stress, memory test (a Go
  pattern test over a large allocation, or `memtester` if allowed), disk
  read and write test on wiped disks, NIC loopback or traffic to a test peer.
- Report results and EDAC counters before and after. Fail on new corrected
  errors above a threshold.
- Intended for new hosts and after hardware repair. CAPRF triggers it on a
  host label.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `ERASE_LEVEL` | `none`, `clear`, `purge` | Applies to provision, deprovision and decommission. `SECURE_ERASE=true` maps to `purge` with `ERASE_FALLBACK=clear` to keep today's behaviour. |
| `ERASE_FALLBACK` | `fail`, `clear` | |
| `ERASE_VERIFY` | `none`, `sample`, `full` | |
| `ERASE_SCOPE` | `target`, `all` | `all` for decommission. |
| `DISK_HEALTH_BASELINE` | ramdisk path | SMART thresholds. |
| `HEALTH_SKIP_CHECKS` | add `disk-smart`, `nvme-health` | Existing key. |
| `MODE` | add `burnin` | |
| `BURNIN_DURATION_MIN` | integer | |

### CAPRF and CAPI API impact

- `RedfishHost.status.lastErase`: method, level, time, report reference.
- CAPRF `force-clean` and decommission flows set `ERASE_LEVEL=purge` and
  `ERASE_SCOPE=all`.
- `RedfishHost` condition `DiskHealthy` from health events. Hosts with a
  failed disk are not scheduled.
- A `burn-in` host state before `available`, like Metal3 inspection.

## Security considerations

- Erase is destructive by design. Keep the in-use guard and the disk
  selection guards. Never erase a disk that is not in scope.
- Some drives report a successful erase without erasing (firmware bugs).
  Verification catches part of this. Track known bad models in the baseline.
- Crypto erase depends on the drive's key management. Treat it as purge only
  for drives with a known good implementation, as policy.
- Erase reports contain serial numbers. Restrict access, keep them for the
  retention period of the data protection policy.
- ATA security uses a temporary password (`temporaryErasePassword`). A power
  loss can leave the drive locked with it. Report the password through a
  secure channel or use `SANITIZE` where possible.

## Testing strategy

- Unit: method selection table per capability set. NVMe and ATA capability
  parsers with recorded `id-ctrl` and IDENTIFY fixtures. Policy outcomes,
  including no silent fallback.
- KVM matrix: QEMU NVMe supports `format` and parts of `sanitize` (verify the
  QEMU version). `erase-nvme-purge`, `erase-fallback-fail` (unsupported
  method with `ERASE_FALLBACK=fail` must fail), `smart-gate` (QEMU NVMe
  smart log with a critical warning injection, verify support).
- ContainerLab: none.
- kindmetal and CAPRF e2e: decommission with `purge` on virtual disks,
  report stored on the host.
- Real hardware: each media type in the HIL lab. Read-back verification after
  each method. Measure erase time per type.

## Phased rollout

1. PR: report the actual method in today's secure erase, and add
   `ERASE_FALLBACK=fail`. Small and urgent.
2. PR: `ERASE_LEVEL` and `ERASE_SCOPE` for deprovision and decommission.
3. PR: NVMe `sanitize` and capability detection through ioctl.
4. PR: SAS `SANITIZE` and `FORMAT UNIT` through `SG_IO`.
5. PR: verification modes.
6. PR: `disk-smart` and `nvme-health` checks.
7. PR: TCG Opal revert.
8. PR: `MODE=burnin`.
9. CAPRF MR: erase and health status, decommission defaults.

## Effort estimate

| Phase | Days |
|-------|------|
| Method reporting and no-silent-fallback | 2 to 3 |
| Erase in all modes | 2 to 3 |
| NVMe sanitize via ioctl | 4 to 6 |
| SAS sanitize | 4 to 6 |
| Verification | 2 to 3 |
| SMART and NVMe health checks | 4 to 6 |
| Opal revert | 5 to 8 |
| Burn-in mode | 6 to 9 |
| CAPRF status | 3 to 5 |
| **Total** | **32 to 49** |

## Open questions

- Which erase level does the data protection policy require for
  decommissioned disks and for reuse between tenants?
- Is a signed erase report required for audits?
- Is burn-in wanted in this tool, or in a vendor acceptance process?

## Follow-up proposals

- The burn-in mode is expanded in [proposal-burn-in.md](proposal-burn-in.md).
- The erase report becomes a signed decommission certificate in
  [proposal-certified-decommission.md](proposal-certified-decommission.md).

## Related proposals

- [disk.md](disk.md)
- [proposal-health-checks.md](proposal-health-checks.md)
- [proposal-nvme-namespaces.md](proposal-nvme-namespaces.md)
- [proposal-go-binary-replacement.md](proposal-go-binary-replacement.md)
- [proposal-burn-in.md](proposal-burn-in.md)
- [proposal-certified-decommission.md](proposal-certified-decommission.md)
