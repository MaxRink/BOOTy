# Contributing to BOOTy

Thank you for your interest in contributing! This document covers the development workflow and coding standards.

## Development Setup

1. Install Go 1.26+
2. Clone the repository:
   ```bash
   git clone https://github.com/telekom/BOOTy.git
   cd BOOTy
   ```
3. Install dependencies:
   ```bash
   go mod download
   ```

## Dependency Updates

Dependabot tracks direct and indirect Go modules, workflow actions, and base
images in all six Dockerfiles through the four configured Docker directories.
ContainerLab `*.clab.yml` topologies are not Kubernetes manifests and are not
updated by Dependabot's Docker ecosystem. Keep their external FRR, Alpine,
nginx and network-multitool image references synchronized manually.

Package pins inside Dockerfile `RUN` instructions, downloaded tool/kernel
versions, checksums and shell-script image references also need manual updates.
Locally built `booty-*` images are test artifacts, not upstream dependencies.
Validate dependency changes with the existing CI and KVM boot matrices before
merging.

## Building

```bash
# Build the binary
make build

# Build the initramfs Docker image (default: full FRR+tools)
docker build -t booty -f initrd.Dockerfile .

# Build bootable ISO (for Redfish virtual media)
docker build --target=iso -f initrd.Dockerfile -o type=local,dest=. .

# Build slim initramfs (DHCP-only, no FRR)
docker build --target=slim -f initrd.Dockerfile -o type=local,dest=. .
```

## Testing

```bash
# Run all tests
make test

# Run tests with coverage
go test -cover ./...

# Run a specific package's tests
go test ./pkg/image/...

# Run E2E integration tests (requires ContainerLab, Linux only)
make clab-test-image
make clab-up && make test-e2e-integration
make clab-gobgp-up && make test-e2e-gobgp
make clab-type5-up && make test-e2e-type5
make clab-boot-up && make test-e2e-boot
make clab-dhcp-up && make test-e2e-dhcp
make clab-bond-up && make test-e2e-bond
make clab-lacp-up && make test-e2e-lacp
make clab-static-up && make test-e2e-static
make clab-multi-nic-up && make test-e2e-multi-nic
make clab-vrnetlab-up && make test-e2e-vrnetlab
make clab-gobgp-vrnetlab-up && make test-e2e-gobgp-vrnetlab
```

### Test Requirements

- File-backed QEMU serial checks use `-display none -monitor none`, not
  `-nographic`: the latter enables a stdio monitor that leaves the Linux runner's
  shared stdout pipe nonblocking and can make later log writes fail with
  `Resource temporarily unavailable`. The workflow asserts blocking stdout
  after QEMU exits, then evaluates the unchanged serial-log boot assertions.
- **Linux only**: All tests in `pkg/provision/`, `pkg/disk/`, `pkg/network/`, and `pkg/realm/` use `//go:build linux`. Use `GOOS=linux` for compilation on macOS/Windows, but execution requires Linux.
- **Coverage gate**: `make test` enforces a **40% coverage** minimum. New code should include tests to maintain or increase coverage.
- **Race detector**: `-race` is enabled by default in all test targets.
- **Table-driven tests**: Prefer `tests := []struct{...}` with subtests over individual test functions.
- **Test helpers**: Use `t.Helper()` in all helper functions so failures report the caller's line.

### Test Levels

Every feature or bug fix **must** include tests at the appropriate level:

