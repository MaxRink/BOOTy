# Better Testing

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1

## Summary

BOOTy has a strong network test pyramid (ContainerLab, GoBGP, vrnetlab,
production templates with fault injection) and a KVM matrix with 26
scenarios. Gaps remain in four areas: OS target coverage, security features
(Secure Boot with custom keys, TPM attestation), failure injection on the
disk and data path, and non-functional tests (fuzzing, property tests,
performance). This proposal closes them and adds a hardware-in-the-loop
(HIL) lab.

## Problem and motivation

- The KVM matrix boots mostly Ubuntu-like test images. New OS families need
  real images in CI (see [proposal-os-targets.md](proposal-os-targets.md)).
- `TestUEFISecureBootSmoke` boots an unsigned kernel and accepts a timeout
  (`test/e2e/kvm/secureboot_test.go`). It does not prove a signed chain.
- `TestTPMSmokeQEMU` starts swtpm but does not check PCRs, quotes or sealing
  (`test/e2e/kvm/tpm_test.go`).
- Disk errors, network loss during streaming and power loss mid-write are
  untested. The code has cleanup paths for these (`wipeLeadingSectors`,
  checkpoint resume, rescue retry), but no test proves them under real
  faults.
- No Go fuzz tests exist (`grep "func Fuzz"` finds none). BOOTy parses many
  untrusted or semi-trusted formats: `/deploy/vars`, `frr.conf`, netplan
  YAML, GPT, qcow2 headers, grub.cfg, OCI manifests, JSON config.
- No benchmarks exist (`grep "func Benchmark"` in `pkg/` finds none). There is
  no throughput or boot-to-provisioned baseline, so regressions go unseen.

## Current state

| Suite | Coverage | Reference |
|-------|----------|-----------|
| Unit | Broad, table-driven, characterization tests for parity. | `pkg/**/*_test.go` |
| Hermetic matrix | {raw, OCI} x {single, RAID1} with in-process mocks. | `test/e2e/full_provisioning_matrix_e2e_test.go`, `docs/e2e-test-matrix.md` |
| KVM matrix | 26 scenarios: secureboot-smoke, luks, tpm, kexec, kexec-persist, ab-sysext-preload, ab-preserve-existing, system-ab-preserve-existing, flatcar-like-usra-system-ab, crash-artifacts, caprf-legacy-compat, caprf-native-parity, disk-wipe, standby-agent, rescue-retry-stale-mounts, network-persistence-static-ip, disk-links-root-uuid, predictable-nic-names, flavor-storage-controllers, uefi-efi-entries, production-templates, bootloader-smoke, iso-headless-q35, mirror-boot-failover, mirror-boot-failover-separate-boot, luks-verify. | `.github/workflows/kvm-matrix.yml`, `test/e2e/kvm/` |
| ContainerLab | FRR EVPN, DHCP, static, bond, multi-NIC, GoBGP, Type-5, production templates with fault, BFD, LLDP and route-map variants. | `test/e2e/integration/`, `test/e2e/clab/` |
| vrnetlab | BOOTy in QEMU on an FRR fabric. | `test/e2e/clab/topology-vrnetlab.clab.yml` |
| Nightly | Scheduled workflow. | `.github/workflows/nightly.yml` |
| CAPRF kindmetal | `e2e-kindmetal-booty`, `e2e-kindmetal-booty-evpn`, BM4X jobs on the CAPRF branch. | CAPRF `docs/booty-feature-test-matrix.md` (branch) |

## Goals

- One KVM scenario per supported OS family.
- A real Secure Boot chain test with custom keys.
- TPM tests that assert PCRs, quotes and sealed unlock across A/B.
- Fault injection for disks, network and power.
- Go native fuzz tests for every parser of external input.
- Property tests for planners (partition layout, A/B slot selection, disk
  selection).
- Benchmarks with tracked baselines.
- A small HIL lab with real BMCs.

## Non-goals

- Replacing the hermetic matrix. It stays the fast first line.
- Testing vendor firmware bugs. The HIL lab records them, it does not fix
  them.

## Design

### 1. KVM matrix extensions

