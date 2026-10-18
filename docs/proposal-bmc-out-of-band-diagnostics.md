# Out-of-Band Diagnostics Through the BMC

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented.

## Priority

P1 for the serial status channel and the SEL failure record. P2 for the
others.

## Summary

When the data network fails, BOOTy cannot send callbacks, logs or metrics,
and CAPRF sees only a timeout. The BMC is still reachable out of band. This
proposal evaluates every place where BOOTy can write a small status record
in-band that CAPRF or bm4x can read through Redfish or IPMI: the IPMI SEL,
Redfish log services through the host interface, writable BMC and FRU
fields, POST codes, the serial console capture, pstore, and UEFI variables.
It recommends a tiered "last provisioning result" record and states the
vendor coverage and limits of each option.

## Problem and motivation

- The most common hard failures happen before or in the network setup: no
  link, wrong VLAN, BGP not established, DHCP timeout, TLS failure. In these
  cases no callback reaches CAPRF.
- CAPRF then waits for its provision timeout and retries. The operator has
  no reason for the failure without a console session.
- A failure after the network is up can still lose its last log lines when
  the network drops or the host reboots.

## Current state

> **Update:** #718 merged: the `BOOTY1` serial status line (`OOB_STATUS`) and
> the `BootyLastResult` UEFI record (`OOB_EFI_VAR`). SEL records and the CAPRF
> readers are still open. The table below is the state before that work.

| Area | Today | Reference |
|------|-------|-----------|
| Callbacks | HTTP only. No fallback when the network fails. | `docs/caprf-integration.md` (End of a run) |
| Console | `slog` console handler on the kernel console. Serial console resolution for the installed OS. | `main.go`, `docs/serial-console-resolution.md` |
| IPMI | `pkg/ipmi` wraps `ipmitool` for BMC LAN config, next boot device, chassis control. Not used for status. | `pkg/ipmi/ipmi.go` |
| pstore | The crash collector reads `pstore` of the installed OS. BOOTy writes nothing there. | `pkg/crash/collector_linux.go` |
| UEFI vars | `pkg/efi` reads and writes variables. | `pkg/efi/vars.go` |
| CAPRF | Redfish through gofish for power, boot and virtual media. No log service reads. | CAPRF `internal/redfish/` |
| Proposal | Local IPMI management. | [proposal-ipmi-local.md](proposal-ipmi-local.md) |

## Goals

- A failure code and a phase reach CAPRF even when the data network is down.
- A small "last provisioning result" record survives reboot and power loss.
- No secrets in any BMC-visible store.
- Each option has a documented size limit, write path, read path and
  vendor coverage.

## Non-goals

- Full logs through the BMC. Only short status data.
- Managing BMC settings. See [proposal-ipmi-local.md](proposal-ipmi-local.md).

## Options

### O1: IPMI SEL records

- Write path: IPMI `Add SEL Entry` (NetFn Storage, command 0x44) through the
  in-band KCS or SSIF interface (`/dev/ipmi0`). `ipmitool sel add <file>`
  does the same. Go can send the command with the `ipmi_devintf` ioctls and
  no binary (verify the ioctl structure).
- Record format: 16 bytes. OEM timestamped records (type 0xC0 to 0xDF) carry
  a 3-byte manufacturer ID and 6 bytes of data. OEM non-timestamped records
  (0xE0 to 0xFF) carry 13 bytes (verify in the IPMI 2.0 spec). Enough for a
  failure code, a phase, a step index and a run ID fragment. A standard
  system event record with a BOOTy sensor type is an alternative that more
  BMCs render.
- Read path: Redfish `LogServices/SEL` (or vendor names: iDRAC `Sel`, iLO
  `IEL` or `IML`, XCC `PlatformLog` or `SEL`, verify) as `LogEntry` objects
  with raw data; or IPMI-over-LAN `sel list`.
- Size: SEL capacity is limited (hundreds to a few thousand records, verify
  per vendor). A full SEL can raise alarms or stop logging.
- Coverage: IPMI KCS exists on Dell, HPE, Lenovo, Supermicro, Fujitsu and
  OpenBMC systems (verify). Some BMCs reject or hide OEM records (verify).
- Verdict: **recommended** for one record per run (start, final result).

### O2: Redfish LogService entries through the host interface

- Write path: the Redfish Host Interface (DMTF DSP0270) gives the host a USB
  network link to the BMC, described by SMBIOS type 42. With credential
  bootstrapping, the host gets temporary BMC credentials (verify vendor
  support). Then BOOTy can call Redfish locally.
- Problem: standard Redfish log services are read-only for clients. Creating
  a `LogEntry` with POST is optional and rare (verify per vendor).
  `EventService.SubmitTestEvent` creates test events only.
