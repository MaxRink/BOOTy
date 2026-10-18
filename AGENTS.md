# BOOTy — Coding Agent Guidance

## OVERVIEW
Lightweight initramfs agent for bare-metal OS provisioning (not a K8s operator). Boots as PID 1, orchestrates provisioning steps including RAID/NVMe setup, disk imaging, shared data mounting, optional sysext loading, and BGP/EVPN networking. After the provisioning orchestrator reports success, `main.go` chooses kexec, hard reboot, or power-off handling.

## STRUCTURE
| Directory | Purpose |
|-----------|---------|
| `pkg/network/` | Pluggable networking: `gobgp` (pure Go), `frr` (legacy), `lldp` (raw frames) |
| `pkg/provision/` | State machine for RAID/NVMe/image/disk/shared-data/sysext/network provisioning flow |
| `pkg/crash/` | Startup crash artifact collection and host metadata correlation |
| `pkg/realm/` | Low-level Linux primitives: syscalls, mounts, device creation |
| `pkg/firmware/` | Firmware inventory and vendor-specific NIC firmware helpers |
| `test/e2e/clab/` | ContainerLab topologies for network/provisioning integration tests |

## WHERE TO LOOK
| Task | Location |
|------|----------|
| Entry point / PID 1 init | `main.go` (orchestrator logic) |
| Network stack selection | `main.go` (setupNetworkMode) |
| Disk / Partitioning | `pkg/disk/` |
| Image streaming/OCI | `pkg/image/` |
| BGP/EVPN logic | `pkg/network/gobgp/` or `pkg/network/frr/` |
| E2E tests | `test/e2e/integration/` |

## CONVENTIONS
- **Linux-only**: Files must have `//go:build linux` at the top.
- **Logging**: Use `log/slog` exclusively. Never use `fmt.Print` or `logrus`.
- **Complexity**: Max 80 lines / 50 statements per function (strict `funlen` lint).
- **Concurrency**: Prefer `atomic` operations over mutexes for simple state.
- **E2E Tests**: Use specific build tags: `e2e_integration`, `e2e_gobgp`, `e2e_boot`, `e2e_vrnetlab`.
- **Changelog**: Never edit `CHANGELOG.md` in a PR. The squash-merged PR title (Conventional Commit) becomes the changelog line; put upgrade, breaking or behaviour notes in a `changes/<PR>.md` fragment. See `CONTRIBUTING.md#changelog`.

## ANTI-PATTERNS
- **No interactive prompts**: The agent runs unattended; all logic must be automated.
- **No unbounded `time.Sleep` in tests**: Unit tests should use channels, tickers, or context cancellation. E2E polling may use deadline-bound sleeps with diagnostics.
- **No shell-outs**: Prefer pure Go or direct syscalls (e.g. `unix.FinitModule`) over `exec.Command`.

## Reuse upstream libraries before writing helpers

Before adding a helper, check in this order: Go standard library → Kubernetes,
controller-runtime, client-go, and apimachinery packages already in use → Flux
`github.com/fluxcd/pkg` → other well-known upstream libraries → the shared
`telekom/t-caas-go-library` packages → custom code last. Use upstream APIs
directly when they fit; document any important behavior that prevents reuse.
This rule applies even if an adoption change in BOOTy has not merged.

