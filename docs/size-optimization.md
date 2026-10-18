# Binary & Image Size Optimization

## Boot time

Boot time is the time from the BMC virtual media insert to "network
ready": the BMC streams the boot loader, the kernel and the initramfs,
the kernel decompresses itself and unpacks the initramfs, and BOOTy loads
the kernel modules and brings up the network. The size work below serves
this goal; CPU use may grow, build time does not matter, and no function
may be lost.

### Model

```
T(B) = S / B + D + M + N
```

- `S`: bytes that the firmware reads from the virtual media: the boot
  loader, the kernel and the initramfs. UEFI reads shim (1,036,152
  bytes) and GRUB (2,684,352) from the ESP; BIOS reads ISOLINUX
  (157,636). The ESP does not hold the kernel or the initramfs, so
  nothing is read twice.
- `B`: virtual media throughput. **Assumption, not measured:** 2 to
  10 MB/s, lower on BMCs with a 100 Mbit/s port (iDRAC up to 13G) or
  with encrypted virtual media. No BMC in this repository's tests was
  measured; verify per BMC model before using the absolute numbers below.
- `D`: in-kernel decompression of the initramfs, single-threaded.
- `M`: module phase, `Run /init` to `starting BOOTy`.
- `N`: network phase, `starting BOOTy` to `network connectivity
  established`.

**Assumption, not measured:** the model above holds for BMCs that map the
image from a network share (HTTP, NFS or CIFS) and fetch only the blocks
that the firmware reads, so other ISO files and padding do not count.
It is false for BMCs that first copy the whole ISO, for example Lenovo
XCC "mount from web" (the browser uploads the image to the BMC). For
those:

```
T_upload(B_up) = I / B_up + S / B_local + D + M + N
```

- `I`: the whole ISO (default PR ISO about 64 MiB), uploaded at `B_up`
  before the firmware can read anything. Every ISO byte counts, the BIOS
  path, the ESP copy and padding included.
- `B_local`: the read from the BMC's own storage, much faster than `B_up`
  (not measured).

With an upload the ISO size, not only `S`, sets the boot time: the
initramfs savings below count in both models, a smaller boot loader only
in the first.

Firmware POST and the kernel's own init do not depend on BOOTy and are
left out. The Debian kernel is signed for Secure Boot, so its image
(12,154,816 bytes, zstd inside the bzImage) stays as it is.

### Codec measurements

The default tree (188.5 MB raw amd64, 171.7 MB arm64). Sizes from the
amd64 tree; in-kernel unpack time from the arm64 tree on the Debian 6.12
kernel under QEMU with HVF on an Apple M2 (3 boots each, median). A
server core is about half as fast. All codecs are built into the kernel
(`CONFIG_RD_*=y`, `CONFIG_XZ_DEC_X86=y`).

