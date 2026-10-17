# Proposal: systemd-cryptenroll with TPM2 Setup

## Status: Phase 1 Implemented (PR #56)

## Priority: P2

## Dependencies: [LUKS Encryption](proposal-luks-encryption.md), [SecureBoot Full Chain](proposal-secureboot-full-chain.md)

## Summary

Bind LUKS2 key slots to TPM2 PCR policies during provisioning, enabling
unattended encrypted boot without manual passphrase entry. Uses Go `go-tpm`
library for direct TPM2 operations (preferred), with `systemd-cryptenroll`
binary as fallback. Configurable PCR binding (default: PCR 7 for SecureBoot
state). Never bind to PCR 14: shim measures the MokList into it. BOOTy
extends its own events into PCR 23 only, see [PCR plan](usage-security-telemetry.md#pcr-plan).

## Motivation

LUKS encryption without automated unlock requires manual passphrase entry
at every boot — unacceptable for remote bare-metal servers. TPM2-sealed
keys solve this by binding disk decryption to the machine's boot chain:

| Unlock Method | Unattended? | Security | Recovery |
|---------------|-------------|----------|----------|
| Passphrase | No | Depends on strength | Easy |
| Key file in initramfs | Yes | Low (key exposed) | Easy |
| TPM2 PCR binding | Yes | High (hardware-bound) | Requires recovery key |
| Clevis/Tang | Yes | Medium (network-dependent) | Tang server required |
| TPM2 + PIN | Semi | Highest | Recovery key + PIN |

TPM2 + PCR binding is the industry standard for unattended secure disk
encryption on bare-metal servers.

## Design

### TPM2 Enrollment Flow

```
BOOTy provisioning:
  1. Create LUKS2 volume (from LUKS proposal)
  2. Add passphrase to key slot 0 (recovery key)
  3. Read current PCR values from TPM2
  4. Generate PCR policy hash
  5. Seal new LUKS key to TPM2 with PCR policy
  6. Add TPM2-sealed key to LUKS key slot 1
  7. Configure provisional OS initramfs for TPM2 unlock

Boot flow (post-provisioning):
  1. UEFI firmware measures boot chain → PCR values
  2. Initramfs requests TPM2 unseal
  3. TPM2 verifies PCR policy matches current measurements
  4. If match → unseal key → unlock LUKS → boot
  5. If mismatch → fall back to recovery passphrase
```

### Go TPM2 Implementation

```go
// pkg/tpm/cryptenroll/enroller.go
package cryptenroll

import (
    "context"
    "crypto/rand"
    "fmt"
    "log/slog"

    "github.com/google/go-tpm/tpm2"
    "github.com/google/go-tpm/tpm2/transport/linuxTPM"
)

// Enroller seals LUKS keys to TPM2 PCR policies.
type Enroller struct {
    tpmPath string // default: /dev/tpmrm0
    log     *slog.Logger
}

// Config specifies the TPM2 enrollment parameters.
type Config struct {
    PCRs       []int  `json:"pcrs"`       // default: [7] (SecureBoot state)
    PCRBank    string `json:"pcrBank"`    // default: "sha256"
    LUKSDevice string `json:"luksDevice"` // e.g., "/dev/sda3"
    KeySlot    int    `json:"keySlot"`    // LUKS key slot (default: 1)
}

func New(log *slog.Logger) *Enroller {
    return &Enroller{tpmPath: "/dev/tpmrm0", log: log}
}

// Enroll generates a random key, seals it to TPM2 PCR policy,
// and adds it to the LUKS volume as a new key slot.
func (e *Enroller) Enroll(ctx context.Context, cfg Config, existingPassphrase string) error {
    // 1. Open TPM
    tpm, err := linuxTPM.Open()
    if err != nil {
        return fmt.Errorf("open TPM: %w", err)
    }
    defer tpm.Close()

    // 2. Generate random LUKS key
    luksKey := make([]byte, 64)
    if _, err := rand.Read(luksKey); err != nil {
        return fmt.Errorf("generate LUKS key: %w", err)
    }

    // 3. Build PCR selection for policy
    pcrSelection := tpm2.PCRSelection{
        Hash: tpm2.AlgSHA256,
        PCRs: cfg.PCRs,
    }

    // 4. Create policy session with PCR binding
    // 5. Seal key under SRK with policy
    // 6. Add sealed key to LUKS as new key slot
    // (implementation details follow TPM2 key sealing protocol)

    return nil
}
```

### Fallback: systemd-cryptenroll Binary

```go
// pkg/tpm/cryptenroll/fallback.go
package cryptenroll

import (
    "context"
    "fmt"
    "os/exec"
    "strings"
)

func (e *Enroller) enrollViaCryptenroll(ctx context.Context, cfg Config, existingPassphrase string) error {
    pcrList := make([]string, len(cfg.PCRs))
    for i, p := range cfg.PCRs {
        pcrList[i] = fmt.Sprintf("%d", p)
    }

    // systemd-cryptenroll --tpm2-device=auto --tpm2-pcrs=7 /dev/sda3
    cmd := exec.CommandContext(ctx, "systemd-cryptenroll",
        "--tpm2-device=auto",
        fmt.Sprintf("--tpm2-pcrs=%s", strings.Join(pcrList, "+")),
        cfg.LUKSDevice,
    )
    cmd.Env = append(cmd.Env, fmt.Sprintf("PASSWORD=%s", existingPassphrase))
    if out, err := cmd.CombinedOutput(); err != nil {
        return fmt.Errorf("systemd-cryptenroll: %s: %w", string(out), err)
    }
    return nil
}
```

### Required Binaries in Initramfs

| Binary | Package | Purpose | Initramfs Flavor | Already Present? |
|--------|---------|---------|-----------------|-----------------|
| `systemd-cryptenroll` | `systemd` | TPM2-seal LUKS key (fallback) | full, gobgp | **No — add** |
| `cryptsetup` | `cryptsetup-bin` | LUKS key slot management | full, gobgp | **No — add** (from LUKS proposal) |
| `tpm2_pcrread` | `tpm2-tools` | Read PCR values (debug) | full, gobgp | **No — add** |
| `tpm2_createprimary` | `tpm2-tools` | Create TPM2 primary key (debug) | full only | **No — optional** |

**Kernel modules needed**:

```dockerfile
# TPM2 kernel modules
for m in ... \
    tpm tpm_crb tpm_tis tpm_tis_core; do \
    find "$MDIR" -name "${m}.ko*" -exec cp {} /modules/ \; 2>/dev/null || true; \
done
```

**Dockerfile change** (tools stage):

```dockerfile
RUN apt-get update && apt-get install -y --no-install-recommends \
    ... existing packages ... \
    tpm2-tools \
    && rm -rf /var/lib/apt/lists/*

# TPM2 tools (debug and fallback)
COPY --from=tools /usr/bin/tpm2_pcrread bin/tpm2_pcrread
# systemd-cryptenroll is part of systemd — extract from systemd package
COPY --from=tools /usr/bin/systemd-cryptenroll bin/systemd-cryptenroll
```

## Files Changed

| File | Change |
|------|--------|
| `pkg/tpm/cryptenroll/enroller.go` | TPM2 key sealing and LUKS enrollment |
| `pkg/tpm/cryptenroll/fallback.go` | systemd-cryptenroll fallback |
| `pkg/tpm/cryptenroll/config.go` | Configuration types |
| `pkg/tpm/cryptenroll/enroller_test.go` | Unit tests |
| `initrd.Dockerfile` | Add TPM2 tools, kernel modules |

## Testing

### Unit Tests

- `cryptenroll/enroller_test.go`:
  - TPM2 PCR policy generation with `go-tpm-tools/simulator`
  - Key sealing/unsealing roundtrip in simulator
  - Fallback invocation with mock Commander

### E2E / KVM Tests

- **KVM matrix** (`kvm-matrix.yml`, tag `e2e_kvm`):
  - QEMU with `-tpmdev emulator,id=tpm0,chardev=chrtpm -chardev socket,id=chrtpm,path=/tmp/swtpm.sock` (swtpm)
  - OVMF firmware with TPM2 support
  - Scenario 1: Create LUKS → enroll TPM2 → reboot → verify auto-unlock
  - Scenario 2: Modify PCR values (simulate different boot chain) → verify unlock fails → recovery key works
  - Scenario 3: Verify PCR 7 binding catches SecureBoot state change

## Risks

| Risk | Mitigation |
|------|------------|
| TPM2 not present on all servers | Detect and skip; fall back to other unlock methods |
| swtpm not available in CI | CI installs swtpm; skip TPM tests if unavailable |
| PCR values differ between BIOS updates | Re-enrollment procedure documented |
| go-tpm library API changes | Pin version; track upstream |

## Effort Estimate

8–12 engineering days (Go TPM2 + systemd-cryptenroll fallback + KVM tests
with swtpm).
