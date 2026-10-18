# Full Measured Boot and Remote Attestation for A/B Schemes

## Status

Proposal. Design only. Nothing in this document is implemented. This proposal
extends [proposal-tpm-attestation.md](proposal-tpm-attestation.md),
[proposal-attestation-enhanced.md](proposal-attestation-enhanced.md) and
[proposal-cryptenroll-tpm.md](proposal-cryptenroll-tpm.md) with the A/B slot
dimension and corrects parts of their PCR plan.

## Priority

P1 for the trust chain and the PCR plan. P2 for remote attestation and
sealed secrets.

## Summary

Build one trust chain from the firmware to the provisioned OS, for both A/B
slots, and prove it to a remote verifier. The chain is: firmware, shim,
BOOTy as a signed UKI, BOOTy measurements of what it installs, the handoff,
and the target OS UKI. Use the UAPI PCR registry so BOOTy does not collide
with shim, GRUB, IMA and systemd. Seal LUKS keys to signed PCR policies or
systemd-pcrlock policies, so that a slot switch does not lock the disk. Attest
with a restricted AK, EK credential activation and event log replay. Keylime
is the reference continuous verifier on the node.

## Problem and motivation

- An A/B upgrade changes the kernel, the initrd and the command line. A LUKS
  key sealed to raw PCR 4, 8, 9 or 11 values does not unseal after the slot
  switch. The node then needs a manual recovery key.
- The verifier must know which slot booted and whether it is the expected
  version. Today only `/etc/booty/ab-slot.env` and the kernel `root=` argument
  carry this. Neither is measured.
- CAPRF disables Secure Boot before it boots BOOTy (`DisableSecureBoot` in
  CAPRF `internal/redfish/system.go`). PCR 7 then shows Secure Boot off for
  the provisioning boot. The chain has a gap at its start.
