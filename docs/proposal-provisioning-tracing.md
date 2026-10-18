# Provisioning Run Tracing

## Status

Proposal. Design only. Nothing in this document is implemented. This
proposal narrows [proposal-telemetry-enhanced.md](proposal-telemetry-enhanced.md)
to traces of one provisioning run and to a size-aware export path.

## Priority

P2

## Summary

Model each provisioning run as one trace: a root span for the run, a child
span per step, and events for retries, source selection and fallbacks.
Propagate one trace ID from CAPRF through BOOTy to the target OS first boot.
Prefer an event-derived trace (CAPRF builds spans from typed BOOTy events)
over an OpenTelemetry SDK in the ramdisk, because of binary size.

## Problem and motivation

- The provisioning steps do not feed the telemetry collector today. The
  metrics summary has no step data and BOOTy sends no events
  (`docs/configuration.md`).
- Operators see a phase and a step name on the RedfishMachine, and log lines
  through `LOG_URL`. They cannot see where time goes across many runs, or
  correlate a slow run with a BMC, an image server or a disk.
- A run spans systems: CAPRF reconcile, BMC virtual media, firmware boot,
  BOOTy, image server, first boot of the OS. Without a shared ID, the
  correlation is manual.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Step loop | `executeStep` runs each step, with retries and checkpoints. | `pkg/provision/orchestrator.go` |
| Telemetry | Collector, `METRICS_URL`, `EVENT_URL`, not fed by steps. | `pkg/telemetry/`, `docs/configuration.md` |
| Logs | `slog`, `LOG_URL` shipping, optional Kafka. | `pkg/caprf/loghandler.go`, `pkg/logging/` |
| Size | Flavors with size budgets. | `docs/size-optimization.md` |

## Goals

- Step-level timing for every run, available in CAPRF and in a tracing
  backend.
- One trace ID across CAPRF, BOOTy and first boot.
- No large dependency in the default flavors.

## Non-goals

- Continuous node tracing after provisioning.
- A metrics backend. Metrics stay in the telemetry proposal.

## Design options

### Option A: OpenTelemetry Go SDK in BOOTy

- Pro: standard OTLP export, direct to a collector.
- Con: adds several MiB to the binary (verify with a size test) and gRPC or
  protobuf HTTP code. Needs network access to a collector from the
  provisioning network.

### Option B: Event-derived spans

BOOTy sends `StepStarted` and `StepSucceeded` or `StepFailed` events with
monotonic timestamps and a `traceparent` (W3C Trace Context) that CAPRF
passes in `/deploy/vars`. CAPRF converts events to spans and exports them
with its own OTel SDK.

- Pro: no new BOOTy dependency. Works with the event contract of
  [proposal-capi-integration.md](proposal-capi-integration.md). CAPRF already
  has the network path.
- Con: spans arrive only when events arrive. Lost events mean gaps. Mitigate
  with `seq` numbers and a final summary event.

### Option C: Write OTLP JSON by hand

- Pro: no SDK.
- Con: reimplements a protocol. Rejected.

### Recommendation

Option B. Add Option A only in a debug flavor if a direct export is needed.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `TRACEPARENT` | W3C `traceparent` value | Set by CAPRF. BOOTy adds it to every event and log line. |
| `TRACE_TARGET` | boolean | Write the `traceparent` into the target (for example `/etc/booty/trace.env`) so first-boot tools can continue the trace. |

### CAPRF and CAPI API impact

- CAPRF starts a span per reconcile phase and per BMC call, and creates the
  `traceparent` for the run.
- CAPRF exports spans to a configured OTLP endpoint.
- `RedfishMachine.status` keeps the last trace ID for lookup.

## Security considerations

- Span attributes must not contain secrets, tokens or full URLs. Reuse the
  BOOTy and CAPRF redaction.
- Trace IDs are not secret, but do not use them as authentication.

## Testing strategy

- Unit: event emission per step in the orchestrator with a fake sink. Order
  and timing fields. Redaction.
- KVM matrix: assert a complete span set for one provision run against the
  test CAPRF server.
- ContainerLab: `e2e_boot` asserts events from all BOOTy containers.
- kindmetal and CAPRF e2e: an OTel collector in kind receives the trace.
- Real hardware: none specific.

## Phased rollout

1. PR: step events in `executeStep` (shared with the CAPI proposal).
2. PR: `TRACEPARENT` in events and logs.
3. CAPRF MR: span conversion and export.
4. PR: `TRACE_TARGET`.

## Effort estimate

| Phase | Days |
|-------|------|
| BOOTy step events and trace context | 2 to 3 |
| CAPRF conversion and export | 4 to 6 |
| Target propagation | 1 |
| Tests | 2 to 3 |
| **Total** | **9 to 13** |

## Open questions

- Is there an OTel collector in the management clusters?
- Do we need traces for deprovision and standby too? (Probably yes, same
  mechanism.)

## Related proposals

- [proposal-telemetry-enhanced.md](proposal-telemetry-enhanced.md)
- [proposal-observability.md](proposal-observability.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
