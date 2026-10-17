# Proposal: Enhanced Attestation

## Status: Phase 1 Implemented (PR #57, #65)

## Priority: P2

## Dependencies: [TPM Attestation](proposal-tpm-attestation.md), [SecureBoot Full Chain](proposal-secureboot-full-chain.md)

## Summary

Extend the existing TPM attestation proposal with BOOTy self-measurement,
OS image measurement, remote attestation quote generation, golden PCR value
management in CAPRF CRDs, TCG2 event log capture, and optional Keylime
integration for continuous attestation.

## Motivation

The existing TPM attestation proposal covers basic PCR extension and quote
generation. This companion proposal adds production-grade capabilities:

| Gap | Impact |
|-----|--------|
| No BOOTy self-measurement | Can't verify provisioner integrity |
| No image measurement | Can't prove which OS was written to disk |
| No golden PCR management | No baseline to verify quotes against |
| No event log | Can't debug attestation failures |
| No continuous attestation | Only boot-time verification |

## Design

### Measurement Architecture

```
┌─────────────────────────────────────────────────────────┐
│ BOOTy Self-Measurement                                  │
│                                                         │
│  1. Hash own binary (/init) → extend PCR[23]            │
│  2. Hash provisioning config → extend PCR[23]           │
│  3. Stream OS image → hash while streaming → PCR[23]    │
│  4. Hash cloud-init config if present → PCR[23]         │
│  5. Generate TPM2 attestation quote                     │
│  6. Send quote + event log to CAPRF                     │
└──────────────────────────┬──────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────┐
│ CAPRF Attestation Verification                          │
│                                                         │
│  1. Verify quote signature (EK certificate chain)       │
│  2. Parse event log                                     │
│  3. Replay event log → compute expected PCR values      │
│  4. Compare against golden PCR policy                   │
│  5. Admit machine to cluster or quarantine              │
└─────────────────────────────────────────────────────────┘
```

### BOOTy Measurement Operations

```go
// pkg/tpm/measure.go
package tpm

import (
    "context"
    "crypto/sha256"
    "fmt"
    "io"
    "os"
)

// BOOTy extends all its events into one PCR (docs/usage-security-telemetry.md): PCR 14 is
// the shim MokList and PCR 15 belongs to systemd in the target OS.
const (
    PCRSelfMeasurement    = 23 // BOOTy binary + config hash
    PCRImageMeasurement   = 23 // OS image hash
)

// Measurer extends TPM PCR values with provisioning artifacts.
type Measurer struct {
    tpm *TPM
    log *slog.Logger
}

// MeasureSelf hashes the BOOTy binary and provisioning config,
// extending PCR[23].
func (m *Measurer) MeasureSelf(ctx context.Context) error {
    // Hash /init (BOOTy binary)
    initHash, err := hashFile("/init")
    if err != nil {
        return fmt.Errorf("hash BOOTy binary: %w", err)
    }
    if err := m.tpm.ExtendPCR(PCRSelfMeasurement, initHash); err != nil {
        return fmt.Errorf("extend PCR[%d] with BOOTy hash: %w", PCRSelfMeasurement, err)
    }

    // Hash provisioning config
    configHash, err := hashFile("/deploy/vars")
    if err != nil {
        m.log.Info("no config to measure", "error", err)
        return nil
    }
    return m.tpm.ExtendPCR(PCRSelfMeasurement, configHash)
}

// MeasureImage hashes OS image data and extends PCR[23].
// Called via io.TeeReader during image streaming.
func (m *Measurer) MeasureImage(ctx context.Context, imageHash []byte) error {
    return m.tpm.ExtendPCR(PCRImageMeasurement, imageHash)
}
```

### Event Log

```go
// pkg/tpm/eventlog.go
package tpm

// TCG2EventLog captures all PCR extension events in TCG2 format.
type TCG2EventLog struct {
    Events []Event `json:"events"`
}

type Event struct {
    PCRIndex  int    `json:"pcrIndex"`
    EventType string `json:"eventType"` // "BOOTy_SELF", "BOOTy_CONFIG", "BOOTy_IMAGE"
    DigestHex string `json:"digestHex"`
    Data      string `json:"data,omitempty"` // human-readable description
}
```

### Golden PCR Management

```yaml
# CAPRF CRD extension — golden PCR values
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: RedfishHost
metadata:
  name: server-001
spec:
  attestation:
    enabled: true
    goldenPCRs:
      14: "sha256:abc123..."  # expected BOOTy + config hash
      15: "sha256:def456..."  # expected OS image hash
    keylimeAgent: false
    quarantineOnFailure: true
```

### Required Binaries in Initramfs

| Binary | Package | Purpose | Initramfs Flavor | Already Present? |
|--------|---------|---------|-----------------|-----------------|
| `tpm2_pcrread` | `tpm2-tools` | Read PCR values (debug/verify) | full, gobgp | **No — add** (from cryptenroll proposal) |
| `tpm2_quote` | `tpm2-tools` | Generate attestation quote (fallback) | full, gobgp | **No — add** |
| `tpm2_eventlog` | `tpm2-tools` | Parse TPM2 event log (debug) | full only | **No — optional** |

**Kernel modules** (shared with cryptenroll proposal):
- `tpm`, `tpm_crb`, `tpm_tis`, `tpm_tis_core`

**Dockerfile change** (add to the tpm2-tools install from cryptenroll proposal):

```dockerfile
COPY --from=tools /usr/bin/tpm2_quote bin/tpm2_quote
COPY --from=tools /usr/bin/tpm2_eventlog bin/tpm2_eventlog
```

## Files Changed

| File | Change |
|------|--------|
| `pkg/tpm/measure.go` | Self-measurement and image measurement |
| `pkg/tpm/eventlog.go` | TCG2 event log types and capture |
| `pkg/tpm/quote.go` | Attestation quote generation |
| `pkg/tpm/tpm.go` | Extend existing TPM operations |
| `pkg/caprf/client.go` | `ReportAttestation()` method |
| `pkg/provision/orchestrator.go` | Measurement steps |
| `initrd.Dockerfile` | Add `tpm2_quote`, `tpm2_eventlog` |

## Testing

### Unit Tests

- `tpm/measure_test.go` — Self-measurement with `go-tpm-tools/simulator`.
  Verify PCR[23] extended correctly after hashing test binary.
- `tpm/eventlog_test.go` — Event log serialization/deserialization.
- `tpm/quote_test.go` — Quote generation with simulator. Verify quote
  covers expected PCRs and is verifiable with EK certificate.

### E2E / KVM Tests

- **KVM matrix** (`kvm-matrix.yml`, tag `e2e_kvm`):
  - QEMU + swtpm (shared with cryptenroll proposal)
  - Scenario 1: Full measurement flow → quote → verify against golden values
  - Scenario 2: Tampered binary → measurement differs → CAPRF rejects
  - Scenario 3: Event log replay → reconstructed PCR values match quote

## Risks

| Risk | Mitigation |
|------|------------|
| TPM2 not present | Skip measurement, report "unmeasured" to CAPRF |
| swtpm limitations vs real TPM | Document differences; test on real hardware periodically |
| Event log too large for CAPRF POST | Compress; truncate to BOOTy-relevant events only |
| Keylime integration complexity | Phase 2; keep optional |

## Effort Estimate

10–14 engineering days (measurements + event log + quote + CAPRF
integration + KVM tests).
