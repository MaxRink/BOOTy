# Provisioning Roadmap Proposals (Index)

## Status

Proposal. The first PRs of some proposals are merged; see the Status column. The remaining design is not implemented.

## Summary

This index groups 40 design proposals that improve how BOOTy provisions,
observes and retires hosts. The first 16 cover OS targets, Windows, ESXi,
Talos, sysext and confext, measured boot for A/B, CAPI integration, BMC
boot paths, UEFI, firmware, testing, image delivery, disk lifecycle,
rollback, tracing and multi-arch. The next 24 add systemd-boot, Flatcar,
metrics with protected push, streaming telemetry, log shipping,
out-of-band diagnostics, SR-IOV, a hardware vendor matrix, and 16
operational topics. Each proposal has the same structure: problem, current
state with file references, goals, options with a recommendation,
interfaces (native keys and CAPRF, CAPI or bm4x impact), security, testing,
phased rollout, effort and open questions. The older proposals in
[roadmap.md](roadmap.md) stay valid; the new documents link to them where
they overlap.

External facts that the authors did not check against a primary source are
marked "verify" in the text.

## Highest impact now (T-CaaS)

Criteria: (a) unblocks the BOOTy drop-in for the legacy CAPRF ramdisk and the
bm4x cable and infrastructure checks; (b) reduces risk (security, data loss,
failed provisioning); (c) reduces operator pain today (debugging when the
network fails, provisioning time); (d) effort and dependencies on work in
flight.

Status as of 2026-10-18. "Merged" means the first PRs of the proposal are on
`main`; the rest of each proposal stays open.

