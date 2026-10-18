# Full Flatcar Container Linux Support

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1

## Summary

Provision Flatcar Container Linux as a first-class target: write the
official image with signature and digest checks, inject Ignition into the
OEM partition, keep Flatcar's own USR-A and USR-B update scheme intact, map
BOOTy's native network model to systemd-networkd units, preload sysexts,
support Secure Boot and TPM-based disk encryption, and support CAPI with
Ignition bootstrap data. BOOTy must not apply its own A/B scheme to Flatcar.

## Problem and motivation

- Flatcar is a common immutable Kubernetes node OS with a built-in A/B
  update system. CAPI supports it with Ignition bootstrap data.
- BOOTy has a `flatcar` OS family, but it rejects cloud-init for it and has no
  Ignition writer (`pkg/config/validate.go` `validateFlatcarCloudInit`), and
  it rejects kubelet extra args for Flatcar
  (`pkg/provision/configurator.go`).
- The only Flatcar test provisions a Flatcar-like `USR-A` source into
  BOOTy's `system-ab` layout. That is not how Flatcar boots.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| OS family | `OS_FAMILY=flatcar`. Network persistence writes systemd-networkd units to `etc/systemd/network`. | `pkg/network/persist/persist.go` (`ConfigPath`) |
| First boot | cloud-init rejected; no Ignition. | `pkg/config/validate.go` |
| Kubelet args | Rejected for Flatcar. | `pkg/provision/configurator.go` |
| A/B source rule | A source image with `USR-A` or `USR-B` fails unless `AB_SOURCE_ROOT_LABEL` is set. | `docs/ab-partitioning.md` |
| KVM scenario | `flatcar-like-usra-system-ab`: builds a synthetic disk with `EFI-SYSTEM` and `USR-A` labels, copies `USR-A` into `BOOTY-ROOT-A` of a `system-ab` layout, and checks hostname, `fstab` (`PARTLABEL=BOOTY-ROOT-A / ext4 ro`), the GRUB `root=` pin, the slot state file and a cloud-init seed on `/var`. It does not boot a real Flatcar image, Ignition, gptprio, or update_engine. | `test/e2e/kvm/ab_sysext_test.go` (`TestFlatcarLikeUSRASourceCanProvisionSystemABVM`, `createFlatcarLikeUSRATestDiskImage`) |
| kexec | Parses `/boot/grub/grub.cfg` menu entries. Flatcar's GRUB config is a script with gptprio logic. | `pkg/kexec/grub.go` |

## Flatcar background (verify each against the current Flatcar docs)

### Disk layout

| # | Label | Content |
|---|-------|---------|
| 1 | `EFI-SYSTEM` | ESP with GRUB, shim, kernels `flatcar/vmlinuz-a` and `vmlinuz-b` |
| 2 | `BIOS-BOOT` | GRUB core for legacy BIOS |
| 3 | `USR-A` | `/usr` image A, dm-verity protected |
| 4 | `USR-B` | `/usr` image B |
| 6 | `OEM` | OEM data: `grub.cfg` snippet, `config.ign`, OEM sysexts |
| 7 | `OEM-CONFIG` | Reserved |
| 9 | `ROOT` | Writable root (`/etc`, `/var`, `/home`), grown on first boot |

### A/B with GPT attributes

- `USR-A` and `USR-B` carry GPT attribute bits: priority (4 bits), tries
  (4 bits) and successful (1 bit), in the ChromeOS-style scheme.
- GRUB `gptprio.next` picks the partition with the highest priority that is
  successful or has tries left, and decrements tries.
- `update_engine` writes the new `/usr` to the inactive partition, sets a
  higher priority with tries 1, and marks it successful after a good boot.
  Nebraska (or the public update server) serves the update payloads.
- Reboot coordination: `locksmith` (older) or the Flatcar Linux Update
  Operator (FLUO) in Kubernetes; `kured` is an alternative.

### Image and verification

- `flatcar_production_image.bin.bz2` (and a `.zst` variant in newer releases,
  verify), with `.DIGESTS` and a detached GPG signature `.sig` signed by the
  Flatcar image signing key.
- `ROOT` is small in the image and is grown on first boot (verify the
  mechanism: `extend-filesystems` or Ignition).

### Provisioning config

