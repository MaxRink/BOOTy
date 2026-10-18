# Hardware Vendor Matrix

## Status

Proposal. Design only. Nothing in this document is implemented. Every cell
in the matrices below is unverified unless the "Evidence" column names a
file in BOOTy, CAPRF or bm4x. Treat all other cells as "verify" and fill
them from the hardware-in-the-loop lab.

## Priority

P1 (it gates the vendor-specific parts of the other proposals)

## Summary

One cross-cutting matrix of server vendors and BMCs, with the Redfish
features and in-band tools that the BOOTy proposals depend on, and the known
quirks. The matrix becomes data: a YAML file that CAPRF capability discovery
and the quirks database (see [proposal-hardware-quirks.md](proposal-hardware-quirks.md))
use, and that the HIL lab keeps current.

## Problem and motivation

- Many proposals need a vendor feature: virtual media, HTTP boot, BIOS
  attributes, Secure Boot key databases, `UpdateService`, the host interface,
  writable logs, NIC SR-IOV attributes. Support differs per vendor and per
  firmware version.
- Today the vendor knowledge is spread over code comments and conditions.

## Current state

| Evidence | What it shows |
|----------|---------------|
| `pkg/system/vendor.go` | BOOTy knows `HPE`, `Lenovo`, `Dell Inc.`, `Supermicro`, `Generic` from DMI `sys_vendor`. |
| `pkg/provision/legacy_grub.go`, `pkg/provision/configurator.go` | Lenovo uses `ttyS1` for the legacy console append mode. |
| CAPRF `internal/redfish/system.go`, `system_hpe.go` | An HPE boot override exists but is disabled; all vendors use the base system. |
| CAPRF `docs/redfish-virtual-media.md` | Lenovo XCC PATCH insert and eject behaviour; HPE iLO 6 resource map; the iLO Redfish emulator in CI. |
| bm4x-operator `api/bm4x/v1alpha1/compute_types.go` | A `Vendor` field ("HPE, Lenovo, ...") on compute and NIC manufacturer data. |
| [proposal-bios-management-vendors.md](proposal-bios-management-vendors.md) | BIOS management plans for HPE, Lenovo, Supermicro, Dell. |

The fleet vendor mix is not in these repositories. Verify it from NetBox
device types and manufacturers.

## Matrix 1: Redfish features relevant to BOOTy

Legend: Y = supported, P = partial or OEM-only, N = not supported, ? = unknown.
All values need verification per firmware version.

| Vendor and BMC | VirtualMedia | UEFI HTTP boot override | BIOS attributes | SecureBoot databases | UpdateService | Host interface | LogService write | NetworkAdapter SR-IOV settings |
|----------------|--------------|-------------------------|-----------------|----------------------|---------------|----------------|------------------|-------------------------------|
| Dell iDRAC 9 | Y | Y (newer fw) | Y | Y | Y | Y | N (SEL via IPMI) | P (OEM NIC attributes) |
| Dell iDRAC 10 | Y | Y | Y | Y | Y | Y | N | P |
| HPE iLO 5 | Y | P (BIOS URL boot) | Y | P | Y | Y | N | P |
| HPE iLO 6 | Y | P | Y | Y | Y | Y | N | P |
| HPE iLO 7 | Y | ? | Y | ? | Y | ? | ? | ? |
| Lenovo XCC | Y (PATCH variants) | ? | Y | P | Y | Y | N | P |
| Lenovo XCC2 | Y | ? | Y | ? | Y | Y | ? | ? |
| Supermicro X11 | P | N | P | N | P | ? | N | N |
| Supermicro X12, X13, X14, H12, H13 | Y (licence on some, verify) | ? | Y | ? | Y | ? | N | ? |
| Fujitsu iRMC S5, S6 | Y | ? | Y | ? | Y | ? | ? | ? |
| Cisco UCS C-series CIMC (standalone) | Y | ? | Y | ? | Y | ? | ? | P |
| Gigabyte, ASRock Rack (AMI MegaRAC) | Y | ? | P | ? | P | ? | ? | ? |
| OpenBMC (OCP, Quanta, Inspur and others) | Y | ? | P (BIOS config manager, verify) | ? | Y | P | P (some log services, verify) | ? |

## Matrix 2: in-band tools

