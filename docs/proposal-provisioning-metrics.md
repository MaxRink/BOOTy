# Provisioning Metrics

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented. It refines
[proposal-observability.md](proposal-observability.md) and
[proposal-telemetry-enhanced.md](proposal-telemetry-enhanced.md) for a
short-lived ramdisk, and pairs with
[proposal-provisioning-tracing.md](proposal-provisioning-tracing.md) and
[proposal-log-shipping.md](proposal-log-shipping.md).

## Priority

P1

## Summary

Define what BOOTy measures in each phase, with stable names, units and
failure codes. Deliver the data in two ways: a pushed final snapshot that
always arrives (embedded in the CAPRF event contract, optionally also as
OTLP metrics), and an optional Prometheus exposition endpoint while BOOTy
runs, for debugging and for long standby sessions. CAPRF aggregates per-run
data into fleet KPIs with bounded label cardinality.

## Problem and motivation

- The orchestrator creates a `telemetry.Collector`
  (`pkg/provision/orchestrator.go`), but the steps do not feed it. The
  metrics summary has no step data and BOOTy sends no events
  (`docs/configuration.md`, Health, inventory, firmware and telemetry).
- `pkg/telemetry` has counters, gauges, histograms, a step tracker and an
  event emitter, but no exposition format and no stable metric names.
- Operators cannot answer: how long does a provision take at p95, where does
  the time go, which failure class is the most common, which image server or
  rack is slow.
- A ramdisk lives for minutes. Pull-based scraping misses most of it, and
  the scraper usually cannot reach the provisioning network.

## Current state

> **Update:** #706 merged: stable failure codes ([failure-codes.md](failure-codes.md))
> in error callbacks, and the orchestrator feeds the step tracker. The
> snapshot event, the endpoint and protected push are still open. The table
> below is the state before that work.

| Area | Today | Reference |
|------|-------|-----------|
| Primitives | `Counter`, `Gauge`, `Histogram`, `Metrics.Snapshot` (JSON). | `pkg/telemetry/metrics.go` |
| Steps | `StepTracker` with start, end, skip and retry records. Not called by the orchestrator. | `pkg/telemetry/step.go` |
| Collector | `RecordStep`, `RecordImage`, `RecordDisk`, `Summarize`. Created but not fed. | `pkg/telemetry/collector.go`, `pkg/provision/orchestrator.go` |
| Push | `TELEMETRY_ENABLED`, `METRICS_URL` (fallback `TELEMETRY_URL`), `EVENT_URL`. | `docs/configuration.md` |
| Pull | No HTTP server in BOOTy. | `grep ListenAndServe` finds none |
| Progress | `WriteCounter` and a progress ticker during the image copy. | `pkg/image/stream.go` |
| Errors | Error classes for retries (transient or permanent). | `pkg/provision/errors.go`, `pkg/provision/retry.go` |

## Goals

- A fixed metric catalogue with names, units, types and labels.
- Stable failure codes for every step failure.
- A final snapshot that reaches CAPRF even when the run fails.
- An optional live endpoint.
- Fleet KPIs in CAPRF and bm4x dashboards.
- Bounded cardinality.

## Non-goals

- A metrics backend. Sites use their existing Prometheus or OTel stack.
- Node metrics after the handoff. node-exporter covers them.

## Metric catalogue

Naming follows Prometheus conventions with the `booty_` prefix and base
units. Every metric carries the run labels in the snapshot envelope, not as
per-sample labels (see Cardinality).

### Boot and network

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `booty_boot_to_init_seconds` | gauge | | Kernel start (`CLOCK_BOOTTIME` at BOOTy start) to PID 1 main. |
| `booty_network_ready_seconds` | gauge | `mode` (dhcp, static, gobgp, frr) | PID 1 to connectivity check pass. |
| `booty_nic_link_up_seconds` | gauge | `nic` (bounded, see below) | Time until carrier. |
| `booty_nic_link_speed_bits` | gauge | `nic` | Negotiated speed. |
| `booty_bgp_session_established_seconds` | gauge | `peer_index` | Start to Established per session. |
| `booty_bgp_session_flaps_total` | counter | `peer_index` | Transitions out of Established. |
| `booty_bfd_session_up_seconds` | gauge | `peer_index` | Start to BFD Up. |
| `booty_evpn_routes_received` | gauge | `type` (2, 5) | Routes at handoff. |
| `booty_dhcp_attempts_total` | counter | | DHCP attempts. |

