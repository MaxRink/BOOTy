# Shared and upstream library adoption

BOOTy is a PID 1 provisioning agent, not a Kubernetes operator. Compare the
**linked agent packages**, not just a dependency module's `go.mod`, before adding
an import. Do not import Kubernetes or controller-runtime into the agent for
generic helpers.

## Adopted

- `github.com/telekom/t-caas-go-library/pkg/redact` v0.1.0 replaces repeated URL
  candidate-generation/redaction in `pkg/image/redact.go`,
  `pkg/network/http.go`, `pkg/caprf/client.go` and `pkg/auth/manager.go`. The package imports only the
  standard library. This release requires Go 1.26.6, already used by BOOTy's CI.
- Go 1.26 `log/slog.MultiHandler` replaces the local fan-out implementation.
  The existing type and constructor remain as compatibility aliases/glue.
  Current in-repository callers are tests, so this does not affect the agent.

Keep image/HTTP source context alongside the shared sanitized message. Keep OCI
reference policy local: image references are not general URLs. CAPRF builds
transport errors only from fixed parts: a `url.Error` with an allowlisted
operation, the request URL reduced to scheme, host and path, and a fixed cause
class. It keeps `context.Canceled`, `context.DeadlineExceeded` and the
`net.Error` timeout flag, and never copies the original message. Auth similarly retains recursively copied
URL, joined, cancellation and timeout errors; it uses only the shared sanitized
message, never the wrapper's original cause. Image/connectivity callers preserve
original causes for `errors.Is/As`; those causes must never be logged. Local
non-URL secret/path scrubbing also remains unchanged.

The shared package additionally strips opaque URLs, dangling query markers and
raw fragments, and sanitizes individual credential/query values. Diagnostic
wording changes, but source context, cancellation and safe outer errors remain.

## Controlled size comparison

Measurements use Go 1.26.6, `CGO_ENABLED=0`, Linux/amd64, `-buildvcs=false`, `-trimpath`, and
`-ldflags '-s -w -buildid= -X=main.Version=adoption-measure
-X=main.Build=35fb8e9a'`. Identical linker values exclude Git revision changes.

The initramfs comparison is a deterministic **micro-profile reconstruction**,
not a claim to have built every production flavor locally. Its `newc` archive
contains the same directories, console/null/ttyS0 device records, `/init`, and
CA bundle as the micro target; timestamps/owners/inodes are fixed and gzip uses
level 6 with a zero timestamp. The unchanged CA bundle SHA-256 is
`6033ac9902abeca1c1248c354ab3472a8a98fb6e22141ad381d1cff8e0ae0752`.
CI separately builds BOOTy's actual production artifacts.

| Stage | Agent bytes | Raw cpio bytes | Gzip bytes | Delta from previous stage |
|---|---:|---:|---:|---|
| Original implementation | 24,039,550 | 24,312,320 | 9,109,161 | baseline |
| Shared redaction | 24,035,454 | 24,308,224 | 9,107,800 | -4,096 agent/cpio; -1,361 gzip |
| Standard-library fan-out | 24,035,454 | 24,308,224 | 9,107,800 | zero; identical agent bytes |

`GOOS=linux GOARCH=amd64 go list -deps .` adds exactly one linked package:
`github.com/telekom/t-caas-go-library/pkg/redact`. It adds **no** Kubernetes,
controller-runtime, gofish, or other non-stdlib dependency to the agent.
The root library's module graph does contain Kubernetes dependencies; these
remain a module-download/tooling cost, not linked agent code.

## Evaluated alternatives

| Candidate | Why adoption is not justified in this change |
|---|---|
| `test/e2e/redfish/mock_server.go` → nested `pkg/redfish/redfishtest` v0.1.0 | The shared fake is stdlib-only; gofish is only a compatibility-test dependency. However, the unchanged `TestMockServerVirtualMedia` fails: BOOTy's existing Image-only InsertMedia request implies `Inserted=true`, whereas the shared fake leaves it false. Initial boot override values and collection names also differ. Do not weaken existing tests or introduce protocol-compatibility shims solely to delete a working test fixture. No nested module is retained. |
| `pkg/retry` → `cenkalti/backoff/v5` | Removed the unreferenced package instead of adding a replacement module. Provisioning and CAPRF retries remain unchanged. |
| `pkg/provision/retry.go` → `cenkalti/backoff/v5` | Backoff v5 is lighter than apimachinery, but its symmetric jitter, finite MaxInterval and permanent/context termination conventions differ from BOOTy's positive-only jitter, zero-as-uncapped policy, saturating delays and classified error messages. Preserving them needs local policy/backoff adapters, substantially reducing the deletion benefit. Retain the tested implementation rather than pulling Kubernetes wait into the agent. |
| `pkg/image/verify` → `opencontainers/go-digest` | Already present indirectly, but this package has no production importers. Its running hash, `Actual` output, case-normalization and mismatch diagnostics still require local code. The production streaming path in `pkg/image/stream.go` separately normalizes checksums and wipes corrupt disk metadata; replacing only the suggested helper would not simplify that path. |
| `pkg/executil` → direct `os/exec` | Already uses `os/exec`; the remaining PID registry prevents PID 1 from stealing managed child exit statuses. Output bounds and sanitized diagnostics are application policy. Removing the wrapper is not equivalent to upstream adoption. |
| `pkg/buildinfo` → `runtime/debug` | Already calls `debug.ReadBuildInfo`; flavor/linker defaults, formatting and component estimates are BOOTy-specific. |

## Verification

The coverage-first PR adds characterization tests on the original implementation.
Those tests are unchanged during adoption. Existing URL/error tests also cover
CAPRF status/commands/crash upload and image HTTP/GPG/OCI paths.

Run `make test`, `make lint`, and the existing `e2e` suites. In particular retain
the full provisioning matrix (raw/gzip/OCI/RAID), image streaming/cancellation,
HTTP connectivity, Redfish protocol tests, and Redfish/CAPRF integration tests.
The existing KVM matrix is required for the affected image/network paths.