| Codec | default initramfs | in-kernel unpack (M2) |
|---|---:|---:|
| zstd -19 --long (before #757) | 47,639,831 | 0.256 s |
| zstd --ultra -22 --long, files grouped (#757) | 46,663,569 | 0.264 s |
| xz -9e, x86 BCJ, 64 MiB dictionary | 42,429,236 | 2.51 s |
| xz -9e | 43,363,472 | 2.39 s |
| gzip -9 | 61,560,557 | 0.50 s |
| lz4 -l -12 | 74,938,002 | 0.11 s |

xz saves 5.2 MB but costs about 4.5 s more on a server core: it is
faster only below about 1.2 MB/s. zstd stays. `--long=28` would save
another 0.36 MB but needs a 256 MiB window in early boot; not done.

### Measured runtime phases

arm64 default flavor of `main`, QEMU `virt` with HVF, 4 vCPUs, one
virtio NIC, static network vars:

| Phase | before | after | PR |
|---|---:|---:|---|
| `M` module phase | 2.48 s | 0.91 s | #758 |
| console output during `M` | 48,804 bytes | 5,699 bytes | #758 |
| `N` network phase (one NIC) | 3.15 s | 0.12 s | #760 |
| disk settle in `detect-disk` / `select-deprovision-disk` | 2.03 s | 0.004 s | #762 |

x86 KVM in CI (GitHub runners), same changes:

| Measurement | main | with the PR | PR |
|---|---:|---:|---|
| `agent-cycle-timing` `network-bring-up`, agent boot | 3.50 s | 0.75 s | #760 |
| `agent-cycle-timing` `detect-disk` and `select-deprovision-disk` | 2.01 s each | 2.01 s each (automatic selection and wipe of all disks keep the full window) | #762 |

Whole CI test times are not used: they vary from 44 to 60 s across PRs
that do not touch the measured code.

The disk settle credit of #762 applies only when the selection waits for
a named disk (`DISK_DEVICE`, `DISK_SERIAL_NUMBER`): an HBA with an
asynchronous SCSI scan can present disks late, so the wipe of all disks,
automatic selection and mirroring keep the full 2 s window. The arm64 row
above names its disk.

QEMU does not rate-limit its UART. A BMC serial-over-LAN UART at 115200
baud takes about 87 us per byte. #758 removes about 27.5 KB of console
output in `M` on x86 (43 KB on arm64), so it saves about 2.4 s more on an
x86 server.

### End to end

Default flavor, UEFI, `S` = shim + GRUB + kernel + initramfs, `D + M + N`
as measured above (VM, one NIC; 5.89 s before, 1.29 s after):

| | S | 2 MB/s | 5 MB/s | 10 MB/s |
|---|---:|---:|---:|---:|
| before | 63,515,151 | 37.7 s | 18.6 s | 12.2 s |
| after #757, #758, #759, #760 | 60,515,139 | 31.6 s | 13.4 s | 7.3 s |
| saved | 3,000,012 | 6.1 s (16%) | 5.2 s (28%) | 4.9 s (40%) |

The absolute numbers rest on the assumed `B` and on a BMC that reads
only the blocks the firmware needs (see Model). On x86 hardware add about
2.4 s for the console output. The disk settle saves up to 2 s per
provision or deprovision that names its disk, after "network ready".

### Candidates

| Candidate | Tried | Gain | Decision |
|---|---|---|---|
| lldpd without the SNMP subagent | yes | default 2.28 MB, gobgp 2.09 MB | done, #759 |
| zstd --ultra -22 --long, files grouped by kind | yes | default 0.98 MB, gobgp 0.52 MB, slim 0.21 MB | done, #757 |
| Drop `curl` (BOOTy uses net/http; busybox `wget` in the shell) | yes | default 1.95 MB, gobgp 1.95 MB, slim 6.13 MB | done, #814 |
| Module load in dependency order, in parallel, one log line | yes | 1.6 s, 27.5 KB console on x86 (43 KB arm64) | done, #758 |
| LLDP receive NIC detection in the background | yes | 3.0 s on single-NIC hosts | done, #760 |
| Disk settle window counted from boot | yes | up to 2 s per run that names its disk | done, #762 (only for a named disk) |
| xz or lz4 instead of zstd | measured | see codecs | rejected: slower above 1.2 MB/s |
| Recompress or trim the kernel | no | | rejected: the Debian signature covers the image (Secure Boot), and all ISOs boot with Secure Boot |
| Strip or compress kernel modules (9.0 MB of the default ramdisk) | no | | rejected: the module signature covers the file; a compressed module inside the compressed ramdisk is larger |
| Drop `lldpcli -f xml` (`libxml2`) | measured | 0.75 MB | superseded: lldpd and lldpcli are dropped, see below |
| Drop lldpd and lldpcli (`booty lldp` diagnoses LLDP, #735) | yes | default 1.02 MB, gobgp 1.01 MB | done, #812 |
| ipmitool without FreeIPMI | measured | 1.15 MB | rejected: loses the `-I free` interface |
| Drop `vtysh` | measured | 2.05 MB | rejected: FRR mode and its diagnostics use it |
| Merge `booty-esp-sync` into `init` | measured | 0.62 MB | rejected: it is installed on the target as its own binary |
| Duplicate files (`ipmi_msghandler`, `ipmi_devintf` in two module dirs) | measured | under 1 KB after compression | not worth it |
| Kernel `quiet` or `loglevel=5` on the ISO | estimated | 2 to 8 s of console output on hardware | not done: hides kernel messages that operators and the ISO tests read (Secure Boot state) |
| GRUB `timeout=0` (UEFI) | no | 1 s | not done: removes the chance to edit the kernel command line |
| Faster first retries of the connectivity check (1 s ticker) | no | about 0.4 s on average when the route comes up late | open |
| DHCP single-NIC settle (3 s, same NIC wait as #760) | no | 3 s for single-NIC DHCP hosts | open: the wait guards the containerlab fabric |
| Optional payloads as a second initrd | no | FRR 4.2 MB, qemu-img 2.3 MB, ipmitool 1.3 MB | open, see proposal-artifact-size.md |

The CI size budgets (`test/e2e/build_flavours_e2e_test.go`,
`.github/workflows/pr-artifacts.yml`) are set about 5% above `main` so a
regression fails early.


## Benchmark Results

Measured on Go 1.26.1 / linux/amd64, UPX 4.2.2.  
All builds use `CGO_ENABLED=1 -linkmode external -extldflags '-static' -s -w` unless noted.

| Variant | Binary (bytes) | Binary (MB) | vs baseline |
|---|---|---|---|
| Baseline (`-s -w -static`) | 23,555,824 | 22.5 MB | — |
| **+ `-trimpath`** ✅ implemented | 23,506,448 | 22.4 MB | −0.2% |
| CGO=0 + `-trimpath` (micro target) | 22,290,594 | 21.3 MB | −5.4% |
| Baseline + UPX `--best` (LZMA) | 9,030,604 | 8.6 MB | −61.7% |
| Baseline + UPX `-9` | 9,121,436 | 8.7 MB | −61.3% |
| CGO=0 + UPX `-9` | 8,284,624 | 7.9 MB | −64.8% |

> Note: the binary is packed inside a `cpio.gz` initramfs. The net gain per
> optimization depends on how compressible the binary is relative to the rest
> of the initramfs contents (FRR binaries, tool binaries, kernel modules).
> `-trimpath` removes host build-system paths from DWARF/debug info — since
> `-s -w` already strips that section, the on-disk effect is small (~49 KB),
> but it improves **reproducible builds** and removes local path leakage from
> the binary.

## Implemented (non-breaking)

### `-trimpath` flag (`-0.2%` binary, reproducible builds)

Added to every `go build` invocation:

- `Makefile` `LDFLAGS`
- `initrd.Dockerfile` — `dev` stage (CGO=1) and `micro-dev` stage (CGO=0)
- `.github/workflows/ci.yml` — all three build steps
- `.github/workflows/release-v2.yml` — release binary build

`-trimpath` removes the absolute build-system paths embedded in the Go
binary's file-name table. With `-s -w` already stripping the full symbol/DWARF
sections the size saving is minor, but it is a best-practice for:
- **Reproducible builds** — output is identical regardless of `$GOPATH` / workspace location
- **No path leakage** — runner home paths (`/home/runner/…`) are not embedded in release artifacts

## Not Implemented (breaking / risky / needs validation)

### UPX compression (removed)

The initramfs builds used to UPX-pack `init`. That made the ramdisk
larger: the packed binary does not compress further, and `zstd -19 --long`
compresses the plain Go binary better than UPX does. Measured on linux/amd64
(default flavor): 55,601,291 bytes with UPX, 53,323,769 bytes without. The
UPX stub also unpacks `init` into anonymous memory at boot, so the ramdisk
held the 10 MB packed file and a 25.7 MB unpacked copy.

The UPX loops for the tool binaries never ran: `upx` came from the Alpine
build stage and needs the musl loader, which the Debian builder stages do
not have. See [proposal-artifact-size.md](proposal-artifact-size.md) for the
current measurements and the remaining ideas.

### `xz` / `zstd` initramfs compression

The default, slim and gobgp ramdisks use `zstd -19 --long`; micro uses
gzip. `xz` is about 10% smaller but decompresses about 5 times slower. See
[proposal-artifact-size.md](proposal-artifact-size.md#2-opt-in-xz-compression-for-slow-delivery-paths).

### Dead-code / profile-guided optimization (PGO)

Go 1.21+ supports PGO via `-pgo=profile.pprof`. A representative CPU profile
collected during a real boot would allow the compiler to inline hot paths and
de-prioritize cold code. Estimated saving: 3–8% binary size, plus throughput
improvement. Requires instrumenting a boot run to collect a profile first.

### Stripping tool binaries with `strip -s`

Implemented: the tools stage runs `strip --strip-all` on every tool binary
after it collects their libraries with `ldd`.
