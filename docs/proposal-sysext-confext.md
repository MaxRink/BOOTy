# systemd-sysext and confext Support

## Status

Proposal. Design only. Nothing in this document is implemented. The existing
sysext preload and active modes are implemented and described in
[sysext-provisioning.md](sysext-provisioning.md).

## Priority

P1

## Summary

BOOTy can already copy sysext `.raw` images into a preload catalog or into
`/var/lib/extensions`, with a SHA256 check per layer. This proposal adds:
validation of the image structure and `extension-release` compatibility
against the target slot, dm-verity signature checks, confext support for
`/etc` overlays, an A/B-aware catalog per slot, and a build and signing
reference pipeline. It also connects sysexts to measured boot (PCR 13).

## Problem and motivation

- A SHA256 pin proves that BOOTy copied the expected bytes. It does not prove
  that the image is a valid Discoverable Disk Image (DDI), that it matches the
  OS in the target slot, or that it carries a valid verity signature.
- A sysext built for OS version N can break a slot that runs version N+1.
  `systemd-sysext` refuses a mismatch at merge time (verify for
  `SYSEXT_LEVEL` and `VERSION_ID` rules). The failure then shows up on the
  node after provisioning, not in BOOTy.
- Sites need per-node `/etc` content (for example a kubelet drop-in or a
  site CA bundle) on a read-only `system-ab` root. confext is the systemd
  mechanism for that.
- Measured boot needs the sysext set to be measured. Today nothing measures
  it.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Config | `SYSEXT_ENABLED`, `SYSEXT_LAYERS` (JSON), `SYSEXT_DEFAULT_MODE`, `SYSEXT_CATALOG_DIR`, `SYSEXT_ACTIVE_DIR`, `SYSEXT_ALLOW_INSECURE_HTTP` | `pkg/config/sysext.go`, `docs/configuration.md` |
| Step | `apply-sysexts` after `mount-root`, `mount-boot` and the shared data mounts, before fstab and chroot steps. | `pkg/provision/orchestrator.go`, `pkg/provision/sysext.go` (`ApplySysexts`) |
| Sources | Local ramdisk path, HTTPS, `oci://` with media type `application/vnd.systemd.sysext.image.v1+raw`. | `pkg/provision/sysext.go` (`openSysextSource`) |
| Integrity | SHA256 per layer, or an OCI digest reference. Atomic write. Path checks inside the target root. | `pkg/provision/sysext.go` (`copySysextSource`, `ensureWithinRoot`) |
| Catalog | `catalog.json` with `name`, `version`, `fileName`, `path`, `sha256`. Upsert by name, version and file name. | `pkg/provision/sysext.go` (`sysextCatalog`) |
| A/B | Preload writes into the root slot that BOOTy writes. KVM scenario `ab-sysext-preload`. | `test/e2e/kvm/ab_sysext_test.go` (`TestABProvisionPreloadsSysextsWithoutActivatingVM`), `test/e2e/ab_sysext_e2e_test.go` |
| confext | Not supported. | none |
| Verity, signatures | Not checked. | none |

## Goals

- Validate a sysext image before install: GPT DDI structure, partition types,
  and the `extension-release` file.
- Check `extension-release` against `os-release` of the target slot.
- Verify dm-verity root hash signatures against a configured certificate.
- Add confext layers with the same sources and modes.
- Keep the preload catalog per slot and record compatibility data.
- Provide a reference build and sign pipeline (mkosi or `systemd-repart`).
- Measure installed sysexts for attestation.

## Non-goals

- Activating preloaded layers. The target OS keeps that role, as today.
- Running `systemd-sysext merge` inside BOOTy.
- A sysext update agent on the node. `systemd-sysupdate` covers that
  (verify that the target OSes ship it).

## Design options

### Option A: Parse DDIs in Go

Read the GPT of the `.raw` file, find the root, verity and verity signature
partitions by type GUID, read the signature JSON (`rootHash`,
`signature`), verify the PKCS#7 signature against the configured certificate,
and read `extension-release` from the root filesystem (erofs or squashfs).

- Pro: no new binary in the ramdisk. Full control of errors.
- Con: needs a read-only erofs and squashfs reader for one file. A minimal
  reader is feasible but new code.

### Option B: Use `systemd-dissect` in the ramdisk

Run `systemd-dissect --validate --json=short` and
`systemd-dissect --copy-from` to read `extension-release`.

- Pro: the same logic that systemd uses at merge time.
- Con: adds systemd binaries and libraries to the initramfs. The size budget
  is tight ([size-optimization.md](size-optimization.md)). Version skew between
  the ramdisk systemd and the target systemd.

### Option C: Validate in the build pipeline only

The pipeline signs a manifest. BOOTy checks only the manifest signature.

- Pro: smallest BOOTy change.
- Con: no check against the target slot. A wrong image still passes.

### Recommendation

Option A for structure, signature and `extension-release`. It keeps the
ramdisk small and fits the "no shell-outs" rule. Read `extension-release`
with a loop mount in the ramdisk if the kernel has erofs and squashfs. That
avoids a Go filesystem reader. Fall back to Option B only in a debug flavor.

Compatibility rule (match systemd, verify against `systemd-sysext(8)`):

- `ID` must equal the target `ID`, or be `_any`.
- If `SYSEXT_LEVEL` is set, it must equal the target `SYSEXT_LEVEL`.
  Otherwise `VERSION_ID` must equal the target `VERSION_ID`.
