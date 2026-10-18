# Talos Linux

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

Provision Talos Linux with BOOTy and CAPRF, and use the Talos CAPI bootstrap
(CABPT) and control plane (CACPPT) providers. BOOTy writes the Talos `metal`
raw image, makes the machine config available to Talos on first boot, and
reboots. Talos owns its own A/B boot assets and upgrades. BOOTy does not run
its A/B mode, its GRUB steps or its chroot steps for Talos.

## Problem and motivation

- Talos is an immutable, API-driven Kubernetes OS. It removes SSH and a shell
  from the node, which reduces the attack surface.
- Talos has CAPI providers: `cluster-api-bootstrap-provider-talos` (CABPT) and
  `cluster-api-control-plane-provider-talos` (CACPPT). CAPRF can serve as the
  infrastructure provider if it delivers the Talos machine config.
- Sidero Metal, the Talos bare-metal CAPI provider, is no longer developed
  (verify). Users need another bare-metal infrastructure provider.
- BOOTy assumes a mutable root with GRUB and a chroot. Talos breaks each of
  these assumptions.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Image write | `whole-disk` raw and zstd streams work for any GPT image. | `pkg/image/stream.go` |
| Root detection | `find-root-partition` and `mount-root` look for a Linux root. Talos has no such root (verify for `STATE`). | `pkg/provision/orchestrator.go` (`findRootPartition`) |
| GRUB and chroot steps | Always run in `provision`. | `configure-grub`, `setup-chroot-binds`, `run-machine-commands` |
| First boot | cloud-init only. | `pkg/cloudinit/` |
| A/B | BOOTy dual-root and system-ab schemes. | `docs/ab-partitioning.md` |
| Handoff | kexec through `grub.cfg`. | `main.go` (`tryKexec`) |

## Talos facts that shape the design (verify each for the target release)

- Image Factory builds images from a schematic: extensions, kernel args and
  overlays. It publishes `metal-<arch>.raw.zst` and an `installer` container
  image per schematic.
- The disk layout has `EFI`, `META`, `STATE` and `EPHEMERAL` partitions. Older
  releases also have `BIOS` and `BOOT`.
- Newer releases boot UKIs with systemd-boot on UEFI. Talos keeps two boot
  assets (A and B) and supports `talosctl rollback`.
- Talos reads the machine config from these sources on `metal`:
  the `talos.config=<url>` kernel argument, `talos.config=metal-iso` (a volume
  labeled `metal-iso` with `config.yaml`), or the `nocloud` platform with a
  `cidata` volume. Without a config, Talos waits in maintenance mode for
  `talosctl apply-config --insecure`.
- `META` holds small key-value data that Talos reads early, for example the
  network config (`talosctl` `--meta` and META key `0xa`).
- CABPT renders the machine config into the CAPI bootstrap data secret.

## Goals

- Provision Talos from a Talos `metal` image with a machine config from CABPT.
- Support a static network config before the machine config is fetched, for
  EVPN or non-DHCP sites.
- Keep BOOTy pre-install steps: inventory, health, firmware, wipe, RAID.
- Let Talos own upgrades and A/B.

## Non-goals

- Running the Talos installer container inside BOOTy.
- BOOTy A/B mode or sysext preload for Talos. Talos uses its own system
  extensions, built into the image.
- `talosctl` operations after the install.

## Design options

### Option A: Kernel argument with a config URL

Bake `talos.config=https://caprf.../talos/<machine>` into the schematic or the
EFI boot entry. CAPRF serves the CABPT data with a short-lived token.

- Pro: no disk-side config. Talos fetches the config itself.
- Con: the UKI kernel command line is part of the signed image. A per-machine
  URL needs a per-machine image or a systemd-boot editable cmdline, which
  breaks Secure Boot (verify). The node needs network access to CAPRF before
  config, which is hard in EVPN fabrics where BOOTy sets up the network.

### Option B: Write a config volume

BOOTy writes the raw image, then creates a small partition labeled
`metal-iso` (or `cidata` for `nocloud`) after the Talos partitions and writes
`config.yaml`. The schematic sets `talos.config=metal-iso` once for all
machines.

- Pro: no per-machine image. No network access needed for the config. Works
  with Secure Boot because the cmdline is generic.
- Con: the config sits on disk in clear text until Talos consumes it. BOOTy
  must not overlap `EPHEMERAL`, which Talos grows on first boot (verify).
  Needs a cleanup step in Talos (for example a machine config patch or a
  documented `wipe` of the partition).

### Option C: Write into `STATE`

Format and write the `STATE` partition as an installed Talos would.