### Image

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `booty_image_source_probe_seconds` | gauge | `source_index` | Probe latency per source. |
| `booty_image_download_bytes_total` | counter | | Compressed bytes received. |
| `booty_image_written_bytes_total` | counter | | Bytes written to disk. |
| `booty_image_download_seconds` | gauge | | Time in network reads. |
| `booty_image_decompress_seconds` | gauge | `format` | Time in the decoder. |
| `booty_image_write_seconds` | gauge | `disk_index` | Time in writes and `fsync`. |
| `booty_image_verify_seconds` | gauge | `kind` (checksum, gpg, cosign) | Verification time. |
| `booty_image_retries_total` | counter | `reason` | Open and resume retries. |
| `booty_image_resumes_total` | counter | | Range resumes (after [proposal-image-delivery.md](proposal-image-delivery.md)). |

Rates (MB/s) are derived: bytes divided by seconds. BOOTy does not export
rates as separate metrics.

### Disk and storage

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `booty_step_duration_seconds` | gauge | `step` | One sample per step of the step list. The step names are a fixed set (`TestProvisionStepCount`). |
| `booty_step_retries_total` | counter | `step` | |
| `booty_disk_wipe_seconds` | gauge | `method` | Wipe or erase time. |
| `booty_raid_sync_seconds` | gauge | | RAID create or resync wait. |
| `booty_luks_format_seconds` | gauge | | After LUKS wiring. |

### Handoff, firmware and health

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `booty_handoff_seconds` | gauge | `kind` (kexec, reboot, poweroff) | `report-success` to the syscall. |
| `booty_health_check_result` | gauge | `check`, `result` | 1 for the result. Fixed check set. |
| `booty_firmware_compliance` | gauge | `component_class` | 1 compliant, 0 not. |
| `booty_run_result` | gauge | `result`, `code` | Final result and failure code. |
| `booty_run_duration_seconds` | gauge | `mode` | Whole run. |

### Failure codes

- Format `BOOTY-<AREA>-<NNN>`, for example `BOOTY-IMG-004` (checksum mismatch),
  `BOOTY-NET-002` (BGP not established), `BOOTY-DSK-010` (target disk in use).
- One Go table maps each code to a class (`transient`, `permanent`,
  `config`, `hardware`, `environment`), a step and a short text.
- Codes never change meaning. Retired codes stay reserved.
- The same code appears in the error callback, the events, the logs
  ([proposal-log-shipping.md](proposal-log-shipping.md)) and the
  out-of-band record
  ([proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)).

### Cable-check metrics (one consumer)

The bm4x cable-check work compares cabling and optics with the expected
topology (verify the scope and owner of that work; the BOOTy
`feat/cablecheck` branch receives LLDP and compares it with
systemd-networkd). The cable-check data fits this catalogue:

| Metric | Type | Labels | Source |
|--------|------|--------|--------|
| `booty_nic_lldp_match` | gauge | `nic` | 1 when the LLDP neighbour matches the expected switch and port. |
| `booty_sfp_rx_power_dbm` | gauge | `nic`, `lane` | ethtool module EEPROM DOM (`ETHTOOL_GMODULEEEPROM`, verify the SFF-8472 and SFF-8636 offsets). |
| `booty_sfp_tx_power_dbm` | gauge | `nic`, `lane` | same |
| `booty_sfp_temperature_celsius` | gauge | `nic` | same |
| `booty_sfp_bias_current_amperes` | gauge | `nic`, `lane` | same |
| `booty_nic_errors_total` | counter | `nic`, `kind` (crc, symbol, drop) | ethtool stats, bounded set |

The cable-check consumer reads the snapshot from CAPRF or bm4x, not from the
node. The DOM thresholds belong to the cable-check baseline, not to BOOTy.

## Cardinality

- Put run identity (run ID, machine UID, host name, cluster, failure domain,
  BOOTy version) in the snapshot envelope or as resource attributes. Do not
  put them on every sample.
- `nic`, `peer_index`, `disk_index`, `source_index` are bounded by the
  hardware. Cap them at 16 each, and fold the rest into `other`.
- `step`, `check`, `code`, `format`, `kind` come from fixed Go tables.
- Never use URLs, serial numbers or error texts as labels.
- In fleet storage, CAPRF converts per-run gauges into histograms per KPI
  (see below) so that the time series count is independent of the fleet
  size.

## Format and transport

