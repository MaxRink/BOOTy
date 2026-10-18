# BMC-Based Preloading and Booting

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P2

## Summary

CAPRF boots BOOTy today with one path: a per-machine ISO served over HTTPS
or HTTP, inserted through Redfish virtual media, with a one-time `Cd` boot
override. This proposal adds UEFI HTTP boot through the Redfish
`BootSourceOverrideTarget` `UefiHttp` and `HttpBootUri`, a generic
(not per-machine) BOOTy image with per-machine data delivered at runtime,
OEM handling for iDRAC, iLO, XCC and Supermicro, and preloading of the
target image or sysexts into a staging area. The goal is faster, more
reliable, PXE-less boots with fewer large per-machine artifacts.

## Problem and motivation

- Virtual media is slow on many BMCs. The BMC fetches the ISO over its own
  management link and serves it as a USB CD. Large initramfs images take
  minutes to load (verify per vendor).
- A per-machine ISO costs build time and storage in CAPRF and makes caching
  hard. The token is in the path, so no two machines share a URL.
- Some BMCs allow only one virtual media session, refuse HTTPS with private
  CAs, or drop query strings (CAPRF `docs/redfish-virtual-media.md`).
- UEFI HTTP boot loads the boot file through the host NIC, which is faster
  and does not use the BMC data path.
- Preloading the next OS image to the host before a maintenance window
  shortens the outage.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Boot media | Per-machine ISO built from a base ISO plus a `/deploy` cpio overlay. | CAPRF `internal/ramdisk/builder.go` (branch adds `cpio.go`, `override.go`) |
| Virtual media | Eject, optional certificate import, `InsertMedia`, PATCH fallback, retry. | CAPRF `internal/redfish/virtualmedia.go`, `docs/redfish-virtual-media.md` |
| Boot override | `Cd` once. | CAPRF `internal/redfish/system.go` (`SetBootTargetOnce`) |
| OEM | HPE system override exists but `NewSystem` returns the base system for HPE. | CAPRF `internal/redfish/system.go`, `system_hpe.go` |
| Secure Boot | CAPRF disables Secure Boot before the BOOTy boot. | CAPRF `internal/redfish/system.go` (`DisableSecureBoot`) |
| BOOTy and Redfish | BOOTy does not call Redfish. Tests use a Redfish mock. | `test/e2e/redfish/mock_server.go` |
| ISO artifacts | BOOTy builds ISO and GoBGP ISO targets. | `Makefile` (`iso`, `gobgp-iso`), `initrd.Dockerfile` |
| Local BMC access | Proposal only. | [proposal-ipmi-local.md](proposal-ipmi-local.md) |

## Goals

- Add UEFI HTTP boot as a second boot transport in CAPRF.
- Make the BOOTy boot artifact generic and cacheable. Deliver per-machine
  data at runtime.
- Keep Secure Boot on during the BOOTy boot when the artifact is signed.
- Document and encode OEM differences.
- Allow preloading of a target image or sysexts while the current OS runs.

## Non-goals

- PXE and TFTP. Out of scope by design.
- Uploading firmware through the BMC. See [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md).
- A BMC-hosted image store as the primary path. Few BMCs support it (see
  Option D).

## Design options

### Option A: Keep virtual media, make the ISO generic

Serve one signed BOOTy ISO per release and flavor. Deliver `/deploy` data at
runtime: BOOTy reads a small per-machine seed (URL and token) from a second
virtual media image (a tiny ISO or floppy image, verify floppy support), from
SMBIOS OEM strings, or from the kernel command line in the ISO boot config.

- Pro: one cached ISO. Signed once. Faster CAPRF builds.
- Con: still the BMC data path. A second medium is not supported everywhere.

### Option B: UEFI HTTP boot of a signed UKI

CAPRF sets `Boot.BootSourceOverrideTarget = UefiHttp`,
`BootSourceOverrideEnabled = Once` and `Boot.HttpBootUri` to the BOOTy UKI or
ISO URL (verify the Redfish schema version that adds `HttpBootUri`). The
firmware gets an address by DHCP (the firmware HTTP boot client needs DHCP or
a static IP set through BIOS attributes, verify per vendor), downloads the
UKI and runs it. Per-machine data comes from a runtime seed (see below).

- Pro: host NIC speed. No ISO. A UKI is one signed PE file.
- Con: needs DHCP or vendor-specific static IP config in firmware. Not
  available on all BMCs. HTTPS needs the CA in the firmware TLS store
  (vendor-specific). EVPN-only fabrics have no DHCP in the provisioning VLAN
  before BOOTy runs.

