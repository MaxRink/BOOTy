package secureboot

import (
	"debug/pe"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/telekom/BOOTy/pkg/efi"
)

// maxEFIBinarySize is the maximum accepted size for an EFI binary before
// attempting to parse it with debug/pe. Real shim/grub EFI binaries are
// typically a few MiB; rejecting oversized files guards against memory/CPU
// exhaustion from crafted headers.
const maxEFIBinarySize = 64 * 1024 * 1024 // 64 MiB

// ChainVerifier validates the Secure Boot chain using EFI variables.
type ChainVerifier struct {
	vars *efi.EFIVarReader
}

// NewChainVerifier creates a chain verifier with the given EFI variable reader.
func NewChainVerifier(vars *efi.EFIVarReader) *ChainVerifier {
	return &ChainVerifier{vars: vars}
}

// Verify checks the Secure Boot chain and returns a result.
func (cv *ChainVerifier) Verify() (*ChainResult, error) {
	result := &ChainResult{}

	enabled, err := cv.vars.IsSecureBootEnabled()
	if err != nil {
		slog.Warn("cannot determine secure boot status", "error", err)
	} else {
		result.SecureBootEnabled = enabled
	}

	setupMode, err := cv.vars.IsSetupMode()
	if err != nil {
		slog.Warn("cannot determine setup mode", "error", err)
	} else {
		result.SetupMode = setupMode
	}

	result.Components = cv.checkComponentPresence()
	// PreconditionsMet requires SecureBoot enabled, not in setup mode, and all required
	// components present on disk. NOTE: this does NOT verify cryptographic
	// signatures — it only confirms expected files exist. Full PE/COFF
	// signature verification is planned but not yet implemented.
	result.PreconditionsMet = result.SecureBootEnabled && !result.SetupMode && cv.allComponentsPresent(result.Components)
	return result, nil
}

// checkComponentPresence checks whether boot chain binaries exist on disk
// and validates PE/COFF headers for EFI binaries.
func (cv *ChainVerifier) checkComponentPresence() []ComponentStatus {
	specs := []struct {
		name  string
		paths []string
	}{
		{"shim", []string{
			"/boot/efi/EFI/BOOT/BOOTX64.EFI",
			"/boot/efi/EFI/BOOT/BOOTAA64.EFI",
		}},
		{"grub", []string{
			"/boot/efi/EFI/ubuntu/grubx64.efi",
			"/boot/efi/EFI/centos/grubx64.efi",
			"/boot/efi/EFI/redhat/grubx64.efi",
			"/boot/efi/EFI/fedora/grubx64.efi",
			"/boot/efi/EFI/sles/grubx64.efi",
			"/boot/efi/EFI/debian/grubx64.efi",
		}},
		{"kernel", []string{
			"/boot/vmlinuz",
			"/boot/vmlinuz-linux",
		}},
	}
	components := make([]ComponentStatus, 0, len(specs))
	for _, s := range specs {
		components = append(components, findValidCandidate(s.name, s.paths))
	}
	return components
}

// findValidCandidate scans candidates in order, returning the first that exists
// and passes PE/COFF validation (for .efi paths). If no candidate passes,
// the returned ComponentStatus carries an error string that distinguishes
// "file not found" from "pe/coff validation failed" for all existing candidates.
func findValidCandidate(name string, candidates []string) ComponentStatus {
	status := ComponentStatus{Name: name}
	type candidateErr struct {
		path string
		err  error
	}
	var validationErrs []candidateErr
	anyFound := false
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				// Unexpected stat error (permission denied, I/O error, etc.).
				// Return immediately so the caller sees the real failure.
				status.Error = fmt.Sprintf("stat %s: %v", path, err)
				return status
			}
			continue
		}
		anyFound = true
		if isEFIPath(path) {
			if err := validatePEHeader(path); err != nil {
				slog.Warn("pe/coff validation failed, trying next candidate",
					"path", path, "error", err)
				validationErrs = append(validationErrs, candidateErr{path: path, err: err})
				continue
			}
		}
		return status
	}
	if anyFound && len(validationErrs) > 0 {
		// Build a per-candidate summary so operators can identify the corrupt file.
		var sb strings.Builder
		sb.WriteString("pe/coff validation failed for all candidates")
		for _, ce := range validationErrs {
			fmt.Fprintf(&sb, "; %s: %v", ce.path, ce.err)
		}
		status.Error = sb.String()
	} else {
		status.Error = fmt.Sprintf("not found: tried %v", candidates)
	}
	return status
}

// isEFIPath reports whether path points to a PE/COFF EFI binary.
// Kernel vmlinuz paths are excluded — they are not PE binaries.
func isEFIPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".efi")
}

// hostPEMachineType returns the PE machine type that matches the running host
// architecture. It is used to validate that EFI binaries target the correct arch.
func hostPEMachineType() uint16 {
	switch runtime.GOARCH {
	case "amd64":
		return pe.IMAGE_FILE_MACHINE_AMD64
	case "arm64":
		return pe.IMAGE_FILE_MACHINE_ARM64
	default:
		return pe.IMAGE_FILE_MACHINE_UNKNOWN
	}
}

// validatePEHeader opens path as a PE/COFF binary, checks the file size,
// validates the PE machine type against the host architecture, and returns
// an error if the file is missing, too large, truncated, has an invalid
// header, or targets a mismatched architecture.
func validatePEHeader(path string) (retErr error) {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("pe/coff stat failed: %w", err)
	}
	if info.Size() > maxEFIBinarySize {
		return fmt.Errorf("pe/coff file too large: %d bytes (max %d)", info.Size(), maxEFIBinarySize)
	}

	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("pe/coff parse failed: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("pe/coff close failed: %w", cerr)
		}
	}()

	return validatePEMachineType(f)
}

// validatePEMachineType checks that the PE file's machine type matches the
// host architecture. An unknown host arch (GOARCH not in the switch) skips
// the check to avoid false negatives in cross-compilation/CI environments.
func validatePEMachineType(f *pe.File) error {
	want := hostPEMachineType()
	if want == pe.IMAGE_FILE_MACHINE_UNKNOWN {
		// Unknown host arch — skip arch validation to avoid false negatives.
		return nil
	}
	if f.FileHeader.Machine != want {
		return fmt.Errorf("pe/coff machine type mismatch: got %#x, want %#x", f.FileHeader.Machine, want)
	}
	return nil
}

func (cv *ChainVerifier) allComponentsPresent(components []ComponentStatus) bool {
	for _, c := range components {
		if c.Error != "" {
			return false
		}
	}
	return len(components) > 0
}
