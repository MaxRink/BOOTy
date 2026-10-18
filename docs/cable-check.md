# Cable check

BOOTy can run the host side of the bm4x cable check (the bm4x-api
`CONNECTION_CHECK` job). bm4x checks the cabling from the switch side: it
boots an LLDP ISO on every STAGED compute through Redfish virtual media,
asks NSO for the LLDP neighbor tables of the switches and compares them
with the cables in NetBox. The compute side only has to send LLDP frames
that bm4x recognizes. BOOTy sends those frames, so bm4x only has to change
the ISO URL (`compute-app.image.lldp.url`).

## The generic check ISO

`make check-iso` builds `booty-check.iso`: the GoBGP flavor with
[`hack/check-iso/vars`](../hack/check-iso/vars) as `/deploy/vars`. The ISO
has no host-specific data, so one ISO serves every host. `hack/make-iso.sh`
packs it like the other BOOTy ISOs, so it boots with BIOS (ISOLINUX) and
with UEFI, also with Secure Boot on: its EFI system partition has the
Debian-signed shim (signed by the Microsoft UEFI CA) and GRUB, and the
kernel is the Debian-signed one. GRUB uses the medium it was started from,
never another disk with the same label. Redfish virtual media on
UEFI-only servers therefore boots it, and the "disable Secure Boot" step
that the bm4x LLDP ISO needs can go. The initramfs itself is not signed;
shim and GRUB do not verify it.

| Key | Value | Effect |
|---|---|---|
| `MODE` | `check` | Run the checks, not provisioning. |
| `NETWORK_MODE` | `links` | Every physical NIC up with IPv6 link-local only, no DHCP, no BGP ([links mode](networking.md#links-mode)). |
| `LLDP_PROFILE` | `bm4x-cablecheck` | Port ID = NIC MAC, system name `bare-minimal`, on every NIC with carrier ([identity profiles](networking.md#lldp-identity-profiles)). |
| `CHECK_HOLD` | `until-poweroff` | Keep the links and LLDP up after the checks until bm4x powers the host off (`STOP_LLDP`). |
| `LLDP_NEIGHBOR_WAIT` | `90s` | Before the checks, wait until every NIC with carrier has an LLDP neighbor. |

BOOTy also stops the firmware LLDP agent of Intel NICs, as in every mode
(`LLDP_DISABLE_FW_AGENT`, on by default), so the switch frames reach BOOTy
and the switch sees the host. The agent stays off after the check: BOOTy
does not put the flag back, and ice keeps it in the NVM (see
[networking](networking.md#bgp-capabilities-and-lldp)). The check ISO vars
do not set the key; a site that needs the firmware agent (firmware DCBX)
builds the ISO with `LLDP_DISABLE_FW_AGENT=false`.

The flow with bm4x is unchanged: `START_LLDP` inserts the ISO and powers the
host on, NSO collects the neighbor tables, and `STOP_LLDP` powers the host
off and ejects the ISO. The kernel sends the first frame on each NIC within
seconds of BOOTy starting and then one every 30 s (TTL 120 s), like
systemd-networkd and lldpd. bm4x waits `redfish.image-boot` (60 s in
production) after the boot.

## Hold

`CHECK_HOLD` (YAML `check.hold`) controls what check mode does after the
checks:

| Value | Effect |
|---|---|
| `off` (default) | Report the result, tear the network down and reboot, as before. |
| `until-poweroff` | Report the result and keep running until the machine is powered off. |
| a duration, for example `10m` | Report the result, keep running that long, then reboot. |

## Tests

- KVM `check-iso-hold` (`TestCheckISOVarsHoldQEMU`) boots the vars of the ISO
  with two NICs. After 130 s (the 90 s neighbor wait runs out, as the VM
  has no switch) the VM still runs, and each NIC has sent the
  four fast LLDP frames and the next one 30 s later with the bm4x identity,
  and no IPv4 frame.
- `TestCheckISOQEMU`, `TestCheckISOUEFIQEMU` and
  `TestCheckISOSecureBootQEMU` boot a built `booty-check.iso` from CD
  (`BOOTY_CHECK_ISO=booty-check.iso`) with BIOS, with UEFI (OVMF) and with
  Secure Boot on (OVMF with the Microsoft keys; the kernel must report
  Secure Boot), with the same checks. `TestCheckISOIgnoresOtherBootyDiskQEMU`
  adds a decoy disk with the BOOTy label and GRUB configs (as in the ISO
  tests) and checks that the cable check still runs from the CD. The CI job
  `check-iso` builds the ISO with `make check-iso` and runs all four.
- The containerlab lab `topology-cablecheck.clab.yml` shows what an lldpd
  leaf reports for BOOTy (`TestCableCheckLLDP`).
