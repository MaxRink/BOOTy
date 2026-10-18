# Rescue and Maintenance Mode

## Status

Proposal. Design only. Nothing in this document is implemented. It extends
[proposal-rescue-mode.md](proposal-rescue-mode.md).

## Priority

P2

## Summary

Turn the rescue shell into an audited maintenance mode that CAPRF can
trigger on demand: time-limited access with short-lived SSH certificates
(not static keys or password hashes), a debug toolkit flavor, session
recording, and automatic exit when the lease ends.

## Problem and motivation

- `RESCUE_MODE=shell|wait` installs `RESCUE_SSH_PUBKEY` and
  `RESCUE_PASSWORD_HASH` and starts `dropbear` when present
  (`pkg/rescue/rescue.go`). The key is long-lived, and there is no audit
  trail or time limit (`RESCUE_TIMEOUT` is parsed but unused,
  `docs/configuration.md`).
- Operators need a way to enter a host on purpose (not only after a
  failure) to debug hardware or network problems.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Modes | `reboot`, `retry`, `shell`, `wait`. | `pkg/rescue/rescue.go`, `docs/caprf-integration.md` |
| Access | Static SSH key and crypt hash; dropbear if in the ramdisk. | `pkg/rescue/rescue.go` (`setupSSHKeys`, `setupPasswordHash`) |
| Timeout | `RESCUE_TIMEOUT` unused. | `docs/configuration.md` |
| Standby | Commands `provision`, `deprovision`, `reboot`, `health-check`. | `docs/caprf-integration.md` |

## Goals

- `MODE=maintenance` and a standby command `maintenance` with a lease.
- SSH certificate authentication with a CA public key from `/deploy`;
  certificates valid for minutes, with principals per operator.
- Session recording (command log or full TTY recording) shipped to the log
  sinks.
- Hard lease end: reboot or return to standby.

## Non-goals

- A web console. The BMC console remains the fallback.

## Design options

- A: dropbear with `authorized_keys` options (`expiry-time`, verify dropbear
  support). Limited.
- B: OpenSSH `sshd` with `TrustedUserCAKeys` in a debug flavor. Standard
  certificates. Larger.
- C: Go SSH server (`golang.org/x/crypto/ssh`) inside BOOTy with
  certificate checks, a PTY, recording and lease logic. No extra binary.
- Recommendation: C. It fits the "no shell-outs" rule and gives full control
  over audit and lease. Keep the static key path for `CAPRF_LEGACY_COMPAT`.

Toolkit: a `debug` flavor with `ethtool`, `devlink`, `nvme`, `smartctl`,
`ipmitool`, `tcpdump`, `mstflint`, `lspci`, `dmidecode` and `vtysh`.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `MAINTENANCE_SSH_CA` | public key | Trusted user CA. |
| `MAINTENANCE_LEASE_SEC` | integer | Access window. |
| `MAINTENANCE_END_ACTION` | `reboot`, `standby` | |
| `MAINTENANCE_RECORD` | `commands`, `tty`, `off` | |
| `RESCUE_TIMEOUT` | integer | Implement it as the lease for `shell` and `wait`. |

### CAPRF and bm4x impact

- A `maintenance` standby command with lease and reason; a CAPRF annotation
  or API to request it; an approval step for production.
- CAPRF signs operator SSH certificates (or uses the site SSH CA).

## Security considerations

- No passwords. No long-lived keys outside legacy mode.
- Bind the SSH server only to the provisioning interface.
- Every session is recorded and shipped; recording failure ends the session
  when `MAINTENANCE_RECORD` is required.
- Lease end is enforced by BOOTy, not by the client.

## Testing strategy

- Unit: certificate validation (expired, wrong principal, wrong CA), lease
  timer, recorder.
- KVM: `maintenance-ssh` with a test CA; an expired certificate fails; the
  lease ends with a reboot.
- kindmetal and CAPRF e2e: request, approve, connect, expire.

## Phased rollout

1. PR: implement `RESCUE_TIMEOUT` as a lease (small, fixes an unused key).
2. PR: Go SSH server with certificate auth.
3. PR: recording and shipping.
4. PR: `maintenance` mode and standby command.
5. PR: debug flavor.
6. CAPRF MR: request and approval flow.

## Effort estimate

| Phase | Days |
|-------|------|
| Lease | 1 to 2 |
| SSH server | 5 to 7 |
| Recording | 3 to 4 |
| Mode and command | 2 to 3 |
| Debug flavor | 2 to 3 |
| CAPRF flow | 4 to 6 |
| **Total** | **17 to 25** |

## Open questions

- Is there a site SSH CA we must use?
- Which approval process applies to production hosts?

## Related proposals

- [proposal-rescue-mode.md](proposal-rescue-mode.md)
- [proposal-log-shipping.md](proposal-log-shipping.md)
- [proposal-console-status.md](proposal-console-status.md)
