# Console Status Screen

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

Show a compact, always-current status screen on the local console (VGA and
the BMC KVM) for data centre technicians: host name, serial, run ID, mode,
phase and step, progress, network state per uplink, the last error with its
code, and a QR code that links to the run in CAPRF or bm4x. Serial consoles
keep the plain log stream; the status screen runs on the VGA console
(`tty1`).

## Problem and motivation

- At the rack or in the BMC KVM window, a technician sees an ASCII art banner
  (`pkg/ux/captain.go`), system info and then scrolling logs. Finding the
  failure reason takes time and access to CAPRF.
- A QR code or short URL with the run ID connects the physical host to the
  run record without typing.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Banner | `ux.Captain()` and `ux.SysInfo()` at start. | `main.go`, `pkg/ux/` |
| Logs | `slog` to the console. | `main.go` |
| Failure codes | Planned in the metrics proposal. | [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md) |

## Goals

- A status view that redraws on state change, readable at 80x25.
- No secrets on screen.
- Works without a framebuffer driver (text mode) and with one.

## Non-goals

- Interactive menus. Input stays with the rescue and maintenance modes.

## Design options

- A: ANSI text redraw on `/dev/tty1` with a small Go renderer; QR code as
  Unicode half blocks (a Go QR library, verify licence and size, for example
  `rsc.io/qr`). Recommendation.
- B: Framebuffer graphics. More code, driver-dependent.
- C: Serial-only status lines. Already covered by the out-of-band status line
  in [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md).

The QR code encodes a URL template from CAPRF, for example
`https://caprf.example/runs/<run id>`, with no token. Show the run ID in text
too.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `CONSOLE_STATUS` | `off`, `tty1`, `auto` | `auto`: on when a VGA console exists. |
| `CONSOLE_STATUS_URL` | URL template with `{run}` | QR target. |
| `CONSOLE_BANNER` | boolean | Keep or drop the ASCII art. |

### CAPRF and bm4x impact

- A run page in CAPRF or bm4x that resolves a run ID.

## Security considerations

- Show no tokens, URLs with credentials, or IP addresses of secret services.
  The QR URL needs authentication on the server side.

## Testing strategy

- Unit: renderer golden output at 80x25 for each phase and a failure.
- KVM: screenshot of the QEMU VGA console through QMP `screendump` and an
  assertion on text via the serial mirror.

## Phased rollout

1. PR: status model fed by the step events.
2. PR: text renderer on `tty1`.
3. PR: QR code.
4. CAPRF MR: run page.

## Effort estimate

| Phase | Days |
|-------|------|
| Model | 1 to 2 |
| Renderer | 2 to 3 |
| QR | 1 |
| CAPRF page | 2 to 4 |
| **Total** | **6 to 10** |

## Open questions

- Is a run page in CAPRF acceptable, or should the QR point to bm4x?

## Related proposals

- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
- [proposal-rescue-maintenance.md](proposal-rescue-maintenance.md)
