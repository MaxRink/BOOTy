# Streaming Telemetry

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

Add continuous, near-real-time telemetry from BOOTy during provisioning and
in standby (agent) mode, beyond periodic snapshots. Evaluate gNMI subscribe
(dial-out from BOOTy, OpenConfig paths), OTLP streaming, a server-sent
events or WebSocket feed to CAPRF, and NATS or Kafka. Recommend gNMI
dial-out for network and transceiver data that matches the fabric
telemetry, and the CAPRF event stream for provisioning progress. Both use
the identity and mTLS model of
[proposal-provisioning-metrics.md](proposal-provisioning-metrics.md).

## Problem and motivation

- Snapshots every 30 seconds hide short events: a BGP flap, a link that
  bounces during the image stream, a DOM value that crosses a threshold.
- Hosts in standby (`MODE=standby`) can wait for hours. Operators and the
  cable-check want live link, LLDP, BGP and optics state from them.
- The fabric switches already stream telemetry to collectors (verify which
  protocol the leaves use: gNMI, sFlow, SNMP). Host-side data in the same
  model and the same collector makes cabling and link problems visible from
  both ends.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Snapshots | Planned in the metrics proposal. Not implemented. | [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md) |
| Standby | Heartbeat every 30 s, command poll every 10 s. | `docs/caprf-integration.md` (Agent mode contract) |
| Network state | GoBGP API in-process; FRR through vtysh in FRR mode; LLDP TX, and LLDP RX on the `feat/cablecheck` branch. | `pkg/network/gobgp/`, `pkg/network/frr/`, `pkg/network/lldp/` |
| gRPC | Indirect dependency through GoBGP. | `go.mod` |
| Lab | ContainerLab topologies with FRR leaves and GoBGP. | `test/e2e/clab/` |

## Goals

- Sub-second to few-second updates for link, BGP, BFD, LLDP and DOM changes.
- Provisioning progress as a stream (phase, step, bytes written).
- The same data model as the fabric where one exists.
- Work from behind NAT or a fabric where nobody can dial in to BOOTy.
- A fixed CPU and memory budget in the ramdisk.

## Non-goals

- Replacing the fabric telemetry system.
- Streaming after the handoff. The installed OS has its own agents.

## Options

| Option | Direction | Model | Pro | Con |
|--------|-----------|-------|-----|-----|
| gNMI subscribe, dial-in | Collector to BOOTy | OpenConfig | Standard for network devices | BOOTy is not reachable: NAT, EVPN VRFs, no stable address |
| gNMI dial-out (BOOTy opens a gRPC connection and the collector subscribes over it, for example with a gRPC tunnel, verify the openconfig `grpctunnel` project, or a vendor dial-out RPC, verify) | BOOTy to collector | OpenConfig | Reachability solved; same model as switches | Dial-out is not standardized in gNMI itself (verify); collector support varies (gnmic has tunnel server support, verify) |
| OTLP streaming (metrics and logs with short export interval) | BOOTy to collector | OTel | Same pipeline as logs and traces | No OpenConfig paths; less fit for on-change network state |
| SSE or WebSocket to CAPRF | BOOTy to CAPRF | BOOTy JSON events | Simplest; one identity; fits the event contract | CAPRF becomes a telemetry hub; scaling |
| NATS or Kafka | BOOTy to broker | Any | Fan-out to many consumers | One more system; authentication per broker; the Kafka handler exists but is not wired |

## Recommendation

- **Provisioning progress**: the CAPRF event channel with a streaming mode
  (HTTP/2 long-lived request with NDJSON, or a WebSocket, verify which CAPRF
  prefers). Events are on-change.
- **Network and optics state**: gNMI dial-out through a gRPC tunnel to the
  site gnmic or the fabric collector, only when configured. Use OpenConfig
  paths so the same dashboards and alerts apply to host and switch ports.
- **Not now**: NATS. Kafka only through the existing handler for logs.

## Data model

| Path | Source | Mode |
|------|--------|------|
| `/interfaces/interface[name]/state/oper-status`, `counters` | netlink, ethtool stats | on-change (status), sample 10 s (counters) |
| `/lldp/interfaces/interface[name]/neighbors/neighbor/state` | LLDP RX (`feat/cablecheck`) | on-change |
| `/network-instances/network-instance[name]/protocols/protocol[BGP]/bgp/neighbors/neighbor/state` (session-state, counters) | GoBGP API or FRR | on-change |
| `/bfd/interfaces/interface/peers/peer/state` (verify the OpenConfig BFD path) | GoBGP BFD | on-change |
| `/components/component[name]/transceiver/state` and physical channels (input-power, output-power, laser-bias-current, temperature) | ethtool module EEPROM DOM | sample 30 s, on-change for thresholds |
| `/booty/provisioning/state` (phase, step, code) | orchestrator | on-change |
| `/booty/provisioning/image` (bytes, rate) | image stream | sample 2 s |
| `/booty/health/check[name]` | health checks | on-change |

