# Security and Telemetry: Current State

This page describes what the Secure Boot, TPM and telemetry code does on
`main` today. [Configuration](configuration.md) lists the keys with their
defaults. Items marked "Planned" are not implemented. Their proposal is
linked.

## Secure Boot

`SECUREBOOT_REENABLE=true` appends `SECUREBOOT_REENABLE=true` to the success
status message and skips kexec. CAPRF then turns Secure Boot on. The
`SECUREBOOT_*_SHA256` and `SECUREBOOT_PINNED_DIGESTS` keys pin the SHA256
digests of the installed shim, GRUB EFI binary and kernel.
[Secure Boot](configuration.md#secure-boot) lists the keys.

The `verify-secureboot-chain` step runs only when at least one digest is
pinned. It then looks for all three components in the installed root:

- shim: `/boot/efi/EFI/BOOT/BOOTX64.EFI` or `BOOTAA64.EFI`,
- GRUB: `grubx64.efi` under `/boot/efi/EFI/<distro>/` for ubuntu, centos,
  redhat, fedora, sles and debian,
- kernel: `/boot/vmlinuz` or `/boot/vmlinuz-linux`.

An EFI binary must have a valid PE/COFF header. A component with a pin must
match its digest. The step fails provisioning when a component is missing
or no candidate passes. The step does not check signatures, and it does not
need Secure Boot to be on.

Planned: Authenticode signature checks of the boot chain. See
[proposal-secureboot-full-chain.md](proposal-secureboot-full-chain.md).

Planned: MOK enrollment. BOOTy parses `MOK_CERT_PATH` and `MOK_PASSWORD`,
but no code uses them. The `pkg/secureboot` library has a `mokutil` wrapper
(`NewMOKEnroller`), but the provisioning flow does not call it. See
[proposal-secureboot.md](proposal-secureboot.md).

## TPM

The provisioning flow, the inventory and the debug dump do not use the TPM.
No vars key controls it.

The `pkg/tpm` library has these functions for later use:

- `Detect` reads TPM presence, version and the SHA256 PCR values from
  `/sys/class/tpm/tpm0`.
- `Open` opens `/dev/tpmrm0` or `/dev/tpm0`. The device can read and extend
  PCRs, measure a file or a reader into a PCR, and seal or unseal data to PCR
  values. It resends a command that the TPM answers with `TPM_RC_RETRY`,
  `TPM_RC_YIELDED` or `TPM_RC_TESTING`.
- Attestation (device side): `EKPublic` (TCG RSA-2048 EK), `AKPublic`,
  `ActivateCredential` and `Quote` (ECDSA P-256 over the SHA-256 PCR bank,
  with the verifier nonce as qualifying data).
- Attestation (verifier side): `MakeCredential`, `VerifyQuote` and
  `VerifyQuoteAgainstPolicy`.

### Attestation flow

1. The device sends `EKPublic` and `AKPublic`. The verifier checks the EK
   (for example the EK certificate of the TPM vendor; BOOTy does not read
   it yet) and calls `MakeCredential(ek, ak, secret)` with a random secret.
   `MakeCredential` checks that the EK public area has the TCG EK
   attributes (restricted decryption key, `fixedTPM`, `fixedParent`,
   `sensitiveDataOrigin`) and the TCG EK auth policy.
2. The device calls `ActivateCredential` and returns the secret. The TPM
   releases it only when it holds this EK and an object with exactly this AK
   public area. Then the verifier trusts the AK.
3. The verifier sends a fresh nonce. The device calls `Quote`. The verifier
   calls `VerifyQuote(ak, quote, nonce)` with the AK from step 2, never with
   a key from the quote. `VerifyQuote` checks the signature, that the quote
   is TPM-generated, the nonce, and that `quote.PCRValues` hash to the
   signed PCR digest. Then it compares the PCR values with the expected
   values (`VerifyQuoteAgainstPolicy`, or `ReplayMeasurementLog` for PCR 23).

Without the EK certificate check, a successful activation proves only that
the same TPM holds the EK and the AK, not that it is a genuine TPM: a
software TPM passes too. The device code assumes empty authorization values
for the endorsement and owner hierarchies, as on a freshly cleared TPM.

The AK is a restricted ECDSA P-256 signing key (`fixedTPM`, `fixedParent`,
`sensitiveDataOrigin`): it signs only data that the TPM generated, so it
cannot sign a forged quote. `MakeCredential` and `VerifyQuote` reject an AK
public area without these attributes. The EK and the AK are primary keys of
the endorsement hierarchy, so the TPM derives the same keys after each boot.

### PCR plan

BOOTy follows the
[UAPI TPM PCR registry](https://uapi-group.org/specifications/specs/linux_tpm_pcr_registry/).

| PCR | Owner | BOOTy use |
| --- | --- | --- |
| 0 to 7 | Firmware; 7 is the Secure Boot policy | Read and quote only |
| 8, 9 | GRUB: commands (8), loaded files such as the BOOTy kernel and initramfs (9) | Read and quote only |
| 10 | IMA | None |
| 11 to 13 | systemd-stub (UKI sections, command line, sysexts); only for a future UKI boot | None today |
| 14 | shim MokList | None. Do not bind LUKS keys to it |
| 15 | systemd machine identity and volume keys | None |
| 16 | Debug | None |
| 23 | Application support | BOOTy runtime events (`PCRBooty`) |

BOOTy extends every event of its own (binary, configuration, image) into
PCR 23 and keeps the order in a `MeasurementLog`; the verifier replays the
log. PCR 15 is not used, because after a kexec handoff the target OS keeps
the PCR values of the BOOTy boot, and systemd in the target expects only
its own events in PCR 15. PCR 23 can be reset by software, so a verifier
must not trust PCR 23 alone. The BOOTy ISO boots through shim and GRUB, not
a UKI: the firmware measures shim and GRUB into PCR 4, and GRUB measures the
kernel and the initramfs (which contains BOOTy) into PCR 9 and its commands
into PCR 8. A quote must cover PCR 4, 8 and 9 together with PCR 23. The
verifier matches PCR 4, 8 and 9 against the expected values of the BOOTy
release first, and only then replays the PCR 23 log. PCR 11 applies only to
a future UKI boot. systemd NvPCRs (NV index based
PCRs) are an option for later; they need TPM NV space and systemd support
in the target.

`pkg/tpm/cryptenroll` holds only the config and the PCR policy for a
`systemd-cryptenroll` setup. It runs no command.

Planned: measured boot and attestation in the provisioning flow. See
[proposal-tpm-attestation.md](proposal-tpm-attestation.md). Planned: TPM2
LUKS unlock. See [proposal-cryptenroll-tpm.md](proposal-cryptenroll-tpm.md).

## Telemetry

`TELEMETRY_ENABLED=true` turns on the telemetry POST requests. `METRICS_URL`,
or `TELEMETRY_URL` when `METRICS_URL` is not set, receives the metrics
summary as JSON. `EVENT_URL` receives the last provisioning event.
[Health, inventory, firmware and telemetry](configuration.md#health-inventory-firmware-and-telemetry)
lists the keys.

When a provisioning run ends, with success or with an error, BOOTy posts the
metrics summary. The requests use the CAPRF token and up to 3 attempts. A
failed request does not change the result of the run.

The summary lists every step that a provision or deprovision run reached,
with its duration, status, retries and, for a failed step, the error and its
[failure code](failure-codes.md). A step that a resumed run skips has the
status `skipped`. The summary has no image or disk data yet. The event
emitter gets no events, so BOOTy sends nothing to `EVENT_URL`.

The step data comes from the step tracker of `pkg/telemetry`. The library
also has counters, gauges and histograms. The provisioning flow does not use
them.

Planned: step metrics, events and exporters. See
[proposal-telemetry-enhanced.md](proposal-telemetry-enhanced.md) and
[proposal-observability.md](proposal-observability.md).

## Out-of-band status

When the data network fails, no callback reaches CAPRF. BOOTy then still
writes a short status that the BMC can show.

With `OOB_STATUS=serial` (the default), BOOTy writes one fixed line to every
active kernel console (`/sys/class/tty/console/active`, else `/dev/console`)
when the phase changes and when a provision or deprovision run ends:

```text
BOOTY1 run=<id8> phase=<phase> code=<code> step=<step> result=<result>
```

- `run`: 8 random hex characters per BOOTy start.
- `phase`: `network`, then the mode (`provision`, `deprovision`, `standby`,
  `check`, `dry-run`); `-` before the config is loaded.
- `code` and `step`: the [failure code](failure-codes.md) and the failed
  step (`network-setup` for a failed network or token setup, `config` for an
  invalid vars file), else `-`.
- `result`: `running` at a phase change, `success` or `failed` at the end.

Every field has at most 48 characters of `A-Z a-z 0-9 . _ -`; other
characters become `_`. The line has no URL, address or secret. CAPRF or
bm4x read it through the BMC serial-over-LAN or the serial capture of the
BMC.

With `OOB_EFI_VAR=true`, BOOTy also stores the end of the run in the
non-volatile UEFI variable `BootyLastResult-f0821f58-5fcc-46a7-8fbd-47f665a67d7f`
(JSON, at most 512 bytes):

```json
{"v":1,"mode":"provision","phase":"network","step":"network-setup","code":"BOOTY-NET-002","result":"failed","booty":"v0.2.0"}
```

The record has no run ID and no time. efivarfs marks the variable immutable
after it is created; BOOTy clears the flag for the write and sets it again,
like `efibootmgr`. BOOTy writes the variable only when the content changes, so a host that fails the same way in a loop does not write NVRAM
again. The next BOOTy start with `OOB_EFI_VAR=true` logs the stored record
(`previous BOOTy run result`). The installed OS can read it from
`/sys/firmware/efi/efivars`.

## Boot loader

The provisioning flow supports GRUB only. There is no boot loader detection
and no systemd-boot support. Planned: see
[proposal-bootloaders.md](proposal-bootloaders.md).
