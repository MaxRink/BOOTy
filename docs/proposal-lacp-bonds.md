# LACP Bonds With Partner Verification

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

Make 802.3ad bonds a verified feature: check LACP partner state per member
(actor and partner system ID, key, port, collecting and distributing
flags), compare the partner with the expected switch pair from LLDP and
NetBox, and report mismatches as part of the cable check. Add a real LACP
lab, because the current bond lab does not negotiate LACP.

## Problem and motivation

- `BOND_INTERFACES` and `BOND_MODE` (default `802.3ad`) create `bond0`
  (`docs/configuration.md`). BOOTy does not check that LACP negotiated, or
  that both members reach the same MLAG pair.
- The README states that the bond lab "does not prove 802.3ad/LACP
  negotiation" (`README.md`, enforced by `docs_consistency_test.go`).
- A half-working bond (one member in individual state, or members on two
  unrelated switches) passes provisioning and fails later.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Bond creation | netlink bond with mode and members. | `pkg/network/bond.go` |
| Lab | `topology-lacp.clab.yml`, `make test-e2e-lacp` alias of the bond test. | `test/e2e/clab/`, `Makefile` |
| LLDP | TX on `main`; RX on `feat/cablecheck`. | `pkg/network/lldp/` |

## Goals

- Read per-member LACP state from netlink (bond slave info: aggregator ID,
  actor and partner oper port state, verify the netlink attributes) or
  `/proc/net/bonding/bond0`.
- Fail or warn when a member is not in the active aggregator, when partner
  system IDs differ unexpectedly (MLAG uses one shared system ID, verify), or
  when LLDP neighbours do not match the expected pair.
- A real LACP lab.

## Non-goals

- Switch configuration.

## Design options

- Data source: netlink `IFLA_BOND_SLAVE_*` and `IFLA_BOND_AD_INFO`
  attributes through `vishvananda/netlink` (verify coverage) or parse
  `/proc/net/bonding`. Recommendation: netlink with a `/proc` fallback.
- Lab: FRR containers do not speak LACP. Use Linux bonds on both ends in
  network namespaces, or a vrnetlab switch image with LACP (verify licence),
  or Open vSwitch with LACP (verify). Recommendation: Linux bond on both
  sides first.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `BOND_VERIFY` | `off`, `warn`, `fail` | |
| `BOND_WAIT_SEC` | integer | Time to reach collecting and distributing. |
| `BOND_EXPECTED_PARTNERS` | JSON | Optional expected switch IDs and ports (from NetBox). |

### CAPRF and bm4x impact

- Per-member LACP state in inventory and in the cable-check metrics
  (`booty_bond_member_state`).
- bm4x provides the expected pair from NetBox.

## Security considerations

- None beyond data exposure of topology.

## Testing strategy

- Unit: parser for netlink and `/proc` bond state fixtures.
- ContainerLab: real LACP with Linux bonds on both ends; a variant where one
  member goes to a wrong peer must fail.
- KVM: two virtio NICs bridged to a bonded peer namespace.
- Real hardware: MLAG pairs per switch vendor.

## Phased rollout

1. PR: bond state reader and report.
2. PR: `BOND_VERIFY` with wait and fail.
3. Lab PR: real LACP topology and README update.
4. PR: expected partner comparison with LLDP.

## Effort estimate

| Phase | Days |
|-------|------|
| Reader and report | 2 to 3 |
| Verify mode | 1 to 2 |
| Lab | 3 to 5 |
| Partner comparison | 2 to 3 |
| **Total** | **8 to 13** |

## Open questions

- Do production leaves use MLAG or EVPN multihoming (ESI-LAG) for host
  bonds (verify)? The partner ID check differs.

## Related proposals

- [proposal-zero-touch-onboarding.md](proposal-zero-touch-onboarding.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-8021x-macsec.md](proposal-8021x-macsec.md)
