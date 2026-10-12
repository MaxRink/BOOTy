# BOOTy — Project Guidelines

Lightweight initramfs agent for bare-metal OS provisioning. Boots as PID 1, reads CAPRF-compatible machine configuration from `/deploy/vars`, and orchestrates disk imaging, network setup, and OS configuration. No standalone legacy HTTP server is shipped. Supports **dry-run** (`MODE=dry-run` or `DRY_RUN=true`) for non-destructive pre-flight validation.

## Architecture

- `cmd/` — CLI entry point (Cobra)
- `pkg/provision/` — provisioning orchestrator; keep step-count claims consistent with `TestProvisionStepCount`
- `pkg/config/` — MachineConfig, Provider interface, Status types
- `pkg/crash/` — Startup crash artifact collection, metadata manifests, CAPRF/S3 upload contracts
- `pkg/network/` — Pluggable networking: DHCP, static, FRR/EVPN, GoBGP, and Linux bonds
- `pkg/network/gobgp/` — Pure-Go BGP stack (underlay eBGP + overlay EVPN), three peering modes: unnumbered, dual, numbered
- `pkg/network/frr/` — FRR config rendering (legacy)
- `pkg/network/lldp/` — LLDP frame listener (raw AF_PACKET)
- `pkg/network/vlan/` — VLAN 802.1Q tagging via netlink
- `pkg/network/vrf/` — VRF configuration types and validation for multi-VRF network isolation
- `pkg/network/netplan/` — Netplan parsing and merge helpers
- `pkg/network/persist/` — Persist rendered network config into target OS
- `pkg/image/` — Multi-format image streaming (gzip, lz4, xz, zstd) + OCI registry
- `pkg/disk/` — Disk detection, partitioning, RAID, LVM, mount, NVMe namespace management
- `pkg/auth/` — JWT token manager (acquisition, renewal, backoff)
- `pkg/caprf/` — CAPRF controller client (status/log shipping)
- `pkg/cloudinit/` — Cloud-init NoCloud/ConfigDrive generation
- `pkg/firmware/` — Firmware inventory and vendor-specific NIC firmware helpers
- `pkg/health/` — Pre-provisioning hardware health checks
- `pkg/inventory/` — Hardware inventory from sysfs/procfs
- `pkg/ipmi/` — IPMI operations and helpers
- `pkg/kexec/` — GRUB config parsing, kexec load/execute
- `pkg/logging/` — Structured logging adapters and sinks
- `pkg/realm/` — Low-level syscalls (devices, mounts, networking)
- `pkg/rescue/` — Rescue mode policy and failure handling
- `pkg/ux/` — ASCII art and system info display
- `pkg/firmware/nic/` — Vendor-specific NIC firmware managers (Intel, Broadcom, Mellanox, ethtool fallback)

- `pkg/buildinfo/` — Binary build information
- `pkg/debug/` — Debug utilities
- `pkg/drivers/` — Architecture-aware kernel driver management
- `pkg/efi/` — EFI variable operations

- `pkg/executil/` — Shared command execution wrappers
- `pkg/secureboot/` — Secure Boot chain setup
- `pkg/serialconsole/` — Serial console resolution (explicit override, ACPI SPCR, device tree, sysfs, DMI evidence)
- `pkg/system/` — System-level operations
- `pkg/telemetry/` — Telemetry and metrics collection
- `pkg/tpm/` — TPM/TPM2 operations and cryptenroll
- `pkg/utils/` — Shared utility functions
- `test/e2e/` — E2E tests with ContainerLab and vrnetlab topologies

## Build and Test

```bash
make build              # Compile binary
make test               # Unit tests with 40% coverage gate
make lint               # golangci-lint v2
make fmt                # gofmt + goimports
make fmt-check          # verify gofmt + goimports without rewriting

# Initramfs build flavors (Linux/Docker)
make dockerx86          # Full FRR+tools (~80 MB)
make gobgp              # GoBGP variant (no FRR)
make slim               # DHCP-only (~15 MB)
make micro              # Pure Go (~10 MB)
make iso                # Bootable ISO
make gobgp-iso          # GoBGP ISO

# E2E tests (require ContainerLab, Linux only)
make clab-up && make test-e2e-integration
make clab-gobgp-up && make test-e2e-gobgp
make clab-vrnetlab-up && make test-e2e-vrnetlab
```

## Code Style

- **Go 1.26+**, Linux-only code uses `//go:build linux`
- **Logging**: `log/slog` only — never `fmt.Print` or `logrus`
- **Errors**: `fmt.Errorf("lowercase context: %w", err)` — lowercase first letter, always wrap
- **Naming**: `ctx` for context, `cfg` for config, `mgr` for manager
- **Imports**: stdlib → external → internal (blank line between groups)

## Reuse upstream before writing helpers