### Option C: Redfish host interface seed

The Redfish Host Interface (DMTF DSP0270, verify) gives the host OS a USB
network link to the BMC and optional credential bootstrapping. BOOTy reads
its per-machine seed from a BMC-side resource over this link.

- Pro: no network dependency for the seed.
- Con: vendor support varies. Needs credentials on the host. More attack
  surface.

### Option D: BMC-hosted image store

Upload BOOTy into the BMC's internal storage (for example iLO Repository or
iDRAC local storage, verify) and boot it from there.

- Pro: no external server at boot.
- Con: vendor-specific APIs, small storage, slow uploads, no common Redfish
  API. Rejected as a default.

### Recommendation

1. Option A first: generic signed ISO plus a runtime seed. This removes the
   per-machine build and enables Secure Boot.
2. Option B as an opt-in transport per `RedfishHost`, with a capability probe
   that checks `UefiHttp` in `BootSourceOverrideTarget@Redfish.AllowableValues`.
3. Option C only as a seed source on vendors that support it well.
4. Reject Option D.

## Runtime seed

BOOTy reads a seed in this order and stops at the first hit:

1. Kernel command line `booty.seed=<url>` (ISO boot config or HTTP boot UKI
   cmdline add-on, verify UKI cmdline rules under Secure Boot).
2. SMBIOS type 11 OEM strings `io.booty.seed=<url>` (QEMU supports them in
   tests; BMC support varies, verify).
3. A second virtual media volume labeled `BOOTYSEED` with `/seed.json`.

The seed holds the CAPRF URL, a one-time bootstrap token and a CA
fingerprint. BOOTy fetches `/deploy` content (vars, files, machine commands)
as a tar from CAPRF over HTTPS, and verifies it with the CA pin. This is the
same data as today, delivered later.

Network bootstrap problem: BOOTy needs network to fetch the seed content. In
EVPN-only fabrics, BOOTy needs its network config before it can fetch.
Therefore the seed may also inline the network section (`NETWORK_DATA`, see
[proposal-capi-integration.md](proposal-capi-integration.md)). Keep the
current embedded `/deploy` path as a fallback.

## OEM differences (verify each)

| Topic | Dell iDRAC 9 | HPE iLO 5 and 6 | Lenovo XCC | Supermicro |
|-------|--------------|-----------------|------------|------------|
| Virtual media insert | Standard `InsertMedia` | Standard plus OEM; one-time boot through OEM override in some versions | PATCH insert and eject on some firmware | Limited; some versions accept only one image or need OEM endpoints |
| HTTPS virtual media with private CA | Certificate import supported on newer firmware | Certificate import supported | Supported on newer firmware | Varies |
| UEFI HTTP boot override | `UefiHttp` with `HttpBootUri` on newer firmware | HTTP boot URL through BIOS attributes (`UrlBootFile`) | Varies | Varies |
| Secure Boot keys via Redfish | `SecureBootDatabases` | `SecureBootDatabases` on iLO 6 | Varies | Varies |
| Boot override persistence | `Once` honored | HPE `SetBootTargetOnce` override exists but is disabled in CAPRF | `Once` honored | Varies |

CAPRF should model these as capability flags discovered at enrollment and
stored on `RedfishHost.status`, not as hard-coded vendor branches.

## Preloading the target image

### Option P1: Staging partition

BOOTy, in standby or during a previous provision, creates a `BOOTY-STAGE`
partition. Before a maintenance window, a standby command streams the next
image into it and verifies the checksum. The real install then copies from the
local stage instead of the network.

- Pro: the outage is only a local copy. Works with A/B (stage into the
  inactive slot directly).
- Con: disk space. Only useful when hosts run BOOTy standby or a node agent.

### Option P2: A/B inactive slot as the stage

For A/B hosts, the inactive slot is the stage. A node-side agent (or a BOOTy
standby command after a reboot into BOOTy) writes the inactive slot while the
active slot runs. The switch is one reboot.

- Pro: no extra partition. Matches `AB_PRESERVE_EXISTING`.
- Con: needs a node-side writer, or a BOOTy boot, which is an outage.

### Option P3: Sysext preload while running

Sysexts are small. A node agent or `systemd-sysupdate` (verify) fetches them
into the catalog of the inactive slot.

### Recommendation for preloading

