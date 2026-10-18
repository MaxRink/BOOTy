# Time Sync Before TLS

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented.

## Priority

P1

## Summary

Get a trustworthy clock early, before the first TLS handshake that checks
certificate validity, and handle clock skew explicitly. Use NTP (or NTS
where available), PTP on fabrics that provide it, or a Roughtime-style
authenticated source, with the RTC as the starting point and a monotonic
clock for all durations.

## Problem and motivation

- BOOTy does not sync the time (`docs/caprf-integration.md`, token expiry
  notes). It uses the RTC, which can be wrong after a board swap, a dead
  CMOS battery, or a BIOS reset.
- A wrong clock breaks TLS certificate validation (`notBefore`, `notAfter`),
  JWT expiry checks, and log timestamps. Today the only workaround is
  `SKIP_TLS_VERIFY`, which disables much more than time checks.

## Current state

> **Update:** #713 and #740 merged: BOOTy syncs the clock before the first
> TLS certificate check (CAPRF `Date`, NTP), writes the RTC back
> (`TIME_WRITE_RTC`) and reports `BOOTY-TIM-001`. See
> [configuration.md](configuration.md#time-sync). The table below is the state
> before that work.

| Area | Today | Reference |
|------|-------|-----------|
| Clock | RTC only. No NTP client. | `grep` finds no time sync code |
| TLS | Go `crypto/tls` with system roots or `SKIP_TLS_VERIFY`. | `pkg/image/stream.go`, `pkg/caprf/client.go` |
| Tokens | `TOKEN_EXPIRES_AT` needs a correct clock; `TOKEN_EXPIRES_IN` does not. | `docs/caprf-integration.md` |

## Goals

- Correct wall-clock time within seconds before the first certificate check.
- Clear failure (`BOOTY-TIM-001`) when no source is reachable and the RTC
  looks wrong.
- Write the corrected time to the RTC for the installed OS.

## Non-goals

- Precision timing for workloads (the installed OS owns PTP).

## Design options

- A: SNTP client in Go (small; `github.com/beevik/ntp` or hand-written,
  verify licence). Unauthenticated.
- B: NTS (RFC 8915) client (verify library maturity). Authenticated.
- C: Roughtime (verify current deployment and library). Authenticated, coarse.
- D: Use the CAPRF HTTP `Date` header from a TLS connection that skips only
  the time check, with a pinned CA. Simple, authenticated by the pin, coarse
  (one second).
- E: PTP via `linuxptp` (adds a binary; only if the fabric provides PTP).
- Recommendation: D as the always-available baseline (the CAPRF connection
  exists anyway), A or B against site NTP servers when configured, and a
  sanity window: if the RTC is within the window of the authenticated time,
  keep it.

Skew handling: a custom `VerifyConnection` that, before sync, accepts a
certificate outside its validity window only when the chain is pinned and
the skew is reported; after sync, normal verification applies.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `TIME_SOURCES` | list of `ntp://`, `nts://`, `caprf` | Default `caprf`. |
| `TIME_MAX_SKEW_SEC` | integer, default `300` | Above this, correct the clock or fail. |
| `TIME_REQUIRE_SYNC` | boolean | Fail when no source answers. |
| `TIME_WRITE_RTC` | boolean, default `true` | |

### CAPRF and bm4x impact

- CAPRF responses already carry `Date`. Optionally a `/time` endpoint with a
  signed timestamp.
- Report the measured skew in metrics (`booty_clock_skew_seconds`).

## Security considerations

- Unauthenticated NTP lets an on-path attacker move the clock and make
  expired certificates valid. Prefer NTS or the pinned CAPRF source.
- The skew-tolerant verification before sync must require a pinned CA.

## Testing strategy

- Unit: skew logic with a fake clock; `VerifyConnection` cases.
- KVM: start QEMU with `-rtc base=2001-01-01` and a CAPRF test server with a
  valid certificate; provisioning must succeed and the RTC must be corrected.
- ContainerLab: an NTP container in the lab.

## Phased rollout

1. PR: CAPRF `Date` based sync with a pinned CA and skew reporting.
2. PR: SNTP client.
3. PR: RTC write.
4. PR: NTS if a library is mature.

## Effort estimate

| Phase | Days |
|-------|------|
| CAPRF date sync and skew-tolerant TLS | 3 to 4 |
| SNTP | 1 to 2 |
| RTC write | 1 |
| NTS | 3 to 5 |
| **Total** | **8 to 12** |

## Open questions

- Which NTP or NTS servers are reachable from the provisioning network?
- Does the fabric offer PTP to hosts?

## Related proposals

- [proposal-jwt-auth.md](proposal-jwt-auth.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
