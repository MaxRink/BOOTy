# Changelog

All notable changes to BOOTy are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and BOOTy uses
[Semantic Versioning](https://semver.org/). The newest release is at the top.
[CONTRIBUTING.md](CONTRIBUTING.md#changelog) describes how to update this file.

Each line names its pull request. Lines without a number are commits that were
pushed without a pull request. Tags without a GitHub release are marked
"tag only". The `main` history was rewritten before `v0.1.0`, so the `v0.0.x`
tags are not ancestors of `main`; their entries come from the GitHub release
notes and the history of each tag.

## [Unreleased]

Changes merged after `v0.2.0-rc.15`. Without `CAPRF_LEGACY_COMPAT` and without
the new keys, only the items under "Behaviour changes in all modes" change
behaviour.

### Highlights

- `CAPRF_LEGACY_COMPAT=true` makes BOOTy behave like the CAPRF legacy shell
  provisioner (power off, wait on failure, disk selection and wipe, GRUB drop-in,
  provisioner files in the ramdisk, transport rules, network wait, hostname, NIC
  firmware log, Mellanox VFs, grow target).
- `CAPRF_LEGACY_COMPAT` is a preset: some legacy behaviours also have their own
  native option, and an explicit option always wins over the preset (#648).
- New deprovision mode `MODE=decommission` (CAPRF `force-clean`) (#586).
- EVPN: the L3VNI provisioning network advertises Type-5 routes only, and every
  host gets its own router MAC (#582, #583).

### Behaviour changes in all modes

- Netplan `set-name` renames the single NIC that its `match` selects, and a glob
  or MAC `match` selects the NIC of a static address, like `netplan apply`.
  Before, BOOTy ignored `set-name` and used the first NIC (#641).
- A provisioner netplan file or `frr.conf` that exists but does not parse
  (malformed YAML, invalid values such as an out-of-range ASN) now fails the
  network setup and the run. Before, BOOTy fell back to the vars. Unknown YAML
  keys still pass. `frr.conf` is read only when netplan files are present (#636).
- FRR config lines without indentation are now read. Numbered peers keep their
  own `remote-as`; a `frr.conf` with different numbered `remote-as` values or
  with `remote-as external` is a parse error and so fails the run (#634, #636).
- EVPN: `advertise-all-vni` enables L2 EVPN (Type-2/Type-3) only when the
  provisioning VNI is not an L3VNI. The CAPRF `frr.conf` (L3VNI 1000) now gives
  Type-5 routes only (#582).
- EVPN router MAC and provision bridge MAC: netplan `macaddress` of the provision
  bridge, else `02:54` + last 4 bytes of the IPMI MAC, else `02:54` + the 4 bytes
  of the underlay IPv4. The shared fallback `02:54:00:00:00:01` is gone. A
  non-IPv4 underlay is an error (#583).
- IPMI NIC autodetection no longer falls back to the first addressed NIC. Hosts
  without an IPMI NIC now derive the router MAC from the underlay IP (#592).
- Image source probing uses `GET` with `Range: bytes=0-0` instead of `HEAD`, so
  presigned URLs work. Every status `>= 400` rules a source out (#599).
- HTTP image downloads abort after 2 minutes without data and go through the
  normal retry and wipe-on-error path (#637).
- DHCP sends the default parameter request list (option 55), so servers such as
  Kea return router, DNS and MTU (#598).
- `e2fsck` killed or canceled now fails `check-filesystem` (#604).
- Soft deprovision without `grub.cfg` now fails instead of reporting success. A
  new `release-stale-mounts` step unmounts the target before rescue retries (#635).
- Ambiguous unnamed Linux root partitions are probed by content like the legacy
  script, instead of failing (#601).
- A separate `/boot` partition from the image fstab is mounted, so `update-grub`
  writes the `grub.cfg` that the firmware boots (#595).
- `EXTRA_KERNEL_PARAMS` from the vars file is appended to
  `MACHINE_EXTRA_KERNEL_PARAMS` (#594).
- Machine files: symlinks are recreated as links, files below `dev`, `proc`,
  `sys`, `run` are skipped, and mountpoints with a symlink in the path are
  refused (#629).
- The CAPRF client and `network.NewHTTPClient` use `http.ProxyFromEnvironment`
  (#589).
- Filesystem resize runs only the tool for the detected filesystem (#639).
- `sync()` runs before kexec, reboot and power off (#597, #623, #585).
- Before the target chroot, BOOTy creates the `/dev/disk/by-uuid`,
  `by-partuuid`, `by-label` and `by-partlabel` links, so `update-grub` writes
  `root=UUID=` instead of `root=/dev/sdXN`. `DISK_LINKS=false` turns it off
  (#671).

### Behaviour behind `CAPRF_LEGACY_COMPAT=true`

| Behaviour | PR | Native option (preset value) |
|---|---|---|
| Success: power off, no kexec | #585 | `SUCCESS_ACTION=poweroff` (#648) |
| Failure: report and wait, ramdisk stays up | #585 | `FAILURE_ACTION=wait` (#648) |
| Disk auto-detection in lsblk order | #590 | `DISK_SELECTION=lsblk` (#649) |
| Wipe every disk on provision and hard deprovision | #591 | `WIPE_ALL_DISKS=true` (#649) |
| `blkdiscard` NVMe disks before the wipe | #591 | `DISCARD_NVME=true` (#649) |
| Bearer token over plain HTTP | #589 | `INSECURE_TRANSPORT=true` |
| No TLS verification (never for `TOKEN_URL` or crash upload) | #589 | `SKIP_TLS_VERIFY=true` (#650) |
| Plain HTTP image URLs | #589 | `IMAGE_ALLOW_INSECURE_HTTP=true` |
| Export every vars entry to the environment | #589 | `EXPORT_VARS=true` (#650) |
| `/deploy/file-system` applied to the ramdisk, target `resolv.conf` not written | #588 | `PROVISIONER_FILES_TARGET=ramdisk`, `WRITE_RESOLV_CONF=false` (#652) |
| Ramdisk hostname set to `HOSTNAME` | #626 | `RAMDISK_HOSTNAME=true` (#652) |
| Legacy GRUB drop-in, no getty units | #594 | `GRUB_CMDLINE_MODE=append` (#653) |
| `nic_firmware` log line per NIC | #628 | `NIC_FIRMWARE_LOG=true` (#656) |
| Mellanox: raise `NUM_OF_VFS` to `NUM_VFS` | #617 | `NUM_VFS_MODE=raise` (#656) |
| `grow-partition` grows the last GPT Linux partition | #625 | none yet |
| Network wait like `wait_for_network` | #622 | none |
| `MODE=decommission` powers off after success | #586 | `SUCCESS_ACTION` |
| Machine commands run as scripts in the target chroot, in numeric order | #587 | none yet |

The preset fills only unset options, so an explicit value (also an explicit
default) wins. `SUCCESS_ACTION=poweroff` is rejected with
`AB_PRESERVE_EXISTING=true` (#648).

### Features

- `MODE=decommission`: wipe all disks (best effort), remove every EFI
  `BootOrder` entry, report success (#586)
- `SUCCESS_ACTION` (`reboot`|`poweroff`), `FAILURE_ACTION` (`reboot`|`wait`) and
  the preset mechanism (#648)
- `DISK_SELECTION` (`prefer-nvme`|`lsblk`), `WIPE_ALL_DISKS`, `DISCARD_NVME` (#649)
- `SKIP_TLS_VERIFY`, `EXPORT_VARS` (#650)
- `PROVISIONER_FILES_TARGET` (`target`|`ramdisk`), `WRITE_RESOLV_CONF`,
  `RAMDISK_HOSTNAME` (#652)
- `GRUB_CMDLINE_MODE` (`resolved`|`append`) (#653)
- `NIC_FIRMWARE_LOG`, `NUM_VFS_MODE` (`set`|`raise`) (#656)
- `CAPRF_LEGACY_COMPAT` parity (#585, #588, #589, #590, #591, #594, #617, #622,
  #625, #626, #628)
- NIC firmware versions are read from ethtool when sysfs has none (#628)
- disk: `DISK_SELECTION=lsblk` warns when more than one disk of the selected
  bus qualifies, because disk names can change between boots; `docs/disk.md`
  recommends `DISK_SERIAL_NUMBER` (#666)

### Security

- qcow2 images with a backing file or an external data file are rejected before
  `qemu-img convert` (#609)
- CAPRF transport errors carry only scheme, host, path and a fixed cause (#624)
- The FRR connectivity wait redacts the target URL; each `vtysh` dump has a 10 s
  timeout (#627)
- Machine-files symlinks are recreated, never followed (#629)
- `SKIP_TLS_VERIFY` never applies to `TOKEN_URL` or the crash upload (#589, #650)

### Fixes

- network: VRF devices no longer count as physical NICs (#605)
- network: a DHCP lease without a subnet mask no longer panics PID 1 (#608)
- gobgp: the periodic RA goroutine stops on teardown (#600)
- gobgp: Type-5 VTEP routes go through the numbered neighbor (#606)
- gobgp: a Type-5 route stays while another gateway still advertises it (#602)
- caprf: log lines after the remote handler is closed are dropped (#596)
- caprf: the multipart reader is closed when an upload is not sent (#638)
- disk: NVMe namespaces attach to the right controller ID (#610)
- disk: hidden NVMe multipath path nodes are skipped (#632)
- A kernel module that never loads is logged at WARN (#631)
- provision: deprovision runs report `deprovisioning started` to `INIT_URL`,
  not `provisioning started` (#663)

### CI, build and tests

- KVM scenarios `caprf-legacy-compat`, `caprf-native-parity` and `kexec-persist`
  (#585, #648, #607)
- e2e: loop device partition nodes are awaited, created without udev and
  checked before use, which fixes the ContainerLab disk image and Linux E2E
  "partition not found" flakes (#674)
- KVM: no `linux-image-generic` install (#640); legacy compat suite gets 40 min (#658)
- ContainerLab: explicit `EVPN_L2_ENABLED` node in the GoBGP lab (#593)
- Stronger tests: GoBGP, CAPRF and DHCP (#646), EVPN Type-2/Type-3 (#644), A/B
  checksum mismatch (#616), config-drive bonds (#618), gpgv fallback (#619), RAID
  minimum devices (#620)
- `make lint` uses the CI golangci-lint version (#603)
- Removed dead code: FRR config builder (#615), duplicate grub config parser (#621)

### Docs

- Canonical pages `docs/configuration.md`, `docs/caprf-integration.md`,
  `docs/caprf-legacy-migration.md`, `docs/networking.md`, `docs/disk.md` (#633)
- README sections link to the canonical docs (#647)
- Design for full FRR and netplan parity in GoBGP mode (#630)

### Dependencies

- `golang.org/x/net` 0.59.0 to 0.60.0 (GO-2026-6603, -6611, -6612, -6617) (#584)
- `github.com/pierrec/lz4/v4` 4.1.31 to 4.1.33 (#578)
- `anchore/sbom-action` 0.24.2 to 0.24.3 (#579)
- `oras-project/setup-oras` 2.0.1 to 2.0.2 (#580)

### Upgrade notes for CAPRF

- With `CAPRF_LEGACY_COMPAT=true`, expect power off after success and a waiting
  ramdisk after failure (no reboot loop).
- Check the provisioner `frr.conf` and netplan files: a file that does not parse
  now fails the run (#636).
- Hosts without an IPMI NIC get a different router MAC (#583, #592).
- `EXTRA_KERNEL_PARAMS` now reaches the kernel command line (#594).

## [0.2.0-rc.15] - 2026-10-07

Internal refactors to the standard library and the shared redaction package. No behaviour change.

### CI, build and tests

- diagnostics: Characterize diagnostics and Redfish before library adoption (#573)

### Maintenance

- auth: Reuse shared URL diagnostic redaction (#577)
- Replace collection helpers with Go standard library (#576)
- Remove unreferenced generic retry package (#575)
- diagnostics: Adopt shared redaction and stdlib slog fan-out (#574)

## [0.2.0-rc.14] - 2026-10-05

### Docs

- Require reuse of upstream libraries before writing helpers (#572)

### Dependencies

- Bump github.com/docker/cli from 29.8.1+incompatible to 29.8.2+incompatible (#569)
- Bump github.com/klauspost/compress from 1.20.0 to 1.20.1 (#570)
- Bump github.com/pierrec/lz4/v4 from 4.1.30 to 4.1.31 (#571)

### Maintenance

- Group Docker Dependabot updates

## [0.2.0-rc.13] - 2026-09-27

Includes the serial console resolution from firmware evidence and the GoBGP netplan VXLAN VRF binding.

### Fixes

- provision: Resolve serial console from firmware evidence (#563)
- gobgp: Honor netplan VXLAN VRF binding (#530)

### CI, build and tests

- ci: Preserve blocking QEMU output and complete dependency tracking (#554)
- build: Retry network downloads in container builds (#535)
- Stop govulncheck stdlib findings from blocking pull requests (#534)
- e2e: Repair the containerlab DHCP topology (#533)

### Dependencies

- Bump google.golang.org/grpc
- Bump github.com/dustin/go-humanize
- Bump github.com/ulikunitz/xz
- Bump orhun/git-cliff-action
- Bump github.com/mdlayher/packet
- Bump github.com/sirupsen/logrus
- Bump github.com/docker/cli
- Bump docker/setup-buildx-action from 4.3.0 to 4.4.0 (#556)
- Bump github.com/pierrec/lz4/v4 from 4.1.29 to 4.1.30 (#558)
- Bump github.com/docker/docker-credential-helpers from 0.9.8 to 0.9.9 (#560)
- Bump github.com/mdlayher/socket from 0.6.1 to 0.7.0 (#561)
- Bump github.com/josharian/native from 1.1.0 to 1.2.0 (#562)
- Bump orhun/git-cliff-action from 4.8.0 to 4.9.0 (#550)
- Bump golang.org/x/net from 0.58.0 to 0.59.0 (#553)
- Bump golang.org/x/sys from 0.47.0 to 0.48.0 (#552)
- Bump github.com/klauspost/compress from 1.19.2 to 1.20.0 (#551)
- Bump github.com/google/go-containerregistry from 0.22.0 to 0.22.1 (#549)
- Bump github.com/google/go-containerregistry from 0.21.9 to 0.22.0 (#542)
- Bump google.golang.org/grpc from 1.83.1 to 1.83.2 (#548)
- Bump google.golang.org/grpc from 1.83.0 to 1.83.1 (#543)
- Bump golang from 1.27.0-alpine to 1.27.1-alpine in /test/e2e/clab/vrnetlab (#544)
- Bump softprops/action-gh-release from 3.0.2 to 3.0.3 (#545)
- Bump golang from 1.26.6-bookworm to 1.27.1-bookworm (#546)
- Bump anchore/sbom-action from 0.24.0 to 0.24.2 (#547)
- Bump golang from 1.26.6-alpine to 1.27.0-alpine in /test/e2e/clab/vrnetlab (#539)
- Bump github.com/pierrec/lz4/v4 from 4.1.28 to 4.1.29 (#541)
- Bump golang from 1.26-alpine to 1.27-alpine in /test/e2e/clab (#540)
- Bump golang.org/x/net from 0.57.0 to 0.58.0 (#537)
- Bump docker/setup-buildx-action from 4.2.0 to 4.3.0 (#538)
- ci: Bump Go to 1.26.6 (#532)
- Bump docker/login-action from 4.5.2 to 4.6.0 (#531)

## [0.2.0-rc.12] - 2026-08-12

GoBGP EVPN: BOOTy installs direct Type-5 routes and follows the leaf gateway model for Type-5 topologies.

### Fixes

- network: support direct EVPN Type-5 routes and align the Type-5 topology with the leaf gateway model (#528)

## [0.2.0-rc.11] - 2026-08-11

### CI, build and tests

- release: Retry transient SBOM downloads (#527)

## [0.2.0-rc.10] - 2026-08-11

Tag only, no GitHub release.

### CI, build and tests

- release: Retry transient GHCR authorization (#526)

## [0.2.0-rc.9] - 2026-08-11

### Dependencies

- Update golang.org/x/text (#525)

## [0.2.0-rc.8] - 2026-08-11

### Dependencies

- Update indirect Go modules (#524)

## [0.2.0-rc.7] - 2026-08-11

### Dependencies

- Update Go module dependencies (#523)

## [0.2.0-rc.6] - 2026-08-11

Tag only, no GitHub release. The first release with these changes is rc.7.

### Fixes

- caprf: Fail command polling on missing endpoint (#522)

### Dependencies

- Bump docker/login-action from 4.5.0 to 4.5.2 (#521)
- Bump docker/login-action from 4.4.0 to 4.5.0 (#516)
- Bump github.com/klauspost/compress from 1.19.0 to 1.19.1 (#517)
- Bump actions/checkout from 7.0.0 to 7.0.1 (#518)
- Bump github.com/ulikunitz/xz from 0.5.15 to 0.5.16 (#519)
- Bump google.golang.org/grpc from 1.79.3 to 1.82.1 (#520)

## [0.2.0-rc.5] - 2026-07-17

No provisioning behaviour change from rc.4.

### CI, build and tests

- Correct nonexistent networkboot/dhcpd image tag in DHCP e2e topology (#512)

### Dependencies

- Bump oras-project/setup-oras from 2.0.0 to 2.0.1 (#515)
- Bump actions/setup-go from 6.5.0 to 7.0.0 (#514)
- Bump softprops/action-gh-release from 3.0.1 to 3.0.2 (#513)
- Bump docker/login-action from 4.3.0 to 4.4.0 (#509)
- Bump golang.org/x/net from 0.56.0 to 0.57.0 (#510)

## [0.2.0-rc.4] - 2026-07-09

GoBGP EVPN pure Type-5: BOOTy advertises the Router MAC extended community on local Type-5 routes and programs gateway neighbors from received Router MACs. Malformed Router MACs are rejected. No configuration change.

### Fixes

- Program router MACs for EVPN type-5 routes (#507)

## [0.2.0-rc.3] - 2026-07-09

The default and GoBGP initramfs (and the GoBGP ISO) include the `vrf` kernel module, which fixes `create VRF: ... operation not supported`. Go toolchain 1.26.5.

### Fixes

- Include vrf module in initramfs (#505)

## [0.2.0-rc.2] - 2026-07-08

GoBGP EVPN: imported Type-5 gateway routes are installed with the on-link flag, so routes via a remote VTEP work when `br.provision` has only a `/32`.

### Fixes

- Mark EVPN type-5 routes on-link

## [0.2.0-rc.1] - 2026-07-07

Adds OCI image pre-pulls (`OCI_PREPULL_*`) and Ubuntu overlayroot (`OVERLAYFS_*`). The provisioning pipeline grows from 43 to 45 steps.

### Breaking changes

- `SYSEXT_CATALOG_DIR` must not equal `SYSEXT_ACTIVE_DIR` or point at an active systemd-sysext search path. Move preload catalogs to an inactive path (#500)

### Features

- Cache OCI image pre-pulls during provisioning (#501)
- Configure overlayroot for immutable Ubuntu images (#502)
- Harden systemd-sysext preload catalog handling (#500)

## [0.1.0] - 2026-07-06

Feature and hardening release: fail-closed provisioning, stricter image and security defaults, and a signed release pipeline (SBOM, Trivy scan, Cosign).

### Breaking changes

- Release tags must be annotated SemVer tags without build metadata, reachable from `main`, without an existing release (#326, #343, #356)
- Provisioning requires `PROVISION_TARGET_OS` or `TARGET_OS`; only `linux` is accepted (#342)
- Plain HTTP image sources need `IMAGE_ALLOW_INSECURE_HTTP=true` (#465). `TOKEN_URL` must use HTTPS except for localhost
- OCI image sources must be digest-pinned or come with `IMAGE_CHECKSUM` (#305); `IMAGE_SIGNATURE_URL` requires `IMAGE_CHECKSUM` (#319)
- `PERSIST_NETWORK=true` requires `OS_FAMILY`; RHEL persistence and Flatcar cloud-init fail closed (#285, #309, #367)
- Non-bond cloud-init network generation requires `STATIC_IFACE` (#310)
- Unsupported GoBGP underlay/BFD combinations, IPv6 GoBGP neighbors, ambiguous root partitions, missing secure erase tools and missing system-A/B data mountpoints fail closed (#320, #341, #345, #386, #414, #430)
- The unused Go packages `pkg/bios`, `pkg/bootloader` and `pkg/event` were removed (#463, #474)

<details><summary>All pull requests in 0.1.0</summary>

#### Features

- provision: Wire telemetry metrics and events into orchestrator (#469)
- secureboot: Enforce pinned digests for expected boot artifacts (#477)

#### Security

- Redact CAPRF logs before local output (#251)
- Redact Kafka log attributes (#255)
- Redact crash artifacts by default (#301)
- Redact debug dump output secrets (#350)
- Redact compound command secrets (#371)
- Redact gpg verification URL errors (#412)
- Redact dry-run URL error variants (#413)
- security: Redact URL credentials and query tokens in command logs (#464)
- security: Reject plain HTTP image sources without explicit opt-in (#465)
- Harden rescue secrets and LUKS errors (#488)
- Redact CAPRF URL construction errors (#497)

#### Fixes

- Replace resolv.conf symlink before DNS write (#246)
- Mount run tmpfs in initramfs (#248)
- Include runtime trust and gpg verification tools (#249)
- Verify checksums in partition image mode (#250)
- Include qemu-img in full initramfs flavours (#252)
- Bind NetworkManager profiles to interfaces (#258)
- Detect Debian EFI grub loaders (#259)
- Gate cloud-init grub datasource args (#260)
- Parse documented BGP_REMOTE_ASN var (#262)
- Configure initramfs DNS resolvers (#264)
- Scope deprovision wipe to configured disk (#265)
- Resize mounted xfs and btrfs roots (#266)
- Remount target paths when resuming checkpoints (#267)
- Reboot after provision when kexec falls back (#268)
- Reject malformed image checksums before streaming (#269)
- Fail closed on explicit gobgp setup errors (#270)
- Reject bearer auth for presigned crash uploads (#271)
- Reboot when secure boot re-enable is requested (#272)
- Render frr local-as and dcgw bfd (#273)
- Include provisioning tools in slim initramfs (#275)
- Run health checks during dry-run (#276)
- Persist static target network config (#277)
- Fail closed on firmware minimum mismatches (#278)
- Create EFI boot entries with host efibootmgr (#279)
- Create bonds before vlan interfaces (#280)
- config: Reject unsupported persisted network layouts (#285)
- Scan all root args for booted A/B slot (#286)
- Combine netplan gateway and static routes (#287)
- Sync qcow2 streams before returning (#288)
- Package LVM command aliases in initramfs (#289)
- Validate image source before destructive provisioning (#291)
- Include real formatters in slim initramfs (#294)
- Fail closed for implicit ab root selection (#296)
- Bind cloud-init networking to static iface (#297)
- Honor zero Mellanox VFs as skip (#299)
- Accept documented CAPRF network vars (#300)
- Write kubelet args to kubeadm env files (#302)
- Verify image signatures before storage wipe (#303)
- Require pinned OCI image sources (#305)
- Validate OCI reachability in dry-run (#306)
- config: Block RHEL network persistence (#309)
- Require cloud-init target interface (#310)
- Require explicit Flatcar USR source selection (#311)
- Preserve sparse source partition numbers (#312)
- Validate EFI device paths before command use (#316)
- Persist NVMe target device for resume (#318)
- Require checksum with image signatures (#319)
- Reject ambiguous root partitions (#320)
- Support ipv6 network persistence (#328)
- Use debian efi boot entry labels (#335)
- Honor static addresses from netplan overlays (#336)
- Derive partition target nodes for md and by-id disks (#338)
- Roll back partial chroot bind mounts (#339)
- Clean up network link layers on teardown (#340)
- Reject unsupported gobgp underlay address families (#341)
- Require provision target os preflight (#342)
- Bound evpn ipmi address derivation (#344)
- Reject unsupported bfd configs (#345)
- Rerun volatile resume setup steps (#346)
- Refresh partition table after growpart (#348)
- Fail clearly on unsupported vfat fsck resize (#349)
- Support ipv6 and vlan cloud-init seeds (#351)
- Preflight qcow2 image prerequisites (#357)
- Stream images into partition layouts (#363)
- config: Reject flatcar cloud-init provisioning (#367)
- Declare netplan backing ethernets (#368)
- Reject vmware image containers (#369)
- Handle stable disk partition numbers (#370)
- Clean up direct frr daemons (#373)
- Reject multi-layer OCI images (#375)
- Include xfs and btrfs tools in slim initramfs (#376)
- Fail closed on requested mellanox sriov (#379)
- Resolve kexec boot-relative paths (#383)
- Fail closed when secure erase tools are missing (#386)
- Use bundled lvm tool for layout operations (#387)
- Preserve target root for kexec (#388)
- Create configured raid arrays (#389)
- Reap adopted child processes as pid1 (#395)
- Fallback to sh for syscall chroot (#397)
- Package util-linux lsblk in initramfs (#398)
- Disable lvm before stopping raid (#400)
- Enforce crash archive size cap (#404)
- Treat terminal success report as best effort (#405)
- Validate standby endpoint readiness (#406)
- Validate image source references before wipe (#410)
- Package util-linux disk probe tools in initramfs (#411)
- Reject ipv6 gobgp neighbors (#414)
- Normalize hpe vendor strings (#415)
- Remove stale network persistence files (#416)
- Stop using board version as bmc firmware (#417)
- Route numbered gobgp gateway via peer (#419)
- Report failed provisioning handoff (#421)
- Wait for partition device nodes (#424)
- Select whole-disk root partition (#425)
- Fail closed on early init setup errors (#428)
- Render networkd DHCP as IPv4 (#429)
- Require system-ab data mountpoints (#430)
- Validate reused boot and data mounts (#432)
- Require gobgp provision gateway (#434)
- Fail initrd lvm build on make errors (#460)
- Fail preflight closed on image source resolution errors (#461)
- provision: Propagate terminal success report errors to trigger retries (#462)
- observability: Ensure remote logs are flushed before terminal actions (#467)
- error-handling: Tear down static network config before DHCP fallback (#468)
- network: Prevent DHCP lockup if address installation fails (Closes #448) (#471)
- network: Return prerequisite failures in FRR setup instead of discarding them (#473)
- bios: Remove unused vendor bios management package (#474)
- Resolve issue 441 (#476)
- Repair secureboot main regressions (#478)
- Fail closed on resume checkpoint errors (#487)
- Retry HTTP sysext fetches (#489)
- Harden provision kubelet and GoBGP setup (#492)
- Fix image checksum sync ordering (#493)
- Stop reboot after failed standby deprovision (#495)
- Render FRR BGP auth password (#496)
- Guard provision copy destinations (#498)

#### CI, build and tests

- Run Flatcar-like system-ab KVM scenario (#240)
- Install Linux e2e tool prerequisites (#241)
- Retry topology smoke output checks (#242)
- Wait for vrnetlab eth0 readiness (#243)
- Fail clearly when swtpm socket is not ready (#244)
- Require release initramfs artifacts (#247)
- Set default topology for integration e2e target (#253)
- Harden release tag publication (#254)
- Bound kvm matrix job runtime (#256)
- Force e2e tests to execute (#257)
- Fail initramfs packaging pipelines (#261)
- Fail e2e assertions when behavior is absent (#263)
- Run kvm matrix for runmode changes (#274)
- Refuse overwriting release ORAS artifacts (#281)
- Harden PR artifact packaging (#282)
- Ignore stale boot logs after restart (#283)
- Quote e2e make target messages (#284)
- Run KVM matrix for network inputs (#292)
- Verify FRR apt key fingerprints (#293)
- Wait for luks nbd disconnects (#295)
- Retain e2e diagnostics artifacts (#304)
- Prepare clab image before local deploy (#308)
- Fail closed on empty e2e discovery (#313)
- Fail kvm matrix on empty test match (#314)
- Fail closed on missing coverage totals (#315)
- Make kvm log upload best effort (#322)
- Fail closed on missing release artifacts (#323)
- Avoid shared clab loop devices (#324)
- Sign release OCI artifacts (#325)
- Require annotated release tags (#326)
- Wait for type5 spine fdb convergence (#331)
- Wait for vrnetlab evpn routes (#332)
- Clean up linux e2e mounts on failure (#333)
- Require release tags on main (#343)
- Harden nightly provenance (#347)
- Fail closed on skipped tests (#353)
- Disable persisted checkout credentials (#354)
- Guard release container tags (#355)
- Reject oci-invalid release tags (#356)
- Clean up local containerlab teardown (#358)
- Package initramfs device nodes (#360)
- Gate manual gitlab package publish (#361)
- Bound release runtimes and harden runner apt setup (#362)
- Gate release promotion dispatch refs (#364)
- Bound boot e2e reachability polling (#365)
- Mark linux e2e tests linux-only (#366)
- Harden diagnostic artifact uploads (#372)
- Stamp release initramfs build info (#374)
- Validate makefile publish variables (#378)
- Include efi and system changes in kvm matrix (#380)
- Bound main workflow job runtimes (#382)
- Annotate release oras artifacts with provenance (#384)
- Clarify flatcar-like kvm scenario (#385)
- Checksum release sbom asset (#390)
- Bound manual workflow runtimes (#391)
- Pin promote checkout action (#392)
- Include runtime packages in kvm filters (#393)
- Cap initramfs and pr iso sizes (#399)
- Isolate oci initramfs artifacts (#401)
- Fail closed on missing release oci artifacts (#402)
- Pin ci workflow action refs (#407)
- Tag redfish mock e2e tests (#408)
- Prevent oci artifact overwrites (#409)
- Run production smoke containerlab e2e (#420)
- Scan built initramfs artifacts (#422)
- Restrict pr iso artifacts to trusted refs (#426)
- Set release oras artifact types (#427)
- Sign per-arch release images (#431)
- Reset boot access logs on startup (#436)
- Include gobgp provision gateway in underlay AF validation (#437)
- Add MOKEnroller coverage via mokutil command seam (#458)
- Refactor e2e polling helpers (#459)
- Extract nightly initramfs artifacts safely (#479)
- Attest nightly SBOM as SPDX JSON (#480)
- ci: Generate nightly SBOM from artifacts (#490)
- Expand artifact and topology coverage (#494)
- Tolerate initramfs device nodes during scan extraction (#499)

#### Docs

- Clarify target OS support boundaries (#245)
- Clarify systemd-boot support status (#298)
- Clarify network persistence limits (#307)
- Fix NVMe namespace config example (#317)
- Clarify ConfigDrive seed support (#321)
- Clarify hardware support status (#327)
- Clarify gobgp vendor test coverage (#329)
- Clarify suse support status (#334)
- Align proposals with implemented support (#337)
- Correct gobgp evpn type5 default (#359)
- Align ci and e2e guidance with workflows (#377)
- Clarify lacp aliases as bond tests (#381)
- Correct fedora bootloader wording (#394)
- Remove stale legacy server mode claims (#396)
- Use https jwt token endpoint examples (#403)
- Document release verification (#423)
- Align proposal support claims (#433)
- Clarify gobgp runtime limits (#435)
- Add missing config flags to feature gates table (Closes #442) (#470)
- Refresh provisioning recovery limits (#491)

#### Dependencies

- Bump softprops/action-gh-release from 3.0.0 to 3.0.1 (#290)
- Bump actions/setup-go from 6.4.0 to 6.5.0 (#481)
- Bump docker/login-action from 4.2.0 to 4.3.0 (#482)
- Bump github.com/klauspost/compress from 1.18.6 to 1.19.0 (#483)
- Bump docker/setup-buildx-action from 4.1.0 to 4.2.0 (#484)
- Bump golangci/golangci-lint-action from 9.2.1 to 9.3.0 (#485)
- Bump actions/download-artifact from 7.0.0 to 8.0.1 (#486)

#### Maintenance

- Cleanup dead bootloader and consolidate event models (#463)
- Centralize HTTP client setup (#466)

</details>

## [0.0.22] - 2026-06-24

### CI, build and tests

- Pin release ORAS to supported version (#239)

## [0.0.21] - 2026-06-24

Tag only, no GitHub release.

### CI, build and tests

- Use ORAS version without v prefix (#238)

## [0.0.20] - 2026-06-24

Tag only, no GitHub release. The first release with these changes is v0.0.22.

### Features

- cloudinit: Support configdrive datasource (#220)

### Fixes

- Mount system-ab roots read-only (#217)
- provision: Scope disk wipe to selected target (#223)
- network: Require packaged FRR daemons on direct start (#226)
- initrd: Include runtime libs for copied tools (#228)
- caprf: Require literal loopback for HTTP bearer auth (#229)
- image: Skip text sidecars for default OCI layer (#230)
- provision: Report secureboot re-enable handoff (#231)
- network: Filter foreign gobgp withdraws (#232)
- image: Fail stream when device sync fails (#236)
- network: Clean up gobgp overlay setup failures (#222)

### CI, build and tests

- Fail KVM prerequisite skips in CI (#225)
- Gate release publishing on vulnerability scan (#227)
- Fix release v2 oras and manifest publishing (#233)
- Clean generated root artifacts (#234)
- Verify initrd source inputs (#237)
- Keep PR artifacts from using publish secrets (#224)
- Clean up clab image loop devices on failure (#221)
- Upload zstd nightly initramfs artifacts (#218)
- Harden nightly version handling (#219)

### Docs

- Align support claims with ci coverage (#235)

## [0.0.19] - 2026-06-23

### Features

- Add system a/b shared data layout (#213)

### Security

- Redact token URL in auth errors (#214)

### CI, build and tests

- Harden release workflow feedback (#215)
- Harden BOOTy feedback gates (#212)
- Enforce coverage threshold (#211)

### Dependencies

- Bump actions/checkout from 6 to 7 (#209)
- Bump github.com/google/go-containerregistry from 0.21.6 to 0.21.7 (#210)

### Maintenance

- Refresh CI tool pins (#216)

## [0.0.18] - 2026-06-14

### Features

- Add native sysext provisioning (#204)
- cmd: Add CLI subcommands for operating modes
- config: Add multi-format config loader (YAML/JSON)

### CI, build and tests

- e2e: Add "mode exited with error" to vrnetlab allowed errors (#196)

### Docs

- config: Full GoDoc coverage and external controller examples

### Dependencies

- Bump golang.org/x/net from 0.55.0 to 0.56.0 (#206)
- Bump alpine from 3.23 to 3.24 in /test/e2e/clab (#205)
- Bump golang.org/x/sys from 0.45.0 to 0.46.0 (#207)
- Bump github.com/pierrec/lz4/v4 from 4.1.26 to 4.1.27 (#203)
- Bump busybox from 1.37.0-musl to 1.38.0-musl in /test/e2e/assets/busybox (#201)
- Bump golang.org/x/net from 0.54.0 to 0.55.0 (#200)
- Bump docker/setup-buildx-action from 4.0.0 to 4.1.0 (#199)
- Bump docker/login-action from 4.1.0 to 4.2.0 (#198)
- Bump busybox from 1.37.0-musl to 1.38.0-musl (#197)

### Maintenance

- Extract mode dispatch into pkg/runmode interface
- config: Composable sub-structs for Config

## [0.0.17] - 2026-05-26

### Features

- Use zstd compression for default and slim initramfs (#189)
- UPX-compress lvm and sfdisk static binaries (#191)
- default: UPX-compress FRR and tool binaries in default initramfs (#190)

## [0.0.16] - 2026-05-26

### CI, build and tests

- release: Include .cpio.zst gobgp initramfs in GitHub Release assets

## [0.0.15] - 2026-05-26

### Features

- gobgp: UPX-compress init and tool binaries (#188)
- Use zstd compression for gobgp initramfs (#187)
- Strip debug symbols from tool binaries in initrd (#186)
- Add -trimpath to all go build invocations (#185)

### Dependencies

- Bump github.com/google/go-containerregistry from 0.21.5 to 0.21.6 (#184)

## [0.0.14] - 2026-05-20

### CI, build and tests

- ci: Disable sbom-action release upload to fix permissions error

## [0.0.13] - 2026-05-20

### Security

- Redact sensitive attributes in remote log handler (#135)
- ipmi: Redact sensitive flag values in ipmitool logs and errors (#157)

### Fixes

- bios: BaseManager.Apply returns ErrNotImplemented instead of no-op (#163)
- provision: Wipe target disk before partition-mode image streaming (#161)
- disk: Surface TeardownChrootBindMounts failures to callers (#160)
- executil: Remove PATH env var from exec error strings (#159)
- image: Replace range-over-int retry with conventional loop (#158)
- Replace os.IsExist with errors.Is(err, os.ErrExist) in netlink paths (#156)
- provision: Block semicolon in validateProvisionCommand to prevent command chaining (#155)
- Validate PE/COFF headers for Secure Boot EFI binaries (#141)
- Prefer BMC-specific interfaces for IPMI autodetection (#139)
- Filter incoming EVPN routes by Route Target community (#138)
- Make BGP underlay readiness require configurable minimum peers (#137)
- Parallelize DHCP probing across NICs (#134)
- Make VLAN and bond setup failures fatal during provisioning (#133)
- Track and report dropped log entries in remote handler (#132)
- disk: Propagate DisableLVM errors symmetrically with EnableLVM (#162)

### CI, build and tests

- e2e: Add pure Type-5 per-machine leaf fabric validation (#183)
- kvm: Add provisioning and operational feature KVM tests (#180)
- kvm: Add network stack KVM tests (#179)
- e2e: Comprehensive provisioning matrix: {raw,OCI} × {single,RAID1} (#177)
- provision: Add unit tests for uncovered provisioning steps (#165)
- Add INSECURE_TRANSPORT=true to all E2E test vars files (#154)
- Stabilize E2E tests by increasing BGP wait and limiting parallelism (#153)

### Docs

- Document all undocumented features and env vars in README (#178)

### Dependencies

- Bump golang.org/x/net from 0.53.0 to 0.54.0 (#181)
- Bump golang.org/x/sys from 0.43.0 to 0.44.0 (#182)
- Bump gcc from 15 to 16 (#173)
- Bump actions/upload-artifact from 7.0.0 to 7.0.1 (#167)

## [0.0.12] - 2026-05-13

### Features

- e2e: Add production-realistic topology with VRF, DCGW, and BFD (#176)

### Dependencies

- Bump github.com/klauspost/compress from 1.18.5 to 1.18.6 (#172)

## [0.0.11] - 2026-05-12

### Features

- Add startup crash artifact upload (#175)

### Security

- Redact sensitive patterns in provisioning command logs (#131)

### Fixes

- Reject unimplemented overlay modes during config validation (#136)
- Add TCP-MD5 authentication support for BGP peers (#140)
- Add removable media safety check to DetectDisk (#151)
- Consistent device type check in disk detection (#149)
- Propagate context in configurator for proper cancellation support (#150)
- Add 'p' separator for mmcblk partition device nodes (#148)
- Correct FRR restart interval comment from 20s to 120s (#130)

### CI, build and tests

- Update containerlab from 0.62.2 to 0.75.0 (#170)
- Harden E2E boot tests against CI timing flakes (#147)
- Increase E2E test timeouts for eth1 detection and BGP convergence (#152)
- Add unit tests for NIC detection and IPMI info functions (#142)
- Add E2E coverage for collect-inventory, health-checks, setup-nvme-namespaces (#129)

### Dependencies

- Bump aquasecurity/trivy-action from 0.35.0 to 0.36.0 (#169)
- Bump github.com/google/go-containerregistry from 0.21.4 to 0.21.5 (#168)
- Bump softprops/action-gh-release from 2.6.1 to 3.0.0 (#166)
- Bump orhun/git-cliff-action from 4.7.1 to 4.8.0 (#171)
- Bump sigstore/cosign-installer from 4.1.1 to 4.1.2 (#174)
- Bump oras-project/setup-oras from 1.2.4 to 2.0.0 (#143)
- Bump github.com/google/go-containerregistry from 0.21.3 to 0.21.4 (#144)
- Bump golang.org/x/net from 0.52.0 to 0.53.0 (#146)
- Bump sigstore/cosign-installer from 3.9.1 to 4.1.1 (#128)
- Bump docker/login-action from 4.0.0 to 4.1.0 (#127)

## [0.0.10] - 2026-03-31

### Features

- Add INSECURE_TRANSPORT flag for testing environments (#126)

## [0.0.9] - 2026-03-31

### Security

- Harden transport security and image streaming safety (#119)
- Second wave hardening fixes (#125)

### Fixes

- Fix network retry state and GoBGP teardown symmetry (#120)
- Validate BGP peer-mode neighbor dependencies early (#122)
- Harden CAPRF log handler shutdown behavior (#121)
- Scope linux-only collectors and add non-linux stubs (#123)

### CI, build and tests

- Harden release workflow permissions and pin actions (#124)

## [0.0.8] - 2026-03-30

### Features

- Wire GracefulRestart into GoBGP server startup (#99)
- NVMe namespace management (Phase 1) (#32)
- auth: JWT token manager with acquisition and renewal (#40)
- cloudinit: Add cloud-init generation and NoCloud injection (#42)
- ipmi: Local IPMI management for BMC network, boot order, chassis control (#45)
- Network configuration persistence to target OS (#61)
- luks: LUKS2 disk encryption with crypttab generation (#44)
- Declarative disk partitioning (Phase 1) (#31)
- BGP networking modes with VRF and policy support (#53)
- Webhook event types for provisioning lifecycle (#69)
- Structured Kafka logging with slog handler (#54)
- Add OCI artifact publishing (#90)

### Security

- Security hardening: JWT over HTTP, LLDP log injection, env var redaction (#110)
- Harden HTTP clients and add response body size limits (#95)

### Fixes

- Disable provenance on per-arch container images
- Surface swallowed errors in log shipping, mdev, and rescue mode (#114)
- Lowercase slog message strings per Go conventions (#105)
- Minor nits: virtual disk list, listDir error, checksum leak (#115)
- Disk safety: LVM idempotency, partial wipe errors, root partition detection (#113)
- Rename SecureBoot Valid to PreconditionsMet (#111)
- Wipe partition table on image checksum mismatch (#112)
- Goroutine safety and context propagation (WARN-7, WARN-20) (#116)
- Networking warnings: DHCP gateway, MTU, VRF table ID (WARN-1, WARN-16, WARN-25) (#117)
- EFI bounds check and shell injection in E2E tests (NIT-2, NIT-8) (#118)
- VRF table ID default, bond/static mode idempotency (#108)
- Validate config enums and reject invalid numeric values (#109)
- Guard unimplemented OverlayType, UnderlayAF, and warn on unused PolicyConfig (#107)
- Propagate disk safety errors instead of returning nil (#106)
- Propagate errors from disk secure erase and filesystem check (#97)
- Add bond Miimon monitoring and static mode route cleanup (#98)

### CI, build and tests

- Add bootloader package unit tests and extract parseBootctlOutput (#104)
- CI, test, and code quality improvements (#100)
- Stabilize CodeQL and release provenance settings (#103)
- Raise low-coverage core package baselines (#102)
- Add Go module caching, timeouts, and optimize job dependencies (#94)
- Replace fake E2E image with real GPT disk image (#93)

### Docs

- Align architecture and e2e/testing references (#101)

### Maintenance

- Unify Commander interface in firmware/nic package (#96)

## [0.0.7] - 2026-03-27

### Features

- Add EVPN Type-2/3 route origination for L2 overlay (#89)

### Fixes

- Initramfs module loading, netplan/FRR parser, and debug diagnostics (#92)

### CI, build and tests

- Migrate ARM64 builds from cross-compilation to native runners (#91)

## [0.0.6] - 2026-03-23

### Features

- Add ISO builds and OCI publishing to release pipeline

## [0.0.5] - 2026-03-23

### Fixes

- Disable buildx provenance to produce simple per-arch images

## [0.0.4] - 2026-03-23

Tag only, no GitHub release.

### Fixes

- Add arch assertion steps and explicit CC=gcc
- Add explicit --platform to docker buildx for deterministic builds

## [0.0.3] - 2026-03-23

Tag only, no GitHub release.

### Fixes

- Enumerate all arch+flavor+runner matrix combinations

### CI, build and tests

- Migrate ARM64 builds from cross-compilation to native runners

## [0.0.2] - 2026-03-22

### Breaking changes

- The legacy (non-CAPRF) provisioning mode was removed (#84)

### Features

- Rescue mode for failed provisioning recovery (#62)
- provision: Resilient provisioning with retry, checkpoint, and error taxonomy (#41)
- Consolidated hardware abstraction, observability, and boot management (#71)
- VLAN configuration and trunk support (#63)
- Add NIC firmware management framework (#47)
- Add advanced OCI image management (#51)
- Add extended hardware inventory collection (#50)
- Image signature verification with checksum streaming (#60)
- Binary optimization with build info and size analysis (#58)
- drivers: Kernel module management with PCI ID auto-detection (#46)
- ARM64 cross-compilation support (Phase 1) (#33)
- Dry-run mode for non-destructive provisioning validation (#30)
- Release automation: nightly, release-v2, promote, changelog (#67)
- standby: Add command acknowledgement and health-check (#38)
- CI testing matrix: KVM scenarios + ContainerLab topologies (#68)

### Security

- Codebase audit P0/P1 - security, bugs, and test coverage (#23)
- Codebase audit P2/P3 - bugs, security, error handling, docs (#24)

### Fixes

- gobgp: Implement EVPN route processing and data plane E2E tests (#85)
- gobgp: Install kernel route to gateway VTEP for VXLAN data plane
- Eliminate E2E test skips and fix CI failure

### CI, build and tests

- e2e: Make BUM FDB and efivarfs mount non-fatal (#87)
- e2e: Add ISO boot tests for headless q35 VMs (#73)
- ci: Upgrade trivy-action to 0.35.0, lowercase GHCR image names
- e2e: Remove BFD profile section from FRR configs for 10.3.1 compat

### Docs

- Add review personas, testing mandate, and documentation completeness checks (#70)
- Update proposal statuses to reflect implementations (#35)

### Dependencies

- Bump orhun/git-cliff-action from 3 to 4 (#80)
- Bump golang.org/x/net from 0.48.0 to 0.52.0 (#78)
- Bump github.com/klauspost/compress from 1.18.4 to 1.18.5 (#82)
- Bump github.com/google/go-containerregistry from 0.21.2 to 0.21.3 (#76)
- Bump actions/download-artifact from 4 to 8 (#74)
- Bump aquasecurity/trivy-action from 0.28.0 to 0.35.0 (#75)
- Bump docker/setup-qemu-action from 3 to 4 (#77)
- Bump google.golang.org/protobuf from 1.36.10 to 1.36.11 (#79)
- Bump docker/login-action from 3 to 4 (#81)
- Bump google.golang.org/grpc from 1.56.3 to 1.79.3 (#72)

### Maintenance

- Eliminate code duplication and fix review findings (#88)
- Remove legacy provisioning mode (CAPRF-only) (#84)

## [0.0.1] - 2026-03-14

First release: CAPRF provisioning agent with FRR/EVPN and GoBGP networking.

### Features

- inventory: Add DIMM-level memory enumeration via DMI
- Add GoBGP/EVPN network mode (#21)
- Add pre-provisioning health check framework (#17)
- Firmware version reporting and validation (#19)
- Add hardware inventory collection and reporting (#18)
- Add IEEE 802.1Q VLAN support (#20)
- e2e: Robust provisioning tests with disk, chroot & error detection (#16)
- CAPRF provisioning integration: single-binary ramdisk agent with FRR/EVPN networking (#2)

### CI, build and tests

- release: Pass dummy command to docker create for scratch images
- release: Use docker cp for scratch images, add ISO and all flavour builds
- ci: Relax UEFI boot validation, add version/build ldflags vars

### Dependencies

- Bump actions/checkout from 4 to 6 (#15)
- Bump gcc from 14 to 15 (#6)
- Bump golang.org/x/sys from 0.41.0 to 0.42.0 (#10)
- Bump docker/login-action from 3 to 4 (#14)
- Bump golangci/golangci-lint-action from 7 to 9 (#7)
- Bump alpine from 3.20 to 3.23 in /test/e2e/clab (#8)
- Bump actions/upload-artifact from 4 to 7 (#9)
- Bump docker/setup-buildx-action from 3 to 4 (#11)
- Bump actions/setup-go from 5 to 6 (#13)
- Bump github.com/vishvananda/netlink from 1.3.0 to 1.3.1 (#12)

### Maintenance

- gobgp: Extract constants, add tests, update README (#22)
- Modernize CI, testing, and codebase (#1)

[Unreleased]: https://github.com/telekom/BOOTy/compare/v0.2.0-rc.15...HEAD
[0.2.0-rc.15]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.15
[0.2.0-rc.14]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.14
[0.2.0-rc.13]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.13
[0.2.0-rc.12]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.12
[0.2.0-rc.11]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.11
[0.2.0-rc.10]: https://github.com/telekom/BOOTy/tree/v0.2.0-rc.10
[0.2.0-rc.9]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.9
[0.2.0-rc.8]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.8
[0.2.0-rc.7]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.7
[0.2.0-rc.6]: https://github.com/telekom/BOOTy/tree/v0.2.0-rc.6
[0.2.0-rc.5]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.5
[0.2.0-rc.4]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.4
[0.2.0-rc.3]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.3
[0.2.0-rc.2]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.2
[0.2.0-rc.1]: https://github.com/telekom/BOOTy/releases/tag/v0.2.0-rc.1
[0.1.0]: https://github.com/telekom/BOOTy/releases/tag/v0.1.0
[0.0.22]: https://github.com/telekom/BOOTy/releases/tag/v0.0.22
[0.0.21]: https://github.com/telekom/BOOTy/tree/v0.0.21
[0.0.20]: https://github.com/telekom/BOOTy/tree/v0.0.20
[0.0.19]: https://github.com/telekom/BOOTy/releases/tag/v0.0.19
[0.0.18]: https://github.com/telekom/BOOTy/releases/tag/v0.0.18
[0.0.17]: https://github.com/telekom/BOOTy/releases/tag/v0.0.17
[0.0.16]: https://github.com/telekom/BOOTy/releases/tag/v0.0.16
[0.0.15]: https://github.com/telekom/BOOTy/releases/tag/v0.0.15
[0.0.14]: https://github.com/telekom/BOOTy/releases/tag/v0.0.14
[0.0.13]: https://github.com/telekom/BOOTy/releases/tag/v0.0.13
[0.0.12]: https://github.com/telekom/BOOTy/releases/tag/v0.0.12
[0.0.11]: https://github.com/telekom/BOOTy/releases/tag/v0.0.11
[0.0.10]: https://github.com/telekom/BOOTy/releases/tag/v0.0.10
[0.0.9]: https://github.com/telekom/BOOTy/releases/tag/v0.0.9
[0.0.8]: https://github.com/telekom/BOOTy/releases/tag/v0.0.8
[0.0.7]: https://github.com/telekom/BOOTy/releases/tag/v0.0.7
[0.0.6]: https://github.com/telekom/BOOTy/releases/tag/v0.0.6
[0.0.5]: https://github.com/telekom/BOOTy/releases/tag/v0.0.5
[0.0.4]: https://github.com/telekom/BOOTy/tree/v0.0.4
[0.0.3]: https://github.com/telekom/BOOTy/tree/v0.0.3
[0.0.2]: https://github.com/telekom/BOOTy/releases/tag/v0.0.2
[0.0.1]: https://github.com/telekom/BOOTy/releases/tag/v0.0.1
