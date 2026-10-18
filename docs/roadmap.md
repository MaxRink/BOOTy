# BOOTy Feature Roadmap

Feature proposals for extending BOOTy and CAPRF beyond current capabilities.
Each proposal follows a standard format: Status, Priority, Summary, Motivation,
Design, Affected Files, Risks, and Effort Estimate.

## Provisioning Roadmap Proposals

[proposal-provisioning-roadmap.md](proposal-provisioning-roadmap.md) indexes
the provisioning proposals (OS targets, measured boot for A/B, CAPI
integration, BMC boot, UEFI, firmware, testing and more) with a prioritized
order, effort and dependencies. The first PRs of some proposals are merged;
see the Status column. The remaining design is not implemented.

## Priority Legend

| Priority | Meaning |
|----------|---------|
| **P0** | Critical — blocks production use or security |
| **P1** | High — significant operational value |
| **P2** | Medium — improves reliability or usability |
| **P3** | Nice to have — advanced capabilities |
| **P4** | Future — exploratory or long-term |

## Proposals by Priority

### P0 — Critical

| Proposal | Summary | Est. Effort |
|----------|---------|-------------|
| [SecureBoot Lifecycle](proposal-secureboot.md) | Re-enable SecureBoot post-provisioning + MOK enrollment | 2-10 days |
| [SecureBoot Full Chain](proposal-secureboot-full-chain.md) | Full signing chain verification + MOK enrollment automation | 10-15 days |
| [Vendor Integrations (HPE)](proposal-vendor-integrations.md) | Re-enable HPE iLO support (disabled code exists) | 1 day |
| [Release Pipelines](proposal-release-pipelines.md) | Semantic versioning, SBOM, signing, nightly/beta/stable channels | 8-12 days |

### P1 — High

| Proposal | Summary | Est. Effort |
|----------|---------|-------------|
| [Hardware Inventory](proposal-hardware-inventory.md) | Collect CPU, memory, disk, NIC specs from sysfs | 9-12 days |
| [Full Server Inventory](proposal-full-inventory.md) | Extended inventory: GPU, storage controllers, thermal, PSU, USB | 10-14 days |
| [Health Pre-Checks](proposal-health-checks.md) | Validate hardware before provisioning | 8-10 days |
| [VLAN Support](proposal-vlan-support.md) | 802.1Q VLAN tagging for provisioning traffic | 6-8 days |
| [BGP Networking Modes](proposal-bgp-networking-modes.md) | Multi-VRF, IPv6 underlay, communities, graceful restart | 10-14 days |
| [Observability (Debug Dump)](proposal-observability.md) | Comprehensive debug state capture on failure | 3 days |
| [NIC Firmware Common](proposal-nic-firmware-common.md) | Common NIC firmware management framework and interface | 6-8 days |
| [NIC Firmware Mellanox](proposal-nic-firmware-mellanox.md) | Mellanox ConnectX firmware management (mstflint/mstconfig) | 8-12 days |
| [NIC Firmware Intel](proposal-nic-firmware-intel.md) | Intel NIC firmware management (devlink, nvmupdate64e) | 8-12 days |
| [NIC Firmware Broadcom](proposal-nic-firmware-broadcom.md) | Broadcom NIC firmware management (devlink, bnxtnvm) | 8-12 days |
| [CI Testing Matrix](proposal-ci-testing-matrix.md) | DHCP/LACP/static/multi-NIC topologies + KVM test matrix | 12-16 days |
| [Resilient Provisioning](proposal-resilient-provisioning.md) | Per-step retry, HTTP Range resume, checkpoint, watchdog | 8-12 days |
| [JWT Authentication](proposal-jwt-auth.md) | JWT token auth for CAPRF with renewal and machine identity | 6-10 days |
| [OCI Advanced](proposal-oci-advanced.md) | Multi-layer OCI, manifest list, Cosign verification, persistent cache partition | 12-16 days |
| [Cloud-Init Management](proposal-cloudinit-management.md) | Cloud-init generation and injection (NoCloud/configdrive) | 6-10 days |
| [Kexec Enhanced](proposal-kexec-enhanced.md) | Multi-kernel selection, chain loading, cmdline management | 5-8 days |
| [LUKS Encryption](proposal-luks-encryption.md) | Full disk encryption with LUKS2, multiple unlock methods | 10-14 days |

### P2 — Medium

