# Better CAPI Integration

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1

## Summary

BOOTy collects much more data than CAPRF uses: inventory, firmware versions,
health results, step progress and A/B slot state. CAPRF on `main` receives
only `init`, `success`, `error`, log lines and a debug dump, and infers image
phases from log text. This proposal defines a structured, versioned
BOOTy-to-CAPRF contract (progress events, results, inventory, network data)
and maps it to the CAPI infrastructure machine contract (v1beta2 status,
conditions, addresses, failure domains, remediation and `clusterctl move`).
It uses cluster-api-provider-metal3 and Ironic as the parity reference.

## Problem and motivation

- CAPRF derives `RedfishMachine.status.currentStep` and image events by
  matching log text (`classifyLogMessage` in CAPRF
  `internal/provision/manager.go`). A changed log message breaks it.
- BOOTy posts inventory, firmware and health JSON only when CAPRF renders
  `INVENTORY_URL`, `FIRMWARE_URL` and `HEALTH_CHECK_URL`. CAPRF `main` has no
  receiver for them (`internal/server/server.go` routes: images, `/log`,
  `/debug`, `/status/`). The data is lost.
- `RedfishHost.status` has only `conditions`. Schedulers and operators cannot
  select hosts by CPU, memory, disk, NIC or firmware.
- CAPRF uses CAPI v1.14 with the v1beta1 contract label
  (`cluster.x-k8s.io/v1beta1=v1alpha1`). `RedfishMachineStatus` has `ready`
  but no `addresses` and no `initialization.provisioned`. The v1beta1
  contract is deprecated in recent CAPI releases (verify the removal
  release).
- MachineHealthCheck can only act on Node conditions. BOOTy health checks and
  BMC sensor faults do not reach CAPI.
- Network data (IP, routes, BGP and EVPN values) comes from files that CAPRF
  renders from its own config. There is no typed IPAM contract with the CAPI
  IPAM API.

## Current state

| Area | BOOTy today | CAPRF today | Reference |
|------|-------------|-------------|-----------|
| Status callbacks | `INIT_URL`, `SUCCESS_URL`, `ERROR_URL` with `text/plain` bodies. | `/status/{init,error,success}`; branch adds `heartbeat`. | `docs/caprf-integration.md`, CAPRF `internal/server/server.go` |
| Progress | Step names in log lines. `EVENT_URL` exists but steps do not feed the collector. | Log text classification. `currentStep`. | `pkg/telemetry/`, `docs/configuration.md` (Health, inventory, firmware and telemetry) |
| Inventory | `INVENTORY_ENABLED`, `INVENTORY_URL`. Extended inventory (GPU, thermal, USB). | No receiver. `RedfishHost.status` has conditions only. | `pkg/inventory/`, CAPRF `api/v1alpha1/redfishhost_types.go` |
| Firmware | `FIRMWARE_REPORT`, `FIRMWARE_URL`, min BIOS and BMC. | No receiver. | `pkg/firmware/collector.go` |
| Health | `HEALTH_CHECKS_ENABLED`, `HEALTH_CHECK_URL`. | No receiver. | `pkg/health/` |
| Agent | `MODE=standby` with heartbeat and commands. | Branch `feat/booty-consolidated` adds `/commands` (returns 204) and heartbeat. | `cmd/standby.go`, CAPRF branch |
| A/B | Slot state file. | Branch adds `RedfishMachineABSpec` and `RedfishMachineABStatus` (`bootedSlot`, `bootedUpgradeID`). | `docs/ab-partitioning.md`, CAPRF branch `api/v1alpha1/redfishmachine_types.go` |
| Provider selection | n/a | Branch adds the `BootyProvisioner` feature gate and a per-host provisioner type annotation. | CAPRF branch `pkg/featuregate/featuregate.go` |
| Failure domain | `FAILURE_DOMAIN`, `REGION`, `PROVIDER_ID` vars. | `RedfishHost.spec.failureDomain`. | `docs/caprf-integration.md` |
| Paused | n/a | Honors the CAPI paused annotation and condition. | CAPRF `internal/controllers/redfishmachine_controller.go` |

## Metal3 and Ironic parity reference