| Option | Fit for a short-lived ramdisk | Pro | Con |
|--------|-------------------------------|-----|-----|
| A. Prometheus exposition endpoint (pull) | Poor alone | Standard, easy to debug with curl. Useful in standby. | The scraper must reach the provisioning network and discover the ramdisk. Data is gone after the handoff. |
| B. Pushgateway | Medium | Prometheus native. | Keeps stale series forever unless deleted. Not meant for per-instance job results (verify Prometheus guidance). One more service. |
| C. Prometheus remote-write | Medium | Direct into Prometheus-compatible stores. | Protobuf and snappy code in BOOTy. Needs a writable endpoint from the provisioning network. |
| D. OTLP metrics push | Good | Vendor-neutral. Same pipeline as traces and logs. | OTel SDK size in the ramdisk (verify); or hand-written OTLP/HTTP JSON. |
| E. Final snapshot in the CAPRF event contract | Best | Uses the existing authenticated channel. Always arrives with the result. No new network path. | CAPRF must convert and export. |

Recommendation:

1. **E always.** BOOTy sends a `MetricsSnapshot` event (JSON, schema in
   `docs/schema/`) at the end of every run, success or failure, and before
   every handoff. It also sends periodic partial snapshots in long phases
   (image streaming, standby).
2. **A optional.** `METRICS_LISTEN=:9100` (off by default) serves Prometheus
   text format from the same registry, with `net/http` only. Useful in
   standby and in labs.
3. **D in CAPRF**, not in BOOTy. CAPRF exports aggregated metrics with its own
   OTel or Prometheus client.
4. Reject B and C in BOOTy.

## Native, protected metric shipping

BOOTy pushes its metrics itself. No scraper has to reach the ramdisk.

### Transport security

- TLS 1.3 only for metric push. mTLS with a client certificate.
- Client identity bound to the machine, in this order of preference:
  1. a TPM-resident key with a certificate issued after attestation (see
     [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md) and
     [proposal-credential-identity.md](proposal-credential-identity.md));
  2. a short-lived certificate (minutes to hours) issued by CAPRF or a SPIFFE
     server after the token exchange;
  3. the existing bearer token, for the CAPRF event channel only.
- Server authentication by a CA bundle from the boot config (`/deploy` or the
  seed CA pin) or a pinned server certificate public key hash.

### Authentication and authorization

- The receiver maps the client identity to exactly one machine. It sets the
  `machine`, `tenant` and `fleet` labels itself from its inventory. It
  ignores or rejects these labels when the client sends them.
- Replay protection: every batch carries `run_id`, a monotonic `seq` and a
  timestamp. The receiver rejects a `seq` it has seen for the run, and
  batches outside a time window when the clock is trusted (see
  [proposal-time-sync.md](proposal-time-sync.md)).
- No secrets in labels or values. The catalogue test enforces label names;
  redaction from [proposal-log-shipping.md](proposal-log-shipping.md)
  applies to string values.

### Integrity and authenticity

- Optional signed batches: a JWS (RFC 7515) or COSE_Sign1 (RFC 9052)
  envelope over the payload, signed with the machine key. This keeps the
  data verifiable when a proxy, a relay or the BMC path carries it.
- Prefer COSE for compact binary payloads; JWS for JSON on the CAPRF channel.
  Start with JWS because the event contract is JSON.
- Optional encryption of the spool at rest with a per-boot random key held
  in RAM only. The spool lives in tmpfs, so this protects only against a
  crash dump that is shipped later.

### Protocols

| Protocol | Security fit | Ramdisk fit | Server side | Verdict |
|----------|--------------|-------------|-------------|---------|
| OTLP/gRPC with mTLS | Good | gRPC is already an indirect dependency through GoBGP; the OTel SDK adds size (verify) | OTel Collector | Option for sites with a collector |
| OTLP/HTTP (protobuf or JSON) with mTLS | Good | Small with hand-written JSON | OTel Collector | Good second sink |
| Prometheus remote-write with mTLS | Good | Protobuf plus snappy | Prometheus, Mimir, Thanos receive | Only if the site has no collector |
| CAPRF event endpoint (JSON, bearer or mTLS) | Good, one identity system | Smallest | CAPRF | **Recommended primary** |

Recommendation: the CAPRF event endpoint is the primary, always-on channel.
Add OTLP/HTTP with mTLS as an optional second sink. CAPRF forwards to the
fleet store.

### Reliability

- Bounded RAM spool per sink (default 1 MiB), retry with backoff and jitter,
  flush with a deadline before kexec, reboot or power off.
- When every network sink fails, write the final summary (result, code,
  duration, step timings in compact form) to the out-of-band store (see
  [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)).

### Privacy and rate limits

- Inventory-like data (serials, MACs) does not belong in metrics. It goes
  into inventory events with stricter access.
- Client-side rate limit: at most one snapshot per interval and a byte budget
  per minute. Server-side rate limit per identity.

## Aggregation in CAPRF and bm4x

- CAPRF stores the last snapshot per `RedfishMachine` (summary on the status,
  full JSON in an owned object or a log sink).
