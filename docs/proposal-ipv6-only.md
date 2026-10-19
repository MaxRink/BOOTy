# IPv6-Only Provisioning

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

Make every part of the provisioning flow work without IPv4: address
configuration with SLAAC and DHCPv6, DNS over IPv6, IPv6 URLs for images,
callbacks and registries, IPv6 BGP underlay and EVPN, and IPv6 in the
persisted network config of the target. Add a CI scenario with IPv4
disabled to keep it that way.

## Problem and motivation

- Sites move to IPv6-only management and provisioning networks.
- The DHCP client is IPv4 (`pkg/network/dhcp4`). There is no
  DHCPv6 client and no explicit SLAAC handling outside the GoBGP device
  setup (`accept_ra` in `pkg/network/gobgp/devices.go`).

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| DHCP | IPv4 client. | `pkg/network/dhcp.go`, `go.mod` |
| Static | `VLANS` accepts IPv6 in brackets. | `docs/configuration.md` |
| GoBGP | IPv6 link-local and `accept_ra` settings; IPv6 Type-5 routes. | `pkg/network/gobgp/devices.go`, CHANGELOG (#651) |
| Persistence | netplan, networkd, NetworkManager writers. | `pkg/network/persist/persist.go` |
| Tests | No IPv4-disabled scenario. | `.github/workflows/kvm-matrix.yml` |

## Goals

- SLAAC and DHCPv6 (stateful and stateless) address configuration.
- No IPv4 literal or IPv4-only code path in the flow (audit).
- Image source probing, CAPRF callbacks, OCI pulls and NTP over IPv6.
- Persisted target config with IPv6.

## Non-goals

- IPv6 for BMC networks (CAPRF side).

## Design options

- DHCPv6 client: A) `github.com/insomniacslk/dhcp` (dhcpv6 package, verify
  licence and size); B) hand-written minimal client. Recommendation: A.
- SLAAC: kernel autoconfiguration with `accept_ra=1`, wait for a global
  address and a default route with a timeout.
- Audit: grep for `net.IPv4`, `"tcp4"`, `0.0.0.0`, `To4()` and IPv4-only
  parsing; add a lint rule or test.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `IP_FAMILY` | `dual`, `ipv4`, `ipv6` | Default `dual` (today's behaviour plus IPv6 when available). |
| `DHCPV6` | `off`, `stateless`, `stateful` | |
| `SLAAC_WAIT_SEC` | integer | |

### CAPRF and bm4x impact

- CAPRF renders IPv6 URLs and must serve on IPv6. Virtual media URLs with
  IPv6 literals need brackets (verify BMC support for IPv6 virtual media
  URLs).
- Network data contract carries IPv6 fields.

## Security considerations

- Router advertisement spoofing on the provisioning segment. Rely on RA guard
  in the fabric; document the requirement.

## Testing strategy

- Unit: DHCPv6 exchange against a fake server; address family audit test.
- KVM: `ipv6-only` scenario with IPv4 disabled in QEMU user networking or a
  bridged IPv6-only test network with radvd and a DHCPv6 server (verify
  tooling in CI).
- ContainerLab: an IPv6-only variant of the DHCP topology.

## Phased rollout

1. PR: address family audit test and fixes.
2. PR: SLAAC wait.
3. PR: DHCPv6 client.
4. PR: `ipv6-only` KVM and ContainerLab scenarios.
5. CAPRF MR: IPv6 endpoints.

## Effort estimate

| Phase | Days |
|-------|------|
| Audit and fixes | 3 to 5 |
| SLAAC | 2 |
| DHCPv6 | 4 to 6 |
| Scenarios | 3 to 5 |
| CAPRF | 2 to 4 |
| **Total** | **14 to 22** |

## Open questions

- Which sites need IPv6-only provisioning and when?
- Do all BMCs accept IPv6 virtual media URLs?

## Related proposals

- [proposal-bgp-networking-modes.md](proposal-bgp-networking-modes.md)
- [proposal-network-persistence.md](proposal-network-persistence.md)
- [proposal-time-sync.md](proposal-time-sync.md)