| Proposal | Summary | Est. Effort |
|----------|---------|-------------|
| [Vendor Integrations (Lenovo)](proposal-vendor-integrations.md) | Lenovo XCC support + vendor quirks | 3-5 days |
| [BIOS Settings](proposal-bios-settings.md) | Declarative BIOS config via Redfish | 8-10 days |
| [BIOS Management Vendors](proposal-bios-management-vendors.md) | Vendor-specific BIOS (Lenovo, HPE, Supermicro, Dell) + Redfish-less local flow | 16-24 days |
| [Firmware Reporting](proposal-firmware-reporting.md) | Collect and validate firmware versions | 8-10 days |
| [Rescue Mode](proposal-rescue-mode.md) | Interactive debug shell with SSH | 5-7 days |
| [Network Persistence](proposal-network-persistence.md) | Write network config into provisioned OS | 9-12 days |
| [Observability (Metrics)](proposal-observability.md) | Prometheus metrics + event stream | 7-10 days |
| [Telemetry Enhanced](proposal-telemetry-enhanced.md) | OpenTelemetry metrics/tracing + OTLP export + Grafana | 8-12 days |
| [Kafka Logging](proposal-kafka-logging.md) | Structured log streaming to Kafka for fleet monitoring | 5-8 days |
| [Cryptenroll TPM](proposal-cryptenroll-tpm.md) | TPM2-sealed LUKS keys for unattended encrypted boot | 8-12 days |
| [Attestation Enhanced](proposal-attestation-enhanced.md) | Self-measurement, image measurement, remote attestation | 10-14 days |
| [Bootloaders](proposal-bootloaders.md) | GRUB + systemd-boot management, auto-detection | 8-12 days |
| [Kernel Drivers](proposal-kernel-drivers.md) | Declarative module manifests, PCI auto-loading, custom injection | 6-9 days |
| [Binary Optimization](proposal-binary-optimization.md) | Build tag gating, per-flavor builds, size budgets | 6-10 days |
| [IPMI Local](proposal-ipmi-local.md) | Local BMC network config, sensors, boot order via /dev/ipmi0 | 8-12 days |

### P3 — Nice to Have

| Proposal | Summary | Est. Effort |
|----------|---------|-------------|
| [Image Signatures](proposal-image-signatures.md) | Checksum + GPG + Cosign verification | 10-14 days |
| [Custom Disk Partitioning](proposal-disk-partitioning.md) | Declarative partition layouts + LVM | 14-17 days |
| [Dry-Run Mode](proposal-dry-run.md) | Simulate provisioning without writes | 9-12 days |
| [Webhooks](proposal-webhooks.md) | Push notifications to Slack, PagerDuty, etc. | 11-14 days |
| [Go Binary Replacement](proposal-go-binary-replacement.md) | Replace shell tools with pure Go (wipefs, partprobe, dmidecode) | 12-18 days |

### P4 — Future

| Proposal | Summary | Est. Effort |
|----------|---------|-------------|
| [TPM Attestation](proposal-tpm-attestation.md) | Measured boot + remote attestation | 17-22 days |
| [ARM64 Support](proposal-arm64-support.md) | Multi-architecture builds and provisioning | 10-14 days |
| [NVMe Namespaces](proposal-nvme-namespaces.md) | NVMe namespace CRUD for advanced storage | 8-11 days |

## Existing Proposals (Implemented or In Progress)

| Proposal | Status |
|----------|--------|
| [Agent / Standby Mode](proposal-agent-mode.md) | Implemented |
| [Fast deprovision and provision cycle](agent-fast-cycle.md) | In progress |
| [GoBGP Network Stack](proposal-gobgp.md) | Implemented |
| [CAPRF Integration](caprf-integration-plan.md) | Implemented |
| [Hardware Inventory](proposal-hardware-inventory.md) | Implemented |
| [Health Pre-Checks](proposal-health-checks.md) | Implemented |
| [VLAN Support](proposal-vlan-support.md) | Implemented |
| [Firmware Reporting](proposal-firmware-reporting.md) | Implemented |
| [Observability (Debug Dump)](proposal-observability.md) | Implemented |
| [JWT Authentication](proposal-jwt-auth.md) | Implemented: `TOKEN_URL` exchange and renewal |
| [Dry-Run Mode](proposal-dry-run.md) | Implemented: `MODE=dry-run` read-only checks |
| [Cloud-Init Management](proposal-cloudinit-management.md) | Implemented: NoCloud and ConfigDrive seeds |
| [Custom Disk Partitioning](proposal-disk-partitioning.md) | Implemented: `PARTITION_LAYOUT` with LVM |
| [NVMe Namespaces](proposal-nvme-namespaces.md) | Implemented: `NVME_NAMESPACES` creation |
| [Network Persistence](proposal-network-persistence.md) | Partial: Ubuntu and Flatcar writers; RHEL blocked |
| [Resilient Provisioning](proposal-resilient-provisioning.md) | Partial: step retries and `BOOTY_RESUME` checkpoint |
| [Rescue Mode](proposal-rescue-mode.md) | Partial: `reboot`, `retry`, `shell`, `wait`; `RESCUE_TIMEOUT` not used |
| [SecureBoot Lifecycle](proposal-secureboot.md) | Partial: `SECUREBOOT_REENABLE` and digest pins; MOK enrollment not implemented |
| [Observability (Metrics)](proposal-observability.md) | Partial: endpoints exist; steps do not feed the collector |
| [LUKS Encryption](proposal-luks-encryption.md) | Planned: library only; the vars parser rejects `LUKS_*` keys |
| CAPRF legacy parity | Partial (open rows in the parity matrix): `CAPRF_LEGACY_COMPAT`, see [caprf-legacy-migration.md](caprf-legacy-migration.md) |

The [configuration reference](configuration.md) lists the keys that are
implemented today.

## Total Estimated Effort

~370-510 engineering days across all proposals (original + 25 new proposals).
Recommended implementation order follows the priority table above.
