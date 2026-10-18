# UEFI Management

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1 for boot entry management and the variable audit. P2 for key
enrollment, `dbx` and capsules.

## Summary

Give BOOTy a complete, declarative view of UEFI state: boot entries and
`BootOrder`, `BootNext`, Secure Boot keys (PK, KEK, db, dbx), `dbx` currency,
HTTP boot settings and capsule update support. BOOTy audits this state on
every run and reports it. It changes it only when the config asks for it, and
only through authenticated paths (setup mode, signed variable updates, or the
BMC).

## Problem and motivation

- `remove-efi-entries` and `create-efi-boot-entry` manage BOOTy's own
  entries, but there is no declarative target state for `BootOrder`, and no
  `BootNext` support for A/B slot switches.
- Secure Boot keys differ across vendors and firmware versions. Operators
  cannot see which `db` certificates and `dbx` entries a host has.
- `dbx` updates fix boot chain vulnerabilities (for example BlackLotus,
  CVE-2023-24932, verify). Hosts with an old `dbx` accept revoked boot
  loaders.
- Custom Secure Boot keys (site PK and KEK, own `db` for BOOTy UKIs) need a
  repeatable enrollment flow.
- The Microsoft UEFI CA 2011 certificates expire in 2026 and are replaced by
  the 2023 CAs (verify dates). Hosts need the new `db` and KEK entries before
  new shims and Windows boot managers are signed only with the new CA.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Variable access | `ReadVar`, `WriteVar`, `ListVars`, `IsSecureBootEnabled`, `IsSetupMode`. | `pkg/efi/vars.go` |
| Load options | `BuildLoadOption` for a file device path. | `pkg/efi/vars.go` |
| Steps | `mount-efivarfs`, `remove-efi-entries`, `create-efi-boot-entry`, `create-mirror-efi-boot-entry`, `install-efi-fallback`. | `pkg/provision/orchestrator.go` |
| Tools | `efibootmgr` is used for entry creation and in debug dumps. | `pkg/provision/retry.go`, `pkg/provision/orchestrator.go` |
| Secure Boot | Chain digest pins, `SECUREBOOT_REENABLE`. MOK library with `mokutil`, not wired (`MOK_CERT_PATH` parsed, not used). | `pkg/secureboot/`, `docs/configuration.md` |
| A/B | No NVRAM entry. Removable fallback loader. | `docs/ab-partitioning.md` |
| KVM | `uefi-efi-entries`, `secureboot-smoke`, `bootloader-smoke`. | `.github/workflows/kvm-matrix.yml` |

## Goals

- Audit: read and report boot entries, `BootOrder`, `BootCurrent`,
  `SecureBoot`, `SetupMode`, `AuditMode`, `DeployedMode` (verify availability),
  PK, KEK, db and dbx (certificate subjects, hashes, counts), and the ESRT.
- Boot entries: declarative desired `BootOrder` with BOOTy-owned entries
  identified by a label prefix and an optional data tag.
- `BootNext` for one-shot slot switches.
- Secure Boot keys: enroll site keys in setup mode, or append to `db` and
  `dbx` with KEK-signed authenticated variables.
- `dbx` currency check against a pinned baseline and optional update.
- Report capsule update capability (ESRT entries).

## Non-goals

- Generating or holding signing keys in BOOTy. BOOTy only applies signed
  payloads.
- BIOS attribute management. See [proposal-bios-settings.md](proposal-bios-settings.md).
- Firmware capsule content. See [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md).

## Design options

### Option A: In-band only (efivarfs)

BOOTy reads and writes all variables through efivarfs.

- Pro: vendor-neutral. Works without BMC credentials. `pkg/efi` exists.
- Con: Secure Boot key writes need setup mode or signed `.auth` payloads. Some
  firmware blocks variable writes or resets `BootOrder`. Some firmware
  refuses `dbx` appends larger than a limit (verify).

### Option B: Out-of-band only (Redfish)

CAPRF uses `Boot.BootOrder`, `BootOptions`, and `SecureBoot` /
`SecureBootDatabases` resources (verify schema and vendor support).

