# Full systemd-boot Support

## Status

Proposal. Design only. Nothing in this document is implemented. This
proposal replaces the systemd-boot parts of
[proposal-bootloaders.md](proposal-bootloaders.md): that document describes a
`pkg/bootloader` package that was removed from `main` in #463.

## Priority

P1

## Summary

Make systemd-boot a first-class target bootloader next to GRUB. BOOTy must
detect, install and update systemd-boot, write and read Boot Loader
Specification (BLS) Type #1 entries and Type #2 UKIs, manage `loader.conf`
and the loader EFI variables, use boot counting for A/B trial boots, kexec
from BLS entries and UKIs (or reboot when measured boot requires it), and
keep Secure Boot and measured boot intact. A `BOOTLOADER` key selects the
behaviour, with `auto` detection as the default.

## Problem and motivation

- Every boot path in BOOTy assumes GRUB:
  - `configure-grub` writes `/etc/default/grub.d/10-caprf-kernel-params.cfg`
    and runs `update-grub` (`pkg/config/provision.go` `GRUBCmdlineMode`).
  - kexec reads only `/boot/grub/grub.cfg` (`main.go` `tryKexec`,
    `pkg/kexec/grub.go` `ParseGrubCfg`). Without it, BOOTy reboots.
  - The bundled EFI fallback loader needs the target `/boot/grub/grub.cfg`
    (`pkg/provision/configurator.go` `installBundledEFIFallbackLoader`).
  - EFI entry creation looks for shim and GRUB loaders (`efiLoaderPath`).
  - A/B writes `root=PARTLABEL=BOOTY-ROOT-A|B` into GRUB defaults
    (`docs/ab-partitioning.md`).
- The measured boot plan for A/B works best with UKIs and systemd-stub
  (signed PCR 11 policies). See [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md).
- Boot counting in systemd-boot is a clean, standard rollback mechanism
  for A/B (see [proposal-rollback.md](proposal-rollback.md)).
- Several target OSes ship or offer systemd-boot (see the OS table below).

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| systemd-boot code | None on `main`. `pkg/bootloader` (with a no-op `SystemdBoot.Configure`) was removed in #463. | `git log -- pkg/bootloader` |
| Bootloader detection | None. GRUB is assumed. | `pkg/provision/configurator.go` |
| kexec | GRUB `menuentry` parsing only. | `pkg/kexec/grub.go`, `main.go` (`tryKexec`) |
| EFI entries | `efibootmgr -c` with a shim or GRUB loader path; mirror entry for RAID1. | `pkg/provision/configurator.go` (`efiLoaderPath`), `create-efi-boot-entry` |
| EFI fallback | Bundled removable loader with a GRUB handoff marker. | `installBundledEFIFallbackLoader` |
| A/B | GRUB `root=` pin, shared ESP, no NVRAM entry. | `docs/ab-partitioning.md` |
| Secure Boot verify | Pins for `shim`, `grub`, `kernel`. | `pkg/secureboot/verify.go` |
| KVM | `bootloader-smoke` boots through the UEFI path; no systemd-boot target. | `test/e2e/kvm/bootloader_test.go` |

## Background (verify each item against the UAPI and systemd documentation)

- **Type #1 entries**: `$BOOT/loader/entries/*.conf` with `title`,
  `version`, `machine-id`, `sort-key`, `linux`, `initrd` (repeatable),
  `options`, `devicetree`, `architecture`.
- **Type #2 entries**: UKIs in `$BOOT/EFI/Linux/*.efi`. PE sections `.linux`,
  `.initrd`, `.cmdline`, `.osrel`, `.uname`, `.sbat`, `.pcrsig`,
  `.pcrpkey`, and multi-profile `.profile` sections in newer systemd.
- **`$BOOT`**: the XBOOTLDR partition if present, otherwise the ESP. The
  boot loader also reads entries from the ESP.
- **`loader.conf`**: `default`, `timeout`, `console-mode`, `editor`,
  `auto-entries`, `auto-firmware`, `secure-boot-enroll`.
- **Loader variables** (vendor GUID `4a67b082-0a4c-41cf-b6c7-440b29bb8c4f`):
  `LoaderEntryDefault`, `LoaderEntryOneShot`, `LoaderEntrySelected`,
  `LoaderConfigTimeout`, `LoaderFeatures`, `LoaderDevicePartUUID`.
- **Boot counting**: an entry file name `name+LEFT-DONE.conf` (or `.efi`).
  systemd-boot decrements LEFT on each try by renaming the file. An entry
  with 0 tries left sorts last. `systemd-bless-boot.service` removes the
  counter after `boot-complete.target`.
