# SR-IOV, NIC, DPU and GPU Configuration

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented. It extends
[proposal-nic-firmware-common.md](proposal-nic-firmware-common.md) and the
vendor NIC proposals ([Mellanox](proposal-nic-firmware-mellanox.md),
[Intel](proposal-nic-firmware-intel.md),
[Broadcom](proposal-nic-firmware-broadcom.md)) with complete SR-IOV
coverage, a handoff contract to the Kubernetes SR-IOV stack, DPU mode and
GPU mode settings.

## Priority

P1

## Summary

BOOTy should provision every firmware-level SR-IOV prerequisite on every
relevant NIC family, verify it, and report it, so that the Kubernetes side
(SR-IOV network operator or the DRA SR-IOV driver) only does runtime work
and never has to change NIC firmware or reboot a node. The same mechanism
covers BlueField DPU mode and BFB installs, and NVIDIA GPU mode settings.
A desired SR-IOV profile per host class drives apply and drift checks.

## Problem and motivation

- Firmware-level SR-IOV settings (SR-IOV enable, maximum VFs, link type,
  BAR size, DPU mode) need a firmware reset or a reboot to take effect. If
  the Kubernetes operator applies them, it drains and reboots nodes in
  production. BOOTy runs before the node joins and already reboots.
- Today BOOTy sets `SRIOV_EN` and `NUM_OF_VFS` on ConnectX cards only, through
  `mstconfig` (`NUM_VFS`, `NUM_VFS_MODE`, `pkg/provision/legacy_mellanox.go`). Other
  vendors and other settings are not handled.
- T-CaaS work in flight depends on SR-IOV nodes: the t-co `SRIOVIntegration`
  feature gate and the `t-caas.telekom.com/sriov` taint (draft MRs), the DRA
  SR-IOV driver work (upstream `dra-driver-sriov`: eSwitch detection by
  `phys_switch_id`, VF representors, tc-flower offload, VF attributes), and
  the node-tuning or compute-profile path in the bundles repository.
- Without a handoff contract, each layer guesses what the other configured.

## Current state