| Vendor | BIOS or system tool | RAID CLI | Redistributable in the initramfs | Note |
|--------|---------------------|----------|----------------------------------|------|
| Dell | `racadm` (local), Dell System Update | `perccli` | Verify licence; likely not | Prefer Redfish out of band |
| HPE | `ilorest` (open source, BSD, verify) | `ssacli` | `ilorest` likely yes; `ssacli` verify | `ilorest` can talk through the host interface |
| Lenovo | `onecli` | `storcli` | Verify | |
| Supermicro | `sum`, `saa` | `storcli` | Verify; `sum` needs a licence key for some features | |
| Fujitsu | `ServerView` tools | `storcli` | Verify | |
| Cisco | none local; CIMC XML API | `storcli` | n/a | |
| Generic | `ipmitool`, `efibootmgr`, `dmidecode`, `nvme`, `smartctl`, `mstflint` | `mdadm` | Yes (open source) | Default flavor set |

Rule: the default flavors contain only open-source tools. Vendor tools go
into opt-in flavors that the site builds itself when the licence allows.

## Matrix 3: known quirks (seed for the quirks database)

| Vendor | Quirk | Evidence or status |
|--------|-------|--------------------|
| Lenovo | Serial console on `ttyS1` in legacy append mode | `pkg/provision/legacy_grub.go` |
| Lenovo XCC | Virtual media insert and eject through PATCH on some firmware | CAPRF `docs/redfish-virtual-media.md` |
| HPE iLO | One-time boot needs an OEM override on some versions; CAPRF override disabled | CAPRF `internal/redfish/system_hpe.go` |
| Several | BMCs cache or drop query strings in virtual media URLs | CAPRF `docs/redfish-virtual-media.md` |
| Several | `BootOrder` reset after BMC or firmware update | verify |
| Supermicro | Virtual media limits and licence gates | verify |
| Several | ATA drives frozen by firmware | `pkg/disk/manager.go` (frozen check) |

## Design

- Store the matrices as `hack/vendors/*.yaml` (or in CAPRF) with a schema:
  vendor, BMC model, firmware range, feature, value, evidence, last verified.
- CAPRF capability discovery fills per-host values at enrollment from
  Redfish (see [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md))
  and compares with the matrix.
- The quirks database consumes Matrix 3.
- Docs generate from the YAML.

## Interfaces

### Native config keys

None in BOOTy. BOOTy reads the vendor from DMI and gets capability flags
from CAPRF in `/deploy/vars` when needed (`BMC_CAPABILITIES` JSON, optional).

### CAPRF and bm4x impact

- `RedfishHost.status.bmcCapabilities` (shared with the BMC proposal).
- bm4x compute objects carry the vendor already; add the BMC model and
  firmware.

## Security considerations

- Vendor tools in the ramdisk run as root with hardware access. Opt-in only.
- Do not store BMC credentials in the matrix data.

## Testing strategy

- Unit: schema validation of the YAML; docs generation.
- KVM matrix: none (vendor behaviour).
- kindmetal and CAPRF e2e: Redfish mocks per vendor profile (iLO emulator
  exists in CAPRF CI; add sushy-tools profiles, verify).
- Real hardware: the HIL lab runs a capability probe per host nightly and
  updates "last verified".

## Phased rollout

1. Docs PR: the matrices in YAML with evidence links.
2. CAPRF MR: capability discovery writes host status.
3. PR: generated docs from YAML.
4. Lab: nightly capability probe.

## Effort estimate

| Phase | Days |
|-------|------|
| YAML and evidence | 3 to 5 |
| CAPRF discovery | 5 to 8 |
| Docs generation | 1 to 2 |
| Lab probe | 3 to 5 |
| **Total** | **12 to 20** |

## Open questions

- Which vendors and BMC generations run in T-CaaS and bm4x today (NetBox)?
- Where does the matrix live: BOOTy, CAPRF, or a shared repository?

## Related proposals

- [proposal-hardware-quirks.md](proposal-hardware-quirks.md)
- [proposal-bmc-preload-boot.md](proposal-bmc-preload-boot.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
- [proposal-sriov-nic-config.md](proposal-sriov-nic-config.md)
- [proposal-bios-raid-as-code.md](proposal-bios-raid-as-code.md)
- [proposal-vendor-integrations.md](proposal-vendor-integrations.md)
