# Firmware Analysis and Updates

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1 for inventory, baseline and drift. P2 for CVE lookup and SBOM. P2 for
updates.

## Summary

Turn BOOTy's firmware report into a full firmware lifecycle: complete
inventory (BIOS, BMC, NIC, disk, GPU, CPLD, PSU where visible), comparison
with a versioned baseline, CVE lookup against vendor advisories, an SBOM-like
hardware and firmware bill of materials, BIOS attribute drift, and
coordinated updates through in-band tools (fwupd, vendor tools, capsules) or
out-of-band through Redfish `UpdateService`.

## Problem and motivation

- `FIRMWARE_MIN_BMC` always fails because no collector reports a BMC version
  (`docs/configuration.md`).
- The report covers BIOS, NICs and storage controllers. It misses disks
  (NVMe and SATA firmware revisions), GPUs, CPLDs, PSUs and the TPM.
- There is no baseline per hardware model, only global minimum versions.
- Operators cannot answer "which hosts run a firmware with known CVEs".
- Updates are manual, vendor-specific and outside the provisioning flow.
- BIOS settings drift after support cases or board replacements.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Collector | BIOS from DMI sysfs, NIC versions from ethtool or sysfs, storage controller versions. BMC empty. | `pkg/firmware/collector.go`, `pkg/firmware/types.go` |
| Policy | `MinBIOSVersion`, `MinBMCVersion`, `MinNICVersions` by driver. Dotted numeric compare. | `pkg/firmware/collector.go` (`Validate`, `compareVersions`) |
| NIC vendors | `FirmwareManager` interface, baseline and diff, Mellanox, Intel, Broadcom, ethtool backends. | `pkg/firmware/nic/` |
| Step | `collect-firmware`. Mellanox SR-IOV setup marks firmware changed, which forces a reboot instead of kexec. | `pkg/provision/orchestrator.go` (`collectFirmware`, `setupMellanox`, `FirmwareChanged`) |
| Keys | `FIRMWARE_REPORT`, `NIC_FIRMWARE_LOG`, `FIRMWARE_URL`, `FIRMWARE_MIN_BIOS`, `FIRMWARE_MIN_BMC`. | `docs/configuration.md` |
| Inventory | GPU, thermal, USB in extended inventory. | `pkg/inventory/` |
| CAPRF | No firmware receiver on `main`. | CAPRF `internal/server/server.go` |

## Goals

- Collect every firmware version that the host exposes in-band.
- Get the BMC version in-band through IPMI `Get Device ID` or the Redfish host
  interface, and out-of-band from CAPRF.
- Compare against a baseline keyed by system vendor, model and component.
- Produce a CycloneDX bill of materials with hardware and firmware components
  (verify the CycloneDX version that supports `device` and `firmware`
  component types).
- Look up CVEs from vendor security advisories (CSAF where available).
- Detect BIOS attribute drift against a baseline.
- Apply updates in a controlled, resumable way, with clear reboot rules.

## Non-goals

- Hosting firmware binaries. Use an internal mirror or OCI registry.
- Writing a CVE database. Consume vendor feeds.
- BIOS attribute writes. See [proposal-bios-settings.md](proposal-bios-settings.md)
  and [proposal-bios-management-vendors.md](proposal-bios-management-vendors.md).

## Inventory sources

| Component | In-band source | Out-of-band source |
|-----------|----------------|--------------------|
| BIOS / UEFI | DMI type 0, ESRT | Redfish `FirmwareInventory` |
| BMC | IPMI `Get Device ID` on `/dev/ipmi0` | Redfish `Managers/*/FirmwareVersion` |
| NIC | ethtool `drvinfo`, devlink `info` | Redfish `NetworkAdapters` |
| NVMe | `id-ctrl` `fr` field | Redfish `Drives` |
| SATA, SAS disks | ATA IDENTIFY, SCSI INQUIRY revision | Redfish `Drives` |
| RAID / HBA | sysfs, vendor CLI | Redfish `Storage` |
| GPU | sysfs, vendor tools (`nvidia-smi`, verify that it is not in the ramdisk) | Redfish `Processors` (GPU), OEM |
| CPLD, PSU | Often not in-band | Redfish `FirmwareInventory`, `PowerSupplies` |
| TPM | `/sys/class/tpm/tpm0/` (`tpm_version_major`), TPM properties | Redfish `TrustedModules` |
| Microcode | `/proc/cpuinfo` `microcode` | n/a |

