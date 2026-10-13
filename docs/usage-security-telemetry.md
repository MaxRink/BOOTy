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
  PCRs, measure a file or a reader into a PCR, create an ECDSA quote over
  PCRs, and seal or unseal data to PCR values.
- `VerifyQuoteSignature` and `VerifyQuoteAgainstPolicy` check a quote.

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

The provisioning steps do not feed the telemetry collector yet. So the
summary has no step, image or disk data. The event emitter gets no events,
so BOOTy sends nothing to `EVENT_URL`.

The `pkg/telemetry` library also has counters, gauges, histograms and a step
tracker. The provisioning flow does not use them.

Planned: step metrics, events and exporters. See
[proposal-telemetry-enhanced.md](proposal-telemetry-enhanced.md) and
[proposal-observability.md](proposal-observability.md).

## Boot loader

The provisioning flow supports GRUB only. There is no boot loader detection
and no systemd-boot support. Planned: see
[proposal-bootloaders.md](proposal-bootloaders.md).