- `ARCHITECTURE`, if set, must match.

A/B awareness:

- The catalog stays inside the target root slot. Each slot has its own
  catalog, so a slot switch also switches the layer set.
- Add `compatible` and `extensionRelease` fields to each catalog entry.
- With `AB_PRESERVE_EXISTING=true`, check each layer against the new slot's
  `os-release`, not the running slot.
- Optional: copy shared, version-independent layers to a shared data partition
  with a per-slot symlink list. Default off.

confext:

- Same layer schema with `kind: confext`.
- Default directories: preload catalog in `/usr/lib/tcaas-confext/preloaded`,
  active mode in `/var/lib/confexts` (verify the search path list in
  `systemd-confext(8)`).
- `extension-release` lives under `etc/extension-release.d/` in a confext.

Measured boot:

- With UKI and systemd-stub, sysexts in `<uki>.extra.d/` on the ESP are
  measured into PCR 13 by the stub (verify). BOOTy can place layers there
  in a third mode `stub`.
- Without UKI, record the layer digests in the BOOTy event log that
  [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md) defines.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `SYSEXT_LAYERS[].kind` | `sysext` (default), `confext` | JSON field. |
| `SYSEXT_LAYERS[].mode` | add `stub` | Copies to the UKI `extra.d` directory on the ESP. |
| `SYSEXT_VERIFY` | `digest` (default), `structure`, `signature` | `structure` checks the DDI and `extension-release`. `signature` also checks the verity signature. |
| `SYSEXT_VERITY_CERT` | ramdisk path | PEM certificate for `signature`. |
| `SYSEXT_COMPAT_CHECK` | `true` (default with `structure`), `false` | Compare `extension-release` with the target `os-release`. |
| `CONFEXT_CATALOG_DIR` | target path | Default `/usr/lib/tcaas-confext/preloaded`. |
| `CONFEXT_ACTIVE_DIR` | target path | Default `/var/lib/confexts`. |

Keep `SYSEXT_LAYERS` as the single list. Do not add `CONFEXT_LAYERS`.

### CAPRF and CAPI API impact

- The `feat/booty-consolidated` CAPRF branch renders sysext vars. Add `kind`,
  `verify` and the certificate secret reference to its sysext section.
- Report the catalog (layer, slot, digest, compatible) back in the success
  payload, so CAPRF can show it in `RedfishMachine.status`.

## Security considerations

- A sysext overlays `/usr`. A malicious layer is root code on the node.
  `signature` mode must become the production default once images are signed.
- Keep the verity certificate in the ramdisk image or in `/deploy`. Do not
  fetch it from the same source as the layer.
- Signed verity alone does not stop a rollback to an old signed layer. Pin
  versions in the catalog and let the target OS enforce a minimum version.
- confext overlays `/etc`. It can change PAM, sudo and SSH config. Treat
  confext with the same signature policy.
- Set the target kernel `systemd.image_policy` (verify) so that the node
  itself refuses unsigned layers. BOOTy checks are a first gate, not the only
  gate.

## Testing strategy

- Unit: GPT DDI parser with fixtures built by `systemd-repart` in a test
  helper (skip when the tool is missing). Compatibility table tests. PKCS#7
  verification with a test CA. Fuzz the GPT and signature JSON parsers.
- KVM matrix: extend `ab-sysext-preload` with a signed layer and an
  incompatible layer that must fail. Add `confext-active` that boots the
  target and checks a merged `/etc` file. Add `ab-preserve-existing` with a
  layer that matches only the new slot.
- ContainerLab: none.
- kindmetal and CAPRF e2e: sysext section with `verify: signature`.
- Real hardware: one node with a signed layer and Secure Boot on.

## Phased rollout

1. PR: DDI structure check and `extension-release` reader (`structure`).
2. PR: compatibility check against the target slot. Catalog fields.
3. PR: verity signature check (`signature`).
4. PR: confext `kind` with preload and active modes.
5. PR: `stub` mode for UKI targets (depends on UKI support).
6. Docs PR: reference mkosi or `systemd-repart` build and sign pipeline.
7. CAPRF MR: new fields and status reporting.

## Effort estimate

| Phase | Days |
|-------|------|
| Structure and release check | 4 to 6 |
| Compatibility and catalog | 2 to 3 |
| Signature check | 3 to 5 |
| confext | 3 to 4 |
| Stub mode | 2 to 3 |
| Build pipeline docs | 2 to 3 |
| Tests (KVM) | 3 to 5 |
| **Total** | **19 to 29** |

## Open questions

- Which filesystems do our sysexts use (erofs or squashfs)? Does the BOOTy
  kernel have both?
- Who owns the verity signing key? Is it the same key as the UKI signing key?
- Should BOOTy refuse an incompatible layer, or warn and skip it?

## systemd-boot and Flatcar

- The `stub` mode needs a UKI target with systemd-stub (see [proposal-systemd-boot.md](proposal-systemd-boot.md)); the
  stub measures `extra.d` sysexts into PCR 13.
- Flatcar activates sysexts through `/etc/extensions` and ships OEM sysexts
  on the OEM partition; per-version layers must match the `/usr` slot (see
  [proposal-flatcar.md](proposal-flatcar.md)).

## Related proposals

- [sysext-provisioning.md](sysext-provisioning.md)
- [ab-partitioning.md](ab-partitioning.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-image-signatures.md](proposal-image-signatures.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-flatcar.md](proposal-flatcar.md)
