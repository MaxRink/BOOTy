# More OS Targets

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1

## Summary

BOOTy provisions Linux images that boot with GRUB and that take a cloud-init
seed. This proposal defines an OS target matrix: OS family, image format,
bootloader and first-boot configuration. It adds an explicit target profile
that tells BOOTy how to write, configure and hand off each family. Flatcar,
Fedora CoreOS, Ubuntu, RHEL-likes, SLES, Debian, NixOS and Talos are in scope.
Windows and ESXi have their own proposals.

## Problem and motivation

- Operators want to run more than Ubuntu-like images on the same fleet.
  Flatcar, Fedora CoreOS and Talos are common Kubernetes node OSes.
- Each family has a different first-boot system. Flatcar and Fedora CoreOS
  use Ignition. Talos uses its own machine config. SLE Micro uses Combustion
  or Ignition. BOOTy only writes cloud-init seeds today.
- RHEL 9 and Fedora use Boot Loader Specification (BLS) entries. The GRUB
  `grub.cfg` then holds a `blscfg` call and no `menuentry` blocks. The kexec
  handoff parses `grub.cfg` and does not read BLS entries.
- Images come as raw, compressed raw, qcow2, OCI layers or root filesystem
  tarballs. BOOTy accepts raw, compressed raw, qcow2 (through a tmpfs copy)
  and single-layer OCI. It rejects VMDK, OVA and OVF. It has no tarball path.
- `OS_FAMILY` accepts only `ubuntu`, `rhel` and `flatcar`. `rhel` rejects
  `PERSIST_NETWORK`. The family value does not select a write or handoff
  strategy.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Target OS class | `TARGET_OS` must be `linux`. Other values fail in `validate-provision-inputs`. | `pkg/config/provision.go`, `pkg/config/validate.go` (`ValidateProvisionTargetOS`) |
| OS family | `OS_FAMILY` is `ubuntu`, `rhel` or `flatcar`. Used by network persistence, overlayfs and the cloud-init guard. | `pkg/network/persist/persist.go` (`ParseOSFamily`), `docs/configuration.md` |
| Image formats | raw, gzip, zstd, lz4, xz, bzip2, qcow2. VMDK, OVA and OVF are detected and rejected. | `pkg/image/format.go`, `pkg/image/qcow2.go` |
| OCI | First matching layer of a single image. No index, no multi-layer extraction. | `pkg/image/oci.go` (`selectDefaultOCILayer`) |
| Write modes | `whole-disk`, `partition` (`PARTITION_LAYOUT`), `ab` | `pkg/provision/orchestrator.go` (`streamImage`, `streamABImage`, `streamPartitionLayoutRootImage`) |
| Flatcar-like images | A/B source selection fails on `USR-A` and `USR-B` without an explicit selector. KVM scenario `flatcar-like-usra-system-ab`. | `docs/ab-partitioning.md`, `.github/workflows/kvm-matrix.yml` |
| First boot | NoCloud and ConfigDrive seeds. Rejected with `OS_FAMILY=flatcar`. | `pkg/cloudinit/`, `pkg/config/validate.go` |
| Bootloader | GRUB drop-in, `update-grub`, EFI fallback loader, EFI boot entry. | `configure-grub`, `install-efi-fallback`, `create-efi-boot-entry` steps |
| Handoff | kexec parses `/boot/grub/grub.cfg` (`ParseGrubCfg`). No BLS, no systemd-boot, no UKI. | `pkg/kexec/grub.go`, `main.go` (`tryKexec`) |
| Architecture | amd64 and arm64 initramfs flavors are built. The provisioning flow assumes x86 GRUB paths in places. | `README.md`, `docs/proposal-arm64-support.md` |

## Goals

- Define one target matrix and test each supported cell in CI.
- Add a target profile that selects the write strategy, the first-boot
  writer, the bootloader handling and the handoff for each family.
- Add Ignition injection for Flatcar and Fedora CoreOS.
- Make kexec work for BLS-based images (RHEL-likes, Fedora, SLES 16, verify).
- Add a root filesystem tarball path for distributions that ship no disk
  image.
- Fail fast and clearly for unsupported cells.

## Non-goals