- Pro: works with the host powered off. Can reset keys to defaults.
- Con: vendor gaps. `BootOptions` references are not stable across vendors.

### Option C: Hybrid

BOOTy audits in-band and applies entry and `BootNext` changes in-band. CAPRF
applies Secure Boot key resets and enrollments out-of-band where supported.
BOOTy applies signed `db` and `dbx` appends in-band as a fallback.

### Recommendation

Option C. The audit is in-band, always. Key enrollment prefers Redfish when
the host reports `SecureBootDatabases` support, and falls back to signed
in-band `.auth` updates.

## Detailed design

### Audit

- New package `pkg/efi/audit` with a pure parser for `EFI_SIGNATURE_LIST`
  (x509 and SHA-256 entries), load options, and device paths.
- Output: an `UEFIState` JSON document, added to the inventory and to the
  typed events in [proposal-capi-integration.md](proposal-capi-integration.md).
- Compare against a baseline: expected PK owner, required `db` subjects,
  minimum `dbx` revision (by SHA-256 of the `dbx` content, or by presence of
  required hashes).

### Boot entries

- BOOTy-owned entries have descriptions with the prefix `BOOTy:` and an
  optional data field with the slot label.
- Desired state: `BOOTy:root-a`, `BOOTy:root-b` for A/B (instead of only the
  removable fallback), plus `BootOrder` = [active slot, other slot, network].
- `BootNext` = new slot for a trial boot. After a successful boot, the target
  OS (or a BOOTy node tool) updates `BootOrder`. See
  [proposal-rollback.md](proposal-rollback.md).
- Replace `efibootmgr` calls with `pkg/efi` writes once the load option
  encoder covers HD device paths with partition GUIDs.

### Secure Boot keys

- Setup mode: CAPRF puts the host in setup mode (Redfish `ResetKeys` with
  `DeletePK`, verify) and boots BOOTy. BOOTy writes db, KEK, then PK, each as
  a signed `.auth` payload from `/deploy/secureboot/`. Writing PK leaves
  setup mode.
- User mode: BOOTy appends to `db` or `dbx` with KEK-signed `.auth` payloads
  (`EFI_VARIABLE_APPEND_WRITE`).
- BOOTy verifies after each write that the variable contains the expected
  entries.

### dbx updates

- Ship the current UEFI `dbx` update as a signed `.auth` file in a
  versioned bundle (from the UEFI revocation list files, verify the source and
  licence). Apply it only when the audit shows an older `dbx` and the
  bootloaders on disk are not revoked by it. A `dbx` update that revokes the
  installed shim bricks the boot.
- fwupd can also apply `dbx` updates (verify). BOOTy does not need fwupd for
  this.

### HTTP boot config

- Audit the firmware HTTP boot settings exposed as variables or BIOS
  attributes (vendor-specific). Report only. Configuration goes through the
  BIOS settings proposal or CAPRF.

### Capsules

- Read `/sys/firmware/efi/esrt/entries/*` (firmware class GUID, version,
  last attempt status).
- Report capsule capability. Applying capsules is in
  [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md).

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `UEFI_AUDIT` | boolean, default `true` once stable | Read-only audit and report. |
| `UEFI_BASELINE` | ramdisk path to JSON | Expected keys, `dbx` revision, boot order. |
| `UEFI_BOOT_ORDER` | `keep`, `booty-first`, `declarative` | `declarative` uses the baseline. |
| `UEFI_BOOT_NEXT` | `none`, `target-slot` | One-shot boot to the new A/B slot. |
| `SECUREBOOT_ENROLL_DIR` | ramdisk path, default `/deploy/secureboot` | Signed `.auth` payloads: `PK.auth`, `KEK.auth`, `db.auth`, `dbx.auth`. |
| `SECUREBOOT_ENROLL_MODE` | `off`, `setup-mode`, `append` | |
| `DBX_UPDATE` | `off`, `report`, `apply` | `apply` checks the on-disk boot chain against the new `dbx` first. |

### CAPRF and CAPI API impact