- Ignition v3. Sources on metal: `/oem/config.ign` on the OEM partition (the
  path that `flatcar-install -i` writes, verify), or `ignition.config.url=`
  on the kernel command line.
- Platform ID `metal` (kernel argument `ignition.platform.id=metal` or the OEM
  ID, verify).
- First boot detection through a flag file on `EFI-SYSTEM` (verify name),
  set in the shipped image.
- Butane converts human YAML to Ignition JSON. This happens upstream of BOOTy
  (in CAPRF or the bootstrap provider).
- Afterburn reads metadata on cloud platforms. On metal it has little to
  read (verify); hostname and SSH keys come from Ignition.

### OEM partition

- `grub.cfg` snippet: `set linux_append="..."` and `set linux_console="..."`
  for kernel arguments and the console, and `flatcar.autologin` for debug
  consoles.
- OEM sysexts (`oem-*.raw`) for platform agents (verify the layout).

### sysext

- Flatcar ships Docker and containerd as systemd-sysext images in newer
  releases, and the community sysext bakery publishes more (Kubernetes,
  CNI, FRR candidates, verify).
- Active sysexts are symlinks in `/etc/extensions` to images under
  `/opt/extensions` or the OEM partition. `update_engine` can update
  official extensions together with the OS (verify).

## Goals

- Write the real Flatcar image with GPG and digest verification.
- Inject Ignition from CAPRF bootstrap data.
- Write kernel arguments and the console through the OEM `grub.cfg`.
- Keep `USR-A`, `USR-B` and the gptprio attributes untouched, so
  `update_engine` keeps working.
- Map BOOTy network data to networkd units through Ignition.
- Preload sysexts per OS version.
- Support Secure Boot, TPM LUKS for `ROOT`, and CAPI kubeadm with Ignition.

## Non-goals

- BOOTy A/B (`IMAGE_MODE=ab`) for Flatcar. Flatcar has its own A/B. The
  `flatcar-like-usra-system-ab` scenario stays as a test of the generic
  source rule, not as the Flatcar product path.
- Running `update_engine` updates from BOOTy.
- Butane conversion inside BOOTy.

## Design options

### Option A: BOOTy writes the image and the OEM files (recommended)

`IMAGE_MODE=whole-disk`, `OS_FAMILY=flatcar`. After the stream, BOOTy:

1. Verifies the GPG signature (`IMAGE_SIGNATURE_URL`, `IMAGE_GPG_PUBKEY`)
   and the digest.
2. Mounts `OEM` (not `ROOT`) and writes `config.ign`, the `grub.cfg` snippet
   (console, extra kernel arguments) and OEM sysexts.
3. Optionally grows `ROOT` (`GROW_PARTITION=last-linux` exists today;
   check whether Flatcar grows it on first boot anyway).
4. Skips GRUB, chroot, `fstab`, cloud-init and machine-command steps (target
   profile from [proposal-os-targets.md](proposal-os-targets.md)).
5. Creates an EFI entry for `\EFI\boot\bootx64.efi` (shim) on `EFI-SYSTEM`.
6. Reboots.

### Option B: Run `flatcar-install` in the ramdisk

- Pro: upstream tool.
- Con: a bash script with its own download and verification; needs `gpg`,
  `bzip2` and more tools; bypasses BOOTy streaming and reporting.

### Option C: Kernel argument `ignition.config.url`

- Pro: no disk write for the config.
- Con: needs network access from the first boot to CAPRF, a per-machine URL
  in the OEM `grub.cfg`, and a token in a URL. Worse for EVPN-only fabrics.

### Recommendation

Option A. Option C only as a fallback for very large configs.

## Networking

- BOOTy converts its network model (`NETWORK_DATA` from
  [proposal-capi-integration.md](proposal-capi-integration.md), or today's
  persisted config) into networkd `.network`, `.netdev` and `.link` units
  and adds them to the Ignition config as `storage.files` (merge with the
  CAPRF config through Ignition `merge`, or write them into the generated
  config before writing `config.ign`).
- BGP and EVPN on Flatcar (no FRR in the base image):
  - FRR in a container (systemd unit with Docker or containerd, host
    network), configured from the same `frr.conf` that BOOTy reads;
  - FRR as a sysext (verify bakery or build our own);
  - BOOTy's GoBGP mode as a small static binary installed as a sysext.
  - Recommendation: FRR container for parity with today's fabric config;
    evaluate the GoBGP sysext later.

