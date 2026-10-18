# Hardware Watchdog

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented.

## Priority

P1

## Summary

BOOTy arms a hardware watchdog at start and pets it from a supervisor
goroutine that checks real progress. A hung run (kernel deadlock, stuck
I/O, stuck step) resets the host by itself. BOOTy prefers the kernel
watchdog device (`/dev/watchdog0`, which on most servers is the IPMI BMC
watchdog or a chipset timer) and falls back to arming the BMC watchdog
directly through IPMI.

## Problem and motivation

- A hung BOOTy run stays hung until CAPRF's provision timeout fires and
  power-cycles the host through the BMC. With no callback, CAPRF does not
  know why.
- Some hangs stop the whole kernel (NVMe or HBA firmware bugs, a stuck
  `D`-state process). No user-space timer can recover from that.

## Current state

> **Update:** #721 merged: `WATCHDOG_ENABLE` arms the hardware watchdog,
> pets it only on step progress with per-step budgets, falls back to the IPMI
> watchdog, and hands it over at kexec and reboot (`WATCHDOG_HANDOFF`). The
> table below is the state before that work.

| Area | Today | Reference |
|------|-------|-----------|
| Watchdog | Not used. | `grep watchdog` finds only the crash collector |
| Step retries | Per-step retry policy and `RESCUE_MODE=retry`. | `pkg/provision/retry.go`, `pkg/rescue/rescue.go` |
| IPMI | `ipmitool` wrapper; no watchdog command. | `pkg/ipmi/ipmi.go` |
| Proposals | Watchdog self-reboot is a later phase of [proposal-resilient-provisioning.md](proposal-resilient-provisioning.md). | |

## Goals

- Reset a host that makes no progress for a bounded time.
- Record the reason for the next run (see
  [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)).
- Never reset during an operation that must not be interrupted without a
  longer, explicit timeout (firmware flashing, secure erase).

## Non-goals

- Watchdog management in the installed OS. BOOTy hands off with the
  watchdog stopped, or armed with a timeout the OS takes over (option).

## Design options

- A: `/dev/watchdog` via the kernel driver (`ipmi_watchdog`, `iTCO_wdt`,
  `sp5100_tco`, verify the drivers in the BOOTy kernel). Standard ioctls
  (`WDIOC_SETTIMEOUT`, `WDIOC_KEEPALIVE`), magic close `V` to stop.
- B: IPMI watchdog commands (`Set Watchdog Timer`, `Reset Watchdog Timer`)
  through `/dev/ipmi0`. Works without `ipmi_watchdog`; the BMC resets the
  host even if the kernel is dead.
- C: User-space timer that reboots. Does not cover kernel hangs.
- Recommendation: A when a device exists, else B. The supervisor pets only
  when the current step has reported progress (bytes written, state change)
  within its step budget. Long steps declare a larger budget.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `WATCHDOG` | `off`, `auto`, `require` | Default `off` first, `auto` later. |
| `WATCHDOG_TIMEOUT_SEC` | integer, default `300` | Hardware timeout. |
| `WATCHDOG_STEP_BUDGETS` | JSON map step to seconds | Overrides. |
| `WATCHDOG_HANDOFF` | `stop`, `keep` | `keep` leaves it armed for the OS (needs an OS-side owner). |

### CAPRF and bm4x impact

- CAPRF sees the reset as a power event and reads the out-of-band record.
- `RedfishMachine.status.lastRun.code` `BOOTY-WDT-001` when the watchdog
  fired.

## Security considerations

- A misconfigured watchdog can reset hosts in a loop. Limit consecutive
  watchdog resets (counter in a UEFI variable) and then stay in `wait`.

## Testing strategy

- Unit: supervisor logic with a fake device and a fake clock.
- KVM: QEMU `i6300esb` (`-device i6300esb -action watchdog=reset`, verify
  the option syntax) with a step that hangs; assert reset and the record on
  the next boot. IPMI path with `ipmi-bmc-sim` (verify watchdog support).
- Real hardware: one hang test per vendor.

## Phased rollout

1. PR: `/dev/watchdog` supervisor with step progress, `WATCHDOG=off` default.
2. PR: IPMI watchdog fallback.
3. PR: reset counter and out-of-band record.
4. PR: default `auto`.

## Effort estimate

| Phase | Days |
|-------|------|
| Supervisor and device | 3 to 4 |
| IPMI fallback | 2 to 3 |
| Counter and record | 1 to 2 |
| Tests | 2 to 3 |
| **Total** | **8 to 12** |

## Open questions

- Which watchdog drivers are present on the fleet hardware?
- Should the installed OS keep the watchdog armed until first boot completes
  (ties into [proposal-rollback.md](proposal-rollback.md))?

## Related proposals

- [proposal-resilient-provisioning.md](proposal-resilient-provisioning.md)
- [proposal-rollback.md](proposal-rollback.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
- [proposal-ipmi-local.md](proposal-ipmi-local.md)