| Feature | Metal3 / Ironic | CAPRF + BOOTy today | Gap |
|---------|-----------------|--------------------|-----|
| Inspection | `BareMetalHost` inspection, `HardwareData` CRD | BOOTy inventory, not stored | Store inventory on the host |
| Host states | registering, inspecting, available, provisioning, provisioned, deprovisioning (verify names) | Machine phases only | Host lifecycle states |
| Cleaning | automated and manual cleaning steps | `decommission`, `deprovision`, wipe modes | Typed cleaning steps, secure erase per media |
| RAID | `spec.raid` hardware and software | `DISK_MIRRORING_MODE`, NVMe namespaces | Hardware RAID via Redfish |
| BIOS settings | `HostFirmwareSettings` CRD | Proposals only | See [proposal-bios-settings.md](proposal-bios-settings.md) |
| Firmware updates | `HostFirmwareComponents` CRD, Ironic firmware interface | Reporting only | See [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md) |
| Root device hints | `rootDeviceHints` (size, serial, WWN, model, rotational) | Device name or serial | More hint fields |
| Network data | `Metal3DataTemplate`, IP address manager, network_data.json | Rendered files | Typed network data, CAPI IPAM |
| Custom deploy | `customDeploy` | `IMAGE_MODE`, machine commands | Covered |
| Live ISO | `live-iso` image format | none | Optional |
| Detached hosts | `detached` annotation | n/a | Needed for `clusterctl move` safety |
| Remediation | `Metal3Remediation` reboot strategy | Restart annotation | External remediation template |
| Node reuse | `nodeReuse` | n/a | Optional |

## Goals

- Replace log text parsing with typed progress events.
- Store inventory, firmware and health data on `RedfishHost.status`.
- Move CAPRF to the CAPI v1beta2 infrastructure machine contract.
- Feed health signals into MachineHealthCheck and external remediation.
- Define a typed network data contract that maps to CAPI IPAM.
- Keep BOOTy agnostic: the contract is HTTP and JSON. BOOTy does not import
  Kubernetes packages.

## Non-goals

- Replacing CAPRF with Metal3.
- BOOTy talking to the Kubernetes API.
- A new BMC abstraction in BOOTy.

## Design options

### Option A: Extend the text callbacks

Add more `text/plain` messages and more URLs.

- Pro: no new endpoint style.
- Con: no schema. CAPRF still parses strings.

### Option B: One versioned JSON event endpoint

BOOTy posts typed events to one `EVENTS_URL`:

```json
{
  "apiVersion": "booty.telekom.de/v1",
  "kind": "ProvisionEvent",
  "runID": "uuid",
  "seq": 17,
  "time": "RFC3339",
  "type": "StepStarted|StepSucceeded|StepFailed|Progress|Result",
  "step": "stream-image",
  "progress": {"bytes": 123, "total": 456},
  "error": {"class": "transient|permanent|config|hardware", "message": "..."},
  "data": {"$ref": "inventory|firmware|health|ab|attestation"}
}
```

Large documents (inventory, firmware, health) use typed kinds
(`HardwareInventory`, `FirmwareReport`, `HealthReport`) on the same endpoint.
The old callbacks stay for compatibility.

- Pro: one schema, versioned, testable with JSON schema files. CAPRF can
  store `seq` to reject replays and detect gaps.
- Con: CAPRF needs a new handler and storage logic.

### Option C: gRPC stream

- Pro: typed, bidirectional, fits standby commands.
- Con: larger binary, harder to proxy, harder to debug with curl. BOOTy
  already has gRPC as an indirect dependency through GoBGP, but the size and
  complexity are not worth it for low-rate events.

### Recommendation

Option B. Publish the JSON schemas in BOOTy under `docs/schema/` and add a
contract test in both repositories.

## CAPI contract mapping

