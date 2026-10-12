---
applyTo: "test/e2e/**"
description: "E2E test conventions for BOOTy — build tags, ContainerLab topologies, helper patterns, container naming. USE WHEN: writing or modifying E2E tests."
---

# E2E Tests

## Build Tags

Every E2E test file **must** start with a build tag matching its topology:

| Tag | Topology | Make target |
|-----|----------|-------------|
| `e2e` | Generic (no infra needed) | `make test-e2e` |
| `e2e_integration` | FRR ContainerLab | `make clab-up && make test-e2e-integration` |
| `e2e_boot` | Boot ContainerLab | `make clab-boot-up && make test-e2e-boot` |
| `e2e_gobgp` | GoBGP ContainerLab | `make clab-gobgp-up && make test-e2e-gobgp` |
| `e2e_gobgp_type5` | Type-5 per-machine leaf | `make clab-type5-up && make test-e2e-type5` |
| `e2e_vrnetlab` | QEMU vrnetlab | `make clab-vrnetlab-up && make test-e2e-vrnetlab` |
| `e2e_gobgp_vrnetlab` | GoBGP QEMU vrnetlab | `make clab-gobgp-vrnetlab-up && make test-e2e-gobgp-vrnetlab` |
| `e2e_production` | CI-proven production smoke: CAPRF/FRR mode, BGP, EVPN AF, VXLAN, overlay reachability, gateway FDB/route | `make clab-production-up && make test-e2e-production` |
| `linux_e2e` | Linux root access | `sudo -E env "PATH=$PATH:/usr/sbin:/sbin" "$(which go)" test -tags linux_e2e -v -count=1 -timeout 5m ./test/e2e/linux/...`, then the same with `-run '^TestType5Netlink' ./pkg/network/gobgp/` |

## Helper Functions

All helpers must call `t.Helper()` as their first line:

```go
func requireBootLab(t *testing.T) {
    t.Helper()
    // Check topology availability, fail with a local repro command if not deployed
}
```

Common helpers:
- `requireXxxLab(t)` — fail with a local repro command if topology is not deployed
- `xxxDockerExec(t, container, args...)` — run command in container, fatal on error
- `xxxDockerExecRaw(t, container, args...)` — run command, return error (non-fatal)
- `waitForLogEntry(t, container, entry, timeout)` — poll container logs
- `dumpDebugState(t)` — collect BGP state on test failure

## Container Naming

Pattern: `clab-booty-[topology]-[component]`

Examples: `clab-booty-boot-lab-booty-provision`, `clab-booty-gobgp-lab-spine01`

Define container names as constants at the top of each test file.

## Polling Pattern

Use timeout + interval loops, not channels, for container state checks. Keep sleeps deadline-bound and include diagnostics on timeout:

```go
deadline := time.Now().Add(timeout)
for time.Now().Before(deadline) {
    // check condition
    time.Sleep(2 * time.Second)
}
dumpDebugState(t)
t.Fatalf("condition not met within %s", timeout)
```

Typical timeouts: 30s for log polling, 60s for BGP convergence.

## Linux E2E Helpers

- `requireRoot(t)` — skip if not root (`os.Getuid() != 0`)
- `createLoopDevice()` — sparse file → sfdisk GPT → losetup → cleanup