- Verdict: **not recommended** as a general write path. Useful as a transport
  to O3 on vendors that support writable fields.

### O3: Writable BMC, system or FRU fields

- Candidates: Redfish `ComputerSystem.AssetTag` (often writable, about 32 to
  64 characters, verify), `ComputerSystem.HostName` (vendor-dependent), IPMI
  `Set System Info Parameters` (OS name, OS version, verify which parameters
  BMCs expose), FRU product extra fields (`ipmitool fru edit`, risky).
- Read path: Redfish properties.
- Problem: these fields have an owner. AssetTag is often used by asset
  management. FRU writes can corrupt the FRU on some boards.
- Verdict: IPMI `Set System Info Parameters` OS name and version are a
  reasonable **optional** channel for "BOOTy <version> <phase> <code>" where
  the BMC shows it in Redfish (verify). Do not write AssetTag or FRU.

### O4: POST codes and boot progress

- Write path: an `outb` to port 0x80 from Linux (needs `CAP_SYS_RAWIO` and
  `ioperm`; kernel lockdown under Secure Boot blocks raw port access, verify).
- Read path: BMCs that snoop port 0x80 show the last codes (OpenBMC
  `PostCode` log service, Supermicro and some others in the web UI, verify).
  Redfish `BootProgress.LastState` is set by firmware, not by the OS.
- Verdict: **experimental**. A two-byte phase code is useful on OpenBMC
  systems. Not portable. Blocked under lockdown.

### O5: SMBIOS OEM strings

- SMBIOS is provided by firmware and the BMC. The OS cannot write it at
  runtime.
- Verdict: **read direction only**. Useful as a seed input (see
  [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)).

### O6: Serial console as a log channel

- Write path: BOOTy writes one structured status line per phase change and
  failure to the serial console, for example
  `BOOTY1 run=<id8> phase=network code=BOOTY-NET-002 step=network-setup`.
  Keep the line short and fixed so a parser on the BMC side is simple.
- Read path:
  - CAPRF or bm4x opens SOL (IPMI SOL or the vendor SSH console) during a
    run and records it. Works on almost every BMC; one SOL session at a time.
  - BMC serial capture buffers: iDRAC Serial Data Capture (verify licence and
    Redfish OEM endpoint), iLO virtual serial port log (verify licence), XCC
    serial log (verify), OpenBMC `obmc-console` log and the HostLogger log
    service (verify).
- Size: kilobytes to a few MiB, vendor-dependent.
- Verdict: **recommended** as the first channel. Zero new hardware access,
  works without IPMI drivers, and helps humans too.

### O7: pstore (ramoops, efi-pstore)

- ramoops: a reserved RAM region survives a warm reboot or a panic reboot,
  not a power cycle. Needs a memory reservation on the kernel command line or
  in the device tree (verify for the BOOTy kernel and ISO config).
- efi-pstore: records go to UEFI variables and survive power loss. Small
  (kilobytes per record) and limited by NVRAM space (verify).
- Read path: the next BOOTy boot reads `/sys/fs/pstore`. Not out of band.
- Verdict: **recommended** for the crash trail of
  [proposal-log-shipping.md](proposal-log-shipping.md). It complements the
  out-of-band channels.

### O8: UEFI variable as a status record

- Write path: `pkg/efi` writes a non-volatile, boot-service and runtime
  variable under a BOOTy vendor GUID, for example `BootyLastResult`, with a
  small binary or JSON record (under 1 KiB).
- Read path: the next BOOTy boot or the installed OS. Redfish does not expose
  arbitrary UEFI variables (verify; some BIOS attribute registries map
  variables).
- Verdict: **recommended** for the "last provisioning result" seen by the
  next run, and for the rollback logic in
  [proposal-rollback.md](proposal-rollback.md). Write it only on change to
  save flash wear.

## Option summary

| Option | Size | Survives | Out-of-band read | Vendor-specific | Recommendation |
|--------|------|----------|------------------|-----------------|----------------|
| O1 SEL | 13 bytes per record | Power loss | Yes (Redfish SEL, IPMI) | Medium | Yes, start and final record |
| O2 Host interface log | n/a | n/a | n/a | High | No |
| O3 System info params | about 16 to 64 chars (verify) | Power loss (BMC) | Yes, where exposed | High | Optional |
| O4 POST codes | 1 to 2 bytes | Until next boot | OpenBMC and some vendors | High | Experimental |
| O5 SMBIOS | n/a | n/a | n/a | n/a | Input only |
| O6 Serial | KiB to MiB | BMC buffer only | Yes (SOL, capture) | Low to medium | Yes, first channel |
| O7 pstore | KiB | Warm reboot or power loss (efi) | No | Low | Yes, crash trail |
| O8 UEFI var | under 1 KiB | Power loss | No | Low | Yes, next-run record |