| CAPI field or signal | Source | Note |
|----------------------|--------|------|
| `RedfishMachine.status.initialization.provisioned` (v1beta2) | `Result` success | Replaces `ready`. Keep `ready` during the migration. |
| `RedfishMachine.status.addresses` | BOOTy network result (IPs on the provisioning and target interfaces), hostname | Lets CAPI and CPI use node addresses (verify the address types used by the CAPI contract). |
| `spec.providerID` | CAPRF | Unchanged. BOOTy writes it into the target (kubelet arg or cloud-init) as today. |
| `spec.failureDomain` | `RedfishHost.spec.failureDomain` | CAPRF reports failure domains on `RedfishCluster.status.failureDomains` (verify the v1beta2 shape). |
| Conditions | `StepFailed` classes | `Provisioned`, `HostHealthy`, `FirmwareCompliant`, `Attested`, `ImageVerified`. Use `metav1.Condition` with v1beta2 reasons. |
| MachineHealthCheck | Host conditions | MHC watches Node conditions. Add an `UnhealthyHardware` Node condition through a small node agent, or use `spec.remediationTemplate` with an external remediation controller that watches `RedfishHost` conditions. |
| External remediation | CAPRF | A `RedfishRemediation` type: power cycle, then reprovision, then mark host failed. Same pattern as `Metal3Remediation`. |
| In-place updates | A/B with `AB_PRESERVE_EXISTING` | CAPI has in-place update work with update extensions (verify the CAPI release and API). CAPRF can implement an update extension that runs a standby A/B upgrade. Until then keep the `upgradeID` flow from the branch. |
| `clusterctl move` | CAPRF objects | Move must not trigger reprovisioning. Persist all state on objects (no in-memory job state). Pause stops reconciles. Add a `detached`-like annotation on `RedfishHost` for hosts that keep running during a move. |

## Inventory into `RedfishHost.status`

- Store a compact summary on the host: CPU model and count, memory, disks
  (name, serial, size, rotational, model, WWN), NICs (name, MAC, driver,
  firmware, link speed, PCI address), GPU list, BIOS, BMC and TPM.
- Store the full JSON in a ConfigMap or a `RedfishHostInventory` object owned
  by the host, because `status` size is limited.
- Add printer columns and labels from the summary (for example
  `hardware.telekom.de/cpu-vendor`) so `hostSelector` can use them.
- Extend `RedfishHost.spec.rootDiskSelector` with Metal3-like hints: size,
  WWN, model, rotational, `by-path`.

## Network data contract

- Define a `NetworkData` JSON document: interfaces, bonds, VLANs, addresses,
  routes, DNS, and the BGP and EVPN values that BOOTy reads today from vars
  and `frr.conf`.
- CAPRF fills addresses from CAPI IPAM (`IPAddressClaim` and `IPAddress`) when
  a pool reference is set on the RedfishMachine template.
- BOOTy reads `/deploy/network-data.json` before it falls back to netplan and
  `frr.conf` files. It uses the same document for network persistence and
  cloud-init network config.
- The document is the same shape as the OpenStack `network_data.json` where
  possible (verify) so cloud-init can read it directly.

## Where CAPRF can hand more to BOOTy

- Disk selection: CAPRF picks the disk from stored inventory, BOOTy only
  validates it. Fewer surprises from device name reordering.
- Image source selection and mirrors: CAPRF passes a ranked list with
  failure domain affinity.
- Firmware and BIOS compliance: BOOTy checks in-band against a baseline that
  CAPRF passes, and reports drift.
- Pre-provisioning in standby: hosts wait in `MODE=standby`, keep inventory
  fresh with heartbeats, and accept a `provision` command for fast kexec
  installs.
- Deprovision cleaning steps: CAPRF sends a typed cleaning plan (secure
  erase, BIOS reset, firmware update) instead of one mode string.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `EVENTS_URL` | URL | Typed JSON events. Fixed for standby payloads. |
| `EVENTS_API_VERSION` | `v1` | Version negotiation. |
| `NETWORK_DATA` | ramdisk path, default `/deploy/network-data.json` | Typed network input. |
| `RUN_ID` | string | Set by CAPRF to correlate events with a provisioning attempt. |

Existing `INVENTORY_URL`, `FIRMWARE_URL`, `HEALTH_CHECK_URL` and `EVENT_URL`
stay. When `EVENTS_URL` is set, BOOTy sends the same data there.

### CAPRF API impact

- `RedfishHostStatus`: `inventory` summary, `firmware` summary,
  `health` summary, `lastSeen`, `provisioner` (BOOTy version, flavor).