- CAPRF exposes fleet metrics on its own `/metrics`:
  - `caprf_provision_runs_total{result, code_class}`
  - `caprf_provision_duration_seconds` histogram `{mode, image_mode}`
  - `caprf_provision_phase_duration_seconds` histogram `{phase}` with a fixed
    phase set (boot, network, image, configure, handoff)
  - `caprf_image_throughput_bytes_per_second` histogram
  - `caprf_failures_total{code}`, bounded by the code table
- Fleet KPIs (recording rules):
  - success rate per day and per failure domain,
  - p50 and p95 provisioning time,
  - failure category distribution,
  - image throughput p50 per image source class.
- bm4x reads the same CAPRF metrics, or the snapshot objects for
  per-host views such as cable-check (verify the bm4x integration point).

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `METRICS_SNAPSHOT` | boolean, default `true` when `EVENTS_URL` is set | Final and partial snapshots on the event channel. |
| `METRICS_SNAPSHOT_INTERVAL_SEC` | integer, default `30` | Partial snapshots in long phases. |
| `METRICS_LISTEN` | `host:port`, default empty | Prometheus endpoint. |
| `TELEMETRY_ENABLED`, `METRICS_URL` | existing | Kept. Send the same snapshot. |
| `CABLECHECK_DOM` | boolean | Read SFP DOM data for the cable-check consumer. |

### CAPRF and CAPI API impact

- New event kind `MetricsSnapshot` in the contract of
  [proposal-capi-integration.md](proposal-capi-integration.md).
- `RedfishMachine.status.lastRun`: duration, result, failure code.
- CAPRF `/metrics` additions listed above.

## Security considerations

- The Prometheus endpoint is unauthenticated text. Bind it only when
  configured. Never expose secrets or URLs as values or labels.
- Snapshots travel on the authenticated event channel.
- DOM and LLDP data reveal topology. Treat them as internal data.

## Testing strategy

- Unit: metric catalogue table (names, types, units, label sets). A test that
  fails when a new label is not in the bounded set. Failure code table:
  unique, every step has codes. Prometheus text format with
  `expfmt`-compatible output (verify whether to add the dependency or write
  the small text format by hand).
- KVM matrix: assert a complete snapshot for a provision run, a failed run
  (with code) and a standby session.
- ContainerLab: BGP and BFD timings in `e2e_gobgp` and the production
  template labs, compared with the lab's own measurements.
- kindmetal and CAPRF e2e: CAPRF `/metrics` shows the run, and the KPI rules
  evaluate.
- Real hardware: DOM values compared with switch-side readings.

## Phased rollout

1. PR: failure code table and codes in error callbacks.
2. PR: feed the step tracker from `executeStep`; snapshot JSON.
3. PR: image, network, BGP and BFD metrics.
4. PR: `MetricsSnapshot` event (after the event contract).
5. PR: optional `METRICS_LISTEN` endpoint.
6. CAPRF MR: storage, fleet metrics and recording rules.
7. PR: DOM and LLDP metrics for the cable-check consumer.
8. PR: mTLS client identity and TLS 1.3 for metric push; receiver-side label
   enforcement in CAPRF.
9. PR: JWS-signed batches and the out-of-band final summary fallback.
10. PR: optional OTLP/HTTP sink.

## Effort estimate

| Phase | Days |
|-------|------|
| Failure codes | 3 to 4 |
| Step tracker wiring and snapshot | 3 to 4 |
| Image, network, BGP, BFD metrics | 4 to 6 |
| Snapshot event | 1 to 2 |
| Prometheus endpoint | 2 |
| CAPRF aggregation | 5 to 8 |
| DOM and LLDP metrics | 3 to 5 |
| mTLS identity and receiver enforcement | 4 to 6 |
| Signed batches and out-of-band fallback | 3 to 4 |
| OTLP/HTTP sink | 2 to 3 |
| **Total** | **30 to 44** |

## Open questions

- Which team owns the fleet dashboards: CAPRF, bm4x or platform monitoring?
- Is the cable-check work in BOOTy, in bm4x, or both?
- Do we need per-host history, or only the last run plus fleet aggregates?

## Related proposals

- [proposal-observability.md](proposal-observability.md)
- [proposal-telemetry-enhanced.md](proposal-telemetry-enhanced.md)
- [proposal-provisioning-tracing.md](proposal-provisioning-tracing.md)
- [proposal-log-shipping.md](proposal-log-shipping.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
- [proposal-streaming-telemetry.md](proposal-streaming-telemetry.md)
- [proposal-credential-identity.md](proposal-credential-identity.md)
