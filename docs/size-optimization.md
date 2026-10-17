# Binary & Image Size Optimization

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