Recommendation: BOOTy collects in-band data with direct syscalls and ioctls
(no new binaries) where possible: NVMe admin ioctl, ATA IDENTIFY via SG_IO,
devlink netlink. CAPRF collects out-of-band data. CAPRF merges both, because
some components are visible only out-of-band.

## Baseline and drift

- A `FirmwareBaseline` document per vendor and model: component match
  (class, vendor, model, PCI ID), allowed versions or a minimum, and the
  update package reference.
- BIOS attribute baseline: allowed values per attribute. BOOTy reads current
  attributes in-band where possible (vendor sysfs or tools), CAPRF reads
  Redfish `Bios.Attributes`. Drift is a report, not an auto-fix.
- Result per component: `compliant`, `outdated`, `unknown`, `vulnerable`.

## CVE lookup

- Options:
  - A: BOOTy queries a feed. Needs internet or a mirror from every host.
    Rejected.
  - B: CAPRF or an offline job matches stored inventories against vendor CSAF
    feeds and NVD (verify feed availability per vendor: Dell, HPE, Lenovo,
    Supermicro, NVIDIA, Intel, Broadcom).
- Recommendation: B. BOOTy only produces accurate inventory and BOM. The
  match runs centrally.

## SBOM output

- BOOTy emits a CycloneDX JSON document per run: hardware components with
  serials, firmware components with versions and vendors, and the BOOTy build
  itself (version, commit, flavor from `pkg/buildinfo`).
- Upload with the inventory event. Store it next to the host inventory.

## Updates

### Option U1: Out-of-band through Redfish `UpdateService`

CAPRF pushes packages with `SimpleUpdate` or `MultipartHttpPushUri` (verify
vendor support and package formats). It schedules the apply on the next reset.

- Pro: works for BMC, CPLD, PSU and BIOS. No host OS needed.
- Con: vendor-specific package formats. Progress tracking through task
  monitors varies.

### Option U2: In-band with fwupd and LVFS

Ship `fwupd` in a firmware flavor of BOOTy. Use a local LVFS mirror.

- Pro: one tool, signed metadata, UEFI capsule support.
- Con: server vendor coverage on LVFS is partial (verify). Large binary with
  many dependencies.

### Option U3: In-band vendor tools

`mstflint` for Mellanox, `nvmupdate64e` for Intel, `bnxtnvm` for Broadcom,
`nvme fw-download` and `fw-commit` for NVMe.

- Pro: covers NICs and disks well. Some exist in proposals already.
- Con: per-vendor code and binaries. Licences differ.

### Option U4: UEFI capsules in-band

Write a capsule to the ESP and set `OsIndications`, or use
`/dev/efi_capsule_loader` (verify kernel support), then reboot.

- Pro: standard for BIOS on many platforms.
- Con: the vendor must ship capsules. Needs a reboot.

### Recommendation

- BMC, BIOS, CPLD, PSU: U1 from CAPRF.
- NIC and NVMe: U3 in a `firmware` flavor of BOOTy, behind the existing
  `pkg/firmware/nic` interface.