- `RedfishMachineStatus`: `initialization`, `addresses`, `currentStep` from
  events, `lastError` with class.
- New types: `RedfishRemediationTemplate`, optional `RedfishHostInventory`.
- Webhooks for validation of new fields.

## Security considerations

- Events use the same bearer token and transport rules. Reject events for
  another machine. Rate-limit per machine.
- Inventory includes serial numbers and MAC addresses. Restrict RBAC on
  `RedfishHost` status and the inventory objects.
- `seq` and `runID` stop replay of old events into a new run.
- Do not include secrets or full URLs in events. Reuse BOOTy redaction.

## Testing strategy

- Unit: JSON schema validation of every event kind in BOOTy. Golden files
  shared with CAPRF.
- KVM matrix: assert the event sequence of a full provision against a test
  server (extend the CAPRF mock in `test/e2e/caprf_full_flow_test.go`).
- ContainerLab: `e2e_boot` asserts events from all three BOOTy containers.
- kindmetal and CAPRF e2e: inventory appears on `RedfishHost`, addresses on
  `RedfishMachine`, MHC remediation on an injected health fault, and
  `clusterctl move` of a provisioned cluster without reprovisioning.
- Real hardware: inventory accuracy per vendor (compare with Redfish
  inventory).

## Phased rollout

1. BOOTy PR: event schema and `EVENTS_URL` emitter for step events.
2. CAPRF MR: event receiver; derive `currentStep` from events; keep log
   parsing as a fallback.
3. BOOTy PR: inventory, firmware and health as typed kinds.
4. CAPRF MR: host status summary and inventory object.
5. CAPRF MR: v1beta2 contract fields (`initialization`, `addresses`,
   conditions).
6. BOOTy PR: `NETWORK_DATA` reader.
7. CAPRF MR: IPAM integration and network data rendering.
8. CAPRF MR: external remediation template.
9. CAPRF MR: `clusterctl move` e2e and detached handling.

## Effort estimate

| Phase | Days |
|-------|------|
| BOOTy event emitter and schema | 4 to 6 |
| CAPRF receiver | 4 to 6 |
| Inventory, firmware, health kinds and host status | 6 to 9 |
| v1beta2 contract | 5 to 8 |
| Network data and IPAM | 8 to 12 |
| Remediation | 5 to 8 |
| Move safety and e2e | 4 to 6 |
| **Total** | **36 to 55** |

## Open questions

- Does CAPRF move to v1beta2 before or after the BOOTy provisioner becomes the
  default?
- Is a separate inventory object acceptable, or should the full inventory
  live on the host status?
- Do we need a node agent for hardware conditions, or is external remediation
  on host conditions enough?
- Should the network data format follow OpenStack `network_data.json` or
  netplan v2?

## Metrics, logs and identity on the event contract

- The `MetricsSnapshot` kind and the failure codes are defined in
  [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md);
  streaming mode in [proposal-streaming-telemetry.md](proposal-streaming-telemetry.md).
- Log records with `run_id`, `seq` and `code` are defined in
  [proposal-log-shipping.md](proposal-log-shipping.md); CAPRF ingests them on
  `/log` in NDJSON.
- When no event arrives, CAPRF reads the out-of-band record from
  [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md).
- mTLS client identity for all channels: [proposal-credential-identity.md](proposal-credential-identity.md).
- Flatcar bootstrap uses the Ignition format (see [proposal-flatcar.md](proposal-flatcar.md)).

## Related proposals

- [caprf-integration.md](caprf-integration.md)
- [caprf-integration-plan.md](caprf-integration-plan.md)
- [proposal-agent-mode.md](proposal-agent-mode.md)
- [proposal-full-inventory.md](proposal-full-inventory.md)
- [proposal-webhooks.md](proposal-webhooks.md)
- [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md)
- [proposal-rollback.md](proposal-rollback.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-log-shipping.md](proposal-log-shipping.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
- [proposal-streaming-telemetry.md](proposal-streaming-telemetry.md)
- [proposal-flatcar.md](proposal-flatcar.md)
- [proposal-credential-identity.md](proposal-credential-identity.md)
