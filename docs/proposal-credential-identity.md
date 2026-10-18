# Credentials and Machine Identity Without the Kernel Command Line

## Status

Proposal. Design only. Nothing in this document is implemented. It extends
[proposal-jwt-auth.md](proposal-jwt-auth.md).

## Priority

P1

## Summary

Remove long-lived credentials from the boot media, the kernel command line
and `/deploy/vars`. Replace them with short-lived, identity-bound
credentials: a single-use bootstrap credential is exchanged for a machine
identity (a short-lived certificate issued by CAPRF, or a SPIFFE SVID issued
by SPIRE with TPM node attestation). Confidential values are then fetched
just in time from Vault or CAPRF with that identity.

## Problem and motivation

- `TOKEN` sits in `/deploy/vars` inside a per-machine ISO, and the ISO URL
  holds the token in its path (CAPRF `docs/redfish-virtual-media.md`).
  Anyone who can read the BMC virtual media configuration or the image
  server logs can reuse it until it expires (CAPRF default 30 minutes plus
  skew).
- With `CAPRF_LEGACY_COMPAT`, every vars entry is exported to the process
  environment (`EXPORT_VARS=true`), where child processes can read it.
- Process environment values can also come from the kernel command line
  (`docs/configuration.md`, Process environment). `/proc/cmdline` is
  readable by every process and is measured into PCR 12 in UKI boots.
- Rescue password hashes, registry credentials and future LUKS keys need a
  better channel.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Token | `TOKEN` bearer; optional `TOKEN_URL` exchange for a JWT with renewal. | `pkg/auth/manager.go`, `docs/caprf-integration.md` (Authentication) |
| Expiry | `TOKEN_EXPIRES_IN`, `TOKEN_EXPIRES_AT`. | `pkg/caprf/token_vars.go` |
| Redaction | Sensitive key names redacted in logs. | `pkg/caprf/loghandler.go` |
| Registry credentials | go-containerregistry keychain (verify the source BOOTy uses). | `pkg/image/oci.go` |
| TPM | Library only. | `pkg/tpm/` |

## Goals

- No reusable credential on boot media, in URLs, or on the command line.
- A credential that only this machine can use, for minutes.
- Confidential values fetched on demand, scoped and audited.

## Non-goals

- Operating Vault or SPIRE; use the site's existing ones.

## Design options

- A: Keep bearer tokens, shorten the lifetime, bind them to the system UUID
  that the BMC reports. Small change; still a bearer token.
- B: CAPRF as a small CA. BOOTy generates a key (TPM-resident when
  available), sends a CSR with the bootstrap token, gets a client
  certificate valid for about an hour, and uses mTLS for all later calls.
  The bootstrap token becomes single-use.
- C: SPIFFE and SPIRE. A SPIRE agent in the ramdisk with TPM node
  attestation (a TPM attestor plugin, verify availability and maturity) gets
  an SVID. CAPRF, log sinks and Vault trust the SPIFFE ID.
- D: Vault with a TPM or certificate auth method; BOOTy fetches values.

Recommendation: B now (small, no new services), C where SPIRE exists, and D
for confidential values such as LUKS keys and registry credentials,
authenticated with the identity from B or C. In every case: single-use
bootstrap token, `EXPORT_VARS` off by default outside legacy mode, nothing
confidential on the command line.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `IDENTITY_MODE` | `bearer`, `cert`, `spiffe` | Default `bearer` (today). |
| `IDENTITY_CSR_URL` | URL | CAPRF certificate endpoint for `cert`. |
| `IDENTITY_KEY` | `tpm`, `memory` | Key storage. |
| `SPIFFE_AGENT_CONFIG` | path | For `spiffe`. |
| `VAULT_URL` or `CREDENTIALS_URL` | URL | Vault or CAPRF endpoint for confidential values. |
| `CREDENTIALS` | JSON list of named values to fetch | Written to tmpfs with mode 0600. |

### CAPRF and bm4x impact

- CAPRF issues single-use bootstrap tokens bound to the machine, and a CSR
  endpoint that checks the token and the system UUID (and the EK when
  attestation is on).
- Per-machine ISO URLs move to the seed model of
  [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md).
- The identity is reused by metric push, log shipping and streaming
  telemetry (mTLS client certificates).

## Security considerations

- A TPM-resident key cannot leave the host. Without a TPM, the key lives in
  RAM only.
- The bootstrap token still travels with the boot media; single use and a
  short lifetime limit the window.
- Fetched values go to tmpfs and are wiped before the handoff, except those
  the target needs, which are written with restrictive permissions.

## Testing strategy

- Unit: CSR flow with a test CA; single-use token enforcement.
- KVM: `identity-cert` with swtpm and a TPM-resident key; a replayed
  bootstrap token must fail.
- kindmetal and CAPRF e2e: mTLS to CAPRF after the exchange.
- Real hardware: TPM key creation per vendor.

## Phased rollout

1. CAPRF MR: single-use bootstrap tokens.
2. PR: `IDENTITY_MODE=cert` with an in-memory key.
3. PR: TPM-resident key.
4. PR: fetch of confidential values into tmpfs.
5. PR: `EXPORT_VARS` warning and environment hygiene.
6. PR: SPIFFE mode.

## Effort estimate

| Phase | Days |
|-------|------|
| Single-use tokens (CAPRF) | 3 to 4 |
| Certificate identity | 5 to 7 |
| TPM key | 3 to 5 |
| Value fetch | 3 to 4 |
| Environment hygiene | 1 to 2 |
| SPIFFE | 6 to 9 |
| **Total** | **21 to 31** |

## Open questions

- Is SPIRE or Vault available in the management clusters?
- Which confidential values must reach the target OS, and how (Ignition,
  cloud-init, files)?

## Related proposals

- [proposal-jwt-auth.md](proposal-jwt-auth.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)
- [proposal-8021x-macsec.md](proposal-8021x-macsec.md)