P2 for A/B hosts, implemented as a small node-side `booty-update` tool that
reuses `pkg/image` streaming and checksum code. P1 only for non-A/B hosts that
need short windows. P3 through the sysext catalog of the target slot.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `booty.seed` | kernel argument | Seed URL. |
| `SEED_CA_SHA256` | hex | Pin of the CAPRF CA in the seed. |
| `STAGE_PARTITION` | `off`, `create`, `use` | P1 staging partition. |
| `STAGE_SIZE_MB` | integer | P1 size. |

### CAPRF and CAPI API impact

- `RedfishHost.spec.bootTransport`: `virtualMedia` (default), `uefiHttp`.
- `RedfishHost.status.bmcCapabilities`: virtual media, HTTP boot,
  certificate import, Secure Boot database, host interface.
- CAPRF serves a generic signed artifact per release from its image server or
  an OCI registry, and a per-machine seed endpoint.
- New standby command `stage-image` for preloading.

## Security considerations

- A generic image cannot carry a per-machine token. The seed carries a
  one-time token with a short lifetime. CAPRF revokes it after first use.
- HTTP boot over plain HTTP is not authenticated. Use a signed UKI with
  Secure Boot on, so the firmware verifies the file. Then plain HTTP is
  acceptable for the BOOTy artifact, but not for the seed content.
- SMBIOS OEM strings and the kernel command line are visible to the OS.
  Do not put long-lived secrets there.
- Redfish host interface credential bootstrapping gives the host BMC
  credentials. Disable it after use, or do not use it.
- Staged images must be verified again before use (checksum at stage time and
  at install time).

## Testing strategy

- Unit: seed parsing and precedence. Capability probe parsing with recorded
  Redfish payloads per vendor.
- KVM matrix: `uefi-http-boot` with OVMF HTTP boot (OVMF supports HTTP boot
  with the network stack enabled, verify the build flags) and a signed UKI.
  `seed-smbios` with QEMU `-smbios type=11`. `stage-image` with a staging
  partition.
- ContainerLab: none for boot. Reuse `e2e_boot` for the seed fetch over EVPN.
- kindmetal and CAPRF e2e: sushy-tools or the Redfish mock in kindmetal with
  `UefiHttp` support (verify sushy-tools support), generic ISO with a seed.
- Real hardware: capability matrix per vendor and firmware version, recorded
  as test fixtures.

## Phased rollout

1. BOOTy PR: runtime seed reader (kernel argument and SMBIOS).
2. BOOTy PR: fetch `/deploy` bundle from CAPRF with CA pin.
3. BOOTy PR: release a generic signed ISO and UKI per flavor.
4. CAPRF MR: seed endpoint and generic ISO path behind a feature gate.
5. CAPRF MR: capability discovery on `RedfishHost`.
6. CAPRF MR: `UefiHttp` transport.
7. BOOTy PR: node-side `booty-update` for A/B staging.
8. CAPRF MR: `stage-image` command.

## Effort estimate

| Phase | Days |
|-------|------|
| Seed reader and bundle fetch | 5 to 7 |
| Generic signed artifacts | 3 to 5 |
| CAPRF seed endpoint and generic ISO | 5 to 8 |
| Capability discovery | 4 to 6 |
| UEFI HTTP transport | 5 to 8 |
| Staging and `booty-update` | 8 to 12 |
| Vendor lab validation | 5 to 8 |
| **Total** | **35 to 54** |

## Open questions

- Which BMC firmware versions in the fleet support `UefiHttp` and
  `HttpBootUri`?
- Is DHCP available for the firmware HTTP boot client on the provisioning
  network? If not, HTTP boot needs static firmware network config.
- Can the generic ISO keep Secure Boot on with the current shim and kernel
  signing?

## BOOTy as a UKI, and out-of-band status

- The generic signed BOOTy artifact for HTTP boot is a UKI (kernel,
  initramfs and command line in one signed PE file), booted directly by the
  firmware without systemd-boot. The per-machine part stays in the runtime
  seed. UKI build rules are in [proposal-systemd-boot.md](proposal-systemd-boot.md).
- When the network fails during or after this boot, BOOTy reports through
  the BMC as described in
  [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md).

## Related proposals

- [proposal-uefi-management.md](proposal-uefi-management.md)
- [proposal-vendor-integrations.md](proposal-vendor-integrations.md)
- [proposal-ipmi-local.md](proposal-ipmi-local.md)
- [proposal-image-delivery.md](proposal-image-delivery.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-bmc-out-of-band-diagnostics.md](proposal-bmc-out-of-band-diagnostics.md)