- **Discoverable Partitions Specification**: GPT type GUIDs for ESP
  (`c12a7328-f81f-11d2-ba4b-00a0c93ec93b`), XBOOTLDR
  (`bc13c2ff-59e6-4262-a352-b275fd6f7172`), root x86-64
  (`4f68bce3-e8cd-4db1-96e7-fbcaf984b709`), root arm64
  (`b921b045-1df0-41c3-af44-4c6f280d3fae`), `/var`
  (`4d21b016-b534-45c2-a9fb-5c16e091fd2d`), `/home`
  (`933ac7e1-2eb4-4f13-b844-0e14e2aef915`), plus `/usr` and verity types.
  `systemd-gpt-auto-generator` mounts them without `fstab`.
- **kernel-install**: `layout=bls` (Type #1 under `$BOOT/<machine-id>/<version>/`),
  `layout=uki` (UKI in `EFI/Linux`), `layout=other`.
- **ESP size**: UKIs and multiple kernels need space. systemd documentation
  recommends 1 GiB or more for the ESP or XBOOTLDR (verify). BOOTy's A/B
  default `AB_BOOT_SIZE_MB` is 512 MiB.

## OS families and systemd-boot (verify each)

| Family | Default | systemd-boot | Notes |
|--------|---------|--------------|-------|
| Fedora | GRUB with BLS | Optional (`sdubby`, kernel-install) | BLS entries already exist for GRUB. |
| RHEL, Rocky, Alma | GRUB with BLS | Not supported by the vendor | BLS kexec still useful. |
| Ubuntu 24.04+ | GRUB | `systemd-boot` package | Signed systemd-boot status: verify. |
| Debian 12+ | GRUB | `systemd-boot` package, signed variant in newer releases (verify) | |
| Arch | none | Common choice | Not a product target. |
| NixOS | depends on config | `boot.loader.systemd-boot.enable` is common on UEFI | Generations as entries. |
| Flatcar | GRUB with gptprio | Not used (verify upstream plans) | See [proposal-flatcar.md](proposal-flatcar.md). |
| Talos | systemd-boot with UKIs on UEFI in newer releases (verify) | Talos owns it | See [proposal-talos.md](proposal-talos.md). |
| SLES 16, openSUSE | GRUB; systemd-boot optional (verify) | | |

## Goals

- Detect the target bootloader and follow it.
- Write and parse Type #1 entries and UKIs.
- Install or update systemd-boot from the ramdisk, with or without `bootctl`.
- Manage `loader.conf`, `LoaderEntryDefault` and `LoaderEntryOneShot`.
- Use boot counting for A/B trial boots and rollback.
- kexec from BLS entries and UKIs, or reboot where measured boot needs it.
- Keep the Secure Boot chain and the measured boot plan.

## Non-goals

- Building UKIs. The OS image pipeline builds and signs them (`ukify`,
  `mkosi`).
- Supporting rEFInd or other loaders.

## Design

### Detection (`BOOTLOADER=auto`)

1. A UKI in `EFI/Linux/*.efi` on the ESP or XBOOTLDR and no Type #1 entry:
   `uki`.
2. `EFI/systemd/systemd-boot<arch>.efi` and `loader/entries/`: `systemd-boot`.
3. `/boot/grub/grub.cfg` or `/boot/grub2/grub.cfg`, with or without BLS:
   `grub` (`grub-bls` when `blscfg` is used).
4. Otherwise: `none` (reboot handoff, firmware decides).

### Writing the target configuration

| Concern | GRUB today | systemd-boot |
|---------|-----------|--------------|
| Kernel parameters | GRUB drop-in plus `update-grub` | Type #1: edit `options` in BOOTy-owned entries, or `/etc/kernel/cmdline` plus kernel-install in a chroot. UKI: parameters are inside the signed UKI; use a signed cmdline add-on (`*.addon.efi`, verify) or bake them into the image. |
| Serial console | GRUB serial terminal | `console-mode` and `console=` in `options` |
| Root selection (A/B) | `root=PARTLABEL=...` in GRUB defaults | `root=PARTLABEL=...` in each entry's `options`, or a UKI per slot; or discoverable partitions with `systemd-gpt-auto-generator` and a slot-specific root type label (verify for A/B) |
| Default | `GRUB_DEFAULT` | `LoaderEntryDefault` variable, or `default` in `loader.conf` |
| One-shot | `grub-reboot` | `LoaderEntryOneShot` variable |

### Installing or updating systemd-boot

- With `bootctl` in the target chroot: `bootctl install --esp-path=/boot/efi`
  (or `update`), which copies `systemd-boot<arch>.efi` to
  `EFI/systemd/` and `EFI/BOOT/BOOT<ARCH>.EFI`, creates `loader/` and the
  random seed, and adds a "Linux Boot Manager" EFI entry.
- Without `bootctl`: BOOTy copies the signed binary from the target root
  (`/usr/lib/systemd/boot/efi/systemd-boot<arch>.efi[.signed]`), writes
  `loader/loader.conf`, creates the EFI entry with `pkg/efi`, and never
  copies an unsigned binary when Secure Boot is on.
- Version check: compare the `.sdmagic` or `.osrel` section version of the
  installed and the new binary (verify the section name) and update only on
  a newer version.

### kexec from BLS entries and UKIs

- Type #1: parse the entry, resolve `linux`, `initrd` and `options` against
  `$BOOT`, call `kexec_file_load`.
- UKI: read the PE file with `debug/pe`, extract `.linux`, `.initrd` and
  `.cmdline` into tmpfs, then `kexec_file_load`. Multi-profile UKIs: select
  the default profile (verify the selection rules).
- Under Secure Boot with kernel lockdown, `kexec_file_load` needs a kernel
  that the kernel keyring trusts. The `.linux` section of a signed UKI is
  often not signed itself (verify). Then kexec fails and BOOTy reboots.
- With measured boot (`TPM_MODE=require`), always reboot: kexec skips
  systemd-stub, so PCR 11, 12 and 13 do not get the UKI measurements.

### Secure Boot

- Two chains:
  - **shim chain**: firmware trusts the Microsoft UEFI CA, shim (vendor
    signed) loads systemd-boot signed by a key in the shim vendor cert or
    MOK. shim checks SBAT.
  - **direct db**: the site key is in `db`. systemd-boot and UKIs are signed
    with it. No shim. Needs key enrollment (see
    [proposal-uefi-management.md](proposal-uefi-management.md)).
- systemd-boot and systemd-stub carry `.sbat` sections. Revocation by SBAT
  generation works with shim (verify for direct db boots).
- Extend `SECUREBOOT_PINNED_DIGESTS` with `systemd-boot` and `uki` keys, and
  make the chain verifier find them.

### Measured boot

- systemd-stub measures UKI sections into PCR 11, the command line and
  credentials into PCR 12, and sysexts and confexts from `extra.d` into PCR
  13. `systemd-pcrphase` extends PCR 11 at boot phases.
- Signed PCR 11 policies (`.pcrsig`) let LUKS unlock any signed UKI, which is
  the A/B recommendation in [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md).
- `systemd-pcrlock` covers Type #1 entries and GRUB targets.

### A/B with systemd-boot

- One entry per slot: `booty-a+3-0.conf` and `booty-b.conf`, or UKIs
  `booty-a.efi` and `booty-b+3-0.efi`.
- Upgrade: write the inactive slot, write its entry with `+3`, set
  `LoaderEntryOneShot` (or `LoaderEntryDefault`) to the new entry, reboot.
- Success: `systemd-bless-boot` removes the counter; BOOTy's `booty-ab` tool
  (see [proposal-rollback.md](proposal-rollback.md)) updates `ab-slot.env`
  and `LoaderEntryDefault`.
- Failure: the tries run out; systemd-boot picks the old slot.
- This replaces the kexec requirement of `AB_PRESERVE_EXISTING` for
  systemd-boot targets.

### XBOOTLDR and partition types

- Use XBOOTLDR when the image has one, and offer it in `PARTITION_LAYOUT`
  and the A/B schemes (`AB_XBOOTLDR_SIZE_MB`).
- Set discoverable GPT type GUIDs in BOOTy-generated layouts (root per
  architecture, `/var`, `/home`), so the target can mount without `fstab`.
  BOOTy keeps writing `fstab` for compatibility.
- Check the ESP size before the write: fail when it is smaller than the sum
  of the UKIs to install plus headroom.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `BOOTLOADER` | `auto`, `grub`, `systemd-boot`, `uki`, `none` | Default `auto`. Shared with [proposal-os-targets.md](proposal-os-targets.md). |
| `SDBOOT_INSTALL` | `keep`, `install`, `update` | Default `keep`: use what the image has. |
| `SDBOOT_TIMEOUT` | integer seconds | `loader.conf` `timeout`. |
| `SDBOOT_DEFAULT` | entry ID or glob | Sets `LoaderEntryDefault`. |
| `SDBOOT_BOOT_TRIES` | integer, default `3` | Counter for A/B trial entries. |
| `UKI_CMDLINE_ADDON` | ramdisk path | Signed cmdline add-on for UKI targets. |
| `AB_XBOOTLDR_SIZE_MB` | integer, `0` disables | XBOOTLDR in A/B layouts. |
| `HANDOFF` | `auto`, `kexec`, `reboot` | From the OS targets and measured boot proposals. |
| `SECUREBOOT_PINNED_DIGESTS` | add `systemd-boot`, `uki` | Existing key. |

### CAPRF and CAPI API impact

- `RedfishMachine.spec.bootloader` (optional, default `auto`).
- `RedfishMachine.status.ab` reports the entry IDs and the boot counter
  state.
- BOOTy itself can boot as a UKI (see
  [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)); that is
  independent of the target bootloader.

## Security considerations

- Never install an unsigned systemd-boot when the target has Secure Boot on.
- `editor no` in `loader.conf` for production, so nobody edits the command
  line at the console. The UKI command line is fixed anyway.
- `LoaderEntryOneShot` and `LoaderEntryDefault` are writable by root in the
  OS. They select among trusted entries only; Secure Boot still verifies.
- Extracted UKI sections in tmpfs must be treated as untrusted until
  `kexec_file_load` verifies them.

## Testing strategy

- Unit: Type #1 parser and writer (fuzzed), UKI PE section extraction with a
  fixture UKI built in the test by `ukify` when available, boot counter file
  name logic, loader variable encoding (UTF-16LE), detection order.
- KVM matrix:
  - `sdboot-type1`: Debian or Fedora image with systemd-boot and Type #1
    entries; BOOTy sets parameters and kexecs.
  - `sdboot-uki`: UKI-only image; BOOTy reboots (or kexecs without Secure
    Boot).
  - `sdboot-secureboot`: OVMF with custom keys, signed systemd-boot and UKI.
  - `sdboot-ab-counting`: A/B upgrade with a broken new UKI; the counter runs
    out; the old slot boots.
  - `sdboot-xbootldr`: entries on XBOOTLDR.
- ContainerLab: none.
- kindmetal and CAPRF e2e: one systemd-boot UKI target.
- Real hardware: `LoaderEntryOneShot` and EFI entry persistence per vendor.

## Phased rollout

1. PR: bootloader detection and `BOOTLOADER` key (report only).
2. PR: Type #1 parser and kexec from BLS (shared with BLS for GRUB in
   [proposal-os-targets.md](proposal-os-targets.md)).
