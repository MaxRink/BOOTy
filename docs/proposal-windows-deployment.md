# Windows Server Deployment

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P3

## Summary

Deploy Windows Server from the BOOTy Linux ramdisk. The recommended path
writes a sysprepped whole-disk image (raw or VHDX converted to raw), injects
`unattend.xml` and a driver folder into the NTFS partition, and reboots. The
firmware boot entry points at the Windows Boot Manager. A WIM apply path is an
option for sites that only have WIM media. This document lists what a Linux
ramdisk can do and what needs Windows itself.

## Problem and motivation

- Some tenants need Windows Server nodes (Windows worker nodes for Kubernetes,
  or legacy workloads) on the same hardware pool.
- CAPRF and BOOTy reject `TARGET_OS=windows` today. Operators then use a
  separate tool chain and lose the inventory, health, wipe and reporting steps.
- A single provisioning path reduces tooling and audit effort.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Target OS | `windows` is rejected before any disk write. | `pkg/config/validate.go` (`ValidateProvisionTargetOS`), `pkg/config/provision.go` comment |
| Image write | Raw stream to the disk, checksum, failed-write cleanup. Format-agnostic for `whole-disk`. | `pkg/image/stream.go` |
| Post-write steps | Assume a Linux root: `mount-root`, `configure-grub`, `write-fstab`, chroot steps. | `pkg/provision/orchestrator.go` (`provisionSteps`) |
| EFI entries | `create-efi-boot-entry` builds load options; `pkg/efi` can write variables. | `pkg/efi/vars.go` (`BuildLoadOption`), `pkg/provision/orchestrator.go` |
| NTFS | The initramfs has no NTFS tools. | `initrd.Dockerfile` |
| Handoff | kexec into Windows is not possible. Reboot is the only path. | `main.go` (`tryKexec`) |

## What a Linux ramdisk can and cannot do

| Task | Linux ramdisk | Note |
|------|---------------|------|
| Write a whole-disk image | Yes | Same as Linux images. |
| Apply a WIM to an NTFS partition | Yes, with `wimlib-imagex apply` and NTFS-3G (verify licence: wimlib is LGPLv3 or GPLv3, NTFS-3G is GPLv2) | Keeps NTFS ACLs and reparse points only with the NTFS-3G backend (verify). |
| Create GPT, ESP, MSR, NTFS partitions | Yes (`sgdisk`, `mkfs.vfat`, `mkntfs`) | MSR has type `E3C9E316-0B5C-4DB8-817D-F92DF00215AE`. |
| Write `unattend.xml` | Yes | `C:\Windows\Panther\unattend.xml` is read by Windows Setup in the specialize and oobeSystem passes (verify search order). |
| Stage drivers | Yes, as files | Reference them in `unattend.xml` with `Microsoft-Windows-PnpCustomizationsNonWinPE` `DriverPaths`. Windows installs them during specialize. |
| Offline driver injection (`dism /add-driver`) | No | Needs Windows or WinPE. |
| Create BCD store (`bcdboot`) | No native tool | Options: ship a template BCD store in the image, or edit it with `hivex` (verify that BCD edits with hivex are reliable). |
| Create UEFI boot entry for `\EFI\Microsoft\Boot\bootmgfw.efi` | Yes | `pkg/efi` or `efibootmgr`. |
| Secure Boot | Partly | Windows Boot Manager is signed by Microsoft. The firmware `db` must trust the Microsoft Windows Production CA (2011 or 2023, verify the current CA transition). BOOTy cannot sign anything for Windows. |
| BitLocker | No | Windows enables it after first boot with TPM protectors. |
| Activation and licensing | No | KMS, AVMA or MAK in `unattend.xml` or after boot. BOOTy only carries the key file. |
| Domain join | No | `unattend.xml` or first-boot script. |

## Goals

- Accept `TARGET_OS=windows` with a Windows target profile.
- Write a sysprepped whole-disk image and inject `unattend.xml`, drivers and a
  first-boot script.
- Create the firmware boot entry for Windows Boot Manager.
- Report success to CAPRF only after Windows reports first boot complete
  (optional phone-home).

## Non-goals

- Building Windows images. The image pipeline (Packer or similar) builds and
  sysprepps them.
- Running WinPE from BOOTy.
- Windows Server licensing management.
- Legacy BIOS (MBR) Windows installs.

## Design options

### Option A: Whole-disk sysprepped image (dd)

The image pipeline builds a GPT image with ESP, MSR and an NTFS `C:`
partition, and runs `sysprep /generalize /oobe`. BOOTy streams it like a Linux
image, mounts the NTFS partition with NTFS-3G (or ntfs3 kernel driver, verify
write support in the shipped kernel), writes `unattend.xml`, copies drivers and
a first-boot script, and unmounts. Then it creates the EFI entry and reboots.

- Pro: reuses the streaming, checksum and wipe code. Fast. Same as MAAS and
  Ironic whole-disk Windows images (verify).
- Con: the image has a fixed partition size. Windows must extend `C:` on first
  boot (`diskpart` or `Resize-Partition` in the first-boot script).

### Option B: WIM apply

