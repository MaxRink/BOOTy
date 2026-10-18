# Proposal: Smaller Ramdisk Artifacts

## Status: Proposed

## Priority: P2

## Context

The build changes in `build/size-optimizations` remove UPX, prune shared
libraries that no shipped executable loads, and build with `grpcnotrace`.
They keep every file that an executable needs. This proposal lists the
next steps. Each one changes a tool build, a package selection or the boot
flow, so each needs its own review.

Measured on linux/amd64 from `main` at `af7dfed8` plus the build changes
(bytes after `zstd -19 --long`, default flavor 53,072,840 bytes):

| Component in the default ramdisk | Raw | zstd -19 --long |
|---|---:|---:|
| `init` (BOOTy) | 25,341,968 | 7,807,073 |
| lldpd SNMP agent and lldpcli XML libraries (ICU, libxml2, net-snmp, perl) | 41,997,400 | 12,136,680 |
| Kernel modules | 32,977,164 | 6,479,743 |
| FRR (bgpd, zebra, bfdd, vtysh, watchfrr, libfrr) | 26,752,616 | 4,245,549 |
| qemu-img with glib and gnutls | 7,037,160 | 2,303,788 |
| ipmitool with libfreeipmi | 6,444,512 | 1,286,081 |
| EFI fallback loader | 3,338,240 | 750,381 |

## 1. Build lldpd without SNMP (done, #759)

Debian builds lldpd with `--with-snmp`. The SNMP subagent needs a running
`snmpd` master agent, which the ramdisk does not ship, so that code could
not work there. The `lldpd` stage of `initrd.Dockerfile` builds the Debian
source package without `--with-snmp` and the tools stage installs that
package. The ramdisk loses net-snmp, `libperl`, `libsensors`, `libpci` and
`libwrap`: 2,281,110 bytes of the zstd default ramdisk and 2,093,792 bytes
of gobgp (slim ships no lldpd).

XML output stays (`lldpcli -f xml`): on trixie `libxml2` no longer pulls
in ICU, and it costs 0.75 MB compressed, so dropping it would lose a
function for little gain.

## 2. Opt-in xz compression for slow delivery paths

`xz -9e --check=crc32` makes the default ramdisk 5,369,280 bytes (10.1%)
smaller than `zstd -19 --long`, but it decompresses 5.4 times slower
(2.27 s against 0.42 s single-threaded on an Apple M-series host; 14 to
26 s against about 5 s in QEMU TCG). The Debian kernel has
`CONFIG_RD_XZ=y`, so both work. Keep zstd as the default and offer xz only
as a build argument for virtual-media or WAN boot paths where transfer
time dominates.

| Codec | Default ramdisk | Decompression (native, 1 thread) |
|---|---:|---:|
| zstd -19 --long (current) | 53,072,840 | 0.42 s |
| zstd --ultra -22 --long | 52,341,417 | 0.35 s |
| xz -9e --check=crc32 | 47,703,560 | 2.27 s |
| lz4 -l -12 | 83,191,425 | 0.12 s |

`--ultra -22` saves 1.4% for twice the compression time per flavor (86 s
against 40 s natively, more for arm64 under QEMU in CI). It is not worth
the CI time now.

## 3. Load optional payloads as a second initrd

The Linux kernel unpacks several concatenated cpio archives. FRR (4.2 MB),
qemu-img (2.3 MB) and ipmitool (1.3 MB) are used only for some
configurations. A bootloader or iPXE script could load a base archive plus
an optional archive per feature, so a DHCP-only deployment downloads less.
This changes how operators deliver the ramdisk, so it needs a design for
the ISO, PXE and CAPRF paths first.

## 4. Release binary build flags

The `grpcnotrace` tag is in `initrd.Dockerfile` and the `Makefile`. The
standalone release and nightly binaries build in
`.github/workflows/release-v2.yml` and `nightly.yml`, which
`deps/gobgp-v4` (#699) also changes. Add the tag there after #699 merges.

## 5. Firmware

The ramdisk ships no `/lib/firmware` today. If firmware is added (for
example the `ice` DDP package), ship only the files of the supported
drivers and store them uncompressed: the ramdisk is compressed as a whole,
and a second compression layer makes it larger.

## Rejected

| Idea | Reason |
|---|---|
| UPX on `init` or tools | The packed file does not compress further, so the ramdisk grows. The stub also unpacks into anonymous memory at every exec. |
| Compressed kernel modules | BOOTy calls `finit_module` without `MODULE_INIT_COMPRESSED_FILE`, and double compression makes the ramdisk larger. #701 already decompresses the trixie `.ko.xz` files. |
| Strip debug sections | Tools are built or copied with `strip --strip-all`. Debian libraries, FRR binaries and modules have no debug sections; modules need their symbol table to load. |
| Separate Go debug file | `-s -w` removes DWARF and the symbol table only. The Go runtime keeps `pclntab`, so panics and crash artifacts still show function names and lines. |
| Build tags that remove GoBGP or gopacket | GoBGP is a runtime network mode in every flavor. gopacket serves LLDP and DHCP. Its 8 MB of tables are BSS (memory), not file size. |
| Busybox applets for disk tools | BOOTy uses util-linux and GNU options (for example `lsblk --json --bytes --paths`, `sfdisk --json`). The Dockerfile tests remove busybox collisions on purpose. |
| Static tool binaries | Each static binary carries its own libc. The shared library set is smaller. |
| ISO layout | The ISO holds one kernel, one initrd and ISOLINUX. It has no EFI image to deduplicate. |
