# More Storage Targets

## Status

Proposal. Design only. Nothing in this document is implemented. It extends
[proposal-disk-partitioning.md](proposal-disk-partitioning.md).

## Priority

P3

## Summary

Extend the storage model beyond GPT, ext4, xfs, vfat, swap, LVM and mdadm
RAID1: btrfs and ZFS root layouts, Ceph OSD disk preparation, and boot from
SAN (iSCSI and NVMe over Fabrics).

## Problem and motivation

- `PARTITION_LAYOUT` supports `vfat`, `ext4`, `xfs`, `swap` and raw
  (`pkg/config/partition.go`). SLES uses btrfs with snapshots; some sites use
  ZFS root (NixOS, Ubuntu option, verify).
- Storage nodes need clean, labeled OSD disks before the Ceph operator runs.
- Diskless or SAN-boot hosts need the root on a remote target.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Filesystems | `vfat`, `ext4`, `xfs`, `swap`, raw. | `pkg/config/partition.go`, `pkg/disk/partition.go` |
| LVM | Volume groups and logical volumes in `PARTITION_LAYOUT`. | `pkg/disk/lvm.go` |
| RAID | mdadm RAID1 mirror. | `pkg/disk/mirror.go` |
| Remote storage | None. | |

## Goals

- btrfs: subvolume layout (for example `@`, `@home`, `@var`), default
  subvolume, compression option, `fstab` with `subvol=`.
- ZFS: pool and datasets; only with a target OS that ships ZFS and a ramdisk
  flavor with ZFS tools (licence: CDDL, verify distribution rules for the
  ramdisk).
- Ceph OSD preparation: wipe, optional NVMe namespaces, label disks
  (GPT name and a marker) for the operator; never create OSDs (Rook owns that).
- iSCSI and NVMe-oF root: login or connect in BOOTy, write the image to the
  remote LUN or namespace, configure the target initramfs to connect at boot
  (iBFT for iSCSI, verify; NVMe-oF boot via NBFT, verify).

## Non-goals

- Ceph cluster management. Storage array management.

## Design options

- btrfs: `mkfs.btrfs` and `btrfs subvolume` in the ramdisk (GPL tools) or
  write a btrfs image from the pipeline. Recommendation: tools in the
  default flavor if size allows (verify), else a `storage` flavor.
- ZFS: opt-in `zfs` flavor only.
- iSCSI: `open-iscsi` in a `san` flavor, or a Go iSCSI initiator (verify
  maturity). NVMe-oF: kernel `nvme-tcp` and `nvme connect` (nvme-cli already
  used for namespaces).
- Recommendation: btrfs first (SLES target), Ceph OSD preparation second
  (simple), NVMe-oF over TCP third, iSCSI fourth, ZFS on demand.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `PARTITION_LAYOUT` | add `btrfs` filesystem with `subvolumes`, and `zfs` pools | JSON schema extension. |
| `CEPH_OSD_DISKS` | JSON selector list | Disks to prepare and label. |
| `SAN_TARGET` | JSON: `{"type":"iscsi","portal":...,"iqn":...}` or `{"type":"nvme-tcp","traddr":...,"nqn":...}` | Remote root target. |
| `SAN_CHAP_SECRET_FILE` | path | Never inline. |

### CAPRF and bm4x impact

- Host class decides OSD disks; bm4x storage types (`storage_types.go` in
  bm4x-operator, verify fit).
- SAN target data from the storage team's inventory.

## Security considerations

- CHAP secrets and NVMe-oF keys only from files with mode 0600, never vars.
- Wrong OSD selection destroys data. Same guards as wipes, plus an explicit
  allow list by serial.

## Testing strategy

- Unit: layout validation for btrfs and ZFS; OSD selector matching.
- KVM: `storage-btrfs` with a SLES or openSUSE image; `nvme-tcp-root` with a
  Linux `nvmet` target in a second VM or the host (verify CI support);
  `iscsi-root` with a `targetcli` LIO target.
- Real hardware: SAN boot per vendor HBA.

## Phased rollout

1. PR: btrfs subvolumes in `PARTITION_LAYOUT`.
2. PR: Ceph OSD preparation.
3. PR: NVMe-oF TCP root.
4. PR: iSCSI root.
5. PR: ZFS flavor.

## Effort estimate

| Phase | Days |
|-------|------|
| btrfs | 4 to 6 |
| Ceph OSD prep | 3 to 4 |
| NVMe-oF | 6 to 9 |
| iSCSI | 6 to 9 |
| ZFS | 5 to 8 |
| **Total** | **24 to 36** |

## Open questions

- Which storage layouts are product requirements, not nice-to-have?

## Related proposals

- [proposal-disk-partitioning.md](proposal-disk-partitioning.md)
- [proposal-disk-lifecycle.md](proposal-disk-lifecycle.md)
- [proposal-os-targets.md](proposal-os-targets.md)
