# Image Delivery: Throughput, Resume, Fleet Distribution and Air Gap

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1 for resume and throughput. P3 for multicast and P2P. P2 for air gap.

## Summary

Make the image write faster and more robust, and make fleet installs scale.
The proposal covers parallel ranged downloads, multi-threaded zstd
decompression, sparse writes, resumable downloads with HTTP `Range`, a
content-defined chunk cache for repeated installs, multicast and P2P
distribution for large rollouts, qcow2 without a full tmpfs copy, and fully
air-gapped operation.

## Problem and motivation

- `image.Stream` uses one HTTP connection, one decoder thread
  (`zstd.WithDecoderConcurrency(1)` in `pkg/image/format.go`) and a 16 MiB
  copy buffer. Fast NVMe disks and 25 or 100 Gbit links are idle most of the
  time (verify with benchmarks first).
- `httpGetWithRetry` retries only until the response starts. A connection
  break mid-body fails the step, and a retry starts again at byte zero
  (`pkg/image/stream.go`). For a 20 GiB image on a lossy path this is costly.
- BOOTy writes every byte, also long zero runs. Raw images are often mostly
  empty.
- qcow2 images are copied to a tmpfs and converted with `qemu-img`
  (`pkg/image/qcow2.go`). The host needs RAM larger than the image.
- A rack-wide reinstall pulls the same image N times from one server.
- Some sites have no internet and strict egress rules. Every external
  dependency must have a local mirror.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Source selection | Probe each URL with a ranged GET, use the fastest. | `pkg/image/select.go` (`SelectBestSource`) |
| Transfer | One GET, retry before the body, idle timeout on the body. | `pkg/image/stream.go` (`httpGetWithRetry`, `idleTimeoutBody`) |
| Decode | gzip, zstd (1 thread), lz4, xz, bzip2. | `pkg/image/format.go` |
| Checksum | Download or decompressed scope, verified at EOF. Failed writes wipe the start of the target. | `pkg/image/checksum_scope.go`, `pkg/image/stream.go` |
| qcow2 | tmpfs, `qemu-img convert`, then raw copy. | `pkg/image/qcow2.go` |
| OCI | One layer, `go-containerregistry`, keychain auth. | `pkg/image/oci.go` |
| Resume | `BOOTY_RESUME` checkpoints skip completed steps. Not within a step. | `pkg/provision/checkpoint.go` |
| Retries | Per-step retry policy. | `pkg/provision/retry.go` |
| Proposals | HTTP Range resume is a later phase of [proposal-resilient-provisioning.md](proposal-resilient-provisioning.md). OCI caching and deltas are in [proposal-oci-advanced.md](proposal-oci-advanced.md). | |

## Goals

- Saturate a 25 Gbit link and a fast NVMe disk with zstd images (target to be
  set by the benchmarks in [proposal-testing-strategy.md](proposal-testing-strategy.md)).
- Resume a broken transfer at the last verified offset.
- Skip zero blocks on write when the target is known to read back zeros.
- Stream qcow2 without a full RAM copy.
- Provide an optional fleet distribution mode.
- Run with no internet access.

## Non-goals

- A new image format. Use existing formats.
- Building images.

## Design options and recommendations

### 1. Throughput

- **Parallel ranged download.** Split the compressed object into ranges and
  fetch K ranges in parallel into a bounded ring of buffers. Feed the decoder
  in order. Needs `Accept-Ranges: bytes` and a known length. Fall back to one
  stream otherwise.
- **Multi-threaded zstd.** Raise decoder concurrency (`klauspost/compress`
  is already a dependency). For true parallel decode, publish images in the
  zstd seekable format (verify tool support), so ranges decode
  independently.
- **Write path.** Use a larger aligned buffer and `O_DIRECT` where the device
  supports it, with a bounded number of in-flight writes. Measure first; page
  cache writes plus `fsync` can be as fast.
- Recommendation: start with decoder concurrency and a benchmark. Add parallel
  ranges only when the benchmark shows the network as the bottleneck.

### 2. Resumable downloads

- Track the number of compressed bytes consumed and the hash state.
- On a body error, re-request with `Range: bytes=<offset>-` and an
  `If-Range` header with the ETag or Last-Modified value. Continue the hash
  and the decoder.
- Go `hash.Hash` for SHA-256 supports `encoding.BinaryMarshaler`, so the hash
  state survives (verify for the stdlib implementation in use). The
  decompressor state does not survive a process restart. Resume therefore
  works within one process, which covers network breaks. Across a reboot,
  restart from zero or from the last zstd frame boundary with seekable zstd.
- OCI blobs support ranged GETs on most registries (verify for the registries
  in use).
- Recommendation: in-process resume first. It fits the existing retry
  classification.

### 3. Sparse writes

- After `wipe-disks` with `blkdiscard` on a device that reports
  `discard_zeroes_data` or a successful `BLKZEROOUT`, BOOTy can skip writing
  all-zero blocks. Track skipped ranges and verify the checksum over the full
  stream, as today.
- Only enable when the wipe proves zeroing. Otherwise old data remains in
  skipped blocks.

### 4. qcow2 without tmpfs

- Option A: `qemu-img convert` from a FUSE or NBD source: still needs tools.
- Option B: stream-convert in Go. A qcow2 file can be converted in one pass
  only if clusters appear in order (verify; images from `qemu-img convert -c`
  are usually ordered). Fall back to tmpfs otherwise.
- Option C: require raw or zstd images in the pipeline.
- Recommendation: Option C as policy, Option B as an optimization with a
  strict fallback.

### 5. Chunk cache and dedup

