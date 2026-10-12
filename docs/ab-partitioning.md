# A/B OS Provisioning

BOOTy supports an optional A/B image mode for dual-root OS provisioning and
upgrades. The `whole-disk` and `partition` image modes do not change.

This page describes A/B mode only. [docs/disk.md](disk.md) describes target
disk selection, wipe behaviour and the other disk steps.
[docs/configuration.md](configuration.md) lists the `AB_*` variables with their
defaults.

## Layout

Set `IMAGE_MODE=ab` (`provision.image.mode: ab`). BOOTy generates the GPT
layout from the A/B settings and ignores `PARTITION_LAYOUT`.

The default `dual-root` scheme creates this layout:

| Partition | Label | Mountpoint | Purpose |
| --- | --- | --- | --- |
| 1 | `BOOTY-EFI` | `/boot/efi` | Shared EFI system partition (vfat), `bootSizeMB`, default 512 MiB |
| 2 | `BOOTY-ROOT-A` | `/` when slot A is the target | Root slot A (ext4), `rootSizeMB`, default 32768 MiB |
| 3 | `BOOTY-ROOT-B` | `/` when slot B is the target | Root slot B (ext4), `rootSizeMB`, default 32768 MiB |
| 4 | `BOOTY-STATE` | `/var/lib/booty` | Persistent BOOTy state (ext4), `stateSizeMB`, `0` fills the remaining disk |

The `system-ab` scheme uses the same EFI and root slots. It replaces
`BOOTY-STATE` with shared data partitions. By default it creates one ext4
partition:

| Partition | Label | Mountpoint | Purpose |
| --- | --- | --- | --- |
| 4 | `BOOTY-DATA` | `/var` | Shared runtime data across root slot upgrades |

`system-ab` root slots are read-only. BOOTy writes `ro` as the mount option of
the target root in `/etc/fstab` and adds `ro` to the GRUB kernel arguments.
Keep mutable state on shared data partitions such as `/var` or `/home`.

`dataPartitions` (`AB_DATA_PARTITIONS`) replaces the default data partition
list. Each entry needs a `label` and an absolute `mountpoint`. The mountpoint
must not be `/` or `/boot/efi`. The `filesystem` defaults to `ext4` and must not
be `vfat` or `swap`. `sizeMB: 0` fills the remaining disk and only the last
entry can use it. `dataPartitions` requires `scheme: system-ab`. Without
`dataPartitions`, `stateSizeMB` (`AB_STATE_SIZE_MB`) sets the size of the
default `/var` partition.

## Target Slot

`targetSlot` is `a`, `b` or `inactive` (default). `inactive` selects the slot
that is not `activeSlot`. Without `activeSlot`, `inactive` selects slot `a`.

Initial provisioning wipes the target disk and creates the layout. Upgrades set
`preserveExisting: true` and `activeSlot`, and normally use
`targetSlot: inactive`. BOOTy then rewrites only the inactive root slot. The
active slot stays available for rollback.

## Source Root Selection

BOOTy copies one root partition from the source image into the target root
slot. It selects the source partition with the first rule that applies:

1. `sourceRootPartition` (`AB_SOURCE_ROOT_PARTITION`): the 1-based partition
   number. It must not be the EFI partition.
2. `sourceRootLabel` (`AB_SOURCE_ROOT_LABEL`): the partition with this GPT
   name. The match ignores case. Exactly one partition must match.
3. Flatcar-like images with a `USR-A` or `USR-B` partition fail. Set an
   explicit selector, for example `AB_SOURCE_ROOT_LABEL="USR-A"`.
4. Exactly one partition with a common root label: `root`, `root-a`,
   `root-b`, `rootfs`, `cloudimg-rootfs`, `booty-root-a`, `booty-root-b`,
   `ubuntu-root`, `debian-root`, `fedora-root`, `opensuse-root`,
   `opensuse-rootfs`, `flatcar-root` or `bottlerocket-root`. The match ignores
   case and treats `_` as `-`.
5. Exactly one partition with the Linux filesystem type.

If more than one partition matches a rule, BOOTy fails and does not guess.
`sourceRootLabel` and `sourceRootPartition` are mutually exclusive. If the
source image has no partition table (a plain root filesystem image), BOOTy
copies the image directly into the target root slot.

`PARTITION_LAYOUT` root streaming uses the same rules with
`IMAGE_SOURCE_ROOT_LABEL` and `IMAGE_SOURCE_ROOT_PARTITION`.

During initial provisioning, BOOTy also copies the first EFI system partition of
the source image into `BOOTY-EFI`, if the image has one. During
`preserveExisting` upgrades, BOOTy does not change `BOOTY-EFI`, so the active
slot keeps its boot files.

## Boot Configuration

After BOOTy mounts the target root, it writes `/etc/booty/ab-slot.env` with
these keys: `BOOTY_AB_SCHEME`, `BOOTY_AB_TARGET_SLOT`, `BOOTY_AB_BOOTED_SLOT`,
`BOOTY_AB_ACTIVE_SLOT`, `BOOTY_AB_PRESERVE_EXISTING` and
`BOOTY_AB_ROOT_PARTITION`. Other tools can read this file after boot to find
the active slot.

BOOTy writes `root=PARTLABEL=BOOTY-ROOT-A` or `root=PARTLABEL=BOOTY-ROOT-B` into
the GRUB defaults of the target root. Kexec and firmware boots then use the
same root slot.

