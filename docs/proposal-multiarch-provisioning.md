# Multi-Architecture Provisioning (arm64 Delta)

## Status

Proposal. Design only. Nothing in this document is implemented.
[proposal-arm64-support.md](proposal-arm64-support.md) has Phase 1
implemented (builds). This document lists the remaining provisioning gaps
that the other proposals in this set touch.

## Priority

P3

## Summary

BOOTy builds `linux/arm64` binaries and initramfs flavors, and the GRUB and
EFI fallback code selects `BOOTAA64.EFI` and `arm64-efi` by `runtime.GOARCH`.
Gaps remain: the Secure Boot chain verifier looks only for `BOOTX64.EFI`,
no KVM scenario boots arm64, image selection is not architecture-aware, and
CAPRF does not use `RedfishHost.spec.architecture` to pick artifacts.

## Problem and motivation

- arm64 servers (Ampere, NVIDIA Grace, verify the fleet plans) need the same
  provisioning path.
- A wrong-architecture image installs and then fails to boot. BOOTy should
  refuse it before it writes.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Builds | amd64 and arm64 binaries and flavors; release publishes both. | `README.md`, `Makefile` |
| EFI paths | `efiRemovableLoaderName` and `grubEFITarget` per `runtime.GOARCH`. | `pkg/provision/configurator.go` |
| Secure Boot verify | Fallback path list has `/boot/efi/EFI/BOOT/BOOTX64.EFI` only. | `pkg/secureboot/verify.go` |
| KVM | No arm64 scenario. | `.github/workflows/kvm-matrix.yml` |
| CAPRF | `RedfishHost.spec.architecture` exists. | CAPRF `api/v1alpha1/redfishhost_types.go` |

## Goals

- Architecture-correct paths everywhere.
- Fail fast on an image for another architecture.
- One arm64 KVM scenario in nightly.
- CAPRF picks the BOOTy artifact and the OS image by host architecture.

## Non-goals

- 32-bit or RISC-V targets.

## Design options

- Image architecture check:
  - A: read the ELF header of the target kernel after the write. Late, but
    simple.
  - B: read the OCI image config `architecture` or a manifest list platform
    before the write.
  - C: read the PE machine type of the image's EFI loader from the ESP in the
    stream prefix (A/B and partition modes parse the GPT already).
  - Recommendation: B for OCI, C for raw images where the ESP is in the
    prefix, and A as a final check before the handoff.
- Secure Boot verifier: derive the loader names from the same helper as the
  configurator. `validatePEMachineType` already checks the machine type.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `IMAGE_ARCH_CHECK` | `off`, `warn`, `fail` (default `fail` once stable) | |

### CAPRF and CAPI API impact

- CAPRF selects the BOOTy ISO or UKI by `spec.architecture`.
- `imageSources` may carry an architecture per source, or CAPRF resolves an
  OCI index by platform.

## Security considerations

- None new. The check prevents a class of failed boots.

## Testing strategy

- Unit: PE and ELF machine type detection with fixtures. Verifier paths per
  architecture.
- KVM matrix: `arm64-smoke` with `qemu-system-aarch64` and AAVMF under TCG in
  nightly (slow, verify runtime), or on an arm64 runner with KVM if
  available.
- ContainerLab: GoBGP and FRR images for arm64 (verify FRR image
  availability).
- kindmetal and CAPRF e2e: none until arm64 runners exist.
- Real hardware: one arm64 server.

## Phased rollout

1. PR: Secure Boot verifier paths per architecture.
2. PR: image architecture check (OCI, then raw).
3. PR: nightly `arm64-smoke`.
4. CAPRF MR: artifact selection by architecture.

## Effort estimate

| Phase | Days |
|-------|------|
| Verifier paths | 1 |
| Architecture check | 3 to 4 |
| arm64 KVM nightly | 3 to 5 |
| CAPRF selection | 2 to 3 |
| **Total** | **9 to 13** |

## Open questions

- Which arm64 platforms are planned, and with which firmware (UEFI and ACPI,
  or device tree)?
- Do arm64 CI runners with KVM exist?

## systemd-boot

- arm64 uses `systemd-bootaa64.efi`, `BOOTAA64.EFI` and arm64 UKIs; the
  root partition type GUID differs per architecture in the Discoverable
  Partitions Specification. The loader name helper must cover systemd-boot
  too (see [proposal-systemd-boot.md](proposal-systemd-boot.md)).

## Related proposals

- [proposal-arm64-support.md](proposal-arm64-support.md)
- [proposal-os-targets.md](proposal-os-targets.md)
- [proposal-oci-advanced.md](proposal-oci-advanced.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
