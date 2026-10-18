# Native Log Shipping

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented.
[proposal-kafka-logging.md](proposal-kafka-logging.md) covers Kafka; this
proposal generalizes the sink model.

## Priority

P1

## Summary

Ship BOOTy logs as structured JSON with stable fields to one or more sinks:
the existing CAPRF `/log` endpoint, syslog (RFC 5424 over TCP or TLS), OTLP
logs, Loki push, the Fluent Bit forward protocol, or a plain HTTP JSON batch.
Buffer in RAM with a bounded size, apply backpressure rules, retry with a RAM
spool, redact secrets, and use TLS or mTLS with the CA from the boot config.
Also ship kernel messages and the console. Flush before every handoff, and
leave a crash trail in pstore that the next boot ships.

## Problem and motivation

- `LOG_URL` sends one `text/plain` POST per log record at `INFO` or higher.
  The handler buffers 256 lines and drops lines when the buffer is full
  (`pkg/caprf/loghandler.go`, `main.go` `NewRemoteHandler(..., 256)`). A slow
  CAPRF loses log lines exactly when something goes wrong.
- The lines are text. CAPRF matches text to derive progress
  (`classifyLogMessage` in CAPRF). There are no stable fields such as run ID
  or event code.
- Kernel messages (driver errors, NVMe timeouts, link flaps) do not reach any
  sink.
- Sites run Loki, OpenSearch, Splunk or syslog collectors. They cannot attach
  them without CAPRF in the path.
- `pkg/logging/kafka.go` exists but has no config keys and is not wired.
- Logs written just before kexec, reboot or a panic are lost.

## Current state

> **Update:** #711 merged: the CAPRF log handler uses a bounded ring with
> level-aware eviction and counted drops, and flushes with a deadline
> (`LOG_FLUSH_TIMEOUT_SEC`) before every handoff. The table below is the state
> before that work.

| Area | Today | Reference |
|------|-------|-----------|
| Logger | `log/slog` only (AGENTS rule). Console handler plus remote handler. | `main.go`, `AGENTS.md` |
| CAPRF sink | `RemoteHandler`: 256-line buffer, drops with periodic warning, redacts sensitive attribute names. | `pkg/caprf/loghandler.go` (`isSensitiveKey`, `redactRecord`) |
| Fan-out | `slog` multi handler. | `pkg/logging/multi.go` |
| Kafka | Handler and adapter; not wired, no keys. | `pkg/logging/kafka.go` |
| Debug dump | Diagnostic dump to the log on failure. | `pkg/debug/dump.go`, `pkg/provision/debug_test.go` |
| Crash artifacts | Collect `pstore` and `ramoops` data of the installed OS before destructive steps. | `pkg/crash/collector_linux.go` |
| URL redaction | Shared `pkg/redact` for URLs. | `AGENTS.md` |

## Goals

- One structured record schema for every sink.
- Pluggable sinks, several at once.
- No silent loss: bounded buffers with counted, reported drops, and a
  priority rule that keeps errors.
- Secrets never leave the node.
- Kernel and console capture.
- Flush before handoff and a crash trail across reboot.

## Non-goals

- A log storage backend.
- Shipping logs of the installed OS. The OS has its own agent.

## Record schema

