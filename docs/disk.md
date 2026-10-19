# Disk Handling

This guide describes how BOOTy selects the target disk, wipes disks, builds
RAID arrays and NVMe namespaces, applies partition layouts, finds the root
partition, and grows the root filesystem. It describes the behaviour on `main`.

[docs/configuration.md](configuration.md) is the reference for all variables,
defaults and accepted values. This guide links to it and does not repeat the
tables. [docs/ab-partitioning.md](ab-partitioning.md) describes A/B image mode.
[docs/caprf-legacy-migration.md](caprf-legacy-migration.md) compares BOOTy with
the CAPRF legacy scripts.

## Configuration Sources

BOOTy reads the vars file `/deploy/vars` and its process environment. It does
not read YAML or JSON files (see
[Configuration sources](configuration.md#configuration-sources)). Some disk
settings exist only as `pkg/config` fields, and the vars file has no key for
them. So BOOTy does not use them at runtime today:

- `provision.disk.raid` (software RAID)
- `deprovision.device` and `deprovision.secureErase`

`BOOTY_ALLOW_REMOVABLE` is not a vars file key. BOOTy reads it from the process
environment (see [Process environment](configuration.md#process-environment)).
A vars file entry works only with `CAPRF_LEGACY_COMPAT=true`.

### Variables Used

See [docs/configuration.md](configuration.md) for each variable:

- Target disk: `DISK_DEVICE`, `DISK_SERIAL`, `DISK_SERIAL_NUMBER`,
  `DISK_SELECTION`, `MIN_DISK_SIZE_GB`, `BOOTY_ALLOW_REMOVABLE`
- Wipe: `MODE`, `SECURE_ERASE`, `WIPE_DEPTH`, `WIPE_ALL_DISKS`,
  `DISCARD_NVME`, `RAID_SUPERBLOCK_CLEAR`, `DRY_RUN`, `CAPRF_LEGACY_COMPAT`
  (presets `DISK_SELECTION=lsblk`, `WIPE_DEPTH=quick`, `WIPE_ALL_DISKS=true`,
  `DISCARD_NVME=true` and `RAID_SUPERBLOCK_CLEAR=all`)
- Layout: `PARTITION_LAYOUT`, `IMAGE_MODE`, `IMAGE_SOURCE_ROOT_LABEL`,
  `IMAGE_SOURCE_ROOT_PARTITION`
- Root partition: `ROOT_PARTITION_LABEL`, `ROOT_PARTITION_NUMBER`
- NVMe: `NVME_NAMESPACES`
- A/B: `AB_*` (see [docs/ab-partitioning.md](ab-partitioning.md))

## Processing Order

Before any disk step, `release-stale-mounts` runs right after `report-init`.
It unmounts the target root and everything below it that a failed earlier
attempt left mounted, for example before a `RESCUE_MODE=retry` run. If this
fails, the run stops before any disk is changed and BOOTy reports the error.

The provisioning pipeline then runs the disk steps in this order:

1. `disable-lvm` and `stop-raid`: deactivate LVM volume groups
   (`lvm vgchange -an`) and stop all md arrays (`mdadm --stop --scan`).
   With `RAID_SUPERBLOCK_CLEAR=all`, `stop-raid` then zeroes the RAID
   superblocks on every disk.
2. `setup-nvme-namespaces`: apply `NVME_NAMESPACES`.
3. `setup-raid`: wipe the RAID member disks and create the arrays.
4. `detect-disk`: select the target disk.
5. `wipe-disks`: wipe the target disk or all disks. With the default
   `RAID_SUPERBLOCK_CLEAR=wiped`, it first zeroes the RAID superblocks on
   the disks that it wipes.
6. `apply-partition-layout`: create the declared GPT layout and LVM volumes.
7. `stream-image`: write the image.
8. `partprobe`, `parse-partitions`: read the new partition table and find the
   boot and root partitions.
9. `check-filesystem`, `enable-lvm`, `mount-root`, `mount-boot`.
10. `grow-partition` and `resize-filesystem`, after the target root is mounted.

## Target Disk Selection

The `detect-disk` step uses the first rule that applies:

1. `device` in `PARTITION_LAYOUT`.
2. `DISK_DEVICE`. The path must exist and must be a block device.
3. `DISK_SERIAL` or `DISK_SERIAL_NUMBER` (see [Serial Lookup](#serial-lookup)).
4. Automatic detection (see below). `DISK_SELECTION` selects the rule:
   `prefer-nvme` (default) or `lsblk`.

If `PARTITION_LAYOUT` sets `device` and `DISK_DEVICE` has a different value,
BOOTy stops with a `disk device conflict` error before it wipes a disk.

`NVME_NAMESPACES` and RAID can set the disk device before this step. See
[NVMe Namespaces](#nvme-namespaces) and [Software RAID](#software-raid).

### Automatic Detection

BOOTy reads `/sys/block` and skips virtual devices: `loop*`, `sr*`, `ram*`,
`zram*`, `dm-*`, `md*`, `zd*` and `nbd*`. It also skips removable media unless
`BOOTY_ALLOW_REMOVABLE=true`. If BOOTy cannot read the `removable` attribute,
it treats the device as removable. It skips disks smaller than
`MIN_DISK_SIZE_GB`. BOOTy measures sizes in GiB (2^30 bytes) and rounds down.

From the remaining disks, BOOTy prefers an NVMe disk. Among disks of the same
kind (NVMe or not NVMe), it takes the largest disk.

Planned (#632): skip hidden NVMe multipath path nodes during disk discovery.

### lsblk Order Detection

With `DISK_SELECTION=lsblk` (preset by `CAPRF_LEGACY_COMPAT=true`), automatic
detection follows the CAPRF legacy scripts:

1. BOOTy runs `lsblk --nodeps --include 259,253` (NVMe and virtio disks), then
   `lsblk --nodeps --include 8` (SCSI and SATA disks).
2. It keeps the disks of at least `MIN_DISK_SIZE_GB` GiB, in `lsblk` order.
3. It takes the first disk.

This order does not skip removable media. A USB disk (major 8) can become the
target when no NVMe or virtio disk qualifies, also without
`BOOTY_ALLOW_REMOVABLE`. `DISK_DEVICE`, the serial lookup and
`PARTITION_LAYOUT.device` keep their normal precedence.

### Disk Names Are Not Stable

The kernel names disks in the order in which it finds them. On a host with
more than one disk of a bus, the names can change between boots: in a lab run
(TCAAS-862) the same 40 GB disk was `sda` in one boot and `sdb` in the next.
These selections can then pick another disk in the next boot:

- `DISK_SELECTION=lsblk` takes the first disk in `lsblk` order. When more
  than one disk of the selected bus (NVMe and virtio, or SCSI and SATA)
  qualifies, BOOTy logs the warning `lsblk disk selection is ambiguous` with
  the candidates.
- `prefer-nvme` takes the largest disk, and the name order decides between
  disks of the same size.
- `DISK_DEVICE=/dev/sdX` is not an automatic rule, but it names the disk
  by its kernel name and has the same problem.

On hosts with more than one disk, select the root disk by a stable value:

- `DISK_SERIAL_NUMBER` (CAPRF `RedfishHost.spec.rootDiskSelector.serialNumber`):
  the identifier that the [Serial Lookup](#serial-lookup) reads, for most
  disks the serial number. For a disk without a serial, the lookup reads the
  WWID (`device/wwid`). A failed lookup lists the identifiers that BOOTy saw.
- `MIN_DISK_SIZE_GB` excludes smaller disks, for example a boot or data
  disk. It does not help when the disks have the same size.

BOOTy runs without udev, so the ramdisk has no `/dev/disk/by-id` or
`/dev/disk/by-path` links; do not use them in `DISK_DEVICE`. BOOTy itself
creates only the `by-uuid`, `by-partuuid`, `by-label` and `by-partlabel`
links (`DISK_LINKS`), and only after the disk selection, for the target
chroot. So `DISK_DEVICE` and `deprovision.device` must be kernel names
(`/dev/sda`, `/dev/nvme0n1`): provisioning, hard deprovision and soft
deprovision reject a path below `/dev/disk/` at once.

### Serial Lookup

`DISK_SERIAL` and `DISK_SERIAL_NUMBER` are aliases. Both set
`provision.disk.serialNumber`. If the vars file sets both, the last line wins.
BOOTy uses the serial only when `DISK_DEVICE` is empty.

For each non-virtual disk, BOOTy reads the first identifier that it finds in
this order:

1. `/sys/block/<disk>/device/serial`
2. `/sys/block/<disk>/serial`
3. `/sys/block/<disk>/device/wwid`
4. `/sys/block/<disk>/device/vpd_pg80` (unit serial number)
5. `/sys/block/<disk>/device/vpd_pg83` (device identification)

The match is exact after BOOTy trims white space. If no disk matches in sysfs,
BOOTy compares the `SERIAL` column of `lsblk --nodeps --paths`. The serial
lookup skips removable media unless `BOOTY_ALLOW_REMOVABLE=true`, also with
`DISK_SELECTION=lsblk`.

The lookup fails when:

- no disk matches (the error lists the observed serials),
- more than one disk matches,
- the matching disk is smaller than `MIN_DISK_SIZE_GB`.

## Wipe Behaviour

`WIPE_DEPTH` selects what a wipe of a disk does. A wipe does not write over
all data. `quick` leaves the data in place. `thorough` ends with `blkdiscard`
of the whole disk: on most SSDs and NVMe disks the discarded blocks then read
as zeros or are no longer readable, so this works like an erase of the
complete disk, but the drive does not guarantee it. For a guaranteed erase
use `SECURE_ERASE`.

- `thorough` (default): BOOTy lists the partitions and the devices on top of
  the disk with `lsblk`. It stops the LVM logical volumes
  (`lvm lvchange -an`), dm-crypt mappings (`cryptsetup close`) and MD arrays
  (`mdadm --stop`) on the disk, the top ones first. For each partition it
  removes an LVM PV label (`lvm pvremove -ff -y`) or an MD superblock
  (`mdadm --zero-superblock`), runs `wipefs -af` and writes zeros to the first
  and the last MiB. Then it runs `sgdisk --zap-all` (MBR, GPT and the backup
  GPT at the end of the disk) and `wipefs -af` on the disk, writes zeros to the
  first and the last MiB of the disk and runs `blkdiscard` on the disk. A
  device without discard support only logs this. When the old partition table
  is written back, `blkid` and `wipefs` find no file system, PV, array or LUKS
  header in the partitions. A holder that cannot be stopped and a failed
  `wipefs` on the disk fail the wipe. This includes an active LV, dm-crypt
  mapping or MD array when `lvm`, `cryptsetup` or `mdadm` is not in the
  ramdisk (for example the slim flavor): the wipe fails. The other failed
  steps are logged, at INFO when the tool is missing.
  The wipe also runs before every image write of a provision (`wipe-disks`),
  so each provision discards the whole target disk. On a large thin
  provisioned LUN or a slow device the discard can take minutes; set
  `WIPE_DEPTH=quick` when that matters.
  Stopping a holder deactivates the whole LV or array: an LVM VG or MD array
  that spans the wiped disk and a disk that is not wiped stops, and the
  members on the other disk keep their labels, so the VG or array is
  incomplete afterwards. BOOTy removes labels only on the wiped disk.
- `quick` (preset by `CAPRF_LEGACY_COMPAT=true`): `sgdisk --zap-all` and then
  `wipefs -af` on the disk only. The partition contents stay: when the old
  partition table is written back, the LVM PVs and VGs, MD members and file
  systems (with their UUIDs) are found again. The legacy `wipe_all_disks` did
  less (`wipefs -af` on each disk).

BOOTy never wipes, discards or secure-erases a disk that is in use. Before
each of these operations it reads `/proc/self/mountinfo` and `/proc/swaps`.
A device counts as in use when it backs a mounted file system or an active
swap area, directly or through a device on top (LVM, dm-crypt and MD, through
`/sys/class/block/<dev>/slaves`); a partition in use makes its disk in use.
Such a disk is not touched:

- A selected disk (`DISK_DEVICE`, `DISK_SERIAL_NUMBER`, the detected install
  target, the deprovision disk) fails the wipe with `refusing to wipe <dev>:
  it backs ...`. With `WIPE_ALL_DISKS=true`, provisioning checks the selected
  target disk first and fails the same way.
- A wipe of all disks (hard deprovision without a disk override,
  `decommission`, `WIPE_ALL_DISKS=true`, secure erase of all disks) skips the
  disk with a WARN line naming it and the mount, and wipes the other disks.
  The deprovision success status names the skipped disks
  (`deprovisioning complete; not wiped, in use: /dev/sdX`), and so does the
  `completed` ACK of a standby deprovision command. When every disk
  is in use, nothing is wiped and the run fails.

If `/proc/self/mountinfo` cannot be read, or sysfs (`/sys/dev/block`,
`/sys/class/block`, `/sys/fs/btrfs/<uuid>/devices`) is missing or does not
resolve, BOOTy refuses the wipe and the run fails. This also holds for
`decommission` and `WIPE_ALL_DISKS=true`, which otherwise log a failed disk
wipe and go on, like the legacy `wipe_all_disks`.

The mount source counts too, for file systems that report an anonymous
device number: btrfs (with all devices of a multi-device btrfs) and bcachefs
(with every device of a `/dev/a:/dev/b` source). ZFS names a pool and
dataset, not a device, so a disk of a mounted ZFS pool is not found.

In the ramdisk nothing is mounted from a disk, so the check changes nothing
there. It protects a host that runs BOOTy in a privileged container, where
`/sys/block` lists the host disks. The check is limited to the mount
namespace of BOOTy: a disk that only another container or the host mounts is
not found, so do not run a deprovision in a container that sees host disks.

The boot medium is not protected: the ramdisk does not mount the ISO or USB
stick that it booted from, so a wipe of all disks wipes it like any other
disk.

"All disks" means every entry in `/sys/block` except the virtual devices listed
in [Automatic Detection](#automatic-detection). This includes removable media.

| Mode | Wipe behaviour |
| --- | --- |
| `provision` | Wipe of the target disk. With `SECURE_ERASE=true`, a hardware erase of the target disk. |
| `provision` with `WIPE_ALL_DISKS=true` | All disks: with `DISCARD_NVME=true`, `blkdiscard` on each NVMe disk, then a wipe of each disk. BOOTy logs failed disk wipes and continues, but the run fails when the selected target disk is in use, when every disk is in use or when the in-use check cannot be made; a disk in use is skipped. `SECURE_ERASE` has no effect. `DISK_DEVICE` does not limit the wipe. |
| `deprovision` (alias `hard`) | `DISK_DEVICE` set: wipe only that disk. `DISK_DEVICE` empty: wipe of all disks. A failed disk fails the run. |
| `deprovision` with `WIPE_ALL_DISKS=true` | `DISK_DEVICE` empty: the legacy wipe of all disks (`blkdiscard` on NVMe with `DISCARD_NVME=true`, wipe, failed disk wipes logged; a disk in use is skipped, and the run fails when every disk is in use or the in-use check cannot be made). `DISK_DEVICE` set: wipe only that disk. |
| `soft-deprovision` (alias `soft`) | No wipe. BOOTy renames `grub.cfg` to `grub.cfg.bak` in `/boot/grub` (Debian) and `/boot/grub2` (RHEL) on the root partition. A `grub.cfg.bak` that already exists counts as done, so a rerun succeeds. If neither directory has a `grub.cfg` or a `grub.cfg.bak`, or the root cannot be unmounted after the rename, the run fails and BOOTy reports the error to `ERROR_URL`. |
| `decommission` | Wipe of all disks, best effort. BOOTy logs failed disk wipes and continues; a disk in use is skipped, and the run fails when every disk is in use or the in-use check cannot be made. With `DISCARD_NVME=true`, `blkdiscard` runs on each NVMe disk first. `DISK_DEVICE` and `SECURE_ERASE` have no effect. Then BOOTy removes all EFI boot entries. |
| `dry-run` (or `DRY_RUN=true`) | No wipe. The dry-run checks do not write to disk. The disk check uses `DISK_DEVICE` or automatic detection and ignores `DISK_SERIAL`. |

Exceptions in the `wipe-disks` step:

- `IMAGE_MODE=ab` with `AB_PRESERVE_EXISTING=true` skips the wipe. BOOTy clears
  only the filesystem signatures of the target root slot before it streams the
  image. See [docs/ab-partitioning.md](ab-partitioning.md).
- A RAID config disables `WIPE_ALL_DISKS`. BOOTy wipes the
  RAID member disks in `setup-raid` and then wipes the array device.
- In deprovision modes, `DISK_DEVICE` must be an absolute `/dev/` path to a
  block device. `soft-deprovision` uses `DISK_DEVICE` or automatic detection to
  find the disk. It ignores `DISK_SERIAL`.

`IMAGE_MODE=partition` wipes the target disk again before it writes the
partitions.

### Secure Erase

`SECURE_ERASE=true` sets `provision.disk.secureErase`. BOOTy uses it only in
`provision` mode, for the target disk and for RAID member disks. Deprovision
modes read `deprovision.secureErase`, which the vars file cannot set (see
[Configuration Sources](#configuration-sources)).

BOOTy selects the erase method by the media type. A SCSI disk (`sd*`) with
the vendor `ATA` in `/sys/block/<disk>/device/vendor` is an ATA disk, also
behind a SAS HBA; other SCSI disks are SAS disks.

| Drive | Erase method | Error when |
| --- | --- | --- |
| NVMe | `nvme format <dev> --ses=1 --force` (User Data Erase) | the format fails, or `nvme` is missing |
| ATA (SATA) | ATA Security Erase with `hdparm` and a temporary password | the drive has no ATA security feature set, is security-frozen, or a `hdparm` command fails; `hdparm` is missing |
| SAS | SCSI `SANITIZE` with `sg_sanitize --quick`: crypto erase, then block erase, then overwrite with zeros; then `FORMAT UNIT` with `sg_format --format --quick` | the drive rejects every method, or a method fails; `sg_sanitize` and `sg_format` are missing |
| Other (virtio, unknown vendor) | none | always |

A failed or unsupported secure erase fails the run by default. BOOTy does not
fall back to a signature wipe on its own. After a failed ATA erase BOOTy tries
to clear the temporary password.

The release initramfs ships `nvme` and `hdparm`, but not `sg_sanitize` and
`sg_format` (sg3-utils). A SAS secure erase fails there with
`sg_sanitize is not in the initramfs, sg_format is not in the initramfs`. Add
sg3-utils to a custom initramfs, or use `SECURE_ERASE_FALLBACK=wipe`.

`SECURE_ERASE_FALLBACK=wipe` (YAML `provision.disk.secureEraseFallback`)
allows the fallback: BOOTy logs a `WARN` line with the erase error, wipes the
disk like without `SECURE_ERASE` (`WIPE_DEPTH`) and adds
`; secure erase not done, wiped instead (SECURE_ERASE_FALLBACK=wipe): <disks>`
to the success status and the standby deprovision ACK. A wipe is not an
erase: the data stays on the media. The default `fail` (or no value) fails
the run.

`CAPRF_LEGACY_COMPAT` does not depend on the fallback: its preset
`WIPE_ALL_DISKS=true` wipes every disk and ignores `SECURE_ERASE`, like the
legacy `wipe_all_disks`. The plain wipe (`SECURE_ERASE=false`, the default)
does not change.

### Failed Image Writes

When a whole-disk or QCOW2 write fails, or the checksum does not match, BOOTy
writes zeros to the first 1 MiB of the target disk. This removes the partition
table, so the machine cannot boot a partial image. When BOOTy writes into
partitions (A/B mode or `PARTITION_LAYOUT`), it writes zeros to the first 1 MiB
of each target partition that it changed.

## Software RAID

`provision.disk.raid` is a list of md arrays. The vars file has no RAID key
(see [Configuration Sources](#configuration-sources)).

| Field | Rule |
| --- | --- |
| `name` | `mdN` (for example `md0`) or `md/<name>`. No `/dev/` prefix. Names must be unique. |
| `level` | `0`, `1`, `5`, `6` or `10` |
| `devices` | Member block devices. BOOTy removes duplicates. A device must not be in two arrays. |

Minimum number of unique member devices:

| Level | Minimum devices |
| --- | --- |
| 0, 1 | 2 |
| 5 | 3 |
| 6, 10 | 4 |

Config validation and `setup-raid` both check these rules. `setup-raid` checks
them before it changes a disk.

The `setup-raid` step:

1. Selects the target device. If `DISK_DEVICE` is set, it must name one of the
   array devices. With one array and no `DISK_DEVICE`, the target is
   `/dev/<name>`. With more than one array, `DISK_DEVICE` is required.
2. Wipes each member disk (`WIPE_DEPTH` wipe, or hardware erase with
   `provision.disk.secureErase`).
3. Creates each array with
   `mdadm --create /dev/<name> --level <level> --raid-devices <n> --run --force --metadata 1.2 <devices>`.

## Disk Mirroring

`DISK_MIRRORING_MODE` mirrors the installed system with RAID1 across two
disks, like the CAPRF legacy provisioner: root as `/dev/md2`, a separate
`/boot` as `/dev/md1`. `raid-1` needs a pair of disks with equal size and
sector size, `auto` falls back to one disk, `none` is the default. See
[Configuration](configuration.md#disk-and-image) and the `DISK_MIRRORING_MODE`
row of the README for the full rules.

The quick wipe (`WIPE_DEPTH=quick`, preset by `CAPRF_LEGACY_COMPAT`) does not
clear the metadata 1.0 superblocks at the end of old member partitions, so a
replicated partition table would bring them back. So BOOTy zeroes the
superblock of every `linux_raid_member` (whole disks and partitions) after
`stop-raid` stopped the arrays. `RAID_SUPERBLOCK_CLEAR` selects the disks:
`wiped` (default) zeroes them in `wipe-disks`, only on the target disk and the
mirror disk, or on every disk with `WIPE_ALL_DISKS=true`; `all` (preset by
`CAPRF_LEGACY_COMPAT`) zeroes them in `stop-raid` on every disk, like
`clear_mdadm_superblocks` of the legacy provisioner. Hard deprovision zeroes
them on the disks it wipes: with a device override the selected disk and its
mirror companion, else every disk. BOOTy never assembles existing arrays while it provisions: the
ramdisk has no `mdadm.conf`, so a scan would also start foreign or stale
arrays. The installed system gets `/etc/mdadm/mdadm.conf` (and
`/etc/mdadm.conf`) with the legacy line `AUTO +imsm +1.x -all` and the
`ARRAY` lines of `/dev/md2` and `/dev/md1`.

On UEFI, the `create-mirror-efi-boot-entry` step copies the ESP to the second
disk and creates the firmware boot entry `<label>-disk2` for it (for example
`ubuntu-disk2`), like `raid_configure_efibootmgr` of the legacy provisioner.
The removal of old boot entries also removes `<label>-disk2`.
`EFI_BOOT_ENTRY_REQUIRED=false` applies to this entry too. Without EFI
variables, the step checks that the ESP has the removable fallback loader
(`EFI/BOOT/BOOTX64.EFI` or `BOOTAA64.EFI`), copies the ESP and skips the
entry.

### ESP Synchronization

The ESP is no RAID member, because the firmware reads it directly. BOOTy copies
it to the second disk once during provisioning. So that later bootloader
updates in the installed system reach both disks, the `install-esp-sync` step
installs the `booty-esp-sync` service into every mirrored target with an ESP:

| Path in the installed system | Content |
|---|---|
| `/usr/local/libexec/booty-esp-sync` | The helper, copied from `/usr/lib/booty/booty-esp-sync` of the ramdisk |
| `/etc/systemd/system/booty-esp-sync.service` | The unit, with the PARTUUIDs of both ESPs on the command line |
| `/etc/systemd/system/multi-user.target.wants/booty-esp-sync.service` | The link that enables the unit |
| `/etc/booty/esp-sync.conf` | The PARTUUIDs of both ESPs (`source_partuuid`, `destination_partuuid`) |

The service starts when `/boot/efi` is mounted. At start and after each change
below `/boot/efi` it finds the ESP that is mounted, selects the other ESP by
its PARTUUID (never by the FAT UUID, which both copies share), mounts it at
`/run/booty/esp-sync` and makes it an exact copy: new and changed files are
written through a staging directory on the destination, files that are gone
from the source are removed. It holds `/run/lock/booty-esp-sync.lock` while it
copies and writes `/run/booty/esp-sync.ready` after each copy. After a boot
from one disk of a degraded mirror, the other ESP is missing: the service logs
this once and keeps watching, it does not fail.

To turn it off, disable the unit in the installed system, for example with a
post-provision command or cloud-init:

```sh
systemctl disable --now booty-esp-sync.service
```

`systemctl mask booty-esp-sync.service` also keeps a later package or script
from enabling it again. Without the service, copy the ESP to the other disk
after each bootloader update.

## NVMe Namespaces

`NVME_NAMESPACES` is a JSON list with one entry per NVMe controller:

```json
[
  {
    "controller": "/dev/nvme0",
    "namespaces": [
      {"label": "os", "sizePct": 20},
      {"label": "data", "sizePct": 80, "blockSize": 4096}
    ]
  }
]
```

| Field | Rule |
| --- | --- |
| `controller` | Controller character device, `/dev/nvmeN`. Each controller can occur only once. |
| `namespaces` | At least one entry. The sum of `sizePct` must not exceed 100. |
| `label` | Required. BOOTy uses it only in log messages. |
| `sizePct` | `1` to `100`, a percentage of the controller capacity (`tnvmcap`). |
| `blockSize` | `512` (default) or `4096`. |

For each controller, the `setup-nvme-namespaces` step:

1. Reads `nvme id-ctrl`. The controller must support more than one namespace
   (`nn` greater than 1), and must report a valid controller ID (`cntlid`,
   0 to 65519), or the step fails before anything is deleted.
2. Deletes all existing namespaces, including inactive ones
   (`nvme list-ns --all`).
3. Creates each namespace with `nvme create-ns` and attaches it with
   `nvme attach-ns -c <cntlid>`, using the controller ID from step 1.

The step fails if it creates no namespace. If `DISK_DEVICE` is empty, BOOTy
sets the target disk to the first namespace that it created on the first
controller, for example `/dev/nvme0n1`. BOOTy does not reset namespaces during
deprovisioning.

## Partition Layout

`PARTITION_LAYOUT` is a JSON object that declares a GPT layout. BOOTy creates
the layout on the target disk and then streams the root filesystem of the
source image into the partition or logical volume with mountpoint `/`.

```sh
export PARTITION_LAYOUT='{"partitions":[{"label":"EFI","sizeMB":512,"filesystem":"vfat","mountpoint":"/boot/efi"},{"label":"root","filesystem":"ext4","mountpoint":"/"}]}'
```

Layout rules (`config.ParsePartitionLayout`):

- `table` must be `gpt` or empty. `device` must be an absolute, clean path.
- 1 to 128 partitions. Labels are required, unique, at most 36 characters, and
  use only letters, digits, space, `-`, `_` and `.`.
- `sizeMB` is in MiB. `0` or no value fills the remaining space. Only the last
  partition can do this.
- `filesystem` is `vfat`, `ext4`, `xfs`, `swap` or empty.
- A `mountpoint` needs a `filesystem`. A swap partition has no `mountpoint`.
  Mountpoints are unique, and exactly one is `/`.
- `mountOptions` is one fstab options token without white space, for example
  `defaults,noatime`.
- `typeGUID` must be a UUID. Without it, BOOTy sets the type from the
  filesystem: EFI system partition for `vfat`, Linux swap for `swap`, Linux
  filesystem for all others. The LVM physical volume partition gets the Linux
  LVM type.
- BOOTy rejects unknown JSON fields and trailing content.

Runtime rules for layouts outside A/B mode:

- `IMAGE_MODE` must be `whole-disk` or empty.
- Only the mountpoints `/` and `/boot/efi` are supported. A logical volume
  cannot use `/boot/efi`.

`IMAGE_SOURCE_ROOT_LABEL` or `IMAGE_SOURCE_ROOT_PARTITION` selects the source
image partition to copy. They are mutually exclusive. Without them, BOOTy uses
the source root selection rules of
[docs/ab-partitioning.md](ab-partitioning.md#source-root-selection).

After `mount-root`, BOOTy writes `/etc/fstab` in the target root. Each entry
uses `PARTLABEL=<label>`. BOOTy reads the root filesystem type of the streamed
image with `blkid`. Default options are `defaults`, and `umask=0077` for
`/boot/efi`. `mountOptions` replaces the default.

In A/B mode, BOOTy generates the layout from the `AB_*` variables and ignores
`PARTITION_LAYOUT`.

### LVM

The `lvm` object adds one volume group on one layout partition:

```sh
export PARTITION_LAYOUT='{"partitions":[{"label":"EFI","sizeMB":512,"filesystem":"vfat","mountpoint":"/boot/efi"},{"label":"lvm"}],"lvm":{"volumeGroup":"sysvg","pvPartition":2,"volumes":[{"name":"swap","sizeMB":4096,"filesystem":"swap"},{"name":"root","filesystem":"ext4","mountpoint":"/"}]}}'
```

- `pvPartition` is the 1-based index of the physical volume partition. That
  partition has no `filesystem` and no `mountpoint`.
- Each volume has a unique `name`. Its size is `sizeMB` (`lvcreate -L`) or
  `extents` (`lvcreate -l`, for example `50%VG`), not both. A volume without
  both uses `100%FREE` and must be the last volume.
- The root volume is `/dev/<volumeGroup>/<name>`.

BOOTy checks for the `lvm` tool before `wipe-disks`. Then it runs
`lvm pvcreate -f`, `lvm vgcreate` (skipped if the volume group exists) and
`lvm lvcreate`, and formats each volume.

## Root Partition Detection

The `parse-partitions` step finds the root partition with the first rule that
applies:

1. `PARTITION_LAYOUT` or A/B mode: the partition or logical volume with
   mountpoint `/`.
2. `ROOT_PARTITION_LABEL`: the partition with this GPT partition name. The match
   ignores case. The partition must have the Linux filesystem type. BOOTy fails
   if the label matches more than one partition or no partition.
3. `ROOT_PARTITION_NUMBER`: the partition with this 1-based number. It must
   have the Linux filesystem type.
4. Automatic detection.

`ROOT_PARTITION_LABEL` and `ROOT_PARTITION_NUMBER` are mutually exclusive.

Automatic detection uses only partitions with the Linux filesystem type (GPT
`0FC63DAF-8483-4772-8E79-3D69D8477DE4` or MBR `83`):

1. If exactly one candidate is named `root` or `/` (case is ignored), BOOTy
   uses it. If more than one has these names, BOOTy fails.
2. If there is only one candidate, BOOTy uses it.
3. If there is more than one candidate, BOOTy checks their content. It mounts
   each candidate read-only without journal replay, in partition table order.
   It selects the first candidate that has a regular file `/etc/os-release`, or
   both an `/etc` and a `/usr` directory. If no candidate matches, BOOTy fails.

`soft-deprovision` uses only rules 1 and 2 of automatic detection.

## Boot Partition and Separate /boot

BOOTy uses the first EFI system partition of the written image as the boot
partition. With `PARTITION_LAYOUT`, it uses the partition with mountpoint
`/boot/efi`. The `mount-boot` step mounts it at `/boot/efi` in the target root.
Without an EFI system partition, BOOTy logs a warning and skips the mount.

For mirrored whole-disk images, BOOTy reads the image `/etc/fstab`, identifies
the separate `/boot` partition (or the GPT XBOOTLDR partition), mirrors it as
`/dev/md1`, and mounts that array before GRUB and initramfs updates. Ambiguous
extra Linux partitions are rejected. Non-mirrored images keep `/boot` on the
root filesystem unless a declarative partition layout supplies the mount.

## Filesystem Check, Partition Growth and Resize

`check-filesystem` checks the root partition before BOOTy mounts it:

| Filesystem | Command | Result |
| --- | --- | --- |
| ext2/3/4 (and unknown types) | `e2fsck -fy` | Exit codes 1 to 3: BOOTy logs a warning and continues. Exit code 4 or higher, or an interrupted run: BOOTy fails. Without `e2fsck`, BOOTy skips the check. |
| XFS | `xfs_repair -n` | Any error fails the step. |
| Btrfs | `btrfs check --readonly` | Any error fails the step. |
| vfat | none | BOOTy fails. vfat is supported only for the EFI system partition. |

`grow-partition` runs `growpart --update on <disk> <root partition number>`.
`NOCHANGE` (the partition already fills the space) is not an error. By default
BOOTy grows only the root partition. If the root partition is not the last
partition, it usually cannot grow. With `PARTITION_LAYOUT` or in A/B mode,
BOOTy skips this step.

With `GROW_PARTITION=last-linux` (preset by `CAPRF_LEGACY_COMPAT=true`), BOOTy
grows the last GPT Linux filesystem partition instead, like `provision.sh`,
also when it is a data partition after the root. When that partition is not
the root, BOOTy runs `e2fsck -p -f` on it before `growpart`, and
`resize-filesystem` resizes it offline with `resize2fs`; the root keeps its
size. Without a GPT Linux partition (for example an MBR image), BOOTy grows
the root.

`last-linux` assumes ext4 on a data partition after the root, like
`provision.sh`. With XFS or btrfs there, `e2fsck` fails before `growpart`
changes the partition table: the run fails, and no data is lost. The BOOTy
ramdisk ships `e2fsck`. Without it, BOOTy logs a warning and skips the
check; `growpart` then enlarges the partition and the offline resize fails
for XFS or btrfs (they grow only when mounted), so the run fails after the
table change. The
partition only grows, so the data stays, but the filesystem keeps its size.

`resize-filesystem` grows the root filesystem. It tries `resize2fs <device>`,
then `xfs_growfs <mountpoint>`, then `btrfs filesystem resize max <mountpoint>`.
It fails for vfat.

## Image Formats

BOOTy writes raw images, compressed raw images (gzip, zstd, lz4, xz, bzip2) and
QCOW2 images. It converts a QCOW2 image to raw in process, in a tmpfs ramdisk,
before it writes the disk; the ramdisk has no `qemu-img`. QCOW2 versions 2 and
3 work, with zlib or zstd compressed clusters and extended L2 entries. A QCOW2
image must be self-contained: BOOTy rejects images with a backing file, an
external data file or encryption, a truncated image (a table or cluster past
the end of the file), and an image whose virtual size is larger than the
target disk. See
[README.md](../README.md#caprf-mode) for the image source rules.
