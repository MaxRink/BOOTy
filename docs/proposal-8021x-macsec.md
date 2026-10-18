# 802.1X Port Authentication and MACsec

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P3

## Summary

Support fabrics that require 802.1X on host ports before they forward
traffic, and fabrics that require MACsec on host links. BOOTy runs an
EAP-TLS supplicant with a machine certificate, and optionally sets up MACsec
with MKA. Both need a machine identity before the network is up.

## Problem and motivation

- On an 802.1X port the switch drops all traffic until the host
  authenticates. BOOTy then cannot reach CAPRF at all.
- MACsec on host links protects provisioning traffic on shared physical
  infrastructure.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| 802.1X | None. | |
| MACsec | None. | |
| Identity | Bearer token; certificate identity is planned in [proposal-credential-identity.md](proposal-credential-identity.md). | `docs/caprf-integration.md` |

## Goals

- EAP-TLS supplicant per port, before DHCP, LLDP-only traffic, or BGP.
- MACsec with MKA (CAK from EAP or a pre-shared CAK).
- Clear failure codes for authentication failures.

## Non-goals

- EAP methods other than EAP-TLS.

## Design options

- A: `wpa_supplicant` with the wired driver and MACsec support
  (`macsec_linux`, verify) in an opt-in flavor. Mature.
- B: Go EAPOL and EAP-TLS implementation. Small, but security-sensitive new
  code.
- Recommendation: A in a `secure-fabric` flavor. Kernel MACsec module
  required.

Credential bootstrap problem: the machine certificate must exist before the
network is up. Options: a certificate on the boot media (short-lived,
per machine, same exposure as the token today), a TPM-resident key with a
certificate from a previous enrollment, or a guest VLAN with restricted access
for enrollment (switch feature, verify).

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `DOT1X` | `off`, `eap-tls` | |
| `DOT1X_INTERFACES` | list | Default: all uplinks. |
| `DOT1X_CERT`, `DOT1X_KEY`, `DOT1X_CA` | paths or `tpm` for the key | |
| `MACSEC` | `off`, `mka-eap`, `mka-psk` | |
| `MACSEC_CAK`, `MACSEC_CKN` | from a secrets file, never from vars | |

### CAPRF and bm4x impact

- CAPRF issues per-machine 802.1X certificates (or uses the site PKI) and
  places them in the boot seed.
- bm4x knows which ports require 802.1X (NetBox port tags, verify).

## Security considerations

- Certificates on boot media are as exposed as tokens today. Prefer
  TPM-bound keys after the first enrollment.
- MACsec keys must never appear in vars or logs.

## Testing strategy

- Unit: config rendering for `wpa_supplicant`.
- ContainerLab: `hostapd` wired authenticator with FreeRADIUS (verify
  containers) on a test link; MACsec between two Linux namespaces.
- Real hardware: switch-side 802.1X on one lab leaf.

## Phased rollout

1. PR: `secure-fabric` flavor with `wpa_supplicant`.
2. PR: EAP-TLS per port before network setup.
3. PR: MACsec MKA.
4. CAPRF MR: certificate issuance.

## Effort estimate

| Phase | Days |
|-------|------|
| Flavor | 2 to 3 |
| EAP-TLS | 4 to 6 |
| MACsec | 4 to 6 |
| Lab | 3 to 5 |
| CAPRF | 3 to 5 |
| **Total** | **16 to 25** |

## Open questions

- Does any T-CaaS fabric require 802.1X or MACsec on host ports today?

## Related proposals

- [proposal-lacp-bonds.md](proposal-lacp-bonds.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