| Field | Type | Note |
|-------|------|------|
| `ts` | RFC 3339 with nanoseconds | Wall clock. |
| `mono_ns` | integer | `CLOCK_BOOTTIME`. Orders records also before the time sync (#713) has set the wall clock. |
| `level` | `DEBUG`, `INFO`, `WARN`, `ERROR` | |
| `msg` | string | Human text. Not a contract. |
| `run_id` | string | Set by CAPRF (`RUN_ID`), or a random UUID. |
| `machine_uid` | string | SMBIOS system UUID. |
| `host` | string | `HOSTNAME`. |
| `mode` | string | `provision`, `standby`, ... |
| `phase` | string | boot, network, image, configure, handoff. Fixed set. |
| `step` | string | Step name from the step list. |
| `code` | string | Event or failure code (`BOOTY-...`, see [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)). |
| `seq` | integer | Per-run sequence. Detects gaps. |
| `source` | `booty`, `kernel`, `console` | |
| `traceparent` | string | From [proposal-provisioning-tracing.md](proposal-provisioning-tracing.md). |
| `attrs` | object | Other `slog` attributes after redaction. |
| `version` | string | BOOTy version and flavor. |

## Sinks

| Sink | Transport | Notes |
|------|-----------|-------|
| `caprf` | Existing `LOG_URL`; new batch mode posts `application/x-ndjson` | Keep the `text/plain` single-line mode for older CAPRF. |
| `syslog` | RFC 5424 over TCP with octet counting (RFC 6587), or TLS (RFC 5425) | Structured data element `[booty@<PEN> run_id=... code=...]`; the private enterprise number needs a decision (verify). |
| `otlp` | OTLP/HTTP JSON logs | Hand-written JSON encoding of the small log record subset, to avoid the SDK size (verify the size of the SDK first). |
| `loki` | Loki push API JSON (`/loki/api/v1/push`, verify) | Labels only `job`, `host`, `mode`; everything else in the line. |
| `fluent` | Fluent Bit forward protocol (msgpack over TCP or TLS, verify the spec version) | Needs a msgpack encoder; small. |
| `http` | Plain HTTP JSON batch (`POST` array) | Generic. |
| `kafka` | Existing handler | Wire it as one more sink; see the Kafka proposal. |

Recommendation for the first version: `caprf` batch, `http` and `syslog`.
Add `otlp` and `loki` next. Add `fluent` and `kafka` on demand.

## Buffering, backpressure and retry

- One in-RAM ring per sink with a byte limit (default 4 MiB, configurable)
  and a record limit.
- Batching by size (64 KiB) and time (1 s).
- When the ring is full: drop `DEBUG` first, then `INFO`. Keep `WARN` and
  `ERROR` by evicting the oldest `INFO` records. Count drops per level and
  send a `BOOTY-LOG-001` record with the counts when the sink recovers.
- Retry with exponential backoff (`cenkalti/backoff/v5`, per the AGENTS
  library table, verify that it is already a dependency) and jitter. Respect
  `Retry-After`.
- A RAM spool (tmpfs file, bounded) holds batches that failed, so a long
  outage does not grow the ring.
- Logging never blocks a provisioning step. The sink goroutine owns the
  network I/O.

## Redaction

- Keep the existing key-name rules (`isSensitiveKey`) and apply them to every
  sink.
- Add value rules: bearer tokens, JWTs (three base64url parts), `password=`
  and `token=` query parameters, PEM private key blocks, LUKS key material,
  crypt(3) hashes. Use `pkg/redact` for URLs.
- Redact before the record enters any ring, so a crash dump of the ring holds
  no secrets.
- A test corpus of known secret shapes must stay redacted.

## Transport security

- TLS by default. Plain TCP or HTTP only with an explicit insecure flag, like
  `INSECURE_TRANSPORT`.
- The CA comes from the boot config (`LOG_CA_FILE` in `/deploy`, or the seed
  CA pin from [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)).
- mTLS with a client certificate from `/deploy`, or a short-lived certificate
  issued by CAPRF after the token exchange.
- Never send the CAPRF bearer token to a third-party sink.

## Kernel and console capture

- Kernel: read `/dev/kmsg` (structured records with priority, sequence and
  monotonic time). Map the priority to `level`. Rate-limit noisy sources.
  Ship from BOOTy start; the kernel ring holds the earlier records.
- Console: the kernel console already goes to the serial port. BOOTy writes
  its own records to the console only at `INFO` and above. For out-of-band
  capture, see [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md).

## Delivery across handoff

- Before kexec, reboot or power off: stop new records, flush every sink with a
  deadline (default 5 s), then report undelivered counts in the final
  callback if it is still possible.
- On failure paths (`RESCUE_MODE`, `FAILURE_ACTION=wait`), keep the sinks
  running.
- Crash trail: enable `ramoops` or `efi-pstore` in the BOOTy kernel (verify the
  kernel config), and write the last N records and the failure code to
  pstore before handoff. On the next BOOTy boot, read `/sys/fs/pstore`, ship
  the old records with `source=pstore`, then clear them. The crash collector
  in `pkg/crash` already reads pstore of the installed OS; reuse its reader.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `LOG_FORMAT` | `text`, `json` | Console format. Default `text`. |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | Default `info`. |
| `LOG_SINKS` | JSON array of sink objects | `{"type":"syslog","address":"logs.example:6514","tls":true}`. |
| `LOG_URL_BATCH` | boolean | NDJSON batch mode for the CAPRF sink. |
| `LOG_BUFFER_BYTES` | integer | Per sink ring size. |
| `LOG_SPOOL_BYTES` | integer | RAM spool size. |
| `LOG_CA_FILE` | ramdisk path | CA bundle for log sinks. |
| `LOG_CLIENT_CERT`, `LOG_CLIENT_KEY` | ramdisk paths | mTLS. |
| `LOG_KERNEL` | boolean | Ship `/dev/kmsg`. |
| `LOG_FLUSH_TIMEOUT_SEC` | integer, default `5` | Before handoff. |
| `LOG_PSTORE` | boolean | Crash trail. |

Sink URLs are fixed for standby payloads, like the other URLs that carry
data off the node.

### CAPRF and CAPI API impact

- CAPRF accepts NDJSON on `/log` and stores records with `run_id` and `seq`.
- CAPRF can forward to the site log stack, so BOOTy needs only one sink in
  simple setups.
- `RedfishMachine.status.lastRun.logDrops` when BOOTy reports drops.

## Security considerations

- Redaction happens before buffering. Tests enforce it.
- Logs reveal topology, serials and addresses. Use TLS and restrict access in
  the backend.
- A malicious sink endpoint could stall BOOTy. Sinks are bounded and never
  block steps.
- pstore content survives reboots. Write no secrets to it, and clear it after
  shipping.

## Testing strategy

- Unit: schema golden files. Redaction corpus. Ring eviction order. Retry and
  spool with a fake server (`httptest`). RFC 5424 framing. Loki and OTLP JSON
  shapes against recorded examples. Fuzz the redactor.
- KVM matrix: `log-sinks` with a test syslog-TLS receiver and an HTTP
  receiver, a sink outage in the middle of the run, and a flush before kexec.
  `log-pstore` with a forced panic and a second boot that ships the trail.
- ContainerLab: `e2e_boot` with a Loki or syslog container in the topology.
- kindmetal and CAPRF e2e: NDJSON `/log` ingestion.
- Real hardware: kernel log volume during NIC and NVMe probing.

## Phased rollout

1. PR: record schema and `LOG_FORMAT=json`.
2. PR: sink interface, ring, retry and spool; `http` sink.
3. PR: CAPRF NDJSON batch mode.
4. PR: value-based redaction and the corpus.
5. PR: `syslog` sink with TLS and mTLS.
6. PR: `/dev/kmsg` shipping.
7. PR: flush before handoff and pstore trail.
8. PR: `otlp` and `loki` sinks.
9. CAPRF MR: NDJSON ingestion and forwarding.

## Effort estimate

| Phase | Days |
|-------|------|
| Schema and JSON format | 2 to 3 |
| Sink framework with ring, retry, spool | 5 to 7 |
| CAPRF batch mode | 1 to 2 |
| Redaction | 2 to 3 |
| syslog | 3 to 4 |
| Kernel capture | 2 to 3 |
| Flush and pstore trail | 4 to 6 |
| OTLP and Loki | 4 to 6 |
| CAPRF ingestion | 3 to 5 |
| **Total** | **26 to 39** |

## Open questions

- Which sinks do the target sites run today?
- Do we need a private enterprise number for RFC 5424 structured data?
- Is a RAM spool enough, or do we want an on-disk spool on a state partition
  (A/B `BOOTY-STATE`) for long standby sessions?

## Related proposals

- [proposal-kafka-logging.md](proposal-kafka-logging.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-provisioning-tracing.md](proposal-provisioning-tracing.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
- [proposal-crash-artifact-upload.md](proposal-crash-artifact-upload.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
