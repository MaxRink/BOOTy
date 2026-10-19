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
| `NUM_VFS` | `0` | integer | Number of SR-IOV VFs to set with `mstconfig` on each Mellanox NIC. `0` skips the step. An SR-IOV profile that sets `sriovEnabled` or `maxVFs` for ConnectX ports replaces it (see [SR-IOV](sriov.md#setup-mellanox-and-the-profiles)). A positive value fails the run when no Mellanox NIC or `mstconfig` device is found. With `NUM_VFS_MODE=raise`, `0` means 32 and missing NICs or `mstconfig` are skipped. |
| `NUM_VFS_MODE` | `set` | `set`, `raise` | How `setup-mellanox` applies `NUM_VFS`. `set`: only with `NUM_VFS` > 0, sets `SRIOV_EN` and `NUM_OF_VFS`, fails without a Mellanox NIC. `raise`: on every provision, raises `NUM_OF_VFS` to `NUM_VFS` (32 when `0`) with `mstconfig` on each ConnectX-4/5/6/6 Dx/6 Lx/7 Ethernet function, like `setup_mellanox_num_of_vfs`; skips quietly without `mstconfig` or such a NIC; a failed `mstconfig` query or set fails the run. A change forces a reboot instead of kexec. YAML `provision.firmware.numVFsMode`. Preset: `raise`. |
| `SRIOV_PROFILES` | (unset) | JSON list | Desired SR-IOV state per NIC kind (YAML `provision.sriov.profiles`). Each profile has `name`, `match` (`vendorId`, `deviceId`, `driver`, `mac`, `interface` name or pattern; every set field must match), `firmware` (`sriovEnabled`, `maxVFs`, `linkType` `eth`/`ib`, `fwLLDPAgent`, `nvConfig`, `devlinkParams`), `runtime` (`numVFs`, `eswitchMode`, `vfDefaults`, `vfs`, `handoff`), `onDrift` and `optional`. The `configure-sriov` step checks them; a PF port that matches two profiles fails the run. Unknown fields and out-of-range values fail. See [SR-IOV](sriov.md#profiles). |
| `SRIOV_ON_DRIFT` | `warn` | `warn`, `fail`, `apply` | Default drift action of the profiles (YAML `provision.sriov.onDrift`). `warn` logs, `fail` stops the run, `apply` stores the firmware settings and reboots instead of kexec. |
| `NIC_TOOLS` | `opensource` | `kernel`, `opensource`, `vendor` | Tool classes for firmware settings (YAML `provision.sriov.nicTools`): `kernel` only devlink and sysfs, `opensource` also `mstconfig` and `ethtool` of the ramdisk, `vendor` also proprietary tools of a site flavor. |
| `SRIOV_VERIFY_REBOOT` | `on` | `on`, `off` | Verify reboot after stored SR-IOV firmware changes that the runtime settings depend on (YAML `provision.sriov.verifyReboot`). `on` boots the ramdisk again to check them and needs `REBOOT_URL` (CAPRF support); without it the run fails with `sriov-verify-reboot-unsupported`. `off` goes on to the handoff without that check. See [SR-IOV](sriov.md#verify-reboot). |
| `SRIOV_UPLINK_VF_DRIVERS` | `mlx5_core,ice` | driver names separated by commas | Drivers that may change the VF count of a port the ramdisk network uses (YAML `provision.sriov.uplinkVFDrivers`); it replaces the list. igb, ixgbe and i40e reset the PF for a VF count change and are refused by default. See [SR-IOV](sriov.md#runtime-settings). |

## CAPRF endpoints, authentication and transport

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `INIT_URL` | (unset) | URL | Receives the `init` status. BOOTy also uses it as the network connectivity target. |
| `SUCCESS_URL` | (unset) | URL | Receives the success status. Connectivity target when `INIT_URL` is unset. |
| `ERROR_URL` | (unset) | URL | Receives the error status. |
| `REBOOT_URL` | (unset) | URL | Receives the `reboot-required` status before BOOTy reboots into the ramdisk again to finish the task, for example `reboot-required: sriov-firmware ...` after SR-IOV firmware settings that need a reset. CAPRF must keep or insert again the boot media and not count it as a failure. See [SR-IOV](sriov.md#verify-reboot). YAML `transport.rebootURL`. |
| `LOG_URL` | (unset) | URL | BOOTy ships each log line of level info and higher to this URL, in order. Up to 8192 lines and 4 MiB wait in RAM; when that is full, the oldest `DEBUG`, then `INFO` lines are dropped, and `WARN` and `ERROR` lines stay while lower lines are left. A line that fails all retries is dropped too. The next line that arrives is followed by one `[WARN] remote log handler dropped lines code=BOOTY-LOG-001 debug=N info=N warn=N error=N` line. Tokens, passwords, JWTs, PEM private keys and LUKS keys are redacted before a line is queued. |
| `LOG_FLUSH_TIMEOUT_SEC` | `5` | integer 0-60, seconds | Before kexec, reboot, power off or the rescue shell, BOOTy stops queuing log lines and waits up to this long for the queued `LOG_URL` lines to arrive, then cancels the request in flight and logs the number of lines left on the console. `0` sends nothing more. Empty means the default. YAML `transport.logFlushTimeoutSec`. |
| `HEARTBEAT_URL` | (unset) | URL | Standby mode: heartbeat endpoint, every 30 s. Required for `MODE=standby`. |
| `COMMANDS_URL` | (unset) | URL | Standby mode: command poll endpoint, every 10 s. BOOTy posts command ACKs to `<COMMANDS_URL>/ack`. Required for `MODE=standby`. See [Agent mode contract](caprf-integration.md#agent-mode-contract). |
| `STANDBY_DEPROVISION_SUCCESS_ACTION` | (unset) | `standby` | Standby mode: `standby` returns to the standby loop after a successful `deprovision` command, in the same boot, without a reboot or power off. Unset ends the run like `SUCCESS_ACTION`. YAML `agent.deprovisionSuccessAction`. `CAPRF_LEGACY_COMPAT` does not preset it. |
| `TOKEN` | (unset) | string | Bearer token. With `TOKEN_URL` it is the bootstrap token for the JWT exchange. |
| `TOKEN_URL` | (unset) | HTTPS URL | JWT endpoint. BOOTy exchanges `TOKEN` for a JWT after the network is up and renews it in the background. Plain HTTP is accepted only for `localhost`, `127.0.0.1` and `::1`. Requires `TOKEN` and `HOSTNAME`. A failed exchange or an exhausted renewal ends the run. |
| `TOKEN_ALGORITHM` | (unset) | `RS256`, `ES256` | JWT signing algorithm that BOOTy requests from `TOKEN_URL`. Empty leaves the choice to the server. |
| `TOKEN_EXPIRES_IN` | (unset) | integer, seconds | Lifetime of `TOKEN`, counted from the start of BOOTy, when there is no `TOKEN_URL` to renew it. It does not depend on the ramdisk clock. BOOTy starts only after the virtual media insert and the boot, so the count runs late by that time: CAPRF subtracts a boot allowance from the value it renders. Wins over `TOKEN_EXPIRES_AT`. BOOTy stops the active task `TOKEN_RENEW_MARGIN_SECONDS` before the expiry, reports the reason to `ERROR_URL` and ends like a failed run (`FAILURE_ACTION`). Ignored with `TOKEN_URL`: the `expires_in` of the token exchange at start is authoritative. YAML `transport.tokenExpiresInSeconds`. |
| `TOKEN_EXPIRES_AT` | (unset) | RFC 3339 time | Expiry of `TOKEN` when there is no `TOKEN_URL`, like `TOKEN_EXPIRES_IN`. It needs a correct clock: BOOTy checks it after the [time sync](#time-sync), and with `TIME_SYNC=off` or without a time source it uses the RTC. Prefer `TOKEN_EXPIRES_IN`. Unset (both): the expiry is unknown and BOOTy does not stop. Ignored with `TOKEN_URL`. YAML `transport.tokenExpiresAt`. |
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

## Time sync

A wrong RTC (a board swap, a dead CMOS battery, a BIOS reset) makes every
TLS certificate look not yet valid or expired. BOOTy therefore sets the
clock after the network connectivity check and before the first request
that checks a certificate or sends data: the JWT exchange, the CAPRF
callbacks, the crash artifact upload and the image download.

- The connectivity check counts a certificate that is not valid at the local
  clock as reachable: the TLS server answered.
- NTP: BOOTy queries `NTP_SERVERS` in order, then, with `TIME_DHCP_NTP`,
  the NTP servers of the DHCP leases (option 42), with SNTP (RFC 4330, 2 s per server). The first good
  answer counts. CAPRF renders `NTP_SERVERS` into the vars file like any
  other key.
- CAPRF `Date` header: BOOTy sends one `HEAD` request without credentials to
  the connectivity target (`INIT_URL`, else `SUCCESS_URL`), with the normal
  certificate check against the system roots. When the local clock is
  outside the validity period of the server certificate, it sends the
  request again with the check at a time inside that period. In both cases
  it then checks the chain, the host name and the key usage again at the
  time of the `Date` header. The time is then authenticated: the server
  holds the key of a certificate that is valid at the time it claims. With
  `SKIP_TLS_VERIFY` or an `http` URL the time is not authenticated.
- Choice: NTP is the most precise. When the authenticated `Date` time also
  answers and differs from NTP by more than `TIME_MAX_SKEW_SEC`, BOOTy logs an
  error and uses the `Date` time, because plain NTP is not authenticated.
  Without NTP, the `Date` time is used.
- Plausible window: BOOTy never sets the clock to a time before the floor or
  more than five years after it. The floor is the committer time of the
  built commit, which the build stamps into the binary
  (`-X github.com/telekom/BOOTy/pkg/buildinfo.commitTime=$(git log -1 --format=%cI)`;
  the Makefile, `initrd.Dockerfile` (build argument `BOOTY_COMMIT_TIME`) and
  the CI, nightly and release workflows set it). A binary without the stamp
  uses the Go VCS stamp (`vcs.time`, only in `go build` from a git checkout;
  the Docker build has no `.git`), and the floor is never earlier than
  2026-01-01. A local `go build` without the stamp therefore falls back to
  2026-01-01.
- When the clock is already plausible (inside that window), BOOTy does not
  set it back by more than `TIME_MAX_SKEW_SEC` on the `Date` header alone; it
  logs an error and keeps the clock (`TIME_SYNC=require` fails). NTP may set
  it back.
- What protects what: the backward-step refusal only guards against a stale
  `Date` header (a caching proxy, a server with a wrong clock). It is not a
  defense against an on-path attacker, who can also answer plain NTP. The
  security boundary against an attacker who wants to move the clock back
  (to make an expired or revoked certificate valid again) is the floor: no
  source can set the clock before the commit time of the build. Where the
  RTC is trusted (a working CMOS battery, the RTC written back by the last
  run), use `TIME_SYNC=require` with a short `TIME_MAX_SKEW_SEC`, for example
  `60`: NTP must then agree with the authenticated `Date` time within that
  bound, and the run fails instead of setting a plausible clock back by more
  than that on the `Date` header.
- BOOTy steps the clock when the skew is larger than the resolution of the
  source (0.5 s for NTP, 2 s for `Date`), logs the skew (`skew_seconds`) and
  writes the RTC in UTC (`TIME_WRITE_RTC`), so the installed OS starts with
  the right time.
- Without a source, BOOTy keeps the RTC time. A clock before the build of
  BOOTy is logged as an error (`BOOTY-TIM-001`). `TIME_SYNC=require` fails the
  run instead.
- A certificate that is still not valid at the local clock gives an error
  that names the skew and the time source (`BOOTY-TIM-002`), for the CAPRF
  callbacks and the image download, instead of the bare x509 error.
  Both codes are in the [failure code table](failure-codes.md): a run that
  fails on them reports the code to `ERROR_URL` and in the step data.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `TIME_SYNC` | (unset) (`auto`) | `off`, `auto`, `require` | `off` keeps the RTC time. `auto` syncs from the sources that answer and logs failures. `require` fails the run (`BOOTY-TIM-001`) when no source gives a usable time, or when the clock can not be set. Dry-run mode does not sync. YAML `time.sync`. |
| `NTP_SERVERS` | (unset) | up to 8 hosts or `host:port`, comma or space separated | SNTP servers, queried before the DHCP option 42 servers (with `TIME_DHCP_NTP`). Default port 123. An IPv6 address with a port needs brackets (`[2001:db8::123]:123`). URLs are rejected. YAML `time.ntpServers` (list). |
| `TIME_MAX_SKEW_SEC` | `0` (300) | integer, seconds, 1-86400 | Largest difference between the NTP time and the authenticated CAPRF `Date` time; above it BOOTy uses the `Date` time. Also the largest backward step of a plausible clock on the `Date` header alone. YAML `time.maxSkewSeconds`. |
| `TIME_DHCP_NTP` | (unset) (`false`) | boolean (`true`, `false`, `1`, `0`, `yes`, `no`) | Also query the NTP servers of the DHCP leases (option 42), after `NTP_SERVERS`. Off by default like the legacy ramdisk, which has no NTP client. YAML `time.dhcpNTP`. |
| `TIME_WRITE_RTC` | (unset) (`true`) | boolean (`true`, `false`, `1`, `0`, `yes`, `no`) | Write the synced time to the RTC in UTC after the clock was stepped. A failed write is logged. Other values are rejected. YAML `time.writeRTC`. |

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
| `SECURE_ERASE` | `false` | boolean | Provisioning only: erase the target disk (and RAID member disks) in hardware instead of a signature wipe, with the method for the media type: `nvme format --ses=1` for NVMe, ATA Security Erase with `hdparm` for ATA disks, SCSI `SANITIZE` (`sg_sanitize`) or `FORMAT UNIT` (`sg_format`) for SAS disks. Deprovisioning, decommission and `WIPE_ALL_DISKS=true` ignore it. A failed erase, a security-frozen drive, a disk without an erase method and a missing erase tool fail the run, unless `SECURE_ERASE_FALLBACK=wipe`. See [Disk handling](disk.md#secure-erase). |
| `SECURE_ERASE_FALLBACK` | `fail` | `fail`, `wipe` | What happens when a requested secure erase fails or the disk does not support it. `fail`: the run fails. `wipe`: BOOTy logs a warning, wipes the disk like without `SECURE_ERASE` (`WIPE_DEPTH`) and names the disk in the success status (`secure erase not done, wiped instead`). A wipe is not an erase: the data stays on the media. Also applies to the YAML `deprovision.secureErase`. YAML `provision.disk.secureEraseFallback`. Not preset: `CAPRF_LEGACY_COMPAT` sets `WIPE_ALL_DISKS=true`, which does not secure erase. |
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
| `NETWORK_MODE` | (unset) | `gobgp`, `frr`, `static`, `dhcp`, `links` | Validation accepts these values. `gobgp` and `links` change the stack selection. `links` sets every physical NIC up with IPv6 link-local only: no DHCP, no address, no route, no BGP, no connectivity wait. See [networking](networking.md#links-mode). |
| `dns_resolver` | (unset) | IP addresses separated by commas | DNS servers for the ramdisk and the target OS. With `PROVISIONER_FILES_TARGET=ramdisk`, a provisioner `/etc/resolv.conf` stays in the ramdisk and this key is ignored for the ramdisk. With `WRITE_RESOLV_CONF=false`, it is ignored for the target OS. |
| `STATIC_IP` | (unset) | CIDR, for example `10.0.0.5/24` | Static address. Selects the static stack. |
| `STATIC_GATEWAY` | (unset) | IP address | Default gateway for the static stack. |
| `PREDICTABLE_NIC_NAMES` | `true` | boolean | When the netplan files, `frr.conf` or vars name an interface that does not exist, rename the link that has this predictable name (systemd net_id, naming scheme v255 of Ubuntu 24.04: `eno<index>`, `ens<slot>`, `enp<bus>s<slot>`, with `f<function>` and `n<phys_port_name>` or `d<dev_port>`, `P<domain>` for PCI domains above 0). The ramdisk has no udev and keeps `eth0`; the netplan and `frr.conf` of the installed OS use `enp1s0` or `ens1f0np0`. Configs that name existing links are not changed. With provisioner netplan files (the input of the legacy ramdisk), every other physical NIC gets its predictable name too, like udev in the legacy ramdisk (the name is the LLDP port ID); a NIC that the config names by its kernel name keeps it. These extra renames are best effort: a NIC whose rename fails, whose name exists or is taken by a configured rename, or that shares its name with another NIC keeps its kernel name (logged); only a failed rename to a configured name fails the setup. A NIC that a netplan `set-name` renames, or whose udev name a `set-name` sets, is left to the `set-name`. A NIC that appears after the network setup keeps its kernel name (`ethN`), so a netplan `en*` match misses it, unlike udev in the legacy ramdisk. SR-IOV VFs are not renamed. YAML `network.predictableNames`. |
| `NIC_WAIT_SECONDS` | `30` | seconds, -1 or 0-3600 | Longest wait for the NICs that the netplan files, `frr.conf` or vars name and that do not exist yet, before the predictable name renames: a driver can bring up its ports after BOOTy starts, and containerlab attaches the lab veths after the container starts. A NIC that has the name as its predictable name ends the wait for it. Names that BOOTy creates or gives itself are not waited for: the VXLAN link, `bond0` and the netplan `set-name` targets. After the wait the setup goes on with the NICs it has and logs the missing ones. `0` means 30, `-1` does not wait. YAML `network.nicWaitSeconds`. |
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
| `EVPN_RD` | (unset) | `ASN:NN` or `IPv4:NN` | Route distinguisher of BOOTy's EVPN routes, like FRR `rd` in the VRF's `address-family l2vpn evpn`. Wins over the `frr.conf` value. Unset uses the `frr.conf` value, else FRR's automatic `<router-id>:<n>` (see [Networking](networking.md#routes-booty-originates)). Unlike FRR, it also applies to the L2VNI Type-2 and Type-3 routes. A value out of range (IPv4 or a 4-byte ASN with an NN above 65535, a 2-byte ASN with an NN above 4294967295) fails validation. GoBGP mode only; FRR mode ignores it and uses the `rd` of `frr.conf`. YAML `network.evpn.rd`. |
| `EVPN_RT_IMPORT` | (unset) | `ASN:value` list, comma-separated | EVPN import route targets, like FRR `route-target import`: a Type-5 route with any of them is imported. Wins over the `frr.conf` value and `vpn_rt`. Without it, the `frr.conf` L3VNI route targets apply, else FRR's auto route target `(AS & 0xFFFF):VNI` for an `frr.conf` L3VNI, else `vpn_rt`, else `asn_server:provision_vni`. YAML `network.evpn.importRouteTargets`. |
| `EVPN_RT_EXPORT` | (unset) | `ASN:value` list, comma-separated | EVPN export route targets of the routes BOOTy originates, like FRR `route-target export`. Same precedence as `EVPN_RT_IMPORT`. YAML `network.evpn.exportRouteTargets`. |
| `EVPN_L2_ENABLED` | `false` | boolean | Originate and handle Type-2 and Type-3 routes. Excludes `EVPN_TYPE5_ONLY`. |
| `EVPN_RESOLVE_OVERLAY_INDEX` | `false` | boolean | Like FRR `enable-resolve-overlay-index` (also taken from `frr.conf`): install a received Type-5 route with a gateway IP overlay index while a Type-2 MAC/IP route carries the gateway IP (RFC 9136 section 3.2). Needs `EVPN_L2_ENABLED`. Off, such routes are not installed, as in FRR. YAML `network.evpn.resolveOverlayIndex`. |
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
| `VXLAN_MTU` | MTU - 50 | 576 to 65535 | Provision VXLAN device MTU, like netplan tunnel `mtu` (GoBGP). A set value wins over netplan. YAML `network.overlay.vxlanMTU`. |
| `VXLAN_MAC` | (derived) | MAC address | Provision VXLAN device MAC, like netplan tunnel `macaddress` (GoBGP). A set value wins over netplan. YAML `network.overlay.vxlanMAC`. |
| `VXLAN_LEARNING` | `false` | boolean | MAC learning of the VXLAN device, like netplan tunnel `mac-learning`. A set value wins over netplan. YAML `network.overlay.vxlanLearning`. |
| `VXLAN_ARP_PROXY` | `false` | boolean | ARP proxy of the VXLAN device, like netplan tunnel `arp-proxy`. A set value wins over netplan. YAML `network.overlay.vxlanARPProxy`. |
| `VXLAN_PORT_HAIRPIN` | kernel default (`false`) | boolean | Hairpin mode of the VXLAN device as bridge port, like netplan tunnel `hairpin`. A set value wins over netplan. YAML `network.overlay.portHairpin`. |
| `VXLAN_PORT_LEARNING` | kernel default (`true`) | boolean | MAC learning of the VXLAN device as bridge port, like netplan tunnel `port-mac-learning`. A set value wins over netplan. YAML `network.overlay.portLearning`. |
| `VXLAN_IPV6_LINK_LOCAL` | `true` | boolean | `false` removes the IPv6 link-local address of the VXLAN device (addrgenmode none), like netplan `link-local: []`. A set value wins over netplan. YAML `network.overlay.vxlanLinkLocal`. |
| `VXLAN_ACCEPT_RA` | kernel default | boolean | IPv6 router advertisements on the VXLAN device, like netplan `accept-ra`. A set value wins over netplan. YAML `network.overlay.vxlanAcceptRA`. |
| `BRIDGE_MTU` | kernel default | 576 to 65535 | Provision bridge MTU, like netplan bridge `mtu` (GoBGP). A set value wins over netplan. YAML `network.overlay.bridgeMTU`. |
| `BRIDGE_IPV6_LINK_LOCAL` | `true` | boolean | `false` removes the IPv6 link-local address of the provision bridge, like netplan `link-local: []`. A set value wins over netplan. YAML `network.overlay.bridgeLinkLocal`. |
| `BRIDGE_ACCEPT_RA` | kernel default | boolean | IPv6 router advertisements on the provision bridge, like netplan `accept-ra`. A set value wins over netplan. YAML `network.overlay.bridgeAcceptRA`. |
| `BRIDGE_STP` | kernel default (off) | boolean | Spanning tree of the provision bridge, like netplan bridge `parameters: stp` (GoBGP). Like netplan, a `parameters` block without `stp` turns it on. A set value wins over netplan. YAML `network.overlay.bridgeSTP`. |
| `ACCEPT_RA_OFF` | (unset) | interface names separated by commas | NICs that ignore IPv6 router advertisements, like netplan `accept-ra: false` (GoBGP). A set value wins over netplan. YAML `network.noAcceptRA`. |
| `LLDP_EMIT` | (unset) | interface names separated by commas | NICs that send LLDP frames like systemd-networkd with netplan `emit-lldp: true` (GoBGP): to the nearest-bridge address, chassis ID = the machine ID of the boot, locally assigned (subtype 7, 32 hex digits), like networkd; the ramdisk has no `/etc/machine-id`, so BOOTy creates a random one per boot in `/run/booty/machine-id` and never uses or writes `/etc/machine-id`; only when it cannot write it, BOOTy sends the NIC MAC (subtype 4), an extension that networkd does not have, port ID = interface name, TTL 120 s, system name = hostname, 4 frames 1 s apart, then every 30 s. A set value wins over netplan. YAML `network.emitLLDP`. |
| `LLDP_RECEIVE` | `true` | boolean | BOOTy listens for LLDP frames on every physical NIC, logs each new or changed neighbor (chassis ID, port ID and its subtype, port description, system name, management addresses, port VLAN, max frame size) and adds the current neighbors to the hardware inventory report (`lldpNeighbors`, see [networking](networking.md#lldp-receive); while none is known yet the report waits until the receive has run for 30 s). It ignores the frames that it sends itself. A neighbor expires after its TTL; one neighbor per NIC is kept. On Intel ice and i40e NICs the firmware LLDP agent may take the frames (see `LLDP_DISABLE_FW_AGENT`). The legacy ramdisk only sends LLDP. YAML `network.lldpReceive`. |
| `LLDP_PROFILE` | `networkd` | `networkd`, `bm4x-cablecheck` | The identity of the sent LLDP frames. `networkd` is the frame above. `bm4x-cablecheck` sends the identity that the bm4x connection check matches (`Connection.isToMacAddress` in bm4x-api): port ID = the NIC MAC (subtype 3), port description = the interface name, like lldpd, and system name `bare-minimal`; chassis ID, TTL, capabilities and timing stay as with `networkd`. It sends on every physical NIC with carrier unless `LLDP_EMIT_ALL=false`. See [networking](networking.md#lldp-identity-profiles). YAML `network.lldp.profile`. |
| `LLDP_PORT_ID_FORMAT` | `mac` | `mac`, `mac-string-colon`, `mac-string-dotted`, `ifname` | The port ID of `LLDP_PROFILE=bm4x-cablecheck` (only with that profile). `mac`: the NIC MAC as a MAC port ID (subtype 3); the switch decides how it shows it (lldpd and Junos `aa:bb:cc:dd:ee:ff`, NX-OS `aabb.ccdd.eeff`). `mac-string-colon`: the string `aa:bb:cc:dd:ee:ff` as a locally assigned port ID (subtype 7), which every switch shows unchanged; use it on NX-OS leaves. `mac-string-dotted`: the string `aabb.ccdd.eeff` (subtype 7). `ifname`: the interface name (subtype 5). bm4x matches only the colon form. See [networking](networking.md#port-id-format). YAML `network.lldp.portIDFormat`. |
| `LLDP_EMIT_ALL` | `true` with `LLDP_PROFILE=bm4x-cablecheck`, else `false` | boolean | Send LLDP on every physical NIC with carrier in every network mode, not only on the `LLDP_EMIT` NICs. NICs without carrier are skipped until the link comes up; then the four fast frames (one per second) start, also again after each carrier loss, like networkd. The GoBGP stack then sends nothing itself, so no NIC gets two senders. YAML `network.lldp.emitAll`. |
| `LLDP_ANNOUNCE` | `parity` | `parity`, `standard`, `full` | The TLVs that BOOTy sends besides the identity. `parity` is what systemd-networkd sends. `standard` adds a BOOTy system description, a port description with driver and PCI address, the management addresses, the 802.3 MAC/PHY status and the maximum frame size. `full` adds the host identity to the system description and the LLDP-MED inventory. See [networking](networking.md#lldp-announce-levels). YAML `network.lldp.announce`. |
| `LLDP_DISABLE_FW_AGENT` | `true` | boolean | Stop the firmware LLDP agent of Intel NICs, in every mode: it consumes the switch's LLDP frames, so the switch never sees the host. i40e (700 series) private flag `disable-fw-lldp` on, ice (800 series) `fw-lldp-agent` off, set over ethtool netlink (no `ethtool` binary) before the network setup, only when the agent runs. This also stops the firmware DCBX of the port. BOOTy does not put the flag back, and ice keeps it in the NVM, so the agent stays off for the installed OS too. `false` leaves the flag alone. Ports whose agent an SR-IOV profile sets (`firmware.fwLLDPAgent`) are left to that profile. A deliberate deviation from the legacy ramdisk, which never touches the flag. Other drivers are not changed. A failure is logged and the run goes on. See [networking](networking.md#bgp-capabilities-and-lldp). YAML `network.lldp.disableFirmwareAgent`. |
| `NETWORK_DEVICES` | (unset) | JSON object | The native form of the provisioner netplan: the keys of its `network:` section (`ethernets` with `match`, `set-name`, `addresses`, `mtu`, `accept-ra`, `link-local`, `emit-lldp`, `routes`, `nameservers`; `vrfs`, `dummy-devices`, `tunnels`, `bridges`, `bonds`, `vlans`). Set, it replaces the provisioner netplan files as a whole and goes through the same parser and checks, so both forms give the same network config. Unknown keys and the route keys BOOTy does not apply fail. YAML `network.devices`. |
| `IP_FORWARDING` | `true` | boolean | IPv4 forwarding sysctl (GoBGP). The provisioner `frr.conf` line `[no] ip forwarding` sets the default. An explicit value wins. YAML `network.ipForwarding`. |
| `IPV6_FORWARDING` | `false` | boolean | IPv6 forwarding sysctl (GoBGP), like `IP_FORWARDING` with `[no] ipv6 forwarding`. Off by default like the legacy ramdisk, where zebra and netplan keep the kernel default; on, the kernel also sets the Router flag in Neighbor Advertisements. YAML `network.ipv6Forwarding`. |
| `BGP_SESSIONS` | (unset) | JSON list | GoBGP sessions in the native form of the `frr.conf` neighbors (YAML `network.bgp.sessions`): `name`, `interface`, `vrf`, `peerGroup`, `remoteAS` (number, `internal`, `external`), `localAS` with `localASMode` (`no-prepend-replace-as`, the only mode), `keepalive`, `hold`, `connectRetry`, `families` (`ipv4-unicast`, `ipv6-unicast`, `l2vpn-evpn`), `allowASIn` (1-10, `allowas-in N` in each family), `routeMaps` (`[{"family":"ipv4-unicast","in":"M","out":"N"}]`, like `neighbor X route-map M in|out`), `bfd`, `bfdProfile` (see `BFD_PROFILES`). Resolved with the `frr.conf` rules: unset fields come from the peer-group, then from `BGP_DEFAULTS`. A non-empty list replaces the `frr.conf` sessions as a whole. Validation rejects a missing remote AS, other local-as modes, local-as equal to the router or remote AS, a member remote AS that differs from its group's, an `allowASIn` above 10, timers 0, hold below 3 and a session without a family. See [Networking](networking.md#bgp-sessions). |
| `BGP_PEER_GROUPS` | (unset) | JSON list | Peer-groups of `BGP_SESSIONS`, same fields without `interface` and `peerGroup` (YAML `network.bgp.peerGroups`). Requires `BGP_SESSIONS`. |
| `BGP_DEFAULTS` | `datacenter` | `traditional`, `datacenter` | The `frr defaults` profile of `BGP_SESSIONS`: timers traditional 60/180/30 s, datacenter 3/9/10 s (keepalive, hold, connect retry), and the other profile defaults (`bgp ebgp-requires-policy`, the software version capability, zebra `ip nht resolve-via-default`; see [Networking](networking.md#bgp-capabilities)). Wins over the `frr.conf` line. Requires `BGP_SESSIONS`. YAML `network.bgp.defaults`. |
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
| `BGP_CONNECT_RETRY` | `0` | uint32, seconds | GoBGP connect retry timer. `0` gives 5 s. GoBGP waits 1.5 to 2 s before the first connect after Idle (at once with `BGP_CONNECT_AT_ONCE`), and waits 0.75 to 1 times the timer, with a floor of 2 s, before each retry, so no retry comes sooner than 1.5 s. After a connection that failed in OpenSent (for example closed by the peer after a collision), the next connect comes after 1.5 to 2 s, whatever the timer is. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the sessions without `timers connect` (an explicit session or peer-group value wins). YAML `network.bgp.connectRetry`. |
| `BGP_CONNECT_AT_ONCE` | `false` | boolean | GoBGP starts the first connect of a peer at once when it leaves Idle, like FRR when `BGP_Start` runs (`ConnectAtOnce` of the GoBGP fork), instead of after a random 1.5 to 2 s. Off by default: after a carrier loss the leaf (FRR) has a connect pending until the link works again, and with this option GoBGP has one too, so both connections complete at the same time and collide. Both sides resolve the collision per RFC 4271 section 6.8 and keep BOOTy's connection, but in the production template lab about one fault in ten still ended with a second session (#811). Without it the leaf's connection usually arrives first and there is no collision; the cost is that a session that BOOTy opens comes up 1.5 to 2 s later. YAML `network.bgp.connectAtOnce`. |
| `BGP_LOCAL_AS` | `0` | uint32 | Local AS that GoBGP sends to unnumbered and other eBGP peers, like FRR `local-as N no-prepend replace-as`. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the eBGP sessions without `local-as` (an explicit session or peer-group value wins). `local_asn` is the FRR template value and does not apply to GoBGP. YAML `network.bgp.localAS`. |
| `BGP_NEIGHBOR_VRF` | (unset) | VRF name | GoBGP binds the numbered peers to this VRF device, like FRR `router bgp N vrf NAME`. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the numbered sessions in the default VRF (a `router bgp N vrf NAME` or session `vrf` wins). YAML `network.bgp.neighborVRF`. |
| `BGP_ALLOWAS_IN` | (unset) | 0 to 10 | How often the own ASN may be in a received AS path, like FRR `allowas-in N`. For `frr.conf` and `BGP_SESSIONS` sessions it is the default of the sessions without `allowas-in` (an explicit session or peer-group value wins). Unset or `0` leaves them without allowas-in. YAML `network.bgp.allowASIn`. |
| `BGP_MULTIPATH_RELAX` | (unset) | boolean | Paths with different AS paths of the same length are equal cost, like FRR `bgp bestpath as-path multipath-relax`. Unset keeps the `frr.conf` value, or off without `frr.conf`. YAML `network.bgp.multipathRelax`. |
| `BGP_INSTALL_ROUTES` | (unset) | boolean | GoBGP installs the learned IPv4 and IPv6 unicast routes into the kernel, like zebra (ECMP, link-local next hops). Then BOOTy installs no own route to a Type-5 VTEP. Unset means `true` with any BGP sessions (from `frr.conf` or `BGP_SESSIONS`) and `false` for the `BGP_PEER_MODE` peers. A BOOTy setting: zebra has no such switch and always installs. YAML `network.bgp.installRoutes`. |
| `BGP_ROUTE_MAPS` | (unset) | JSON list of route-maps | Route-maps like FRR, with the subset BOOTy implements: `[{"name":"TAG-IN","entries":[{"seq":10,"setLocalPreference":90,"setCommunities":["65169:200"],"setCommunityAdditive":true}]}]`. Entry fields: `seq`, `deny`, `matchCommunity` (a community-list), `setLocalPreference`, `setCommunities`, `setCommunityAdditive`. Entries run in `seq` order; a matching permit entry applies its sets and stops, a matching deny entry rejects, and no match rejects (implicit deny). A non-empty list replaces the `frr.conf` route-maps as a whole. Unknown fields fail. YAML `network.bgp.routeMaps`. |
| `BGP_COMMUNITY_LISTS` | (unset) | JSON list of community-lists | Standard community-lists for `matchCommunity`: `[{"name":"tagged","entries":[{"communities":["65169:200"]}]}]`. An entry matches a route with all its communities. Deny entries are not implemented. A non-empty list replaces the `frr.conf` community-lists as a whole. YAML `network.bgp.communityLists`. |
| `BGP_ROUTE_MAP_IN` | (unset) | route-map name | Import route-map of every peer in every address family, like `neighbor X route-map M in`. Also replaces the `frr.conf` import route-maps. YAML `network.bgp.routeMapIn`. |
| `BGP_ROUTE_MAP_OUT` | (unset) | route-map name | Export route-map of every peer in every address family. Also replaces the `frr.conf` export route-maps. YAML `network.bgp.routeMapOut`. |
| `BGP_EBGP_REQUIRES_POLICY` | (unset) | boolean | Like FRR `bgp ebgp-requires-policy`: an eBGP peer without a route-map in a family and direction exchanges no routes there, and BOOTy logs each such peer, family and direction at warning level. Unset follows `frr defaults` like FRR 10.3 for the `frr.conf` sessions and `BGP_SESSIONS`: on with `traditional`, off with `datacenter` and without the line (a `[no] bgp ebgp-requires-policy` line wins); off for the `BGP_PEER_MODE` peers. A set value applies to all of them. YAML `network.bgp.ebgpRequiresPolicy`. |
| `VRF_NAME` | (unset) | VRF name | Underlay VRF. Empty means no VRF. FRR renders `router bgp ASN vrf NAME`. Alias: `vrf_name`. |
| `VRF_TABLE_ID` | `1000` | uint32 | Routing table of `VRF_NAME`. `0` and `1` become `1000`. Alias: `vrf_table_id`. |
| `OVERLAY_VRF_NAME` | (unset) | VRF name or `default` | VRF of the provision bridge (GoBGP). `default` keeps the bridge in the default VRF, like the bm4x netplan. Unset means `VRF_NAME`. A set value wins over netplan. YAML `network.vrf.overlayName`. |
| `OVERLAY_VRF_TABLE_ID` | `0` | uint32 | Routing table of `OVERLAY_VRF_NAME`. `0` means `VRF_TABLE_ID`. A set value wins over netplan. YAML `network.vrf.overlayTableID`. |
| `BFD_TRANSMIT_MS` | `0` | 10 to 60000, ms | BFD transmit interval. Set it together with `BFD_RECEIVE_MS`. GoBGP: BFD on every native peer (single-hop, but multihop for an iBGP peer that is not on a connected network of its VRF, like FRR), and an explicit value replaces the interval of `frr.conf` BFD sessions. FRR also reads it: BFD on the fabric peers. YAML `network.bgp.bfdTransmitMS`. Alias: `bfd_transmit_ms`. |
| `BFD_RECEIVE_MS` | `0` | 10 to 60000, ms | BFD receive interval. Set it together with `BFD_TRANSMIT_MS`. FRR also reads it. YAML `network.bgp.bfdReceiveMS`. Alias: `bfd_receive_ms`. |
| `BFD_PROFILES` | (unset) | JSON list | BFD profiles of the `bfdProfile` of `BGP_SESSIONS` and of the `frr.conf` neighbors, like `bfd` / `profile NAME`: `name`, `transmitInterval` and `receiveInterval` (ms, 10 to 60000), `detectMultiplier` (2 to 255). Unset values and an unknown profile use the FRR defaults 300 ms, 300 ms, 3. A non-empty list replaces the `frr.conf` profiles as a whole. YAML `network.bgp.bfdProfiles`. |
| `BFD_DETECT_MULTIPLIER` | `0` (3) | 2 to 255 | BFD detect multiplier (GoBGP). An explicit value replaces the multiplier of `frr.conf` BFD sessions. YAML `network.bgp.bfdDetectMultiplier`. |
| `BGP_GRACEFUL_RESTART` | `true` | boolean | GoBGP sends the graceful restart (RFC 4724) and long-lived graceful restart (RFC 9494) capabilities like the FRR default helper mode: without address family tuples, so a peer does not keep BOOTy's routes when the session ends, and BOOTy keeps the routes of a restarting peer for its restart time. `false` sends neither, like FRR `bgp graceful-restart-disable`. YAML `network.bgp.gracefulRestart`. |
| `BGP_GRACEFUL_RESTART_TIME` | `120` | 1 to 4095, s | Restart time of the graceful restart capability, like FRR `bgp graceful-restart restart-time`. YAML `network.bgp.gracefulRestartTime`. |
| `BGP_GRACEFUL_NOTIFICATION` | version | boolean | GoBGP sets the N bit (RFC 8538) of the graceful restart capability, like FRR `bgp graceful-restart notification`. Unset follows `frr version` like FRR: off below 8.3 (the bm4x templates have 8.2.2), else on, also without `frr.conf`. With N exchanged, a NOTIFICATION other than Hard Reset keeps the routes of a peer that sends address family tuples, and an administrative shutdown is sent as Hard Reset. No effect with `BGP_GRACEFUL_RESTART=false`. YAML `network.bgp.gracefulNotification`. |
| `BGP_ADDPATH_RX` | `true` | boolean | GoBGP sends the add-path capability (RFC 7911) with receive in every address family, like FRR. YAML `network.bgp.addPathReceive`. |
| `BGP_HOSTNAME` | (unset) | host name | GoBGP sends it as the hostname in the FQDN capability. GoBGP sends the system host name, so BOOTy sets it as the ramdisk host name before BGP starts. Unset keeps the system host name (`HOSTNAME` with `RAMDISK_HOSTNAME=true`), like FRR, which also ignores a `hostname` line in `frr.conf`. At most 64 letters, digits, `-` and `.`. YAML `network.bgp.hostname`. |
| `BGP_SOFTWARE_VERSION` | profile | boolean | GoBGP sends the software version capability to every peer, like FRR `neighbor X capability software-version`. Unset follows `frr defaults` like FRR 10.3: every peer with `datacenter` (also without `frr.conf` and `BGP_DEFAULTS`), iBGP peers only with `traditional`. YAML `network.bgp.softwareVersion`. |

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
| `LLDP_NEIGHBOR_WAIT` | (unset) | duration, for example `90s` | `MODE=check` waits before the checks until every physical NIC with carrier has an LLDP neighbor, at most this long, and logs the NICs without one. Switches send every 30 s, so 90 s covers one lost frame. Set to `90s` in the cable check ISO. Skipped with `LLDP_RECEIVE=false`. YAML `check.lldpNeighborWait`. |
| `CHECK_PROFILES` | `health` | `health`, `cabling` separated by commas | What `MODE=check` runs: `health` the hardware health checks, `cabling` the cabling checks of every physical NIC (LLDP neighbor, expected cabling, dual homing). See [cable check](cable-check.md#host-side-checks). YAML `check.profiles`. |
| `CHECK_EXPECTED_CABLING` | (unset) | JSON list of `{mac or interface, switch, port, speedMbps}` | The expected switch port of each NIC, for example rendered from NetBox. A NIC is named by its MAC or, without one, its interface name. Unknown keys fail. YAML `check.expectedCabling`. |
| `CHECK_THRESHOLDS` | (unset) | JSON object | The action of each cabling check on failure (`off`, `warn`, `block`; block fails the run) and its limits. See [cable check](cable-check.md#thresholds). Unknown keys fail. YAML `check.thresholds`. |
| `CHECK_RESULT_SINKS` | (unset: log only) | `file`, `http`, `caprf`, `bm4x-report` separated by commas | Where the check result goes besides the log: `file` writes `/run/booty/check-result.json`, `http` posts it to `CHECK_RESULT_URL`, `caprf` posts the check results to `HEALTH_CHECK_URL`, `bm4x-report` writes the expected cabling diff in the bm4x `ConnectionReport` form to `/run/booty/bm4x-connection-report.json` and the log. YAML `check.sinks`. |
| `CHECK_RESULT_URL` | (unset) | URL | Receives the check result document (sink `http`), with the CAPRF authentication. YAML `check.resultURL`. |
| `CHECK_HOLD` | `off` | `off`, `until-poweroff`, a positive duration such as `10m` | What `MODE=check` does after the checks: `off` reboots as before; `until-poweroff` keeps BOOTy, the links and LLDP running until the machine is powered off (the bm4x cable check powers the host off); a duration holds that long, then reboots. See [cable check](cable-check.md#hold). YAML `check.hold`. |
| `INVENTORY_ENABLED` | `false` | boolean | Collect the hardware inventory. Failures are logged only. Each PF port of a NIC gets an `sriov` report, see [SR-IOV](sriov.md#report). |
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
| `OOB_STATUS` | `serial` | `serial`, `off` | Out-of-band status. `serial`: one `BOOTY1` status line on every active console (`/sys/class/tty/console/active`) at each phase change and at the end of a provision or deprovision run, also when the network fails. See [Out-of-band status](usage-security-telemetry.md#out-of-band-status). YAML `telemetry.oobStatus`. |
| `OOB_EFI_VAR` | `false` | boolean | Write the result of a run to the `BootyLastResult` UEFI variable when it changes, and log the previous result at start. Needs efivarfs. YAML `telemetry.oobEFIVar`. |

Provision and deprovision runs fill the `steps` list of the metrics summary:
name, `durationMs`, `status` (`done`, `failed` or `skipped`), `retries`, and
for a failed step `error` and `code` (see [Failure codes](failure-codes.md)).
The summary has no image or disk data yet, and BOOTy sends no events.

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

## Hardware watchdog

With `WATCHDOG_ENABLE=true` BOOTy arms a hardware watchdog right after it
reads the vars file, before the network setup, and pets it from a
supervisor goroutine. A hung run resets the host by itself, also when the
kernel hangs (stuck NVMe or HBA firmware, a process in `D` state), instead
of waiting for the CAPRF provision timeout.

- Device: `WATCHDOG_DEVICE`, else `/dev/watchdog0`, then `/dev/watchdog`.
  BOOTy first loads the drivers in `/modules/watchdog` of the ramdisk:
  `iTCO_wdt` (with `lpc_ich` and `i2c_i801`), `sp5100_tco`, `ipmi_watchdog`,
  `wdat_wdt` and `i6300esb`. They load only with `WATCHDOG_ENABLE`, so a host
  without it keeps its firmware watchdog behaviour. BOOTy sets the timeout
  with `WDIOC_SETTIMEOUT` and uses the value that the driver reports.
- IPMI fallback: without a device and with `WATCHDOG_IPMI_FALLBACK=true`,
  BOOTy programs the BMC watchdog over `/dev/ipmi0` with Set Watchdog Timer
  (NetFn 0x06, command 0x24, hard reset) and pets it with Reset Watchdog Timer
  (command 0x22). It sends the commands itself; the ramdisk has no `ipmitool`.
- Progress: BOOTy pets every third of the timeout. During a provisioning or
  deprovisioning step it pets only while the step makes progress: the step
  start counts, and so do written image bytes and every finished disk
  command. A step without progress for
  its budget stops the keepalive; BOOTy logs `BOOTY-WDT-001` with the step
  name and writes it out of band (the `BOOTY1` serial status line and, with
  `OOB_EFI_VAR`, the `BootyLastResult` UEFI variable that the next run logs;
  see the [failure code table](failure-codes.md)), and the hardware resets the host at most one timeout later. Outside
  a step (network setup, standby, a rescue shell, `FAILURE_ACTION=wait`)
  BOOTy pets as long as it runs.
- Budgets: a step may run this long without progress, then the keepalive
  stops. Steps not in the table use `WATCHDOG_TIMEOUT_SEC`.
  `WATCHDOG_STEP_BUDGETS` overrides any entry by step name. Wipe and erase
  steps report progress after every disk command (`wipefs`, `hdparm`,
  `nvme format`, `sg_sanitize`, `dd`), because the overwrite runs inside one
  command in the drive firmware without byte progress; one firmware
  overwrite of a 20 TB hard disk takes about 22 h, so their budget is 48 h
  per command.

  | Step | Default budget | Why |
  |------|----------------|-----|
  | `wipe-disks`, `decommission-wipe` | 48 h per disk command | Secure erase, sanitize and firmware overwrite of large disks |
  | `collect-firmware`, `setup-mellanox` | 1 h | Firmware reads and NIC firmware changes |
  | `setup-raid`, `setup-nvme-namespaces` | 1 h | Array creation and namespace changes wait for devices |
  | `apply-partition-layout`, `grow-partition` | 1 h | Partitioning and growth wait for devices and udev |
  | `stream-image` | 1 h | Image modes that write with `dd`; the HTTP download reports every written byte |
  | `mirror-root`, `check-filesystem`, `resize-filesystem` | 1 h | Copies and file system checks of large file systems |
  | `run-machine-commands`, `run-post-provision-cmds` | 1 h | Operator commands |
  | every other step | `WATCHDOG_TIMEOUT_SEC` (300 s) | |

- Handoff: before kexec and reboot BOOTy disarms the watchdog with the magic
  close (`V`), or with `WATCHDOG_HANDOFF=keep` leaves it armed for a target OS
  that takes it over (systemd `RuntimeWatchdogSec`); see the caveats in the
  [watchdog runbook](caprf-integration.md#hardware-watchdog). The target must open
  the device within the timeout, including firmware POST time on a reboot.
  Drivers that stop the watchdog on reboot (`watchdog_stop_on_reboot`) still
  disarm it at kexec and reboot; the BMC watchdog stays armed. A power off
  always disarms the watchdog, because a BMC watchdog would power the host on
  again. A kernel with `nowayout` ignores the magic close.
- Dry-run mode does not arm the watchdog. A missing device is logged as an
  error and the run goes on without a watchdog.

| Key | Default | Values | Description |
|-----|---------|--------|-------------|
| `WATCHDOG_ENABLE` | `false` | boolean (`true`, `false`, `1`, `0`, `yes`, `no`) | Arm the hardware watchdog. Other values are rejected. YAML `watchdog.enable`. |
| `WATCHDOG_TIMEOUT_SEC` | `0` (300) | integer, seconds, 10-3600 | Hardware timeout, and the default step budget. The driver may round it. YAML `watchdog.timeoutSeconds`. |
| `WATCHDOG_DEVICE` | (unset) (`/dev/watchdog0`, then `/dev/watchdog`) | clean absolute path under `/dev/` | Watchdog device. YAML `watchdog.device`. |
| `WATCHDOG_HANDOFF` | (unset) (`stop`) | `stop`, `keep` | `stop` disarms the watchdog at kexec and reboot; `keep` leaves it armed for the target OS. A power off always disarms it. YAML `watchdog.handoff`. |
| `WATCHDOG_IPMI_FALLBACK` | `false` | boolean | Arm the BMC watchdog through IPMI when no watchdog device exists. Needs `/dev/ipmi0` (the ramdisk loads `ipmi_si` and `ipmi_devintf`). YAML `watchdog.ipmiFallback`. |
| `WATCHDOG_STEP_BUDGETS` | (unset) | JSON object, step name to seconds (1-86400) | Time a step may run without progress, for example `{"wipe-disks":28800}`. Overrides the built-in budgets. YAML `watchdog.stepBudgets`. |

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