- U2 only if LVFS covers the fleet.
- One update plan per host: ordered steps, each with `requiresReset`,
  `resetType`, and `verify` (version after apply). BOOTy runs in-band steps in
  standby, reports, and asks CAPRF for the reset. Kexec is never used after a
  firmware change (this rule exists today).

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `FIRMWARE_REPORT` | boolean | Existing. Extend the collectors. |
| `FIRMWARE_BASELINE` | ramdisk path to JSON | Replaces the min version keys over time. `FIRMWARE_MIN_*` stay. |
| `FIRMWARE_ENFORCE` | `report`, `warn`, `fail` | `fail` stops provisioning on non-compliance. |
| `FIRMWARE_SBOM` | boolean | Emit CycloneDX. |
| `FIRMWARE_UPDATE_PLAN` | ramdisk path to JSON | In-band steps for standby `firmware-update` commands. |

New standby command type: `firmware-update`, with the plan as payload.

### CAPRF and CAPI API impact

- `RedfishHost.status.firmware`: components, compliance, last update.
- `HostFirmwareBaseline` CRD (cluster-scoped) keyed by vendor and model.
- `HostFirmwareUpdate` CRD or a field on the host: requested plan, status,
  maintenance window. Similar to Metal3 `HostFirmwareComponents` (verify).
- MachineHealthCheck or a CAPRF condition `FirmwareCompliant` blocks
  scheduling of non-compliant hosts.

## Security considerations

- Verify every firmware package: vendor signature where the vendor provides
  one, and a SHA-256 pin in the plan.
- Firmware downgrade can reintroduce CVEs. Refuse downgrades unless the plan
  sets an explicit flag.
- A failed BMC update can brick the BMC. Never update the BMC and the BIOS in
  the same reset cycle unless the vendor requires it.
- The BOM contains serial numbers. Restrict access.

## Testing strategy

- Unit: parsers for NVMe `id-ctrl`, ATA IDENTIFY, devlink info with
  recorded fixtures. Baseline matching. CycloneDX schema validation.
- KVM matrix: `firmware-inventory` with QEMU NVMe (`-device nvme`, firmware
  revision configurable with `fr` property, verify), emulated IPMI BMC
  (`-device ipmi-bmc-sim`, verify), and a capsule ESRT in OVMF if available.
- ContainerLab: none.
- kindmetal and CAPRF e2e: host status shows firmware and compliance. Redfish
  mock or sushy-tools for `UpdateService` (verify sushy support).
- Real hardware: one host per vendor. Record inventories as fixtures. Run one
  NIC and one NVMe update in a lab.

## Phased rollout

1. PR: BMC version in-band via IPMI. Fixes `FIRMWARE_MIN_BMC`.
2. PR: NVMe and SATA firmware revisions, TPM version, microcode.
3. PR: baseline file and compliance result.
4. PR: CycloneDX emitter.
5. CAPRF MR: firmware status, baseline CRD, merge with Redfish inventory.
6. CAPRF MR or job: CVE matching against vendor feeds.
7. PR: `firmware` flavor and `firmware-update` standby command for NIC and
   NVMe.
8. CAPRF MR: Redfish `UpdateService` plans for BMC and BIOS.

## Effort estimate

| Phase | Days |
|-------|------|
| BMC, disk, TPM, microcode collectors | 5 to 7 |
| Baseline and compliance | 3 to 5 |
| CycloneDX | 3 to 4 |
| CAPRF status and merge | 5 to 8 |
| CVE matching | 6 to 10 |
| In-band updates (NIC, NVMe) | 10 to 15 |
| Redfish updates | 8 to 12 |
| **Total** | **40 to 61** |

## Open questions

- Which vendors publish machine-readable advisories (CSAF) for server
  firmware?
- Do we keep firmware binaries in the OCI registry next to OS images?
- Who approves an update plan for a production host?

## Related proposals

- [proposal-firmware-reporting.md](proposal-firmware-reporting.md)
- [proposal-nic-firmware-common.md](proposal-nic-firmware-common.md)
- [proposal-bios-settings.md](proposal-bios-settings.md)
- [proposal-bios-management-vendors.md](proposal-bios-management-vendors.md)
- [proposal-ipmi-local.md](proposal-ipmi-local.md)
- [proposal-uefi-management.md](proposal-uefi-management.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