Custom paths live under a `/booty/` YANG module (to be written; small).

## Budget and cardinality

- Sample intervals: 2 s for image progress, 10 s for counters, 30 s for DOM.
  On-change for states.
- Suppress redundant updates (`suppress_redundant`, verify gNMI option) and
  use heartbeat intervals of 60 s for on-change paths.
- Cardinality is bounded by the hardware (NICs, sessions, transceivers).
  Cap at 16 interfaces and 16 sessions; aggregate the rest.
- Budget: at most 2 percent of one CPU core and 16 MiB RSS for the telemetry
  goroutines (measure; set a test gate). Streaming pauses while the image
  write saturates the CPU, except for progress events.

## Security

- The same mTLS identity as metric push (see
  [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)).
- Dial-out only. BOOTy listens on no port for gNMI.
- The collector enforces machine identity and path allow-lists.
- No secrets in any path or value.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `STREAM_EVENTS` | boolean | Streaming mode on the CAPRF event channel. |
| `GNMI_DIALOUT_ADDR` | `host:port` | Tunnel server. Empty disables gNMI. |
| `GNMI_PATHS` | list | Subset of the data model. Default: interfaces, lldp, bgp, transceiver. |
| `GNMI_CA_FILE`, `GNMI_CLIENT_CERT`, `GNMI_CLIENT_KEY` | paths | Or the shared identity from the secrets proposal. |
| `STREAM_SAMPLE_INTERVALS` | JSON | Override sample rates. |

### CAPRF and bm4x impact

- CAPRF accepts the streaming event connection and updates
  `RedfishMachine.status.currentStep` live.
- bm4x and the cable-check consume the gNMI stream from the collector and
  compare host LLDP and DOM with the switch side.
- Standby hosts keep a stream open; the heartbeat can become a stream
  keepalive.

## Testing strategy

- Unit: OpenConfig path rendering from netlink, GoBGP and ethtool fixtures.
  Sample scheduler. Budget accounting.
- ContainerLab: add `gnmic` as a tunnel server and collector to the
  production template lab; BOOTy dials out; assert LLDP and BGP paths match
  the FRR leaf state; flap a link and assert an on-change update within 2 s.
- Protected push tests: valid client certificate accepted; expired, wrong CA
  and wrong machine identity rejected; replayed `seq` rejected.
- Load test: 1000 simulated BOOTy streams against one gnmic and one CAPRF
  replica; measure CPU, memory and latency.
- KVM matrix: standby with a stream for 30 minutes; assert the budget.
- Real hardware: DOM values compared with the switch DOM stream.

## Phased rollout

1. PR: streaming mode for CAPRF events (on-change provisioning state).
2. CAPRF MR: streaming receiver.
3. PR: OpenConfig path renderer for interfaces, LLDP, BGP, BFD (no transport).
4. PR: gNMI dial-out client with mTLS.
5. Lab PR: gnmic in ContainerLab and assertions.
6. PR: transceiver DOM paths (shared with the cable-check metrics).
7. Load test and budget gate.

## Effort estimate

| Phase | Days |
|-------|------|
| Streaming event mode and CAPRF receiver | 5 to 7 |
| OpenConfig renderer | 4 to 6 |
| gNMI dial-out client | 5 to 8 |
| Lab and tests | 4 to 6 |
| DOM paths | 2 to 3 |
| Load test | 2 to 3 |
| **Total** | **22 to 33** |

## Open questions

- Which telemetry protocol and collector do the fabric leaves use today
  (verify with the network team)?
- Does the site collector accept gRPC tunnel dial-out?
- Is the BOOTy binary size budget fine with a gNMI client (gRPC is already
  linked through GoBGP; verify the delta)?

## Related proposals

- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
- [proposal-agent-mode.md](proposal-agent-mode.md)
- [proposal-log-shipping.md](proposal-log-shipping.md)
- [proposal-zero-touch-onboarding.md](proposal-zero-touch-onboarding.md)