BOOTy partitions the disk, formats NTFS, applies `install.wim` with wimlib,
copies a prepared BCD store and boot files to the ESP, and writes
`unattend.xml`.

- Pro: uses Microsoft media directly. Partition sizes fit the disk.
- Con: the BCD store needs disk and partition GUIDs. Without `bcdboot`, BOOTy
  must patch the store. This is fragile. Adds wimlib and NTFS-3G to a new
  initramfs flavor.

### Option C: Chain to WinPE

BOOTy prepares the disk and boots WinPE from a staging partition, which runs
`dism` and `bcdboot`.

- Pro: native tools. Full driver injection.
- Con: two ramdisks. WinPE licensing. The handoff from BOOTy to WinPE needs a
  reboot and a one-time boot entry. Hard to report progress.

### Recommendation

Option A first. Keep Option B as a second phase if image pipelines cannot
produce whole-disk images. Reject Option C because it doubles the agent
surface.

Build a separate initramfs flavor `windows` (NTFS-3G or ntfs3, `hivex` only if
needed). The default flavors stay small.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `TARGET_OS` | add `windows` | Selects the Windows profile. |
| `WINDOWS_UNATTEND` | ramdisk path, default `/deploy/windows/unattend.xml` | Written to `Windows\Panther\unattend.xml`. |
| `WINDOWS_DRIVERS_DIR` | ramdisk path, default `/deploy/windows/drivers` | Copied to `C:\Drivers`. |
| `WINDOWS_FIRSTBOOT_SCRIPT` | ramdisk path | Copied to `Windows\Setup\Scripts\SetupComplete.cmd` (verify). |
| `WINDOWS_SYSTEM_PARTITION` | GPT label or number | NTFS partition to mount. Default: the largest Microsoft basic data partition. |
| `WINDOWS_EXTEND_SYSTEM` | boolean, default `true` | Adds the extend command to the first-boot script. |

### CAPRF and CAPI API impact

- `RedfishMachine.spec` needs an OS family `windows` and a reference to an
  unattend template secret. CAPRF renders the template with host data
  (hostname, network) and places it in `/deploy/windows/`.
- CAPI has no Windows bootstrap provider for bare metal in core. Kubeadm
  Windows support uses a cloudbase-init or a script (verify). The first
  version should target non-Kubernetes Windows hosts or a site-specific
  bootstrap.
- CAPRF must not call `DisableSecureBoot` for Windows when the image is signed
  by the Microsoft CA, and must not use `SECUREBOOT_REENABLE` flows that kexec.

## Security considerations

- `unattend.xml` can hold the local administrator password and product keys.
  Write it with restrictive ACLs if NTFS-3G supports them (verify), and add a
  first-boot step that deletes it. Never log it.
- Drivers are kernel code. Require signed drivers. Windows enforces driver
  signatures with Secure Boot on.
- Secure Boot: keep the Microsoft UEFI CA trust in `db` for Windows hosts.
  Track the Microsoft CA 2023 transition and `dbx` updates (see
  [proposal-uefi-management.md](proposal-uefi-management.md)).
- BitLocker keys belong in a vault, not in CAPRF objects.

## Testing strategy

- Unit: Windows profile validation. NTFS partition selection on fixture GPTs.
  `unattend.xml` template rendering. Rejected keys.
- KVM matrix: a weekly scenario `windows-dd` with an evaluation image if the
  licence allows CI use (verify). OVMF with Secure Boot and the Microsoft keys
  (`OVMF_VARS.ms.fd` in Debian, verify). Assert first boot reaches
  `SetupComplete` through a serial or network marker.
- ContainerLab: none.
- kindmetal and CAPRF e2e: one Windows machine when the CAPRF API exists.
- Real hardware: one node per vendor. Check driver staging for the main NIC and
  RAID controllers.

## Phased rollout

1. PR: Windows profile and `TARGET_OS=windows` validation behind a build tag or
   the `windows` flavor. Steps that need a Linux root are skipped.
2. PR: NTFS mount in the `windows` flavor and `unattend.xml` and driver
   injection.
3. PR: EFI boot entry for `bootmgfw.efi` and reboot handoff.
4. PR: first-boot phone-home (optional `SUCCESS_URL` call from Windows).
5. CAPRF MR: OS family and unattend template.
6. Optional PR: WIM apply mode.

## Effort estimate

| Phase | Days |
|-------|------|
| Profile and validation | 2 to 3 |
| `windows` flavor with NTFS | 3 to 5 |
| Injection and EFI entry | 4 to 6 |
| KVM test with Windows | 5 to 8 |
| CAPRF plumbing | 4 to 6 |
| WIM apply (optional) | 8 to 12 |
| **Total without WIM** | **18 to 28** |

## Open questions

- Is there a real demand for Windows on this platform? If not, keep the
  rejection and close this proposal.
- Can CI use a Windows evaluation image under its licence terms?
- Which Kubernetes bootstrap, if any, should Windows nodes use?
- Is `ntfs3` in the BOOTy kernel good enough, or is NTFS-3G required?

## Related proposals

- [proposal-os-targets.md](proposal-os-targets.md)
- [proposal-uefi-management.md](proposal-uefi-management.md)
- [proposal-secureboot.md](proposal-secureboot.md)