| Scenario | What it proves | Depends on |
|----------|----------------|------------|
| `os-<family>` (Debian, Rocky BLS, Fedora CoreOS, Flatcar, NixOS, Talos) | Write, first boot, handoff per family | OS targets proposal |
| `secureboot-custom-keys` | OVMF in setup mode, enroll test PK, KEK, db; boot a signed BOOTy UKI; install a signed target; target boots with Secure Boot on | UEFI management, UKI build |
| `tpm-pcr-plan` | BOOTy extends only its PCR; CEL replays to the quoted digest | Measured boot proposal |
| `tpm-attest` | Restricted AK, EK cert from `swtpm_setup --create-ek-cert` (verify flag), quote accepted by a test verifier | Measured boot proposal |
| `tpm-ab-unseal` | LUKS root unlocks after an A/B slot switch with a signed policy; fails with an unsigned UKI | Measured boot proposal |
| `fault-disk-eio` | Write errors during streaming lead to a cleaned target and a reported error | Fault injection (below) |
| `fault-net-drop` | Connection loss mid-stream leads to a retry or resume, and a correct checksum | Fault injection, resumable downloads |
| `fault-power-loss` | QEMU kill mid-write, reboot into BOOTy, rerun succeeds, the old slot of an A/B host still boots | Fault injection |
| `uefi-http-boot` | OVMF HTTP boot of a UKI | BMC proposal |
| `arm64-smoke` | `qemu-system-aarch64` with AAVMF boots the arm64 flavor (TCG, slow; nightly) | arm64 proposal |

Keep per-PR scenarios under 15 minutes each. Move image-heavy OS scenarios to
nightly. Cache images in a CI cache keyed by the pinned digest.

### 2. Fault injection

| Fault | Mechanism | Notes |
|-------|-----------|-------|
| Disk I/O errors | Linux `dm-flakey` or `dm-error` over a loop device in the guest, or QEMU `blkdebug` rules on the virtio disk (verify `blkdebug` event names) | `blkdebug` is host-side and needs no guest tools. |
| Slow disk | `dm-delay` | Tests idle timeouts. |
| Disk vanishes | QEMU monitor `device_del` | Tests settle and selection. |
| Network loss | `tc netem loss` or link down on the host tap; or a fault proxy that cuts the HTTP body at byte N | The proxy is deterministic and works in unit tests too. |
| Corrupt bytes | Fault proxy flips a byte | Checksum must catch it. |
| Power loss | QEMU `quit` through QMP at a step boundary or byte offset, then restart with the same disks | Asserts recoverable state. |
| BMC errors | Redfish mock returns 5xx, 412, slow responses | CAPRF side mostly. |

Build a small Go `faultproxy` test helper under `test/e2e/` that wraps
`httptest` and supports "cut after N bytes", "delay", "corrupt at N" and
"return status S for the first K requests".

### 3. Fuzzing

Add `func FuzzX(f *testing.F)` targets with seed corpora from existing test
fixtures:

| Target | Package |
|--------|---------|
| `ParseVars` | `pkg/caprf` |
| `frr.conf` parser | `pkg/network/frrconf` |
| netplan reader | `pkg/network/netplan` |
| `ParseGrubCfg` and the BLS parser | `pkg/kexec` |
| GPT parser in A/B streaming | `pkg/image` (`parseGPTPartitions`) |
| qcow2 header check | `pkg/image` (`checkQCOW2Header`) |
| Format detection | `pkg/image` (`DetectFormat`) |
| `PARTITION_LAYOUT` and `AB_DATA_PARTITIONS` JSON | `pkg/config` |
| EFI signature lists and load options | `pkg/efi` (after the UEFI proposal) |
| Event log replay | `pkg/tpm` (after the measured boot proposal) |

Run each fuzz target for 30 to 60 seconds per PR when its package changes,
and longer in nightly. Store found crashers in `testdata/fuzz/` as regression
seeds (the Go toolchain does this).

### 4. Property tests

Use the standard `testing/quick` or plain randomized table generation (no new
dependency first). Properties:

- Partition layout planner: partitions never overlap, fit the disk, keep
  alignment, and keep the ESP first.
- A/B target selection: the target slot is never the active slot with
  `preserveExisting`.
- Disk selection: the same input set yields the same disk, independent of
  enumeration order.
- Vars parsing: parse then render then parse is stable for accepted inputs.

### 5. Benchmarks and regression tracking

- Go benchmarks: `image.Stream` from an in-memory server to `/dev/null` and to
  a tmpfs file, per format (raw, gzip, zstd, xz, lz4), with and without
  checksum. Vars parsing. Event log replay.
- KVM metrics: boot-to-`report-init`, `report-init`-to-`report-success`,
  stream throughput, kexec time. Extract from structured logs.