| Rank | Proposal | Why now | Status |
|------|----------|---------|--------|
| 1 | [Metrics: failure codes and step data](proposal-provisioning-metrics.md) | Every other observability item needs stable failure codes and step timings. | Merged: failure codes and step telemetry (#706, [failure-codes.md](failure-codes.md)). Open: snapshot event, Prometheus endpoint, protected push. |
| 2 | [Out-of-band diagnostics](proposal-bmc-out-of-band-diagnostics.md) | Network failures ended in a silent CAPRF timeout. | Merged: `BOOTY1` serial status line and `BootyLastResult` UEFI record (#718). Open: SEL records, CAPRF readers. |
| 3 | [Log shipping](proposal-log-shipping.md) | The CAPRF log handler dropped lines when its buffer was full, and logs before the handoff were lost. | Merged: bounded log ring with level-aware eviction and flush deadline (#711). Open: JSON schema, other sinks, kernel log, pstore. |
| 4 | [CAPI integration: event contract](proposal-capi-integration.md) | The drop-in needs CAPRF to stop parsing log text and to receive inventory, firmware and health data. | Partly: the success message names the handoff (#724). Open: `EVENTS_URL` contract and CAPRF receivers. |
| 5 | [Hardware watchdog](proposal-hardware-watchdog.md) | A hung run waited for the CAPRF timeout without a reason. | Merged: watchdog armed and petted on progress, handover at kexec and reboot (#721). |
| 6 | [Time sync before TLS](proposal-time-sync.md) | Wrong RTC clocks broke TLS. | Merged: clock sync before the first TLS check, RTC write-back (#713, #740). |
| 7 | [Credentials and machine identity](proposal-credential-identity.md) | The bearer token travels in the ISO path and in vars. | Open. TPM evidence fixes merged (#707) as a prerequisite. |
| 8 | [SR-IOV, NIC, DPU and GPU configuration](proposal-sriov-nic-config.md) | The T-CaaS SR-IOV and DRA work needs firmware prerequisites and a handoff contract. | Merged: SR-IOV inventory per PF port (#709), profiles with drift check (#717), runtime settings through kernel interfaces (#719), [sriov.md](sriov.md). Open: vendor firmware backends (#726), KVM igb scenario (#728). |
| 9 | [Image delivery: resume and throughput](proposal-image-delivery.md) | A mid-download break restarts the image at byte zero. | Open. Fast cycle design and timing merged (#714, #715, [agent-fast-cycle.md](agent-fast-cycle.md)); standby latency work open (#716, #730). |
| 10 | [Disk lifecycle](proposal-disk-lifecycle.md) and [certified decommissioning](proposal-certified-decommission.md) | Data loss and compliance risk. | Merged: secure erase must not silently fall back (#704). Open: erase in all modes, verification, report. |

Related work merged or in flight since this index was written:

- kexec: GRUB default entry resolution with an allow-list (#720), handoff
  named in the success message (#724), a real KVM kexec scenario with the EFI
  runtime check (#725). Open: BGP Cease and BFD AdminDown before kexec and
  reboot (#723); the limitations page `docs/kexec.md` (#705).
- UEFI and Secure Boot ISO boot with lockdown measurements: open (#737).
- Cable check: in progress (#727, #729, #731, #734, #735).
- Ramdisk on Debian 13 with the 6.12 kernel (#701) and smaller ramdisks (#722).

Next to start (no overlap with work in flight): the `EVENTS_URL` event
contract (rank 4), the metrics snapshot event on top of the merged codes
(rank 1), and the SEL record writer (rank 2).

## Prioritized roadmap

Value: H = high, M = medium, L = low. Effort in engineer-days, BOOTy and CAPRF
together. Order is the suggested start order; independent rows can run in
parallel. Numbers in "Depends on" refer to the Order column.

| Order | Proposal | Value | Effort (days) | Depends on | First small PR |
|-------|----------|-------|---------------|------------|----------------|
| 1 | [Better testing](proposal-testing-strategy.md) | H | 35 to 51 | none | Go fuzz targets for vars, `frr.conf`, netplan, grub.cfg, GPT, qcow2 |
| 2 | [Disk lifecycle: erase, health, burn-in](proposal-disk-lifecycle.md) | H | 32 to 49 | in-flight secure-erase fix | Erase in all modes |
| 3 | [CAPI integration](proposal-capi-integration.md) | H | 36 to 55 | none | Typed step events on `EVENTS_URL` |
| 4 | [Image delivery: throughput, resume, fleet, air gap](proposal-image-delivery.md) | H | 33 to 50 | 1 (benchmarks) | zstd decoder concurrency |
| 5 | [UEFI management](proposal-uefi-management.md) | H | 28 to 41 | none | Read-only UEFI audit report |
| 6 | [Rollback story](proposal-rollback.md) | H | 23 to 33 | 5, 17 | Document today's implicit trial boot |
| 7 | [Measured boot and attestation for A/B](proposal-measured-boot-ab.md) | H | 44 to 67 | 5, 17, in-flight TPM fixes, [LUKS](proposal-luks-encryption.md) | CEL event log and `TPM_MODE=measure` |
| 8 | [sysext and confext](proposal-sysext-confext.md) | M to H | 19 to 29 | 17 for stub mode | DDI structure and `extension-release` check |
| 9 | [More OS targets](proposal-os-targets.md) | H | 28 to 42 | none | `TargetProfile` table for today's families |
| 10 | [Firmware analysis and updates](proposal-firmware-lifecycle.md) | M to H | 40 to 61 | 3 | BMC version in-band via IPMI |
| 11 | [BMC preloading and booting](proposal-bmc-preload-boot.md) | M | 35 to 54 | 3, 7, 17, 22 | Runtime seed reader |
| 12 | [Talos Linux](proposal-talos.md) | M | 17 to 25 | 9 | Talos profile that skips Linux-root steps |
| 13 | [Provisioning run tracing](proposal-provisioning-tracing.md) | M | 9 to 13 | 3 | `TRACEPARENT` in events and logs |
| 14 | [Multi-arch delta](proposal-multiarch-provisioning.md) | L to M | 9 to 13 | 9, 17 | Secure Boot verifier paths per architecture |
| 15 | [Windows Server](proposal-windows-deployment.md) | L (demand unclear) | 18 to 28 | 9, 5 | Windows profile behind a flavor |
| 16 | [VMware ESXi](proposal-esxi-deployment.md) | L | 17 to 26 | none | Generic `MODE=prepare` |
| 17 | [Full systemd-boot support](proposal-systemd-boot.md) | H | 33 to 45 | 9 (BLS parser), 5 | Bootloader detection and `BOOTLOADER` key |
| 18 | [Full Flatcar support](proposal-flatcar.md) | H | 28 to 43 | 9, 8, 3 | Flatcar target profile and OEM writer |
| 19 | [Provisioning metrics and protected push](proposal-provisioning-metrics.md) | H | 30 to 44 | 3 for the event kind, 28 for mTLS identity | Failure code table and step tracker wiring |
| 20 | [Streaming telemetry](proposal-streaming-telemetry.md) | M | 22 to 33 | 19, 3, in-flight cable-check | Streaming mode for CAPRF events |
| 21 | [Native log shipping](proposal-log-shipping.md) | H | 26 to 39 | 19 (codes) | JSON record schema and error-priority ring |
| 22 | [Out-of-band diagnostics through the BMC](proposal-bmc-out-of-band-diagnostics.md) | H | 19 to 29 | 19 (codes) | Serial status line and UEFI result variable |
| 23 | [SR-IOV, NIC, DPU and GPU configuration](proposal-sriov-nic-config.md) | H | 43 to 67 | 3, 24 | Kernel-only SR-IOV inventory |
| 24 | [Hardware vendor matrix](proposal-hardware-vendor-matrix.md) | M to H | 12 to 20 | none | Matrices as YAML with evidence |
| 25 | [BIOS attributes and HW RAID as code](proposal-bios-raid-as-code.md) | M | 24 to 36 | 24 | Profile schema and read-only drift report |
| 26 | [Zero-touch onboarding](proposal-zero-touch-onboarding.md) | M | 18 to 27 | 3, in-flight cable-check | `MODE=discover` |
| 27 | [Supply-chain trust](proposal-supply-chain-trust.md) | H | 21 to 32 | none | Sign all artifacts, attach provenance |
| 28 | [Credentials and machine identity](proposal-credential-identity.md) | H | 21 to 31 | in-flight TPM fixes for TPM keys | CAPRF single-use bootstrap tokens |
| 29 | [Hardware watchdog](proposal-hardware-watchdog.md) | H | 8 to 12 | none | `/dev/watchdog` supervisor |
| 30 | [Time sync before TLS](proposal-time-sync.md) | H | 8 to 12 | none | CAPRF `Date` sync with pinned CA |
| 31 | [Hardware quirks database](proposal-hardware-quirks.md) | M | 9 to 13 | 24 | Database format with two actions |
| 32 | [Rescue and maintenance mode](proposal-rescue-maintenance.md) | M | 17 to 25 | 21 | Implement `RESCUE_TIMEOUT` as a lease |
| 33 | [Hardware burn-in suite](proposal-burn-in.md) | M | 18 to 27 | 2, 19 | `MODE=burnin` skeleton with counters |
| 34 | [IPv6-only provisioning](proposal-ipv6-only.md) | M | 14 to 22 | none | Address family audit test |
| 35 | [LACP bonds with partner verification](proposal-lacp-bonds.md) | M | 8 to 13 | in-flight cable-check | Bond state reader |
| 36 | [802.1X and MACsec](proposal-8021x-macsec.md) | L | 16 to 25 | 28 | `secure-fabric` flavor |
| 37 | [Console status screen](proposal-console-status.md) | M | 6 to 10 | 19 (codes) | Status model from step events |
| 38 | [Certified decommissioning](proposal-certified-decommission.md) | H | 8 to 13 | 2, in-flight secure-erase fix | Report schema |
| 39 | [Fleet rollout control](proposal-fleet-rollout-control.md) | H | 16 to 24 | 19, 31 | `BootyRelease` object (CAPRF) |
| 40 | [More storage targets](proposal-storage-targets.md) | L to M | 24 to 36 | none | btrfs subvolumes |
| | **Total** | | **872 to 1315** | | |

## Dependency sketch

```text
testing (1) -> image delivery (4)
CAPI events (3) -> firmware (10), tracing (13), BMC boot (11), metrics (19)
metrics codes (19) -> logs (21), OOB diagnostics (22), console (37), burn-in (33), rollout (39)
OOB diagnostics (22) -> BMC boot (11)
UEFI mgmt (5) -> rollback (6), measured boot (7), systemd-boot (17)
OS profiles (9) -> systemd-boot (17), Flatcar (18), Talos (12), multi-arch (14), Windows (15)
systemd-boot (17) -> rollback (6), measured boot (7), sysext stub mode (8), BMC UKI boot (11)
sysext (8) -> Flatcar (18)
identity (28) -> metrics mTLS (19), 802.1X (36)
vendor matrix (24) -> SR-IOV (23), BIOS and RAID (25), quirks (31)
quirks (31) -> rollout control (39)
disk lifecycle (2) -> certified decommission (38), burn-in (33)
```

## Quick wins (one PR each, under three days)

1. Done (#718): serial status line and UEFI result variable
   ([out-of-band diagnostics](proposal-bmc-out-of-band-diagnostics.md)).
2. Done (#706): failure code table and step tracker wiring
   ([metrics](proposal-provisioning-metrics.md)).
3. Raise zstd decoder concurrency, behind a benchmark
   ([image delivery](proposal-image-delivery.md)).
4. Add Go fuzz targets for the existing parsers
   ([testing](proposal-testing-strategy.md)).
5. Make the Secure Boot verifier use per-architecture loader names
   ([multi-arch](proposal-multiarch-provisioning.md)).
6. Document the implicit trial boot of A/B preserve-existing upgrades
   ([rollback](proposal-rollback.md)).

## Cross-cutting findings from the code review

These came up while the current state was studied. They are input for the
proposals, not separate changes in this PR.

| Finding | Where | Proposal |
|---------|-------|----------|
| `pkg/tpm` assigns PCR 8, 9, 10, 14, 15 to BOOTy. These collide with GRUB, IMA, shim MokList and systemd (UAPI registry, verify). The cryptenroll proposal binds LUKS to PCR 14. Fixed in #707 (one PCR, 23; cryptenroll uses PCR 7). | `pkg/tpm/pcr.go`, `docs/proposal-cryptenroll-tpm.md` | [measured boot](proposal-measured-boot-ab.md) |
| The quote AK is not a restricted key, and there is no EK credential activation. `VerifyQuoteSignature` uses the key in the quote. Fixed in #707 (restricted AK, credential activation, `VerifyQuote`). | `pkg/tpm/attest.go` | [measured boot](proposal-measured-boot-ab.md) |
| `pkg/tpm` and `pkg/disk/luks` are not wired into the provisioning flow. | `grep` of non-test importers | [measured boot](proposal-measured-boot-ab.md) |
| Secure erase falls back to `wipefs` on failure, on frozen drives and on drives without ATA security, and the run succeeds. SAS disks take the `hdparm` path. Fixed in #704. | `pkg/disk/manager.go` | [disk lifecycle](proposal-disk-lifecycle.md) |
| CAPRF disables Secure Boot before it boots BOOTy, so PCR 7 shows Secure Boot off for the provisioning boot. | CAPRF `internal/redfish/system.go` | [measured boot](proposal-measured-boot-ab.md), [UEFI](proposal-uefi-management.md) |
| CAPRF `main` has no receiver for `INVENTORY_URL`, `FIRMWARE_URL` and `HEALTH_CHECK_URL`, and derives image phases from log text. | CAPRF `internal/server/server.go`, `internal/provision/manager.go` | [CAPI](proposal-capi-integration.md) |
| kexec parses only `grub.cfg` `menuentry` blocks. BLS-based targets (RHEL-likes, Fedora) need a BLS parser. #720 now resolves the GRUB default with an allow-list and reboots for BLS; a BLS parser is still needed for kexec. | `pkg/kexec/grub.go` | [OS targets](proposal-os-targets.md) |
| The Secure Boot verifier fallback path is `BOOTX64.EFI` only. | `pkg/secureboot/verify.go` | [multi-arch](proposal-multiarch-provisioning.md) |
| Image download retries stop once the body starts; a mid-body break restarts at byte zero. | `pkg/image/stream.go` | [image delivery](proposal-image-delivery.md) |
| No Go fuzz tests and no benchmarks exist. | `grep` of `_test.go` files | [testing](proposal-testing-strategy.md) |
| `pkg/bootloader` (systemd-boot scaffolding) was removed in #463; GRUB is hard-coded in kexec, EFI fallback and entry creation. | `pkg/kexec/grub.go`, `pkg/provision/configurator.go` | [systemd-boot](proposal-systemd-boot.md) |
| The `flatcar-like-usra-system-ab` KVM scenario copies a synthetic `USR-A` into BOOTy's own A/B layout; no real Flatcar boot, Ignition or gptprio is tested. | `test/e2e/kvm/ab_sysext_test.go` | [Flatcar](proposal-flatcar.md) |
| The CAPRF log handler drops lines when its 256-line buffer is full. Fixed in #711 (level-aware ring, flush deadline). | `main.go`, `pkg/caprf/loghandler.go` | [log shipping](proposal-log-shipping.md) |
| The telemetry collector is created but no step feeds it; there is no metrics endpoint. Step telemetry fixed in #706; no endpoint yet. | `pkg/provision/orchestrator.go`, `pkg/telemetry/` | [metrics](proposal-provisioning-metrics.md) |
| BOOTy does not sync time; TLS depends on the RTC. Fixed in #713. | `docs/caprf-integration.md` | [time sync](proposal-time-sync.md) |
| `RESCUE_TIMEOUT` is parsed but unused; rescue access uses static keys. | `docs/configuration.md`, `pkg/rescue/rescue.go` | [rescue and maintenance](proposal-rescue-maintenance.md) |
| The DHCP client is IPv4 only. | `pkg/network/dhcp.go` | [IPv6-only](proposal-ipv6-only.md) |
| The bond lab does not negotiate LACP; no partner check exists. | `README.md`, `pkg/network/bond.go` | [LACP](proposal-lacp-bonds.md) |

## How these proposals relate to older ones

| New proposal | Older proposals it extends or narrows |
|--------------|---------------------------------------|
| Measured boot A/B | [tpm-attestation](proposal-tpm-attestation.md), [attestation-enhanced](proposal-attestation-enhanced.md), [cryptenroll-tpm](proposal-cryptenroll-tpm.md), [secureboot-full-chain](proposal-secureboot-full-chain.md) |
| UEFI management | [secureboot](proposal-secureboot.md), [bootloaders](proposal-bootloaders.md) |
| Firmware lifecycle | [firmware-reporting](proposal-firmware-reporting.md), [nic-firmware-common](proposal-nic-firmware-common.md), [bios-settings](proposal-bios-settings.md) |
| Image delivery | [resilient-provisioning](proposal-resilient-provisioning.md), [oci-advanced](proposal-oci-advanced.md) |
| Tracing | [telemetry-enhanced](proposal-telemetry-enhanced.md), [observability](proposal-observability.md) |
| Testing | [ci-testing-matrix](proposal-ci-testing-matrix.md), [e2e-test-matrix](e2e-test-matrix.md) |
| Multi-arch | [arm64-support](proposal-arm64-support.md) |
| OS targets | [cloudinit-management](proposal-cloudinit-management.md), [network-persistence](proposal-network-persistence.md) |
| Disk lifecycle | [health-checks](proposal-health-checks.md), [go-binary-replacement](proposal-go-binary-replacement.md) |
| systemd-boot | [bootloaders](proposal-bootloaders.md) (its `pkg/bootloader` was removed in #463) |
| Metrics, logs, streaming | [observability](proposal-observability.md), [telemetry-enhanced](proposal-telemetry-enhanced.md), [kafka-logging](proposal-kafka-logging.md) |
| SR-IOV | [nic-firmware-common](proposal-nic-firmware-common.md) and the vendor NIC proposals |
| BIOS and RAID as code | [bios-settings](proposal-bios-settings.md), [bios-management-vendors](proposal-bios-management-vendors.md) |
| Credential identity | [jwt-auth](proposal-jwt-auth.md) |
| Rescue and maintenance | [rescue-mode](proposal-rescue-mode.md) |
| Supply-chain trust | [release-pipelines](proposal-release-pipelines.md), [image-signatures](proposal-image-signatures.md) |
| Storage targets | [disk-partitioning](proposal-disk-partitioning.md) |

## kexec limitations

The kexec handoff skips the firmware, so boot counting, `BootNext`, measured
boot, Secure Boot verification by shim and pending firmware changes do not
apply to the kexec'd kernel. The full list, with a kexec or reboot decision
table, is in `docs/kexec.md` (PR #705).

## Process

- Each proposal lists small, independent PRs. Land them one at a time.
- New vars keys need a row in [configuration.md](configuration.md)
  (`TestConfigurationDocListsEveryVarsKey`).
- Changes to the provisioning step list must keep `TestProvisionStepCount`
  consistent.
- CAPRF changes go through CAPRF merge requests. This repository holds only
  the BOOTy side and the shared contract.
