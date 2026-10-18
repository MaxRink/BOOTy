# CAPRF integration

This page describes how cluster-api-provider-redfish (CAPRF) drives BOOTy:
the boot contract, the run modes, the callbacks, authentication, transport
rules and what BOOTy does at the end of a run.

Related pages:

- [Configuration reference](configuration.md): every `/deploy/vars` variable,
  with defaults and accepted values.
- [Migration from the CAPRF legacy ramdisk](caprf-legacy-migration.md): the
  `CAPRF_LEGACY_COMPAT` behaviour and the parity matrix.
- [Networking](networking.md): network stacks, EVPN, connectivity check.
- [Disk handling](disk.md): disk selection, wipe rules, RAID.

## Boot contract

CAPRF builds an ISO for each machine and boots it through Redfish virtual
media. The ISO holds a kernel and the BOOTy initramfs. BOOTy runs as PID 1
and reads its input from the `/deploy` directory of the ramdisk:

| Path | Content | Used by |
|------|---------|---------|
| `/deploy/vars` | Machine configuration, one variable per line | All modes. BOOTy cannot start without it. |
| `/deploy/file-system/` | Provisioner files (CAPRF `provisionerConfig.files`) | Copied into the installed OS. With `PROVISIONER_FILES_TARGET=ramdisk` (preset by `CAPRF_LEGACY_COMPAT=true`), copied into the ramdisk instead. |
| `/deploy/file-system/etc/netplan/*.yaml`, `/deploy/file-system/etc/frr/frr.conf` | Network input | Network setup in all modes. See [Netplan and FRR config input](networking.md#netplan-and-frr-config-input). |
| `/deploy/machine-files/` | Machine files | `copy-machine-files` step: copied into the installed OS. |
| `/deploy/machine-commands/` | One command per file. With `MACHINE_COMMANDS_MODE=script` (preset by `CAPRF_LEGACY_COMPAT=true`), one script per file (`#!` line or `/bin/sh`). | `run-machine-commands` step: run in a chroot of the installed OS, in CAPRF index order. |

### `/deploy/vars` format

`caprf.ParseVars` reads the file line by line:

- A line is `KEY=VALUE` or `export KEY=VALUE`. Empty lines and lines that
  start with `#` are ignored.
- BOOTy does not run the file as a shell script. It does not expand
  variables or command substitutions.
- Quoting, comments, duplicate keys and unknown keys are described in
  [Vars file format](configuration.md#vars-file-format).
- A value that does not parse, or a configuration that fails validation,
  stops the run. See [End of a run](#end-of-a-run).

Example for a provision run:

```bash
export MODE="provision"
export HOSTNAME="worker-01"
export IMAGE="https://images.example.com/ubuntu-24.04.img.gz"
export TARGET_OS="linux"
export TOKEN="bootstrap-token"
export INIT_URL="https://caprf.example.com/status/init"
export SUCCESS_URL="https://caprf.example.com/status/success"
export ERROR_URL="https://caprf.example.com/status/error"
export LOG_URL="https://caprf.example.com/log"
export DEBUG_URL="https://caprf.example.com/debug"
export MACHINE_EXTRA_KERNEL_PARAMS="intel_iommu=on"
export PROVIDER_ID="redfish://bmc-01/Systems/1"
export FAILURE_DOMAIN="az-1"
export REGION="eu-central"
```

Provisioning needs `TARGET_OS=linux` (or `PROVISION_TARGET_OS=linux`). The
`validate-provision-inputs` step fails without it, before any disk is
changed.

## Startup sequence

`runCAPRF` in `main.go` runs these steps:

1. Mount the early filesystems and load the kernel modules from `/modules`.
2. Parse `/deploy/vars`. If this fails, BOOTy has no client and sends no
   callback.
3. Apply the transport settings: TLS verification and proxy. See
   [Transport rules](#transport-rules).
4. With `PROVISIONER_FILES_TARGET=ramdisk`: copy `/deploy/file-system` into
   the ramdisk. With `RAMDISK_HOSTNAME=true` and for provisioning: set the
   ramdisk hostname. `CAPRF_LEGACY_COMPAT=true` presets both.
5. If `DRY_RUN=true`, set the mode to `dry-run`.
6. If `LOG_URL` is set, start the log shipping.
7. Set up the network and wait for connectivity. Dry-run skips this step.
   See [Connectivity check](networking.md#connectivity-check).
8. If `TOKEN_URL` is set, get a JWT and start its renewal.
9. Inspect crash artifacts of an earlier run, if crash artifact upload is
   configured.
10. Run the mode that `MODE` selects.
11. Hand off: kexec, reboot, power off, rescue shell or wait.

## Modes

`MODE` selects the run mode. An empty `MODE` means `provision`.

| `MODE` | What BOOTy does |
|--------|-----------------|
| `provision` | Runs the provisioning pipeline: writes the image to the target disk and configures the installed OS. |
| `deprovision`, `hard` | Wipes the disks and removes the EFI boot entries. Without a disk override it wipes every disk. |
| `soft-deprovision`, `soft` | Renames `grub.cfg` to `grub.cfg.bak` in `/boot/grub` and `/boot/grub2` on the installed root. The disk is not wiped. An existing `grub.cfg.bak` counts as done, so a rerun succeeds. If no `grub.cfg` or `grub.cfg.bak` exists, or the root cannot be unmounted after the rename, BOOTy reports an error, not success. |
| `decommission` | Wipes every disk and removes every EFI boot entry in `BootOrder`. Failures are logged and do not stop the run. CAPRF uses this mode for the `force-clean` annotation. |
| `standby` | Sends a heartbeat to `HEARTBEAT_URL` every 30 seconds and long polls `COMMANDS_URL` (`?wait=20`), at most every 10 seconds. It runs the `provision`, `deprovision`, `reboot` and `health-check` commands. Both URLs are required. See [Agent mode contract](#agent-mode-contract). |
| `dry-run` | Runs read-only checks (configuration, image, disk, EFI, health) and reports the result. It does not set up the network and does not write to disks. `DRY_RUN=true` selects this mode. |
| `check` | Runs the hardware health checks and reports the result. |

Wipe and disk rules are in [Disk handling](disk.md).

## Callbacks

BOOTy sends each callback as an HTTP `POST` with a `text/plain` body. It
tries a callback up to 3 times, with a 1 second and a 2 second pause. If a
URL is empty, BOOTy skips the callback.

| Variable | When BOOTy calls it | Body |
|----------|---------------------|------|
| `INIT_URL` | At the start of provision and deprovision (`report-init` step). Standby sends `standby`, check mode sends `health-check`. BOOTy also uses `INIT_URL` as the connectivity check target. | `provisioning started`, or `deprovisioning started` in the deprovision modes |
| `SUCCESS_URL` | At the end of a successful run (`report-success` step), and for dry-run and check results. | `provisioning complete handoff=<method>` or `deprovisioning complete handoff=<method>`, or a check summary. See [Handoff](#handoff). |
| `ERROR_URL` | When a step fails, when a handoff fails, and, with `FAILURE_ACTION=wait`, when the run fails before or outside a mode. | `step <name> failed [<code>]: <error>` with a [failure code](failure-codes.md), or a short failure message |
| `LOG_URL` | For every log record at level `INFO` or higher. BOOTy redacts attributes with sensitive names, buffers 256 lines and drops lines when the buffer is full. | One log line |
| `DEBUG_URL` | Never. BOOTy reads the variable but sends nothing to it. Diagnostic dumps go to the log and through `LOG_URL`. | |

## Handoff

The success message of a provision names how BOOTy starts the installed
OS: `handoff=kexec`, `handoff=reboot` or `handoff=poweroff`
(`provision.PlannedHandoff`).

| Method | When | What CAPRF does |
|--------|------|-----------------|
| `poweroff` | `SUCCESS_ACTION=poweroff` (also `CAPRF_LEGACY_COMPAT=true`), or A/B preserve-existing without a GRUB entry that BOOTy can resolve | Waits for power off, ejects the media, powers on. |
| `reboot` | kexec disabled, `SECUREBOOT_REENABLE=true`, firmware values changed, or no GRUB entry that BOOTy can resolve | Ejects the media and does not power cycle: BOOTy reboots. |
| `kexec` | Otherwise | Ejects the media and does not power cycle: BOOTy kexecs. If the kexec load or execute fails, BOOTy reboots; with A/B preserve-existing it powers off instead, because the firmware still boots the active slot (see below). |

A power cycle after `kexec` or `reboot` would cut the first boot of the
installed OS (cloud-init, Ignition). Older CAPRF releases ignore the field.

The deprovision success message names its handoff too, before the
not-wiped note (`deprovisioning complete handoff=reboot; not wiped, ...`):

| Method | When | What CAPRF does |
|--------|------|-----------------|
| `standby` | A `deprovision` command of a standby run with `STANDBY_DEPROVISION_SUCCESS_ACTION=standby` (not `MODE=deprovision`) | Leaves the host running: the agent waits for the next command. |
| `poweroff` | `SUCCESS_ACTION=poweroff` | Waits for the power off. |
| `reboot` | Otherwise | Powers the host off at once: the disks are wiped, and a wait for a power off that does not come costs the grace period. |

A/B preserve-existing never reboots after a provision: the firmware boot
path still points at the active slot, so without a kexec BOOTy powers off
and the host stays off until CAPRF or an operator acts (CAPRF's A/B flow
leaves the host running for the kexec and does not power cycle).

The kexec limitations and the decision table are in [kexec.md](kexec.md).

## Agent mode contract

In `MODE=standby` (agent mode) BOOTy stays in the ramdisk and takes commands
from CAPRF. CAPRF renders the two URLs into `/deploy/vars`. The canonical
paths are the ones of the CAPRF agent endpoints
(`internal/server/commands.go`):

| Method and path | BOOTy variable | Request | Response |
|-----------------|----------------|---------|----------|
| `POST /agent/heartbeat` | `HEARTBEAT_URL` | `text/plain` body `heartbeat` | `2xx`. BOOTy logs a failure and sends the next heartbeat 30 seconds later. |
| `GET /agent/commands` | `COMMANDS_URL` with `?wait=20` added | No body | A long poll: the server may hold the request until a command is queued or the wait ends. `204`: no command. `200`: a JSON array of commands, at most 1 MiB. Other codes count as a failed poll; BOOTy logs it. The next poll starts 10 seconds after the start of the previous one (at once when the poll took longer, also after a failed poll), and after a poll that returned commands, after 1 second or the rest of the 10 seconds, whichever is shorter. |
| `POST /agent/commands/ack` | `COMMANDS_URL` + `/ack` | JSON `{"id": "...", "status": "completed" or "failed", "message": "..."}` | `2xx`. BOOTy logs a failed ACK and does not send it again. |

Rules:

- Every request has `Authorization: Bearer <TOKEN>`, or the JWT when
  `TOKEN_URL` is set. The [Transport rules](#transport-rules) apply: plain
  HTTP needs `INSECURE_TRANSPORT=true` or a loopback URL.
- A command is `{"id": "...", "type": "...", "payload": "..."}`. `type` is
  `provision`, `deprovision`, `reboot` or `health-check`. BOOTy ACKs an
  unknown type with `failed`.
- CAPRF removes the commands that it returns (claim). BOOTy runs each
  command once and sends one ACK for it.
- `payload` is optional. It is base64 in JSON, like a Go `[]byte`. The
  decoded payload has lines in the [vars file format](configuration.md#vars-file-format).
- Heartbeats pause while a command runs; a heartbeat that already started
  may still arrive. CAPRF must not mark the agent unhealthy before the
  command ACK.

At start, standby sends `standby` to `INIT_URL`, one heartbeat and one
command poll. If one of them fails, the run fails. Then it loops. The
heartbeats run in their own goroutine, so a long poll does not delay them.
At start BOOTy logs `memTotalMiB` and `memAvailableMiB`.

A proxy or load balancer between BOOTy and CAPRF must not close an idle
request before 20 seconds: with a shorter idle timeout the first long poll
fails and standby ends at boot.

### Command payload

A `provision` or `deprovision` command can carry a payload, for example:

```text
HOSTNAME=worker-07
EXTRA_KERNEL_PARAMS=intel_iommu=on
```

- BOOTy appends the payload lines to the `/deploy/vars` content that it
  read at start (it does not read the file again) and parses the result as
  one vars file. So a payload entry wins over the same key in `/deploy/vars`,
  and the [vars file rules](configuration.md#vars-file-format) apply: the
  last entry wins, `EXTRA_KERNEL_PARAMS` still goes after
  `MACHINE_EXTRA_KERNEL_PARAMS`, `CAPRF_LEGACY_COMPAT` presets fill only
  unset options, and `Config.Validate` checks the result.
- The payload applies to that command only: BOOTy builds a separate config
  for the command and does not change the config of the standby loop. The
  next command starts from `/deploy/vars` again.
- `EXPORT_VARS=true` exports only the `/deploy/vars` entries at start. The
  payload keys are not exported to the process environment.
- The ramdisk already uses some settings, so a payload can not change them:
  the transport (`TOKEN`, `TOKEN_URL`, `TOKEN_ALGORITHM`, `INIT_URL`,
  `SUCCESS_URL`, `ERROR_URL`, `LOG_URL`, `DEBUG_URL`, `INSECURE_TRANSPORT`,
  `SKIP_TLS_VERIFY`, `EXPORT_VARS`), the agent (`HEARTBEAT_URL`,
  `COMMANDS_URL`, `STANDBY_DEPROVISION_SUCCESS_ACTION`), the network keys
  ([Common network](configuration.md#common-network),
  [GoBGP and EVPN](configuration.md#gobgp-and-evpn) and
  [FRR](configuration.md#frr)), `MODE`, `DRY_RUN` and `CAPRF_LEGACY_COMPAT`.
  The command type selects the mode.
- A payload can not redirect a request that carries the bearer token:
  `INVENTORY_URL`, `FIRMWARE_URL`, `HEALTH_CHECK_URL`, `TELEMETRY_URL`,
  `METRICS_URL`, `EVENT_URL`, `CRASH_ARTIFACTS_PREPARE_URL` and
  `CRASH_ARTIFACTS_UPLOAD_URL` are fixed too.
- BOOTy rejects a payload that changes one of these settings, that has an
  unknown key (also a misspelled one), a line that is not `KEY=VALUE`, a
  value that does not parse, or that fails validation. It does not run the
  command, ACKs it as `failed` with `invalid <type> payload: <reason>` (the
  reason names the keys, never their values) and stays in standby.
- A payload with only blank and comment lines counts as no payload.
- The `completed` ACK names the payload keys, for example
  `provisioning complete with payload keys EXTRA_KERNEL_PARAMS,HOSTNAME`.
- Trust: BOOTy trusts the payload as much as `/deploy/vars`. It can change
  the image, the disk and the kernel parameters. Serve `COMMANDS_URL` over
  HTTPS with a certificate that BOOTy verifies: do not use
  `INSECURE_TRANSPORT` or `SKIP_TLS_VERIFY` for agent mode in production.

### End of a command

| Command | Result | What BOOTy does |
|---------|--------|-----------------|
| `provision` | Success | ACK `completed`, then like a provision run: kexec, reboot or power off. |
| `provision` | Failure | ACK `failed`, then `RESCUE_MODE`. With `RESCUE_MODE=wait` the ACK comes before the wait. |
| `deprovision` | Success | ACK `completed`; when the wipe of all disks skipped disks in use, the message ends with `; not wiped, in use: <disks>`. With `STANDBY_DEPROVISION_SUCCESS_ACTION=standby`: send `standby` to `INIT_URL` and go on with heartbeats and polls in the same boot. Otherwise reboot, or power off with `SUCCESS_ACTION=poweroff`. |
| `deprovision` | Failure | ACK `failed`, then reboot, or wait with `FAILURE_ACTION=wait`. |
| `reboot` | | ACK `completed`, then reboot. |
| `health-check` | | ACK `completed` with message `healthy`, stay in standby. |

## Authentication

- `TOKEN`: BOOTy sends it as `Authorization: Bearer <TOKEN>`.
- `TOKEN_URL`: after the network is up, BOOTy exchanges `TOKEN` for a JWT
  and renews the JWT in the background. This needs `TOKEN` and `HOSTNAME`.
  `TOKEN_ALGORITHM` accepts `RS256` or `ES256`.
- If the JWT exchange fails, the run fails. See [End of a run](#end-of-a-run).

### Token renewal and expiry

A long agent mode run outlives a token. BOOTy renews or stops in time:

- With `TOKEN_URL`, BOOTy renews the JWT `TOKEN_RENEW_MARGIN_SECONDS` before
  it expires (default 20% of the `expires_in` lifetime). A failed attempt is
  retried `TOKEN_RENEW_RETRIES` times (default 5) with a backoff that starts
  at `TOKEN_RENEW_BACKOFF_SECONDS` (default 1 s), doubles up to 60 s and
  never waits past the expiry. The attempts stop when the token expires.
- With `TOKEN_URL`, the exchange at start returns a fresh `expires_in`. It is
  authoritative: BOOTy ignores `TOKEN_EXPIRES_IN` and `TOKEN_EXPIRES_AT`,
  which are only fallbacks for a token that BOOTy can not renew.
- Without `TOKEN_URL`, BOOTy can not renew `TOKEN`. With `TOKEN_EXPIRES_IN`
  (seconds from the start of BOOTy) or `TOKEN_EXPIRES_AT` (RFC 3339, needs a
  correct clock; see [time sync](configuration.md#time-sync)) it stops
  `TOKEN_RENEW_MARGIN_SECONDS` before the expiry, while the token still works
  for the report. `TOKEN_EXPIRES_IN` wins. BOOTy starts after the virtual
  media insert and the boot, so it sees `TOKEN_EXPIRES_IN` late by that time;
  CAPRF subtracts a boot allowance from the value it renders.
- The margin is at most half the lifetime, so a renewed token is not renewed
  again at once.
- When the renewal fails for good, the token expires, or the
  `TOKEN_EXPIRES_IN` or `TOKEN_EXPIRES_AT` margin is reached, BOOTy stops the active task (the
  standby loop or a running command), reports
  `token renewal failed, task stopped: <reason>` to `ERROR_URL` and ends like
  a failed run: it reboots, or waits with `FAILURE_ACTION=wait` (preset by
  `CAPRF_LEGACY_COMPAT=true`). A task that does not stop within 2 minutes is
  ended with a reboot (not with `FAILURE_ACTION=wait`).
- `CAPRF_LEGACY_COMPAT` presets none of the token options: the legacy
  provisioner had no renewal, and the defaults only act when CAPRF sends
  `TOKEN_URL`, `TOKEN_EXPIRES_IN` or `TOKEN_EXPIRES_AT`.

CAPRF today: the machine token is an opaque random token (not a JWT) that
lives as long as `--provision-timeout` (default 30 minutes) plus 30 seconds of
clock skew grace. CAPRF has no token or renewal endpoint and does not send the
expiry, so a standby agent gets `401` after 30 minutes. The smallest contract
that closes this, with no new BOOTy code:

| Method and path | Request | Response |
|-----------------|---------|----------|
| `POST /agent/token` (rendered as `TOKEN_URL`) | `Authorization: Bearer <current token>`; JSON body ignored | `200` with `{"access_token": "<new token for the same machine>", "expires_in": <seconds>}`. The old token stays valid until it expires. |

CAPRF issues the first token at boot in the same way, so BOOTy exchanges
`TOKEN` at start and renews before each expiry. Until CAPRF serves it, CAPRF
can render `TOKEN_EXPIRES_IN` (the token lifetime in seconds minus a boot
allowance), and BOOTy ends the run cleanly before the token expires.

## Transport rules

Each rule has its own option. `CAPRF_LEGACY_COMPAT=true` presets all of them
to the legacy value; an explicit option wins.

| Rule | Default | Legacy value | Option (preset value) |
|------|---------|--------------|-----------------------|
| Bearer token over plain HTTP | Refused, the request fails. Allowed only for `https://` or loopback URLs. | Allowed | `INSECURE_TRANSPORT=true` |
| `COMMANDS_URL` and crash artifact URLs over plain HTTP | Refused, except for loopback | Allowed | `INSECURE_TRANSPORT=true` |
| TLS certificate check for callbacks, the connectivity check and image downloads | Verified against the system roots | Not verified, like `curl --insecure`. BOOTy logs a warning. | `SKIP_TLS_VERIFY=true` |
| TLS certificate check for the crash artifact upload | Verified | Verified | None, always verified |
| `http://` image URLs | Refused | Allowed | `IMAGE_ALLOW_INSECURE_HTTP=true` |
| `TOKEN_URL` | HTTPS required (plain HTTP only for loopback). TLS verified. No proxy. | Same | None, `SKIP_TLS_VERIFY` and the exported proxy never apply |
| Proxy (`https_proxy`, `http_proxy`, `no_proxy`) | Read from the process environment. `/deploy/vars` entries do not reach the environment. | Every `/deploy/vars` entry is exported, so proxy entries apply. | `EXPORT_VARS=true` |

Planned (#624): redact URL query strings in transport errors.

## End of a run

"Wait" means that the ramdisk keeps running until it gets `SIGTERM` or
`SIGINT`, and then reboots. BOOTy runs `sync` before every reboot, power off
and kexec.

`SUCCESS_ACTION` selects the end of a successful run and `FAILURE_ACTION`
the end of a failed run. `CAPRF_LEGACY_COMPAT=true` presets
`SUCCESS_ACTION=poweroff` and `FAILURE_ACTION=wait`; an explicit value wins.
The right column shows the result with `SUCCESS_ACTION=poweroff` for the
success rows and `FAILURE_ACTION=wait` for the failure rows.

| Result | Default (`reboot`) | `SUCCESS_ACTION=poweroff` or `FAILURE_ACTION=wait` |
|--------|---------|----------------------------|
| Provision success | Report success, then kexec into the default GRUB entry of the installed OS. If kexec is not allowed or fails, reboot. | Report success, then power off. No kexec. |
| Deprovision success (all deprovision modes) | Report success, then reboot | Report success, then power off |
| Provision failure | Report the error, then apply `RESCUE_MODE` (default `reboot`) | Report the error, then apply `RESCUE_MODE` (default `wait`). See [Rescue modes](#rescue-modes). |
| Deprovision failure | Report the error, then reboot | Report the error, then wait |
| `/deploy/vars` does not parse | Reboot. No callback is possible. | Wait. No callback is possible. |
| Network setup, connectivity check or JWT exchange fails | Reboot | Report the error, then wait |
| Token renewal fails for good, the token expires, or the `TOKEN_EXPIRES_IN` or `TOKEN_EXPIRES_AT` margin is reached | Stop the task, report the error, then reboot | Stop the task, report the error, then wait |
| Reboot or power off request fails after success | Report the error, then open a shell. After a deprovision reboot, BOOTy opens the shell without a report. | Report the error, then open a shell |
| Dry-run or check ends | Reboot | Reboot |
| Standby `reboot` command | Reboot | Reboot |

A standby `provision` or `deprovision` command ends like the matching mode.
With `STANDBY_DEPROVISION_SUCCESS_ACTION=standby`, a successful standby
`deprovision` command returns to the standby loop instead. See
[Agent mode contract](#agent-mode-contract).

BOOTy skips kexec and reboots when one of these is true:

- `DISABLE_KEXEC=true`,
- `SECUREBOOT_REENABLE=true`,
- a step changed firmware values, for example the Mellanox SR-IOV setup,
- the installed root has no usable GRUB entry (see below).

BOOTy kexecs the entry that GRUB would boot, and only when it is sure:
any doubt, reboot. It does not emulate GRUB; it accepts the shapes that
`grub-mkconfig` generates on Debian and Ubuntu (`kexec.DefaultEntry`):

- `grub.cfg` from `/boot/grub`, then `/boot/grub2`.
- The default is an unconditional `set default`, or the generated
  `if [ "${next_entry}" ] ; then ... else set default=X fi` block.
  `${saved_entry}` comes from `grubenv` (`GRUB_DEFAULT=saved`); an empty
  default is entry 0.
- Submenu paths by number, id or title (`1>2`,
  `gnulinux-advanced-...>gnulinux-...`).
- Entries of the generated commands only: `insmod`, `echo`, `recordfail`,
  `load_video`, `gfxmode`, `set root=`, `search --set=root`, one `linux`
  and at most one `initrd` with one file, both outside conditions. The one
  exception is the initrdless boot fallback of Ubuntu cloud images
  (`GRUB_FORCE_PARTUUID`): `if [ "${initrdfail}" = 1 ]` with `linux` and
  `initrd` in the then branch and, in the else branch, the same `linux`
  line plus one trailing `panic=-1` and no `initrd`; any other difference
  reboots. GRUB boots the initrdless branch while `grubenv` has no
  `initrdfail`; BOOTy kexecs the initrd branch, GRUB's own fallback for
  the same kernel and root, because kexec needs an initrd and that branch
  boots on every image. `grubenv` stays as it is.
- Outside entries only the generated commands: `set`, assignments,
  `export`, `insmod`, `load_env` (not `-f`), `save_env`, `terminal_input`,
  `terminal_output`, `terminal`, `serial`, `font`, `loadfont`,
  `load_video`, `background_image`, `echo`, `true`, `search`,
  `fwsetup --is-supported`, the generated `unset` of
  `initrdless_boot_fallback_triggered`, the `41_custom` source line, and
  `if`, `function`, `menuentry` and `submenu` blocks. Function bodies get
  the same allow-list plus the entry helpers and may not set the default,
  `source` a file or run `linux` or `initrd`.
- `grubenv` variables count as unknown wherever they are used, because
  `load_env` imports them where it runs; a `default` in `grubenv` reboots.
- The default is matched like GRUB: a plain decimal number (no sign, no
  leading zero) is a position; otherwise the first entry whose id or title
  matches, in order.
- Variables that `grub.cfg` sets once and unconditionally at the top level
  are expanded like GRUB, when they are set (`set a=$b` takes the value
  of `b` at that line) (no expansion in single quotes, a backslash
  escapes, an unquoted value splits into words); `$vt_handoff` is dropped.
  A kernel argument with blanks is quoted for the kernel.

BOOTy reboots instead when `grubenv` sets `next_entry` (GRUB boots it once
and clears it), `prev_saved_entry`, `initrdfail` or `prev_entry` (the
Ubuntu initrdless fallback); for any other conditional default; for a
default entry inside a condition or a number after one; for any other
command in the entry (`set`, `search`/`probe`/`regexp --set` of another
variable, `source`, an unknown function); for `eval`, `regexp`, `probe`,
`unset`, `load_env -f` or any unknown command outside entries; for a `linux` or `initrd` inside
a condition; for BLS (`blscfg`, RHEL-likes), Flatcar (`configfile`, gptprio
kernel), any `source` or `configfile` (except the `41_custom` line when
the image has no `custom.cfg`); for an unknown or conditional variable,
several initrds, or a kernel argument with a double quote.

An A/B upgrade with `AB_PRESERVE_EXISTING=true` needs kexec. If kexec does
not happen, BOOTy powers off instead of a reboot. `SUCCESS_ACTION=poweroff`
(also through the `CAPRF_LEGACY_COMPAT=true` preset) cannot be combined with
`AB_PRESERVE_EXISTING=true`; the configuration is rejected.

### Rescue modes

`RESCUE_MODE` applies to a failed provision run, and to a failed
`provision` command in standby mode.

| `RESCUE_MODE` | What BOOTy does |
|---------------|-----------------|
| `reboot` | Reboot. This is the default unless `FAILURE_ACTION=wait`. |
| `retry` | Run the pipeline again after 30 seconds, up to 3 times, then reboot. Each run first unmounts what the failed run left under the target root. |
| `shell` | Open a rescue shell on the console. Reboot when the shell exits. |
| `wait` | Wait. This is the default with `FAILURE_ACTION=wait`. |

An explicit `RESCUE_MODE` wins over `FAILURE_ACTION` for a failed provision.
`FAILURE_ACTION` only selects the default:

| `RESCUE_MODE` | `FAILURE_ACTION` | Failed provision |
|---------------|------------------|------------------|
| (unset) | (unset) or `reboot` | Reboot |
| (unset) | `wait` (or `CAPRF_LEGACY_COMPAT=true`) | Wait |
| `reboot`, `retry`, `shell` or `wait` | any | That rescue mode |

`FAILURE_ACTION` alone decides a failed deprovision and a failure before or
outside a mode. `RESCUE_MODE` has no effect there.

For `shell` and `wait`, BOOTy installs `RESCUE_SSH_PUBKEY` for root, sets
`RESCUE_PASSWORD_HASH`, starts `dropbear` if the ramdisk has it, and mounts
the disks when `RESCUE_AUTO_MOUNT=true`.

## Hardware watchdog

Runbook for `WATCHDOG_ENABLE=true`. The keys and the budget table are in
[configuration.md](configuration.md#hardware-watchdog).

- A host that resets during a run without a CAPRF error callback: look for
  `BOOTY-WDT-001` in the shipped log, in the `BOOTY1 ... code=BOOTY-WDT-001
  step=...` serial status line, or in the `BootyLastResult` UEFI variable
  that the next run logs. It names the step that made no progress. The hardware reset follows at most
  `WATCHDOG_TIMEOUT_SEC` later. Raise the budget of that step with
  `WATCHDOG_STEP_BUDGETS` when the step is slow but healthy, for example
  `{"wipe-disks":259200}` for very large disks.
- `hardware watchdog not armed` in the log: no driver found a device. Check
  the driver of the platform (`iTCO_wdt`, `sp5100_tco`, `ipmi_watchdog`,
  `wdat_wdt`) and the firmware setting that hides the TCO timer, or set
  `WATCHDOG_IPMI_FALLBACK=true`.
- `WATCHDOG_HANDOFF=keep` caveats. Use it only when the target OS takes the
  watchdog over:
  - The target must open the device and pet it before the timeout runs out.
    The time counts from the handoff and includes the kexec or the firmware
    POST of a reboot and the boot of the target. Set `WATCHDOG_TIMEOUT_SEC`
    longer than that, and configure the target (systemd
    `RuntimeWatchdogSec`, `watchdog` daemon). Otherwise the host resets in a
    loop after every provisioning.
  - Drivers that stop the watchdog on reboot (`watchdog_stop_on_reboot`)
    disarm it at kexec and reboot anyway; `keep` then has no effect. The BMC
    watchdog of the IPMI fallback stays armed.
  - A power off always disarms the watchdog, also with `keep`: an armed BMC
    watchdog would power the host on again.
  - A kernel with `nowayout` ignores the magic close, so with `stop` the
    watchdog also stays armed. Do not boot BOOTy with `nowayout`.

## Enable CAPRF legacy compatibility

CAPRF sets `CAPRF_LEGACY_COMPAT=true` in `/deploy/vars` when it runs BOOTy
in place of its legacy shell provisioner.

- `true`, `1` and `yes` enable it, in any letter case. Other values disable
  it. When the key occurs more than once, the last line wins.
- `CAPRF_LEGACY_COMPAT` is a preset: `Config.Validate` sets each option that
  it implies and that the vars file does not set. An explicit option always
  wins. The list is in
  [CAPRF legacy compatibility](configuration.md#caprf-legacy-compatibility).
- `caprf.FailureConfig` reads only `CAPRF_LEGACY_COMPAT` and
  `FAILURE_ACTION`. So a `/deploy/vars` file that does not parse still
  selects the failure handling: BOOTy waits instead of a reboot.
- The preset includes the legacy transport rules: `INSECURE_TRANSPORT=true`,
  `IMAGE_ALLOW_INSECURE_HTTP=true`, `SKIP_TLS_VERIFY=true` and
  `EXPORT_VARS=true`. See [Transport rules](#transport-rules).

The full list of changes is in
[What CAPRF_LEGACY_COMPAT changes](caprf-legacy-migration.md#what-caprf_legacy_compat-changes).
