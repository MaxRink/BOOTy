# SR-IOV

BOOTy reports the SR-IOV state of every physical NIC function (PF) in the
hardware inventory. This document describes the report and the handoff
contract: what BOOTy guarantees to the Kubernetes side (the SR-IOV network
operator, the DRA SR-IOV driver and the T-CaaS node tuning) when it hands
the host over to the installed OS.

The design is in `docs/proposal-sriov-nic-config.md` of the provisioning
proposals (PR #700, branch `docs/provisioning-proposals`, not merged yet).

## Report

`collect-inventory` (`INVENTORY_ENABLED=true`) adds an `sriov` object to
every NIC in the inventory JSON that is a PF port. A PF port is a netdev
with a parent device that is not a VF (no `physfn` link) and not a VF or SF
representor (`phys_port_name` such as `pf0vf3` or `c1pf0sf2`). BOOTy reads
only kernel interfaces: sysfs, devlink (generic netlink), rtnetlink and the
ethtool ioctl. It needs no vendor tool, so the report works for every NIC
family.

| Field | Source | Notes |
|-------|--------|-------|
| `interface`, `mac` | `/sys/class/net/<if>`, `address` | |
| `bus`, `pciAddress` | `device/subsystem`, `device` links | The devlink handle is `<bus>/<pciAddress>`. `bus` is `pci` on hardware. |
| `vendorId`, `deviceId` | `device/vendor`, `device/device` | Lower-case hex without `0x`, for example `15b3` and `101d`. |
| `driver` | `device/driver` link | `mlx5_core`, `ice`, `i40e`, `ixgbe`, `bnxt_en`, `qede`, `cxgb4`, `sfc`, ... |
| `firmware` | ethtool `ETHTOOL_GDRVINFO`, else devlink running `fw`, `fw.version` or `fw.mgmt` | |
| `numaNode` | `device/numa_node` | `-1` when the platform has no NUMA information. |
| `linkType` | `type` (ARPHRD) | `eth`, `ib`, or `arphrd-<n>`. |
| `sriovCapable` | `sriovTotalVFs > 0` | `false` when SR-IOV is off in the NIC firmware or the BIOS: the device then has no SR-IOV capability. |
| `sriovTotalVFs`, `sriovNumVFs` | `device/sriov_totalvfs`, `device/sriov_numvfs` | Total VFs is the limit after the last firmware reset. |
| `eswitch.capability` | devlink `DEVLINK_CMD_ESWITCH_GET` | `supported` when the driver answers, `unsupported` on `EOPNOTSUPP` or without a devlink instance, `unknown` on other errors. |
| `eswitch.mode` | devlink | `legacy` or `switchdev`. Also `inlineMode` and `encapMode` when the driver sets them. |
| `physSwitchId`, `physPortName` | `phys_switch_id`, `phys_port_name` | mlx5 sets `phys_switch_id` only in switchdev mode. |
| `portCount` | PF ports on the same PCI slot | For example 2 on a dual-port ConnectX. |
| `vfLag`, `vfLagReason` | driver, `portCount`, eSwitch | The kernel has no VF LAG query, see [VF LAG](#vf-lag). |
| `devlink` | devlink `DEVLINK_CMD_INFO_GET` | Handle, driver, serial number and the `fixed`, `running` and `stored` versions. A stored version that differs from the running one means a firmware reset is pending. |
| `vfs` | rtnetlink `IFLA_VFINFO_LIST`, `device/virtfn<N>` | Only when `sriovNumVFs > 0`: index, VF PCI address, MAC, VLAN, QoS, min and max TX rate (Mbit/s), spoof check, trust, link state. |
| `warnings` | | Each failed read. A failed read leaves its field empty; it never fails the inventory. |

Example (a ConnectX-6 Dx port in legacy mode):

```json
{"interface":"ens1f0np0","mac":"b8:ce:f6:00:00:01","bus":"pci",
 "pciAddress":"0000:3b:00.0","vendorId":"15b3","deviceId":"101d",
 "driver":"mlx5_core","firmware":"22.39.1002 (MT_0000000359)","numaNode":0,
 "linkType":"eth","sriovCapable":true,"sriovTotalVFs":64,"sriovNumVFs":0,
 "eswitch":{"capability":"supported","mode":"legacy"},
 "physPortName":"p0","portCount":2,"vfLag":"supported",
 "vfLagReason":"mlx5 VF LAG: bond both PF ports of the adapter (switchdev, or legacy mode with the LAG bond before the VFs)",
 "devlink":{"handle":"pci/0000:3b:00.0","driver":"mlx5_core",
            "running":[{"name":"fw.version","value":"22.39.1002"}]}}
```

### VF LAG

The kernel has no VF LAG capability query. BOOTy reports:

| Driver | `vfLag` |
|--------|---------|
| `mlx5_core` | `supported` with an Ethernet link, two PF ports on the adapter and a devlink eSwitch. `unsupported` otherwise, with the reason. |
| `ice` | `unknown` with two PF ports (SR-IOV LAG depends on the kernel, the NVM and the DDP package), `unsupported` with one. |
| `bnxt_en` | `unknown` with two PF ports and a devlink eSwitch (needs TruFlow firmware), `unsupported` otherwise. |
| Other drivers | `unsupported`. |

## Profiles

A profile is the desired SR-IOV state of one NIC kind in a host class.
CAPRF passes the profiles of the host class in `SRIOV_PROFILES` (a JSON
list) or YAML `provision.sriov.profiles`:

```yaml
provision:
  sriov:
    onDrift: fail          # SRIOV_ON_DRIFT: warn (default), fail, apply
    nicTools: opensource   # NIC_TOOLS: kernel, opensource (default), vendor
    profiles:
      - name: cx6dx
        match: {vendorId: "15b3", deviceId: "101d"}
        firmware:
          sriovEnabled: true
          maxVFs: 64
          linkType: eth
          nvConfig:
            - {name: CQE_COMPRESSION, value: "1"}
        runtime:
          numVFs: 8
          eswitchMode: switchdev
          vfDefaults: {spoofChk: true, trust: false}
          vfs:
            - {index: 0, mac: "02:00:00:00:00:10", vlan: 100, qos: 3, maxTxRate: 10000}
      - name: e810
        match: {driver: ice}
        optional: true
        firmware: {fwLLDPAgent: false}
```

| Field | Meaning |
|-------|---------|
| `name` | 1 to 63 letters, digits, `.`, `_`, `-`; unique. |
| `match` | `vendorId` and `deviceId` (four lower-case hex digits), `driver`, `mac`, `interface` (a name or a shell pattern such as `ens1f*`). Every set field must match; at least one is required. A PF port that matches two profiles fails the run. |
| `firmware.sriovEnabled` | SR-IOV in the NIC firmware. Active value: `sriovCapable`. `true` with `maxVFs: 0` fails validation. |
| `firmware.maxVFs` | VF limit, 0 to 65535. Active value: `sriovTotalVFs`. |
| `firmware.linkType` | `eth` or `ib` (VPI adapters). Active value: `linkType`. |
| `firmware.fwLLDPAgent` | Intel firmware LLDP/DCBX agent; `false` lets the host run LLDP and DCBX. |
| `firmware.nvConfig` | More NVIDIA/Mellanox NV parameters as `{name, value}` with the `mstconfig` names. `SRIOV_EN`, `NUM_OF_VFS` and `LINK_TYPE_P<n>` are rejected: use the fields above. |
| `firmware.devlinkParams` | devlink parameters in the permanent (NIC-stored) mode as `{name, value}`. |
| `runtime.numVFs` | `sriov_numvfs`; not above `firmware.maxVFs`. |
| `runtime.eswitchMode` | `legacy` or `switchdev`. |
| `runtime.vfDefaults`, `runtime.vfs[]` | VF attributes: `trust`, `spoofChk`, `vlan` (0 to 4095), `qos` (0 to 7, needs a VLAN), `minTxRate`, `maxTxRate` (Mbit/s), `linkState` (`auto`, `enable`, `disable`); `vfs[]` also `index` and `mac` (unicast). They need `numVFs` > 0. |
| `runtime.handoff` | `reset` (default) or `keep`. |
| `onDrift` | Overrides `SRIOV_ON_DRIFT` for this profile. |
| `optional` | The profile may match no PF port; otherwise that is drift. |

### Drift check

The `configure-sriov` step runs after `setup-mellanox`. Without profiles it
does nothing. It compares every firmware setting of a profile with two
values:

- the stored value, which a firmware backend reads from the NIC (NV config
  or a permanent devlink parameter), and
- the active value, which the kernel shows (`sriovCapable`,
  `sriovTotalVFs`, `linkType`).

| Status | Meaning |
|--------|---------|
| `ok` | The setting has the wanted value. |
| `drift` | The stored value differs, or the active value differs and there is no stored value. |
| `pending-reset` | The stored value is right, the active one is not: a firmware reset or a power cycle is pending, or the BIOS blocks it (for example SR-IOV off in the BIOS). |
| `unverified` | Neither value is readable, for example no backend for the driver or `NIC_TOOLS` excludes it. |
| `applied` | BOOTy stored the value; it takes effect after the reboot. |
| `failed` | The apply failed or is not possible. |

`onDrift` decides what happens to a profile that is not compliant (any
status other than `ok` and `applied`):

- `warn` logs every finding (`SR-IOV drift`) and goes on.
- `fail` stops provisioning with the findings.
- `apply` stores the drifted settings through the backend of the driver.
  BOOTy then reboots instead of kexec, so the new values are active in the
  installed OS. A `pending-reset` finding is not applied again: a reboot
  already happened before BOOTy ran, so another one would loop. It fails the
  run and asks for a power cycle or a BIOS check.

After the set BOOTy reads the settings back. A setting that the NIC did
not keep fails (`set did not persist`) instead of rebooting into the same
drift again.

### Adapter-wide settings

`sriovEnabled`, `maxVFs` and the `nvConfig` parameters belong to the whole
adapter (all PCI functions of a slot share the ConnectX NV configuration
and the Broadcom NVM). `linkType`, `fwLLDPAgent` and `devlinkParams` belong
to one port.

- Two profiles that want different values for an adapter-wide setting on
  ports of one adapter fail for that setting and nothing is written. Both
  writes would change the value back on every provision, a reboot loop.
- BOOTy writes an adapter-wide setting once. It records every stored
  setting in a journal in the ramdisk (`/tmp/booty-sriov-stored.json`). The
  next port of the adapter, or a second run in the same boot (rescue
  `retry`, a resumed checkpoint), then reads the stored value with the old
  active one and reports `applied` ("stored in this boot") instead of
  `pending-reset`, and reboots. The reboot clears the journal.

### Cold power cycle

Some NICs apply NV configuration only after a cold power cycle. After a
warm reboot such a setting shows `pending-reset`, and with `onDrift`
`fail` or `apply` the run fails. With the default `RESCUE_MODE=reboot`
(from `FAILURE_ACTION=reboot`) the host then warm-reboots into BOOTy and
fails the same way until CAPRF power-cycles it. For such NICs use
`SUCCESS_ACTION=poweroff`, so the reboot after the apply is a power off and
CAPRF powers the host on again, and `FAILURE_ACTION=wait`, so a
`pending-reset` waits for an operator or a CAPRF power cycle instead of
looping. BOOTy has no callback that asks CAPRF for a power cycle; the error
report (`ERROR_URL`) names the `pending-reset` settings.

BOOTy logs a summary line `SR-IOV compliance` with the finding counts.

### Runtime settings

Runtime settings do not survive the kexec or the reboot into the installed
OS. BOOTy applies them in the ramdisk to prove that the PF ports can run
them, checks the result, and by default restores the state from before:

1. Group the PF ports by device (devlink handle): ports of one device share
   `sriov_numvfs` and the eSwitch. Two ports of a device that want
   different values fail.
2. On a port the ramdisk uses, BOOTy refuses the changes that would
   disrupt the provisioning network and its BGP sessions:
   - every eSwitch change: mlx5 replaces the PF netdev by the uplink
     representor in switchdev mode;
   - a VF count change on a driver that resets the PF for it. Per driver
     (Linux 6.12 source):

     | Driver | `sriov_configure` | VF count on a port in use |
     |--------|-------------------|---------------------------|
     | `mlx5_core` | enables the legacy eSwitch and `pci_enable_sriov`; the PF netdev stays up. With a hardware LAG (the port has a bond master), `mlx5_eswitch_enable` toggles the LAG on the first VFs (`mlx5_lag_disable_change`) | allowed, refused on a bonded port |
     | `ice` | `ice_ena_vfs` / `ice_free_vfs` change only VF resources | allowed |
     | `igb` | `igb_sriov_reinit` closes and opens the PF | refused |
     | `ixgbe` | `ixgbe_sriov_reinit` reinitializes the PF | refused |
     | `i40e` | resets and rebuilds the PF when it turns VEB mode on (first VFs) or off (no VFs) | refused |
     | any other | not checked | refused |

     `SRIOV_UPLINK_VF_DRIVERS` (YAML `provision.sriov.uplinkVFDrivers`)
     replaces the list of allowed drivers. On T-CaaS bm4x the ConnectX-6
     uplinks are the SR-IOV PFs, so `mlx5_core` must stay in it.

     Runbook, T-CaaS bm4x: a profile with `eswitchMode: switchdev` on the
     ConnectX uplinks always ends `refused-in-use` and fails the run with
     `onDrift` `fail` or `apply`. Use `onDrift: warn` for such a profile,
     or leave the eSwitch mode to the operator or the DRA driver and set
     only `numVFs` and the VF attributes.

   A refused setting is `refused-in-use`, and the VF findings of a refused
   VF count are too. The target is not met, so with `onDrift` `fail` or
   `apply` the run fails, and with `warn` it is logged. A setting whose
   target is already met needs no change and is never refused.

   BOOTy decides "in use" from its own network config first: the
   interfaces the stack really uses. GoBGP peers on the interfaces of its
   unnumbered sessions (frr.conf `neighbor X interface`, `BGP_SESSIONS`),
   else on `BGP_INTERFACES`, else on every physical NIC; the FRR stack
   peers on every physical NIC, also with `BGP_INTERFACES`; the static
   interface (without one, the first physical NIC), bond members, VLAN
   parents, netplan ethernets and set-name targets also count, with globs
   resolved against the links of the host. The kernel state adds a master
   device, a VLAN or other link on top, an address other than IPv6
   link-local, a TCP socket bound to the port (sock_diag, when the
   `inet_diag` and `tcp_diag` modules are there) and a unicast route
   through it. BOOTy never connects to a peer to find out: against a peer
   with graceful restart, a new connection from an established neighbor
   can reset the live session.
3. eSwitch mode through devlink (`DEVLINK_CMD_ESWITCH_SET`), with the VFs
   off first.
4. `sriov_numvfs` through `/sys/bus/<bus>/devices/<address>/sriov_numvfs`,
   through 0 when the count changes.
5. VF attributes through rtnetlink on the PF netdev, for each VF below
   `numVFs`: `vfDefaults` merged with the `vfs[]` entry of the index. MAC,
   VLAN and QoS, min and max rate, spoof check, link state and trust, in
   the order of `ip link set <pf> vf N`.
6. Read the ports again and compare `eswitchMode`, `numVFs` and each VF
   (`vfs[N]`, with the wanted and the read attributes). BOOTy finds the
   device and its netdev by bus, PCI address and port (`phys_port_name`,
   else the MAC), never by the interface name: a switchdev change
   re-registers the PF netdev, which can then have another name.
7. `handoff: reset` (the default) turns the VFs off, restores the eSwitch
   mode if BOOTy changed it and restores an earlier VF count, then checks
   it (`handoff`). `handoff: keep` leaves the state, for modes in which the
   ramdisk keeps running (standby, agent).

When BOOTy stored a change of `sriovEnabled`, `maxVFs` or `linkType` of
the adapter in the same run, `sriov_totalvfs` and the link type are still
the old ones, so creating the VFs now would fail. The runtime settings of
that adapter are `deferred` (logged as a warning) and BOOTy makes a
[verify reboot](#verify-reboot) before any disk change; the next boot of
the ramdisk checks them for real. Other stored firmware changes, such as
`CQE_COMPRESSION` alone, do not defer anything: the runtime settings are
checked in the same run, and the handoff reboots instead of kexec.

### Verify reboot

A verify reboot stops the task at `configure-sriov`, before the disk is
touched, and boots the BOOTy ramdisk again:

1. BOOTy posts the status `reboot-required` to `REBOOT_URL` (YAML
   `transport.rebootURL`). The body is one line of space-separated tokens,
   values without spaces:

   ```text
   reboot-required reason=sriov-firmware adapters=0000:3b:00,0000:5e:00 settings=maxVFs,sriovEnabled
   ```

   The first token is always `reboot-required`, then `reason=<token>`
   (today only `sriov-firmware`), then optional `key=value` tokens: here
   the adapters (PCI slots) and the stored settings that need the reset.
   CAPRF arms the boot media again before it answers 2xx, so BOOTy waits
   for a 2xx answer: it retries every 2 s for at most 30 s. Without a 2xx
   it reboots anyway and logs an error, because the host may then boot the
   disk instead of BOOTy.
2. BOOTy reboots (warm), without the error status, without rescue
   handling and without a failure count.
3. In the next boot the journal is gone, the stored values are active
   (`ok`), the runtime settings are applied and checked, and the task goes
   on. A setting that still is not active is `pending-reset` (see
   [Cold power cycle](#cold-power-cycle)).

CAPRF must, on `reboot-required`:

- keep the boot media inserted, or insert it again when it ejected the
  media at the `init` callback, so the host boots BOOTy again;
- not count the reboot as a failure and not power-cycle the host, and
  expect a second `init` callback of the same task;
- keep the task timeout running across both boots, or extend it.

Without `REBOOT_URL` BOOTy does not reboot: the run fails with the error
`sriov-verify-reboot-unsupported: SR-IOV firmware changed; a verify reboot
needs REBOOT_URL support in CAPRF`. A plain reboot could boot the old OS
in the middle of the task (CAPRF ejects the media at `init`), whose
compute profile can write `NUM_OF_VFS` again, and the task would stall.
`SRIOV_VERIFY_REBOOT=off` (YAML `provision.sriov.verifyReboot`) skips the
verify reboot: the run goes on to the handoff, which reboots instead of
kexec, and the deferred runtime settings are not checked (a warning).

CAPRF should cap repeated `reboot-required` statuses of the same task (for
example two), so a firmware setting that never becomes active cannot
reboot the host forever; BOOTy reports `pending-reset` for it on the next
boot. In standby mode a `provision` command that needs the verify
reboot is acknowledged as `failed` with the same status line; send it
again after the reboot.

Runtime findings are otherwise `ok`, `failed` or `refused-in-use`. `onDrift` `warn` logs a failure,
`fail` and `apply` stop the run. The runtime settings use only kernel
interfaces, so they work on every driver that implements them: `mlx5_core`,
`ice`, `i40e`, `ixgbe`, `bnxt_en`, `qede`, `cxgb4`, `sfc` and `igb`. In
switchdev mode, mlx5 does not take VLAN, QoS, trust and spoof check through
the PF netdev; those fail with the driver error.

`provision.sriov` is always decoded strictly: an unknown key fails even
when the rest of the YAML or JSON config is not loaded strictly.

## Handoff contract

The handoff is the kexec into the installed OS, or the reboot when a
firmware setting changed. The Kubernetes side owns the runtime state after
it: it creates the VFs, sets the eSwitch mode and the VF attributes, and
runs the representors and tc-flower offload.

BOOTy guarantees at the handoff, for every PF port:

1. The inventory has the `sriov` report of the port: with SR-IOV
   profiles, `configure-sriov` sends the inventory again after its runtime
   changes (`INVENTORY_ENABLED=true`). Firmware values that BOOTy stored in
   the same run still show the old active values (`sriovTotalVFs`,
   `linkType`) until the reboot; after a verify reboot the inventory of the
   next boot has the new values.
2. BOOTy never leaves VFs or a switchdev eSwitch behind that it created
   (`handoff: reset`, the default): the installed OS starts with the state
   that the NIC firmware and the OS driver defaults give (`sriovNumVFs` 0,
   eSwitch `legacy`). With profiles, the runtime settings of every matched
   PF port worked in the ramdisk with the firmware values of the handoff,
   or the run failed: when a stored firmware change affects them, the
   [verify reboot](#verify-reboot) checks them in the next boot before the
   task goes on.
3. A firmware-level setting that BOOTy changed is active: BOOTy reboots
   instead of kexec after the change, so `sriovTotalVFs`, the link type and
   the SR-IOV enable are the new values in the installed OS.
4. With profiles, every firmware setting of every matched PF port is
   compliant, or the run failed (`onDrift` `fail` or `apply`), or the drift
   is in the log (`warn`).

BOOTy does not guarantee:

- Runtime settings in the installed OS: `sriov_numvfs`, the eSwitch mode,
  VF trust, spoof check, VLAN, QoS, rate, MAC and link state do not survive
  the kexec or the reboot. The SR-IOV network operator or the DRA driver
  sets them.
- Representors, tc-flower offload, OVS, bonding of PFs for VF LAG, and the
  DDP package of the OS.

### Consumers

| Consumer | Uses | Field |
|----------|------|-------|
| DRA SR-IOV driver (`k8snetworkplumbingwg/dra-driver-sriov`) | eSwitch mode (the driver reads it through devlink and assumes legacy without it; eSwitch detection by `phys_switch_id` was proposed in upstream PR #120, closed without merge), PF name, vendor and device ID, NUMA node, link type | `eswitch.mode`, `physSwitchId`, `interface`, `vendorId`, `deviceId`, `numaNode`, `linkType` |
| DRA representor and tc-flower work (upstream issues #121, #122) | Whether the hardware can run switchdev | `eswitch.capability` |
| DRA VF attributes (upstream #123) and sriov-cni | VF limits | `sriovTotalVFs` |
| t-co `SRIOVIntegration` gate | The `SRIOVEnabled` bootstrap input that taints SR-IOV worker groups. The input should come from this report (`sriovCapable` and `sriovTotalVFs` of the profile PFs), not from a manual flag. | `sriovCapable`, `sriovTotalVFs` |
| CAPRF and bm4x | Host status and the host class | the whole `sriov` object |

The t-co draft MRs name the taint `sriov.t-caas.telekom.com/enabled`
(`api/v1alpha1/taints.go` on `feat/sriov-taint-const`); the proposal and
older notes name it `t-caas.telekom.com/sriov`. BOOTy sets no taint, so only
the t-co side has to settle the key.

### Ownership of firmware settings

| Setting | Owner |
|---------|-------|
| ConnectX `NUM_OF_VFS`, `SRIOV_EN` | BOOTy (`setup-mellanox`, `NUM_VFS`). |
| ConnectX `FLEX_PARSER_PROFILE_ENABLE`, `CQE_COMPRESSION`, `NUM_OF_VFS` from the OS | The T-CaaS compute profile (`mellanox_cfg.service`, inputs `FlexParserProfileEnable`, `CqeCompression`, `NumOfVFs`) writes them with `mstconfig` on every OS boot. They take effect only after the next reboot. |
| Runtime state | SR-IOV network operator or DRA driver. |

The compute profile writes `NUM_OF_VFS` too. When both set it to different
values, each provision and each OS boot changes it back, and every change
needs a reboot to take effect. Set it in one place only.
