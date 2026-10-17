package tpm

// PCR plan, following the UAPI TPM PCR registry
// (https://uapi-group.org/specifications/specs/linux_tpm_pcr_registry/):
//
//	PCR 0-7:  firmware; read only. PCR 7 is the Secure Boot policy.
//	PCR 8, 9: GRUB commands and files (the BOOTy kernel and initramfs).
//	          BOOTy does not extend them; quotes include them.
//	PCR 10:   IMA. BOOTy does not extend it.
//	PCR 11-13: systemd-stub (UKI sections, command line, sysexts).
//	PCR 14:   shim MokList. BOOTy does not extend it.
//	PCR 15:   systemd machine identity and volume keys.
//	PCR 16:   debug.
//	PCR 23:   application support. BOOTy runtime events.
//
// BOOTy extends its own events (binary, configuration, image) into one
// PCR, PCRBooty, and keeps the event order in a MeasurementLog, which the
// verifier replays. PCR 15 is not used: after a kexec handoff the target OS
// keeps the PCR values of the BOOTy boot, and systemd in the target expects
// PCR 15 to hold only its own events. PCR 23 is resettable by software, so
// a verifier must not trust PCR 23 alone. The BOOTy ISO boots through shim
// and GRUB: the firmware measures shim and GRUB into PCR 4, and GRUB
// measures the kernel and the initramfs (which contains BOOTy) into PCR 9
// and its commands into PCR 8. A quote must therefore cover PCR 4, 8 and 9
// together with PCR 23, and the verifier matches 4, 8 and 9 against the
// expected BOOTy release before it replays the PCR 23 log. PCR 11 applies
// only to a future UKI boot.
// See docs/usage-security-telemetry.md#pcr-plan.
const (
	PCRFirmware    = 0  // Platform firmware measurements
	PCRFirmwareCfg = 1  // Firmware configuration
	PCRBootLoader  = 4  // Bootloader measurements (shim, GRUB)
	PCRGRUBCmds    = 8  // GRUB commands and kernel command line
	PCRGRUBFiles   = 9  // Files that GRUB loads: kernel and initramfs (BOOTy)
	PCRSecureBoot  = 7  // Secure Boot policy
	PCRBooty       = 23 // BOOTy runtime events (application support)
)

// PCRDescription returns a human-readable description for a PCR index.
func PCRDescription(pcr int) string {
	switch pcr {
	case PCRFirmware:
		return "platform firmware"
	case PCRFirmwareCfg:
		return "firmware configuration"
	case PCRBootLoader:
		return "bootloader"
	case PCRSecureBoot:
		return "secure boot policy"
	case PCRGRUBCmds:
		return "GRUB commands"
	case PCRGRUBFiles:
		return "GRUB loaded files (kernel, initramfs)"
	case PCRBooty:
		return "BOOTy runtime events"
	default:
		return "unknown"
	}
}
