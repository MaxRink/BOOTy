# kexec: Behaviour, Limitations and Drawbacks

This page describes how BOOTy uses kexec to start the installed OS, and every
known limitation and risk of that path. It is reference documentation for
operators and reviewers. It does not propose changes; proposals that change
the kexec path are linked at the end.

Statements about BOOTy behaviour cite the code. Statements about the Linux
kernel, firmware, systemd, GRUB, Flatcar or vendor hardware that were not
checked against a primary source in this repository are marked "verify".

## Contents

1. [How BOOTy uses kexec today](#1-how-booty-uses-kexec-today)
2. [Firmware and hardware state that kexec skips](#2-firmware-and-hardware-state-that-kexec-skips)
3. [Measured boot and attestation](#3-measured-boot-and-attestation)
4. [Secure Boot and kernel lockdown](#4-secure-boot-and-kernel-lockdown)
5. [Boot loader semantics that kexec bypasses](#5-boot-loader-semantics-that-kexec-bypasses)
6. [Console, display and logs](#6-console-display-and-logs)
7. [Memory](#7-memory)
8. [Hardware and driver problems](#8-hardware-and-driver-problems)
9. [Networking and the fabric](#9-networking-and-the-fabric)
10. [Operations: CAPRF, BMC, watchdog, time](#10-operations-caprf-bmc-watchdog-time)
11. [Architecture specifics (arm64)](#11-architecture-specifics-arm64)
12. [Decision table: kexec or reboot](#12-decision-table-kexec-or-reboot)
13. [Test coverage and gaps](#13-test-coverage-and-gaps)

## 1. How BOOTy uses kexec today

### Code path

| Step | What happens | Reference |
|------|--------------|-----------|
| Trigger | A successful provision run (or a successful standby `provision` command) returns `runmode.ProvisionCompleteError`. `endRun` calls `finishProvision`, which calls `tryKexec`. | `main.go` (`endRun`, `finishProvision`) |
| Announcement | Before that, the `report-success` step sends `provisioning complete handoff=<kexec\|reboot\|poweroff>`. `PlannedHandoff` makes the same decision as `tryKexec`, including a dry run of the entry resolution. CAPRF power cycles the host only for `poweroff`. | `pkg/provision/orchestrator.go` (`PlannedHandoff`), [caprf-integration.md](caprf-integration.md#handoff) |
| Gates | `tryKexec` returns `false` (no kexec) when `DISABLE_KEXEC=true`, when `SUCCESS_ACTION=poweroff` (`KexecDisabled`), when `SECUREBOOT_REENABLE=true`, or when a step changed firmware values (`FirmwareChanged`: the Mellanox `NUM_VFS` path, or an SR-IOV profile that stored firmware values, `res.Reboot`). A stored SR-IOV firmware change that needs a verify reboot (`SRIOV_VERIFY_REBOOT=on` with `REBOOT_URL`) ends the task earlier with `RebootRequiredError`: BOOTy reboots into the ramdisk and does not reach `tryKexec`. | `main.go` (`tryKexec`), `pkg/config/config.go` (`KexecDisabled`), `pkg/provision/sriov.go` (`configureSRIOV`, `RebootRequiredError`) |
| Target kept mounted | When kexec is possible, `teardown-chroot` keeps `/newroot` mounted so BOOTy can read the kernel. | `pkg/provision/orchestrator.go` (`ShouldKeepTargetRootMountedForKexec`) |
| Entry resolution | `kexec.DefaultEntry` returns the entry that GRUB would boot, and only when it is sure. It reads `grub.cfg` from `/boot/grub`, then `/boot/grub2`, accepts only the shapes that `grub-mkconfig` generates on Debian and Ubuntu, and returns `ErrUnsupported` for everything else. | `pkg/kexec/default.go`, `pkg/kexec/grubwords.go`, real fixtures in `pkg/kexec/testdata/` |
| Path resolution | The kernel and initrd paths are resolved under the target root, then under `/boot` inside it. Symlinks are followed only inside the target root. | `main.go` (`resolveKexecPath`, `resolveKexecCandidate`) |
| Load | `kexec_file_load(kernel_fd, initrd_fd, cmdline, flags=0)`. | `pkg/kexec/kexec_linux.go` (`Load`) |
| Release target | After the load, BOOTy unmounts every mount below the target root; a busy block device mount is remounted read-only. `KEXEC_UNMOUNT_TARGET=false` keeps it mounted. | `main.go` (`releaseKexecTarget`), `pkg/disk` (`UnmountOrRemountReadOnly`) |
| 1. Watchdog | `watchdogHandoff(false)` disarms the watchdog: the magic close (`V`) for `/dev/watchdog`, an IPMI Set Watchdog Timer without action (stop) for the BMC watchdog of the IPMI fallback (PR #721; since PR #816 over `/dev/ipmi0` without `ipmitool`). `WATCHDOG_HANDOFF=keep` leaves either armed. | `main_watchdog.go`, `pkg/watchdog/device_linux.go`, `pkg/watchdog/bmc_linux.go` |
| 2. LLDP shutdown | `stopLLDPEmit` stops the every-NIC LLDP sender, which sends the IEEE 802.1AB shutdown LLDPDU (TTL 0) on each NIC. | `main.go` (`stopLLDPEmit`), `pkg/network/lldp/tx.go` (`ShutdownFrame`) |
| 3. Log flush | `flushLogs` (`flushObservability`) closes the CAPRF log handler; the bounded ring waits up to `LOG_FLUSH_TIMEOUT_SEC` (default 5 s) for queued lines. | `main.go` (`doKexecExecute`, `flushObservability`), `pkg/caprf/loghandler.go` |
| 4. Network stop | `stopNetwork` (`stopNetworkOnce`, PR #723) tears the network mode down once and waits at most 5 s (`networkStopTimeout`). GoBGP mode sends a Cease NOTIFICATION to every peer and BFD AdminDown; FRR mode stops FRR (`frr.Manager.Teardown`). | `main.go` (`stopNetworkOnce`) |
| 5. Execute | `sync()`, then `reboot(LINUX_REBOOT_CMD_KEXEC)`. | `pkg/kexec/kexec_linux.go` (`Execute`) |
| Fallback | Any failure before or in execute returns `false`; BOOTy cleans up the target mounts and reboots through `reboot(2)`, or powers off for A/B preserve-existing. | `main.go` (`cleanupFailedKexecTarget`, `handleProvisionHandoff`) |
| A/B rule | With `IMAGE_MODE=ab` and `AB_PRESERVE_EXISTING=true`, a missing kexec leads to a power off, not a reboot, because the firmware boot path still points at the old slot. Config validation rejects `DISABLE_KEXEC`, `SECUREBOOT_REENABLE` and `SUCCESS_ACTION=poweroff` with it. `CAPRF_LEGACY_COMPAT` presets `SUCCESS_ACTION=poweroff` only when `SUCCESS_ACTION` is not set explicitly; validation does not reject `CAPRF_LEGACY_COMPAT` itself. | `pkg/provision/orchestrator.go` (`RequiresABKexec`), `docs/ab-partitioning.md` |

`doKexecExecute` runs steps 1 to 5 in this order, after the target release.

### Which entry BOOTy kexecs

The full rule list is in
[caprf-integration.md](caprf-integration.md) (section on the kexec boot
entry). In short:

- Accepted: an unconditional `set default`, or the generated `next_entry`
  block with `next_entry` unset; `${saved_entry}` from `grubenv`; submenu
  paths by number, id or title; entries made only of generated commands with
  one `linux` and at most one `initrd`; variables set once and
  unconditionally at the top level, expanded when they are set;
  `$vt_handoff` is dropped.
- The Ubuntu initrdless fallback (`GRUB_FORCE_PARTUUID` cloud images): GRUB
  would boot the initrdless branch; BOOTy kexecs the branch with the initrd,
  because BOOTy's `kexec.Load` needs an initrd: it always opens the initrd
  and does not pass `KEXEC_FILE_NO_INITRAMFS`. Both branches must differ only in one
  trailing `panic=-1`. `grubenv` stays unchanged.
- Refused (reboot through the firmware): BLS (`blscfg`, RHEL-likes, also BLS
  entries without `grub.cfg`), Flatcar (`configfile`, gptprio), any other
  `source` or `configfile`, `next_entry`, `prev_saved_entry`, `initrdfail`
  or `prev_entry` in `grubenv`, a `default` in `grubenv`, any other
  conditional default, a default inside a condition, `linux` or `initrd`
  inside a condition, unknown commands, `eval`, `regexp`, `probe`, function
  bodies that set the default or load a kernel, several initrds, and a
  kernel argument with a double quote.
- Not supported at all: systemd-boot Type #1 entries and UKIs. Such targets
  reboot.

### When BOOTy does not kexec

| Condition | Result | Success message |
|-----------|--------|-----------------|
| `DISABLE_KEXEC=true` | Reboot. | `handoff=reboot` |
| `SUCCESS_ACTION=poweroff` (also via `CAPRF_LEGACY_COMPAT=true`) | Power off. | `handoff=poweroff` |
| `SECUREBOOT_REENABLE=true` | Reboot. CAPRF turns Secure Boot on. | `handoff=reboot` |
| Firmware values changed (Mellanox `NUM_VFS`, or an SR-IOV profile stored firmware values) | Reboot. | `handoff=reboot` |
| SR-IOV verify reboot (`SRIOV_VERIFY_REBOOT=on`, `REBOOT_URL` set) | Reboot into the ramdisk before any disk change (`RebootRequiredError`); the next BOOTy boot finishes the task. | `reboot-required` status, no success message yet |
| `DefaultEntry` returns an error (refused shape, no `grub.cfg`) | Reboot. | `handoff=reboot` |
| `kexec_file_load` fails (for example a kernel signature that lockdown rejects, section 4) | Reboot. | `handoff=kexec` was announced; the actual handoff is a reboot. CAPRF does not power cycle in either case. |
| `reboot(LINUX_REBOOT_CMD_KEXEC)` returns | Reboot. | as above |
| A/B preserve-existing and any of the above | Power off. | `handoff=poweroff` when known in advance |

## 2. Firmware and hardware state that kexec skips

kexec jumps from the running kernel into a new kernel. The firmware does not
run again. Everything that POST and the firmware would do on a reboot does
not happen.

| Area | What a reboot does | What kexec does | Impact |
|------|--------------------|-----------------|--------|
| POST and device init | Resets and re-initializes devices, trains memory and links | Nothing. Devices stay in the state that the old kernel's `shutdown` callbacks left them in. | Devices with weak or missing `shutdown` handlers start in an unexpected state (section 8). |
| Device shutdown | Power or bus reset | The kernel calls `device_shutdown()` (each driver's `.shutdown`) before it jumps (verify for the BOOTy kernel version). | Quality of `.shutdown` decides success. |
| NIC, HBA, GPU firmware | Restarts on reset | Not restarted. Firmware-level settings (for example ConnectX `NUM_OF_VFS`, `SRIOV_EN`, `LINK_TYPE`) that need a firmware reset stay pending. | BOOTy already refuses kexec after its own firmware changes (Mellanox `NUM_VFS`, SR-IOV profiles that store firmware values). Other firmware changes (by vendor tools, by CAPRF through Redfish while the host ran BOOTy, or BIOS changes) are not detected by BOOTy. |
| DMA in flight | Stopped by reset | Must be stopped by drivers. A device that keeps DMA running can write into memory that the new kernel now uses. | Memory corruption that is hard to diagnose (verify; classic kexec risk). |
| IOMMU | Reset, tables rebuilt by firmware and kernel | The old kernel disables translation on shutdown, or the new kernel inherits enabled translation and must handle it (the kdump case copies tables, verify for normal kexec). | DMAR or AMD-Vi faults at boot, devices that fail to probe (section 8). |
| SR-IOV VFs | Gone after reset | PF drivers usually remove VFs in `.shutdown` or `remove` (verify per driver). If not, VFs and their PCI state survive. | Unexpected VFs, wrong `sriov_numvfs`, or failure to create VFs in the target until a reboot. |
| NVMe controllers | Reset | The `nvme` driver shuts the controller down cleanly (verify). Namespace changes from `NVME_NAMESPACES` are already visible because BOOTy rescans. | Usually fine. Firmware activation that waits for a reset (`fw-commit` with action 2, verify) does not happen. |
| Option ROMs | Run by firmware (PXE, RAID setup, GOP) | Not run. | GOP framebuffer from the BOOTy boot is reused; RAID controller BIOS config utilities never run; boot-time controller init that a driver expects from the option ROM is missing (verify per controller). |
| BIOS settings | Applied at the next POST | Not applied. | A BIOS attribute change scheduled through Redfish `@Redfish.Settings` during the BOOTy run takes effect only at the next real reboot. BOOTy cannot see that such a change is pending. |
| Microcode | Firmware applies its microcode; the kernel loads early microcode from the initrd | The new kernel loads early microcode from its own initrd during early boot (verify). The CPU keeps the newest revision already loaded; a downgrade is not possible. | Usually fine. Late loading is disabled by default in recent kernels (verify). |
| ACPI tables | Rebuilt by firmware | The new kernel uses the tables the firmware built for the BOOTy boot. | BIOS changes that alter ACPI tables (for example SR-IOV, C-states, NUMA settings) are not reflected. |
| EFI runtime services | `SetVirtualAddressMap` once per boot | `SetVirtualAddressMap` cannot be called twice. On x86, `kexec_file_load` passes the old runtime mapping to the new kernel. The KVM `kexec` scenario checks under OVMF that the target lists the EFI variables (PR #725, `test/e2e/kvm/kexec_test.go`). | Works on x86 under OVMF. On real firmware and on arm64 it is not tested; if the handover fails, `efivarfs` is empty in the target and `efibootmgr`, `bootctl`, `fwupd` and ESP sync fail (verify on hardware). |
| SMBIOS | Rebuilt | Same tables as the BOOTy boot. | Changes made by the BMC during the run (for example asset tag) are not visible until a reboot (verify). |
| Syscall choice | n/a | BOOTy uses `kexec_file_load`. The older `kexec_load` lets user space supply the image and purgatory and is blocked by kernel lockdown. | See section 4. |
| `KEXEC_SIG` | n/a | With `CONFIG_KEXEC_SIG`, `kexec_file_load` checks the kernel signature; with `CONFIG_KEXEC_SIG_FORCE` or lockdown it requires a valid one (verify the Debian config of the BOOTy kernel, see `initrd.Dockerfile`). | Unsigned or foreign-signed target kernels fail to load under lockdown; BOOTy then reboots. |

## 3. Measured boot and attestation

| Topic | Behaviour after kexec |
|-------|-----------------------|
| PCR values | The TPM is not reset. PCRs keep the values of the BOOTy boot: firmware (PCR 0 to 7), shim and the BOOTy kernel (PCR 4, 7, 14 when shim is used), and whatever BOOTy or its kernel measured. Nothing measures the new kernel unless the old kernel does it on purpose. |
| Event log | The firmware TCG event log describes the BOOTy boot. Linux can pass the log to the new kernel through the EFI configuration table (verify for the BOOTy and target kernels). Either way, it does not describe the target kernel. |
| IMA | With `CONFIG_IMA_KEXEC`, the IMA measurement list is carried into the new kernel, and PCR 10 continues from the old value (verify that the BOOTy kernel enables IMA at all). |
| systemd-stub | A UKI started by kexec does not run systemd-stub as an EFI application, so PCR 11 (UKI sections), PCR 12 (command line) and PCR 13 (sysexts) are not measured (verify for extracted UKIs). |
| systemd-pcrphase | Extends PCR 11 at boot phases in the target, on top of BOOTy-era values. Predictions made for a firmware boot do not match. |
| systemd's PCR 15 rule | systemd measures machine identity and volume keys into PCR 15 and can bind secrets to PCR 15 being zero (verify the default). After kexec, PCR 15 is whatever the BOOTy boot left. |
| LUKS TPM unlock | A policy bound to raw PCR values (PCR 4, 7, 9, 11, 12) of a firmware boot of the target does not match after kexec, so unlock fails and the system asks for a recovery key. A PCR 7 only policy can still match because Secure Boot state did not change, but only if Secure Boot was on for the BOOTy boot. A signed PCR 11 policy (`--tpm2-public-key`) fails because the stub did not measure the UKI. |
| systemd-pcrlock | Predicts values from firmware boot components. A kexec boot is a variant that pcrlock does not predict (verify), so unlock fails. |
| Remote attestation | A verifier that replays the event log sees the BOOTy chain and cannot attest the target kernel. |
| Conclusion | Hosts that use TPM-sealed disk keys, remote attestation or measured-boot policies must reboot through the firmware after provisioning. The measured boot proposal in PR #700 (`docs/proposal-measured-boot-ab.md`) builds on this. |

## 4. Secure Boot and kernel lockdown

- The firmware and shim verify only what they load: shim, the boot loader
  and the BOOTy kernel. The kexec'd kernel is never seen by the firmware,
  shim, or MOK. Shim's SBAT checks, MOK enrollment and `dbx` revocations do
  not apply to it.
- The BOOTy kernel is the Debian-signed 6.12 kernel of Debian 13 (PR #701,
  `initrd.Dockerfile`). Debian kernels enable lockdown (`integrity`)
  automatically when Secure Boot is on.
- Booting BOOTy with Secure Boot on is supported (PR #737; see the README
  [Secure Boot](../README.md#secure-boot) section). Measured in QEMU with
  OVMF, Microsoft keys, shim and GRUB from the BOOTy ISO:

  | Call under lockdown | Result | Effect on BOOTy |
  |---------------------|--------|-----------------|
  | `kexec_file_load`, Debian-signed kernel | accepted (the Debian CA reaches the keyring through shim's MOK list) | kexec works for Debian targets |
  | `kexec_file_load`, Canonical-signed (Ubuntu) kernel | `ENOKEY` | `tryKexec` fails, BOOTy reboots |
  | `kexec_file_load`, unsigned kernel | `ENODATA` | BOOTy reboots |
  | `kexec_load` | `EACCES` | not used by BOOTy |
  | PCI config write, BAR `mmap` | `EPERM` | `mstconfig` fails, so `setup-mellanox` fails: a run with `NUM_VFS` > 0 fails under Secure Boot, with no kexec and no handoff (configure SR-IOV out of band) |

  Without Secure Boot, `lockdown=integrity` on the command line gives the
  same denials, and the Debian kernel is also refused, because the Debian CA
  reaches the keyring only through shim.
- Consequence: with Secure Boot on during the BOOTy boot, kexec works only
  into kernels signed by a key in the BOOTy kernel's keyrings (built-in,
  secondary, platform with MOK). Ubuntu targets always reboot.
  `PlannedHandoff` does not know this in advance and announces `kexec`; the
  load fails and BOOTy reboots.
- By default, CAPRF disables Secure Boot through Redfish before it boots
  BOOTy (`disableSecureBoot` step in CAPRF; CAPRF behaviour, verify). Then
  BOOTy runs without lockdown and unsigned kexec works. This also means the provisioning boot itself is not
  Secure Boot protected.
- `SECUREBOOT_REENABLE=true` disables kexec because CAPRF turns Secure Boot on
  and the host must boot through the firmware.
- The pinned digests (`SECUREBOOT_*_SHA256`) check files on disk; they do not
  make a kexec boot equivalent to a verified firmware boot.

## 5. Boot loader semantics that kexec bypasses

| Mechanism | Effect of kexec |
|-----------|-----------------|
| UEFI `BootNext`, `BootOrder`, `BootCurrent` | Not used. `BootCurrent` in the target reports the BOOTy boot entry (virtual media or the BOOTy entry), not the installed OS (verify). Tools in the target that read `BootCurrent` draw wrong conclusions. |
| EFI boot entry created by BOOTy | Created, but not exercised. A broken entry is found only at the next reboot. |
| GRUB `grubenv` (`saved_entry`, `next_entry`, `initrdfail`, `boot_success`, `boot_indeterminate`) | `saved_entry` is read. A pending `next_entry` (a one-shot boot) or a set `initrdfail` makes BOOTy reboot, so GRUB consumes it. BOOTy never writes `grubenv`; counters such as Ubuntu's `initrdfail` and Fedora's boot success variables (verify) do not move after a kexec boot. |
| GRUB `gptprio` (Flatcar `USR-A`/`USR-B`) | `DefaultEntry` refuses Flatcar's `grub.cfg` (`configfile`, gptprio kernel variables), so Flatcar targets always reboot and GRUB evaluates the priority, tries and successful bits. |
| systemd-boot boot counting (`+tries`) | The loader does not rename the entry, so the counter does not decrease. `systemd-bless-boot` in the target then marks an entry good that systemd-boot never tried (verify the behaviour when `LoaderBootCountPath` is absent). |
| `LoaderEntryOneShot`, `LoaderEntryDefault` | Not consumed. |
| BOOTy A/B preserve-existing | Kexec boots the new slot, but firmware still points at the old slot. The next firmware reboot goes back to the old slot. This is an implicit trial boot without a commit step ([ab-partitioning.md](ab-partitioning.md)). |
| Kernel command line edits in the boot loader | `GRUB_CMDLINE_LINUX_DEFAULT` changes that need `update-grub` are effective only if `grub.cfg` was regenerated before kexec. |
| Ubuntu initrdless boot | GRUB would try the initrdless branch first; BOOTy kexecs the initrd branch. The first boot of the target therefore does not exercise the initrdless path. |

## 6. Console, display and logs

- Serial console: the new kernel uses the `console=` arguments from the parsed
  `grub.cfg` line. GRUB `serial` and `terminal` commands do not run, so GRUB's
  own serial setup is irrelevant. The UART keeps the baud rate set by the
  BOOTy kernel until the new kernel reprograms it.
- Framebuffer: the GOP or VGA framebuffer from the BOOTy boot is handed over
  through boot parameters (`screen_info`, verify). The firmware does not
  re-initialize the display. GPU drivers that expect a freshly posted device
  can fail (section 8). The BMC KVM can show a frozen or blank screen.
- dmesg: the new kernel starts a new ring buffer. BOOTy's kernel messages are
  lost unless they were shipped before the handoff.
- BOOTy logs: `flushObservability` closes the CAPRF log handler before
  execute. The bounded ring (PR #711) waits up to `LOG_FLUSH_TIMEOUT_SEC`
  (default 5 s) for queued lines, evicts lower levels first when full, and
  reports drops. Lines that do not arrive in time are lost.
- Out-of-band status: the `BOOTY1` serial status line (`OOB_STATUS=serial`,
  PR #718) marks the end of the run on every active console before the
  handoff; with `OOB_EFI_VAR=true` the `BootyLastResult` UEFI variable holds
  the result for the next BOOTy run.
- pstore: a `ramoops` region in reserved memory survives kexec when both
  kernels reserve the same region (verify). `efi-pstore` needs EFI runtime
  services in the new kernel (section 2).
- No POST screen and no firmware boot messages appear between BOOTy and the
  target kernel. A hang right after the jump shows no output.

## 7. Memory

- The new kernel and initrd are loaded into memory before the jump, while
  BOOTy's own ramdisk is still in RAM. The ISO targets ship one initramfs
  with FRR inside (`initrd.Dockerfile`). Only the optional `--target=split`
  build (PR #782) has a base ramdisk plus payload archives, which all stay
  in RAM (today only `frr`; `qemu-img` and `ipmitool` are not payloads since
  PR #815 and PR #816). BOOTy does not check
  free memory before the load. When `kexec_file_load` fails with `ENOMEM`,
  `tryKexec` returns `false` and BOOTy reboots through the firmware.
- Placement limits: on x86 the initrd must fit where the new kernel's boot
  protocol allows (below 4 GiB unless the kernel supports higher placement,
  verify).
- `crashkernel=` reservations: the BOOTy kernel's reservation is released;
  the target kernel reserves its own region early. Fragmentation after a long
  BOOTy run is not a problem for boot-time reservations, because the new
  kernel builds a fresh memory map, but very large reservations can fail on
  hosts with odd firmware maps (verify).
- KASLR: `kexec_file_load` on x86 lets the new kernel randomize its placement
  (verify for arm64).
- Memory map: the new kernel receives the firmware memory map from the BOOTy
  boot (`e820` on x86, the EFI memory map). Memory that firmware would retrain
  or retire after errors is not re-evaluated.
- Purgatory: a small trampoline verifies a SHA-256 digest of the loaded
  segments before it jumps (verify). A large initrd adds a short delay.
- Memory errors: correctable errors counted by EDAC in BOOTy are lost; the
  target starts counting from zero.

## 8. Hardware and driver problems

All rows are "verify" unless a test in this repository covers them. Record
confirmed cases in the quirks database.

| Component | Known risk after kexec |
|-----------|------------------------|
| NVIDIA GPUs (`nouveau`, proprietary `nvidia`) | The GPU is not posted again; driver init can fail or hang. The proprietary driver documents limited kexec support (verify). |
| Other GPUs and display controllers | Same class of problem; BMC VGA usually works. |
| `mlx5_core` (ConnectX, BlueField) | Firmware state survives; a pending firmware change does not apply (BOOTy refuses kexec after its own change). PCI function reset behaviour after shutdown varies with firmware (verify). |
| `ice` (Intel E810) | Reset and DDP package reload behaviour after a warm handoff (verify). |
| `bnxt_en`, `i40e`, `ixgbe` | Generally robust; firmware settings changes need a reset (verify). |
| `megaraid_sas`, `mpt3sas` | Controllers can need a firmware-level reset; known issues in kdump scenarios (`reset_devices`) suggest care (verify for normal kexec). |
| IOMMU (`intel_iommu`, `amd_iommu`) | DMA remapping and interrupt remapping state from the old kernel; faults at boot when devices still DMA (verify). |
| TPM | Not reset (section 3). Some TPM drivers need a clean `TPM2_Shutdown` sequence (verify). |
| Watchdog devices | A running hardware watchdog keeps counting (section 10). |

## 9. Networking and the fabric

- BOOTy stops its network mode right before the jump (PR #723).
  `doKexecExecute` runs `watchdogHandoff`, `stopLLDPEmit`, `flushLogs`,
  `stopNetwork` and then `kexecExecute` (`main.go`). `stopNetworkOnce` tears
  the network mode down once and waits at most 5 s: GoBGP mode sends a BGP
  Cease NOTIFICATION to every peer and BFD AdminDown; FRR mode stops FRR
  (`frr.Manager.Teardown`). With GoBGP the leaf ends the sessions at once,
  not after the BFD detection time or the BGP hold timer. That the FRR stop
  ends them at once in the same way is FRR behaviour that is not tested
  here (verify).
- A teardown that does not finish in 5 s does not stop the kexec. Sessions
  that were not closed then end on the leaf at the BFD detection time or
  the hold timer, because the link usually stays up during a kexec.
- The leaf withdraws the host routes, EVPN Type-2 and Type-5 entries. The
  target OS then establishes its own sessions. The fabric sees one session
  flap per handoff. With BFD this looks like a link failure in fabric
  monitoring.
- Link state: NIC drivers bring the link down in `.shutdown` or not, depending
  on the driver (verify). The switch port can see a short carrier loss, no
  loss, or a longer renegotiation.
- LLDP: `stopLLDPEmit` sends the IEEE 802.1AB shutdown LLDPDU (TTL 0,
  `pkg/network/lldp/tx.go`) on each NIC, so the switch removes the neighbour
  at once. The switch has no neighbour until the target OS sends LLDP.
- Intel firmware LLDP: with `LLDP_DISABLE_FW_AGENT=true` (default) BOOTy
  turns off the firmware LLDP agent of Intel NICs, and with it the firmware
  DCBX, and never turns them on again. They stay off after the kexec; ice
  keeps the setting in the NVM, so it also survives a reboot. Ports whose
  agent an SR-IOV profile sets (`firmware.fwLLDPAgent`) are left to that
  profile (see [configuration.md](configuration.md)).
- MAC addresses: physical MACs are unchanged. Bond MACs and VF MACs set by
  BOOTy are not carried over, unless VFs survive (section 2).
- VLAN, VRF, VXLAN devices created by BOOTy disappear with the old kernel.
- Static addresses: the target applies its own config. If BOOTy persisted
  network config (`PERSIST_NETWORK`), it applies after kexec as after a
  reboot.
- Networking parity work: the target-side FRR or networkd setup must accept a
  fabric whose BGP and BFD state just flapped. Graceful restart is not used
  between BOOTy and the target (verify the leaf configuration).

## 10. Operations: CAPRF, BMC, watchdog, time

- Debugging: a hang right after kexec shows no POST, no firmware messages, and
  possibly no console output. The BMC shows the host as on and healthy. The
  last `BOOTY1` serial status line and the shipped log end at the handoff.
- BMC and Redfish state: `PowerState` stays `On`. `BootProgress` and POST
  codes are not updated (verify per vendor). The BMC does not log a reset.
  Out-of-band tools cannot tell that the OS changed.
- CAPRF contract: the success message names the handoff (PR #724). CAPRF
  should power cycle only for `poweroff`, and eject the media without a power
  cycle for `kexec` and `reboot`. A power cycle after `kexec` would cut the
  first boot of the installed OS (cloud-init, Ignition). Older CAPRF builds,
  and the `feat/booty-consolidated` branch as reviewed for this page, run
  `shutdownSystem`, `ejectAllMedia` and `turnOnSystem` after every success
  except A/B preserve-existing, so they still shut the new OS down; the CAPRF
  side of the contract is tracked in [agent-fast-cycle.md](agent-fast-cycle.md).
  Until CAPRF honours `handoff=`, use `DISABLE_KEXEC=true` or
  `SUCCESS_ACTION=poweroff` with such CAPRF builds.
- Virtual media: after kexec, the BOOTy ISO can still be inserted until CAPRF
  ejects it. The one-time boot override was consumed by the BOOTy boot. If the
  virtual CD is early in `BootOrder`, a reboot of the installed OS before the
  eject boots BOOTy again (verify per vendor).
- Standby agent fast cycle: in `MODE=standby`, a `provision` command ends with
  the same handoff. Kexec saves one POST per provision, the largest single
  saving of the fast cycle ([agent-fast-cycle.md](agent-fast-cycle.md),
  item 4.7).
- Watchdog (PR #721, also the IPMI fallback; PR #816 moved it to
  `/dev/ipmi0` and dropped `ipmitool`): with `WATCHDOG_ENABLE=true`,
  BOOTy disarms the watchdog before kexec (`WATCHDOG_HANDOFF=stop`, default)
  or leaves it armed (`keep`). `stop` is the magic close for `/dev/watchdog`
  and an IPMI stop command for the BMC watchdog of the IPMI fallback
  (`pkg/watchdog/bmc_linux.go`). With `keep`, the target must pet it before
  the timeout, which counts from the handoff and includes the kexec and the
  target boot. Drivers that stop the watchdog on reboot
  (`watchdog_stop_on_reboot`) also disarm it at kexec. The BMC watchdog of
  the IPMI fallback stays armed only with `keep`; the target then needs an
  IPMI watchdog client that pets the BMC watchdog. See
  [caprf-integration.md](caprf-integration.md).
- Kernel panic (PR #765): BOOTy sets `kernel.panic=10` only in the ramdisk
  kernel (`main_panic.go`), unless the BOOTy command line sets `panic=`. The
  value does not carry over: the target kernel uses its own default unless
  its command line sets `panic=`. A panic of the BOOTy kernel during the
  jump still reboots the host after 10 s.
- Time and RTC (PR #713): BOOTy syncs the clock before the first TLS check and
  writes the RTC in UTC (`TIME_WRITE_RTC`). The kexec'd kernel reads the RTC
  again, so it starts with the corrected time; kernel time itself does not
  carry over. With `TIME_SYNC=off`, a wrong RTC stays wrong.
- Logs and metrics from the jump itself are lost if the target never comes
  up; there is no firmware-level record.

## 11. Architecture specifics (arm64)

- `kexec_file_load` on arm64 accepts the `Image` format; support for
  compressed or EFI zboot images depends on the kernel version (verify).
- The device tree or ACPI tables come from the BOOTy boot; on ACPI servers
  the same considerations as x86 apply. On device tree systems,
  `kexec_file_load` passes the running DTB with updated `/chosen` (verify).
- EFI runtime services on arm64 use a fixed mapping created once per boot
  (verify the handover).
- BOOTy selects `BOOTAA64.EFI` and `arm64-efi` by `runtime.GOARCH`
  (`pkg/provision/configurator.go`); the kexec path itself has no
  architecture-specific code. No arm64 kexec scenario runs in CI.

## 12. Decision table: kexec or reboot

| Situation | Use | Why |
|-----------|-----|-----|
| Standby agent fast cycle, no firmware or security changes | kexec | Saves POST time. |
| A/B preserve-existing upgrade (today) | kexec (required by BOOTy) | Firmware still points at the old slot; see [ab-partitioning.md](ab-partitioning.md). |
| TPM-sealed disk keys, remote attestation, measured-boot policies | reboot | Section 3. |
| Firmware settings changed (NIC NV config, SR-IOV firmware, disk firmware activation) | reboot | Section 2. BOOTy enforces it only for its own changes: the Mellanox `NUM_VFS` path and SR-IOV profiles that store firmware values. |
| BIOS attributes changed or pending | reboot | Section 2. BOOTy cannot detect pending Redfish settings. |
| Secure Boot on during BOOTy, target not signed by a key in BOOTy's keyrings (for example Ubuntu) | reboot (automatic, load fails) | Section 4. |
| `SECUREBOOT_REENABLE=true` | reboot (enforced) | Section 4. |
| Targets with BLS, systemd-boot, UKI, or a `grub.cfg` shape outside the allow-list | reboot (automatic, `DefaultEntry` refuses) | Section 1. |
| Flatcar | reboot (automatic, refused) | Section 5. |
| A/B trial boots that rely on boot counting or `BootNext` | reboot | Section 5. |
| GPUs or controllers with known kexec problems | reboot | Section 8. |
| CAPRF build that power cycles after every success (ignores `handoff=`) | power off (`SUCCESS_ACTION=poweroff`) or `DISABLE_KEXEC=true` | Section 10. |

Knobs and recommended defaults:

| Key | Today | Recommendation |
|-----|-------|----------------|
| `DISABLE_KEXEC` | `false` | Keep `false` with a CAPRF that honours `handoff=`; set `true` with older CAPRF builds and for attested or Secure Boot strict hosts. Must stay `false` for A/B preserve-existing. |
| `SUCCESS_ACTION` | `reboot` (`poweroff` with legacy compat) | `poweroff` when CAPRF owns the power cycle. |
| `KEXEC_UNMOUNT_TARGET` | `true` | Keep `true`. |
| `SECUREBOOT_REENABLE` | `false` | `true` for Secure Boot hosts; it disables kexec. |
| `WATCHDOG_HANDOFF` | `stop` | Keep `stop` unless the target OS takes the watchdog over in time. |
| `LOG_FLUSH_TIMEOUT_SEC` | `5` | Keep; raise on slow log paths. |

## 13. Test coverage and gaps

| Test | What it covers | Reference |
|------|----------------|-----------|
| `TestKexecSmokeQEMU` (KVM scenario `kexec`) | Provisions a target whose `grub.cfg` has a marker argument, lets BOOTy kexec into it, and checks from the second kernel that it booted with the target command line. Runs under SeaBIOS and OVMF; under OVMF it checks that the target sees the EFI variables (PR #725). | `test/e2e/kvm/kexec_test.go`, `.github/workflows/kvm-matrix.yml` |
| `TestKexecHandoffPersistsTargetWritesQEMU` (scenario `kexec-persist`) | Writes made just before the handoff reach the disk; the journal is clean. | `test/e2e/kvm/kexec_persist_test.go` |
| `TestKexecLoadAndExecute` | BOOTy attempts kexec after a provision; accepts a failed attempt in QEMU. | `test/e2e/kvm/provision_gaps_test.go` |
| `pkg/kexec` unit tests | `DefaultEntry` with real fixtures (Ubuntu 22.04 cloud image, Ubuntu 24.04 GCP with the initrdless fallback, CentOS Stream 9 BLS, Flatcar) and one test per refusal. | `pkg/kexec/default_test.go`, `pkg/kexec/testdata/` |
| Target release | Unmount or read-only fallback before kexec. | `main_kexec_release_test.go`, `pkg/disk/kexec_release_test.go` |
| Handoff message | `PlannedHandoff` decisions. | `pkg/provision` tests (PR #724) |
| Network stop before the handoff | `doKexecExecute` runs the log flush, then the network stop, then the kexec (`TestDoKexecExecuteFlushesThenStopsNetwork`); `tryKexec` stops the network after the load; one teardown, bounded wait; the stop before reboot and power off. | `main_network_stop_test.go` (PR #723, PR #810) |
| LLDP shutdown at the handoff | `doKexecExecute`, `reboot`, `requestReboot` and `requestPowerOff` call `stopLLDPEmit` (AST check; not the position of the call). | `main_test.go` (`TestHandoffsStopLLDPEmit`) |
| A/B preserve-existing KVM scenarios | Upgrade paths that end in kexec. | `test/e2e/kvm/ab_sysext_test.go` |
| Fast cycle timing | Deprovision and provision cycle time on every PR. | PR #715 |

Gaps:

- Secure Boot with lockdown is measured (PR #737, README
  [Secure Boot](../README.md#secure-boot)) but kexec under lockdown is not a
  KVM regression scenario on `main`.
- No test with a TPM that checks PCR values or sealed unlock after kexec.
- No test of the announced `handoff=kexec` followed by a failed load (the
  message and the actual handoff differ).
- No test with SR-IOV VFs present at kexec (QEMU `igb` with VFs could cover
  part of it, PR #728).
- No test of the BGP and BFD behaviour at the handoff with a real peer
  (#723 has unit tests; the CAPRF kindmetal EVPN lab is the planned place).
- No test of the watchdog with `WATCHDOG_HANDOFF=keep` across kexec.
- No test pins the full order in `doKexecExecute`: where `watchdogHandoff`
  and `stopLLDPEmit` run relative to the log flush and the network stop.
- No arm64 kexec test.
- No test on real hardware of the EFI runtime handover, GPUs, RAID
  controllers or IOMMU state after kexec.

## Related pages and proposals

- [caprf-integration.md](caprf-integration.md): end of a run and handoff.
- [ab-partitioning.md](ab-partitioning.md): why A/B preserve-existing needs
  kexec.
- [configuration.md](configuration.md): `DISABLE_KEXEC`,
  `KEXEC_UNMOUNT_TARGET`, `SUCCESS_ACTION`.
- [proposal-kexec-enhanced.md](proposal-kexec-enhanced.md): kernel selection
  and command line management.
- [proposal-agent-mode.md](proposal-agent-mode.md): the standby fast cycle.
- [proposal-tpm-attestation.md](proposal-tpm-attestation.md),
  [proposal-secureboot.md](proposal-secureboot.md),
  [proposal-bootloaders.md](proposal-bootloaders.md).
