# Migration from the CAPRF legacy ramdisk

This page is for operators who move hosts from the CAPRF legacy shell ramdisk
to BOOTy. The legacy ramdisk runs `provision.sh`, `deprovision.sh` and
`common.sh` from `internal/provision/files/` in cluster-api-provider-redfish.
BOOTy replaces these scripts with one Go binary.

This page is the reference for `CAPRF_LEGACY_COMPAT`. For the CAPRF contract
in general, see [CAPRF integration](caprf-integration.md). For variables, see
the [configuration reference](configuration.md).

## Migration checklist

1. Set `CAPRF_LEGACY_COMPAT=true` in `/deploy/vars`. CAPRF sets it when it
   runs BOOTy in place of the legacy ramdisk. See
   [Enable CAPRF legacy compatibility](caprf-integration.md#enable-caprf-legacy-compatibility).
2. Set `TARGET_OS=linux`, for example through CAPRF `additionalVars`. BOOTy
   refuses to provision without it. The legacy scripts do not use it.
3. Do not move hosts that use `DISK_MIRRORING_MODE=raid-1` yet. BOOTy
   ignores the variable and installs on one disk. Planned (#581).
4. Check the machine commands. BOOTy rejects shell syntax in them.
   Planned (#587).
5. Check images with a separate `/boot` partition. BOOTy does not mount it.
   Planned (#595).
6. Check machine files that contain symlinks. BOOTy skips them.
   Planned (#629).
7. Remove `AB_PRESERVE_EXISTING=true`. It cannot be combined with
   `CAPRF_LEGACY_COMPAT=true` or `SUCCESS_ACTION=poweroff`.

## What CAPRF_LEGACY_COMPAT changes

With `CAPRF_LEGACY_COMPAT=true`, BOOTy keeps the legacy behaviour where the
two differ. Without it, BOOTy keeps its own defaults. The variable is a
preset: it sets options that the vars file does not set, and an explicit
option wins. The options are listed in the last column of the
[parity matrix](#parity-matrix) and in
[CAPRF legacy compatibility](configuration.md#caprf-legacy-compatibility).
The variable changes these behaviours:

- **End of run.** After success, BOOTy powers off. It never uses kexec
  (`SUCCESS_ACTION=poweroff`). After a failure, BOOTy reports the error and
  waits (`FAILURE_ACTION=wait`). Details are in
  [End of a run](caprf-integration.md#end-of-a-run).
- **Transport.** Plain HTTP is allowed and TLS certificates are not
  verified, like `curl --insecure`. Every `/deploy/vars` entry is exported
  to the environment, like `source vars`. Details are in
  [Transport rules](caprf-integration.md#transport-rules).
- **Provisioner files and DNS.** `/deploy/file-system` goes into the ramdisk
  before the network starts, like `copy_files`, and not into the installed
  OS. BOOTy does not write the installed OS `resolv.conf`.
- **Disks.** The target disk is selected like `_select_single_disk`
  (`DISK_SELECTION=lsblk`), and `wipe-disks` wipes every disk like
  `wipe_all_disks` (`WIPE_ALL_DISKS=true`). `decommission` also discards NVMe
  disks (`DISCARD_NVME=true`).
- **GRUB and console.** The kernel parameters are written like
  `provision.sh`. BOOTy writes no getty units.
- **Ramdisk hostname and NIC firmware log** are set and written like
  `provision.sh`.

The matrix below has one row for each behaviour.

## Parity matrix

The last column names the option that selects the legacy behaviour without
`CAPRF_LEGACY_COMPAT`, with the value that the preset sets. An explicit
option always wins over the preset. "None yet" means that only
`CAPRF_LEGACY_COMPAT=true` selects the behaviour. "Not needed" means that both
modes behave the same.

Status values:

- **Merged (#NNN)**: on `main`.
- **Planned (#NNN)**: open pull request. Not on `main`.
- **Not implemented**: no BOOTy equivalent and no open pull request.
- **Deliberately different**: BOOTy does not copy the legacy behaviour.

### Run lifecycle

| Legacy behaviour | BOOTy default | BOOTy with `CAPRF_LEGACY_COMPAT=true` | Status | Native option (preset value) |
|------------------|---------------|---------------------------------------|--------|------------------------------|
| Provision success: `status_success`, then `shutdown now` | Report success, kexec into the installed OS, else reboot | Report success, power off, no kexec | Merged (#585) | `SUCCESS_ACTION=poweroff` |
| Deprovision success: `status_success`, `sync`, `poweroff -f` | Report success, reboot | Report success, power off | Merged (#585) | `SUCCESS_ACTION=poweroff` |
| `sync` before the handoff | `sync` before reboot, power off and kexec | Same | Merged (#623, #597) | Not needed |
| Failure: `fail()` posts to `ERROR_URL`, the ramdisk stays up | Report the error. Provision applies `RESCUE_MODE` (default `reboot`). Deprovision reboots. | Report the error and wait. An explicit `RESCUE_MODE` still wins for provision. | Merged (#585) | `FAILURE_ACTION=wait` |
| Failure before the task (network, JWT) or JWT renewal failure: `fail()` | Reboot | Report the error and wait | Merged (#585) | `FAILURE_ACTION=wait` |
| `/deploy/vars` is a shell script that `source` runs | Parsed as data: no variable expansion, no command substitution | Same | Deliberately different | Not needed |
| No target OS check | `TARGET_OS=linux` or `PROVISION_TARGET_OS=linux` required for provisioning | Same | Deliberately different | Not needed |
| `MODE=decommission` (`force-clean`): `decommission_wipe`, `remove_all_efi_boot_entries`, power off | Wipe every disk with `sgdisk` and `wipefs`, remove every `BootOrder` entry, log failures and continue, reboot | Same, plus `blkdiscard` on every NVMe disk first, then power off | Merged (#586) | `SUCCESS_ACTION=poweroff`, `DISCARD_NVME=true` |
| Soft deprovision: rename `grub.cfg` | Rename `grub.cfg` to `grub.cfg.bak` in `/boot/grub` and `/boot/grub2`. An existing `grub.cfg.bak` counts as done. No config at all, or an unmount failure after the rename, is an error. | Same, the disk is selected like `_select_single_disk` | Merged (#590), `/boot/grub2` and errors in #635 | `DISK_SELECTION=lsblk` |

### Disks

Detail for these rows is in [Disk handling](disk.md).

| Legacy behaviour | BOOTy default | BOOTy with `CAPRF_LEGACY_COMPAT=true` | Status | Native option (preset value) |
|------------------|---------------|---------------------------------------|--------|------------------------------|
| `wipe_all_disks` for provision and hard deprovision: `blkdiscard` on NVMe disks, then signature wipes on every disk | Provision wipes only the target disk; `SECURE_ERASE=true` erases it in hardware. Hard deprovision without `DISK_DEVICE` wipes every disk, without `blkdiscard`. | Every disk: `blkdiscard` on NVMe disks, then `sgdisk --zap-all` and `wipefs -af`. Failures are logged and do not stop the run. `SECURE_ERASE` has no effect. Not for dry-run, explicit RAID configs, or a deprovision with `DISK_DEVICE`. | Merged (#591) | `WIPE_ALL_DISKS=true`, `DISCARD_NVME=true` |
| `_select_single_disk`: first disk of at least `MIN_DISK_SIZE_GB` in `lsblk` order, NVMe and virtio first, then SCSI/SATA. Removable media are not skipped. | Without `DISK_DEVICE` or `DISK_SERIAL`: NVMe first, then the largest disk. Removable media are skipped unless `BOOTY_ALLOW_REMOVABLE=true`. | Legacy order. A USB disk can be selected when no NVMe or virtio disk qualifies. | Merged (#590) | `DISK_SELECTION=lsblk` |
| Grow the last GPT Linux partition | Grow the root partition | Same as default | Planned (#625) | Not needed |
| Mount a separate `/boot` from the image `fstab` when the root has no `/boot/grub` | Not mounted. `update-grub` writes `grub.cfg` on the root filesystem. | Same as default | Planned (#595) | Not needed |
| `DISK_MIRRORING_MODE=raid-1`: RAID1 `/dev/md2` over two disks | Variable ignored. One disk. | Same as default | Planned (#581) | Not needed |

### Transport and configuration input

| Legacy behaviour | BOOTy default | BOOTy with `CAPRF_LEGACY_COMPAT=true` | Status | Native option (preset value) |
|------------------|---------------|---------------------------------------|--------|------------------------------|
| `curl --insecure` for callbacks, the connectivity check and image downloads. Any URL scheme. | TLS certificates verified. Bearer token only over HTTPS or loopback. `http://` images need `IMAGE_ALLOW_INSECURE_HTTP=true`. | Plain HTTP allowed. TLS certificates not verified. The crash artifact upload still verifies. | Merged (#589) | None yet |
| `source vars` exports every entry, so `https_proxy` and `no_proxy` apply | Unknown keys ignored. Proxy only from the process environment. | Every entry with a shell identifier as key is exported. Proxy entries apply to callbacks, the connectivity check and image downloads. | Merged (#589) | None yet |
| `copy_files`: `/deploy/file-system` into the ramdisk before the network starts, netplan files mode `0600`, symlinks skipped | Copied into the installed OS (`copy-provisioner-files` step). Symlinks skipped. | Copied into the ramdisk before the network starts. Netplan files get mode `0600`. Not copied into the installed OS. If the copy fails, BOOTy waits without a callback. | Merged (#588) | None yet |
| Ramdisk `/etc/resolv.conf` from the provisioner files, never rewritten. Installed OS `resolv.conf` not written. | Ramdisk `resolv.conf` written from `dns_resolver` or netplan. Installed OS `resolv.conf` written from `dns_resolver`. | A provisioner `/etc/resolv.conf` stays in the ramdisk. The installed OS `resolv.conf` is not written. | Merged (#588) | None yet |
| `debug()` progress lines to `DEBUG_URL` | `DEBUG_URL` is read but not used. Progress goes to the log and `LOG_URL`. | Same | Not implemented | Not needed |

### Installed OS configuration

| Legacy behaviour | BOOTy default | BOOTy with `CAPRF_LEGACY_COMPAT=true` | Status | Native option (preset value) |
|------------------|---------------|---------------------------------------|--------|------------------------------|
| `/etc/default/grub.d/10-caprf-kernel-params.cfg` with `GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud console=ttyS0,115200n8 <extra>"`. `ttyS1` on Lenovo. `update-grub` only when the image has it. | Same file, but `GRUB_CMDLINE_LINUX` does not keep the image value. `ds=` only with `CLOUDINIT_ENABLED=true`. One `console=` from the [serial console resolver](serial-console-resolution.md), plus a GRUB serial terminal and getty units. `update-grub` must exist. | Legacy line. `ttyS1` when DMI `sys_vendor` is `Lenovo`. Extra parameters verbatim, so several `console=` tokens can occur. `update-grub` only when the image has it. No getty units. | Merged (#594) | None yet |
| Extra kernel parameters: `MACHINE_EXTRA_KERNEL_PARAMS` and `EXTRA_KERNEL_PARAMS` (from `additionalVars`) | Both read. `EXTRA_KERNEL_PARAMS` goes after `MACHINE_EXTRA_KERNEL_PARAMS`. `console=` tokens are merged into the resolved console. | Both read, same order, appended verbatim. A value with `"`, `\`, `$`, a backtick or a line break fails the run. | Merged (#594) | None yet |
| `hostname "$HOSTNAME"` in the ramdisk | Not set in the ramdisk. `/etc/hostname` of the installed OS is written in both modes. | Set for provisioning. A failure is only logged. | Merged (#626) | None yet |
| `nic_firmware` log lines with the firmware version of every NIC (`ethtool`) | Only with `FIRMWARE_REPORT=true`, as a JSON report to `FIRMWARE_URL` | `nic_firmware` log lines on every provision, also without `FIRMWARE_REPORT` | Merged (#628) | None yet |
| `setup_mellanox_num_of_vfs` on every provision: set `NUM_OF_VFS=32` when lower, skip quietly without `mstconfig` or ConnectX NIC | Only with `NUM_VFS` > 0: sets `SRIOV_EN` and `NUM_OF_VFS`. Fails without a Mellanox NIC. A change forces a reboot instead of kexec. | On every provision: raises `NUM_OF_VFS` to 32 (or `NUM_VFS`) with `mstconfig` in the ramdisk on each ConnectX-4/5/6/6 Dx/6 Lx/7 Ethernet function. Skips quietly without `mstconfig` or such a NIC. A failed `mstconfig` query or set fails the run. | Merged (#617) | None yet |
| Machine commands: each file runs as a script in a chroot (`#!` line or `/bin/sh`), in CAPRF index order | Each file is one command. Shell syntax (`;`, `\|`, `&&`, `>`, `<`, `$(`, backtick) is rejected. Files run in name order. | Same as default | Planned (#587) | Not needed |
| Machine files: `cp -rfv`, symlinks kept | Copied, symlinks skipped | Same as default | Planned (#629) | Not needed |

### Networking

Detail for these rows is in [Networking](networking.md).

| Legacy behaviour | BOOTy default | BOOTy with `CAPRF_LEGACY_COMPAT=true` | Status | Native option (preset value) |
|------------------|---------------|---------------------------------------|--------|------------------------------|
| EVPN through FRR with the provisioner netplan and `frr.conf` | Reads the provisioner netplan and `frr.conf` as input and sets up FRR or GoBGP | Same, with the provisioner files also applied to the ramdisk | See [Netplan and FRR config input](networking.md#netplan-and-frr-config-input) | None yet |
| `wait_for_network`: `NETWORK_WAIT_MAX_ATTEMPTS` rounds of 20 seconds (default 15), restart FRR, then `fail()` | Up to 3 network setups, each followed by an HTTP `HEAD` poll on `INIT_URL` (see [Connectivity check](networking.md#connectivity-check)) | Same as default. After the last attempt BOOTy reports the error and waits. | Planned (#622) | Not needed |

### Common pipeline

These legacy functions have a BOOTy step with the same effect in both modes.

| Legacy function | BOOTy step or package |
|-----------------|-----------------------|
| `load_vars` | `caprf.ParseVars` |
| `status_init`, `status_success` | `report-init`, `report-success` |
| `mount_efi_vars` | `mount-efivarfs` |
| `remove_efi_boot_entries` | `remove-efi-entries` |
| `mdadm --stop` | `stop-raid` |
| `pick_best_source` | `image.SelectBestSource`: the URL with the fastest ranged `GET` |
| Compression detection and `curl \| dd` | `stream-image` |
| `sfdisk` partition parsing, EFI and root GUID match | `parse-partitions` |
| `e2fsck`, `resize2fs` | `check-filesystem`, `resize-filesystem` |
| Mount the root partition | `mount-root` |
| Chroot bind mounts | `setup-chroot-binds` |
| Kubelet configuration | `configure-kubelet` |

### Not carried over

The original integration plan listed these parts of the older deployer and
debirf ramdisks.

| Legacy component | BOOTy |
|------------------|-------|
| `aria2c` multi-source download | Not used. BOOTy picks one source and streams it. |
| `journalbeat` | Not used. Logs go to `LOG_URL`. |
| netplan in the ramdisk | Not used. BOOTy parses the netplan files and sets up the network itself. |
| Image formats | BOOTy reads raw, gzip, lz4, xz, zstd and bzip2 images, QCOW2 images and OCI references. A QCOW2 image with a backing file or an external data file is rejected: Merged (#609). |
