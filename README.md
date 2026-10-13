# BOOTy

[![CI](https://github.com/telekom/BOOTy/actions/workflows/ci.yml/badge.svg)](https://github.com/telekom/BOOTy/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/telekom/BOOTy)](https://goreportcard.com/report/github.com/telekom/BOOTy)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A lightweight initramfs agent for bare-metal OS provisioning over the network.

BOOTy boots as the init process inside a minimal initramfs, reads machine configuration from `/deploy/vars`, and runs the lifecycle of a bare-metal machine: disk imaging, OS configuration, network setup, and the handoff to the installed OS. The runtime entry point is the CAPRF-compatible provisioning flow. The old standalone HTTP-server flow is not shipped.

> **Warning:** This software has **no guard rails**. Incorrect use can overwrite an existing Operating System.

## Architecture

BOOTy's supported runtime flow is CAPRF-compatible provisioning driven by `/deploy/vars`:

### CAPRF Mode (Cluster API Provider Redfish)

```
┌─────────────┐     ┌─────────────────┐     ┌──────────────────────┐
│  Redfish BMC │────▶│  BOOTy initrd   │────▶│   CAPRF Controller   │
│  (ISO boot)  │     │  /deploy/vars   │     │   (status/log)       │
└─────────────┘     └───────┬─────────┘     └──────────────────────┘
                            │
               ┌────────────┼────────────┐
               │            │            │
        ┌──────▼──┐  ┌──────▼──┐  ┌──────▼──────┐
        │ Network │  │  Disk   │  │    OS       │
        │ BGP/DHCP│  │ Stream  │  │ Configure   │
        └─────────┘  └─────────┘  └─────────────┘
```

1. A Redfish BMC mounts an ISO that contains a kernel, the BOOTy initramfs, and the `/deploy/vars` file.
2. BOOTy reads `/deploy/vars` for the machine config, the image URLs, and the CAPRF endpoints. [docs/configuration.md](docs/configuration.md) lists every key.
3. BOOTy sets up the ramdisk network: **GoBGP/EVPN** (`NETWORK_MODE=gobgp`), **FRR/EVPN** (BGP underlay), **static** IP, or **DHCP**. FRR and static setup fall back to DHCP when they fail. See [docs/networking.md](docs/networking.md).
4. The provisioning pipeline validates input, cleans storage state, prepares NVMe namespaces, detects disks, applies the partition layout, streams the image, mounts shared data, optionally configures target overlayFS and sysexts, configures the OS, installs EFI fallbacks, and injects cloud-init. After a successful run, BOOTy tries kexec into the installed kernel and falls back to a hard reboot. It powers off instead with `SUCCESS_ACTION=poweroff` (preset by CAPRF legacy compatibility), or when an A/B `preserveExisting` upgrade needs kexec and kexec is not available.
5. BOOTy sends status callbacks and, with `LOG_URL`, its log lines to the CAPRF controller. See [docs/caprf-integration.md](docs/caprf-integration.md).

PXE or iPXE can still be used by an external boot environment to load the
kernel and initramfs, but BOOTy does not include a standalone `server/` package
or fetch MAC-indexed legacy config over HTTP. The initramfs must receive a
CAPRF-compatible `/deploy/vars` file.

## Features

- **CAPRF-compatible provisioning** — `/deploy/vars` driven lifecycle with status, log, debug, and artifact reporting
- **FRR/EVPN networking** — BGP underlay with VXLAN overlay for data center fabrics (FRR-based)
- **GoBGP/EVPN networking** — Pure-Go BGP stack with VXLAN overlay (no external daemons)
- **Static IP networking** — Direct IP assignment via netlink (no external tools)
- **Bond networking** — configurable bond modes, including 802.3ad/LACP on
  capable switches; the current ContainerLab bond smoke proves multi-link bond
  connectivity, not real LACP negotiation
- **DHCP fallback** — Automatic DHCP on all physical interfaces with connectivity check
- **Broad NIC driver support** — Intel (e1000e, igb, igc, ixgbe, i40e, ice), Broadcom (tg3, bnxt_en), Mellanox/NVIDIA (mlx4, mlx5), plus virtio for VMs
- **Multi-format image streaming** — Gzip, lz4, xz, zstd decompression with auto-detection
- **OCI registry support** — Pull images from OCI registries (authenticated & unauthenticated) via `oci://` URLs
- **HTTP retry with backoff** — Automatic exponential backoff retry for image downloads and OCI pulls
- **Secure erase** — NVMe format (SES1) and ATA Security Erase for full disk sanitization
- **Software RAID** — mdadm array creation (RAID 0/1/5/6/10)
- **Filesystem support** — ext2, ext3, ext4, xfs, btrfs mount/resize; vfat mount/format for EFI system partitions
- **LLDP discovery** — Raw AF_PACKET-based LLDP listener for switch topology discovery
- **Post-provision hooks** — Execute arbitrary commands in chroot after OS configuration
- **46-step provisioning pipeline** — provisioning input validation, RAID cleanup, NVMe namespace setup, RAID array setup, disk detection, partition layout, image streaming, shared data mounting, optional target overlayFS configuration, partition growth, LVM, filesystem resize, optional sysext loading, OCI image pre-pulls, OS configuration, serial console resolution, EFI fallback installation, final Secure Boot chain verification, cloud-init injection, EFI boot, Mellanox SR-IOV, post-provision hooks
- **systemd-sysext provisioning** — Optional digest-checked sysext preload or active loading into the provisioned OS image
- **Ubuntu overlayFS configuration** — Optional target-root overlayroot config for immutable Ubuntu images, with tmpfs or operator-provided persistent backing storage
- **Kexec support** — Fast reboot into installed kernel without full BIOS POST (auto-disabled after firmware changes)
- **Remote logging** — Real-time log and debug shipping to CAPRF controller
- **Startup crash artifact upload** — Best-effort pre-wipe collection of existing OS crash logs, dumps, and host metadata for CAPRF/S3 correlation
- **Hard/soft deprovisioning** — Full disk wipe or GRUB rename for reprovisioning
- **Standby mode** — Hot standby with heartbeats and command polling for sub-second provisioning
- **Cloud-init injection** — NoCloud and ConfigDrive seed generation from provisioning identity and network config
- **Netplan overlay** — Drop-in netplan YAML config from provisioner overrides `/deploy/vars` network settings
- **Network persistence renderers** — Library support for netplan, NetworkManager, and systemd-networkd config generation
- **IPMI operations** — Library support for local BMC network reads, boot device control, and chassis power
- **TPM 2.0 support** — Detection, PCR reading, metadata collection, LUKS2 TPM enrollment (Phase 2)
- **Bootloader helpers** — GRUB-oriented provisioning plus experimental bootloader detection helpers
- **BGP policy engine** — Import/export filtering, community tagging, graceful restart
- **Multi-architecture builds** — CI builds `linux/amd64` and `linux/arm64` artifacts
- **Multiple build flavors** — Full (FRR+tools), GoBGP (pure Go BGP), slim (DHCP-only), micro (pure Go), ISO (bootable)

## Prerequisites

- Go **1.26+**
- Docker (for building the initramfs)
- Redfish BMC with ISO virtual media or another boot path that supplies the BOOTy kernel, initramfs, and `/deploy/vars`

### Build Environment

| Requirement          | Version | Notes |
|----------------------|---------|-------|
| Go                   | 1.26+   | `GOOS=linux` for cross-compilation on macOS/Windows |
| Docker / Buildx      | 20.10+  | Multi-arch builds (`linux/amd64`, `linux/arm64`) |
| golangci-lint        | v2.12.2 | Installed into `bin/` by `make lint`, config in `.golangci.yml` |
| GNU Make             | 4.0+    | Build automation |
| ContainerLab         | 0.44+   | E2E tests only (Linux) |
| KVM / QEMU           | —       | E2E boot tests only (Linux) |

### Supported and Tested Scope

BOOTy is Linux-only runtime software: it runs as PID 1 inside an initramfs and
uses Linux syscalls, mounts, block devices, netlink, KVM/QEMU test coverage,
and ContainerLab networking tests. macOS and Windows are supported only as
cross-compilation hosts for the Go binary.

| Scope | Current CI proof |
|-------|------------------|
| Go binary | `linux/amd64` and `linux/arm64` build jobs |
| Initramfs artifacts | `linux/amd64` default, slim, micro, and GoBGP build-flavor jobs plus path-filtered `linux/arm64` flavor packaging for initramfs input changes; release builds publish default, slim, micro, and GoBGP for both `linux/amd64` and `linux/arm64` |
| Boot/provisioning behavior | x86_64 KVM/QEMU jobs on Ubuntu GitHub runners |
| Network integration | ContainerLab and vrnetlab jobs on privileged Linux runners, with nightly smoke coverage for DHCP, static, multi-NIC, and bond topologies |
| Network persistence | Unit tests for Ubuntu/netplan, RHEL/NetworkManager, and Flatcar/systemd-networkd writers plus explicit provisioning opt-in wiring |

CI does not currently prove macOS/Windows runtime behavior, non-Linux boot
targets, vendor Flatcar images, VMware ESXi provisioning, Windows target
provisioning, SUSE/openSUSE-specific provisioning, Fedora-specific
provisioning, RHEL/Rocky/Alma target bootloader behavior, Debian target image
first boot, or automatic target-OS detection for persistent network
configuration.

Target OS support is limited to behavior the repository implements and tests:

Set `PROVISION_TARGET_OS=linux` (or `TARGET_OS=linux`) to explicitly declare a
Linux-compatible target image. Non-Linux values such as `windows` and `esxi`
fail during preflight before BOOTy reaches disk wipe, partitioning, or image
streaming. Leaving it empty also fails provision preflight; BOOTy does not
treat undeclared or unknown images as safe for destructive provisioning.

| Target OS family | Current status |
|------------------|----------------|
| Generic Linux images | Supported at the image/disk/provisioning level when the target image is compatible with the GRUB-oriented provisioning flow and required target-side tools/files are present. Current CI uses synthetic Linux images on Ubuntu runners rather than real distro cloud images. |
| Ubuntu/Debian-like images | Common examples and GRUB/update-grub assumptions exist, but CI does not prove first boot of a real Ubuntu or Debian target image with cloud-init, netplan, or systemd applying the generated files. |
| RHEL/Rocky/Alma/Fedora | Not target-OS proven. Unit tests cover NetworkManager keyfile rendering and some RHEL-like labels, but active provisioning does not implement native GRUB2/BLS/vendor EFI paths, SELinux relabeling, or distro-specific first-boot validation. |
| SUSE/openSUSE/SLES | Not target-OS proven. Some openSUSE source-root labels and an SLES secure-boot verifier path exist, but there is no SUSE network persistence writer, native GRUB2 handoff, package/init integration, or distro-specific first-boot validation. |
| Flatcar | Unit tests cover systemd-networkd rendering and source-root selection can handle explicit `USR-A`/`USR-B` labels. The existing KVM test uses a Flatcar-like synthetic source layout, not a real Flatcar vendor image, Ignition, update-engine, or Nebraska flow. Cloud-init injection is rejected for `OS_FAMILY=flatcar` because BOOTy does not implement Ignition. |
| VMware ESXi | Unsupported and unclaimed. The repository has no ESXi/VMware/vSphere/VMFS provisioning path or CI coverage. |
| Windows | Unsupported as a BOOTy runtime or provisioned target OS. Windows is mentioned only as a possible Go cross-compilation host. |

## Building

### BOOTy Binary

Compile the BOOTy binary for the configured Go target:

```bash
make build
```

### Initramfs (recommended)

Build the full amd64 initramfs container image with Docker:

```bash
make dockerx86
```

To emit a local bootable artifact instead of loading a Docker image, use one of
the build-flavor targets below, for example `make gobgp` or `make iso`. These
Docker targets compile BOOTy and assemble a bootable initramfs or ISO; the
included networking stack, disk tools, firmware tools, and kernel modules vary
by flavor.

To extract the initramfs to the local filesystem:

```bash
ID=$(docker create ghcr.io/telekom/booty:latest null)
docker cp "$ID:/initramfs.cpio.zst" initramfs.cpio.zst
docker rm "$ID"
```

### Build Flavors

The `initrd.Dockerfile` supports multiple build targets via `--target`:

| Target | Size | Networking | Disk Tools | Use Case |
|--------|------|-----------|------------|----------|
| *(default)* | ~80 MB | FRR/EVPN + DHCP | Full (LVM, sfdisk, mdadm) | Production bare-metal provisioning |
| `gobgp` | ~45 MB | GoBGP/EVPN + DHCP | Full (LVM, sfdisk, mdadm) | Production without FRR dependency |
| `iso` | ~100 MB | FRR/EVPN + DHCP | Full | Bootable ISO for Redfish virtual media |
| `gobgp-iso` | ~65 MB | GoBGP/EVPN + DHCP | Full | Bootable GoBGP ISO for Redfish virtual media |
| `slim` | ~15 MB | DHCP only | Provisioning basics, no LVM/FRR | Lightweight provisioning without BGP |
| `micro` | ~10 MB | None (pure Go) | None | Minimal agent, custom network stack |

#### ARM64 Targets

| Make Target | Description |
|-------------|-------------|
| `make arm64` | Full initramfs Docker image for ARM64 |
| `make arm64-slim` | Slim initramfs for ARM64 (output to `dist/arm64/`) |
| `make arm64-gobgp` | GoBGP initramfs for ARM64 (output to `dist/arm64/`) |
| `make build-all` | Cross-compile Go binary for both amd64 and arm64 |
| `make oci-push` | Push binary + initramfs as OCI artifacts to GHCR |
| `make oci-push-initramfs` | Push initramfs only as OCI artifact |
| `make oci-push-binary` | Push binary only as OCI artifact |

```bash
# Build ISO (for Redfish BMC virtual media boot)
docker build --target=iso -f initrd.Dockerfile -o type=local,dest=. .

# Build slim initramfs
docker build --target=slim -f initrd.Dockerfile -o type=local,dest=. .

# Build ARM64 GoBGP initramfs
make arm64-gobgp
```

### Binary only

```bash
GOOS=linux go build -o booty .
```

## Hardware Compatibility

### Network Interface Cards

BOOTy includes kernel modules for common data center NICs. Modules are loaded
automatically at boot from the `/modules/` directory in the initramfs.

| Vendor | Driver | Hardware | Speed |
|--------|--------|----------|-------|
| **Intel** | `e1000e` | I217/I218/I219 | 1G |
| **Intel** | `igb` | I350, I210/I211 | 1G |
| **Intel** | `igc` | I225/I226 | 2.5G |
| **Intel** | `ixgbe` | X520, X540, X550 (82599) | 10G |
| **Intel** | `i40e` | X710, XL710, XXV710 | 10/25/40G |
| **Intel** | `ice` | E810 | 25/50/100G |
| **Intel** | `iavf` | Adaptive Virtual Function | VF |
| **Broadcom** | `tg3` | NetXtreme BCM57xx | 1G |
| **Broadcom** | `bnxt_en` | NetXtreme-E/C BCM573xx/574xx | 10/25/50/100G |
| **NVIDIA/Mellanox** | `mlx4_core`/`mlx4_en` | ConnectX-3 | 10/40G |
| **NVIDIA/Mellanox** | `mlx5_core` | ConnectX-4/5/6/7, BlueField | 10/25/40/50/100/200/400G |
| **Emulex/Broadcom** | `be2net` | OneConnect OCe14xxx | 10G |
| **QEMU/KVM** | `virtio_net` | VirtIO NIC | Virtual |

**Mellanox SR-IOV**: ConnectX-4+ cards are automatically detected via sysfs PCI vendor
ID (`/sys/bus/pci/devices/*/vendor`) — no `lspci` binary needed. SR-IOV is configured
using `mstconfig` during provisioning (requires a hard reboot to apply firmware changes).

## Usage

### CAPRF Mode

BOOTy expects `/deploy/vars` at startup. The CAPRF controller or an equivalent
provisioner generates it. Example:

```bash
# Mode: provision, deprovision, soft-deprovision, decommission, standby,
# dry-run or check.
export MODE="provision"
export PROVISION_TARGET_OS="linux"
export IMAGE="https://images.example.com/ubuntu-24.04.img.gz"
export HOSTNAME="worker-01"
export TOKEN="bearer-token-for-auth"
export PROVIDER_ID="redfish://bmc/Systems/1"
export FAILURE_DOMAIN="az-1"
export REGION="eu-central"
export INIT_URL="https://caprf.example.com/status/init"
export SUCCESS_URL="https://caprf.example.com/status/success"
export ERROR_URL="https://caprf.example.com/status/error"
export LOG_URL="https://caprf.example.com/log"

# Optional JWT authentication. Omit it to use TOKEN as a static bearer token.
export TOKEN_URL="https://caprf.example.com/auth/token"
export TOKEN_ALGORITHM="RS256"

# Optional FRR/EVPN network. Omit it for DHCP.
underlay_ip="10.0.0.11"
asn_server="65001"
provision_vni="10100"
overlay_subnet="fd00::/64"
dns_resolver="8.8.8.8"
```

Comments must be on their own lines: BOOTy keeps text after a value as part of
the value. BOOTy sends the bearer token only to HTTPS or loopback URLs, unless
`INSECURE_TRANSPORT=true`. Plain `http://` image URLs need
`IMAGE_ALLOW_INSECURE_HTTP=true`. `CAPRF_LEGACY_COMPAT=true` presets both. When
`TOKEN_URL` is set and the JWT exchange or renewal fails, BOOTy reboots. With
`FAILURE_ACTION=wait` (preset by `CAPRF_LEGACY_COMPAT=true`) it reports the
error and keeps the ramdisk running.
[docs/configuration.md](docs/configuration.md) describes the file format and
every key.

`IMAGE` must reference a raw disk image, a supported compressed raw image
(`.gz`, `.lz4`, `.xz`, `.zst`, `.bz2`), a QCOW2 image, or an OCI layer that
contains a supported image payload. VMware VMDK, OVA, and OVF containers are
not provisioning inputs; convert them to raw or QCOW2 before publishing.
A QCOW2 image must be self-contained. BOOTy rejects QCOW2 images that name a
backing file or an external data file; flatten them first, for example with
`qemu-img convert -O qcow2 in.qcow2 out.qcow2`. With more than one URL in
`IMAGE`, BOOTy probes each URL and streams from the fastest one.

### Removed Legacy Server Flow

Older BOOTy revisions and proposals referenced `server/server.go` with
`writeImage` and `readImage` actions. That standalone provisioning server is
not present in this repository. Use `/deploy/vars` and the CAPRF-compatible
MachineConfig fields instead of `server/server.go` command-line flags.

### LVM & Disk Growth

Disk growth, filesystem resize, and LVM operations are driven by MachineConfig
and `/deploy/vars`, then executed by the provisioning pipeline after the target
root is mounted.

### Feature Gates

| Variable | Default | Description |
|----------|---------|-------------|
| `MODE` | `provision` | `provision`, `deprovision`, `soft-deprovision`, `decommission`, `standby`, or `dry-run`. `decommission` wipes all disks and removes every EFI boot entry listed in `BootOrder` on a best-effort basis (CAPRF force-clean) |
| `DRY_RUN` | `false` | When `true`, forces `MODE=dry-run`: validates prerequisites without destructive writes |
| `BOOTY_RESUME` | `false` | Enable checkpoint resume — skip previously completed steps on restart |
| `DISABLE_KEXEC` | `false` | Skip kexec, always hard-reboot. Must stay `false` for A/B preserve-existing upgrades |
| `CAPRF_LEGACY_COMPAT` | `false` | Preset that behaves like the CAPRF legacy shell provisioner. See [CAPRF legacy compatibility](#caprf-legacy-compatibility) |
| `SUCCESS_ACTION` | `reboot` | `poweroff` powers off after a successful run instead of kexec or reboot. Preset by `CAPRF_LEGACY_COMPAT` |
| `FAILURE_ACTION` | `reboot` | `wait` reports a failed run and keeps the ramdisk running. Default of `RESCUE_MODE`. Preset by `CAPRF_LEGACY_COMPAT` |
| `MIN_DISK_SIZE_GB` | `0` | Minimum disk size filter (0 = no minimum) |
| `DISK_SELECTION` | `prefer-nvme` | `lsblk` selects the first large enough disk in legacy `lsblk` order. Preset by `CAPRF_LEGACY_COMPAT` |
| `WIPE_ALL_DISKS` | `false` | Wipe every disk, not only the target, like `wipe_all_disks`. Preset by `CAPRF_LEGACY_COMPAT` |
| `DISCARD_NVME` | `false` | `blkdiscard` every NVMe disk before a wipe of all disks. Preset by `CAPRF_LEGACY_COMPAT` |
| `NETWORK_WAIT_MAX_ATTEMPTS` | unset (15) | CAPRF legacy compat only: 20 second network wait rounds before the run fails (0-100000). `0` means no round: the run fails at once, like the legacy provisioner |
| `BOOTY_ALLOW_REMOVABLE` | `false` | Allow USB/removable media as provisioning target |
| `MACHINE_EXTRA_KERNEL_PARAMS` | — | Additional kernel cmdline parameters |
| `EXTRA_KERNEL_PARAMS` | — | Additional kernel cmdline parameters, read by the CAPRF legacy provisioner. Appended to `MACHINE_EXTRA_KERNEL_PARAMS` |
| `MACHINE_SERIAL_CONSOLE` | — | Pin the serial console of the provisioned system (e.g. `ttyS1,115200n8`), or `none` to disable it. Overrides ACPI SPCR / device-tree / sysfs evidence. Alias: `SERIAL_CONSOLE` |
| `INIT_URL` | — | CAPRF init status endpoint |
| `SUCCESS_URL` | — | CAPRF success status endpoint |
| `ERROR_URL` | — | CAPRF error status endpoint |
| `LOG_URL` | — | CAPRF structured log endpoint |
| `DEBUG_URL` | — | CAPRF debug payload endpoint |
| `HEARTBEAT_URL` | — | Standby mode: URL for periodic keepalives |
| `COMMANDS_URL` | — | Standby mode: URL to poll for pending commands |
| `TOKEN_URL` | — | JWT token acquisition endpoint (HTTPS required except localhost) |
| `TOKEN_ALGORITHM` | — | JWT algorithm override: `RS256` or `ES256` |
| `SECURE_ERASE` | `false` | Use NVMe format / ATA secure erase instead of partition wipe |
| `INSECURE_TRANSPORT` | `false` | Allow insecure HTTP connections. Preset by `CAPRF_LEGACY_COMPAT` |
| `SKIP_TLS_VERIFY` | `false` | Skip TLS certificate checks for CAPRF callbacks, the connectivity check and images, like `curl --insecure`. Never for `TOKEN_URL`. Preset by `CAPRF_LEGACY_COMPAT` |
| `EXPORT_VARS` | `false` | Export every `/deploy/vars` entry to the environment, like `source vars`. Preset by `CAPRF_LEGACY_COMPAT` |
| `PARTITION_LAYOUT` | — | Custom partition layout specification |
| `DISK_SERIAL` | — | Target specific disk by serial number |
| `RESCUE_PASSWORD_HASH` | — | Password hash for rescue mode access |
| `POST_PROVISION_CMDS` | — | Semicolon-separated commands to run in chroot after provisioning |
| `RESCUE_MODE` | `reboot` | Failure recovery strategy: `reboot`, `retry`, `shell`, `wait` |
| `RESCUE_TIMEOUT` | `0` | *(Phase 2)* Rescue wait timeout in seconds (0 = indefinite) |
| `RESCUE_SSH_PUBKEY` | — | *(Phase 2)* SSH public key for rescue shell access |
| `RESCUE_AUTO_MOUNT` | `false` | *(Phase 2)* Auto-mount disks in rescue shell mode |
| `EVPN_L2_ENABLED` | `false` | Enable EVPN L2 overlay (Type-2/3 route origination and handling) in GoBGP mode |
| `EVPN_TYPE5_ONLY` | `false` | Explicitly select direct EVPN Type-5 routing without an L2 gateway; mutually exclusive with `EVPN_L2_ENABLED` |
| `HEALTH_CHECKS_ENABLED` | `false` | Run pre-provisioning hardware health checks |
| `HEALTH_MIN_MEMORY_GB` | `0` | Minimum RAM (GiB) for health check (0 = skip check) |
| `HEALTH_MIN_CPUS` | `0` | Minimum CPU count for health check (0 = skip check) |
| `HEALTH_SKIP_CHECKS` | — | Comma-separated check names to skip (e.g., `thermal,disk-smart`) |
| `HEALTH_CHECK_URL` | — | POST endpoint for health check results |
| `INVENTORY_ENABLED` | `false` | Collect and report hardware inventory to CAPRF |
| `INVENTORY_URL` | — | POST endpoint for hardware inventory JSON |
| `FIRMWARE_REPORT` | `false` | Enable firmware version collection and reporting |
| `FIRMWARE_URL` | — | POST endpoint for firmware report |
| `FIRMWARE_MIN_BIOS` | — | Minimum BIOS version (vendor-specific string) |
| `FIRMWARE_MIN_BMC` | — | Minimum BMC firmware version; fails closed when no real BMC firmware source reports a version |
| `TELEMETRY_ENABLED` | `false` | Enable provisioning metrics and telemetry collection |
| `TELEMETRY_URL` | — | POST endpoint for telemetry snapshot |
| `METRICS_URL` | — | POST endpoint for provisioning metrics |
| `EVENT_URL` | — | POST endpoint for provisioning lifecycle events |
| `CRASH_ARTIFACTS_ENABLED` | `false` | Inspect the existing OS before destructive actions and upload crash artifacts when evidence is found |
| `CRASH_ARTIFACTS_PREPARE_URL` | — | CAPRF endpoint that receives crash metadata and returns upload instructions |
| `CRASH_ARTIFACTS_UPLOAD_URL` | — | Direct CAPRF proxy endpoint for crash archive uploads when no prepare endpoint is used |
| `CRASH_ARTIFACTS_MAX_MB` | `256` | Maximum crash artifact payload size in MiB |
| `CRASH_ARTIFACTS_UPLOAD_TIMEOUT_SEC` | `120` | Timeout for crash artifact prepare/upload requests |
| `CRASH_ARTIFACTS_INCLUDE_MEMORY_DUMPS` | `false` | Include raw `vmcore` and systemd coredump files; disabled by default because these can contain process memory and secrets |
| `SECUREBOOT_REENABLE` | `false` | Signal CAPRF to re-enable Secure Boot after provisioning |
| `SECUREBOOT_SHIM_SHA256` | — | Expected SHA256 digest for the target shim EFI artifact; accepts bare hex or `sha256:<hex>` |
| `SECUREBOOT_GRUB_SHA256` | — | Expected SHA256 digest for the target GRUB EFI artifact; accepts bare hex or `sha256:<hex>` |
| `SECUREBOOT_KERNEL_SHA256` | — | Expected SHA256 digest for the target kernel artifact; accepts bare hex or `sha256:<hex>` |
| `SECUREBOOT_PINNED_DIGESTS` | — | JSON map of component pins keyed by `shim`, `grub`, and `kernel`; individual digest vars override only their component |
| `MOK_CERT_PATH` | — | *(Phase 2)* Path to DER-encoded MOK certificate for custom kernel signing |
| `MOK_PASSWORD` | — | *(Phase 2)* One-time password for MokManager confirmation |
| `IMAGE_CHECKSUM` | — | Expected hex digest of the raw disk image |
| `IMAGE_CHECKSUM_TYPE` | — | Checksum algorithm: `sha256` or `sha512` |
| `IMAGE_MODE` | `whole-disk` | Image write mode: `whole-disk`, `partition`, or `ab`; with a declarative partition layout, `whole-disk` streams the selected root filesystem into the declared root partition |
| `IMAGE_SOURCE_ROOT_LABEL` | — | Source-image GPT partition label to stream into a declarative partition layout root |
| `IMAGE_SOURCE_ROOT_PARTITION` | — | 1-based source-image partition number for declarative partition layout root streaming |
| `OCI_PREPULL_ENABLED` | `false` | Pull configured OCI images during provisioning and install a first-boot runtime importer |
| `OCI_PREPULL_IMAGES` | — | Comma- or space-separated OCI image references to cache for first boot; `oci://` prefixes are accepted |
| `OCI_PREPULL_CACHE_DIR` | `/var/lib/booty/oci-prepulls` | Target-root directory for cached image archives, catalog, import list, and import state |
| `OCI_PREPULL_IMPORT_NAMESPACE` | `k8s.io` | containerd/nerdctl namespace used by the first-boot importer |
| `OCI_PREPULL_ALLOW_INSECURE` | `false` | Allow insecure HTTP registry access for controlled local or air-gapped labs |
| `AB_SCHEME` | `dual-root` | A/B partitioning scheme for `IMAGE_MODE=ab` |
| `AB_ACTIVE_SLOT` | — | Currently booted A/B slot, `a` or `b` |
| `AB_TARGET_SLOT` | `inactive` | A/B slot to write: `inactive`, `a`, or `b` |
| `AB_PRESERVE_EXISTING` | `false` | Reuse an existing A/B layout and write only the target root slot |
| `AB_BOOT_SIZE_MB` | `512` | Shared EFI partition size for generated A/B layouts |
| `AB_ROOT_SIZE_MB` | `32768` | Size of each generated A/B root slot |
| `AB_STATE_SIZE_MB` | `0` | Persistent state partition size; `0` fills remaining disk |
| `AB_DATA_PARTITIONS` | — | JSON list of shared data partitions for `AB_SCHEME=system-ab`; the single `sizeMB:0` fill-remaining partition must be last |
| `AB_SOURCE_ROOT_LABEL` | — | Source-image GPT partition label to copy into the target root slot |
| `AB_SOURCE_ROOT_PARTITION` | — | 1-based source-image partition number to copy when no stable label exists |
| `DISK_DEVICE` | auto-detect | Explicit disk device path override (e.g. `/dev/sda`) |
| `DISK_SERIAL_NUMBER` | — | Select the target disk by exact sysfs serial number when `DISK_DEVICE` is unset |
| `ROOT_PARTITION_LABEL` | — | Whole-disk image root GPT partition label/PARTLABEL; use when multiple Linux partitions exist |
| `ROOT_PARTITION_NUMBER` | — | Whole-disk image root partition number; use only when no stable root partition label exists |
| `IMAGE_SIGNATURE_URL` | — | URL to detached GPG signature for image verification |
| `IMAGE_GPG_PUBKEY` | — | Path to GPG public key for image signature verification |
| `LUKS_ENABLED` | `false` | *(Planned, not accepted: a `LUKS_*` key in `/deploy/vars` fails the run)* Enable LUKS2 encryption for target partitions |
| `LUKS_PASSPHRASE` | (unset) | *(Planned, not accepted: a `LUKS_*` key in `/deploy/vars` fails the run)* Passphrase for initial LUKS volume creation |
| `LUKS_UNLOCK_METHOD` | `passphrase` | *(Planned, not accepted: a `LUKS_*` key in `/deploy/vars` fails the run)* Unlock method: `passphrase`, `tpm2`, `clevis`, `keyfile` |
| `LUKS_CIPHER` | `aes-xts-plain64` | *(Planned, not accepted: a `LUKS_*` key in `/deploy/vars` fails the run)* LUKS2 cipher algorithm |
| `LUKS_KEY_SIZE` | `512` | *(Planned, not accepted: a `LUKS_*` key in `/deploy/vars` fails the run)* LUKS2 key size in bits |
| `LUKS_HASH` | `sha256` | *(Planned, not accepted: a `LUKS_*` key in `/deploy/vars` fails the run)* LUKS2 hash algorithm |
| `NUM_VFS` | `0` | Number of SR-IOV virtual functions for Mellanox NICs (0 = skip) |
| `NVME_NAMESPACES` | — | JSON config for NVMe namespace creation (e.g. `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`) |
| `CLOUDINIT_ENABLED` | `false` | Generate and inject cloud-init config |
| `CLOUDINIT_DATASOURCE` | `nocloud` | Cloud-init datasource type: `nocloud` or `configdrive` |
| `SYSEXT_ENABLED` | `false` | Copy configured systemd-sysext layers into the provisioned root |
| `SYSEXT_DEFAULT_MODE` | `preload` | Default sysext layer mode: `preload` writes an inactive catalog; `active` writes to a systemd-sysext search path |
| `SYSEXT_CATALOG_DIR` | `/usr/lib/tcaas-sysext/preloaded` | Target inactive catalog directory for preloaded sysext layers; must not be an active systemd-sysext search path |
| `SYSEXT_ACTIVE_DIR` | `/var/lib/extensions` | Target directory for active sysext layers |
| `SYSEXT_LAYERS` | — | JSON array of sysext layer objects |
| `SYSEXT_ALLOW_INSECURE_HTTP` | `false` | Allow plain HTTP sysext sources on controlled provisioning networks |
| `OVERLAYFS_ENABLED` | `false` | Write Ubuntu overlayroot config into the provisioned target root |
| `OVERLAYFS_MODE` | `tmpfs` | Overlayroot mode: `tmpfs` for volatile writes or `device` for persistent backing storage |
| `OVERLAYFS_DEVICE` | — | Backing filesystem for `OVERLAYFS_MODE=device`, e.g. `LABEL=BOOTY-OVERLAY` or `/dev/disk/by-label/BOOTY-OVERLAY` |
| `OVERLAYFS_DIRECTORY` | — | Optional overlayroot `dir=` value under the backing filesystem |
| `OVERLAYFS_UPPER_DIR` | — | Optional target-root directory BOOTy pre-creates for custom overlayFS initramfs hooks |
| `OVERLAYFS_WORK_DIR` | — | Optional target-root work directory; must be set together with `OVERLAYFS_UPPER_DIR` |
| `OVERLAYFS_TIMEOUT_SEC` | `0` | Seconds overlayroot should wait for `OVERLAYFS_DEVICE` in `device` mode |
| `OVERLAYFS_RECURSE` | `false` | Render overlayroot `recurse=1`; default `false` keeps separate data mounts writable |
| `OVERLAYFS_SWAP` | `false` | Render overlayroot `swap=1` |
| `OVERLAYFS_DEBUG` | `false` | Render overlayroot `debug=1` |

#### Network Variables

BOOTy selects the ramdisk network stack from the vars: GoBGP with
`NETWORK_MODE=gobgp`, FRR with `asn_server` and `underlay_subnet` or
`underlay_ip`, static with `STATIC_IP`, otherwise DHCP. Bond and VLAN keys
(`BOND_INTERFACES`, `VLANS`) add link layers below the stack. Netplan files
from the provisioner override these keys.

[docs/configuration.md](docs/configuration.md#common-network) lists the
network keys in the Common network, GoBGP and EVPN, and FRR sections. [docs/networking.md](docs/networking.md)
describes the stack selection, the peer modes and the EVPN route modes.

The checked-in `topology-lacp.clab.yml` file is retained for compatibility with
older `clab-lacp-*` target names. It uses a Linux bridge and validates
multi-link bond connectivity only; it does not prove 802.3ad/LACP negotiation
against a LACP-capable switch.

### CAPRF legacy compatibility

CAPRF sets `CAPRF_LEGACY_COMPAT=true` when it runs BOOTy in place of its
legacy shell provisioner (`internal/provision/files/*.sh` in
cluster-api-provider-redfish). The variable is a preset: it sets the options
that the vars file does not set, and an explicit option wins. The options are
listed in [CAPRF legacy compatibility](docs/configuration.md#caprf-legacy-compatibility).
With the variable set, BOOTy keeps the legacy behaviour where the two differ:

- After a successful provision or deprovision, BOOTy powers the host off
  (`SUCCESS_ACTION=poweroff`). It does not kexec or reboot. CAPRF then ejects
  the ISO and powers the host on.
- After a failed provision or deprovision, BOOTy reports the error and keeps
  the ramdisk running (`FAILURE_ACTION=wait`), like `RESCUE_MODE=wait`. An
  explicit `RESCUE_MODE` still wins for provisioning.
- `setup-mellanox` raises `NUM_OF_VFS` to 32 (or `NUM_VFS`) with `mstconfig`
  on every ConnectX-4/5/6/6 Dx/6 Lx/7 Ethernet function, like
  `setup_mellanox_num_of_vfs`, and skips quietly without `mstconfig` or such
  NICs. Without the variable `NUM_VFS` stays opt-in.
- Without `DISK_DEVICE` or `DISK_SERIAL`, the target disk is the first disk
  (`DISK_SELECTION=lsblk`) of at least `MIN_DISK_SIZE_GB` in `lsblk` order, NVMe and virtio disks
  first, then SCSI/SATA disks, like `_select_single_disk`. BOOTy otherwise
  prefers NVMe and then the largest disk. Like the legacy script, this order
  does not skip removable media: a USB disk (SCSI major 8) can be selected
  when no NVMe or virtio disk qualifies, even without `BOOTY_ALLOW_REMOVABLE`.
- GRUB gets `GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud
  console=ttyS0,115200n8 <extra>"` in `10-caprf-kernel-params.cfg`, with
  `ttyS1` on Lenovo hosts, and `update-grub` runs only when the image has it.
  BOOTy does not resolve the console from firmware and does not write getty
  units in this mode.
- Transport follows the legacy `curl --insecure` calls: CAPRF callbacks and
  image URLs may use plain HTTP (`INSECURE_TRANSPORT=true`,
  `IMAGE_ALLOW_INSECURE_HTTP=true`), and TLS certificates are not verified
  for callbacks, the connectivity check and image downloads
  (`SKIP_TLS_VERIFY=true`).
- Every entry of `/deploy/vars` is exported to the environment, like
  `source vars` (`EXPORT_VARS=true`). Proxy variables such as `https_proxy` from CAPRF
  `additionalVars` apply to the CAPRF callbacks, the connectivity check and
  the image downloads.
- Files in `/deploy/file-system` (CAPRF `provisionerConfig.files`) are copied
  into the ramdisk root before the network starts, like `copy_files`. Netplan
  files get mode `0600`. They are not copied into the installed OS, and BOOTy
  does not write the installed OS `/etc/resolv.conf`. A provisioner
  `/etc/resolv.conf` stays in place in the ramdisk; BOOTy does not replace it
  with resolvers from the vars or netplan.
- `wipe-disks` wipes every disk, not only the target disk, for provisioning
  and hard deprovisioning, like `wipe_all_disks` (`WIPE_ALL_DISKS=true`):
  `blkdiscard` on NVMe disks (`DISCARD_NVME=true`), then signature wipes.
  `MODE=decommission` also runs `blkdiscard` on every NVMe disk in this mode. Failures are logged and do not stop the run. This
  does not apply to dry-run, A/B preserve-existing upgrades, explicit RAID
  configs or a deprovision disk override (`DISK_DEVICE`); those keep the
  BOOTy wipe rules.
- For provisioning, the ramdisk takes the machine `HOSTNAME`, like
  `hostname "$HOSTNAME"` in `provision.sh`.
- BOOTy logs the firmware version of every NIC (`nic_firmware`), like
  `provision.sh`, also without `FIRMWARE_REPORT`.
- `grow-partition` grows the last GPT Linux filesystem partition, like
  `provision.sh`, also when it is a data partition after the root. That
  partition then gets `e2fsck` and `resize2fs`, and the root filesystem keeps
  its size. BOOTy otherwise grows the root partition.
- The network wait follows `wait_for_network`: `NETWORK_WAIT_MAX_ATTEMPTS`
  rounds of 20 seconds (default 15, about 5 minutes) on one network setup.
  In FRR mode, FRR restarts after each failed round unless a BGP session is
  established. The limit covers the whole wait, also the GoBGP session wait.
  Then BOOTy reports the error. BOOTy otherwise sets up the network up to 3
  times and waits up to 5 minutes each time.

Without the variable, BOOTy keeps its own defaults.

### Debugging

BOOTy takes no command-line flags. Use these settings instead:

- `MODE=dry-run` or `DRY_RUN=true` runs read-only checks. See
  [Dry-Run Mode](#dry-run-mode).
- `RESCUE_MODE=shell` or `RESCUE_MODE=wait` keeps the ramdisk available after a
  failed provisioning. See [Rescue Mode](#rescue-mode).
- When a step fails, BOOTy logs a debug dump: block devices, mounts, links,
  routes, FRR or GoBGP state, and the `BOOTY_*`, `MODE` and `NETWORK_MODE`
  environment entries with secrets redacted. With `LOG_URL`, the dump goes to
  CAPRF with the other log lines.

### Resilient Provisioning

BOOTy retries steps with known transient failures and can resume a failed
provisioning run from a checkpoint.

**Automatic retries.** These steps use exponential backoff with jitter.
`DefaultPolicies` in `pkg/provision/retry.go` defines them:

| Step | Max Retries | Initial Delay | Max Delay |
|------|-------------|---------------|-----------|
| `report-init` | 5 | 2 s | 30 s |
| `configure-dns` | 5 | 1 s | 15 s |
| `stream-image` | 3 | 5 s | 60 s |
| `setup-raid` | 2 | 2 s | 10 s |
| `detect-disk` | 3 | 2 s | 10 s |
| `partprobe` | 3 | 1 s | 5 s |
| `create-efi-boot-entry` | 2 | 1 s | 5 s |
| `report-success` | 5 | 2 s | 30 s |

BOOTy never retries a `PermanentError`. It retries other errors only when the
step policy has `Transient: true`. All policies above have it.

**Checkpoint resume.** Set `BOOTY_RESUME=true` in the process environment, not
as a normal vars key. See
[Process environment](docs/configuration.md#process-environment). BOOTy then
saves the checkpoint to `/tmp/booty-checkpoint.json` after each step. The file
is in the ramdisk, so it helps only when provisioning runs again in the same
boot, for example with `RESCUE_MODE=retry`. A new run skips the completed
steps, except the steps that rebuild runtime state or check safety rules:
`validate-provision-inputs`, `verify-image`, `mount-efivarfs`,
`setup-mellanox`, `detect-disk`, `parse-partitions`, `enable-lvm`,
`mount-root`, `mount-boot`, `mount-shared-data`, `setup-chroot-binds`,
`configure-serial-console`, `configure-grub` and `teardown-chroot`.

When no checkpoint exists, BOOTy starts a new one. When the checkpoint file
can not be read or decoded, provisioning fails with `load checkpoint`. Remove
`/tmp/booty-checkpoint.json` to force a new run.

### Rescue Mode

`RESCUE_MODE` sets what BOOTy does after a failed provisioning: `reboot`
(default), `retry`, `shell` or `wait` (default with `FAILURE_ACTION=wait`,
which `CAPRF_LEGACY_COMPAT=true` presets). In `shell` and `wait` modes, BOOTy can give SSH
access with `RESCUE_SSH_PUBKEY` and mount the found disks.

```bash
export RESCUE_MODE=wait
export RESCUE_SSH_PUBKEY="ssh-ed25519 AAAAC3NzaC1... admin@ops"
export RESCUE_AUTO_MOUNT=true
```

[Rescue modes](docs/caprf-integration.md#rescue-modes) describes each mode.
[Rescue](docs/configuration.md#rescue) lists the keys.

### Dry-Run Mode

Dry-run mode runs read-only checks instead of the provisioning pipeline. It
makes no changes to disks or EFI variables, and it does not set up the ramdisk
network or the JWT flow.

```bash
export MODE="dry-run"
```

`DRY_RUN=true` has the same effect. The checks are: config validation, image
reachability, image prerequisites such as `qemu-img` for QCOW2 sources, image
checksum format, image signature settings, image mode, disk detection, NIC
link state, EFI variable access, health checks, and an inventory probe. BOOTy
sends a text summary of all results in the status message: an error status
when a check fails, otherwise a success status.

### Health Checks

Health checks validate the machine before the destructive steps. Enable them
with `HEALTH_CHECKS_ENABLED=true`. `MODE=check` runs only the checks.

```bash
export HEALTH_CHECKS_ENABLED=true
# Fail when the host has less than 64 GiB RAM or fewer than 16 CPUs.
export HEALTH_MIN_MEMORY_GB=64
export HEALTH_MIN_CPUS=16
# Check names, separated by commas.
export HEALTH_SKIP_CHECKS=thermal-state
export HEALTH_CHECK_URL="https://caprf.example.com/health"
```

| Check | Severity | Fails when |
|-------|----------|------------|
| `disk-presence` | Critical | No eligible disk is found |
| `minimum-memory` | Critical | RAM is below `HEALTH_MIN_MEMORY_GB` |
| `minimum-cpu` | Critical | The CPU count is below `HEALTH_MIN_CPUS` |
| `memory-ecc` | Critical | The memory controllers report uncorrectable ECC errors |
| `disk-ioerr` | Warning | A disk reports a non-zero `ioerr_cnt` |
| `nic-link-state` | Warning | A physical NIC has no carrier |
| `thermal-state` | Warning | A thermal zone is above 95 °C |

BOOTy posts the results to `HEALTH_CHECK_URL` when it is set. A failed critical
check stops provisioning. A failed warning check is only logged.

### Hardware Inventory

BOOTy collects the system, CPU, memory, disk, NIC, PCI device and accelerator
data from sysfs and procfs, and posts it to `INVENTORY_URL`. The
`collect-inventory` step runs early in provisioning.

```bash
export INVENTORY_ENABLED=true
export INVENTORY_URL="https://caprf.example.com/inventory"
```

Inventory collection is best-effort: BOOTy logs failures and continues.

### Firmware Reporting

`FIRMWARE_REPORT=true` collects the BIOS, BMC vendor, NIC and storage
controller firmware versions and posts them to `FIRMWARE_URL`. It can also
enforce minimum versions.

```bash
export FIRMWARE_REPORT=true
export FIRMWARE_URL="https://caprf.example.com/firmware"
export FIRMWARE_MIN_BIOS="U46"
```

BIOS data comes from `/sys/class/dmi/id/`. BOOTy reads the BMC vendor from DMI
board data, but no source reports a BMC firmware version, so
`FIRMWARE_MIN_BMC` always fails when it is set. The NIC firmware version comes
from `/sys/class/net/<iface>/device/firmware_version`, or from the ethtool
driver information when sysfs has none. Storage controller firmware comes from
`/sys/class/scsi_host/*/firmware_rev`. The minimum versions apply only with
`FIRMWARE_REPORT=true`. An unknown version or a version below the minimum
fails provisioning after BOOTy posts the report. A failed POST also fails
provisioning. With `CAPRF_LEGACY_COMPAT=true`, BOOTy logs the NIC firmware
versions also without `FIRMWARE_REPORT`.

### Image Verification

BOOTy supports checksum and GPG signature verification for streamed images.
OCI image sources must either be digest-pinned (`oci://repo/image@sha256:...`)
or provide `IMAGE_CHECKSUM`; mutable OCI tags without a checksum are rejected
before destructive storage steps.

```bash
# Hex digest of the decompressed image. A "sha256:" or "sha512:" prefix is
# accepted. Without IMAGE_CHECKSUM_TYPE, BOOTy infers the type from the length.
export IMAGE_CHECKSUM="<64 hex digits>"
export IMAGE_CHECKSUM_TYPE="sha256"

# GPG signature verification. Requires IMAGE_CHECKSUM because BOOTy verifies
# the signature before storage setup, then streams the image in a later fetch.
export IMAGE_SIGNATURE_URL="https://images.example.com/ubuntu.img.gz.sig"
export IMAGE_GPG_PUBKEY="/deploy/signing-key.gpg"
```

BOOTy hashes the decompressed bytes while it writes them and compares the
digest after streaming. GPG verification
downloads the detached signature and verifies it against the provided public key
before destructive storage setup starts. When `IMAGE_SIGNATURE_URL` is set,
`IMAGE_CHECKSUM` is required so the bytes written during the later streaming step
are bound to the verified image.

`IMAGE_SIGNATURE_URL` is only supported for non-`oci://` image sources today.
For OCI image sources, use digest-pinned references such as
`oci://registry.example/os-image@sha256:<digest>` and keep `IMAGE_CHECKSUM`
enabled when the source is not pinned by digest. Runtime Cosign or Notation
verification for provisioning `oci://` images is not implemented yet.

### OCI Image Pre-Pulls

BOOTy can cache workload/runtime OCI images into the provisioned target root
during provisioning. This is opt-in and separate from the provisioning
`IMAGE`, which still describes the OS image to write to disk.

```bash
export OCI_PREPULL_ENABLED=true
export OCI_PREPULL_IMAGES="oci://registry.example.com/platform/pause@sha256:<digest>,registry.example.com/platform/cni:v1.5.0"
export OCI_PREPULL_IMPORT_NAMESPACE="k8s.io"
```

During provisioning BOOTy pulls each configured image with the default Docker
keychain, writes a deterministic Docker-compatible archive below
`OCI_PREPULL_CACHE_DIR/archives`, and records `catalog.json` plus
`import-list.tsv` in the same cache directory. BOOTy does not import directly
into containerd from the initramfs because the target runtime daemon and target
root service state are not guaranteed to be available or safe to mutate there.

Instead, BOOTy installs and enables
`booty-oci-prepull-import.service` in the target root. On first boot the unit
runs before kubelet and attempts import with available runtime tools in this
order until one succeeds: `ctr`, `nerdctl`, `podman`, then `docker`. For containerd and
nerdctl it imports into `OCI_PREPULL_IMPORT_NAMESPACE` (`k8s.io` by default).
Successful imports are marked under `OCI_PREPULL_CACHE_DIR/imported`.

Migration notes:
- Replace ad-hoc `POST_PROVISION_CMDS` runtime pulls with `OCI_PREPULL_IMAGES`
  when the image can be fetched during BOOTy provisioning.
- Prefer digest-pinned references. Mutable tags are resolved during
  provisioning and the resolved digest is written to the catalog, but BOOTy does
  not perform Cosign or Notation verification for these cached runtime images.
- BOOTy uses the default Docker keychain of the ramdisk, for example
  `$DOCKER_CONFIG/config.json` or `$HOME/.docker/config.json`. Provisioner
  files reach the ramdisk only with `CAPRF_LEGACY_COMPAT=true`. Do not place
  registry passwords in image references or logs.
- `OCI_PREPULL_ALLOW_INSECURE=true` is for controlled lab registries only; use
  TLS registries in production.
- The first-boot import requires one supported runtime CLI in the target OS. If
  none is present, the systemd unit fails and can be retried after installing a
  supported runtime.

### Release Artifact Verification

Release builds sign checksum files, container images, and ORAS-published
artifacts with Sigstore keyless signing from the `release-v2.yml` workflow.
Verify release assets before use instead of trusting tags or downloaded files
alone:

```bash
VERSION=v1.2.3
VERSION_NO_V=${VERSION#v}
ISSUER="https://token.actions.githubusercontent.com"
IDENTITY="https://github.com/telekom/BOOTy/.github/workflows/release-v2.yml@refs/tags/${VERSION}"

# Verify a GitHub release checksum file and then verify the payload checksum.
ARTIFACT=default-amd64-initramfs.cpio.zst
cosign verify-blob \
  --bundle "${ARTIFACT}.sha256.bundle" \
  --certificate-identity "${IDENTITY}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "${ARTIFACT}.sha256"
sha256sum -c "${ARTIFACT}.sha256"

# Verify signed OCI release refs before pulling or mirroring them.
cosign verify \
  --certificate-identity "${IDENTITY}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "ghcr.io/telekom/booty:${VERSION_NO_V}"
cosign verify \
  --certificate-identity "${IDENTITY}" \
  --certificate-oidc-issuer "${ISSUER}" \
  "ghcr.io/telekom/booty:${VERSION_NO_V}-gobgp"
```

The release workflow also signs these OCI artifact refs:

| Artifact | Example ref |
|----------|-------------|
| Initramfs | `ghcr.io/telekom/booty/initramfs:${VERSION_NO_V}-default-amd64` |
| Binary | `ghcr.io/telekom/booty/binary:${VERSION_NO_V}-amd64` |
| ISO | `ghcr.io/telekom/booty-iso:${VERSION_NO_V}` |
| GoBGP ISO | `ghcr.io/telekom/booty-iso:${VERSION_NO_V}-gobgp` |
| SBOM | `ghcr.io/telekom/booty/sbom:${VERSION_NO_V}` |

### Telemetry and Metrics

`TELEMETRY_ENABLED=true` makes BOOTy post a metrics summary when provisioning
ends.

```bash
export TELEMETRY_ENABLED=true
export METRICS_URL="https://caprf.example.com/metrics"
```

| Endpoint | Payload |
|----------|---------|
| `METRICS_URL`, or `TELEMETRY_URL` when `METRICS_URL` is unset | Metrics summary (JSON) |
| `EVENT_URL` | The last provisioning event |

The provisioning steps do not feed the telemetry collector yet. The summary
has no step data, and BOOTy sends no events. Telemetry is best-effort:
failures do not block provisioning.

### Startup Crash Artifact Upload

BOOTy can inspect the existing OS on the target disk before provisioning or
deprovisioning performs destructive disk operations. When crash evidence is
found, BOOTy uploads a `tar.gz` archive containing allowlisted crash artifacts
plus `manifest.json` and `metadata.json` for fleet correlation.

```bash
export CRASH_ARTIFACTS_ENABLED=true
export CRASH_ARTIFACTS_PREPARE_URL="https://caprf.example.com/crash-artifacts/prepare"
export CRASH_ARTIFACTS_MAX_MB=256
```

The collector searches for kernel crash and dump evidence in paths such as
`/var/crash`, `/var/lib/systemd/coredump`, `/var/log/journal`, kernel logs, and
`/sys/fs/pstore`. The archive metadata reuses BOOTy's hardware inventory,
firmware report, debug dump, and build information. Upload failures are
best-effort and do not block provisioning.
Text logs and JSON metadata are redacted before upload. Raw `vmcore` and
systemd coredump files are skipped unless
`CRASH_ARTIFACTS_INCLUDE_MEMORY_DUMPS=true` is set.

CAPRF can either return presigned S3 upload instructions from
`CRASH_ARTIFACTS_PREPARE_URL` or accept direct proxy uploads at
`CRASH_ARTIFACTS_UPLOAD_URL`. BOOTy never sends its bearer token to presigned
S3 URLs and does not log presigned query strings.

### Secure Boot

`SECUREBOOT_REENABLE=true` asks the CAPRF controller to turn Secure Boot on
after provisioning. The OS image must contain signed boot loaders.

```bash
export SECUREBOOT_REENABLE=true
```

BOOTy appends `SECUREBOOT_REENABLE=true` to the success status message and
skips kexec, because the change needs a firmware boot. If the installed OS has
no signed boot loaders, the machine does not boot.

The `SECUREBOOT_*_SHA256` and `SECUREBOOT_PINNED_DIGESTS` keys pin the
digests of the installed shim, GRUB and kernel. The `verify-secureboot-chain`
step fails provisioning on a mismatch. MOK enrollment (`MOK_CERT_PATH`,
`MOK_PASSWORD`) is not implemented. See
[Secure Boot](docs/configuration.md#secure-boot).

### LUKS Encryption (Planned)

The `pkg/disk/luks` library can format, open and close LUKS2 volumes and write
`/etc/crypttab`. The provisioning flow does not use it yet. The vars parser
rejects every key that starts with `LUKS_`, so a vars file with `LUKS_ENABLED`
stops the run. See [docs/proposal-luks-encryption.md](docs/proposal-luks-encryption.md).

### JWT Authentication

Set `TOKEN_URL` to exchange the bootstrap token for a JWT and renew it in the
background:

```bash
export TOKEN="bootstrap-token"
export TOKEN_URL="https://caprf.example.com/auth/token"
export TOKEN_ALGORITHM="RS256"
```

**Lifecycle:**
1. After the network is up, BOOTy sends `TOKEN` and `HOSTNAME` to `TOKEN_URL`
   and uses the returned JWT for all later requests.
2. A background goroutine renews the JWT at 80% of its remaining lifetime.
3. A failed renewal is tried 5 times with exponential backoff.

When `TOKEN_URL` is not set, BOOTy uses the static `TOKEN` for all requests.
When it is set, a failed exchange or exhausted renewal reboots the host. With
`FAILURE_ACTION=wait`, BOOTy reports the error and keeps the ramdisk running
instead. `TOKEN_URL` must use HTTPS, except for `localhost`,
`127.0.0.1` and `::1`.

### VLAN Support

BOOTy supports 802.1Q VLAN tagging via netlink. Configure VLANs with the
`VLANS` variable as a comma-separated list of `ID:parent[:address[:gateway]]`
entries:

```bash
# IPv4 VLAN with an address, plus a second VLAN without an address.
export VLANS="200:eno1:10.200.0.42/24,300:eno2"
# Alternative: IPv6 address and gateway.
export VLANS="200:eno1:[2001:db8:200::42/64]:[2001:db8:200::1]"
```

Each VLAN creates a tagged sub-interface (`eno1.200`), assigns the IP address
(if provided), and brings the link up. BOOTy creates the VLANs before it
starts the network mode. Without `STATIC_IFACE`, the first VLAN interface
carries `STATIC_IP`. IPv6 address or gateway fields in `VLANS` must
be wrapped in brackets because `:` separates fields in the compact format.

### Kexec Boot

BOOTy uses kexec for fast reboots into the provisioned OS without full firmware
re-initialization. After provisioning, BOOTy parses the installed GRUB config,
loads the kernel and initramfs, then executes the kexec syscall.

```bash
# Disable kexec (force hard reboot via firmware)
export DISABLE_KEXEC=true
```

BOOTy skips kexec when firmware values changed during provisioning (for
example Mellanox SR-IOV), with `SECUREBOOT_REENABLE=true`, and with
`SUCCESS_ACTION=poweroff`, which powers off instead. If GRUB parsing fails or
the kernel or initramfs is not found, BOOTy falls back to a `reboot(2)`
syscall.

### SR-IOV Configuration

BOOTy automatically detects Mellanox ConnectX NICs via PCI vendor ID and
configures SR-IOV virtual functions using `mstconfig`:

```bash
# Set a positive number of virtual functions. NUM_VFS=0 skips this step.
export NUM_VFS=16
```

For each detected Mellanox device, BOOTy runs
`mstconfig -d <device> -y set SRIOV_EN=True NUM_OF_VFS=<n>`. This modifies NIC
firmware and needs a hard reboot, so BOOTy skips kexec when SR-IOV changed.
When `NUM_VFS` is positive, missing Mellanox NICs, missing `/dev/mst` pciconf
devices, or `mstconfig` failures abort provisioning instead of silently
continuing without the requested firmware change. In CAPRF legacy compat the
rules differ: see
[CAPRF legacy migration](docs/caprf-legacy-migration.md#installed-os-configuration).

### Secure Erase

`SECURE_ERASE=true` makes provisioning erase the target disk, and the RAID
member disks, with a hardware erase instead of a signature wipe:

```bash
export SECURE_ERASE=true
```

| Drive Type | Erase Method |
|-----------|--------------|
| NVMe | `nvme format --ses=1` (User Data Erase). When the command fails, BOOTy falls back to `wipefs -af`. |
| SATA/SAS | ATA Security Erase via `hdparm`. When the drive has no ATA security or is frozen, BOOTy falls back to `wipefs -af`. When the erase command fails, provisioning fails. |

A missing `nvme` or `hdparm` tool fails provisioning, so BOOTy does not
silently downgrade an explicit secure erase request. Deprovisioning,
decommission and the wipe of all disks (`WIPE_ALL_DISKS=true`) do not read
`SECURE_ERASE`.

### Post-Provision Commands

Run commands in the provisioned root chroot near the end of provisioning,
after cloud-init injection and before the EFI boot entry:

```bash
# Commands separated by semicolons.
export POST_PROVISION_CMDS="apt-get update; apt-get install -y custom-pkg; systemctl enable my-service"
```

Commands run as root inside the chroot (`/newroot`). A command can contain
only letters, digits, white space and `_ . / : = , @ % + -`. BOOTy rejects
pipes, redirects, quotes and command substitution. A failed command fails the
provisioning. Use this for OS changes that do not need a new image.

### Standby Mode

Persistent agent mode that polls for provisioning commands from CAPRF:

```bash
export MODE=standby
export HEARTBEAT_URL="https://caprf.example.com/status/heartbeat"
export COMMANDS_URL="https://caprf.example.com/commands"
```

In standby mode, both URLs are required. BOOTy reports standby status, sends
an initial heartbeat, and polls commands once before entering the periodic loop
(heartbeats every 30s, command polls every 10s). Startup readiness failures
return an error immediately. After readiness succeeds, heartbeat and command-poll
failures are logged and retried on the next tick. `RESCUE_MODE` applies to hot
provision command failures; deprovision command failures are ACKed as failed
before BOOTy reboots. With `FAILURE_ACTION=wait`, a failed hot provision
defaults to `RESCUE_MODE=wait` (ACKed first), a failed deprovision command is
ACKed and BOOTy waits instead of rebooting. With `SUCCESS_ACTION=poweroff`,
successful provision and deprovision commands power the host off. Both are
preset by `CAPRF_LEGACY_COMPAT=true`.
Only `204 No Content` denotes an empty command queue; a `404 Not Found` or any
other unexpected response is treated as a polling failure rather than an empty
queue.

| Command | Behavior |
|---------|----------|
| `provision` | Run the full provisioning pipeline, then kexec or reboot |
| `deprovision` | Run hard deprovisioning, then reboot |
| `reboot` | Reboot at once |
| `health-check` | Liveness probe: ACK with `"healthy"` |

BOOTy acknowledges each command to the controller with `completed`, or with
`failed` and the error message. It acknowledges an unknown command type as
`failed`.

### BGP Policy

The GoBGP package has policy types for community tagging, local-pref, MED, and
graceful restart, and validates them. No vars key sets them, and the stack
does not apply import or export policy to its sessions. When a policy is
present, setup logs a warning and continues without it.

**Community formats:**
- **Standard communities**: `ASN:value` (16-bit each, e.g. `65000:100`)
- **Extended communities**: `TYPE:ASN:value` fields are parsed as 32-bit values (e.g. `RT:4200000001:100`); EVPN route-target encoding with a 4-octet ASN can only carry values up to `65535`
- **Large communities**: `GA:LD1:LD2` (32-bit each, e.g. `65000:1:100`)

The helpers in `pkg/network/gobgp/policy.go` parse and validate policy data.

`VRF_NAME` puts the underlay in a VRF. An empty value means no VRF; BOOTy does
not generate a name. `VRF_TABLE_ID` sets its routing table (default `1000`).
See [GoBGP and EVPN](docs/configuration.md#gobgp-and-evpn).

### Cloud-Init

BOOTy generates and injects cloud-init configuration into the provisioned
OS. Supported datasource types are `nocloud` and `configdrive`.

```bash
export CLOUDINIT_ENABLED=true
# nocloud (default) or configdrive
export CLOUDINIT_DATASOURCE=nocloud
```

When enabled, BOOTy writes cloud-init seed data to the appropriate path
on the provisioned root filesystem. `nocloud` writes
`/var/lib/cloud/seed/nocloud/`, and `configdrive` writes OpenStack ConfigDrive
v2 seed files under `/var/lib/cloud/seed/config_drive/openstack/latest/`.
For non-bond cloud-init network config, `STATIC_IFACE` is required so the
generated v2 network-config names the target OS interface instead of an
initramfs-only fallback.
The active provisioning integration currently generates:

- **Instance metadata**: instance-id, local-hostname, and platform (`booty`).
  When a provider-id is configured, it is used as the source for instance-id
  and is not written as a separate metadata field.
- **User data**: hostname and `manage_etc_hosts`
- **Network config v2**: IPv4/IPv6 addresses and gateways, bonds, VLANs,
  and nameservers
  (generated from the active provisioning network config)

The cloud-init generator package has fields for richer user-data such as users,
packages, NTP, file writes, and `runcmd`, but the provisioning configuration
surface does not currently populate those fields from CAPRF or environment
configuration.

### Ubuntu OverlayFS

BOOTy can prepare immutable Ubuntu target images by writing Ubuntu overlayroot
configuration into the installed OS while the target root is mounted. This is a
target-root configuration step only: BOOTy does not remount or mutate the
initramfs root it is currently running from.

```bash
export PROVISION_TARGET_OS=linux
export OS_FAMILY=ubuntu
export OVERLAYFS_ENABLED=true
export OVERLAYFS_MODE=tmpfs
```

For persistent writes, provide a separate backing filesystem that the installed
Ubuntu initramfs can find on first boot:

```bash
export OVERLAYFS_ENABLED=true
export OVERLAYFS_MODE=device
export OVERLAYFS_DEVICE=LABEL=BOOTY-OVERLAY
export OVERLAYFS_DIRECTORY=booty
export OVERLAYFS_TIMEOUT_SEC=30
```

When enabled, BOOTy writes `/etc/overlayroot.local.conf` in the provisioned
root. The generated file disables overlayroot config-disk discovery and renders
an explicit `overlayroot="..."` line. `OVERLAYFS_RECURSE` defaults to `false`,
so separate mounted data partitions stay writable unless the operator opts into
recursive overlays.

Optional `OVERLAYFS_UPPER_DIR` and `OVERLAYFS_WORK_DIR` create target-root
directories for custom overlayFS initramfs hooks. They must be set together and
must live on storage that will be writable before the custom overlay is mounted;
directories left on the root filesystem itself are not a persistent upper layer.

Prerequisites:

- The target image must be Ubuntu-like and include the `overlayroot` package and
  an initramfs rebuilt with that package present.
- Set `PROVISION_TARGET_OS=linux` and `OS_FAMILY=ubuntu`; non-Ubuntu families are
  rejected for this feature.
- `device` mode requires an operator-created writable backing filesystem that
  survives provisioning and is discoverable at early boot, for example by label.
- BOOTy does not install packages, rebuild the target initramfs, or prove first
  boot of real Ubuntu cloud images in CI.

Migration:

1. Rebuild the Ubuntu base image with `overlayroot` installed and the initramfs
   updated.
2. For persistent mode, add a small data partition or other backing filesystem
   with a stable label or `/dev/disk/by-*` path.
3. Enable `OVERLAYFS_ENABLED=true` in a non-production host first and confirm
   that first boot shows the expected overlayroot state.
4. Roll the config across the fleet only after rollback has been tested on the
   same image version.

Rollback:

- Boot once with the kernel command line `overlayroot=disabled` to bypass the
  generated overlayroot file.
- Remove or replace `/etc/overlayroot.local.conf` in the target image or through
  the persistent upper layer, then reboot.
- For persistent `device` mode, keep or archive the backing filesystem before
  wiping it; it contains writes made while the base root was immutable.

### Netplan Overlay

BOOTy supports provisioner-supplied netplan configuration as a drop-in
alternative to `/deploy/vars` network settings. When netplan YAML files
are present in `/deploy/file-system/etc/netplan/`, BOOTy parses them
and uses the derived network config, with netplan values overriding
`/deploy/vars` settings.

```
/deploy/file-system/
├── etc/
│   ├── netplan/
│   │   └── 01-network.yaml    # Netplan config (bonds, bridges, VLANs, tunnels, static routes)
│   └── frr/
│       └── frr.conf           # Optional FRR config (ASN, peers, EVPN settings)
```

The netplan parser supports ethernets, bonds, bridges, VLANs, tunnels
(VXLAN/GRE), dummy devices, VRFs, and static routes. When an FRR config
file is also present, BOOTy extracts BGP parameters (ASN, router-ID,
peers, EVPN settings) from it, including the GoBGP EVPN route mode. A netplan config with an ASN and
a VNI selects the GoBGP stack. [docs/networking.md](docs/networking.md)
describes the network setup in detail.

| Input | EVPN routes BOOTy originates |
|---|---|
| `EVPN_L2_ENABLED=true` | Type-2 + Type-3 + Type-5 |
| FRR `advertise-all-vni`, provisioning VNI is an L2VNI | Type-2 + Type-3 + Type-5 |
| FRR `advertise-all-vni`, provisioning VNI is an L3VNI (`vni N` at the top level or in a `vrf` block, as in the CAPRF frr.conf) | Type-5 only |
| FRR config without `advertise-all-vni` (for example only `advertise ipv4 unicast`), or no FRR config | Type-5 only |
| `EVPN_TYPE5_ONLY=true` | Type-5 only, without an L2 gateway; overrides L2 handling inferred from the FRR config |

The L3VNI rule follows FRR: `advertise-all-vni` creates Type-2/Type-3
routes for L2VNIs only, so the CAPRF L3VNI setup stays Type-5 only, like
the FRR-based legacy deployer. `EVPN_L2_ENABLED` and `EVPN_TYPE5_ONLY`
cannot both be set.

### Network Persistence

BOOTy includes renderer helpers for persistent network configuration on the
target OS filesystem. Set `PERSIST_NETWORK=true` and an explicit `OS_FAMILY`
to write those files during provisioning. BOOTy does not auto-detect the
target OS family, and static address persistence without a bond requires
`STATIC_IFACE` because the initramfs auto-detected interface name may not be
stable in the provisioned OS. During provisioning, BOOTy also writes DNS
configuration and can generate cloud-init network config when cloud-init
injection is enabled. Flatcar is the exception: `OS_FAMILY=flatcar` is limited
to the systemd-networkd writer, and cloud-init injection fails validation
because BOOTy does not implement Ignition.

Implemented renderer formats:

| OS Family | Format | Config Path | Scope |
|-----------|--------|-------------|-------|
| Ubuntu | Netplan YAML | `/etc/netplan/` | Unit-tested renderer and explicit provisioning opt-in |
| RHEL | NetworkManager keyfiles | `/etc/NetworkManager/system-connections/` | Renderer unit-tested only; active provisioning fails closed until native bootloader, BLS, and SELinux first-boot support exist |
| Flatcar | systemd-networkd units | `/etc/systemd/network/` | Unit-tested renderer and explicit provisioning opt-in, including static/DHCP interfaces, bonds, and VLANs |

The renderer package can write interface configs, bond settings, addresses,
gateways, DNS, and VLAN assignments where the selected writer supports them.

### IPMI Operations

BOOTy includes an IPMI manager (`pkg/ipmi/`) for local BMC operations via
`ipmitool`. This package is a library; it is not currently wired into the
automatic provisioning, debug, or inventory flows.

| Operation | Description |
|-----------|-------------|
| `GetBMCNetwork` | Read BMC IP, netmask, gateway, MAC, DHCP, and VLAN settings |
| `SetNextBoot` | Set next boot device: `pxe`, `disk`, `cdrom`, `bios` |
| `ChassisControl` | Power on/off/cycle/reset/soft |

No vars key enables automatic IPMI use today. Sensor reading,
chassis status reporting, and BMC auto-detection are tracked in the IPMI
proposal docs but are not implemented in the runtime package.

### TPM Support

The `pkg/tpm` library detects TPM 2.0 hardware via sysfs and reads PCR values.
The provisioning flow, the inventory and the debug dump do not use it today.

| Capability | Description |
|-----------|-------------|
| Detection | Reads `/sys/class/tpm/tpm0` for presence, version, manufacturer, firmware |
| PCR reading | Reads SHA256 PCR bank values from sysfs |
| Attestation | Quote-based TPM attestation (Phase 2) |
| Measurement | PCR extend operations (Phase 2) |
| LUKS enrollment | `systemd-cryptenroll` integration for TPM2-bound LUKS2 unlock (Phase 2) |

### Boot Loader

The provisioning pipeline is GRUB-oriented. It writes a GRUB drop-in
configuration, runs `update-grub` in the target chroot, and creates EFI boot
entries for the GRUB paths. The repository has no systemd-boot support. See
[docs/proposal-bootloaders.md](docs/proposal-bootloaders.md).

## Extending Bundled Binaries

The initramfs is built in `initrd.Dockerfile` using a multi-stage Docker build. To add
a new binary:

1. **Install the package** in the `tools` build stage:
```dockerfile
# In the 'tools' stage (FROM debian:bookworm-slim AS tools)
RUN apt-get update && apt-get install -y --no-install-recommends \
    your-package \
    && rm -rf /var/lib/apt/lists/*
```

2. **Copy the binary** into the final initramfs:
```dockerfile
# In the builder stage
COPY --from=tools /usr/sbin/your-binary sbin/your-binary
```

3. **Verify all shared library dependencies** are satisfied:
```bash
# Inside the container
ldd /usr/sbin/your-binary
```

The default flavor bundles, among others: `mdadm`, `wipefs`, `sfdisk`,
`sgdisk`, `growpart`, `e2fsck`, `resize2fs`, `xfs_growfs`, `xfs_repair`,
`btrfs`, `parted`, `kpartx`, `lvm`, `lsblk`, `blkid`, `hdparm`, `nvme`,
`qemu-img`, `gpgv`, `mstconfig`, `mstflint`, `ethtool`, `lldpcli`, `lldpd`,
`dropbear`, `efibootmgr`. See `initrd.Dockerfile` for the full list per
flavor.

> **Prefer Go libraries**: Where possible, use Go syscalls or libraries instead of
> shelling out to external binaries. Examples: `unix.FinitModule()` instead of `insmod`,
> `syscall.SysProcAttr{Chroot}` instead of `chroot`, sysfs reads instead of `lspci`.

## Development

```bash
make build       # Build the binary
make test        # Unit tests with the race detector and the coverage gate
make lint        # golangci-lint for the Linux build
make fmt-check   # Check the formatting. "make fmt" rewrites the files.

# E2E tests without KVM (Linux). Some tests build the initramfs with Docker.
make test-e2e

# ContainerLab E2E (Linux, root): start a topology, then run its tests.
make clab-test-image
make clab-up && make test-e2e-integration
make clab-type5-up && make test-e2e-type5

# Disk, mount and loop device E2E (Linux, root)
sudo -E env "PATH=$PATH:/usr/sbin:/sbin" "$(which go)" test -tags linux_e2e -v -count=1 -timeout 5m ./test/e2e/linux/...

# KVM E2E (Linux, root, QEMU). BOOTY_KERNEL and BOOTY_INITRAMFS select the
# kernel and the initramfs. .github/workflows/kvm-matrix.yml shows the CI setup.
make test-kvm
```

[CONTRIBUTING.md](CONTRIBUTING.md#testing) lists all ContainerLab and
vrnetlab topologies and the test levels.
[docs/e2e-test-matrix.md](docs/e2e-test-matrix.md) describes the E2E suites.

## Project Structure

```
├── main.go                     # Entry point: CAPRF-compatible runtime flow, kernel module loading
├── cmd/booty.go                # Cobra CLI entry point
├── initrd.Dockerfile           # Multi-stage initramfs build (default, iso, slim, micro)
├── pkg/
│   ├── auth/                   # JWT token manager (acquisition, renewal, backoff)
│   ├── firmware/nic/           # Vendor-specific NIC firmware managers
│   ├── bootloader/             # Bootloader detection (GRUB, systemd-boot)
│   ├── buildinfo/              # Binary build information (version, commit, date)
│   ├── caprf/                  # CAPRF client (status, log, debug, vars parsing)
│   ├── cloudinit/              # Cloud-init NoCloud and ConfigDrive generation
│   ├── config/                 # MachineConfig, Provider interface, Status types
│   ├── crash/                  # Startup crash artifact collection, metadata, upload contracts
│   ├── debug/                  # Structured debug dump collection
│   ├── disk/                   # Disk detection, partitioning, RAID, LVM, mount, NVMe namespaces
│   ├── drivers/                # Architecture-aware kernel module loading
│   ├── efi/                    # EFI variable and boot entry operations
│   ├── event/                  # Provisioning lifecycle event types + dispatcher
│   ├── executil/               # Shared command execution helpers
│   ├── firmware/               # Firmware inventory from sysfs and NIC tooling
│   ├── health/                 # Pre-provisioning hardware health checks
│   ├── image/                  # Image streaming (HTTP, OCI, gzip/lz4/xz/zstd auto-detect)
│   ├── inventory/              # Hardware inventory from sysfs/procfs
│   ├── ipmi/                   # IPMI operations and helpers
│   ├── kexec/                  # GRUB parsing, kexec load/execute
│   ├── logging/                # Structured log handlers and sinks
│   ├── network/                # Network mode abstraction (FRR, GoBGP, DHCP, Static, Bond)
│   │   ├── frr/               # FRR/EVPN: config rendering, address derivation
│   │   ├── gobgp/             # Pure-Go BGP stack (3-tier: Underlay, Overlay, IPMI)
│   │   ├── lldp/              # LLDP frame listener (raw AF_PACKET sockets)
│   │   ├── netplan/           # Netplan YAML + FRR config parser (provisioner overlay)
│   │   ├── persist/           # Persist network config into target OS (netplan, NM, systemd-networkd)
│   │   ├── vrf/               # VRF configuration and validation
│   │   └── vlan/              # VLAN 802.1Q tagging via netlink
│   ├── provision/              # Orchestrator (46-step provision, deprovision)
│   │   └── configurator.go    # OS config: hostname, kubelet, GRUB, DNS, EFI, Mellanox SR-IOV
│   ├── realm/                  # Device, mount, network, shell operations
│   ├── rescue/                 # Rescue mode behavior and retry policy
│   ├── secureboot/             # Secure Boot setup and validation helpers
│   ├── serialconsole/          # Serial console resolution from SPCR/device-tree/sysfs evidence
│   ├── system/                 # Host-level system operations
│   ├── telemetry/              # Telemetry models and collectors
│   ├── tpm/                    # TPM/TPM2 detection, PCR reading, attestation
│   │   └── cryptenroll/       # systemd-cryptenroll integration for TPM2 LUKS unlock
│   ├── utils/                  # Cmdline parsing, helpers
│   └── ux/                     # ASCII art & system info display
├── test/e2e/                   # E2E tests (ContainerLab + vrnetlab + KVM)
│   ├── clab/                   # ContainerLab topologies and FRR configs
│   │   └── vrnetlab/          # QEMU VM image builder for vrnetlab testing
│   ├── kvm/                    # KVM/QEMU boot tests (provisioning, network, hardware)
│   └── integration/           # Integration test suites
├── docs/                       # Design documents and proposals
├── .github/workflows/          # CI (lint, test, build, E2E clab, E2E vrnetlab, KVM boot)
└── .golangci.yml               # Linter configuration
```

## Documentation

| Resource | Description |
|----------|-------------|
| [docs/configuration.md](docs/configuration.md) | Reference of every `/deploy/vars` key and environment variable |
| [docs/caprf-integration.md](docs/caprf-integration.md) | CAPRF lifecycle, status callbacks, and authentication |
| [docs/caprf-legacy-migration.md](docs/caprf-legacy-migration.md) | `CAPRF_LEGACY_COMPAT` behaviour and migration from the legacy shell provisioner |
| [docs/networking.md](docs/networking.md) | Ramdisk network stacks: GoBGP, FRR, static, DHCP, bond, VLAN, netplan |
| [docs/disk.md](docs/disk.md) | Disk selection, wipe, partition layout, and image streaming |
| [docs/ab-partitioning.md](docs/ab-partitioning.md) | Optional A/B partitioning and inactive-slot provisioning |
| [docs/sysext-provisioning.md](docs/sysext-provisioning.md) | Optional systemd-sysext loading while provisioning |
| [docs/gobgp-evpn-guide.md](docs/gobgp-evpn-guide.md) | GoBGP EVPN guide |
| [docs/serial-console-resolution.md](docs/serial-console-resolution.md) | Serial console resolution for the installed OS |
| [docs/e2e-test-matrix.md](docs/e2e-test-matrix.md) | End-to-end test matrix |
| [docs/size-optimization.md](docs/size-optimization.md) | Initramfs size optimization |
| [docs/library-adoption.md](docs/library-adoption.md) | Library adoption policy |
| [docs/roadmap.md](docs/roadmap.md) | Feature roadmap (P0 to P4 priorities) and design proposals (`docs/proposal-*.md`) |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Development setup, coding standards, PR process |
| [.github/AGENTS.md](.github/AGENTS.md) | Copilot agents, review personas, prompts |
| [.github/copilot-instructions.md](.github/copilot-instructions.md) | Project guidelines for Copilot |

## Contributing

Before writing helpers, follow the [upstream-first guidance](AGENTS.md#reuse-upstream-libraries-before-writing-helpers).
BOOTy uses the stdlib-only shared `pkg/redact` for URL diagnostics and Go's
`slog.MultiHandler` for fan-out; see [library adoption](docs/library-adoption.md)
for retained policies and linked dependency/size measurements.

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, coding standards, and the PR process.

## License

This project is licensed under the Apache License 2.0 — see [LICENSE](LICENSE) for details.
