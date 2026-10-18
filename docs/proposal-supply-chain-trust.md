# Supply-Chain Trust for BOOTy

## Status

Proposal. Design only. Nothing in this document is implemented. It extends
[proposal-release-pipelines.md](proposal-release-pipelines.md) and
[proposal-image-signatures.md](proposal-image-signatures.md).

## Priority

P1

## Summary

Make every BOOTy artifact verifiable from source to boot: keyless Sigstore
signatures on all artifacts, SLSA provenance, reproducible initramfs builds,
an SBOM of the ramdisk content (not only the Go module graph), and cosign
verification of OCI-packaged OS images and sysexts inside BOOTy.

## Problem and motivation

- CAPRF downloads a BOOTy ISO or initramfs and boots it with root access to
  the hardware. A tampered artifact owns the fleet.
- BOOTy verifies OS images by checksum and GPG; OCI sources by digest. It
  does not verify Sigstore signatures (`IMAGE_SIGNATURE_URL` is GPG only and
  "not supported for `oci://` sources", `docs/configuration.md`).

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Release signing | `cosign sign-blob` over checksum files; container signing job; buildx `--provenance=true`. | `.github/workflows/release-v2.yml` (`sign`, `container-sign`) |
| SBOM | `anchore/sbom-action` SPDX for the release. | `.github/workflows/release-v2.yml` (`sbom`) |
| Image checks | Checksum, GPG for HTTP, OCI digest pins. | `pkg/image/verify/`, `pkg/image/gpg.go`, `pkg/image/oci.go` |
| Initramfs build | Multi-stage Dockerfile from Debian and Alpine bases. | `initrd.Dockerfile` |

## Goals

- Sigstore signatures and SLSA provenance (level 3 target, verify the
  builder requirements) for the ISO, initramfs, kernel, binary and OCI
  artifacts.
- Reproducible initramfs: same inputs give the same bytes.
- SBOM of every file in the initramfs, including Debian packages and static
  binaries.
- cosign verification in BOOTy for OCI images and sysexts.
- CAPRF verifies BOOTy artifacts before it builds or serves an ISO.

## Non-goals

- Signing OS images; the image pipelines own that.

## Design options and recommendation

- Signing: keyless cosign with GitHub OIDC (already partly used) for public
  artifacts; a KMS key for air-gapped verification. Recommendation: both;
  keyless primary, KMS-signed bundle for offline sites.
- Provenance: `slsa-github-generator` or GitHub artifact attestations
  (verify current tooling). Attach to OCI artifacts as referrers.
- Reproducibility: pin base images by digest, pin Debian snapshot mirrors
  (`snapshot.debian.org`, verify), set `SOURCE_DATE_EPOCH`, sort cpio
  entries, zero timestamps, deterministic compression; a CI job builds twice
  and compares.
- Ramdisk SBOM: generate from the Debian package database of the build
  stages plus Go build info of each binary (syft on the unpacked initramfs,
  verify coverage).
- In-BOOTy verification: use `sigstore-go` (verify size) to verify a cosign
  bundle offline against a trusted root in the ramdisk; policy keys per
  source (identity and issuer for keyless, public key otherwise).

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `IMAGE_COSIGN_POLICY` | JSON or path | Identities, issuers, keys; enables cosign checks for `oci://` images. |
| `SYSEXT_COSIGN_POLICY` | same | For sysext OCI sources. |
| `SIGSTORE_TRUSTED_ROOT` | path | Offline trusted root. |
| `IMAGE_VERIFY_MODE` | `digest`, `signature`, `both` | |

### CAPRF and bm4x impact

- CAPRF verifies the BOOTy artifact signature and provenance before it
  serves it; refuses unsigned artifacts unless a dev flag is set.
- `RedfishMachine.spec` gains a verification policy reference.

## Security considerations

- The trusted root and policies inside the ramdisk must themselves be signed
  as part of the artifact.
- Keyless verification needs a trusted timestamp; offline sites need a TSA
  or a key-based fallback.
- SBOMs reveal versions; publish them only where the artifact is published.

## Testing strategy

- Unit: cosign bundle verification with test fixtures; policy matching.
- CI: reproducibility job (build twice, compare hashes); provenance
  verification job with `slsa-verifier` (verify).
- KVM: `oci-cosign` scenario with a local registry and a signed image; a
  tampered image must fail.
- kindmetal and CAPRF e2e: CAPRF refuses an unsigned BOOTy artifact.

## Phased rollout

1. PR: sign every release artifact and attach provenance (complete the
   existing jobs).
2. PR: ramdisk SBOM from the unpacked initramfs.
3. PR: reproducibility fixes and the double-build job.
4. PR: cosign verification in BOOTy for OCI images.
5. PR: the same for sysexts.
6. CAPRF MR: artifact verification.

## Effort estimate

| Phase | Days |
|-------|------|
| Signing and provenance completion | 3 to 4 |
| Ramdisk SBOM | 2 to 3 |
| Reproducible builds | 6 to 10 |
| cosign in BOOTy (images, sysexts) | 6 to 9 |
| CAPRF verification | 4 to 6 |
| **Total** | **21 to 32** |

## Open questions

- Do air-gapped sites accept keyless signatures with an offline trusted root?
- Is SLSA level 3 required by security policy?

## Related proposals

- [proposal-release-pipelines.md](proposal-release-pipelines.md)
- [proposal-image-signatures.md](proposal-image-signatures.md)
- [proposal-oci-advanced.md](proposal-oci-advanced.md)
- [proposal-sysext-confext.md](proposal-sysext-confext.md)
