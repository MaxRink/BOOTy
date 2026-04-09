package secureboot

import (
	"debug/pe"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/telekom/BOOTy/pkg/efi"
)

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
	paths := []struct {
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
	components := make([]ComponentStatus, 0, len(paths))
	for _, p := range paths {
		status := ComponentStatus{Name: p.name}
		found := false
		for _, path := range p.paths {
			if _, err := os.Stat(path); err != nil {
				continue
			}
			found = true
			if isEFIPath(path) {
				if err := validatePEHeader(path); err != nil {
					status.Error = fmt.Sprintf("invalid PE/COFF header in %s: %v", path, err)
					found = false
				}
			}
			break
		}
		if !found && status.Error == "" {
			status.Error = fmt.Sprintf("not found: tried %v", p.paths)
		}
		components = append(components, status)
	}
	return components
}

// isEFIPath reports whether path points to a PE/COFF EFI binary.
// Kernel vmlinuz paths are excluded — they are not PE binaries.
func isEFIPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".efi")
}

// validatePEHeader opens path as a PE/COFF binary using debug/pe and
// returns an error if the file is missing, truncated, or has an invalid header.
func validatePEHeader(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("PE/COFF parse failed: %w", err)
	}
	if err := f.Close(); err != nil {
		slog.Warn("failed to close PE file", "path", path, "error", err)
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