A/B mode does not create an EFI NVRAM boot entry. BOOTy installs the removable
EFI fallback loader instead. With `preserveExisting`, BOOTy also keeps the
existing EFI boot entries.

## Shared Data Partitions

For `system-ab`, BOOTy mounts the shared data partitions after the root and
EFI partitions. This happens before the hostname, provisioner files, sysexts,
fstab, cloud-init, machine files and post-provision commands.

During initial provisioning, BOOTy seeds an empty shared partition from the
matching directory in the target root slot before it mounts the partition.
During `preserveExisting` upgrades, BOOTy mounts the shared data partitions but
does not format or seed them.

## Preserve-Existing Upgrades

Before BOOTy changes the inactive slot, it checks the disk:

1. The existing partition table must contain the generated layout: the same
   partition nodes, labels and partition types.
2. The target slot must not be the active slot.
3. If a `root=` argument on the current kernel command line identifies an A/B
   slot, that slot must equal `activeSlot`. BOOTy accepts
   `PARTLABEL=BOOTY-ROOT-A`, `LABEL=BOOTY-ROOT-A`,
   `/dev/disk/by-partlabel/...`, `/dev/disk/by-label/...`, `UUID=...`,
   `PARTUUID=...` and partition nodes such as `/dev/sda2`.
4. BOOTy mounts the active slot read-only. Its `/etc/booty/ab-slot.env` must
   report the slot that `activeSlot` declares.

If `root=` does not identify an A/B slot, for example after a virtual-media
boot, BOOTy skips check 3 and relies on check 4. A stale `AB_ACTIVE_SLOT`
value then cannot wipe the active root partition.

Then BOOTy clears the filesystem signatures of the target slot and streams the
image into it. If the write fails or the checksum does not match, BOOTy writes
zeros to the start of the changed partitions (see
[docs/disk.md](disk.md#failed-image-writes)).

Preserve-existing upgrades require kexec. BOOTy keeps `/newroot` mounted until
kexec runs. If kexec is disabled or fails, BOOTy powers off and does not do a
normal reboot, because the firmware boot path can still point at the old
active slot.

Config validation rejects these combinations:

- `preserveExisting` without `IMAGE_MODE=ab` or without `activeSlot`.
- `preserveExisting` with a `targetSlot` equal to `activeSlot`.
- `preserveExisting` with `DISABLE_KEXEC=true`, `SECUREBOOT_REENABLE=true` or
  `CAPRF_LEGACY_COMPAT=true`.

## Vars File Example

The initramfs agent reads the vars file `/deploy/vars`:

```sh
export PROVISION_TARGET_OS="linux"
# Pin the OCI image by digest. A mutable tag needs IMAGE_CHECKSUM.
export IMAGE="oci://registry.example.com/tcaas/os-node@sha256:<digest>"
export IMAGE_MODE="ab"
export AB_SCHEME="dual-root"
export AB_ACTIVE_SLOT="a"
export AB_TARGET_SLOT="inactive"
export AB_PRESERVE_EXISTING="true"
export AB_SOURCE_ROOT_LABEL="rootfs"
export AB_BOOT_SIZE_MB="512"
export AB_ROOT_SIZE_MB="65536"
export AB_STATE_SIZE_MB="0"
```

Use `AB_SOURCE_ROOT_PARTITION="2"` instead of `AB_SOURCE_ROOT_LABEL` only for
source images without a stable GPT root partition label.

For `system-ab`, set the shared data partitions as JSON:

```sh
export AB_SCHEME="system-ab"
export AB_DATA_PARTITIONS='[{"label":"BOOTY-HOME","mountpoint":"/home","filesystem":"ext4","sizeMB":65536},{"label":"BOOTY-DATA","mountpoint":"/var","filesystem":"ext4","sizeMB":0}]'
```

## YAML Example

The same settings in the `pkg/config` YAML schema. The runtime reads only
`/deploy/vars`, not YAML. The schema is for library users and tests.

```yaml
provision:
  image:
    mode: ab
    urls:
      - oci://registry.example.com/tcaas/os-node:v2
    checksumType: sha256
    checksum: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  ab:
    scheme: system-ab
    activeSlot: a
    targetSlot: inactive
    preserveExisting: true
    sourceRootPartition: 2
    bootSizeMB: 512
    rootSizeMB: 65536
    dataPartitions:
      - label: BOOTY-HOME
        mountpoint: /home
        filesystem: ext4
        sizeMB: 65536
      - label: BOOTY-DATA
        mountpoint: /var
        filesystem: ext4
        sizeMB: 0
```

## Requirements

- Sign source images and pin them by checksum.
- `rootSizeMB` must hold the source root filesystem with operational headroom.
  BOOTy does not grow the root partition in A/B mode.
- Use `preserveExisting: true` only on disks that BOOTy provisioned with the
  same A/B scheme.
- Prefer a kernel `root=` argument that resolves to `BOOTY-ROOT-A`,
  `BOOTY-ROOT-B` or the matching partition node. For virtual-media boots, the
  state file of the active slot is the required check.
- Keep `/etc/booty/ab-slot.env` in both root slots. BOOTy writes it during
  provisioning and checks it during later upgrades.
- Sysext layers stay independent of the OS image. Preload sysexts into the
  target root during A/B provisioning, then activate the composition at boot
  or through a higher-level updater.