3. PR: kernel parameter writing for Type #1 targets.
4. PR: UKI section extraction and kexec or reboot decision.
5. PR: systemd-boot install and update without `bootctl`.
6. PR: loader variables and `loader.conf`.
7. PR: A/B with boot counting.
8. PR: XBOOTLDR and discoverable partition types.
9. PR: Secure Boot pins for systemd-boot and UKIs.
10. KVM scenarios with each phase.

## Effort estimate

| Phase | Days |
|-------|------|
| Detection | 2 |
| BLS Type #1 parse and kexec | 3 to 4 |
| Parameter writing | 3 to 4 |
| UKI extraction and handoff rules | 3 to 4 |
| Install and update | 4 to 6 |
| Loader variables | 2 to 3 |
| A/B boot counting | 5 to 7 |
| XBOOTLDR and GPT types | 3 to 4 |
| Secure Boot pins | 2 |
| KVM scenarios | 6 to 9 |
| **Total** | **33 to 45** |

## Open questions

- Which target OSes in the product use systemd-boot today, and which will
  move to UKIs?
- Do we sign systemd-boot ourselves (direct db) or rely on distribution
  signatures through shim?
- Is 512 MiB enough for the A/B ESP with UKIs, or must the default grow?

## kexec limitations

The kexec handoff skips the firmware, so boot counting, `BootNext`, measured
boot, Secure Boot verification by shim and pending firmware changes do not
apply to the kexec'd kernel. The full list, with a kexec or reboot decision
table, is in `docs/kexec.md` (PR #705).

## Related proposals

- [proposal-bootloaders.md](proposal-bootloaders.md)
- [proposal-os-targets.md](proposal-os-targets.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-rollback.md](proposal-rollback.md)
- [proposal-uefi-management.md](proposal-uefi-management.md)
- [proposal-sysext-confext.md](proposal-sysext-confext.md)
- [proposal-flatcar.md](proposal-flatcar.md)
