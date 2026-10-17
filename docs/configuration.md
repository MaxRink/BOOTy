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

The parser is `ParseVars` in `pkg/caprf/client.go`, with the parity options in
`pkg/caprf/parity_vars.go`. The test
`TestConfigurationDocListsEveryVarsKey` in `docs_consistency_test.go` fails
when the parser accepts a key that this page does not list.

## Configuration sources

BOOTy runs as PID 1 and reads its configuration from two sources:

1. The vars file `/deploy/vars`. CAPRF puts it on the ISO. All keys in this
   page come from this file, except the keys in
   [Process environment](#process-environment).
2. The process environment of PID 1. The kernel passes unknown `name=value`
   kernel command line parameters to init as environment variables. With
   `EXPORT_VARS=true` (preset by `CAPRF_LEGACY_COMPAT=true`), BOOTy also
   exports every vars file entry to the environment.

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
`FAILURE_ACTION=wait` or `CAPRF_LEGACY_COMPAT=true` it keeps the ramdisk
running instead.

Boolean keys are true for `true`, `1` or `yes`, in any letter case. All other
values are false.

## Mode and lifecycle

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `MODE` | `provision` | `provision`, `deprovision`, `hard`, `soft-deprovision`, `soft`, `decommission`, `standby`, `dry-run`, `check` | Operating mode. `hard` is an alias of `deprovision`, `soft` is an alias of `soft-deprovision`. `deprovision` wipes the disks and removes the EFI boot entries. `soft-deprovision` renames `grub.cfg`. `decommission` wipes every disk and removes every EFI boot entry, and logs failures without stopping. `check` runs the health checks only. |
| `DRY_RUN` | `false` | boolean | Sets `MODE=dry-run`. Dry-run skips the network setup and the token flow, runs read-only checks and reports the result. |
| `HOSTNAME` | (unset) | host name | Machine name. BOOTy writes it into the target OS, sends it to `TOKEN_URL`, and with `RAMDISK_HOSTNAME=true` sets it as the ramdisk host name for provisioning. |
| `PROVISION_TARGET_OS` | (unset) | `linux` | Target OS class. Provisioning preflight fails when it is empty or not `linux`. Alias: `TARGET_OS`. |
| `TARGET_OS` | (unset) | `linux` | Alias of `PROVISION_TARGET_OS`. |
| `OS_FAMILY` | (unset) | `ubuntu`, `rhel`, `flatcar` | Target OS family. Required by `PERSIST_NETWORK=true`. `OVERLAYFS_ENABLED=true` requires `ubuntu`. `flatcar` rejects `CLOUDINIT_ENABLED=true`. `rhel` rejects `PERSIST_NETWORK=true`. |
| `DISK_LINKS` | `true` | boolean | Before the target chroot, create the `/dev/disk/by-uuid`, `by-partuuid`, `by-label` and `by-partlabel` links from `blkid`, like udev on the installed system. `update-grub` then writes `root=UUID=` instead of `root=/dev/sdXN`, which breaks the boot when the disk order changes. Like udev, `by-uuid` and `by-label` are made for filesystems, swap and crypto containers only (not for RAID or LVM members), `by-partuuid` and `by-partlabel` for every partition. When two devices have the same filesystem UUID (a disk that was not wiped and holds a copy of the image), the first one wins and BOOTy logs a warning; `root=UUID=` is then ambiguous on the installed system too, like with the legacy scripts. A `blkid` failure other than "no device found" fails the run. YAML `provision.diskLinks`. |
| `EFI_BOOT_ENTRY_REQUIRED` | `true` | boolean | On a system booted via UEFI, a failure of the EFI boot entry steps fails the run, like the legacy scripts that ran `efibootmgr` with `set -e`. `false` turns only the "EFI variables unavailable" failures into warnings: efivarfs can not be mounted or is not mounted (for example a kernel booted with `efi=noruntime` or `noefi`, which keep `/sys/firmware/efi`), `efibootmgr` is missing, `efibootmgr` can not read the EFI variables, or the firmware refuses to delete a managed entry (for example a locked boot order on Lenovo XCC, HPE iLO or Dell iDRAC systems). Creating the entry is not covered: a failed `efibootmgr -c` (it ran and returned an error, also when the firmware refuses the new entry) or a missing EFI loader on the ESP always fails the run. BIOS systems (no `/sys/firmware/efi`) and BOOTy in a container (`/.dockerenv` or `/run/.containerenv`, which sees the host EFI variables) skip these steps. Deprovisioning runs them before it wipes a disk. `MODE=decommission` is always best effort and skips on BIOS and in a container. YAML `provision.efiBootEntryRequired`. |
| `DISABLE_KEXEC` | `false` | boolean | Skip kexec after provisioning and reboot instead. Can not be combined with `AB_PRESERVE_EXISTING=true`. |
| `KEXEC_UNMOUNT_TARGET` | `true` | boolean | After the kexec kernel is loaded, unmount every mount below the target root, so the installed OS does not start with a journal that needs recovery. Only a busy mount of a block device is remounted read-only instead; a busy virtual mount (proc, sysfs, the `/dev` bind) is left as it is. A failure is logged and kexec continues. YAML `provision.kexecUnmountTarget`. |
| `CAPRF_LEGACY_COMPAT` | `false` | boolean | Preset that makes BOOTy behave like the CAPRF legacy shell provisioner. It sets the options in [CAPRF legacy compatibility](#caprf-legacy-compatibility) that the vars file does not set. |
| `SUCCESS_ACTION` | `reboot` | `reboot`, `poweroff` | End of a successful provision, deprovision or decommission. `reboot`: provision kexecs into the installed OS (reboot when kexec is disabled or fails), the other modes reboot. `poweroff`: power off, never kexec. `poweroff` can not be combined with `AB_PRESERVE_EXISTING=true`. YAML `successAction`. Preset: `poweroff`. |
| `PROVISIONER_FILES_TARGET` | `target` | `target`, `ramdisk` | Where the provisioner files (`/deploy/file-system`) go. `target`: into the installed OS. `ramdisk`: into the ramdisk root before the network starts, like `copy_files`; netplan files get mode `0600`, a provisioner `/etc/resolv.conf` stays in the ramdisk, and nothing reaches the installed OS. If the copy fails, BOOTy waits without a callback. YAML `provisionerFilesTarget`. Preset: `ramdisk`. |
| `WRITE_RESOLV_CONF` | `true` | boolean | Write the installed OS `/etc/resolv.conf` from `dns_resolver`. `false` keeps the image file, like the legacy scripts. YAML `provision.writeResolvConf`. Preset: `false`. |
| `RAMDISK_HOSTNAME` | `false` | boolean | For provisioning, set `HOSTNAME` as the ramdisk host name, like `hostname "$HOSTNAME"` in `provision.sh`. A failure is only logged. YAML `ramdiskHostname`. Preset: `true`. |
| `FAILURE_ACTION` | `reboot` | `reboot`, `wait` | End of a failed run. `wait`: report the error to `ERROR_URL` and keep the ramdisk running. Applies to a failed deprovision, a failure before the mode runs (vars file, network, JWT exchange) and exhausted JWT renewal. For a failed provision it is the default of `RESCUE_MODE`; an explicit `RESCUE_MODE` wins. YAML `failureAction`. Preset: `wait`. |
| `MACHINE_EXTRA_KERNEL_PARAMS` | (unset) | kernel parameters | Extra kernel parameters for the installed OS GRUB configuration. A `console=` token is folded into the serial console resolution. |
| `EXTRA_KERNEL_PARAMS` | (unset) | kernel parameters | Extra kernel parameters that the legacy provisioner reads. BOOTy appends them after `MACHINE_EXTRA_KERNEL_PARAMS`, whatever the line order. |
| `MACHINE_SERIAL_CONSOLE` | (unset) | for example `ttyS1,115200n8`, `ttyS0`, `none` | Serial console of the installed OS. It overrides the firmware evidence (ACPI SPCR, device tree, sysfs). `none` configures no serial console. `GRUB_CMDLINE_MODE=append` ignores it and logs a warning. Alias: `SERIAL_CONSOLE`. |
| `SERIAL_CONSOLE` | (unset) | see above | Alias of `MACHINE_SERIAL_CONSOLE`. |
| `GRUB_CMDLINE_MODE` | `resolved` | `resolved`, `append` | How BOOTy writes `10-caprf-kernel-params.cfg`. `resolved`: the [serial console resolver](serial-console-resolution.md) picks at most one console; for that console BOOTy writes `console=`, a GRUB serial terminal and getty units (none with `MACHINE_SERIAL_CONSOLE=none`); `update-grub` must exist. `append`: like `provision.sh`, `GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ds=nocloud console=ttyS0,115200n8 <extra>"` (`ttyS1` when DMI `sys_vendor` is `Lenovo`), extra parameters verbatim, `update-grub` only when the image has it, no getty units, `MACHINE_SERIAL_CONSOLE` ignored. YAML `provision.grubCmdlineMode`. Preset: `append`. |
| `FAILURE_DOMAIN` | (unset) | Kubernetes label value | Kubelet node label `topology.kubernetes.io/zone`. |
| `REGION` | (unset) | Kubernetes label value | Kubelet node label `topology.kubernetes.io/region`. |
| `PROVIDER_ID` | (unset) | string without white space | Kubelet `--provider-id`. Cloud-init also uses it as the instance ID. |
| `POST_PROVISION_CMDS` | (unset) | commands separated by `;` | Commands that run in the target root chroot near the end of provisioning. Each command can contain only letters, digits, white space and `_ . / : = , @ % + -`. A failed command fails the provisioning. |
| `MACHINE_COMMANDS_MODE` | `command` | `command`, `script` | How `run-machine-commands` runs the files in `/deploy/machine-commands`, in CAPRF index order. `command`: each file holds one command; shell syntax (`;`, `\|`, `&&`, `>`, `<`, `$(`, backtick) is rejected. `script`: like `run_script_in_chroot`, each file runs as a script in the target root, with its `#!` interpreter or `/bin/sh`. YAML `provision.machineCommandsMode`. Preset: `script`. |
| `NUM_VFS` | `0` | integer | Number of SR-IOV VFs to set with `mstconfig` on each Mellanox NIC. `0` skips the step. A positive value fails the run when no Mellanox NIC or `mstconfig` device is found. With `NUM_VFS_MODE=raise`, `0` means 32 and missing NICs or `mstconfig` are skipped. |
| `NUM_VFS_MODE` | `set` | `set`, `raise` | How `setup-mellanox` applies `NUM_VFS`. `set`: only with `NUM_VFS` > 0, sets `SRIOV_EN` and `NUM_OF_VFS`, fails without a Mellanox NIC. `raise`: on every provision, raises `NUM_OF_VFS` to `NUM_VFS` (32 when `0`) with `mstconfig` on each ConnectX-4/5/6/6 Dx/6 Lx/7 Ethernet function, like `setup_mellanox_num_of_vfs`; skips quietly without `mstconfig` or such a NIC; a failed `mstconfig` query or set fails the run. A change forces a reboot instead of kexec. YAML `provision.firmware.numVFsMode`. Preset: `raise`. |

## CAPRF endpoints, authentication and transport

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `INIT_URL` | (unset) | URL | Receives the `init` status. BOOTy also uses it as the network connectivity target. |
| `SUCCESS_URL` | (unset) | URL | Receives the success status. Connectivity target when `INIT_URL` is unset. |
| `ERROR_URL` | (unset) | URL | Receives the error status. |
| `LOG_URL` | (unset) | URL | BOOTy ships each log line of level info and higher to this URL. |
| `HEARTBEAT_URL` | (unset) | URL | Standby mode: heartbeat endpoint, every 30 s. Required for `MODE=standby`. |
| `COMMANDS_URL` | (unset) | URL | Standby mode: command poll endpoint, every 10 s. BOOTy posts command ACKs to `<COMMANDS_URL>/ack`. Required for `MODE=standby`. See [Agent mode contract](caprf-integration.md#agent-mode-contract). |
| `STANDBY_DEPROVISION_SUCCESS_ACTION` | (unset) | `standby` | Standby mode: `standby` returns to the standby loop after a successful `deprovision` command, in the same boot, without a reboot or power off. Unset ends the run like `SUCCESS_ACTION`. YAML `agent.deprovisionSuccessAction`. `CAPRF_LEGACY_COMPAT` does not preset it. |
| `TOKEN` | (unset) | string | Bearer token. With `TOKEN_URL` it is the bootstrap token for the JWT exchange. |
| `TOKEN_URL` | (unset) | HTTPS URL | JWT endpoint. BOOTy exchanges `TOKEN` for a JWT after the network is up and renews it in the background. Plain HTTP is accepted only for `localhost`, `127.0.0.1` and `::1`. Requires `TOKEN` and `HOSTNAME`. A failed exchange or an exhausted renewal ends the run. |
| `TOKEN_ALGORITHM` | (unset) | `RS256`, `ES256` | JWT signing algorithm that BOOTy requests from `TOKEN_URL`. Empty leaves the choice to the server. |
| `TOKEN_EXPIRES_IN` | (unset) | integer, seconds | Lifetime of `TOKEN`, counted from the start of BOOTy, when there is no `TOKEN_URL` to renew it. It does not depend on the ramdisk clock. BOOTy starts only after the virtual media insert and the boot, so the count runs late by that time: CAPRF subtracts a boot allowance from the value it renders. Wins over `TOKEN_EXPIRES_AT`. BOOTy stops the active task `TOKEN_RENEW_MARGIN_SECONDS` before the expiry, reports the reason to `ERROR_URL` and ends like a failed run (`FAILURE_ACTION`). Ignored with `TOKEN_URL`: the `expires_in` of the token exchange at start is authoritative. YAML `transport.tokenExpiresInSeconds`. |
| `TOKEN_EXPIRES_AT` | (unset) | RFC 3339 time | Expiry of `TOKEN` when there is no `TOKEN_URL`, like `TOKEN_EXPIRES_IN`. It needs a correct ramdisk clock (RTC): BOOTy does not sync the time. Prefer `TOKEN_EXPIRES_IN`. Unset (both): the expiry is unknown and BOOTy does not stop. Ignored with `TOKEN_URL`. YAML `transport.tokenExpiresAt`. |
| `TOKEN_RENEW_MARGIN_SECONDS` | `0` (20% of the token lifetime) | integer, seconds | How long before the token expiry the renewal starts (`TOKEN_URL`), or BOOTy stops (`TOKEN_EXPIRES_AT`). At most half the lifetime. For `TOKEN_EXPIRES_IN` and `TOKEN_EXPIRES_AT` the lifetime is the time left at the start, so the default is 20% of it. YAML `transport.tokenRenewMarginSeconds`. |
| `TOKEN_RENEW_RETRIES` | `0` (5) | integer, 1-100 | Renewal attempts before the renewal fails for good. The attempts also stop when the token expires. Then BOOTy stops the active task, reports the reason and ends like a failed run. YAML `transport.tokenRenewRetries`. |
| `TOKEN_RENEW_BACKOFF_SECONDS` | `0` (1) | integer, seconds | Wait after the first failed renewal attempt; it doubles after each failure up to 60 seconds and never goes past the expiry. YAML `transport.tokenRenewBackoffSeconds`. |
| `INSECURE_TRANSPORT` | `false` | boolean | Send the bearer token over plain HTTP. Without it, BOOTy refuses to send the token to an `http://` URL that is not a loopback address. YAML `transport.insecure`. Preset: `true`. |
| `SKIP_TLS_VERIFY` | `false` | boolean | Do not verify TLS certificates for the CAPRF callbacks, the connectivity check and image downloads, like `curl --insecure`. BOOTy logs a warning. `TOKEN_URL` and the crash artifact upload always verify. YAML `transport.skipTLSVerify`. Preset: `true`. |
| `EXPORT_VARS` | `false` | boolean | Export every vars file entry whose key is a valid shell identifier to the process environment, like `source vars`. Proxy entries (`https_proxy`, `no_proxy`) and the [process environment](#process-environment) variables then apply. YAML `transport.exportVars`. Preset: `true`. |

The CAPRF callback, connectivity check and image HTTP clients use the
standard proxy variables (`https_proxy`, `http_proxy`, `no_proxy`) from the
process environment. The `TOKEN_URL` client uses no proxy and always verifies
TLS, also with `SKIP_TLS_VERIFY=true` or `EXPORT_VARS=true`.

## CAPRF legacy compatibility

`CAPRF_LEGACY_COMPAT=true` (YAML `legacyCompat: true`) is a preset.
`Config.Validate` sets each option below that the configuration does not
set. An explicit option always wins over the preset, also an explicit
`false` or the default value. Without the preset the options keep their
defaults.

| Behaviour | Vars key | YAML option | Default | Preset value |
|-----------|----------|-------------|---------|--------------|
| End of a successful run | `SUCCESS_ACTION` | `successAction` | `reboot` | `poweroff` |
| End of a failed run | `FAILURE_ACTION` | `failureAction` | `reboot` | `wait` |
| Target disk auto-detection | `DISK_SELECTION` | `provision.disk.selection` | `prefer-nvme` | `lsblk` |
| Wipe every disk, not only the target | `WIPE_ALL_DISKS` | `provision.disk.wipeAllDisks` | `false` | `true` |
| `blkdiscard` NVMe disks before a wipe of all disks | `DISCARD_NVME` | `provision.disk.discardNVMe` | `false` | `true` |
| Wipe of a disk | `WIPE_DEPTH` | `provision.disk.wipeDepth` | `thorough` | `quick` |
| Disks whose RAID superblocks provisioning zeroes | `RAID_SUPERBLOCK_CLEAR` | `provision.disk.raidSuperblockClear` | `wiped` | `all` |
| Bearer token over plain HTTP | `INSECURE_TRANSPORT` | `transport.insecure` | `false` | `true` |
| No TLS certificate check, like `curl --insecure` | `SKIP_TLS_VERIFY` | `transport.skipTLSVerify` | `false` | `true` |
| Plain HTTP image URLs | `IMAGE_ALLOW_INSECURE_HTTP` | `provision.image.allowInsecureHTTP` | `false` | `true` |
| Export the vars entries, like `source vars` | `EXPORT_VARS` | `transport.exportVars` | `false` | `true` |
| Provisioner files into the ramdisk, like `copy_files` | `PROVISIONER_FILES_TARGET` | `provisionerFilesTarget` | `target` | `ramdisk` |
| Write the installed OS `resolv.conf` | `WRITE_RESOLV_CONF` | `provision.writeResolvConf` | `true` | `false` |
| Ramdisk host name for provisioning | `RAMDISK_HOSTNAME` | `ramdiskHostname` | `false` | `true` |
| GRUB kernel parameters like `provision.sh`, no getty units | `GRUB_CMDLINE_MODE` | `provision.grubCmdlineMode` | `resolved` | `append` |
| `nic_firmware` log lines on every provision | `NIC_FIRMWARE_LOG` | `provision.firmware.logNICs` | `false` | `true` |
| Raise `NUM_OF_VFS` on every ConnectX NIC | `NUM_VFS_MODE` | `provision.firmware.numVFsMode` | `set` | `raise` |
| Grow the last GPT Linux partition, not the root | `GROW_PARTITION` | `provision.disk.growPartition` | `root` | `last-linux` |
| Network wait like `wait_for_network` | `NETWORK_WAIT_MODE` | `network.waitMode` | `default` | `rounds` |
| Machine commands as scripts, like `run_script_in_chroot` | `MACHINE_COMMANDS_MODE` | `provision.machineCommandsMode` | `command` | `script` |

[CAPRF legacy migration](caprf-legacy-migration.md) describes all
behaviour changes of this mode.

| Key | Default | Values | Effect |
|-----|---------|--------|--------|
| `NETWORK_WAIT_MODE` | `default` | `default`, `rounds` | `default`: up to 3 network setups, each waiting up to 5 minutes. `rounds`: like the legacy `wait_for_network`, one network setup and `NETWORK_WAIT_MAX_ATTEMPTS` rounds of 20 seconds; in FRR mode, FRR restarts after each failed round unless a BGP session is established. YAML `network.waitMode`. Preset: `rounds`. |
| `NETWORK_WAIT_MAX_ATTEMPTS` | (unset or empty, 15 rounds) | integer 0-100000 | With `NETWORK_WAIT_MODE=rounds`: the number of 20 second connectivity rounds before the run fails. `0` means no round: the run fails at once, like `${NETWORK_WAIT_MAX_ATTEMPTS:-15}` in the legacy provisioner. Ignored with the default wait. YAML `network.waitMaxAttempts`. |

## Disk and image

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `IMAGE` | (unset) | URLs separated by commas or spaces | OS image sources: `https://`, `http://` or `oci://`. With more than one URL, BOOTy probes each URL with a ranged GET and uses the fastest. Required for provisioning. |
| `IMAGE_ALLOW_INSECURE_HTTP` | `false` | boolean | Accept `http://` image URLs. YAML `provision.image.allowInsecureHTTP`. Preset: `true`. |
| `IMAGE_CHECKSUM` | (unset) | hex digest, optional `sha256:` or `sha512:` prefix | Expected digest of the image; `IMAGE_CHECKSUM_SCOPE` selects the bytes. Required for `oci://` sources that are not pinned by digest, and with `IMAGE_SIGNATURE_URL`. A mismatch fails the run: whole-disk streaming wipes the start of the target disk, A/B and partition-layout root streaming invalidate the target partitions they wrote. |
| `IMAGE_CHECKSUM_TYPE` | from digest length | `sha256`, `sha512` | Hash algorithm of `IMAGE_CHECKSUM`. |
| `IMAGE_CHECKSUM_SCOPE` | `download` | `download`, `decompressed` | Bytes that `IMAGE_CHECKSUM` covers. `download`: the file as downloaded, before decompression or qcow2 conversion, like the `.sha256` files published next to `.lz4` images and like the CAPRF legacy provisioner. `decompressed`: the decompressed image; for qcow2 the converted raw image. For `oci://` sources both scopes hash the uncompressed layer (`layer.Uncompressed()`), not the compressed blob of the layer digest. A mismatch error names the scope. YAML `provision.image.checksumScope`. |
| `IMAGE_SIGNATURE_URL` | (unset) | URL | Detached GPG signature of the image. Not supported for `oci://` sources. |
| `IMAGE_GPG_PUBKEY` | (unset) | file path in the ramdisk | Public key for `IMAGE_SIGNATURE_URL`. |
| `IMAGE_MODE` | `whole-disk` | `whole-disk`, `partition`, `ab` | How BOOTy writes the image. See [A/B partitioning](#ab-partitioning) for `ab`. |
| `IMAGE_SOURCE_ROOT_LABEL` | (unset) | GPT partition label | Source image root partition to stream into a `PARTITION_LAYOUT` root. Excludes `IMAGE_SOURCE_ROOT_PARTITION`. |
| `IMAGE_SOURCE_ROOT_PARTITION` | `0` | 1-based partition number | Same as `IMAGE_SOURCE_ROOT_LABEL`, by number. |
| `DISK_DEVICE` | auto-detect | block device path | Target disk. Hard deprovisioning then wipes only this disk instead of all disks. |
| `DISK_SERIAL_NUMBER` | (unset) | serial number | Select the target disk by its exact serial number when `DISK_DEVICE` is unset. Alias: `DISK_SERIAL`; the last line wins. |
| `DISK_SERIAL` | (unset) | serial number | Alias of `DISK_SERIAL_NUMBER`. |
| `MIN_DISK_SIZE_GB` | `0` | integer, GiB | Smallest disk that auto-detection accepts. `0` means no limit. |
| `DISK_SETTLE_TIMEOUT_SEC` | `30` | integer 0-600, seconds | Before the target disk is selected (provision, deprovision) and before a wipe of all disks (decommission), wait until the set of disks in `/sys/block` did not change for 2 seconds, and until the disk that the selection uses exists: `PARTITION_LAYOUT` device (a `/dev/disk/` path there is used as it is, without a wait), `DISK_DEVICE` (deprovision: `deprovision.device` first) or `DISK_SERIAL_NUMBER`; at most this long in total. A serial number error that waiting can not fix (several disks with the serial, a disk below `MIN_DISK_SIZE_GB`) ends the wait at once. SAS/RAID HBA drivers scan their buses asynchronously after the module load. A timeout is logged and the selection reports the missing disk. A `DISK_DEVICE` or `deprovision.device` below `/dev/disk/` (by-id, by-path) fails at once in every mode: the ramdisk has no udev, so these links do not exist. `0` disables the wait. YAML `provision.disk.settleTimeoutSec`. |
| `DISK_SELECTION` | `prefer-nvme` | `prefer-nvme`, `lsblk` | Auto-detection rule when `DISK_DEVICE` and `DISK_SERIAL` are unset. `prefer-nvme`: NVMe first, then the largest disk, removable media skipped. `lsblk`: the first disk of at least `MIN_DISK_SIZE_GB` in `lsblk` order (NVMe and virtio, then SCSI/SATA), like `_select_single_disk`; removable media are not skipped. YAML `provision.disk.selection`. Preset: `lsblk`. |
| `WIPE_ALL_DISKS` | `false` | boolean | `wipe-disks` wipes every disk, like `wipe_all_disks`: for provisioning, and for hard deprovisioning without `DISK_DEVICE`. Failed disk wipes are logged and do not stop the run. A disk in use is skipped; the run fails when the selected target disk is in use, when every disk is in use or when the in-use check cannot be made. See [Disk handling](disk.md#wipe-behaviour). `SECURE_ERASE` has no effect. Not for dry-run, RAID configs or A/B preserve-existing upgrades. YAML `provision.disk.wipeAllDisks`. Preset: `true`. |
| `GROW_PARTITION` | `root` | `root`, `last-linux` | Partition that `grow-partition` and `resize-filesystem` enlarge. `last-linux`: the last GPT Linux filesystem partition, like `provision.sh`, also a data partition after the root; it gets `e2fsck -p -f` before `growpart` and an offline `resize2fs`. Without a GPT Linux partition BOOTy grows the root. YAML `provision.disk.growPartition`. Preset: `last-linux`. |
| `DISCARD_NVME` | `false` | boolean | Run `blkdiscard` on every NVMe disk before a wipe of all disks (`WIPE_ALL_DISKS=true` or `MODE=decommission`). Failures are logged. With the default `WIPE_DEPTH=thorough` every wiped disk is discarded anyway, so this adds something only with `WIPE_DEPTH=quick`. YAML `provision.disk.discardNVMe`. Preset: `true`. |
| `WIPE_DEPTH` | `thorough` | `thorough`, `quick` | How `wipe-disks` and `decommission` clear a disk. `thorough`: stop the LVM, dm-crypt and MD devices on the disk, remove the LVM PV labels and MD superblocks, run `wipefs -af` and zero the first and last MiB of every partition, then `sgdisk --zap-all` (GPT, GPT backup and MBR), `wipefs -af` and zero the first and last MiB of the disk, and `blkdiscard` the whole disk where the device supports it, also before each provision image write (slow on large thin LUNs, works like an erase on most SSDs). A restored old partition table then finds no file system, PV or array. `quick`: `sgdisk --zap-all` and `wipefs -af` on the disk only, so the partition contents come back with the old partition table, like the legacy `wipe_all_disks`. `SECURE_ERASE` replaces the wipe. See [Disk handling](disk.md#wipe-behaviour). YAML `provision.disk.wipeDepth`. Preset: `quick`. |
| `RAID_SUPERBLOCK_CLEAR` | `wiped` | `wiped`, `all` | Disks on which provisioning runs `mdadm --zero-superblock` for every `linux_raid_member` (whole disks and partitions) after `stop-raid` stopped the arrays. The quick wipe keeps the metadata 1.0 superblocks at the end of old member partitions, so a replicated or restored partition table would bring the old array back. `wiped`: in `wipe-disks`, right before the wipe, only on the disks that it wipes: the target disk and the `DISK_MIRRORING_MODE` mirror disk, or every disk with `WIPE_ALL_DISKS=true`; not with `provision.disk.raid`, whose member disks `setup-raid` wipes. `all`: in `stop-raid`, on every disk of the host, like `clear_mdadm_superblocks` of the CAPRF legacy provisioner; this also ends foreign data arrays on disks that are not provisioned. Hard deprovision zeroes the superblocks on the disks it wipes with both values: with `deprovision.device` or `DISK_DEVICE` on that disk and its mirror companion, else on every disk. A failed zeroing is logged; a failed `lsblk` fails the step. YAML `provision.disk.raidSuperblockClear`. Preset: `all`. |
| `SECURE_ERASE` | `false` | boolean | Provisioning only: erase the target disk with `nvme format --ses=1` or ATA Security Erase instead of a signature wipe. Deprovisioning, decommission and `WIPE_ALL_DISKS=true` ignore it. A missing `nvme` or `hdparm` tool fails the run. |
| `ROOT_PARTITION_LABEL` | (unset) | GPT partition name | Root partition of a whole-disk image. Excludes `ROOT_PARTITION_NUMBER`. |
| `ROOT_PARTITION_NUMBER` | `0` | 1-based partition number | Root partition of a whole-disk image when it has no stable label. |
| `PARTITION_LAYOUT` | (unset) | JSON object | Declarative GPT layout with optional LVM. An invalid layout stops the parse. |
| `DISK_MIRRORING_MODE` | (unset) | `raid-1`, `auto`, `none` | Mirror the installed system with RAID1 across a pair of disks with equal size and sector size, like the CAPRF legacy provisioner. `raid-1` needs a pair, `auto` falls back to one disk, `none` is the default. Ignored when `DISK_DEVICE` or `DISK_SERIAL_NUMBER` selects the disk. A mirrored target with an ESP gets the enabled `booty-esp-sync` service (`/usr/local/libexec/booty-esp-sync`, `/etc/systemd/system/booty-esp-sync.service`, `/etc/booty/esp-sync.conf`), which copies changes of `/boot/efi` to the ESP on the other disk; turn it off with `systemctl disable booty-esp-sync.service` in the installed system, see [ESP Synchronization](disk.md#esp-synchronization). See the README table for the full rules. |
| `NVME_NAMESPACES` | (unset) | JSON array | NVMe namespaces to create before disk detection, for example `[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]`. |

[Disk](disk.md) describes disk selection, wipe rules and the layout format.
The config fields `provision.disk.raid` (software RAID), `deprovision.device` and `deprovision.secureErase` have no vars key, so the runtime can not set them. `DISK_MIRRORING_MODE` mirrors the root disk instead.

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
| `FRR_CONFIG_STRICT` | `true` | boolean | `true`: a provisioner `frr.conf` statement that BOOTy does not implement fails the network setup. `false`: BOOTy logs a warning with the line and skips the statement. Statements without effect on routing (`neighbor X description`, `bgp log-neighbor-changes`, `debug`, `log timestamp precision`) pass in both modes. Only `true`, `false`, `1`, `0` (and other Go booleans) are accepted. See [Networking](networking.md#strict-input-and-the-accepted-grammar). YAML `network.frrStrict`. |
| `NETPLAN_STRICT` | `true` | boolean | `true`: a provisioner netplan key that BOOTy does not implement fails the network setup. `false`: BOOTy logs a warning with the line and skips the key. A value of the wrong type fails in both modes; `optional` passes in both. YAML `network.netplanStrict`. |
| `NETWORK_MODE` | (unset) | `gobgp`, `frr`, `static`, `dhcp` | Validation accepts all four values. Only `gobgp` changes the stack selection. |
| `dns_resolver` | (unset) | IP addresses separated by commas | DNS servers for the ramdisk and the target OS. With `PROVISIONER_FILES_TARGET=ramdisk`, a provisioner `/etc/resolv.conf` stays in the ramdisk and this key is ignored for the ramdisk. With `WRITE_RESOLV_CONF=false`, it is ignored for the target OS. |
| `STATIC_IP` | (unset) | CIDR, for example `10.0.0.5/24` | Static address. Selects the static stack. |
| `STATIC_GATEWAY` | (unset) | IP address | Default gateway for the static stack. |
| `PREDICTABLE_NIC_NAMES` | `true` | boolean | When the netplan files, `frr.conf` or vars name an interface that does not exist, rename the link that has this predictable name (systemd net_id, naming scheme v255 of Ubuntu 24.04: `eno<index>`, `ens<slot>`, `enp<bus>s<slot>`, with `f<function>` and `n<phys_port_name>` or `d<dev_port>`, `P<domain>` for PCI domains above 0). The ramdisk has no udev and keeps `eth0`; the netplan and `frr.conf` of the installed OS use `enp1s0` or `ens1f0np0`. Configs that name existing links are not changed. SR-IOV VFs are not renamed. YAML `network.predictableNames`. |
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
| `vpn_rt` | (unset) | `ASN:value` | Explicit EVPN route target. The `frr.conf` route targets (and FRR's auto route target of an `frr.conf` L3VNI) win over it, as in FRR; it applies only without them and without `EVPN_RT_IMPORT`/`EVPN_RT_EXPORT`. FRR also reads it with `dcgw_ips`. |
| `EVPN_RT_IMPORT` | (unset) | `ASN:value` list, comma-separated | EVPN import route targets, like FRR `route-target import`: a Type-5 route with any of them is imported. Wins over the `frr.conf` value and `vpn_rt`. Without it, the `frr.conf` L3VNI route targets apply, else FRR's auto route target `(AS & 0xFFFF):VNI` for an `frr.conf` L3VNI, else `vpn_rt`, else `asn_server:provision_vni`. YAML `network.evpn.importRouteTargets`. |
| `EVPN_RT_EXPORT` | (unset) | `ASN:value` list, comma-separated | EVPN export route targets of the routes BOOTy originates, like FRR `route-target export`. Same precedence as `EVPN_RT_IMPORT`. YAML `network.evpn.exportRouteTargets`. |
| `EVPN_L2_ENABLED` | `false` | boolean | Originate and handle Type-2 and Type-3 routes. Excludes `EVPN_TYPE5_ONLY`. |
| `EVPN_TYPE5_ONLY` | (unset) | boolean | Direct Type-5 routing without an L2 gateway. Excludes `EVPN_L2_ENABLED`. Unset means `true` when the provisioner `frr.conf` maps the provision VNI to a VRF (L3VNI), like FRR, and `provision_gateway` is not set (with a gateway the mode and its BUM FDB entry stay as before), else `false`. An explicit value, or `EVPN_L2_ENABLED=true`, wins. YAML `network.evpn.type5Only`. |
| `EVPN_TYPE5_NETWORKS` | (unset) | CIDRs separated by commas | Prefixes that BOOTy originates as Type-5 routes, like FRR `network` lines with `advertise ipv4\|ipv6 unicast`. Only a prefix of a `LOOPBACK_ADDRESSES` entry, of `provision_ip` or of a `BLACKHOLE_ROUTES` entry is advertised (FRR import check). A set value replaces the provisioner `frr.conf` value. YAML `network.evpn.type5Networks`. |
| `LOOPBACK_ADDRESSES` | (unset) | CIDRs separated by commas | Addresses that GoBGP adds to `lo`, like netplan `lo: addresses`. They replace the overlay loopback address. A set value replaces the netplan value. YAML `network.loopbackAddresses`. |
| `BLACKHOLE_ROUTES` | (unset) | CIDRs separated by commas | Blackhole routes in the main table (protocol 196, metric 20, like zebra), like FRR `ip route P blackhole`. A received Type-5 route for such a prefix is not installed. A set value replaces the `frr.conf` value. YAML `network.blackholeRoutes`. |
| `BRIDGE_NAME` | `br.provision` | interface name | Provision bridge name (GoBGP). A set value wins over the netplan bridge name. YAML `network.evpn.bridgeName`. |
| `VXLAN_NAME` | `vx<VNI>` | interface name | Provision VXLAN device name (GoBGP). A set value wins over the netplan tunnel name. YAML `network.evpn.vxlanName`. |
| `BRIDGE_MAC` | (unset) | MAC address | Provision bridge MAC, the EVPN router MAC (GoBGP). Unset derives a MAC per host. A set value wins over netplan. YAML `network.evpn.bridgeMAC`. |
| `VXLAN_LINK` | (unset) | interface name | Device the VXLAN device sends from, like netplan tunnel `link` (GoBGP). Unset uses the underlay dummy with `VRF_NAME`. A set value wins over netplan. YAML `network.evpn.vxlanLink`. |
| `UNDERLAY_DUMMY_NAME` | `dummy.underlay` | interface name | Underlay dummy device that holds `underlay_ip` (GoBGP). A set value wins over netplan. YAML `network.evpn.underlayDummyName`. |
| `MTU` | `9000` | 626 to 65535 | MTU of the fabric NICs and the underlay dummy (GoBGP). The VXLAN MTU is 50 less. A set value wins; unset takes the largest netplan `mtu`. YAML `network.mtu`. |
| `BGP_SESSIONS` | (unset) | JSON list | GoBGP sessions in the native form of the `frr.conf` neighbors (YAML `network.bgp.sessions`): `name`, `interface`, `vrf`, `peerGroup`, `remoteAS` (number, `internal`, `external`), `localAS` with `localASMode` (`no-prepend-replace-as`, the only mode), `keepalive`, `hold`, `connectRetry`, `families` (`ipv4-unicast`, `ipv6-unicast`, `l2vpn-evpn`), `allowASIn` (1-10, `allowas-in N` in each family), `routeMaps` (`[{"family":"ipv4-unicast","in":"M","out":"N"}]`, like `neighbor X route-map M in|out`), `bfd`, `bfdProfile` (see `BFD_PROFILES`). Resolved with the `frr.conf` rules: unset fields come from the peer-group, then from `BGP_DEFAULTS`. A non-empty list replaces the `frr.conf` sessions as a whole. Validation rejects a missing remote AS, other local-as modes, local-as equal to the router or remote AS, a member remote AS that differs from its group's, an `allowASIn` above 10, timers 0, hold below 3 and a session without a family. See [Networking](networking.md#bgp-sessions). |
| `BGP_PEER_GROUPS` | (unset) | JSON list | Peer-groups of `BGP_SESSIONS`, same fields without `interface` and `peerGroup` (YAML `network.bgp.peerGroups`). Requires `BGP_SESSIONS`. |
| `BGP_DEFAULTS` | `datacenter` | `traditional`, `datacenter` | Timer profile of `BGP_SESSIONS`, like `frr defaults`: traditional 60/180/30 s, datacenter 3/9/10 s (keepalive, hold, connect retry). Requires `BGP_SESSIONS`. YAML `network.bgp.defaults`. |
| `BGP_FAST_EXTERNAL_FAILOVER` | (unset) | boolean | GoBGP mode stops an unnumbered session as soon as its NIC loses carrier and starts it again with the carrier, like `bgp fast-external-failover`. Unset keeps the `frr.conf` value (on unless `no bgp fast-external-failover`) and is off for the `BGP_PEER_MODE` peers; set, it applies to every session and to those peers. Only booleans parse. YAML `network.bgp.fastExternalFailover`. |
| `BGP_PEER_MODE` | `unnumbered` | `unnumbered`, `dual`, `numbered` | Peering mode. With BGP sessions (from `frr.conf` or `BGP_SESSIONS`) GoBGP mode ignores it and logs a warning. |
| `BGP_INTERFACES` | all physical NICs | interface names separated by commas | Interfaces for unnumbered and dual peers. With BGP sessions (from `frr.conf` or `BGP_SESSIONS`) GoBGP mode ignores it and logs a warning. |
| `BGP_NEIGHBORS` | (unset) | IP addresses separated by commas | Numbered peers. Required for `dual` and `numbered`. With BGP sessions (from `frr.conf` or `BGP_SESSIONS`) GoBGP mode ignores it and logs a warning. |
| `BGP_REMOTE_ASN` | `0` | uint32 | Remote ASN of numbered peers. `0` means iBGP. Alias: `bgp_remote_asn`. With BGP sessions (from `frr.conf` or `BGP_SESSIONS`) GoBGP mode ignores it and logs a warning. |
| `BGP_UNDERLAY_AF` | `ipv4` | `ipv4` | Underlay address family. Validation rejects other values. |
| `BGP_OVERLAY_TYPE` | `evpn-vxlan` | `evpn-vxlan`, `none` | Overlay type. Validation also accepts `l3vpn`, but the GoBGP setup rejects it as not implemented. |
| `BGP_MIN_PEERS` | `1` | integer | Number of established peers that make the underlay ready. `0` means 1. |
| `BGP_KEEPALIVE` | `0` | uint32, seconds | Keepalive timer. `0` gives 3 s in GoBGP. FRR uses it only together with `BGP_HOLD`. Alias: `bgp_keepalive`. With BGP sessions (from `frr.conf` or `BGP_SESSIONS`) GoBGP mode ignores it and logs a warning. |
| `BGP_HOLD` | `0` | uint32, seconds | Hold timer. `0` gives 9 s in GoBGP. FRR uses it only together with `BGP_KEEPALIVE`. Alias: `bgp_hold`. With BGP sessions (from `frr.conf` or `BGP_SESSIONS`) GoBGP mode ignores it and logs a warning. |
| `BGP_AUTH_PASSWORD` | (unset) | string without white space | TCP-MD5 password for all peers. FRR also reads it. |
| `BGP_CONNECT_RETRY` | `0` | uint32, seconds | GoBGP connect retry timer. `0` gives 5 s. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the sessions without `timers connect` (an explicit session or peer-group value wins). YAML `network.bgp.connectRetry`. |
| `BGP_LOCAL_AS` | `0` | uint32 | Local AS that GoBGP sends to unnumbered and other eBGP peers, like FRR `local-as N no-prepend replace-as`. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the eBGP sessions without `local-as` (an explicit session or peer-group value wins). `local_asn` is the FRR template value and does not apply to GoBGP. YAML `network.bgp.localAS`. |
| `BGP_NEIGHBOR_VRF` | (unset) | VRF name | GoBGP binds the numbered peers to this VRF device, like FRR `router bgp N vrf NAME`. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the numbered sessions in the default VRF (a `router bgp N vrf NAME` or session `vrf` wins). YAML `network.bgp.neighborVRF`. |
| `BGP_ALLOWAS_IN` | (unset) | 0 to 10 | How often the own ASN may be in a received AS path, like FRR `allowas-in N`. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the sessions without `allowas-in` (an explicit session or peer-group value wins). Unset or `0` leaves them without allowas-in. YAML `network.bgp.allowASIn`. |
| `BGP_MULTIPATH_RELAX` | (unset) | boolean | Paths with different AS paths of the same length are equal cost, like FRR `bgp bestpath as-path multipath-relax`. Unset keeps the `frr.conf` value, or off without `frr.conf`. YAML `network.bgp.multipathRelax`. |
| `BGP_INSTALL_ROUTES` | (unset) | boolean | GoBGP installs the learned IPv4 and IPv6 unicast routes into the kernel, like zebra (ECMP, link-local next hops). Then BOOTy installs no own route to a Type-5 VTEP. Unset means `true` with any BGP sessions (from `frr.conf` or `BGP_SESSIONS`) and `false` for the `BGP_PEER_MODE` peers. A BOOTy setting: zebra has no such switch and always installs. YAML `network.bgp.installRoutes`. |
| `BGP_ROUTE_MAPS` | (unset) | JSON list of route-maps | Route-maps like FRR, with the subset BOOTy implements: `[{"name":"TAG-IN","entries":[{"seq":10,"setLocalPreference":90,"setCommunities":["65169:200"],"setCommunityAdditive":true}]}]`. Entry fields: `seq`, `deny`, `matchCommunity` (a community-list), `setLocalPreference`, `setCommunities`, `setCommunityAdditive`. Entries run in `seq` order; a matching permit entry applies its sets and stops, a matching deny entry rejects, and no match rejects (implicit deny). A non-empty list replaces the `frr.conf` route-maps as a whole. Unknown fields fail. YAML `network.bgp.routeMaps`. |
| `BGP_COMMUNITY_LISTS` | (unset) | JSON list of community-lists | Standard community-lists for `matchCommunity`: `[{"name":"tagged","entries":[{"communities":["65169:200"]}]}]`. An entry matches a route with all its communities. Deny entries are not implemented. A non-empty list replaces the `frr.conf` community-lists as a whole. YAML `network.bgp.communityLists`. |
| `BGP_ROUTE_MAP_IN` | (unset) | route-map name | Import route-map of every peer in every address family, like `neighbor X route-map M in`. Also replaces the `frr.conf` import route-maps. YAML `network.bgp.routeMapIn`. |
| `BGP_ROUTE_MAP_OUT` | (unset) | route-map name | Export route-map of every peer in every address family. Also replaces the `frr.conf` export route-maps. YAML `network.bgp.routeMapOut`. |
| `BGP_EBGP_REQUIRES_POLICY` | (unset) | boolean | Like FRR `bgp ebgp-requires-policy`: an eBGP peer without a route-map in a family and direction exchanges no routes there, and BOOTy logs each such peer, family and direction at info level. Unset means on for the `frr.conf` sessions (unless the router has `no bgp ebgp-requires-policy`, as the bm4x templates do) and for `BGP_SESSIONS`, and off for the `BGP_PEER_MODE` peers. A set value applies to all of them. YAML `network.bgp.ebgpRequiresPolicy`. |
| `VRF_NAME` | (unset) | VRF name | Underlay VRF. Empty means no VRF. FRR renders `router bgp ASN vrf NAME`. Alias: `vrf_name`. |
| `VRF_TABLE_ID` | `1000` | uint32 | Routing table of `VRF_NAME`. `0` and `1` become `1000`. Alias: `vrf_table_id`. |
| `OVERLAY_VRF_NAME` | (unset) | VRF name or `default` | VRF of the provision bridge (GoBGP). `default` keeps the bridge in the default VRF, like the bm4x netplan. Unset means `VRF_NAME`. A set value wins over netplan. YAML `network.vrf.overlayName`. |
| `OVERLAY_VRF_TABLE_ID` | `0` | uint32 | Routing table of `OVERLAY_VRF_NAME`. `0` means `VRF_TABLE_ID`. A set value wins over netplan. YAML `network.vrf.overlayTableID`. |
| `BFD_TRANSMIT_MS` | `0` | 10 to 60000, ms | BFD transmit interval. Set it together with `BFD_RECEIVE_MS`. GoBGP: BFD on every native peer (single-hop, but multihop for an iBGP peer that is not on a connected network of its VRF, like FRR), and an explicit value replaces the interval of `frr.conf` BFD sessions. FRR also reads it: BFD on the fabric peers. YAML `network.bgp.bfdTransmitMS`. Alias: `bfd_transmit_ms`. |
| `BFD_RECEIVE_MS` | `0` | 10 to 60000, ms | BFD receive interval. Set it together with `BFD_TRANSMIT_MS`. FRR also reads it. YAML `network.bgp.bfdReceiveMS`. Alias: `bfd_receive_ms`. |
| `BFD_PROFILES` | (unset) | JSON list | BFD profiles of the `bfdProfile` of `BGP_SESSIONS` and of the `frr.conf` neighbors, like `bfd` / `profile NAME`: `name`, `transmitInterval` and `receiveInterval` (ms, 10 to 60000), `detectMultiplier` (2 to 255). Unset values and an unknown profile use the FRR defaults 300 ms, 300 ms, 3. A non-empty list replaces the `frr.conf` profiles as a whole. YAML `network.bgp.bfdProfiles`. |
| `BFD_DETECT_MULTIPLIER` | `0` (3) | 2 to 255 | BFD detect multiplier (GoBGP). An explicit value replaces the multiplier of `frr.conf` BFD sessions. YAML `network.bgp.bfdDetectMultiplier`. |

The lowercase aliases `bgp_keepalive`, `bgp_hold`, `bgp_remote_asn`,
`vrf_name` and `vrf_table_id` set the same values as the uppercase keys.

## FRR

The FRR stack runs when `asn_server` and `underlay_subnet` or `underlay_ip`
are set and `NETWORK_MODE` is not `gobgp`. It also reads the GoBGP keys marked
"FRR also reads it". When FRR setup fails, BOOTy falls back to DHCP. Only the
default and `iso` build flavors contain the FRR daemons.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
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
| `FIRMWARE_REPORT` | `false` | boolean | Collect BIOS, BMC, NIC and storage controller firmware versions. `NIC_FIRMWARE_LOG=true` logs the NIC firmware versions also without it. |
| `NIC_FIRMWARE_LOG` | `false` | boolean | Log a `nic_firmware` line with the firmware version of every NIC on each provision, like `provision.sh`. Does not need `FIRMWARE_REPORT`. YAML `provision.firmware.logNICs`. Preset: `true`. |
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

[Rescue modes](caprf-integration.md#rescue-modes) describes what each mode
does.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `RESCUE_MODE` | `reboot`, `wait` with `FAILURE_ACTION=wait` | `reboot`, `retry`, `shell`, `wait` | Action after a failed provisioning, also for a failed standby `provision` command. |
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
| `BOOTY_ALLOW_REMOVABLE` | unset | exactly `true` | Accept removable disks in the default disk selection. `DISK_SELECTION=lsblk` ignores it. |
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
- Planned (#622): `NETWORK_WAIT_MAX_ATTEMPTS`.
