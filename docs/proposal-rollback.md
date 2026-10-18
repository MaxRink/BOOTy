# Rollback Story

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1

## Summary

Define how a host returns to a known-good state after a failed write, a
failed first boot, or a bad OS release. For A/B hosts, add a trial boot,
health confirmation, commit, and automatic fallback with boot counting. For
whole-disk hosts, define a reprovision-to-previous flow. Expose rollback as a
CAPRF operation with clear status.

## Problem and motivation

- A/B preserve-existing upgrades kexec into the new slot. BOOTy does not
  change the shared ESP in this mode (`docs/ab-partitioning.md`). A later
  firmware reboot therefore boots the old slot again, unless the OS or a
  higher-level updater switches the boot target. This is an implicit trial
  boot, but nobody documents or owns the commit step.
- There is no boot counting. A new slot that panics in a loop stays broken
  until an operator acts, or until a reboot happens to pick the old slot.
- CAPRF cannot ask "roll back to the previous slot" as an explicit operation.
  The `feat/booty-consolidated` branch tracks `activeSlot`, `bootedSlot` and
  `upgradeID`, but no rollback state.
- Whole-disk installs have no rollback except a new install. The previous
  image reference is not stored.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Failed write | Wipes the start of the target (whole-disk) or invalidates the written partitions (A/B, partition layout). | `pkg/image/stream.go` (`wipeLeadingSectors`), `docs/disk.md` (Failed image writes) |
| A/B guards | Active slot checks, read-only mount of the active slot, refusal to reboot without kexec. | `pkg/provision/orchestrator.go` (`validateABActiveSlotState`), `main.go` (`requiresABKexec`) |
| Slot state | `/etc/booty/ab-slot.env` in each slot. | `pkg/provision/orchestrator.go` (`writeABSlotState`) |
| Boot entries | A/B: removable fallback loader, no NVRAM entry. | `docs/ab-partitioning.md` |
| Rescue | `RESCUE_MODE` for failed provisioning runs. | `docs/caprf-integration.md` (Rescue modes) |
| CAPRF | Branch A/B spec and status fields. | CAPRF branch `api/v1alpha1/redfishmachine_types.go` |

## Goals

- A documented A/B lifecycle: write, trial boot, confirm, commit, or fall
  back.
- Automatic fallback after N failed boots of the new slot.
- An explicit rollback operation in CAPRF.
- Whole-disk rollback by reprovisioning the last good image.

## Non-goals

- Filesystem snapshots (btrfs, ZFS). The OS owns them.
- Data rollback on shared partitions (`/var`). Data migrations are the
  application's job.

## Design options

### Option A: GRUB environment counters

Use `grubenv` variables (`next_entry`, a try counter) on the shared ESP.
GRUB decrements and falls back.

- Pro: works with GRUB targets, which are most targets today.
- Con: GRUB scripting in a shared `grub.cfg` on the ESP. The images bring
  their own GRUB configs. Needs a BOOTy-owned ESP `grub.cfg`.

### Option B: systemd-boot boot counting

Name entries `<entry>+3-0.conf` (or UKIs) and let `systemd-bless-boot`
mark them good after `boot-complete.target` (verify the UAPI boot counting
spec).

- Pro: standard, well specified. Works with UKIs and the measured boot plan.
- Con: needs systemd-boot targets.

### Option C: UEFI `BootNext` plus confirmation

BOOTy creates NVRAM entries for both slots. It sets `BootNext` to the new
slot and keeps `BootOrder` on the old slot. After a healthy boot, the OS (or a
CAPRF command through a node agent) moves the new slot to the front of
`BootOrder`.

- Pro: firmware-level, OS-agnostic. A failed boot falls back on the next
  reset automatically, because `BootNext` is one-shot.
- Con: a hang without reset needs a watchdog (BMC watchdog or the kernel
  watchdog) to trigger the reset. Some firmware resets `BootOrder`.

### Recommendation

Option C as the generic mechanism, with Option B for systemd-boot targets.
Keep kexec as an optional fast path for the trial boot only. Commit is always
a firmware-visible change. Arm the BMC watchdog (IPMI `Set Watchdog Timer`,
see [proposal-ipmi-local.md](proposal-ipmi-local.md)) before the trial boot,
and let the OS disarm it after health confirmation.

Health confirmation:

1. The OS reaches `boot-complete.target` (or a custom unit).
2. Optional: the kubelet registers and the Node becomes Ready. CAPRF observes
   this.
3. Optional: attestation of the new slot passes (see
   [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)).