Vendor coverage per BMC is in [proposal-hardware-vendor-matrix.md](proposal-hardware-vendor-matrix.md).

## Recommended design

A `LastResult` record:

```json
{"v":1,"run":"<run id>","mode":"provision","phase":"network",
 "step":"network-setup","code":"BOOTY-NET-002","result":"failed",
 "booty":"<version>","t":"<RFC 3339>","mono":123456}
```

- Always: O6 serial line at each phase change and at the end.
- With IPMI available: O1 SEL record at start and at the end, encoded from
  the record (code number, phase number, result, run ID fragment).
- Always on UEFI hosts: O8 variable at the end, and at a failure.
- With pstore: O7 trail of the last log records.
- Optional: O3 system info OS name string.

CAPRF reads on timeout: the SEL through Redfish, then the serial capture
where available, then reports the code in the RedfishMachine condition
message. bm4x reads the same through its Redfish access.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `OOB_STATUS` | `off`, `serial`, `auto` (default once stable) | `auto` uses every available channel below. |
| `OOB_SEL` | `off`, `auto`, `require` | IPMI SEL records. |
| `OOB_EFI_VAR` | boolean | UEFI `BootyLastResult` variable. |
| `OOB_SYSINFO` | boolean | IPMI system info OS name. |
| `OOB_POSTCODE` | boolean | Port 0x80 codes. Experimental. |
| `LOG_PSTORE` | boolean | From [proposal-log-shipping.md](proposal-log-shipping.md). |

### CAPRF and bm4x impact

- CAPRF reads the SEL and the serial capture on a provision timeout and on
  error callbacks with no detail.
- CAPRF filters SEL entries by the BOOTy manufacturer ID or sensor type.
- `RedfishMachine.status.lastRun.code` from the out-of-band record when no
  callback arrived. Condition reason `OutOfBandFailureReport`.
- CAPRF clears or ignores BOOTy SEL records older than the run start.

## Security considerations

- BMC stores are readable by every BMC user and often exported to central
  BMC log collection. Write only codes, phases, versions and run ID
  fragments. Never tokens, URLs, IP addresses of secrets services, or
  passwords.
- Rate-limit SEL writes (at most a few per run) to avoid a full SEL.
- Host interface credential bootstrapping hands BMC credentials to the host.
  Do not enable it for this feature.
- Port 0x80 and raw IPMI access need privileges that kernel lockdown may
  forbid. Fail soft.

## Testing strategy

- Unit: record encoding to SEL bytes and back. Serial line format. UEFI
  variable encoding. Fuzz the decoders used by CAPRF.
- KVM matrix: `oob-sel` with QEMU `ipmi-bmc-sim` and `isa-ipmi-kcs` (verify
  SEL add support in the simulator) or an external `ipmi_sim` through
  `ipmi-bmc-extern`; a forced network failure; assert the SEL record.
  `oob-efivar` with OVMF; reboot; the next run reads the record.
  `oob-serial` parses the console output.
- ContainerLab: none.
- kindmetal and CAPRF e2e: Redfish mock or sushy-tools with a SEL log service
  (verify sushy support); CAPRF reports the code on timeout. An OpenBMC QEMU
  machine model (for example an AST2600 board, verify) for the BMC side in a
  nightly job.
- Real hardware: SEL add and Redfish read per vendor in the HIL lab.

## Phased rollout

1. PR: failure codes (shared with the metrics proposal) and the serial status
   line.
2. PR: UEFI `BootyLastResult` variable.
3. PR: SEL records through `/dev/ipmi0` without `ipmitool`.
4. CAPRF MR: SEL and serial capture read on timeout.
5. PR: pstore trail (shared with log shipping).
6. PR: optional system info and POST code channels.

## Effort estimate

| Phase | Days |
|-------|------|
| Serial status line | 1 to 2 |
| UEFI variable | 2 |
| SEL writer | 4 to 6 |
| CAPRF readers | 5 to 8 |
| pstore trail | (counted in log shipping) |
| Optional channels | 3 to 5 |
| Vendor validation | 4 to 6 |
| **Total** | **19 to 29** |

## Open questions

- Which BMCs in the fleet have a serial capture feature, and is it licensed?
- Does central BMC log collection already ingest SEL entries? Then BOOTy
  records appear in the existing monitoring for free.
- Which IANA private enterprise number do we use as the manufacturer ID?

## Related proposals

- [proposal-log-shipping.md](proposal-log-shipping.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-ipmi-local.md](proposal-ipmi-local.md)
- [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)
- [proposal-hardware-vendor-matrix.md](proposal-hardware-vendor-matrix.md)
