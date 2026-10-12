# Configuration Reference

This page lists every `/deploy/vars` key and every process environment
variable that BOOTy reads. It is the canonical reference. Topic pages explain
the behaviour and link here:

- [CAPRF integration](caprf-integration.md)
- [CAPRF legacy migration](caprf-legacy-migration.md)
- [Networking](networking.md)
- [Disk](disk.md)
- [A/B partitioning](ab-partitioning.md)
- [Sysext provisioning](sysext-provisioning.md)

The parser is `ParseVars` in `pkg/caprf/client.go`. The test
`TestConfigurationDocListsEveryVarsKey` in `docs_consistency_test.go` fails
when the parser accepts a key that this page does not list.

## Configuration sources

BOOTy runs as PID 1 and reads its configuration from two sources:

1. The vars file `/deploy/vars`. CAPRF puts it on the ISO. All keys in this
   page come from this file, except the keys in
   [Process environment](#process-environment).
2. The process environment of PID 1. The kernel passes unknown `name=value`
   kernel command line parameters to init as environment variables. With
   `CAPRF_LEGACY_COMPAT=true`, BOOTy also exports every vars file entry to the
   environment. See [CAPRF legacy compatibility](#caprf-legacy-compatibility).

The runtime binary does not read YAML or JSON configuration files. The
`pkg/config` loader for `.yaml` and `.json` files is a library. Only the
`cmd/` Cobra package uses it, and the BOOTy binary does not build that package.

Files that CAPRF puts below `/deploy/file-system`, `/deploy/machine-files` and
`/deploy/machine-commands` are not configuration keys. The topic pages
describe them.

## Vars file format

```bash
# A comment line.
export MODE="provision"
HOSTNAME=worker-01
export IMAGE='https://images.example.com/ubuntu-24.04.img.zst'
```

Rules, from `parseVarLine` and `unquoteVarValue`:

- BOOTy reads one entry per line. It ignores empty lines and lines that start
  with `#`. A `#` after a value is part of the value, so do not put comments
  at the end of a line.
- The `export ` prefix is optional.
- The key is the text before the first `=`. Do not put spaces around `=`.
  Keys are case-sensitive.
- A value in double quotes loses the quotes. Inside double quotes, `\$`,
  `` \` ``, `\"` and `\\` become `$`, `` ` ``, `"` and `\`. Other backslashes
  stay. BOOTy does not expand `$VAR` references.
- A value in single quotes loses the quotes and stays literal.
- A value without quotes stays as it is, including spaces.
- A value can not span more lines. A line longer than 64 KiB stops the parse.
- When a key occurs more than once, the last entry wins.
- BOOTy ignores unknown keys, except keys that start with `LUKS_`. A `LUKS_`
  key stops the parse with "is not supported yet".
- An integer key with a value that is not an integer stops the parse.
- After the parse, `Config.Validate` checks enum values and cross-field rules.
  A validation error stops the run.

When the vars file does not parse, BOOTy reports nothing and reboots. With
`CAPRF_LEGACY_COMPAT=true` it keeps the ramdisk running instead.

Boolean keys are true for `true`, `1` or `yes`, in any letter case. All other
values are false.

## Mode and lifecycle

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `MODE` | `provision` | `provision`, `deprovision`, `hard`, `soft-deprovision`, `soft`, `decommission`, `standby`, `dry-run`, `check` | Operating mode. `hard` is an alias of `deprovision`, `soft` is an alias of `soft-deprovision`. `deprovision` wipes the disks and removes the EFI boot entries. `soft-deprovision` renames `grub.cfg`. `decommission` wipes every disk and removes every EFI boot entry, and logs failures without stopping. `check` runs the health checks only. |
| `DRY_RUN` | `false` | boolean | Sets `MODE=dry-run`. Dry-run skips the network setup and the token flow, runs read-only checks and reports the result. |
| `HOSTNAME` | (unset) | host name | Machine name. BOOTy writes it into the target OS, sends it to `TOKEN_URL`, and in legacy compat sets it as the ramdisk host name for provisioning. |
| `PROVISION_TARGET_OS` | (unset) | `linux` | Target OS class. Provisioning preflight fails when it is empty or not `linux`. Alias: `TARGET_OS`. |
| `TARGET_OS` | (unset) | `linux` | Alias of `PROVISION_TARGET_OS`. |
| `OS_FAMILY` | (unset) | `ubuntu`, `rhel`, `flatcar` | Target OS family. Required by `PERSIST_NETWORK=true`. `OVERLAYFS_ENABLED=true` requires `ubuntu`. `flatcar` rejects `CLOUDINIT_ENABLED=true`. `rhel` rejects `PERSIST_NETWORK=true`. |
| `DISABLE_KEXEC` | `false` | boolean | Skip kexec after provisioning and reboot instead. Can not be combined with `AB_PRESERVE_EXISTING=true`. |
| `CAPRF_LEGACY_COMPAT` | `false` | boolean | Behave like the CAPRF legacy shell provisioner. See [CAPRF legacy compatibility](#caprf-legacy-compatibility). |
| `MACHINE_EXTRA_KERNEL_PARAMS` | (unset) | kernel parameters | Extra kernel parameters for the installed OS GRUB configuration. A `console=` token is folded into the serial console resolution. |
| `EXTRA_KERNEL_PARAMS` | (unset) | kernel parameters | Extra kernel parameters that the legacy provisioner reads. BOOTy appends them after `MACHINE_EXTRA_KERNEL_PARAMS`, whatever the line order. |
| `MACHINE_SERIAL_CONSOLE` | (unset) | for example `ttyS1,115200n8`, `ttyS0`, `none` | Serial console of the installed OS. It overrides the firmware evidence (ACPI SPCR, device tree, sysfs). `none` configures no serial console. Legacy compat ignores it. Alias: `SERIAL_CONSOLE`. |
| `SERIAL_CONSOLE` | (unset) | see above | Alias of `MACHINE_SERIAL_CONSOLE`. |
| `FAILURE_DOMAIN` | (unset) | Kubernetes label value | Kubelet node label `topology.kubernetes.io/zone`. |
| `REGION` | (unset) | Kubernetes label value | Kubelet node label `topology.kubernetes.io/region`. |
| `PROVIDER_ID` | (unset) | string without white space | Kubelet `--provider-id`. Cloud-init also uses it as the instance ID. |
| `POST_PROVISION_CMDS` | (unset) | commands separated by `;` | Commands that run in the target root chroot near the end of provisioning. Each command can contain only letters, digits, white space and `_ . / : = , @ % + -`. A failed command fails the provisioning. |
| `NUM_VFS` | `0` | integer | Number of SR-IOV VFs to set with `mstconfig` on each Mellanox NIC. `0` skips the step. A positive value fails the run when no Mellanox NIC or `mstconfig` device is found. In legacy compat, `0` means 32 and missing NICs or `mstconfig` are skipped. See [CAPRF legacy compatibility](#caprf-legacy-compatibility). |

## CAPRF endpoints, authentication and transport

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `INIT_URL` | (unset) | URL | Receives the `init` status. BOOTy also uses it as the network connectivity target. |
| `SUCCESS_URL` | (unset) | URL | Receives the success status. Connectivity target when `INIT_URL` is unset. |
| `ERROR_URL` | (unset) | URL | Receives the error status. |
| `LOG_URL` | (unset) | URL | BOOTy ships each log line of level info and higher to this URL. |
| `HEARTBEAT_URL` | (unset) | URL | Standby mode: heartbeat endpoint, every 30 s. Required for `MODE=standby`. |
| `COMMANDS_URL` | (unset) | URL | Standby mode: command poll endpoint, every 10 s. Required for `MODE=standby`. |
| `TOKEN` | (unset) | string | Bearer token. With `TOKEN_URL` it is the bootstrap token for the JWT exchange. |
| `TOKEN_URL` | (unset) | HTTPS URL | JWT endpoint. BOOTy exchanges `TOKEN` for a JWT after the network is up and renews it in the background. Plain HTTP is accepted only for `localhost`, `127.0.0.1` and `::1`. Requires `TOKEN` and `HOSTNAME`. A failed exchange or an exhausted renewal ends the run. |
| `TOKEN_ALGORITHM` | (unset) | `RS256`, `ES256` | JWT signing algorithm that BOOTy requests from `TOKEN_URL`. Empty leaves the choice to the server. |
| `INSECURE_TRANSPORT` | `false` | boolean | Send the bearer token over plain HTTP. Without it, BOOTy refuses to send the token to an `http://` URL that is not a loopback address. Legacy compat sets it. |

The CAPRF callback, connectivity check and image HTTP clients use the
standard proxy variables (`https_proxy`, `http_proxy`, `no_proxy`) from the
process environment. The `TOKEN_URL` client uses no proxy and always verifies
TLS, also in legacy compat.

## CAPRF legacy compatibility

`CAPRF_LEGACY_COMPAT=true` changes these settings:

- `INSECURE_TRANSPORT=true`, `IMAGE_ALLOW_INSECURE_HTTP=true`, and TLS
  certificate checks off for the CAPRF callbacks, the connectivity check and
  the image downloads.
- BOOTy exports every vars file entry whose key is a valid shell identifier to
  the process environment, like `source vars`. Proxy variables and the
  [process environment](#process-environment) variables then apply.
- No kexec. `DISABLE_KEXEC` has no further effect.
- The default `RESCUE_MODE` for a failed provisioning is `wait`.
- `MACHINE_SERIAL_CONSOLE` is ignored.
- `setup-mellanox` raises `NUM_OF_VFS` to 32 (or `NUM_VFS`) on every
  ConnectX Ethernet function. It skips quietly without `mstconfig` or such a
  NIC.
- For provisioning, the ramdisk takes the `HOSTNAME`.
- `AB_PRESERVE_EXISTING=true` is rejected.

[CAPRF legacy migration](caprf-legacy-migration.md) describes all
behaviour changes of this mode.

## Disk and image

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `IMAGE` | (unset) | URLs separated by commas or spaces | OS image sources: `https://`, `http://` or `oci://`. With more than one URL, BOOTy probes each URL with a ranged GET and uses the fastest. Required for provisioning. |
| `IMAGE_ALLOW_INSECURE_HTTP` | `false` | boolean | Accept `http://` image URLs. Legacy compat sets it. |
| `IMAGE_CHECKSUM` | (unset) | hex digest, optional `sha256:` or `sha512:` prefix | Expected digest of the decompressed image. Required for `oci://` sources that are not pinned by digest, and with `IMAGE_SIGNATURE_URL`. |
| `IMAGE_CHECKSUM_TYPE` | from digest length | `sha256`, `sha512` | Hash algorithm of `IMAGE_CHECKSUM`. |
| `IMAGE_SIGNATURE_URL` | (unset) | URL | Detached GPG signature of the image. Not supported for `oci://` sources. |
| `IMAGE_GPG_PUBKEY` | (unset) | file path in the ramdisk | Public key for `IMAGE_SIGNATURE_URL`. |
| `IMAGE_MODE` | `whole-disk` | `whole-disk`, `partition`, `ab` | How BOOTy writes the image. See [A/B partitioning](#ab-partitioning) for `ab`. |
| `IMAGE_SOURCE_ROOT_LABEL` | (unset) | GPT partition label | Source image root partition to stream into a `PARTITION_LAYOUT` root. Excludes `IMAGE_SOURCE_ROOT_PARTITION`. |
| `IMAGE_SOURCE_ROOT_PARTITION` | `0` | 1-based partition number | Same as `IMAGE_SOURCE_ROOT_LABEL`, by number. |
| `DISK_DEVICE` | auto-detect | block device path | Target disk. Hard deprovisioning then wipes only this disk instead of all disks. |
| `DISK_SERIAL_NUMBER` | (unset) | serial number | Select the target disk by its exact serial number when `DISK_DEVICE` is unset. Alias: `DISK_SERIAL`; the last line wins. |
| `DISK_SERIAL` | (unset) | serial number | Alias of `DISK_SERIAL_NUMBER`. |
| `MIN_DISK_SIZE_GB` | `0` | integer, GiB | Smallest disk that auto-detection accepts. `0` means no limit. |
| `SECURE_ERASE` | `false` | boolean | Provisioning only: erase the target disk with `nvme format --ses=1` or ATA Security Erase instead of a signature wipe. Deprovisioning, decommission and the legacy compat wipe of all disks ignore it. A missing `nvme` or `hdparm` tool fails the run. |
| `ROOT_PARTITION_LABEL` | (unset) | GPT partition name | Root partition of a whole-disk image. Excludes `ROOT_PARTITION_NUMBER`. |
| `ROOT_PARTITION_NUMBER` | `0` | 1-based partition number | Root partition of a whole-disk image when it has no stable label. |
| `PARTITION_LAYOUT` | (unset) | JSON object | Declarative GPT layout with optional LVM. An invalid layout stops the parse. |
| `NVME_NAMESPACES` | (unset) | JSON array | NVMe namespaces to create before disk detection, for example `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`. |

[Disk](disk.md) describes disk selection, wipe rules and the layout format.
The config fields `provision.disk.raid` (software RAID), `deprovision.device` and `deprovision.secureErase` have no vars key, so the runtime can not set them. Planned (#581): `DISK_MIRRORING_MODE` for a
mirrored root disk.

## A/B partitioning

These keys apply to `IMAGE_MODE=ab`. See [A/B partitioning](ab-partitioning.md).

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `AB_SCHEME` | `dual-root` | `dual-root`, `system-ab` | A/B layout scheme. |
| `AB_ACTIVE_SLOT` | (unset) | `a`, `b` | The slot that runs now. |
| `AB_TARGET_SLOT` | `inactive` | `inactive`, `a`, `b` | The slot to write. |
| `AB_PRESERVE_EXISTING` | `false` | boolean | Keep the existing A/B layout and write only the target slot. Needs kexec, so it excludes `DISABLE_KEXEC`, `SECUREBOOT_REENABLE` and `CAPRF_LEGACY_COMPAT`. |
| `AB_BOOT_SIZE_MB` | `512` | integer, MiB | Shared EFI partition size. |
| `AB_ROOT_SIZE_MB` | `32768` | integer, MiB | Size of each root slot. |
| `AB_STATE_SIZE_MB` | `0` | integer, MiB | State partition size. `0` fills the rest of the disk. |
| `AB_DATA_PARTITIONS` | one ext4 `BOOTY-DATA` partition on `/var` | JSON array of `{"label","sizeMB","filesystem","mountpoint"}` | Shared data partitions for `system-ab`. Only the last entry can use `sizeMB` 0. |
| `AB_SOURCE_ROOT_LABEL` | (unset) | GPT partition label | Source image partition to copy into the target slot. |
| `AB_SOURCE_ROOT_PARTITION` | `0` | 1-based partition number | Same as `AB_SOURCE_ROOT_LABEL`, by number. |

## Common network

BOOTy selects the ramdisk network stack in this order: GoBGP when
`NETWORK_MODE=gobgp`, FRR when `asn_server` and `underlay_subnet` or
`underlay_ip` are set, static when `STATIC_IP` is set, otherwise DHCP. Netplan
files from the provisioner override these keys. See
[Networking](networking.md).

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `NETWORK_MODE` | (unset) | `gobgp`, `frr`, `static`, `dhcp` | Validation accepts all four values. Only `gobgp` changes the stack selection. |
| `dns_resolver` | (unset) | IP addresses separated by commas | DNS servers for the ramdisk and the target OS. Legacy compat keeps a provisioner `/etc/resolv.conf` and ignores this key for the ramdisk. |
| `STATIC_IP` | (unset) | CIDR, for example `10.0.0.5/24` | Static address. Selects the static stack. |
| `STATIC_GATEWAY` | (unset) | IP address | Default gateway for the static stack. |
| `STATIC_IFACE` | first physical NIC, `bond0` with a bond, or the first VLAN interface | interface name | Interface for `STATIC_IP`. Required for network persistence and cloud-init network config without a bond. |
| `BOND_INTERFACES` | (unset) | interface names separated by commas | Create `bond0` from these NICs before the stack starts. |
| `BOND_MODE` | `802.3ad` | `802.3ad`, `lacp`, `balance-rr`, `active-backup`, `balance-xor` | Bond mode. An unknown value logs a warning and uses `802.3ad`. |
| `VLANS` | (unset) | `ID:parent[:address[:gateway]]` entries separated by commas | 802.1Q interfaces, for example `200:eno1:10.200.0.42/24,300:eno2`. Put IPv6 fields in brackets: `200:eno1:[2001:db8::42/64]:[2001:db8::1]`. |
| `PERSIST_NETWORK` | `false` | boolean | Write the network configuration into the target OS. Requires `OS_FAMILY`. |
| `ipmi_subnet` | (unset) | CIDR | IPMI subnet. BOOTy derives the underlay and overlay addresses from the host offset of the IPMI address in this subnet. |

## GoBGP and EVPN

These keys apply to the in-process GoBGP stack (`NETWORK_MODE=gobgp`). Keys
that FRR also reads say so.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `asn_server` | (unset) | uint32 | Local BGP ASN. Required. FRR also reads it. |
| `underlay_ip` | (unset) | IP address | Underlay loopback address and router ID. Takes precedence over `underlay_subnet`. FRR also reads it. |
| `underlay_subnet` | (unset) | CIDR | Underlay subnet. BOOTy derives the address from the IPMI address and `ipmi_subnet`. It reads the IPMI address from a host NIC named `ipmi*`, `bmc*`, `idrac*`, `ilo*`, `imm*`, `mgmt*` or `management*`, or with an address in `ipmi_subnet`. Without such a NIC, set `underlay_ip`. FRR also reads it. |
| `overlay_subnet` | (unset) | CIDR | Overlay subnet. BOOTy derives the overlay address like the underlay address. Without it the overlay address is the underlay address. FRR also reads it. |
| `provision_vni` | (unset) | uint32 | VXLAN VNI of the provisioning network. Required for GoBGP in every overlay type (1 to 16777215). GoBGP rejects `asn_server` above 65535 together with `provision_vni` above 65535. |
| `provision_ip` | (unset) | CIDR | Address on the provisioning bridge. |
| `provision_gateway` | (unset) | IP address | Gateway VTEP for BUM flooding and the kernel route. Optional with `EVPN_TYPE5_ONLY=true`. FRR ignores it. |
| `vpn_rt` | (unset) | `ASN:value` | Explicit EVPN route target. FRR also reads it with `dcgw_ips`. |
| `EVPN_L2_ENABLED` | `false` | boolean | Originate and handle Type-2 and Type-3 routes. Excludes `EVPN_TYPE5_ONLY`. |
| `EVPN_TYPE5_ONLY` | `false` | boolean | Direct Type-5 routing without an L2 gateway. Excludes `EVPN_L2_ENABLED`. |
| `BGP_PEER_MODE` | `unnumbered` | `unnumbered`, `dual`, `numbered` | Peering mode. |
| `BGP_INTERFACES` | all physical NICs | interface names separated by commas | Interfaces for unnumbered and dual peers. |
| `BGP_NEIGHBORS` | (unset) | IP addresses separated by commas | Numbered peers. Required for `dual` and `numbered`. |
| `BGP_REMOTE_ASN` | `0` | uint32 | Remote ASN of numbered peers. `0` means iBGP. Alias: `bgp_remote_asn`. |
| `BGP_UNDERLAY_AF` | `ipv4` | `ipv4` | Underlay address family. Validation rejects other values. |
| `BGP_OVERLAY_TYPE` | `evpn-vxlan` | `evpn-vxlan`, `none` | Overlay type. Validation also accepts `l3vpn`, but the GoBGP setup rejects it as not implemented. |
| `BGP_MIN_PEERS` | `1` | integer | Number of established peers that make the underlay ready. `0` means 1. |
| `BGP_KEEPALIVE` | `0` | uint32, seconds | Keepalive timer. `0` gives 3 s in GoBGP. FRR uses it only together with `BGP_HOLD`. Alias: `bgp_keepalive`. |
| `BGP_HOLD` | `0` | uint32, seconds | Hold timer. `0` gives 9 s in GoBGP. FRR uses it only together with `BGP_KEEPALIVE`. Alias: `bgp_hold`. |
| `BGP_AUTH_PASSWORD` | (unset) | string without white space | TCP-MD5 password for all peers. FRR also reads it. |
| `VRF_NAME` | (unset) | VRF name | Underlay VRF. Empty means no VRF. FRR renders `router bgp ASN vrf NAME`. Alias: `vrf_name`. |
| `VRF_TABLE_ID` | `1000` | uint32 | Routing table of `VRF_NAME`. `0` and `1` become `1000`. Alias: `vrf_table_id`. |

The lowercase aliases `bgp_keepalive`, `bgp_hold`, `bgp_remote_asn`,
`vrf_name` and `vrf_table_id` set the same values as the uppercase keys.

## FRR

The FRR stack runs when `asn_server` and `underlay_subnet` or `underlay_ip`
are set and `NETWORK_MODE` is not `gobgp`. It also reads the GoBGP keys marked
"FRR also reads it". When FRR setup fails, BOOTy falls back to DHCP. Only the
default and `iso` build flavors contain the FRR daemons.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `BFD_TRANSMIT_MS` | `0` | uint32, ms | BFD transmit interval. Set it together with `BFD_RECEIVE_MS`. Rejected with `NETWORK_MODE=gobgp`. Alias: `bfd_transmit_ms`. |
| `BFD_RECEIVE_MS` | `0` | uint32, ms | BFD receive interval. Set it together with `BFD_TRANSMIT_MS`. Rejected with `NETWORK_MODE=gobgp`. Alias: `bfd_receive_ms`. |
| `dcgw_ips` | (unset) | IP addresses separated by commas | Data center gateway iBGP peers. Turns on the onefabric FRR template. |
| `overlay_aggregate` | (unset) | CIDR | Aggregate address in the onefabric template. |
| `local_asn` | `0` | uint32 | `local-as` for the fabric peers. |

## Health, inventory, firmware and telemetry

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `HEALTH_CHECKS_ENABLED` | `false` | boolean | Run the health checks before destructive steps. A failed critical check stops provisioning. |
| `HEALTH_MIN_MEMORY_GB` | `0` | integer, GiB | Minimum RAM for `minimum-memory`. `0` passes the check. |
| `HEALTH_MIN_CPUS` | `0` | integer | Minimum CPU count for `minimum-cpu`. `0` passes the check. |
| `HEALTH_SKIP_CHECKS` | (unset) | check names separated by commas | Checks to skip: `disk-presence`, `disk-ioerr`, `memory-ecc`, `minimum-memory`, `minimum-cpu`, `nic-link-state`, `thermal-state`. |
| `HEALTH_CHECK_URL` | (unset) | URL | Receives the check results as JSON. |
| `INVENTORY_ENABLED` | `false` | boolean | Collect the hardware inventory. Failures are logged only. |
| `INVENTORY_URL` | (unset) | URL | Receives the inventory JSON. |
| `FIRMWARE_REPORT` | `false` | boolean | Collect BIOS, BMC, NIC and storage controller firmware versions. Legacy compat logs the NIC firmware versions also without it. |
| `FIRMWARE_URL` | (unset) | URL | Receives the firmware report. A failed POST fails provisioning. |
| `FIRMWARE_MIN_BIOS` | (unset) | version string | Minimum BIOS version. Applies only with `FIRMWARE_REPORT=true`. |
| `FIRMWARE_MIN_BMC` | (unset) | version string | Minimum BMC version. Applies only with `FIRMWARE_REPORT=true`. No collector reports a BMC version today, so a set value always fails. |
| `TELEMETRY_ENABLED` | `false` | boolean | Post the metrics summary when provisioning ends. |
| `METRICS_URL` | (unset) | URL | Receives the metrics summary. |
| `TELEMETRY_URL` | (unset) | URL | Fallback for `METRICS_URL`. |
| `EVENT_URL` | (unset) | URL | Receives the last provisioning event. |

The provisioning steps do not feed the telemetry collector yet. The metrics
summary has no step data and BOOTy sends no events.

## Crash artifacts

These keys apply to the provision and deprovision modes.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `CRASH_ARTIFACTS_ENABLED` | `false` | boolean | Inspect the existing OS for crash evidence before destructive steps and upload an archive. |
| `CRASH_ARTIFACTS_PREPARE_URL` | (unset) | URL | Receives the crash metadata and returns upload instructions. |
| `CRASH_ARTIFACTS_UPLOAD_URL` | (unset) | URL | Direct proxy upload endpoint when no prepare URL is set. |
| `CRASH_ARTIFACTS_MAX_MB` | `256` | integer, MiB | Largest archive payload. |
| `CRASH_ARTIFACTS_UPLOAD_TIMEOUT_SEC` | `120` | integer, seconds | Timeout of the prepare and upload requests. |
| `CRASH_ARTIFACTS_INCLUDE_MEMORY_DUMPS` | `false` | boolean | Include raw `vmcore` and systemd coredump files. These files can contain secrets. |

## Secure Boot

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `SECUREBOOT_REENABLE` | `false` | boolean | Append `SECUREBOOT_REENABLE=true` to the success message and skip kexec. CAPRF then turns Secure Boot on. |
| `SECUREBOOT_SHIM_SHA256` | (unset) | 64 hex digits, optional `sha256:` prefix | Expected digest of the installed shim. |
| `SECUREBOOT_GRUB_SHA256` | (unset) | same | Expected digest of the installed GRUB EFI binary. |
| `SECUREBOOT_KERNEL_SHA256` | (unset) | same | Expected digest of the installed kernel. |
| `SECUREBOOT_PINNED_DIGESTS` | (unset) | JSON object with keys `shim`, `grub`, `kernel` | All pins in one key. A single-component key wins over its entry here. |

## Sysext

See [Sysext provisioning](sysext-provisioning.md).

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `SYSEXT_ENABLED` | `false` | boolean | Copy the sysext layers into the target root. |
| `SYSEXT_LAYERS` | (unset) | JSON array of `{"name","version","source","fileName","sha256","mode"}` | Layers. `source` is a local path, an HTTPS URL or an `oci://` reference. |
| `SYSEXT_DEFAULT_MODE` | `preload` | `preload`, `active` | Mode for layers without `mode`. |
| `SYSEXT_CATALOG_DIR` | `/usr/lib/tcaas-sysext/preloaded` | target path | Inactive catalog directory for preloaded layers. |
| `SYSEXT_ACTIVE_DIR` | `/var/lib/extensions` | target path | Directory for active layers. |
| `SYSEXT_ALLOW_INSECURE_HTTP` | `false` | boolean | Accept `http://` layer sources. |

## OverlayFS

These keys write the Ubuntu overlayroot configuration into the target root.
`OVERLAYFS_ENABLED=true` requires `OS_FAMILY=ubuntu`.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `OVERLAYFS_ENABLED` | `false` | boolean | Write `/etc/overlayroot.local.conf`. |
| `OVERLAYFS_MODE` | `tmpfs` | `tmpfs`, `device` | Volatile or persistent upper layer. |
| `OVERLAYFS_DEVICE` | (unset) | for example `LABEL=BOOTY-OVERLAY` | Backing filesystem. Required for `device`, rejected for `tmpfs`. |
| `OVERLAYFS_DIRECTORY` | (unset) | directory | overlayroot `dir=` value. |
| `OVERLAYFS_TIMEOUT_SEC` | `0` | integer, seconds | Wait time for the backing device. Only for `device`. |
| `OVERLAYFS_UPPER_DIR` | (unset) | target path | Upper directory that BOOTy creates for custom hooks. Set it together with `OVERLAYFS_WORK_DIR`. |
| `OVERLAYFS_WORK_DIR` | (unset) | target path | Work directory. Set it together with `OVERLAYFS_UPPER_DIR`. |
| `OVERLAYFS_RECURSE` | `false` | boolean | Render `recurse=1`. |
| `OVERLAYFS_SWAP` | `false` | boolean | Render `swap=1`. |
| `OVERLAYFS_DEBUG` | `false` | boolean | Render `debug=1`. |

## Cloud-init

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `CLOUDINIT_ENABLED` | `false` | boolean | Write a cloud-init seed into the target root. Rejected with `OS_FAMILY=flatcar`. |
| `CLOUDINIT_DATASOURCE` | `nocloud` | `nocloud`, `configdrive` | Seed type. |

## OCI pre-pulls

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `OCI_PREPULL_ENABLED` | `false` | boolean | Pull images into the target root and install a first-boot import unit. |
| `OCI_PREPULL_IMAGES` | (unset) | references separated by commas or spaces | Images to cache. The `oci://` prefix is optional. |
| `OCI_PREPULL_CACHE_DIR` | `/var/lib/booty/oci-prepulls` | target path | Cache directory for archives, catalog and import state. |
| `OCI_PREPULL_IMPORT_NAMESPACE` | `k8s.io` | containerd namespace | Namespace for the first-boot import. |
| `OCI_PREPULL_ALLOW_INSECURE` | `false` | boolean | Allow plain HTTP registries. |

## Rescue

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `RESCUE_MODE` | `reboot`, `wait` in legacy compat | `reboot`, `retry`, `shell`, `wait` | Action after a failed provisioning. `retry` waits 30 s between up to 3 retries, then reboots. `shell` drops to a shell on the console. `wait` keeps the ramdisk running. Standby mode applies it to failed `provision` commands. |
| `RESCUE_SSH_PUBKEY` | (unset) | SSH public key | Authorized key for root in `shell` and `wait` modes. BOOTy starts `dropbear`. |
| `RESCUE_PASSWORD_HASH` | (unset) | crypt(3) hash | Root password hash in `shell` and `wait` modes. |
| `RESCUE_AUTO_MOUNT` | `false` | boolean | Mount the found disks in `shell` and `wait` modes. |
| `RESCUE_TIMEOUT` | `0` | integer, seconds | Parsed, but no code uses the value. |

## Process environment

BOOTy reads these variables from its process environment, not from the
parsed vars. Set them on the kernel command line, or in the vars file with
`CAPRF_LEGACY_COMPAT=true`.

| Variable | Default | Values | Description |
|----------|---------|--------|-------------|
| `BOOTY_RESUME` | `false` | Go `strconv.ParseBool` values, for example `true`, `1` | Save a checkpoint in `/tmp/booty-checkpoint.json` and skip completed steps when provisioning runs again in the same boot. An unreadable checkpoint fails provisioning. |
| `BOOTY_ALLOW_REMOVABLE` | unset | exactly `true` | Accept removable disks in the default disk selection. The legacy compat disk selection ignores it. |
| `BOOTY_NO_REBOOT` | unset | any non-empty value | Do not call the reboot syscall. It does not stop a power off. For test containers. |
| `https_proxy`, `http_proxy`, `no_proxy` | unset | proxy URLs | Standard Go proxy settings for the HTTP clients. |

## Parsed but not used

BOOTy parses these keys, but no code acts on the value:

| Key | Note |
|-----|------|
| `DEBUG_URL` | No code sends to it. Debug dumps go to the log, and `LOG_URL` ships them. |
| `leaf_asn` | Logged in the debug dump only. |
| `MOK_CERT_PATH` | MOK enrollment is not implemented. |
| `MOK_PASSWORD` | MOK enrollment is not implemented. |
| `RESCUE_TIMEOUT` | See [Rescue](#rescue). |

## Planned

- `LUKS_*` keys, for example `LUKS_ENABLED`: the parser rejects every key
  that starts with `LUKS_`. The `pkg/disk/luks` library is not part of the
  provisioning flow.
- Planned (#581): `DISK_MIRRORING_MODE`.
- Planned (#622): `NETWORK_WAIT_MAX_ATTEMPTS`.
