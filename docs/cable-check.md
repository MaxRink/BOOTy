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
| `CHECK_PROFILES` | `health,cabling` | Also run the host-side cabling checks; their result goes to the log (serial console). |

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

## Host-side checks

With `CHECK_PROFILES=health,cabling` (set in the check ISO) BOOTy also
checks the cabling from the host side, after `LLDP_NEIGHBOR_WAIT`:

| Check | Fails when | Default action |
|---|---|---|
| `lldp-neighbor-present` | a NIC with carrier has no LLDP neighbor | warn |
| `cabling-match` | a NIC is on another switch port than `CHECK_EXPECTED_CABLING` says, a NIC with a neighbor is not in it, or an expected NIC is missing (skipped without an expected cabling) | block |
| `uplink-dual-homed` | all NICs with a neighbor reach the same switch (chassis ID) | warn |
| `link-speed` | a NIC with carrier runs below `minSpeedMbps` or not at its expected `speedMbps` | warn |
| `link-flaps` | a NIC lost carrier more than `maxFlaps` times (default 4: a boot bounce, the link setup and the firmware LLDP agent switch of ice and i40e each reset the port) since the driver loaded (`carrier_down_count`) | warn |
| `link-errors` | a NIC has more than `maxErrors` (default 0) receive and transmit errors since the driver loaded | warn |
| `fec` | a NIC has uncorrectable FEC blocks, or runs without FEC on lanes that need it: PAM4 lanes (50G and more per lane) always, 25G lanes (25G, 50G-R2, 100G-R4 such as SR4 and CR4) unless the module is a long-reach optic (LR, LR4, ER, ER4), a CA-N cable or a BER 1e-12 AOC/ACC (SFF-8024 compliance code). 10G lanes (10G, 40G) need no FEC | warn |
| `pcie-link` | the PCIe link of a NIC runs below the speed or width that both the device and its slot (the upstream port) can (downtrained) | warn |
| `optics-alarm` | a transceiver raised an alarm flag (temperature, supply voltage, Tx bias, Tx or Rx power, per lane) | block |
| `optics-warning` | a transceiver raised a warning flag | warn |

BOOTy reads the link state over ethtool netlink (`LINKMODES`, `LINKSTATE`,
`FEC` with statistics), the error counters from the netdev statistics and
the carrier losses and the PCIe link from sysfs. The lane count comes from
the driver (`LINKMODES` lanes), else from the speed (40G, 100G and 200G
have 4 lanes, 50G 2, 400G 8). Values that a driver does
not report stay out of the document: virtio has no FEC, a veth no PCIe
link. The counters are totals since the driver loaded, not rates.

### Transceivers

BOOTy reads the transceiver memory over ethtool netlink
(`MODULE_EEPROM_GET`, page by page; it needs `CAP_NET_ADMIN`, which PID 1
has) and decodes it itself (`pkg/network/sff`), so neither the `ethtool`
binary nor a library is needed:

| Spec | Modules | Read |
|---|---|---|
| SFF-8472 | SFP, SFP+, SFP28 | A0h (identity), A2h (diagnostics, thresholds, flags) |
| SFF-8636 | QSFP+, QSFP28 | lower page, page 00h, page 03h (thresholds) unless the memory is flat |
| CMIS 4/5 | QSFP-DD, OSFP, QSFP with CMIS | lower page, pages 00h, 01h, 02h, 11h unless the memory is flat |

Each NIC with a module gets `module` in the result document: type,
connector, vendor, OUI, part number, revision, serial number, date code,
wavelength, and with diagnostics the temperature, supply voltage, per lane
Tx bias, Tx and Rx power (dBm), the alarm and warning thresholds and the
raised flags. The checks use the flags that the module computes itself;
SFF-8636 and CMIS flags are latched, so they also show an event since the
last read. Limits of the decoder: SFF-8472 modules with external
calibration get a note instead of diagnostics, and the CMIS lane count
comes from the form factor (8 for QSFP-DD and OSFP, 4 for QSFP with
CMIS). The decoder is tested with memories built from the specifications;
no real module dumps were available.

The switch port of a neighbor is its port ID when that is a name (subtype
`ifname`, `interface-alias`, or a `local` ID that is not a number);
otherwise, such as for the Junos ifIndex or the lldpd MAC port ID, it is
the port description. Switch names compare without case.

### Thresholds

`CHECK_THRESHOLDS` (YAML `check.thresholds`) sets the action of each check:
`off` skips it, `warn` reports a failure as a warning, `block` as critical,
which makes the run fail (status `error`).

| Key | Check |
|---|---|
| `lldpMissing` | `lldp-neighbor-present` |
| `cablingMismatch` | `cabling-match` |
| `dualHoming` | `uplink-dual-homed` |
| `linkSpeed`, `minSpeedMbps` | `link-speed` |
| `linkFlaps`, `maxFlaps` | `link-flaps` |
| `linkErrors`, `maxErrors` | `link-errors` |
| `fec` | `fec` |
| `pcie` | `pcie-link` |
| `opticsAlarm` | `optics-alarm` |
| `opticsWarning` | `optics-warning` |

Example: `CHECK_THRESHOLDS='{"lldpMissing":"block","dualHoming":"off"}'`.

### Result document and sinks

One result document (`schema: booty.check/v1`) holds the host, the status
(`pass`, `warn` or `fail`), every check result (health and cabling), every
NIC with its LLDP neighbor and expected port, and the diff. The log always
gets one line per check and a summary; `CHECK_RESULT_SINKS` adds `file`,
`http`, `caprf` and `bm4x-report`. A failing sink is logged and the others
still run.

The `bm4x-report` sink writes the diff in the form of the bm4x
`ConnectionReport`:

```json
{"diff":[{"a":{"hostname":"site1com5k01","interfaceName":"ens1f1np1"},
  "bactual":{"hostname":"site1lsw5k11","interfaceName":"et-0/0/4"},
  "bexpected":{"hostname":"site1lsw5k11","interfaceName":"et-0/0/3"}}]}
```

`a` is the host NIC, `bactual` the switch port that LLDP shows and
`bexpected` the expected one. `deviceName` and `deviceStatus`, which only
NetBox knows, are left out. The names `bactual` and `bexpected` are what
Jackson writes for the Lombok getters `getBActual` and `getBExpected` of
`ConnectionEntry`; verify them against a real bm4x job output before bm4x
reads this report.

## Host identity: what goes where

Two decisions keep the machine serial number out of the default data:

| Data | Default check result and inventory | LLDP `parity`, `standard` | LLDP `full` (opt-in) |
|---|---|---|---|
| Hostname | yes (`host.hostname`, diff `a.hostname`) | as the system name (`networkd` profile) | the same |
| Product, DMI UUID | yes (`host.product`, `host.uuid`) | no | in the system description |
| DMI serial number | no | no | LLDP-MED inventory |

- The check result document (`linkcheck.Host`) and the hardware inventory
  leave the serial number out. CAPRF and bm4x know it already, and the
  documents go to more receivers (logs, sinks) than the switch.
- `LLDP_ANNOUNCE=full` sends the serial number, the product and the UUID
  to the switch, for operators on the switch CLI and in the LLDP-MIB. It is
  opt-in; `parity` (the default) and `standard` send none of them. See
  [LLDP announce levels](networking.md#lldp-announce-levels).

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
  leaf reports for BOOTy (`TestCableCheckLLDP`). Its vars are the ISO vars
  plus an expected cabling with one wrong port; `TestCableCheckDiff` checks
  the bm4x report and the result document.