- `RedfishHost.status.uefi`: Secure Boot state, key owner fingerprints, `dbx`
  revision, compliance condition.
- `RedfishHost.spec.secureBoot`: desired key set reference (Secret with
  `.auth` payloads), enrollment mode.
- CAPRF can stop calling `DisableSecureBoot` for hosts whose `db` trusts the
  BOOTy signing key.

## Security considerations

- Never hold private keys in BOOTy or `/deploy`. Only signed `.auth`
  payloads. They are safe to transport, because the firmware verifies them.
- A wrong PK or KEK locks the owner out of key management. Require a
  baseline match and a CAPRF-side approval flag for PK writes.
- A `dbx` update can revoke the installed boot chain. BOOTy must check every
  EFI binary on the target ESP against the new `dbx` before it applies it.
- Variable writes wear SPI flash. Write only on change.
- Treat variable content as untrusted input. Bound sizes and counts in the
  parsers. Fuzz them.

## Testing strategy

- Unit: `EFI_SIGNATURE_LIST` and load option parsers with fixtures from OVMF
  and real hosts. Baseline comparison. `dbx` safety check. Fuzz the parsers.
- KVM matrix: `uefi-audit` (OVMF with Microsoft keys, verify the
  `OVMF_VARS.ms.fd` file), `secureboot-enroll` (OVMF in setup mode, enroll
  test PK, KEK, db with `.auth` files made by `sbsign` tools in the test
  helper, then boot a signed UKI), `dbx-apply-safe` and
  `dbx-apply-refuse`, `boot-next-ab`.
- ContainerLab: none.
- kindmetal and CAPRF e2e: audit data on `RedfishHost`. Secure Boot stays on
  during the BOOTy boot with a site `db` key.
- Real hardware: per vendor, record variable dumps as fixtures. Test
  `BootOrder` persistence after firmware resets and BMC resets.

## Phased rollout

1. PR: `pkg/efi/audit` parsers and `UEFI_AUDIT` report.
2. PR: baseline comparison and compliance result.
3. PR: BOOTy-owned A/B boot entries and `UEFI_BOOT_NEXT`.
4. PR: `append` enrollment for `db` and `dbx` with the safety check.
5. CAPRF MR: Redfish `SecureBootDatabases` reset and status.
6. PR: `setup-mode` enrollment.
7. PR: replace `efibootmgr` calls with `pkg/efi`.

## Effort estimate

| Phase | Days |
|-------|------|
| Audit parsers and report | 4 to 6 |
| Baseline comparison | 2 to 3 |
| A/B entries and `BootNext` | 3 to 5 |
| Append enrollment and `dbx` safety | 5 to 7 |
| CAPRF Redfish key reset | 4 to 6 |
| Setup-mode enrollment | 3 to 4 |
| `efibootmgr` replacement | 3 to 4 |
| KVM scenarios | 4 to 6 |
| **Total** | **28 to 41** |

## Open questions

- Who signs the `.auth` payloads, and with which KEK?
- Do we keep the Microsoft UEFI CA in `db` on Linux-only hosts? Removing it
  hardens the host but breaks option ROMs signed by Microsoft (verify for
  the NICs and GPUs in the fleet).
- Is `BootNext` honored by every vendor after a BMC-initiated reset?

## systemd-boot

- systemd-boot adds its own variables under the vendor GUID
  `4a67b082-0a4c-41cf-b6c7-440b29bb8c4f` (`LoaderEntryDefault`,
  `LoaderEntryOneShot`, `LoaderFeatures`, verify the full list). The audit
  reports them; the writer sets the default and one-shot entries.
- The firmware entry for systemd-boot is "Linux Boot Manager" pointing to
  `\EFI\systemd\systemd-boot<arch>.efi`, or shim when chained. See
  [proposal-systemd-boot.md](proposal-systemd-boot.md).

## Related proposals

- [proposal-secureboot.md](proposal-secureboot.md)
- [proposal-secureboot-full-chain.md](proposal-secureboot-full-chain.md)
- [proposal-bootloaders.md](proposal-bootloaders.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