- kexec does not reset PCRs. After a kexec handoff, the target OS sees the
  PCR values of the BOOTy boot. Policies that expect a fresh boot (for example
  systemd's PCR 15 equals zero rule, verify) fail or become meaningless.
- Nothing proves to CAPRF that BOOTy itself is the expected build.

## Current state

| Area | Today | Reference |
|------|-------|-----------|
| TPM library | `Open`, `ExtendPCR`, `ReadPCR`, `MeasureReader`, `Quote`, `SealSecret`, `UnsealSecret`, a soft measurement log and golden PCR comparison. | `pkg/tpm/device.go`, `pkg/tpm/attest.go`, `pkg/tpm/seal.go`, `pkg/tpm/measure.go`, `pkg/tpm/verify.go` |
| Runtime wiring | No non-test code imports `pkg/tpm` or `pkg/tpm/cryptenroll`. The provisioning flow does not measure, quote or seal. | `grep` of `pkg/provision`, `main.go`, `cmd/` |
| PCR plan | BOOTy assigns PCR 8, 9, 10, 14 and 15 to its own events. | `pkg/tpm/pcr.go` |
| LUKS | Library only. The vars parser rejects `LUKS_*` keys. | `pkg/disk/luks/`, `pkg/caprf/client.go`, `docs/configuration.md` (Planned) |
| Secure Boot | Digest pins for shim, GRUB and kernel. `SECUREBOOT_REENABLE` skips kexec so CAPRF can turn Secure Boot on. | `pkg/secureboot/verify.go`, `docs/configuration.md` |
| kexec | `kexec_file_load` with flags 0. | `pkg/kexec/kexec_linux.go` |
| A/B | Slot state file, `root=PARTLABEL=BOOTY-ROOT-A|B`, shared ESP, removable fallback loader, no NVRAM entry. | `docs/ab-partitioning.md`, `pkg/image/ab.go` |
| KVM tests | `tpm` scenario boots with swtpm. It does not check PCRs or quotes. `secureboot-smoke` boots OVMF with Secure Boot firmware. | `test/e2e/kvm/tpm_test.go`, `test/e2e/kvm/secureboot_test.go` |

### Findings in the existing library

> **Update:** #707 fixed findings 1 to 5: a restricted AK, credential
> activation, `VerifyQuote` with a caller-supplied AK and the quoted digest,
> and one BOOTy PCR (`PCRBooty = 23`; PCR 15 is not used, because kexec keeps
> the BOOTy values). The design below follows that plan. Open question 1 is
> answered by #707.

These are not runtime bugs today, because nothing calls the code. They must
be fixed before any wiring:

1. **PCR collisions.** `pkg/tpm/pcr.go` uses PCR 8 and 9 (GRUB commands and
   files), PCR 10 (IMA), PCR 14 (shim MokList) and PCR 15 (systemd machine
   identity) for BOOTy events. The UAPI TPM PCR registry assigns those PCRs
   to other components (verify the current registry). The cryptenroll
   proposal binds LUKS to PCR 14 as "provisioner identity". On a shim boot,
   PCR 14 holds the MOK list.
2. **The AK is not restricted.** `createAK` creates a signing key under the
   endorsement hierarchy without the `Restricted` attribute. An unrestricted
   key can sign any digest, including a forged `TPMS_ATTEST`. A quote from it
   proves nothing.
3. **No AK to EK binding.** There is no `MakeCredential` and
   `ActivateCredential` exchange and no EK certificate check. A verifier cannot
   tell a real TPM from software.
4. **Self-referential verification.** `VerifyQuoteSignature` checks the
   signature with the public key carried in the same quote.
5. **PCR values are read after the quote.** `Quote` reads the PCRs in a second
   call. The verifier must compare the quoted PCR digest in `TPMS_ATTEST`, not
   the separately read values.

## Goals

- A documented PCR plan that follows the UAPI registry.
- BOOTy shipped as a signed UKI, booted with Secure Boot on.
- BOOTy measures what it installs: image digest, slot, config digest,
  sysexts, and the target boot artifacts.
- A handoff mode that gives the target OS a clean, attestable boot.
- LUKS unlock that survives A/B slot switches and rejects unapproved
  artifacts.
- Provision-time attestation to CAPRF, and node-time attestation with Keylime
  or an equivalent.
- Event log capture and replay for the verifier.

## Non-goals

- A new verifier service. Use Keylime or a small CAPRF endpoint built on
  existing libraries.
- DRTM (Intel TXT, AMD SKINIT). It is a later option.
- IMA runtime policy content. Keylime owns it.

## Trust chain

```text
Firmware (CRTM)        PCR 0-7   measured by firmware, PCR 7 = Secure Boot policy
  |
  v
shim (MS-signed)       PCR 4 (Authenticode), PCR 7 (cert used), PCR 14 (MokList)
  |
  v
BOOTy UKI              PCR 4 (UKI image), PCR 11 (UKI sections via systemd-stub),
  (systemd-stub)       PCR 12 (cmdline, credentials), PCR 13 (sysexts)
  |
  v
BOOTy runtime          BOOTy event log (CEL JSON) + one BOOTy PCR (see PCR plan)
  |   measures: config digest, image digest, slot, sysext digests,
  |             target UKI or kernel+initrd+cmdline digests
  |   quote to CAPRF before handoff
  v
Handoff: reboot (recommended for attested targets) or kexec
  |
  v
Target firmware boot   PCR 0-7 again, then shim, then target UKI of slot A or B
  |
  v
Target OS              PCR 11 phases (systemd-pcrphase), PCR 15 identity,
                       Keylime agent quotes PCRs + event log + IMA
```

## PCR plan

| PCR | Owner (UAPI registry, verify) | BOOTy use |
|-----|-------------------------------|-----------|
| 0 to 7 | Firmware | Read only. PCR 7 is the main policy PCR for Secure Boot state. |
| 8, 9 | GRUB | Do not extend. The target GRUB boot uses them. |
| 10 | IMA | Do not extend. |
| 11 | systemd-stub UKI sections, systemd-pcrphase | Measured by the stub for the BOOTy UKI. Do not extend from BOOTy code. |
| 12 | Kernel command line, credentials | Measured by the stub. |
| 13 | sysext images from the stub | Measured by the stub. |
| 14 | shim MokList | Do not extend. Remove it from the cryptenroll proposal. |
| 15 | systemd machine identity, volume keys | Do not extend in the target boot. |
| 16 | Debug | Never in policies. |
| 23 | Application support, resettable | Candidate for BOOTy runtime events within the BOOTy boot only. |

Recommendation: BOOTy extends its runtime events into **one** PCR within the
BOOTy boot. PCR 23 is resettable by software (verify the locality rules), so a
quote over it is weaker. A second candidate is PCR 15, which is free during the
BOOTy boot because BOOTy is not systemd. With a reboot handoff it resets, so
the target OS keeps the systemd semantics. With a kexec handoff it does not
reset, so kexec and a PCR 15 based policy in the target do not mix. Decide
this in phase 1 (open question 1). Rewrite `pkg/tpm/pcr.go` to one constant.

Every BOOTy event goes to a Canonical Event Log (TCG CEL, JSON encoding,
verify) at `/run/booty/event-log.json`. BOOTy uploads it with the quote and
copies it to `/var/lib/booty/attestation/` in the target.

## A/B and sealed secrets

Options to unseal a LUKS key across slot switches:

### Option A: Raw PCR values

Seal to PCR 7 plus PCR 4 or 11 values of the current slot. Reseal after each
upgrade.

- Pro: simple. `pkg/tpm/seal.go` does this.
- Con: the reseal must happen in the old slot before the switch, and must
  predict the new values. A failed boot of the new slot leaves no valid seal
  for it. Not viable for unattended A/B.

### Option B: Signed PCR policy (PolicyAuthorize)

Build a UKI per slot version with `ukify`. `systemd-measure` predicts PCR 11
for each boot phase and signs the prediction (`.pcrsig` section, verify). Enrol
LUKS with `systemd-cryptenroll --tpm2-public-key=<pem> --tpm2-public-key-pcrs=11`
plus `--tpm2-pcrs=7`.

- Pro: any UKI signed by the policy key unseals. Slot A and slot B both work.
  No reseal on upgrade. `systemd-pcrphase` limits unsealing to the initrd
  phase.
- Con: needs UKI targets. An old signed UKI also unseals (rollback). Mitigate
  with an NV-index revocation or a policy key per release line.

### Option C: systemd-pcrlock

`systemd-pcrlock` predicts PCR values from component lists and stores a
policy with variants in an NV index (verify the minimum systemd version, 255
or later). Each A/B slot is one variant. The OS updates the policy before the
switch.

- Pro: works for GRUB and UKI targets. Revocation by NV index update.
- Con: the target OS must ship and run pcrlock. Policy updates are another
  step in the upgrade path. Prediction can fail on firmware updates.

### Option D: Network-bound unlock (Tang, Clevis) plus TPM

Combine a TPM PCR 7 policy with a Tang server (`clevis` SSS).

- Pro: no per-slot PCR prediction. Works today on most distros.
- Con: needs Tang servers reachable at boot. Not hardware-only.

### Recommendation

- UKI targets: Option B. It is the cleanest A/B story.
- GRUB targets: Option C where systemd 255 or later is present, otherwise
  Option D.
- Never seal to PCR 14 or to BOOTy PCRs.
- BOOTy writes the enrolment during provisioning in the target slot only for
  Option B, because only the public key and the PCR list are needed. For
  Option C, BOOTy installs the component files and the target OS creates the
  NV policy on first boot.

## Handoff

| Handoff | PCR state in target | Use |
|---------|--------------------|-----|
| Reboot | Fresh. Firmware measures the target chain. | Required for attested and sealed targets. |
| kexec | Carries the BOOTy boot PCRs. The firmware event log may not be passed to the new kernel (verify for the shipped kernel). | Fast path for non-attested nodes. Required today for `AB_PRESERVE_EXISTING`. |

The A/B preserve-existing upgrade needs kexec today, because the firmware
boot path still points at the old slot. For attested A/B, replace that with a
firmware-visible slot switch: a one-time `BootNext` entry for the new slot, or
systemd-boot `LoaderEntryOneShot` with boot counting (see
[proposal-rollback.md](proposal-rollback.md) and
[proposal-uefi-management.md](proposal-uefi-management.md)). Then use a reboot.

kexec under Secure Boot and lockdown needs a kernel with a signature that the
kernel keyring trusts (`KEXEC_SIG`, verify). BOOTy calls `kexec_file_load`,
which supports this.

## Remote attestation

### Provision-time (BOOTy to CAPRF)

1. BOOTy reads the EK certificate from the TPM NV index (RSA and ECC
   templates) and the EK public key.
2. BOOTy creates a **restricted** signing AK under the endorsement or owner
   hierarchy.
3. BOOTy sends EK cert, EK public and AK public to CAPRF.
4. CAPRF validates the EK certificate against the TPM vendor CA bundle. It
   runs `MakeCredential` with a secret and returns the blob.
5. BOOTy runs `ActivateCredential` and returns the secret. CAPRF now trusts the
   AK.
6. CAPRF sends a nonce. BOOTy quotes PCR 0 to 7, 11 to 13 and the BOOTy PCR,
   and sends the quote, the firmware event log
   (`/sys/kernel/security/tpm0/binary_bios_measurements`) and the BOOTy CEL.
7. CAPRF replays the logs against the quoted digest and checks them against a
   policy: Secure Boot on, expected `db` and `dbx`, expected shim and BOOTy
   UKI Authenticode hashes, expected image digest and slot.

Libraries: `github.com/google/go-attestation` or `go-tpm-tools` for AK
activation and event log parsing (verify licence and binary size). BOOTy
already uses `github.com/google/go-tpm`.

### Node-time (Keylime)

- BOOTy writes the Keylime agent config (registrar address, agent UUID equal
  to the machine UID, trusted CA) into the target.
- The Keylime agent in the target registers with the registrar. The verifier
  applies a measured boot policy (firmware log) and an IMA policy (verify the
  current Keylime policy formats).
- CAPRF adds the agent to the verifier with the golden values for the booted
  slot. A failed attestation sets a CAPRF condition and can trigger
  remediation.

## Interfaces

### Native config keys

| Key | Values | Note |
|-----|--------|------|
| `TPM_MODE` | `off` (default), `measure`, `attest`, `require` | `measure` extends and logs. `attest` also quotes to CAPRF. `require` fails the run without a TPM 2.0 or on a failed attestation. |
| `ATTESTATION_URL` | URL | CAPRF attestation endpoint. Fixed like `INVENTORY_URL` for standby payloads. |
| `ATTESTATION_PCRS` | list, default `0-7,11-13` plus the BOOTy PCR | Quote selection. |
| `TPM_EK_CA_BUNDLE` | ramdisk path | Optional local EK CA check. |
| `HANDOFF` | `auto`, `kexec`, `reboot` | `TPM_MODE=require` forces `reboot`. Shared with [proposal-os-targets.md](proposal-os-targets.md). |
| `LUKS_TPM_POLICY` | `pcr7`, `signed`, `pcrlock`, `tang` | Replaces the planned raw PCR binding. |
| `LUKS_TPM_PUBKEY` | ramdisk path | Public key for `signed`. |
| `KEYLIME_AGENT_CONFIG` | ramdisk path | Copied to the target. |

### CAPRF and CAPI API impact

- New CAPRF endpoints: `POST /attestation/ak` (EK and AK registration),
  `POST /attestation/activate`, `POST /attestation/quote`. Same bearer rules
  as other callbacks.
- `RedfishHost.status` gets `tpm` (present, version, EK cert fingerprint) and
  `attestation` (last result, time, slot, BOOTy build).
- `RedfishMachine.status` gets an `Attested` condition. The A/B status fields
  of the `feat/booty-consolidated` branch (`bootedSlot`, `bootedUpgradeID`)
  must come from an attested source when `TPM_MODE` is `attest` or `require`.
- A golden value source: an `AttestationPolicy` object or a ConfigMap per OS
  release with expected UKI hashes and PCR 11 predictions.
- CAPRF must keep Secure Boot on when it boots a signed BOOTy UKI. That
  removes the `DisableSecureBoot` call for those hosts.

## Security considerations

- The restricted AK and EK activation are mandatory. Without them the quote is
  not evidence.
- The nonce must come from CAPRF and must be single-use.
- The EK certificate is a stable hardware identifier. Treat it as personal
  data under the site privacy rules if hosts map to persons (unlikely in a
  data centre, but document it).
- The signed PCR policy key is a root of trust for disk unlock. Keep it in an
  HSM or a CI signing service. Rotate per release line.
- An old signed UKI unseals under Option B. Add NV-based revocation or
  `dbx`-style UKI revocation before production use.
- Event logs can be large. Bound the upload size.
- Do not put secrets in BOOTy CEL events.

## Testing strategy

- Unit: PCR plan constants. CEL encoding. Event log replay with recorded
  fixtures from OVMF and real hardware. Quote verification with a restricted
  AK on the go-tpm simulator (`go-tpm-tools/simulator`, verify). Negative
  tests: unrestricted AK, wrong nonce, replayed quote, log mismatch.
- KVM matrix:
  - `tpm-attest`: swtpm with an EK certificate (`swtpm_setup --create-ek-cert`,
    verify), OVMF with Secure Boot and custom keys, BOOTy as a signed UKI,
    quote to a test CAPRF endpoint.
  - `tpm-ab-unseal`: provision slot A with a LUKS root bound to a signed
    policy, upgrade to slot B, reboot, assert unlock without a recovery key.
  - `tpm-ab-negative`: boot an unsigned UKI in slot B and assert that unlock
    fails.
- ContainerLab: none.
- kindmetal and CAPRF e2e: libvirt with swtpm in kindmetal VMs (verify that
  the kindmetal image has swtpm). Attested condition on the RedfishMachine.
- Real hardware: one node per vendor. Record firmware event logs as fixtures.
  Check EK certificate availability per TPM vendor.

## Phased rollout

1. PR: fix `pkg/tpm` findings (restricted AK, verify against the quoted
   digest, one BOOTy PCR constant). No wiring.
2. PR: CEL event log and `TPM_MODE=measure` in the provision flow.
3. PR: BOOTy UKI build target and signing in the release pipeline.
4. PR: EK and AK activation client and `TPM_MODE=attest`.
5. CAPRF MR: attestation endpoints with a golden value policy.
6. PR: `HANDOFF=reboot` with a `BootNext` slot switch for A/B.
7. PR: LUKS wiring with `signed` and `tang` policies (depends on the LUKS
   proposal).
8. PR: Keylime agent config writer.
9. KVM scenarios for each phase.

## Effort estimate

| Phase | Days |
|-------|------|
| Library fixes | 3 to 4 |
| CEL and measure mode | 4 to 6 |
| BOOTy UKI and signing | 5 to 8 |
| AK activation and attest mode | 6 to 9 |
| CAPRF endpoints and policy | 8 to 12 |
| Reboot handoff with `BootNext` | 4 to 6 |
| LUKS policies | 6 to 9 |
| Keylime writer | 2 to 3 |
| KVM scenarios | 6 to 10 |
| **Total** | **44 to 67** |

## Open questions

1. Which PCR carries BOOTy runtime events: 15 (only with reboot handoff) or
   23?
2. Do all target OSes ship UKIs, or do we need pcrlock for GRUB targets?
3. Do we run Keylime, or a smaller CAPRF-native verifier?
4. Where do golden values come from? The OS image pipeline must publish them.
5. Do all TPMs in the fleet have EK certificates in NV? Some vendors do not
   provision them (verify per vendor).

## systemd-boot and Flatcar

- Option B (signed PCR 11 policy) needs UKIs booted through systemd-boot or
  directly by the firmware. [proposal-systemd-boot.md](proposal-systemd-boot.md) defines how BOOTy installs them, uses boot
  counting for the slot switch, and why kexec skips the stub measurements.
- Flatcar boots with GRUB, so a `/usr` update changes PCR 4, 8 and 9. Bind
  `ROOT` encryption to PCR 7, pcrlock or Tang (see [proposal-flatcar.md](proposal-flatcar.md)).

## kexec limitations

The kexec handoff skips the firmware, so boot counting, `BootNext`, measured
boot, Secure Boot verification by shim and pending firmware changes do not
apply to the kexec'd kernel. The full list, with a kexec or reboot decision
table, is in `docs/kexec.md` (PR #705).

## Related proposals

- [proposal-tpm-attestation.md](proposal-tpm-attestation.md)
- [proposal-attestation-enhanced.md](proposal-attestation-enhanced.md)
- [proposal-cryptenroll-tpm.md](proposal-cryptenroll-tpm.md)
- [proposal-luks-encryption.md](proposal-luks-encryption.md)
- [proposal-secureboot-full-chain.md](proposal-secureboot-full-chain.md)
- [proposal-uefi-management.md](proposal-uefi-management.md)
- [proposal-rollback.md](proposal-rollback.md)
- [proposal-systemd-boot.md](proposal-systemd-boot.md)
- [proposal-flatcar.md](proposal-flatcar.md)