Commit writes `BootOrder` and updates `ab-slot.env` in both slots.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `AB_COMMIT_MODE` | `os` (default), `caprf`, `immediate` | Who commits. `immediate` keeps today's behaviour for tests. |
| `AB_TRIAL_BOOT` | `kexec`, `bootnext` | `bootnext` reboots through firmware. |
| `AB_MAX_TRIES` | integer, default `3` | For systemd-boot counting. |
| `BMC_WATCHDOG_SEC` | integer, `0` disables | Armed before the trial boot. |

A small node-side tool `booty-ab` (same repo, separate binary like
`booty-esp-sync`) reads `ab-slot.env`, confirms, commits and reports.

### CAPRF and CAPI API impact

- `RedfishMachine.spec.ab.rollbackTo`: `previous`. CAPRF triggers a BOOTy
  standby command or a node-agent command that sets `BootNext` to the other
  slot and reboots.
- `RedfishMachine.status.ab`: add `committedSlot`, `trialSlot`,
  `trialAttempts`, `lastRollbackReason`.
- `RedfishMachine.status.lastGoodImage` for whole-disk hosts. CAPRF uses it
  for a "reprovision to previous" operation.
- MachineHealthCheck remediation can choose rollback before reprovision for
  A/B hosts.

## Security considerations

- Rollback can reinstall a vulnerable release. Allow rollback only to slots
  whose image is not revoked (check against a minimum version policy or a
  `dbx`-like denylist for UKIs).
- The commit tool must run as root and must verify that the slot it commits
  is the booted slot (`root=` and `ab-slot.env`).
- A watchdog that resets the host is an availability risk if misconfigured.
  Default off.

## Testing strategy

- Unit: state machine for trial, confirm, commit and fallback. `BootNext` and
  `BootOrder` writes on fake efivars.
- KVM matrix: `ab-trial-fallback` (new slot kernel panics, reset, old slot
  boots, status reports the fallback), `ab-trial-commit` (new slot confirms,
  `BootOrder` updated, reboot stays on the new slot), `ab-watchdog` (hang,
  QEMU `i6300esb` watchdog resets, verify device).
- ContainerLab: none.
- kindmetal and CAPRF e2e: rollback operation on a provisioned A/B machine.
- Real hardware: `BootNext` and `BootOrder` persistence per vendor, BMC
  watchdog per vendor.

## Phased rollout

1. Docs PR: document today's implicit trial boot behaviour.
2. PR: NVRAM entries for both A/B slots (shared with the UEFI proposal).
3. PR: `AB_TRIAL_BOOT=bootnext`.
4. PR: `booty-ab` confirm and commit tool.
5. PR: BMC watchdog arm and disarm.
6. CAPRF MR: rollback operation and status.
7. PR: systemd-boot boot counting for systemd-boot targets.

## Effort estimate

| Phase | Days |
|-------|------|
| Docs | 1 |
| Slot entries and `BootNext` | 4 to 6 |
| `booty-ab` tool | 4 to 6 |
| Watchdog | 3 to 4 |
| CAPRF rollback | 4 to 6 |
| Boot counting | 3 to 4 |
| KVM scenarios | 4 to 6 |
| **Total** | **23 to 33** |

## Open questions

- Should the commit depend on Kubernetes Node readiness, or on OS health only?
- Is the BMC watchdog reliable across the fleet?
- Do we keep kexec for trial boots at all once `BootNext` works?

## systemd-boot boot counting and Flatcar gptprio

- On systemd-boot targets, boot counting (`+tries` in the entry name and
  `systemd-bless-boot`) replaces `BootNext` for the trial boot; `BootNext`
  stays the generic path for GRUB targets. [proposal-systemd-boot.md](proposal-systemd-boot.md) has the entry naming and the
  `LoaderEntryOneShot` flow. Both can complement each other: `BootNext` for
  the firmware entry, counters for the entry inside systemd-boot.
- Flatcar has its own rollback: GPT priority, tries and successful bits on
  `USR-A` and `USR-B`, handled by GRUB `gptprio` and `update_engine`. BOOTy
  must not interfere (see [proposal-flatcar.md](proposal-flatcar.md)).

## kexec limitations

The kexec handoff skips the firmware, so boot counting, `BootNext`, measured
boot, Secure Boot verification by shim and pending firmware changes do not
apply to the kexec'd kernel. The full list, with a kexec or reboot decision
table, is in `docs/kexec.md` (PR #705).

## Related proposals

- [ab-partitioning.md](ab-partitioning.md)
- [proposal-uefi-management.md](proposal-uefi-management.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
- [proposal-ipmi-local.md](proposal-ipmi-local.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-flatcar.md](proposal-flatcar.md)