## Sysext

- BOOTy preloads sysexts that match the target `VERSION_ID` and
  `SYSEXT_LEVEL` (see [proposal-sysext-confext.md](proposal-sysext-confext.md)).
- Location: OEM partition for platform agents, `/opt/extensions` on `ROOT`
  plus a symlink in `/etc/extensions` for active layers. Written as Ignition
  files or directly on `ROOT` (decide in phase 1).
- Per slot: sysexts must match the `/usr` version. After an `update_engine`
  update, mismatched sysexts are skipped by `systemd-sysext`. Use the
  official update mechanism for extensions where it exists (verify), or
  ship layers with `ID=_any` when they do not depend on the OS version.

## Updates, A/B and CAPI in-place updates

- After provisioning, Flatcar updates itself with `update_engine`. FLUO
  drains and reboots nodes one by one in Kubernetes.
- CAPI rollouts replace machines; in-place update work in CAPI (verify the
  release) could call a Flatcar update instead. CAPRF should not run BOOTy
  A/B for Flatcar.
- Version pinning: CAPRF sets the update group and server in the Ignition
  config (`/etc/flatcar/update.conf`). For full control, run a site
  Nebraska.

## kexec or reboot

- Flatcar GRUB runs `gptprio.next`, sets `mount.usr` and verity arguments
  and the OEM snippet. A kexec straight into `vmlinuz-a` would need BOOTy to
  rebuild these arguments and would skip the tries accounting.
- Ignition needs the first boot flag; kexec keeps it (it is a file), but
  BOOTy would have to replicate the GRUB argument logic.
- Recommendation: reboot. Keep kexec off for `OS_FAMILY=flatcar` until a
  test proves a correct argument set (verify the full argument list from
  the Flatcar `grub.cfg`).

## Secure Boot

- Flatcar ships a signed shim and GRUB in recent releases (verify the release
  and the signing status of kernels and the verity root hash).
- BOOTy keeps Secure Boot pins (`SECUREBOOT_PINNED_DIGESTS`) usable with
  Flatcar's `EFI-SYSTEM` paths.

## systemd-boot

- Flatcar uses GRUB with gptprio. There is no announced switch to
  systemd-boot that we know of (verify upstream discussions). The
  [systemd-boot proposal](proposal-systemd-boot.md) applies only if Flatcar
  adopts it; then boot counting would replace gptprio tries.

## Measured boot and TPM

- `ROOT` encryption: Ignition `storage.luks` with `clevis.tpm2` (verify Flatcar
  support) or `systemd-cryptenroll` on first boot.
- PCR stability: a `/usr` update changes the kernel and the GRUB-measured
  files (PCR 4, 8, 9). Bind to PCR 7 only, or use systemd-pcrlock or a Tang
  server (see [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)).
  Raw PCR 4 or 9 policies break on every update.

## CAPI

- Kubeadm bootstrap with `KubeadmConfig.spec.format: ignition` (behind the
  `KubeadmBootstrapFormatIgnition` feature gate, verify). The bootstrap data
  secret then holds Ignition JSON with a `format` key.
- CAPRF reads the format and passes the payload as `/deploy/ignition.json`.
- image-builder publishes Flatcar CAPI images (verify the raw or qcow2
  artifact names); BOOTy streams them like the official image.
- `RedfishMachineTemplate` example in the CAPRF docs with `osFamily: flatcar`.
- Provider ID: kubelet `--provider-id` set through the Ignition kubelet
  drop-in that kubeadm renders (verify) or a CAPRF patch.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `OS_FAMILY` | `flatcar` | Existing. Selects the Flatcar profile. |
| `FIRSTBOOT_FORMAT` | `ignition` | From the OS targets proposal. |
| `IGNITION_CONFIG` | ramdisk path, default `/deploy/ignition.json` | |
| `FLATCAR_OEM_KERNEL_ARGS` | string | Written to the OEM `grub.cfg` `linux_append`. `EXTRA_KERNEL_PARAMS` maps here for Flatcar. |
| `FLATCAR_OEM_CONSOLE` | string | `linux_console`. Defaults from the serial console resolver. |
| `FLATCAR_AUTOLOGIN` | boolean, default `false` | Debug only. |
| `FLATCAR_NETWORK` | `ignition`, `root`, `none` | How network units are delivered. |
| `FLATCAR_UPDATE_SERVER`, `FLATCAR_UPDATE_GROUP` | strings | Written to `/etc/flatcar/update.conf` via Ignition. |