| Concern | Prefer | BOOTy-specific guidance |
|---------|--------|-------------------------|
| Provisioning retry/backoff | [`github.com/cenkalti/backoff/v5`](https://pkg.go.dev/github.com/cenkalti/backoff/v5) | Reuse `Retry` and bounded retry options; keep provisioning-specific retry classification and policies local. |
| Context-aware waits | [`context`](https://pkg.go.dev/context), [`time`](https://pkg.go.dev/time) | Make waits cancellable and bounded; do not add a custom retry scheduler. |
| Checksums and OCI digests | [`github.com/opencontainers/go-digest`](https://pkg.go.dev/github.com/opencontainers/go-digest) | Use `Parse` and `Digest.Verifier`; check copy/read errors and incomplete input before accepting verification. |
| Process execution | [`os/exec`](https://pkg.go.dev/os/exec), [`k8s.io/utils/exec`](https://pkg.go.dev/k8s.io/utils/exec), [`k8s.io/utils/exec/testing`](https://pkg.go.dev/k8s.io/utils/exec/testing) | Prefer direct `CommandContext` calls and injectable upstream fakes over a new execution wrapper. Keep PID tracking, output limits, and sanitized diagnostics local where needed. |
| Structured logging | [`log/slog`](https://pkg.go.dev/log/slog), [`github.com/samber/slog-multi`](https://pkg.go.dev/github.com/samber/slog-multi) | Use `slog.NewMultiHandler` on Go 1.26+; consider slog-multi only when supporting older Go. Avoid a custom multiplexer. |
| HTTP clients and transports | [`net/http`](https://pkg.go.dev/net/http), [`net/url`](https://pkg.go.dev/net/url) | Clone transports when customizing them; set explicit TLS, timeout, and request-context behavior. `URL.Redacted` does not scrub every sensitive field. |
| HTTP tests | [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) | Use standard test servers and clients before building protocol fixtures. |
| Redfish | [`github.com/stmcginnis/gofish`](https://pkg.go.dev/github.com/stmcginnis/gofish), [`github.com/stmcginnis/gofish/schemas`](https://pkg.go.dev/github.com/stmcginnis/gofish/schemas) | Use gofish directly for Redfish inventory, reset, boot, and virtual media; keep BOOTy vendor policy local. |
| Redfish test fixtures | [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) | Shared [`pkg/redfish/redfishtest`](https://pkg.go.dev/github.com/telekom/t-caas-go-library/pkg/redfish/redfishtest) is released in a nested module; verify its protocol contract before adoption. See [the evaluated incompatibilities](docs/library-adoption.md). |
| Linux networking | [`github.com/vishvananda/netlink`](https://pkg.go.dev/github.com/vishvananda/netlink) | Use the existing netlink API for link/address/route operations; keep BOOTy-specific orchestration and policy local. |
| IP addresses and prefixes | [`net/netip`](https://pkg.go.dev/net/netip), [`go4.org/netipx`](https://pkg.go.dev/go4.org/netipx) | Prefer `netip` parsing/comparison and `netipx` ranges/sets; preserve BOOTy's address-family and host-boundary policy. |
| Shared IP arithmetic | [`github.com/telekom/t-caas-go-library/pkg/netutil`](https://pkg.go.dev/github.com/telekom/t-caas-go-library/pkg/netutil) | Merged package for checked arithmetic, budgeted prefix subdivision, and repeated usable/broadcast conventions; verify semantics before migrating local policy. |
| Config decoding | [`encoding/json`](https://pkg.go.dev/encoding/json), [`sigs.k8s.io/yaml`](https://pkg.go.dev/sigs.k8s.io/yaml), [`go.yaml.in/yaml/v3`](https://pkg.go.dev/go.yaml.in/yaml/v3) | Use native decoders, retaining existing strictness, validation, and trailing-document behavior; do not silently tighten accepted configs. |
| Build metadata | [`runtime/debug`](https://pkg.go.dev/runtime/debug) | Use `ReadBuildInfo` and VCS settings; keep BOOTy's linker variables and presentation local. |
| Unix syscalls | [`golang.org/x/sys/unix`](https://pkg.go.dev/golang.org/x/sys/unix) | Prefer direct syscalls over shelling out or wrapping a single syscall. |

The source decision guide is [`telekom/t-caas-go-library/docs/upstream-libraries.md`](https://github.com/telekom/t-caas-go-library/blob/main/docs/upstream-libraries.md) in the public library. Recommendations are not dependencies to add automatically: check license, dependency weight, Go compatibility, and semantics. `pkg/redact` is used for shared URL diagnostics; `pkg/netutil` may help with repeated IP arithmetic. Other packages such as `pkg/patch`, `pkg/remoteclient`, `pkg/namespaceselector`, and `pkg/discovery/tracker` target Kubernetes controllers or API clients and are not a fit for BOOTy's PID 1 provisioning agent.

Convenience wrappers belong in BOOTy only when the same glue demonstrably
repeats across multiple repositories. In that case, contribute the shared
wrapper to `telekom/t-caas-go-library` instead of duplicating it. Existing
helpers to evaluate as migration candidates (no migration is part of this
guidance change) are `pkg/provision/retry` → backoff/v5;
`pkg/image/verify` → go-digest; `pkg/executil` → `os/exec` and `k8s.io/utils/exec`;
`pkg/buildinfo` → `runtime/debug`; and `pkg/config/loader` → native decoders.
URL diagnostics already use shared `pkg/redact`; `pkg/logging/multi` already
uses stdlib fan-out. Use gofish directly for Redfish; consider
the shared HTTP test fixture only for `test/e2e/redfish/mock_server.go`,
not as another Redfish client. Preserve local validation, vendor rules, and
sanitization where upstream APIs do not provide them. See
[library adoption decisions and size measurements](docs/library-adoption.md)
for evaluated candidates, retained policies and linked dependency constraints.

## COMMANDS
| Action | Command |
|--------|---------|
| Compile | `make build` |
| Unit Tests | `make test` (40% coverage gate) |
| Formatter Check | `make fmt-check` |
| Full Initramfs | `make dockerx86` |
| Build ISO | `make iso` |
| E2E Network | `make clab-up && make test-e2e-integration` |
| E2E Boot | `make clab-boot-up && make test-e2e-boot` |
| E2E QEMU/KVM | `make clab-vrnetlab-up && make test-e2e-vrnetlab` |
| Changelog preview | `make changelog` (needs git-cliff) |

## NOTES
- **PID 1**: BOOTy manages its own mounts/devices in early init. See `main.go:setupMountsAndDevices`.
- **Dry Run**: Supports `MODE=dry-run` or `DRY_RUN=true` for non-destructive validation.
- **Copilot**: See `.github/AGENTS.md` for specialized review personas (Security, Networking, Provisioning).