- Pro: closest to an installed node.
- Con: internal format. Talos can encrypt `STATE`. Rejected.

### Option D: Maintenance mode and apply-config from CAPRF

Let Talos boot with no config. CAPRF calls `apply-config --insecure` over the
maintenance API.

- Pro: no disk-side config.
- Con: the maintenance API is unauthenticated (verify). CAPRF needs network
  reach to every node. Rejected for production.

### Recommendation

Option B with the `metal-iso` source, plus META network data for static or
EVPN networking. BOOTy skips its Linux-root steps for the `talos` profile from
[proposal-os-targets.md](proposal-os-targets.md). The handoff is a reboot.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `OS_FAMILY` | `talos` | Selects the Talos profile. |
| `TALOS_CONFIG` | ramdisk path, default `/deploy/talos/config.yaml` | Machine config from CABPT, rendered by CAPRF. |
| `TALOS_CONFIG_SOURCE` | `metal-iso`, `nocloud` | Partition label and layout. Must match the schematic. |
| `TALOS_META_NETWORK` | ramdisk path | Optional network config for META key `0xa` (verify the key). |

### CAPRF and CAPI API impact

- CAPRF reads the bootstrap data secret of the CAPI Machine. For CABPT data,
  it writes `/deploy/talos/config.yaml` instead of cloud-init. It detects the
  type from the secret `format` field or a new `RedfishMachine.spec.osFamily`.
- CAPI matches Nodes by `spec.providerID`. Talos sets the providerID through
  `machine.kubelet.extraArgs` or a config patch (verify). CAPRF or CABPT must
  set `redfish://...` in the patch. This is the main integration risk.
- CACPPT replaces control plane machines on upgrade. CAPRF handles it as a
  normal delete and create. No in-place A/B upgrade from CAPRF.
- The `ab` and `imageMode` fields from the `feat/booty-consolidated` CAPRF
  branch must be rejected for `talos`.

## Security considerations

- The machine config holds the cluster CA keys and join tokens. The config
  partition must exist only until first boot. Add a documented Talos-side
  cleanup or wipe it through a later BOOTy run. Verify whether Talos removes
  the `metal-iso` source after it applies the config.
- Use Talos disk encryption with TPM for `STATE` and `EPHEMERAL` (verify the
  current Talos TPM support).
- Secure Boot: use Image Factory Secure Boot images with Sidero keys or
  site keys. Enrol the keys with [proposal-uefi-management.md](proposal-uefi-management.md).

## Testing strategy

- Unit: Talos profile step skipping. Config partition placement on fixture
  GPTs. Rejection of A/B, sysext and cloud-init keys.
- KVM matrix: `talos-metal` scenario. Stream a pinned Talos raw image, add the
  config volume, reboot, and assert that the Talos API answers on the node
  (or that the serial console shows the config load).
- ContainerLab: none.
- kindmetal and CAPRF e2e: one Talos workload cluster with CABPT and CACPPT on
  kindmetal VMs. Assert Node providerID matching.
- Real hardware: one Talos node per vendor, with Secure Boot on.

## Phased rollout

1. PR: Talos profile that skips the Linux-root steps (depends on the profile
   work in [proposal-os-targets.md](proposal-os-targets.md)).
2. PR: config volume writer for `metal-iso`.
3. PR: META network writer.
4. PR: KVM scenario.
5. CAPRF MR: CABPT bootstrap data detection and providerID patch.
6. CAPRF MR: kindmetal Talos e2e.

## Effort estimate

| Phase | Days |
|-------|------|
| Profile and step skipping | 2 to 3 |
| Config volume and META writers | 4 to 6 |
| KVM scenario | 3 to 4 |
| CAPRF bootstrap and providerID | 4 to 6 |
| kindmetal e2e | 4 to 6 |
| **Total** | **17 to 25** |

## Open questions

- Does Talos accept `metal-iso` from a disk partition, or only from a CD
  device? Test this first; it decides between Option B and a CAPRF-served URL.
- Who maintains the Image Factory schematic and its Secure Boot keys?
- Do we need EVPN in Talos itself, or only during provisioning? Talos does not
  run FRR by default.

## systemd-boot

- Newer Talos releases boot UKIs with systemd-boot on UEFI and keep GRUB for
  BIOS and upgraded older installs (verify per release). Talos owns its boot
  entries; BOOTy only writes the image and the config volume, then reboots.
  BOOTy must not apply `BOOTLOADER` settings from [proposal-systemd-boot.md](proposal-systemd-boot.md) to Talos.

## Related proposals

- [proposal-os-targets.md](proposal-os-targets.md)
- [proposal-capi-integration.md](proposal-capi-integration.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
