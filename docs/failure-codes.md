# Failure codes

<!-- Generated from pkg/provision/codes.go by TestFailureCodeCatalogDoc. Do not edit. -->

BOOTy attaches a stable code `BOOTY-<AREA>-<NNN>` to every step failure.
The code is in the `ERROR_URL` body
(`step <name> failed [<code>]: <error>`) and in the failed step of the
`METRICS_URL` summary. A code never changes its meaning. A retired
code stays reserved.

A known error (for example a checksum mismatch) gets its own code in every
step (step `*`). Any other error gets the code of its step.

| Code | Class | Step | Meaning |
|------|-------|------|---------|
| `BOOTY-RUN-001` | permanent | `*` | Step failed with an error that has no specific code. |
| `BOOTY-RUN-002` | environment | `*` | Run canceled or deadline exceeded. |
| `BOOTY-NET-001` | environment | `network-setup` | Network setup failed. |
| `BOOTY-NET-002` | environment | `network-setup` | Connectivity check to the provisioning server failed. |
| `BOOTY-NET-003` | environment | `network-setup` | Token exchange or token renewal start failed. |
| `BOOTY-RPT-001` | transient | `report-init` | Init callback failed. |
| `BOOTY-RPT-002` | transient | `report-success` | Success callback failed. |
| `BOOTY-HW-001` | hardware | `collect-inventory` | Hardware inventory collection failed. |
| `BOOTY-HW-002` | hardware | `collect-firmware` | Firmware collection failed. |
| `BOOTY-HW-003` | hardware | `health-checks` | A health check failed. |
| `BOOTY-HW-004` | hardware | `setup-mellanox` | Mellanox NIC setup failed. |
| `BOOTY-CFG-001` | config | `validate-provision-inputs` | Provisioning inputs are invalid. |
| `BOOTY-IMG-001` | permanent | `verify-image` | Image signature verification failed. |
| `BOOTY-IMG-002` | transient | `stream-image` | Image download or write failed. |
| `BOOTY-IMG-003` | permanent | `*` | Image checksum mismatch. |
| `BOOTY-IMG-004` | permanent | `*` | Image format probe failed. |
| `BOOTY-IMG-005` | transient | `apply-sysexts` | System extension download or install failed. |
| `BOOTY-IMG-006` | transient | `prepare-oci-prepulls` | OCI image pre-pull preparation failed. |
| `BOOTY-DSK-001` | environment | `release-stale-mounts` | Stale target mounts could not be released. |
| `BOOTY-DSK-002` | hardware | `disable-lvm` | Disabling LVM failed. |
| `BOOTY-DSK-003` | hardware | `stop-raid` | Stopping RAID arrays failed. |
| `BOOTY-DSK-004` | hardware | `setup-nvme-namespaces` | NVMe namespace setup failed. |
| `BOOTY-DSK-005` | hardware | `setup-raid` | RAID setup failed. |
| `BOOTY-DSK-006` | hardware | `detect-disk` | No usable target disk found. |
| `BOOTY-DSK-007` | hardware | `wipe-disks` | Disk wipe or secure erase failed. |
| `BOOTY-DSK-008` | hardware | `apply-partition-layout` | Applying the partition layout failed. |
| `BOOTY-DSK-009` | hardware | `partprobe` | Partition table re-read failed. |
| `BOOTY-DSK-010` | environment | `*` | Target disk in use, or the in-use check failed. |
| `BOOTY-DSK-011` | permanent | `parse-partitions` | Partitions of the target disk not found. |
| `BOOTY-DSK-012` | permanent | `*` | Ambiguous root partition candidates. |
| `BOOTY-DSK-013` | hardware | `mirror-root` | Root mirror setup failed. |
| `BOOTY-DSK-014` | hardware | `check-filesystem` | File system check failed. |
| `BOOTY-DSK-015` | hardware | `enable-lvm` | Enabling LVM failed. |
| `BOOTY-DSK-016` | hardware | `mount-root` | Mounting the root file system failed. |
| `BOOTY-DSK-017` | hardware | `mount-boot` | Mounting the boot file system failed. |
| `BOOTY-DSK-018` | hardware | `mount-shared-data` | Mounting the shared data file system failed. |
| `BOOTY-DSK-019` | hardware | `grow-partition` | Growing the root partition failed. |
| `BOOTY-DSK-020` | hardware | `resize-filesystem` | Resizing the root file system failed. |
| `BOOTY-DSK-021` | config | `configure-mdadm` | Writing the mdadm configuration failed. |
| `BOOTY-DSK-022` | config | `select-deprovision-disk` | Deprovision disk selection failed. |
| `BOOTY-DSK-023` | hardware | `decommission-wipe` | Decommission disk wipe failed. |
| `BOOTY-EFI-001` | environment | `mount-efivarfs` | Mounting efivarfs failed. |
| `BOOTY-EFI-002` | environment | `*` | EFI variables unavailable. |
| `BOOTY-EFI-003` | hardware | `remove-efi-entries` | Removing EFI boot entries failed. |
| `BOOTY-EFI-004` | hardware | `remove-all-efi-entries` | Removing all EFI boot entries failed. |
| `BOOTY-EFI-005` | config | `install-esp-sync` | Installing ESP sync failed. |
| `BOOTY-EFI-006` | config | `install-efi-fallback` | Installing the EFI fallback loader failed. |
| `BOOTY-EFI-007` | permanent | `verify-secureboot-chain` | Secure Boot chain verification failed. |
| `BOOTY-EFI-008` | hardware | `create-efi-boot-entry` | Creating the EFI boot entry failed. |
| `BOOTY-EFI-009` | hardware | `create-mirror-efi-boot-entry` | Creating the mirror EFI boot entry failed. |
| `BOOTY-OS-001` | config | `configure-overlayfs` | Overlay file system configuration failed. |
| `BOOTY-OS-002` | config | `set-hostname` | Setting the host name failed. |
| `BOOTY-OS-003` | config | `copy-provisioner-files` | Copying provisioner files failed. |
| `BOOTY-OS-004` | config | `configure-dns` | DNS configuration failed. |
| `BOOTY-OS-005` | config | `write-fstab` | Writing fstab failed. |
| `BOOTY-OS-006` | environment | `setup-chroot-binds` | Chroot bind mounts failed. |
| `BOOTY-OS-007` | config | `configure-kubelet` | Kubelet configuration failed. |
| `BOOTY-OS-008` | config | `configure-serial-console` | Serial console configuration failed. |
| `BOOTY-OS-009` | config | `configure-grub` | GRUB configuration failed. |
| `BOOTY-OS-010` | config | `inject-cloudinit` | Cloud-init injection failed. |
| `BOOTY-OS-011` | config | `copy-machine-files` | Copying machine files failed. |
| `BOOTY-OS-012` | config | `run-machine-commands` | A machine command failed. |
| `BOOTY-OS-013` | config | `run-post-provision-cmds` | A post-provision command failed. |
| `BOOTY-OS-014` | environment | `teardown-chroot` | Chroot teardown failed. |
| `BOOTY-OS-015` | config | `soft-deprovision` | Soft deprovision failed. |
