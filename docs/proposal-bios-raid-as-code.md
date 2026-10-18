# BIOS Attributes and Hardware RAID as Code

## Status

Proposal. Design only. Nothing in this document is implemented. It builds on
[proposal-bios-settings.md](proposal-bios-settings.md) and
[proposal-bios-management-vendors.md](proposal-bios-management-vendors.md)
and adds hardware RAID controllers, per-model profiles and drift reporting.

## Priority

P2

## Summary

Declare BIOS attributes and hardware RAID controller layouts as versioned
per-model profiles. CAPRF applies them out of band through Redfish before
the BOOTy boot where the BMC supports it; BOOTy applies the rest in-band with
vendor tools in an opt-in flavor, verifies the result, and reports drift on
every run.

## Problem and motivation

- BIOS settings (virtualization, SR-IOV global enable, C-states, boot mode,
  TPM, Secure Boot, power profile) decide performance and whether
  provisioning works at all. They drift after repairs and board swaps.
- Hosts with a hardware RAID controller need a logical disk before BOOTy can
  select a target. BOOTy supports only software RAID (`DISK_MIRRORING_MODE`,
  `provision.disk.raid`) and NVMe namespaces.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| BIOS | Proposals only. No code on `main`. | `docs/proposal-bios-settings.md`, `docs/proposal-bios-management-vendors.md` |
| Software RAID | mdadm RAID1 mirror, RAID config in YAML only. | `pkg/disk/mirror.go`, `docs/configuration.md` |
| NVMe namespaces | `NVME_NAMESPACES`. | `pkg/disk/nvme.go` |
| HW RAID | None. Storage controller firmware is reported. | `pkg/firmware/collector.go` |
| Vendor | DMI vendor detection. | `pkg/system/vendor.go` |

## Goals

- One profile format for BIOS attributes and RAID layouts, keyed by vendor,
  model and controller.
- Out-of-band apply first, in-band fallback, always in-band verify.
- Drift report on every run, enforce on demand.

## Non-goals

- A GUI or a vendor-neutral attribute naming layer for every attribute. Use
  vendor attribute names with an optional alias table for the common ones.

## Design options

- A: Redfish only (CAPRF). Pro: no tools in the ramdisk. Con: gaps on some
  vendors and controllers.
- B: In-band only (BOOTy with vendor tools). Pro: works without BMC features.
  Con: licences, tool size, per-vendor code.
- C: Hybrid. Recommendation: C. Redfish `Bios` with `@Redfish.Settings` and
  `Storage`/`Volumes` (`Volume` create, verify vendor support) from CAPRF;
  BOOTy in-band with `storcli`, `ssacli`, `perccli`, `ilorest` only in an
  opt-in `vendor` flavor; BOOTy reads current values in-band where possible
  and reports drift.

Profile example:

```yaml
name: hpe-dl380-gen11-worker
match: {vendor: HPE, model: "ProLiant DL380 Gen11"}
bios:
  attributes: {WorkloadProfile: Virtualization-MaxPerformance, Sriov: Enabled}
  onDrift: warn
raid:
  controller: {match: "MR416i"}
  volumes: [{name: os, level: RAID1, disks: 2, sizeGB: 480}]
  onDrift: fail
```

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `HW_PROFILE` | ramdisk path or JSON | Profile for this host, rendered by CAPRF. |
| `BIOS_ON_DRIFT` | `ignore`, `warn`, `fail`, `apply` | `apply` needs the vendor flavor and a reboot. |
| `RAID_ON_DRIFT` | same | |

### CAPRF and bm4x impact

- Profiles as a CRD or ConfigMaps; host class mapping from bm4x.
- CAPRF applies Redfish settings and waits for the settings job before the
  BOOTy boot.
- `RedfishHost.status.biosCompliance`, `raidCompliance`.

## Security considerations

- BIOS attributes include Secure Boot and TPM switches. Changing them changes
  the trust chain and PCR 1 and 7. Coordinate with the measured boot policy.
- Vendor tools run as root; opt-in flavor only.
- RAID apply destroys data. Same guards as wipes.

## Testing strategy

- Unit: profile matching and diff with recorded Redfish `Bios` and `Storage`
  payloads per vendor.
- KVM: none for HW RAID; OVMF variables for a few BIOS-like settings only.
- kindmetal and CAPRF e2e: Redfish mock with `Bios` settings and `Volumes`.
- Real hardware: one host per vendor and controller in the HIL lab.

## Phased rollout

1. PR: profile schema and in-band read-only drift report (DMI, efivars,
   `storcli` JSON when present).
2. CAPRF MR: Redfish BIOS apply with settings jobs.
3. CAPRF MR: Redfish volume create.
4. PR: vendor flavor with in-band apply.

## Effort estimate

| Phase | Days |
|-------|------|
| Schema and drift report | 4 to 6 |
| CAPRF BIOS apply | 6 to 9 |
| CAPRF RAID apply | 6 to 9 |
| Vendor flavor apply | 8 to 12 |
| **Total** | **24 to 36** |

## Open questions

- Which hosts in the fleet have hardware RAID controllers?
- Who owns profiles: bm4x host classes or CAPRF templates?

## Related proposals

- [proposal-bios-settings.md](proposal-bios-settings.md)
- [proposal-bios-management-vendors.md](proposal-bios-management-vendors.md)
- [proposal-hardware-vendor-matrix.md](proposal-hardware-vendor-matrix.md)
- [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md)