- Store results as CI artifacts in a stable JSON format. Compare with the
  last `main` result and fail when a metric regresses by more than a
  threshold (for example 15 percent, tuned after noise measurement).
- Use `benchstat` for Go benchmarks.

### 6. Hardware-in-the-loop lab

- Minimum: one host each from Dell, HPE, Lenovo and Supermicro, one GPU host,
  one host with hardware RAID, managed PDUs, and one leaf pair with EVPN.
- A self-hosted runner per lab, triggered nightly and on release tags.
- Flows: full CAPRF provision, A/B upgrade, deprovision, firmware inventory,
  Secure Boot with site keys, TPM attestation.
- Record Redfish payloads, firmware event logs, UEFI variable dumps and
  inventories as fixtures for unit tests.

## Interfaces

No new runtime keys. New Make targets:

| Target | Purpose |
|--------|---------|
| `make fuzz` | Run all fuzz targets for a short time. |
| `make bench` | Run Go benchmarks and write a JSON report. |
| `make test-kvm-faults` | Run the fault injection scenarios. |

New build tag: `e2e_hil` for lab tests.

### CAPRF and CAPI API impact

- CAPRF kindmetal gains fault scenarios: BMC errors, callback loss, slow
  virtual media.
- Shared contract fixtures (events, vars) between the repositories.

## Security considerations

- Test keys for Secure Boot and TPM must be clearly marked and must never
  reach release artifacts. Generate them in the test, do not commit private
  keys.
- The HIL lab has real BMC credentials. Keep them in the CI secret store with
  a lab-only scope. Isolate the lab network.
- Fuzz corpora can contain crafted inputs. Keep them under `testdata/`.

## Testing strategy

This proposal is the testing strategy. Each phase lands with its own CI job.

## Phased rollout

1. PR: fuzz targets for vars, `frr.conf`, netplan, grub.cfg, GPT, qcow2,
   format detection. `make fuzz`.
2. PR: `faultproxy` helper and unit tests for streaming under faults.
3. PR: Go benchmarks and `make bench` with a JSON report.
4. PR: KVM `fault-disk-eio`, `fault-net-drop`, `fault-power-loss`.
5. PR: property tests for layout, A/B and disk selection.
6. PR: `secureboot-custom-keys` (needs a BOOTy UKI target).
7. PR: TPM scenarios (with the measured boot work).
8. PR: OS family scenarios (with the OS targets work).
9. PR: benchmark regression gate in CI.
10. Lab: HIL runner and `e2e_hil` suite.

## Effort estimate

| Phase | Days |
|-------|------|
| Fuzz targets | 4 to 6 |
| Fault proxy and streaming fault tests | 3 to 4 |
| Benchmarks and report | 3 to 4 |
| KVM fault scenarios | 6 to 9 |
| Property tests | 3 to 4 |
| Secure Boot custom keys scenario | 4 to 6 |
| Regression gate | 2 to 3 |
| HIL lab setup (excluding hardware purchase) | 10 to 15 |
| **Total** | **35 to 51** |

OS and TPM scenarios are counted in their own proposals.

## Open questions

- Is there budget and rack space for a HIL lab, or can an existing lab be
  shared with CAPRF?
- Which CI runners have `/dev/kvm` with nested virtualization for long
  scenarios?
- What regression threshold is acceptable given CI noise?

## systemd-boot and Flatcar scenarios

- systemd-boot: `sdboot-type1`, `sdboot-uki`, `sdboot-secureboot`,
  `sdboot-ab-counting`, `sdboot-xbootldr` (see [proposal-systemd-boot.md](proposal-systemd-boot.md)).
- Flatcar: `flatcar-stable`, `flatcar-update-switch`, `flatcar-sysext`,
  `flatcar-secureboot`, `flatcar-tpm-luks` (see [proposal-flatcar.md](proposal-flatcar.md)). The existing
  `flatcar-like-usra-system-ab` scenario only tests the generic source rule.

## Related proposals

- [proposal-ci-testing-matrix.md](proposal-ci-testing-matrix.md)
- [e2e-test-matrix.md](e2e-test-matrix.md)
- [proposal-os-targets.md](proposal-os-targets.md)
- [proposal-measured-boot-ab.md](proposal-measured-boot-ab.md)
- [proposal-image-delivery.md](proposal-image-delivery.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-flatcar.md](proposal-flatcar.md)
