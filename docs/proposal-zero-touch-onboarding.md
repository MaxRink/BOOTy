# Zero-Touch Onboarding and Discovery

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

An unknown or newly racked server boots BOOTy in a discovery mode. BOOTy
collects inventory and cabling (LLDP, link, DOM), validates both against
NetBox, and registers the host: it reports to a discovery endpoint that
creates the `RedfishHost` (or moves the bm4x host into its staged state) only
when the validation passes.

## Problem and motivation

- Onboarding needs manual steps: enter the BMC address and credentials,
  check the cabling, create the host object. Mistakes show up later as failed
  provisioning.
- BOOTy already collects inventory and runs LLDP, and the cable-check work
  compares LLDP with the expected topology (BOOTy `feat/cablecheck` branch,
  unmerged).

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Inventory | CPU, memory, disks, NICs, GPU, thermal, USB. | `pkg/inventory/` |
| LLDP | Transmit on `main`; receive and compare on `feat/cablecheck`. | `pkg/network/lldp/` |
| Modes | `check` mode runs health checks and reports. | `docs/caprf-integration.md` |
| CAPRF | `RedfishHost` is created by hand or by bm4x from templates. | CAPRF `api/v1alpha1/redfishhost_types.go`, bm4x-operator `redfishhosttemplate_types.go` |
| bm4x states | A staged flow is mentioned by the requester; not found in the local bm4x-operator clone (verify). | |

## Goals

- `MODE=discover`: inventory, cabling, health, then report and power off or
  wait.
- Validation against NetBox: serial, model, NIC MACs, switch and port per
  NIC, optics type.
- Self-registration with a single-use enrollment credential.

## Non-goals

- Creating NetBox data. NetBox is the source of truth; discovery only
  validates and reports differences.
- BMC credential rotation (separate concern).

## Design options

- A: BOOTy calls NetBox directly. Con: NetBox credentials on every host.
- B: BOOTy reports to a discovery service (CAPRF or bm4x), which validates
  against NetBox and creates objects. Recommendation: B.
- Boot trigger: PXE-less. Options are a factory default boot to a generic
  BOOTy over HTTP boot or virtual media inserted by an enrollment controller
  that scans BMC subnets (verify the network design), or DHCP option driven
  HTTP boot on an onboarding VLAN.

Validation result per check: `match`, `mismatch`, `missing`. The service
creates the host only with no `mismatch` on blocking checks, else it sets a
`DiscoveryFailed` status with the diff.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `MODE` | add `discover` | |
| `DISCOVERY_URL` | URL | Discovery service. |
| `DISCOVERY_TOKEN` | string | Single-use enrollment token, short lifetime. |
| `DISCOVERY_LLDP_WAIT_SEC` | integer | Time to collect LLDP frames on every NIC. |
| `DISCOVERY_SUCCESS_ACTION` | `poweroff`, `standby` | |

### CAPRF and bm4x impact

- A discovery endpoint (CAPRF or bm4x) with NetBox read access.
- A `DiscoveredHost` object or bm4x staged state with the inventory, the
  cabling and the validation result; an approval step creates the
  `RedfishHost`.

## Security considerations

- An attacker can boot any machine with BOOTy. The enrollment token must be
  single-use and scoped to discovery; the service accepts only hosts whose
  serial exists in NetBox, and requires approval for new serials.
- With TPM attestation (see [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)),
  bind the host record to the EK certificate.
- Discovery data contains serials and topology; restrict access.

## Testing strategy

- Unit: validation rules with NetBox fixtures.
- ContainerLab: the production template lab with LLDP on the leaves; a
  miscabled variant must produce a `mismatch`.
- KVM: `discover` mode end to end with a mock discovery service.
- kindmetal and CAPRF e2e: discovery creates a host after approval.
- Real hardware: one onboarding run per vendor.

## Phased rollout

1. PR: `MODE=discover` with inventory and LLDP (after the cable-check work
   lands).
2. Service MR: discovery endpoint with NetBox validation.
3. Service MR: object creation with approval.
4. PR: DOM and optics checks (shared with the metrics proposal).

## Effort estimate

| Phase | Days |
|-------|------|
| Discover mode | 4 to 6 |
| Discovery service and NetBox validation | 8 to 12 |
| Object creation and approval | 4 to 6 |
| Optics checks | 2 to 3 |
| **Total** | **18 to 27** |

## Open questions

- Does the discovery service live in CAPRF or in bm4x?
- How does an unknown server boot BOOTy without PXE in the target network
  design?

## Related proposals

- [proposal-capi-integration.md](proposal-capi-integration.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-streaming-telemetry.md](proposal-streaming-telemetry.md)
- [proposal-burn-in.md](proposal-burn-in.md)
- [proposal-lacp-bonds.md](proposal-lacp-bonds.md)
