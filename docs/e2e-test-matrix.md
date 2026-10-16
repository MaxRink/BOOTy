# BOOTy End-to-End Test Matrix

This document describes the comprehensive E2E test coverage for BOOTy
provisioning, with a focus on the **full product matrix** of image source ×
disk layout exercised by [`test/e2e/full_provisioning_matrix_e2e_test.go`](../test/e2e/full_provisioning_matrix_e2e_test.go).

## Why a matrix?

In production (telekom t-co / CAPRF environment), BOOTy is deployed across
fleets that combine two orthogonal choices:

| Axis              | Variants                                          |
|-------------------|---------------------------------------------------|
| **Image source**  | Raw HTTP (gzipped/raw/xz/lz4/zstd), OCI registry  |
| **Disk layout**   | Single disk (`/dev/sda`), RAID1 mirror (`/dev/md0` over `/dev/sda`+`/dev/sdb`) |

Every customer-impacting bug in past releases has involved at least one of
these axes (image checksum mismatch on gzipped HTTP, OCI auth regression,
mdadm `--create` argument ordering, RAID metadata not zeroed on re-provision,
…). PR and default-branch CI exercise every matrix cell. The release workflow
builds, scans, signs, and uploads release artifacts from the release ref; it does
not by itself rerun the generic E2E matrix unless a separate CI run is also
triggered for that ref.

## Cells covered

`TestFullProvisioningMatrix` (in [`test/e2e/full_provisioning_matrix_e2e_test.go`](../test/e2e/full_provisioning_matrix_e2e_test.go))
runs four subtests in parallel:

|             | single-disk                  | RAID1 mirror                          |
|-------------|------------------------------|---------------------------------------|
| **raw HTTP**| `raw_single_disk`            | `raw_raid1`                           |
| **OCI**     | `oci_single_disk`            | `oci_raid1`                           |

Each cell drives the **real** production code paths:

1. `provider.ReportStatus(ctx, StatusInit, …)` — same call the orchestrator
   makes on startup.
2. `disk.Manager.StopRAIDArrays(ctx)` — unconditional pre-wipe.
3. For single-disk: `disk.Manager.WipeDisk(ctx, sda)`.
4. For RAID1: `disk.Manager.WipeDisk(sda)` + `WipeDisk(sdb)` +
   `CreateRAIDArray(ctx, "md0", 1, [sda, sdb])`.
5. `image.Stream(ctx, imageURL, target, StreamOpts{Checksum, ChecksumType})`
   — the real streamer with gzip detection, OCI pull, sha256 verification.
6. `provider.ReportStatus(ctx, StatusSuccess, …)`.

The target "disk" is a regular tempfile bound via `cfg.DiskDevice`, so
`image.Stream` actually writes bytes to it. The test then reads the file back
and asserts byte-exact equality with the source image.

## Mock architecture

Each cell wires every external integration the production agent talks to,
**fully in-process**:

| Component             | Mock                                                | File                                             |
|-----------------------|-----------------------------------------------------|--------------------------------------------------|
| CAPRF controller      | `caprfTestServer` — init/log/success/error/debug HTTP | [`test/e2e/caprf_full_flow_test.go`](../test/e2e/caprf_full_flow_test.go) |
| BMC (Redfish)         | `redfish.MockServer` — power, virtual media, reset  | [`test/e2e/redfish/mock_server.go`](../test/e2e/redfish/mock_server.go) |
| OCI registry          | `startOCIRegistry` — `google/go-containerregistry`  | [`test/e2e/oci_e2e_test.go`](../test/e2e/oci_e2e_test.go) |
| Raw HTTP image server | `httptest.NewServer` serving gzipped bytes          | inline in matrix test                            |
| Target disk           | tempfile pointed to by `cfg.DiskDevice`             | inline in matrix test                            |
| Shell commands        | `mockCommander` programmed per layout               | [`test/e2e/caprf_full_flow_test.go`](../test/e2e/caprf_full_flow_test.go) |

This mirrors the **production t-co topology**:

```
CAPI Machine
   │
   ▼
cluster-api-provider-redfish controller          ← mocked: caprfTestServer
   │  (status / log / debug HTTP)                ← mocked: redfish.MockServer
   ▼
BMC (Redfish)
   │  (virtual-media boot)
   ▼
BOOTy initramfs (PID 1)
   ├─► disk.Manager  ── exec ─► sfdisk / mdadm / wipefs / partprobe   ← mocked: mockCommander
   └─► image.Stream  ── HTTP ─► OCI registry / raw HTTP image server  ← mocked: in-process
```

