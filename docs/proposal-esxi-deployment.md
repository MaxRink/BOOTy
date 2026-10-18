# VMware ESXi Deployment

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P4

## Summary

Deploy VMware ESXi on hosts that CAPRF manages. ESXi is not a Linux system.
BOOTy cannot kexec into it and cannot configure it from a chroot. The
recommended design lets CAPRF boot the ESXi installer with a kickstart file
through Redfish virtual media, and uses BOOTy only for the pre-install work
(inventory, health, firmware checks, wipe, RAID) and for post-install
verification. A dd image path is documented with its limits.

## Problem and motivation

- Some sites run ESXi hosts on the same hardware fleet and want one bare-metal
  lifecycle tool.
- BOOTy rejects `TARGET_OS=esxi` today. This is correct for the current
  Linux-only flow.
- The ESXi product and licence situation changed after the Broadcom
  acquisition (verify current terms). Any design must not depend on a free
  ESXi edition.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Target OS | `esxi` and VMware values are rejected. | `pkg/config/provision.go`, `pkg/config/validate.go` |
| Pre-install steps | Inventory, firmware report, health, wipe, RAID, NVMe namespaces run before the image write. | `pkg/provision/orchestrator.go` (`provisionSteps`) |
| Virtual media | CAPRF builds one ISO per machine and inserts it. Boot override is `Cd` once. | CAPRF `internal/redfish/system.go` (`SetBootTargetOnce`), `internal/ramdisk/builder.go` |
| Standby | BOOTy can wait for commands. | `cmd/standby.go`, `docs/caprf-integration.md` |

## ESXi facts that shape the design (verify each for the target version)

- The installer (weasel) reads a kickstart file named by `ks=` in `boot.cfg`
  `kernelopt`. Sources include `cdrom:`, `usb:`, `file:` and HTTP or HTTPS
  URLs.
- Kickstart commands include `vmaccepteula`, `rootpw`, `install --firstdisk`
  or `--disk=`, `--overwritevmfs`, `network`, and `%firstboot` scripts.
- ESXi 7 and later use a new layout: system boot partition, two boot banks
  (`BOOTBANK1`, `BOOTBANK2`) and `ESX-OSData`, plus an optional VMFS datastore.
  The two boot banks are an A/B scheme that ESXi manages itself.
- A cloned ESXi disk keeps the system UUID and other identity unless it is
  reset. VMware does not support cloning an installed ESXi disk as a
  deployment method (verify the current KB position).
- ESXi supports UEFI Secure Boot with VMware-signed boot components and TPM 2.0
  attestation through vCenter.

## Goals

- Provide a supported ESXi install flow through CAPRF.
- Reuse BOOTy for hardware checks and disk preparation before the install.
- Verify the installed host after first boot (optional).

## Non-goals

- Kexec into ESXi.
- Managing vCenter, clusters or licences.
- CAPI workload clusters on ESXi hosts. CAPV owns VMs on vSphere.
- Building custom ESXi images (Image Builder or depot tooling).

## Design options

### Option A: CAPRF boots the installer ISO with kickstart

CAPRF builds or selects an ESXi installer ISO with a kickstart file (on the
ISO, or by an HTTPS `ks=` URL served by the CAPRF image server). It inserts
the ISO and sets a one-time boot to virtual media. The kickstart reports
success through a `%firstboot` script that calls `SUCCESS_URL`.

- Pro: supported install path. ESXi owns its layout and boot banks.
- Con: no BOOTy pre-install steps unless CAPRF chains them. Progress reporting
  is coarse.

### Option B: Two-stage, BOOTy then installer

CAPRF boots BOOTy in a `prepare` mode first. BOOTy runs inventory, health,
firmware validation, secure erase and RAID setup, reports, and powers off.
Then CAPRF runs Option A.

- Pro: one inventory and health story for all OSes. Clean disks before the
  install.
- Con: two boots. A new BOOTy mode.

### Option C: dd a golden ESXi image

BOOTy writes a golden disk image and edits `boot.cfg` or the state archive to
inject host config.

- Pro: fast.
- Con: unsupported by VMware (verify). The state archive (`state.tgz`) format
  is internal. Identity reset is fragile.

### Recommendation

Option B. Add a generic BOOTy `MODE=prepare` that runs the non-OS steps and
stops. This mode is useful for other OS installers too (Windows WinPE,
vendor installers). Do not implement Option C.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `MODE` | add `prepare` | Runs `report-init` to `wipe-disks` and RAID or namespace setup, then reports and powers off. |
| `PREPARE_SUCCESS_ACTION` | `poweroff`, `standby` | `standby` lets CAPRF send the next step without a power cycle. |

No ESXi-specific keys in BOOTy.

### CAPRF and CAPI API impact

- `RedfishMachine.spec` gets an installer section: installer ISO source,
  kickstart template reference, and target disk selector. CAPRF renders the
  kickstart.
- CAPRF needs a two-stage state machine: `Preparing`, then `Installing`.
- The kickstart `%firstboot` script calls the same callback endpoints as
  BOOTy, with the same token rules.
- CAPRF must keep Secure Boot on for ESXi when the firmware trusts the VMware
  keys, and must not call `DisableSecureBoot`.

## Security considerations

- The kickstart holds the root password hash. Serve it over HTTPS with a
  per-machine token, or embed it in the per-machine ISO. Expire the URL after
  use.
- `%firstboot` scripts run as root. Treat the template as privileged config.
- Verify the installer ISO checksum before insert. Prefer VMware-signed ISOs.

## Testing strategy

- Unit: `MODE=prepare` step list and validation in BOOTy.
- KVM matrix: `prepare-mode` scenario (no ESXi). It asserts that no image
  write happens and that the disks are wiped.
- ContainerLab: none.
- kindmetal and CAPRF e2e: ESXi as a nested guest is possible on some hosts
  (verify licence and CPU support). Prefer a CAPRF unit and envtest suite for
  the state machine, plus a mocked Redfish server.
- Real hardware: one host per vendor in a lab with a valid licence.

## Phased rollout

1. BOOTy PR: `MODE=prepare`. Useful without ESXi.
2. CAPRF MR: installer section and kickstart rendering behind a feature gate.
3. CAPRF MR: two-stage state machine.
4. Lab validation on hardware.

## Effort estimate

| Phase | Days |
|-------|------|
| BOOTy `MODE=prepare` | 3 to 4 |
| CAPRF installer and kickstart | 6 to 9 |
| CAPRF two-stage flow | 5 to 8 |
| Lab validation | 3 to 5 |
| **Total** | **17 to 26** |

## Open questions

- Is ESXi in scope at all? If licences are not available, defer this.
- Who owns the kickstart templates and the root credentials?
- Should `MODE=prepare` be the base for all non-Linux installers?

## Related proposals

- [proposal-windows-deployment.md](proposal-windows-deployment.md)
- [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)
- [proposal-disk-lifecycle.md](proposal-disk-lifecycle.md)