- Publish a chunk index (casync, desync or a simple content-defined chunk
  list) next to the image (verify desync licence and format stability).
- BOOTy fetches only chunks that are not already present on the target disk
  (for A/B upgrades, the active slot has most chunks) or on a local cache
  partition.
- Recommendation: use this for A/B upgrades first, where the active slot is a
  free seed. This overlaps with OCI delta updates in
  [proposal-oci-advanced.md](proposal-oci-advanced.md); pick one mechanism.

### 6. Multicast and P2P

| Option | Fit | Notes |
|--------|-----|-------|
| UDP multicast (`uftp`, `udpcast`, verify licences) | One rack, one L2 domain | EVPN fabrics may not carry multicast to the provisioning VLAN. Needs FEC. |
| BitTorrent-style P2P | Any routed fabric | Hosts must reach each other. Security: peers serve data to peers; the checksum protects integrity. |
| OCI P2P (Dragonfly, Kraken, Spegel, verify) | OCI sources | Spegel needs a cluster; BOOTy runs before the cluster. Dragonfly has a dfget client. |
| Rack-local cache server | Any | A caching HTTP proxy per rack. Simple. |

Recommendation: a rack-local HTTP cache first (no BOOTy change; CAPRF ranks
it first in `IMAGE`). P2P only if the cache is not enough.

### 7. Air-gapped operation

- Every input has a local source: OS images and sysexts (local HTTP or OCI
  mirror), BOOTy artifacts (OCI mirror), DNS (local resolvers), time (the
  NTP servers in the site network (`NTP_SERVERS`) or the CAPRF `Date`
  source of the time sync from #713; see `TOKEN_EXPIRES_AT` notes
  in [caprf-integration.md](caprf-integration.md)), certificates (private CA
  in the ramdisk or the seed).
- Add an offline bundle format: a single OCI artifact with the BOOTy initramfs,
  kernel, the image, sysexts and a manifest. CAPRF mirrors it with `oras`.
- Add registry mirror config for `oci://` sources (`OCI_REGISTRY_MIRRORS`),
  like containerd mirror config.
- A CI job that runs the KVM matrix with no default route except to the test
  servers proves that nothing reaches out.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `IMAGE_PARALLEL_RANGES` | integer, default `1` | Parallel ranged GETs. |
| `IMAGE_DECODER_THREADS` | integer, default `0` (auto) | zstd decoder concurrency. |
| `IMAGE_RESUME` | boolean, default `true` | In-process Range resume. |
| `IMAGE_SPARSE_WRITE` | `off`, `auto` | `auto` only after a proven zeroing wipe. |
| `IMAGE_CHUNK_INDEX` | URL | Optional chunk index for dedup. |
| `OCI_REGISTRY_MIRRORS` | JSON map of registry to mirror list | Air gap. |

### CAPRF and CAPI API impact

- `RedfishMachine.spec.imageSources` already allows several URLs. Add a
  priority or failure-domain hint so CAPRF can put the rack cache first.
- CAPRF reports throughput from events (see the CAPI proposal).

## Security considerations

- Resume with `If-Range` prevents mixing two versions of an object.
- The checksum must still cover the full stream. Sparse writes and resume
  must not skip bytes in the hash.
- P2P peers are untrusted. Verify every chunk against a signed index, and the
  whole image against the pinned checksum.
- Multicast is unauthenticated. Same rule: verify every chunk.
- Air-gapped mirrors must keep signatures (cosign, GPG) with the artifacts.

## Testing strategy

- Unit: resume with the `faultproxy` helper (cut at N bytes, ETag change).
  Parallel range ordering. Sparse write skip logic with a zeroing fake.
  qcow2 ordered and unordered fixtures.
- KVM matrix: `fault-net-drop` with resume. A benchmark scenario with a large
  zstd image (nightly). `airgap` scenario with no default route.
- ContainerLab: stream over the EVPN fabric with netem loss on the leaf link.
- kindmetal and CAPRF e2e: rack cache first in `IMAGE`, fallback on cache
  failure.
- Real hardware: throughput on 25 and 100 Gbit hosts with NVMe.

## Phased rollout

1. PR: benchmarks (from the testing proposal) to set the baseline.
2. PR: zstd decoder concurrency.
3. PR: in-process Range resume.
4. PR: sparse writes behind `IMAGE_SPARSE_WRITE=auto`.
5. PR: parallel ranges if the benchmark justifies it.
6. PR: `OCI_REGISTRY_MIRRORS` and the air-gap KVM scenario.
7. Docs PR: rack cache deployment guide.
8. Later: chunk index dedup for A/B, P2P evaluation.

## Effort estimate

| Phase | Days |
|-------|------|
| Decoder concurrency | 1 to 2 |
| Range resume | 4 to 6 |
| Sparse writes | 3 to 4 |
| Parallel ranges | 5 to 7 |
| Registry mirrors and air-gap test | 3 to 5 |
| qcow2 streaming | 4 to 6 |
| Chunk dedup | 8 to 12 |
| P2P evaluation | 5 to 8 |
| **Total** | **33 to 50** |

## Open questions

- What is the current bottleneck on production hardware: network, decoder or
  disk?
- Do the image servers and registries in use support `Range` and stable
  ETags?
- Is multicast available in any provisioning network?

## Related proposals

- [proposal-resilient-provisioning.md](proposal-resilient-provisioning.md)
- [proposal-oci-advanced.md](proposal-oci-advanced.md)
- [proposal-image-signatures.md](proposal-image-signatures.md)
- [proposal-testing-strategy.md](proposal-testing-strategy.md)