### CAPRF and CAPI API impact

- `RedfishMachine.spec.osFamily: flatcar` and Ignition bootstrap format
  detection.
- Reject `imageMode: ab` with Flatcar.
- `RedfishMachine.status` reports the booted `/usr` partition through the
  CAPI event contract (from a small first-boot report unit, optional).

## Security considerations

- `config.ign` holds bootstrap tokens and keys. It stays on the OEM partition
  after first boot (verify whether Ignition removes it). Add a first-boot
  unit that deletes it, or use the URL source with a single-use token.
- `flatcar.autologin` gives a root console without a password. Never in
  production.
- Verify the image GPG signature with a pinned Flatcar key in the ramdisk.
- The update server decides what runs on the fleet. Use a site Nebraska with
  TLS for production.

## Testing strategy

- Unit: OEM `grub.cfg` writer, Ignition merge of network units, profile step
  skipping, GPT attribute preservation check (BOOTy must not change
  `USR-*` attributes).
- KVM matrix:
  - `flatcar-stable`: the real Flatcar stable image (pinned version, cached),
    Ignition applied, networkd unit from BOOTy, assert first boot
    completes and the hostname is set.
  - `flatcar-update-switch`: simulate an update by writing `USR-B` with a
    newer image and setting gptprio attributes in the test (`cgpt` or
    `sgdisk` attribute bits, verify), reboot, assert `USR-B` boots; break it
    and assert fallback to `USR-A`.
  - `flatcar-sysext`: a preloaded sysext is merged.
  - `flatcar-secureboot`: OVMF with Microsoft keys and Flatcar's shim.
  - `flatcar-tpm-luks`: swtpm and an Ignition LUKS `ROOT` with TPM unlock.
- ContainerLab: FRR container on Flatcar is out of scope for clab; test the
  FRR config rendering in unit tests.
- kindmetal and CAPRF e2e: one Flatcar worker in a workload cluster with
  kubeadm Ignition bootstrap.
- Real hardware: one Flatcar node per vendor.

## Phased rollout

1. PR: Flatcar target profile (skip Linux-root steps, reboot handoff).
2. PR: OEM partition writer (`config.ign`, `grub.cfg` snippet).
3. PR: GPG verification test path for the official image.
4. PR: networkd units into Ignition.
5. PR: KVM `flatcar-stable`.
6. PR: sysext preload for Flatcar.
7. CAPRF MR: Ignition bootstrap format and `osFamily`.
8. PR: KVM `flatcar-update-switch`, `flatcar-secureboot`, `flatcar-tpm-luks`.
9. CAPRF MR: kindmetal Flatcar e2e.
10. Docs PR: FRR on Flatcar guide.

## Effort estimate

| Phase | Days |
|-------|------|
| Profile and OEM writer | 4 to 6 |
| Verification path | 1 to 2 |
| Network units via Ignition | 4 to 6 |
| sysext preload | 2 to 3 |
| KVM scenarios | 8 to 12 |
| CAPRF bootstrap format and e2e | 6 to 9 |
| FRR on Flatcar | 3 to 5 |
| **Total** | **28 to 43** |

## Open questions

- Do we run our own Nebraska, or use the public update server with a pinned
  channel?
- FRR container or GoBGP sysext for the fabric on Flatcar nodes?
- Does Ignition remove `config.ign` after use on metal?
- Which Flatcar channel and minimum version do we support?

## kexec limitations

The kexec handoff skips the firmware, so boot counting, `BootNext`, measured
boot, Secure Boot verification by shim and pending firmware changes do not
apply to the kexec'd kernel. The full list, with a kexec or reboot decision
table, is in `docs/kexec.md` (PR #705).

## Related proposals

- [proposal-os-targets.md](proposal-os-targets.md)
- [proposal-sysext-confext.md](proposal-sysext-confext.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-rollback.md](proposal-rollback.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
- [proposal-network-persistence.md](proposal-network-persistence.md)
