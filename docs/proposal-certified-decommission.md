# Certified Decommissioning

## Status

Proposal. Design only. Nothing in this document is implemented. It builds on
[proposal-disk-lifecycle.md](proposal-disk-lifecycle.md) and on the secure
erase fix that is in progress in a separate PR.

## Priority

P1

## Summary

When a host retires or changes tenant, produce a data destruction report per
disk that follows the NIST SP 800-88 structure (verify the current
revision): media identification, sanitization method and category (clear,
purge), verification method and result, tool and firmware versions, time,
and operator or automation identity. Sign the report and store it with the
host record.

## Problem and motivation

- `MODE=decommission` wipes every disk and logs failures without stopping
  (`docs/caprf-integration.md`). It produces no evidence.
- Audits and customer contracts can require proof of sanitization per
  drive.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Decommission | Wipe all disks and EFI entries; failures logged. CAPRF `force-clean` uses it. | `docs/caprf-integration.md` |
| Secure erase | Provisioning only; silent `wipefs` fallbacks (fix in progress separately). | `pkg/disk/manager.go` |
| Report | None. | |

## Goals

- One report per disk and one summary per host, in JSON and a printable
  form.
- Signed by BOOTy with its machine identity, countersigned by CAPRF.
- Non-compliant results (fallback used, verification failed) are visible and
  block the "decommissioned" state.

## Non-goals

- Physical destruction certificates (third-party process).

## Design

Report fields per disk (from the NIST SP 800-88 sample certificate
structure, verify): manufacturer, model, serial, media type, capacity,
sanitization category (clear or purge), method (for example NVMe sanitize
crypto erase), tool and version, drive firmware, verification method and
result, start and end time, result, and notes. Host summary: host serial,
asset tag, run ID, BOOTy version and digest, CAPRF identity, approver.

Signing: JWS with the machine key (TPM-resident when available, see
[proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)); CAPRF adds a
second signature and stores the report in an object or an archive bucket.

Flow: CAPRF sets `MODE=decommission`, `ERASE_LEVEL=purge`,
`ERASE_SCOPE=all`, `ERASE_VERIFY=sample` (or `full`), `ERASE_FALLBACK=fail`
(keys from the disk lifecycle proposal), and `DECOMMISSION_REPORT=true`.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `DECOMMISSION_REPORT` | boolean | Build and send the report. |
| `DECOMMISSION_REPORT_URL` | URL | Fixed for standby payloads. |
| `DECOMMISSION_REQUIRE_PURGE` | boolean | Fail when any disk got only `clear`. |

### CAPRF and bm4x impact

- `RedfishHost.status.decommission`: result, report reference, time.
- bm4x marks the host retired only with a compliant report.

## Security considerations

- Reports contain serials. Retain per the data protection policy.
- A forged report hides a failed erase. Signatures and verification from
  BOOTy's measured identity prevent that.

## Testing strategy

- Unit: report schema, JWS signing and verification.
- KVM: decommission with QEMU NVMe; report complete and signed; an
  unsupported method with `ERASE_FALLBACK=fail` yields a failed report.
- kindmetal and CAPRF e2e: report stored on the host.
- Real hardware: one report per media type in the HIL lab.

## Phased rollout

1. PR: report schema and JSON emission (after the erase method reporting
   lands).
2. PR: signing.
3. CAPRF MR: storage and countersignature.
4. PR: printable rendering.

## Effort estimate

| Phase | Days |
|-------|------|
| Schema and emission | 2 to 3 |
| Signing | 2 to 3 |
| CAPRF | 3 to 5 |
| Rendering | 1 to 2 |
| **Total** | **8 to 13** |

## Open questions

- Which retention period and format does the audit function require?

## Related proposals

- [proposal-disk-lifecycle.md](proposal-disk-lifecycle.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