| Level | Build Tag | When Required | Command |
|-------|-----------|---------------|----------|
| **Unit** | *(none)* | All code changes | `make test` |
| **Linux E2E** | `linux_e2e` | Disk, mount, loop device, partition code | `go test -tags linux_e2e` (root) |
| **ContainerLab** | `e2e_integration` | Network modes, FRR, DHCP, bonds, static | `make clab-up && make test-e2e-integration` |
| **GoBGP** | `e2e_gobgp` | GoBGP peering, tiers, PeerMode changes | `make clab-gobgp-up && make test-e2e-gobgp` |
| **Boot** | `e2e_boot` | Provisioning orchestrator, step ordering | `make clab-boot-up && make test-e2e-boot` |
| **vrnetlab (QEMU)** | `e2e_vrnetlab` | Full boot flow, kexec, EVPN fabric, ISO | `make clab-vrnetlab-up && make test-e2e-vrnetlab` |
| **GoBGP vrnetlab** | `e2e_gobgp_vrnetlab` | GoBGP with real switch VMs | `make clab-gobgp-vrnetlab-up && make test-e2e-gobgp-vrnetlab` |
| **Production** | `e2e_production` | VRF, DCGW, BFD, production-like topology | `make clab-production-up && make test-e2e-production` |

### When to Use KVM / QEMU Tests

Use vrnetlab (QEMU-backed) E2E tests when the change:
- Affects the **boot sequence** (PID 1 init, kexec, reboot)
- Modifies **image streaming** to block devices
- Changes **EVPN overlay** behavior requiring real switch control planes
- Alters **bootloader installation** (GRUB, systemd-boot, ESP)
- Touches **Redfish integration** or ISO boot paths

vrnetlab tests require a Linux host with KVM support. CI runs these on
`ubuntu-latest` GitHub Actions runners.

## Linting

```bash
make lint
```

This runs [golangci-lint](https://golangci-lint.run/) with the configuration in `.golangci.yml`.
The first run installs the golangci-lint version that CI uses into `bin/`.
Like CI, it builds and runs the linter with the Go version from `go.mod`. The
`go` command downloads that toolchain when your local Go is different.
Do not run the linter with a newer local Go. With Go 1.27, golangci-lint
v2.12.2 panics in the staticcheck `fact_purity` analyzer, and a golangci-lint
built with Go 1.26 fails with "file requires newer Go version go1.27".
The lint always checks the Linux build, also on macOS.

Key lint rules:
- **cyclop**: Maximum function complexity of 15
- **funlen**: Maximum 80 lines / 50 statements per function

## Coding Standards

- **Logging**: Use `log/slog` — never `fmt.Print` for operational logs or `logrus`.
- **Errors**: Use `%w` in `fmt.Errorf` for error wrapping. Start error messages with a lowercase letter.
- **Imports**: Group into stdlib, external, and internal blocks separated by blank lines.
- **Build tags**: Linux-specific code must have `//go:build linux` at the top of the file.
- **Naming**: Use `ctx` for `context.Context`, `cfg` for config structs, `mgr` for managers.
- **Tests**: Prefer table-driven tests. Use `t.Helper()` in test helpers. E2E tests use build tags (`e2e`, `e2e_integration`).

## Pull Request Process

1. Fork the repository and create a feature branch from `main`.
2. Make your changes with clear, focused commits.
3. Ensure `make lint` and `make test` pass.
4. Include tests at the appropriate level (see Test Levels above).
5. Update documentation if your change:
   - Adds a new package, feature gate, or CLI flag
   - Changes the provisioning step count
   - Adds a new Make target or build flavor
   - Creates a new E2E topology or build tag
6. Open a PR with a description of what changed and why.
7. A maintainer will review and merge once CI is green.

### Documentation Checklist

When your PR introduces new functionality, verify:

- [ ] New packages are listed in `README.md` Project Structure
- [ ] New feature gates are in the `README.md` Feature Gates table
- [ ] New Make targets are documented in `README.md` and this file
- [ ] New build tags are added to `.github/instructions/e2e-tests.instructions.md`
- [ ] `.github/copilot-instructions.md` architecture section is up to date
- [ ] Step count references are consistent with the source-of-truth
      `TestProvisionStepCount` regression in `pkg/provision/orchestrator_test.go`
      across `README.md`, `copilot-instructions.md`, and `CONTRIBUTING.md`
- [ ] New agents/prompts/instructions are listed in `.github/AGENTS.md`

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