- Windows and ESXi. See [proposal-windows-deployment.md](proposal-windows-deployment.md)
  and [proposal-esxi-deployment.md](proposal-esxi-deployment.md).
- Talos details. See [proposal-talos.md](proposal-talos.md). This proposal
  only adds Talos to the matrix.
- Building the OS images. BOOTy consumes images and does not build them.
- Legacy BIOS boot on new families. BOOTy keeps UEFI as the primary path.

## Target matrix

The table shows the recommended cell for each family. "Native" means the
format that the vendor publishes for bare metal or generic clouds. Verify
each format against the current vendor release before you implement it.

| Family | Native image format | Root layout | Bootloader | First-boot config | BOOTy write mode | Handoff |
|--------|---------------------|-------------|------------|-------------------|------------------|---------|
| Ubuntu | raw `.img` (qcow2 on cloud-images, verify) | single root | GRUB + shim | cloud-init NoCloud | `whole-disk`, `partition`, `ab` | kexec (grub.cfg) |
| Debian | `genericcloud` raw and qcow2 (verify) | single root | GRUB + shim | cloud-init NoCloud | `whole-disk`, `partition`, `ab` | kexec (grub.cfg) |
| RHEL, Rocky, Alma, CentOS Stream | qcow2 KVM guest image | single root, often LVM or xfs | GRUB + BLS entries | cloud-init NoCloud or ConfigDrive | `whole-disk` | kexec after BLS support, else reboot |
| SLES 15 and 16 | qcow2 or raw minimal VM image (verify) | btrfs root with snapshots | GRUB (BLS in SLES 16, verify) | cloud-init or Combustion and Ignition (SLE Micro) | `whole-disk` | reboot first, kexec later |
| Fedora CoreOS | raw `metal` image (`.raw.xz`) | ostree, `boot`, `root` | GRUB + BLS | Ignition (`/boot/ignition/config.ign`, verify) | `whole-disk` | reboot (Ignition needs first boot) |
| Flatcar | `flatcar_production_image.bin.bz2` | `USR-A`, `USR-B`, `ROOT`, `OEM` | GRUB (Flatcar build) | Ignition (`config.ign` on `OEM`, verify) | `whole-disk` | reboot |
| NixOS | raw EFI image from `nixos-generators` or `nixos-rebuild build-image` (verify) | single root | systemd-boot or GRUB | baked into the image, optional cloud-init | `whole-disk` | reboot, kexec only with GRUB |
| Talos | raw `metal` image from Image Factory (`.raw.zst`, verify) | `EFI`, `META`, `STATE`, `EPHEMERAL` (verify) | systemd-boot UKI (newer releases, verify) | Talos machine config | `whole-disk` | reboot |

Format axis:

| Format | Status | Proposal |
|--------|--------|----------|
| raw, compressed raw | Supported | Keep |
| qcow2 | Supported through tmpfs and `qemu-img` | Stream qcow2 without a full tmpfs copy when RAM is small (see [proposal-image-delivery.md](proposal-image-delivery.md)) |
| OCI single layer | Supported | Keep. Index and multi-layer are in [proposal-oci-advanced.md](proposal-oci-advanced.md) |
| Root filesystem tarball (`.tar.zst`) | Not supported | New `IMAGE_MODE=rootfs`: partition, `mkfs`, extract, install bootloader |
| VMDK, OVA, OVF | Rejected | Keep rejected. Convert in the image pipeline. |
| VHDX, WIM | Rejected implicitly | See the Windows proposal |

## Design options

### Option A: Keep one generic Linux path and add flags

Add more `OS_FAMILY` values and more boolean flags (`IGNITION_ENABLED`,
`BLS_KEXEC`). Each step checks the flags.

- Pro: small first PRs.
- Con: the flag set grows per family. Invalid combinations multiply. Steps such
  as `configure-grub` and `run-machine-commands` assume a writable,
  chroot-capable root. That is false for Flatcar `USR-*`, Fedora CoreOS ostree
  and Talos.

### Option B: Target profiles

Add a `TargetProfile` record per family. Each profile declares:

- `writeModes`: allowed `IMAGE_MODE` values.
- `firstBoot`: `cloud-init`, `ignition`, `talos`, `none`.
- `bootloader`: `grub`, `grub-bls`, `systemd-boot`, `uki`, `opaque`.
- `handoff`: `kexec-grub`, `kexec-bls`, `reboot`.
- `mutableRoot`: whether chroot steps can run.
- `skipSteps`: steps that do not apply (for example `configure-grub`,
  `grow-partition`, `run-machine-commands` on an immutable family).

`OS_FAMILY` selects the profile. The orchestrator asks the profile before each
OS-touching step. Validation rejects keys that the profile does not support,
before any disk write.

- Pro: one place per family. Clear errors. Testable as a table.
- Con: a refactor of step guards. Must keep `TestProvisionStepCount` stable or
  update it on purpose.

### Option C: Delegate to vendor installers

Run `coreos-installer`, `flatcar-install` or `talosctl` inside the ramdisk.

- Pro: the vendor owns the layout and the first-boot embedding.
- Con: more binaries in the initramfs. Each tool brings its own download,
  checksum and retry logic, which bypasses BOOTy's streaming, checksum and
  failed-write cleanup. Some tools need network access to vendor endpoints.

### Recommendation

Option B. Keep BOOTy as the single writer. Implement the small first-boot
writers in Go:

- Ignition: write the config file to the location that the family reads on
  first boot. Flatcar reads `/oem/config.ign` on the `OEM` partition (verify
  for the current release). Fedora CoreOS reads `/boot/ignition/config.ign`
  (verify; `coreos-installer --ignition-file` writes there). Add the
  `ignition.firstboot` and `ignition.platform.id=metal` kernel arguments only
  when the family needs them (verify).
- BLS: add a parser for `/boot/loader/entries/*.conf` next to `ParseGrubCfg`.
  Select the entry from `saved_entry` in `grubenv` or the highest version.
- Rootfs tarball: a new write mode that reuses `PARTITION_LAYOUT`, extracts the
  tarball into the root and installs the bootloader through a chroot command
  list supplied by the profile.

Use Option C only for Talos if the image write path cannot embed the config
(see the Talos proposal).

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `OS_FAMILY` | add `debian`, `fedora-coreos`, `sles`, `nixos`, `talos`; keep `ubuntu`, `rhel`, `flatcar` | Selects the target profile. Required for provisioning once profiles exist; the default stays the current generic path during the migration. |
| `FIRSTBOOT_FORMAT` | `cloud-init`, `ignition`, `talos`, `none` | Default from the profile. |
| `IGNITION_CONFIG` | path in the ramdisk, default `/deploy/ignition.json` | CAPRF writes the rendered config there. |
| `IMAGE_MODE` | add `rootfs` | Tarball extraction into `PARTITION_LAYOUT`. |
| `BOOTLOADER` | `auto`, `grub`, `grub-bls`, `systemd-boot`, `none` | Default `auto` from the profile. Ties into [proposal-bootloaders.md](proposal-bootloaders.md). |
| `HANDOFF` | `auto`, `kexec`, `reboot` | Replaces the implicit decision in `tryKexec` with a profile default. `DISABLE_KEXEC` stays as an override. |

`docs/configuration.md` must list every new key in a table row.
`TestConfigurationDocListsEveryVarsKey` enforces this.

### CAPRF and CAPI API impact

- `RedfishMachine.spec` needs an OS family and a bootstrap data format. Today
  CAPRF renders cloud-init from the CAPI bootstrap secret. A Kubeadm bootstrap
  provider can emit Ignition (`KubeadmConfig.spec.format: ignition`, verify).
  CAPRF must read the `format` key of the bootstrap secret and pass
  `FIRSTBOOT_FORMAT` and the payload file.
- The `feat/booty-consolidated` CAPRF branch adds `imageMode` and `ab` to
  `RedfishMachineSpec`. Add `osFamily` beside them.
- No CAPI core change.

## Security considerations

- Ignition configs and cloud-init user data carry bootstrap tokens. BOOTy must
  write them with mode `0600` and must not log their content. Reuse the
  redaction in `pkg/provision/redaction.go`.
- Ignition can fetch remote resources on first boot. Document that operators
  should pin them with `verification.hash` (Ignition spec, verify).