Check in order: standard library → Kubernetes/controller-runtime/client-go/
apimachinery packages already in use → Flux `github.com/fluxcd/pkg` → other
established libraries → `telekom/t-caas-go-library` → custom code last. For
BOOTy's retry, digest,
execution, HTTP, Redfish, netlink, config, and build-info concerns, follow the
package table and migration candidates in [AGENTS.md](../AGENTS.md#reuse-upstream-libraries-before-writing-helpers).
Use wrappers only for glue repeated across repositories; contribute that shared
glue to `telekom/t-caas-go-library` rather than duplicating it.

URL diagnostics in image, connectivity, CAPRF and auth use the stdlib-only
`github.com/telekom/t-caas-go-library/pkg/redact`; logging fan-out uses Go's
`slog.MultiHandler`. Keep sanitized auth/CAPRF error trees local and never log
the original causes retained by `redact.Wrap`. See
[adoption decisions](../docs/library-adoption.md) for linked dependency limits.

## Testing Requirements

Every feature or bug fix **must** include tests at the appropriate level. Do not
consider work complete until tests are written, passing, and cover the change.

| Test Level | When Required | How to Run |
|------------|---------------|------------|
| **Unit tests** | All code changes — minimum 40% coverage gate | `make test` |
| **Linux E2E** (`linux_e2e`) | Disk, mount, loop device, or partition code | `sudo -E env "PATH=$PATH:/usr/sbin:/sbin" "$(which go)" test -tags linux_e2e -v -count=1 -timeout 5m ./test/e2e/linux/...` (requires root) |
| **ContainerLab E2E** (`e2e_integration`) | Network modes, FRR, DHCP, bonds, static | `make clab-up && make test-e2e-integration` |
| **GoBGP E2E** (`e2e_gobgp`) | GoBGP peering, tiers, PeerMode changes | `make clab-gobgp-up && make test-e2e-gobgp` |
| **Boot E2E** (`e2e_boot`) | Provisioning orchestrator, step ordering | `make clab-boot-up && make test-e2e-boot` |
| **vrnetlab / QEMU** (`e2e_vrnetlab`) | Full boot flow, kexec, EVPN fabric, ISO boot | `make clab-vrnetlab-up && make test-e2e-vrnetlab` |
| **GoBGP vrnetlab** (`e2e_gobgp_vrnetlab`) | GoBGP QEMU VMs with FRR fabric containers (all PeerModes) | `make clab-gobgp-vrnetlab-up && make test-e2e-gobgp-vrnetlab` |
| **Production smoke** (`e2e_production`) | CI-proven production-like topology: CAPRF/FRR mode, BGP, EVPN AF, VXLAN, overlay reachability, gateway FDB/route. VRF, DCGW BFD, and EVPN type-5 remain unproven limitations. | `make clab-production-up && make test-e2e-production` |

### When to Use KVM / QEMU Testing

Use vrnetlab (QEMU-backed) E2E tests when the change:
- Affects the **boot sequence** (PID 1 init, kexec, reboot)
- Modifies **image streaming** to block devices (not just unit-testable decompression)
- Changes **EVPN overlay** behavior that requires FRR fabric control planes
- Alters **bootloader installation** (GRUB, systemd-boot, ESP partitions)
- Touches **Redfish integration** or ISO boot paths
- Cannot be validated with ContainerLab containers alone

vrnetlab tests run QEMU VMs inside ContainerLab — they require a Linux host
with KVM support. CI runs these on `ubuntu-latest` GitHub Actions runners.

### Test Checklist

- [ ] Unit tests cover new/changed logic with table-driven style
- [ ] Test helpers call `t.Helper()` as first line
- [ ] E2E tests use correct build tags
- [ ] KVM/QEMU tests included when touching boot flow, disk imaging, or EVPN
- [ ] No `time.Sleep` in unit tests — use channels, tickers, or mocks
- [ ] `make test` passes locally before pushing

## Conventions

- **Tests**: table-driven with `t.Helper()` in helpers; E2E tests use build tags (`e2e`, `e2e_integration`, `e2e_boot`, `e2e_vrnetlab`, `e2e_gobgp`)
- **Network code**: all `netlink` and raw-socket code is Linux-only (build-tagged)
- **Error handling**: return errors for fatal conditions, log and continue for optional features
- **Concurrency**: use `atomic` operations (not mutexes) for simple counters; channels for signaling
- **Security**: gosec enabled — exceptions documented in `.golangci.yml` for intentional device ops (G115, G204, G301, G304, G306, G706)
- **Function limits**: max 80 lines / 50 statements (exceptions: `main.go`, `pkg/ux/captain.go`)

## Code Review

Copilot PR reviews use **review personas** for focused feedback. See
[AGENTS.md](AGENTS.md) for the full setup.

- **Security** — vulns, auth, crypto, file permissions, gosec exceptions
- **Networking** — BGP, EVPN, netlink, VRF, build tags, PeerMode coverage
- **Provisioning** — disk safety, image checksums, step ordering, kexec
- **General** — slog, error wrapping, imports, tests, function complexity

Review instructions: [`instructions/review.instructions.md`](instructions/review.instructions.md)

## Agents & Prompts

| Type | Name | Purpose |
|------|------|---------|
| Agent | `caprf` | Cross-repo BOOTy ↔ CAPRF coordination |
| Agent | `security-reviewer` | Security-focused PR review |
| Agent | `networking-reviewer` | Networking-focused PR review |
| Agent | `provisioning-reviewer` | Provisioning-focused PR review |
| Prompt | `proposal` | Scaffold a design proposal document |
| Prompt | `review` | Run a structured code review |

## Related Repositories

This workspace includes companion repos — see their own README files for details:

- **cluster-api-provider-redfish** — Kubernetes CAPI provider that BOOTy reports to (Go, kubebuilder, controller-runtime)
- **virtual-bm4x** — Virtual bare-metal test environment (Kind + ContainerLab topologies)
- **gcp-dev-env** — GCP infrastructure with GKE and CI runners (OpenTofu, Ansible, Packer)