## Companion tests

Two adjacent tests in the same file complete the coverage:

- **`TestFullProvisioningMatrixImageDataIntegrity`** — exercises the image
  streaming subsystem independently across **4** source variants (raw HTTP,
  gzip HTTP, OCI raw layer, OCI gzip layer), asserting byte-exact output
  and sha256 verification.

- **`TestFullProvisioningMatrixCAPRFLifecycle`** — exercises the CAPRF
  init → log → success lifecycle against the in-process CAPRF mock,
  asserting status order and bearer-token propagation.

## Relationship to other E2E suites

The matrix test is the **first line of defence** for the provisioning flow.
It runs in seconds, requires nothing beyond Go and `linux`, and covers every
combination of the two axes that matter most. The wider E2E pyramid still
applies:

| Suite                       | Tag                      | Why use it                                                                 |
|-----------------------------|--------------------------|----------------------------------------------------------------------------|
| **Matrix (this doc)**       | `e2e && linux`           | Fast, hermetic; every PR. Covers {raw,OCI}×{single,RAID1} end-to-end.      |
| `TestFullProvisionFlow`     | `e2e && linux`           | Single-cell orchestrator drive-through; legacy reference.                  |
| `TestRAIDLVMProvisionOrder*`| `e2e && linux`           | Asserts step ordering within the orchestrator.                             |
| ContainerLab integration    | `e2e_integration`        | FRR EVPN fabric in containers (`make test-e2e-integration`), plus DHCP, static, bond and multi-NIC smoke topologies (`make test-e2e-dhcp`, `test-e2e-static`, `test-e2e-bond`, `test-e2e-multi-nic`). |
| GoBGP E2E                   | `e2e_gobgp`              | Real GoBGP peering in all PeerModes and the EVPN route modes (`make test-e2e-gobgp`).          |
| GoBGP Type-5                | `e2e_gobgp_type5`        | Type-5-only EVPN on a per-machine leaf fabric without static spine FDB entries (`make test-e2e-type5`). |
| Production templates        | `e2e_prodtpl`            | BOOTy with the rendered bm4x-operator templates next to a shadow FRR node with the same template; compares the leaf and route reflector views of both (`make clab-prodtpl-up && make test-e2e-prodtpl`). |
| Boot E2E                    | `linux && e2e_boot`      | Three BOOTy containers on the FRR EVPN fabric run provision, deprovision and standby (`make test-e2e-boot`). |
| vrnetlab / QEMU             | `e2e_vrnetlab`           | BOOTy in QEMU VMs (direct kernel and initramfs boot) on an FRR EVPN fabric; provision, deprovision and standby over EVPN (`make test-e2e-vrnetlab`). Uses KVM when `/dev/kvm` exists. |
| GoBGP vrnetlab              | `e2e_gobgp_vrnetlab`     | GoBGP QEMU VMs with FRR fabric only; no vendor NOS interoperability proof (`make test-e2e-gobgp-vrnetlab`). |
| Production smoke            | `e2e_production`         | CI-proven production-like topology: CAPRF/FRR mode, BGP peering, EVPN AF, VXLAN, underlay routes, overlay reachability, gateway FDB/route. VRF, DCGW BFD, and EVPN type-5 assertions are not CI-proven and are covered by `make test-e2e-production-full` as known limitations. |

Network behaviour is described in [networking.md](networking.md).

## Running

```bash
# The matrix + companion tests only:
go test -tags 'e2e linux' -count=1 -timeout 120s -run TestFullProvisioningMatrix ./test/e2e/

# Full e2e package (also includes initramfs Docker builds, ~10 min):
make test-e2e
```

## Extending the matrix

To add a new axis (e.g. LUKS-encrypted root, partition mode, additional
compression formats):

1. Add a new bool flag to the case struct in `TestFullProvisioningMatrix`.
2. Plumb it through `runMatrixCell` to drive the corresponding `disk.Manager`
   or `image.Stream` configuration.
3. Add layout-specific assertions (`countCalls` / `mdadmCallsContaining`).
4. Document the new axis in this file's matrix table.

Keep each cell **hermetic** — no shared state between subtests, every
external dependency mocked in-process. The whole matrix should run in under
1 second.
