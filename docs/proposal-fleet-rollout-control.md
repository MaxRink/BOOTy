# Fleet Rollout Control for BOOTy Versions

## Status

Proposal. Design only. Nothing in this document is implemented.

## Priority

P1

## Summary

Roll out new BOOTy versions safely: canary cohorts, rate limits, automatic
halt on failure rate, and a policy gate that decides per host whether it may
be provisioned with a given BOOTy and image (for example "block known-bad
firmware", "block hosts without TPM for attested clusters"). The policy is
evaluated in CAPRF with OPA (Rego) or CEL before CAPRF boots BOOTy, and
optionally re-checked by BOOTy with in-band facts.

## Problem and motivation

- CAPRF selects BOOTy or the legacy ramdisk by a feature gate and a per-host
  annotation (`BootyProvisioner` gate, provisioner type annotation on the
  `feat/booty-consolidated` branch). A new BOOTy release then hits every
  selected host at once.
- Known-bad combinations (firmware, NIC, disk model) are tribal knowledge.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| Selection | Feature gate and per-host annotation. | CAPRF branch `pkg/featuregate/featuregate.go`, `api/v1alpha1/redfishhost_provisioner.go` |
| BOOTy version | Pinned BOOTy ISO tree in the CAPRF image. | CAPRF branch `docs/booty-feature-test-matrix.md` |
| Policy | None. | |

## Goals

- BOOTy version as data: a `BootyRelease` object with artifact digests.
- Cohorts (canary, early, broad) by label selector, with soak times.
- Rate limit: at most N concurrent provisions per failure domain and release.
- Auto-halt when the failure rate of a release exceeds a threshold
  (metrics from [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)).
- Policy gate with clear deny reasons.

## Non-goals

- OS image rollout (CAPI MachineDeployments own that).

## Design options

- Policy engine: A) OPA as a sidecar or library (`github.com/open-policy-agent/opa`,
  verify size for CAPRF); B) CEL (`cel-go`, already used in t-co node tuning
  governance); C) Kyverno policies on CAPRF objects. Recommendation: B for
  simple rules inside CAPRF (small, typed, already in the T-CaaS stack); A if
  security wants shared Rego policies.
- In-band re-check: BOOTy evaluates a small rule set (CEL in Go, verify size)
  against in-band facts (firmware versions, quirks) before destructive steps.

Example rule (CEL): `!(host.inventory.bios.version in policy.badBios)`.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `POLICY` | path or JSON | In-band rules. |
| `POLICY_ON_DENY` | `fail`, `warn` | |

### CAPRF and bm4x impact

- New CRDs: `BootyRelease`, `BootyRollout` (cohorts, rate, halt threshold),
  `ProvisionPolicy`.
- Condition `PolicyDenied` with reason on `RedfishMachine`.
- bm4x provides host classes and labels for cohorts.

## Security considerations

- Policies are privileged config; restrict RBAC.
- A deny must never leave a host half-provisioned; evaluate before any
  destructive step.

## Testing strategy

- Unit: CEL rule evaluation with fixtures; rollout state machine.
- kindmetal and CAPRF e2e: two releases, canary cohort, injected failures
  halt the rollout.
- KVM: in-band deny before `wipe-disks`.

## Phased rollout

1. CAPRF MR: `BootyRelease` and version selection by label.
2. CAPRF MR: rate limits.
3. CAPRF MR: policy gate with CEL.
4. CAPRF MR: auto-halt on metrics.
5. PR: in-band re-check in BOOTy.

## Effort estimate

| Phase | Days |
|-------|------|
| Release object and selection | 4 to 6 |
| Rate limits | 2 to 3 |
| Policy gate | 4 to 6 |
| Auto-halt | 3 to 5 |
| In-band check | 3 to 4 |
| **Total** | **16 to 24** |

## Open questions

- Where does the known-bad firmware list come from (vendor advisories, the
  quirks database)?

## Related proposals

- [proposal-hardware-quirks.md](proposal-hardware-quirks.md)
- [proposal-firmware-lifecycle.md](proposal-firmware-lifecycle.md)
- [proposal-provisioning-metrics.md](proposal-provisioning-metrics.md)
- [proposal-supply-chain-trust.md](proposal-supply-chain-trust.md)