- A tarball extraction is a path traversal risk. Use `os.Root` (Go 1.24+) as
  `pkg/espsync` already does, and reject absolute and `..` entries.
- BLS parsing reads files from the target root. Treat them as untrusted input.
  Bound the file count and size.

## Testing strategy

- Unit: profile table tests (each family, each rejected key). BLS parser with
  fixtures from Fedora, RHEL and SLES. Ignition writer with a fake `OEM`
  partition. Tarball extraction with path traversal cases. Add Go fuzz tests
  for the BLS parser (see [proposal-testing-strategy.md](proposal-testing-strategy.md)).
- KVM matrix: one scenario per family with a small public image cached in CI:
  `os-debian`, `os-rocky-bls`, `os-fcos-ignition`, `os-flatcar-ignition`,
  `os-nixos`. Each boots the result to a marker unit or a serial console
  string. Keep the large images in a weekly job, not per PR.
- ContainerLab: no change. Network is OS-independent in the ramdisk. Add one
  check that persisted network config works for each new family with
  `PERSIST_NETWORK`.
- kindmetal and CAPRF e2e: one Flatcar or Fedora CoreOS machine through CAPRF
  with a Kubeadm Ignition bootstrap.
- Real hardware: one node per vendor (Dell, HPE, Lenovo, Supermicro) per new
  family before the family is marked supported.

## Phased rollout

1. PR: `TargetProfile` table with the three current families. No behaviour
   change. Tests prove equivalence.
2. PR: BLS entry parser and `kexec-bls` handoff. KVM scenario with Rocky Linux.
3. PR: `debian` profile. KVM scenario.
4. PR: Ignition writer and `flatcar` first-boot. KVM scenario.
5. PR: `fedora-coreos` profile. KVM scenario.
6. PR: `IMAGE_MODE=rootfs`. KVM scenario with a Debian tarball.
7. PR: `sles` and `nixos` profiles. Weekly KVM scenarios.
8. CAPRF MR: `osFamily` and bootstrap format plumbing.
9. Docs PR: support matrix page with the tested cells.

## Effort estimate

| Phase | Days |
|-------|------|
| Profiles refactor | 4 to 6 |
| BLS kexec | 3 to 4 |
| Ignition writer (Flatcar, Fedora CoreOS) | 4 to 6 |
| Rootfs tarball mode | 5 to 7 |
| SLES, NixOS, Debian profiles | 4 to 6 |
| KVM scenarios and image cache | 5 to 8 |
| CAPRF plumbing | 3 to 5 |
| **Total** | **28 to 42** |

## Open questions

- Which families does the product support, and which are best effort?
- Does CAPRF keep cloud-init rendering, or does it pass the raw CAPI bootstrap
  data and let BOOTy convert it?
- Can CI cache vendor images legally and within storage limits? Some vendor
  images (RHEL, SLES) need a subscription.
- Should the support matrix live in `docs/` as a generated table from the
  profile code?

## systemd-boot

- The matrix column "Bootloader" maps to `BOOTLOADER=auto|grub|systemd-boot|uki`
  from [proposal-systemd-boot.md](proposal-systemd-boot.md). Families with systemd-boot today or as an option: NixOS,
  Debian, Ubuntu, Fedora (optional), Talos (UKI, verify). RHEL-likes and
  Flatcar use GRUB.
- The BLS parser of this proposal is shared with systemd-boot Type #1
  entries. UKI targets need the UKI extraction or a reboot handoff.

## Flatcar

The Flatcar row of the matrix is detailed in [proposal-flatcar.md](proposal-flatcar.md): real image with GPG
verification, Ignition on the OEM partition, gptprio A/B owned by Flatcar,
and reboot handoff. BOOTy A/B mode is not used for Flatcar.

## Related proposals

- [proposal-bootloaders.md](proposal-bootloaders.md)
- [proposal-cloudinit-management.md](proposal-cloudinit-management.md)
- [proposal-network-persistence.md](proposal-network-persistence.md)
- [proposal-oci-advanced.md](proposal-oci-advanced.md)
- [proposal-arm64-support.md](proposal-arm64-support.md)
- [proposal-talos.md](proposal-talos.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-flatcar.md](proposal-flatcar.md)