> **Update:** #709 (SR-IOV per PF port in the inventory), #717 (profiles
> with strict validation and a drift check) and #719 (runtime settings through
> kernel interfaces) merged; see [sriov.md](sriov.md). Vendor firmware backends
> (#726) and the KVM `igb` scenario (#728) are open. The table below is the
> state before that work.

| Area | Today | Reference |
|------|-------|-----------|
| ConnectX VFs | `setup-mellanox` with `NUM_VFS` and `NUM_VFS_MODE` (`set`: sets `SRIOV_EN` and `NUM_OF_VFS`; `raise`: raises `NUM_OF_VFS` on every ConnectX-4 to 7 Ethernet function, preset by legacy compat). A change forces a reboot instead of kexec. | `pkg/provision/legacy_mellanox.go` (`setupMellanoxLegacy`), `pkg/provision/orchestrator.go` (`setupMellanox`, `FirmwareChanged`) |
| NIC firmware framework | `FirmwareManager` interface with `Capture` and `Apply`, baselines and diff; Mellanox (`mstconfig`), Intel, Broadcom, ethtool backends. Not wired into the provision flow except logging. | `pkg/firmware/nic/` |
| Inventory | NIC name, MAC, driver, firmware version, PCI address. No SR-IOV data. | `pkg/inventory/`, `pkg/firmware/collector.go` |
| Runtime VFs | None. BOOTy does not set `sriov_numvfs`. | |
| DPU, GPU | None. | |

## Goals

- For each NIC family: read, apply, verify and report firmware-level SR-IOV
  settings, and read the runtime capabilities.
- A desired `SriovProfile` per host class with `fail` or `warn` drift
  handling.
- Prefer kernel interfaces (devlink, netlink, sysfs) over vendor binaries.
- A documented handoff contract to the Kubernetes SR-IOV stack.
- BlueField DPU mode and BFB install.
- NVIDIA GPU mode settings where they need a reset.

## Non-goals

- Creating VFs for workloads. The Kubernetes operator or DRA driver does
  that at runtime.
- Configuring OVS or switchdev offload rules.

## Settings model

### Firmware level (persistent, need a reset)

| Setting | Meaning |
|---------|---------|
| `sriovEnabled` | SR-IOV capability in NIC firmware |
| `maxVFs` | Maximum VFs per PF (`sriov_totalvfs` after reset) |
| `linkType` | Ethernet or InfiniBand per port (VPI cards) |
| `portMode` | Port split or breakout, port count (vendor-specific) |
| `barSize` | PF or VF BAR size where tunable |
| `eswitchCapable` | Firmware support for switchdev |
| `vfLag` | LAG and multi-port eSwitch prerequisites |
| `rocePfcDcb` | RoCE enable, default PFC priorities, DCBX mode (host or firmware LLDP agent) |
| `dpuMode` | BlueField DPU (embedded CPU owns the eSwitch) or NIC mode |

### Runtime (after boot, owned by Kubernetes)

`sriov_numvfs`, eSwitch mode (`devlink dev eswitch set mode switchdev|legacy`),
VF trust, spoof check, VLAN, QoS, rate, MAC, link state, representors.
BOOTy reads and reports these capabilities. It sets them only for its own
validation, and resets them before the handoff.

## Coverage per NIC family (verify every row against vendor documentation and the target kernel)

| Family | Driver | Firmware-level tool | Kernel-only path | Notes |
|--------|--------|---------------------|------------------|-------|
| NVIDIA ConnectX-4 Lx, 5, 6 Dx, 6 Lx, 7, 8 | `mlx5_core` | `mstconfig` (mstflint, open source) or `mlxconfig` (MFT, proprietary). NV params `SRIOV_EN`, `NUM_OF_VFS`, `LINK_TYPE_P1/P2`, `NUM_VF_MSIX`, BAR and RoCE params | devlink params (some generic and mlx5 params), devlink eswitch mode, `sriov_numvfs`; NV config is not fully exposed in devlink | `mstfwreset` or a cold reboot to apply. VF LAG needs bonding in switchdev mode. OVS hardware offload needs switchdev, `hw-tc-offload on`, matching firmware. |
| NVIDIA BlueField-2, BlueField-3 | `mlx5_core` on the host | `mstconfig` plus DPU mode params (`INTERNAL_CPU_MODEL` or newer offload engine params, verify); BFB install through `rshim` | `rshim` host driver exposes the boot device | DPU mode: the Arm side owns the eSwitch; the host sees only representors or VFs. NIC mode behaves like ConnectX. |
| Intel E810 | `ice` | `nvmupdate64e` (proprietary) for NVM; DDP package in `/lib/firmware/intel/ice/ddp/` | devlink (flash, params, eswitch mode), `sriov_numvfs`, ethtool private flag `fw-lldp-agent` | DDP package version must match the driver. ADQ uses tc and is runtime. |
| Intel X710, XL710, XXV710 | `i40e` | `nvmupdate64e` | `sriov_numvfs`, private flag `disable-fw-lldp` | Max VFs set in NVM. No switchdev. |
| Intel X540, X550 | `ixgbe` | `nvmupdate64e` | `sriov_numvfs` (max 63, verify) | Legacy. |
| Intel IPU E2000 family | `idpf` | Vendor tooling (verify) | verify | Treat as DPU; out of first scope. |
| Broadcom NetXtreme-E (P5, P7 Thor) | `bnxt_en` | `niccli` or `bnxtnvm` (proprietary) for NVM options | devlink generic params `enable_sriov`, `msix_vec_per_pf_max` (verify), devlink eswitch mode (TruFlow, verify), `sriov_numvfs` | Licence of the tools limits redistribution. |
| Marvell/QLogic FastLinQ | `qede`, `qed` | Vendor tools (verify) | `sriov_numvfs` | Max VFs in NVM. |
| Chelsio T6 | `cxgb4` | Firmware config file and `cxgbtool` (verify) | `sriov_numvfs` on the PF that owns VFs (verify) | |
| AMD Pensando | `ionic` | Vendor tooling (verify) | `sriov_numvfs` (verify) | DPU-like. |
| AMD Solarflare | `sfc`, `sfc_ef100` | `sfboot` (proprietary) | `sriov_numvfs`, switchdev on EF100 (verify) | |

What works without vendor binaries:

- Runtime capability reading for every family (sysfs `sriov_totalvfs`,
  `sriov_numvfs`, devlink `eswitch show`, `phys_switch_id`, `phys_port_name`).
- Firmware-level changes on ConnectX and BlueField with `mstconfig` from
  mstflint (open source, verify licence: dual GPLv2 or BSD).
- Firmware-level changes on Broadcom through devlink generic params where the
  driver supports them (verify).
- Intel firmware-level max VF changes need `nvmupdate64e` or the Redfish NIC
  attributes. Ship the vendor tool only in a separate, non-redistributed
  flavor if the licence forbids redistribution.

Kernel minimums: check each switchdev, VF LAG and devlink param feature
against the kernel of the `build/kernel-debian` work (verify); report a
capability as `unsupported-kernel` instead of failing.

### Out of band: Redfish NetworkAdapter

- Redfish `NetworkAdapters/*/NetworkDeviceFunctions` and controller
  capabilities report SR-IOV capability and VF counts. Settings are often in
  OEM extensions, for example Dell NIC attributes such as virtualization
  mode and VFs advertised (verify names), HPE through BIOS or NIC settings
  (verify), Lenovo through UEFI settings (verify).
- Use the Redfish path when the in-band tool is not redistributable, or for
  NICs without a Linux tool. CAPRF applies it before the BOOTy boot, and
  BOOTy verifies in-band.

## Handoff contract to Kubernetes

BOOTy guarantees, per PF in the profile, at handoff time:

1. Firmware-level settings equal the profile, and the firmware reset that
   applies them is done (BOOTy reboots instead of kexec when it changed
   anything, as today).
2. `sriov_totalvfs` is at least the profile `maxVFs`.
3. `sriov_numvfs` is 0 and the eSwitch is in `legacy` mode, unless the profile
   says otherwise. The runtime owner then sets them.
4. The link type and port mode equal the profile.

BOOTy reports in the inventory (and through the CAPI event contract):

| Field | Source |
|-------|--------|
| PCI address, vendor and device ID, NUMA node | sysfs |
| Driver and firmware version | ethtool `drvinfo`, devlink `info` |
| `sriovTotalVFs`, `sriovNumVFs` | sysfs |
| `eswitchModes` supported and current | devlink |
| `physSwitchID`, `physPortName` | sysfs |
| `representorsSupported` | devlink port listing in switchdev (probe) |
| `linkType`, `portMode` | NV config or devlink |
| `roce`, `dcbxMode` | NV config, ethtool flags |
| `dpuMode` | NV config |
| Profile compliance per setting | drift check |

T-CaaS consumers:

- t-co sets the `t-caas.telekom.com/sriov` taint from the bootstrap input
  `SRIOVEnabled` behind `SRIOVIntegration` (draft). That input should come
  from the compliance result, not from a manual flag.
- The DRA SR-IOV driver uses `phys_switch_id` for eSwitch detection
  (upstream PR, see `dra-driver-sriov` #120) and needs representors and
  tc-flower offload (#121, #122). BOOTy reports whether the hardware and
  firmware can provide them.
- The node-tuning or compute-profile path in the bundles repository stays the
  owner of runtime tuning (CPU governor, offloads). BOOTy does not duplicate it.

## Profiles and drift

```json
{"name":"sriov-cx6dx-2p","match":{"vendor":"15b3","device":"101d"},
 "firmware":{"sriovEnabled":true,"maxVFs":64,"linkType":"eth",
             "eswitchCapable":true,"roce":true,"dpuMode":"nic"},
 "onDrift":"fail"}
```

- CAPRF passes profiles per host class (from NetBox or bm4x host data,
  verify the source).
- `onDrift`: `fail` (stop provisioning), `warn` (report), `apply` (change and
  reboot).

## DPU and GPU

- BlueField: switch between DPU and NIC mode by profile; install a pinned BFB
  through `rshim` from the host when `bfb` is set; verify the BFB version
  through the DPU's own reporting (verify the interface). Long operation;
  run it in standby or in a dedicated `MODE=hardware-config`.
- NVIDIA GPUs: settings that need a reset, for example MIG mode enable or
  ECC mode, with `nvidia-smi` (proprietary, needs the driver; verify whether
  a ramdisk flavor with the driver is acceptable) or out of band where the
  BMC supports it (verify). Default: report only; apply in the OS through
  the GPU operator.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `SRIOV_PROFILES` | JSON array or ramdisk path | Desired profiles. |
| `SRIOV_ON_DRIFT` | `fail`, `warn`, `apply` | Default for profiles without `onDrift`. |
| `NIC_TOOLS` | `kernel`, `opensource`, `vendor` | Which tool classes BOOTy may use. |
| `DPU_MODE` | `keep`, `dpu`, `nic` | BlueField. |
| `DPU_BFB` | URL or path with digest | Optional BFB install. |
| `GPU_SETTINGS` | JSON | Report-only by default. |
| `NUM_VFS`, `NUM_VFS_MODE` (existing) | integer, `set` or `raise` | Kept. Mapped to an implicit ConnectX profile. |

### CAPRF, CAPI and bm4x impact

- `RedfishHost.status.nics[].sriov` with the reported fields.
- A host class to profile mapping in CAPRF or bm4x.
- CAPRF can apply Redfish NIC attributes before the BOOTy boot.
- A `SriovReady` condition that t-co can read through the bootstrap input.

## Security considerations

- NIC firmware changes can break connectivity of the provisioning link. Apply
  them only on PFs that are not the active provisioning uplink, or apply them
  last and verify after reboot. Never change the link type of the
  provisioning port.
- Vendor binaries in the ramdisk widen the attack surface and have licence
  limits. Keep them in a separate flavor.
- BFB images are firmware. Verify digests and vendor signatures.
- VF trust and spoof check defaults matter for tenant isolation; BOOTy leaves
  them to the runtime owner but reports insecure firmware defaults.

## Testing strategy

- Unit: parsers for `mstconfig q`, `devlink dev param show -j`,
  `devlink dev eswitch show -j`, `devlink dev info -j` with captured fixtures
  per family. Profile matching and drift results.
- netdevsim: devlink eswitch mode switching, `sriov_numvfs`, port listing
  logic in a privileged CI job (netdevsim supports SR-IOV and devlink,
  verify the feature set for the CI kernel).
- KVM matrix: QEMU `igb` with SR-IOV (`igbvf` VFs, verify the QEMU version)
  to test `sriov_totalvfs` reading, `sriov_numvfs` reset and the reboot
  decision.
- ContainerLab: none.
- kindmetal and CAPRF e2e: profile delivery and status reporting with
  emulated NICs.
- Real hardware lab plan: one host each with ConnectX-6 Dx, ConnectX-7,
  BlueField-3, E810, X710 and a Broadcom P5 card; run apply, reset, verify
  and drift tests nightly.

## Phased rollout

1. PR: SR-IOV inventory fields (kernel-only) for all families.
2. PR: profile schema, matching and drift report (`warn`).
3. PR: ConnectX firmware-level apply through `FirmwareManager` with
   `mstconfig`; replace the legacy-only path while keeping its behaviour.
4. PR: handoff reset (`sriov_numvfs` 0, eSwitch legacy) and contract checks.
5. PR: Broadcom devlink param path.
6. PR: Intel E810 and X710 reporting plus DDP package check; vendor tool
   path behind `NIC_TOOLS=vendor`.
7. CAPRF MR: profiles per host class, status, `SriovReady`.
8. PR: BlueField DPU mode and BFB install.
9. PR: GPU settings report.
10. CAPRF MR: Redfish NIC attribute apply for vendors that support it.

## Effort estimate

| Phase | Days |
|-------|------|
| Inventory fields | 4 to 6 |
| Profiles and drift | 3 to 5 |
| ConnectX apply | 4 to 6 |
| Handoff contract | 2 to 3 |
| Broadcom | 3 to 5 |
| Intel | 4 to 6 |
| CAPRF integration | 5 to 8 |
| BlueField | 6 to 9 |
| GPU report | 2 to 3 |
| Redfish NIC attributes | 5 to 8 |
| Lab tests | 5 to 8 |
| **Total** | **43 to 67** |

## Open questions

- Which NIC families are in the T-CaaS fleet today and planned (verify with
  NetBox device types)?
- Can we ship MFT or other vendor tools in a flavor, or only open-source
  tools?
- Does the SR-IOV network operator in T-CaaS still apply firmware settings
  itself (its config daemon can run `mstconfig`, verify)? If yes, disable that
  to avoid double ownership.
- Who owns the profiles: bm4x host classes, NetBox, or CAPRF templates?

## Related proposals

- [proposal-nic-firmware-common.md](proposal-nic-firmware-common.md)
- [proposal-nic-firmware-mellanox.md](proposal-nic-firmware-mellanox.md)
- [proposal-nic-firmware-intel.md](proposal-nic-firmware-intel.md)
- [proposal-nic-firmware-broadcom.md](proposal-nic-firmware-broadcom.md)
- [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md)
- [proposal-hardware-vendor-matrix.md](proposal-hardware-vendor-matrix.md)
- [proposal-hardware-quirks.md](proposal-hardware-quirks.md)
