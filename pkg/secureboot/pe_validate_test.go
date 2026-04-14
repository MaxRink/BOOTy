package secureboot

import (
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// peOptionalHeader32PlusMinSize is the minimum size of a PE32+ optional header
// (IMAGE_OPTIONAL_HEADER64), as required by the PE/COFF specification.
const peOptionalHeader32PlusMinSize = 112

// minimalValidPE returns a minimal PE32+ binary whose machine type matches
// the host architecture and has IMAGE_SUBSYSTEM_EFI_APPLICATION set,
// so it passes validatePEHeader on any supported arch.
func minimalValidPE() []byte {
	hostMachine := hostPEMachineType()
	if hostMachine == pe.IMAGE_FILE_MACHINE_UNKNOWN {
		hostMachine = pe.IMAGE_FILE_MACHINE_AMD64
	}
	return minimalPEWithMachineAndSubsystem(hostMachine, pe.IMAGE_SUBSYSTEM_EFI_APPLICATION)
}

func writeTempFile(t *testing.T, data []byte, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	return path
}

func TestValidatePEHeader_ValidPE(t *testing.T) {
	path := writeTempFile(t, minimalValidPE(), "boot.efi")
	if err := validatePEHeader(path); err != nil {
		t.Errorf("expected valid PE to pass, got: %v", err)
	}
}

func TestValidatePEHeader_TruncatedMZ(t *testing.T) {
	// MZ signature only — no PE signature
	data := []byte{'M', 'Z', 0x00, 0x00}
	path := writeTempFile(t, data, "truncated.efi")
	if err := validatePEHeader(path); err == nil {
		t.Error("expected error for truncated MZ-only file, got nil")
	}
}

func TestValidatePEHeader_NonPE(t *testing.T) {
	data := []byte("this is plain text, not a PE binary at all")
	path := writeTempFile(t, data, "notpe.efi")
	if err := validatePEHeader(path); err == nil {
		t.Error("expected error for non-PE file, got nil")
	}
}

func TestValidatePEHeader_EmptyFile(t *testing.T) {
	path := writeTempFile(t, []byte{}, "empty.efi")
	if err := validatePEHeader(path); err == nil {
		t.Error("expected error for empty file, got nil")
	}
}

func TestFindValidCandidate_SkipsInvalidPEThenFindsValid(t *testing.T) {
	dir := t.TempDir()

	invalid := filepath.Join(dir, "bad.efi")
	if err := os.WriteFile(invalid, []byte("not a PE binary"), 0o600); err != nil {
		t.Fatalf("write invalid: %v", err)
	}

	valid := filepath.Join(dir, "good.efi")
	if err := os.WriteFile(valid, minimalValidPE(), 0o600); err != nil {
		t.Fatalf("write valid: %v", err)
	}

	status := findValidCandidate("shim", []string{invalid, valid})
	if status.Error != "" {
		t.Errorf("expected valid candidate to be found, got error: %s", status.Error)
	}
}

func TestFindValidCandidate_AllInvalidPEReturnsError(t *testing.T) {
	dir := t.TempDir()

	bad1 := filepath.Join(dir, "bad1.efi")
	bad2 := filepath.Join(dir, "bad2.efi")
	for _, p := range []string{bad1, bad2} {
		if err := os.WriteFile(p, []byte("garbage"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	status := findValidCandidate("shim", []string{bad1, bad2})
	if status.Error == "" {
		t.Error("expected error when all candidates have invalid PE headers")
	}
	if !strings.HasPrefix(status.Error, "pe/coff validation failed for all candidates") {
		t.Errorf("expected pe/coff validation failed for all candidates error prefix, got misleading message: %q", status.Error)
	}
}

// minimalPEWithMachine returns a minimal PE binary with the given machine type
// and IMAGE_SUBSYSTEM_EFI_APPLICATION as the subsystem.
// The optional header magic is PE32+ (0x020b) regardless of machine type.
func minimalPEWithMachine(machine uint16) []byte {
	return minimalPEWithMachineAndSubsystem(machine, pe.IMAGE_SUBSYSTEM_EFI_APPLICATION)
}

// minimalPEWithMachineAndSubsystem returns a minimal PE32+ binary with the
// given machine type and subsystem values. It is the canonical builder for
// all PE test fixtures in this package.
func minimalPEWithMachineAndSubsystem(machine uint16, subsystem uint16) []byte {
	const (
		dosStubSize   = 64
		peSignature   = 4
		coffHdrSize   = 20
		optHdrOffset  = dosStubSize + peSignature + coffHdrSize
		magicPE32Plus = uint16(0x020b)
		subsystemOff  = 68 // byte offset of Subsystem within OptionalHeader64
	)
	buf := make([]byte, optHdrOffset+peOptionalHeader32PlusMinSize)
	buf[0] = 'M'
	buf[1] = 'Z'
	binary.LittleEndian.PutUint32(buf[0x3c:], dosStubSize)
	copy(buf[dosStubSize:], []byte("PE\x00\x00"))
	coffBase := dosStubSize + peSignature
	binary.LittleEndian.PutUint16(buf[coffBase:], machine)
	binary.LittleEndian.PutUint16(buf[coffBase+16:], peOptionalHeader32PlusMinSize)
	binary.LittleEndian.PutUint16(buf[coffBase+18:], 0x0002)
	binary.LittleEndian.PutUint16(buf[optHdrOffset:], magicPE32Plus)
	binary.LittleEndian.PutUint16(buf[optHdrOffset+subsystemOff:], subsystem)
	return buf
}

// wrongArchMachine returns the PE machine type for an architecture that is
// guaranteed to differ from the host, so we can test arch-mismatch rejection.
func wrongArchMachine() uint16 {
	switch runtime.GOARCH {
	case "amd64":
		return pe.IMAGE_FILE_MACHINE_ARM64
	default:
		// For arm64 (and any other arch), use AMD64 as the wrong type.
		return pe.IMAGE_FILE_MACHINE_AMD64
	}
}

// TestValidatePEHeader_MachineTypeMismatch verifies that validatePEHeader
// rejects EFI binaries that target a different architecture than the host.
func TestValidatePEHeader_MachineTypeMismatch(t *testing.T) {
	if hostPEMachineType() == pe.IMAGE_FILE_MACHINE_UNKNOWN {
		t.Skip("host arch not mapped to a PE machine type; skipping arch-mismatch test")
	}
	data := minimalPEWithMachine(wrongArchMachine())
	path := writeTempFile(t, data, "wrongarch.efi")
	err := validatePEHeader(path)
	if err == nil {
		t.Error("expected error for EFI binary with wrong machine type, got nil")
	}
	if err != nil && !strings.Contains(err.Error(), "machine type mismatch") {
		t.Errorf("expected 'machine type mismatch' in error, got: %v", err)
	}
}

// TestValidatePEHeader_CorrectMachineType verifies that validatePEHeader
// accepts an EFI binary that targets the host architecture.
func TestValidatePEHeader_CorrectMachineType(t *testing.T) {
	want := hostPEMachineType()
	if want == pe.IMAGE_FILE_MACHINE_UNKNOWN {
		t.Skip("host arch not mapped to a PE machine type; skipping machine type test")
	}
	data := minimalPEWithMachine(want)
	path := writeTempFile(t, data, "correctarch.efi")
	if err := validatePEHeader(path); err != nil {
		t.Errorf("expected valid PE for host arch to pass, got: %v", err)
	}
}

func TestIsEFIPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/boot/efi/EFI/BOOT/BOOTX64.EFI", true},
		{"/boot/efi/EFI/ubuntu/grubx64.efi", true},
		{"/boot/vmlinuz", false},
		{"/boot/vmlinuz-linux", false},
		{"/boot/efi/EFI/BOOT/BOOTAA64.EFI", true},
	}
	for _, tc := range cases {
		got := isEFIPath(tc.path)
		if got != tc.want {
			t.Errorf("isEFIPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestValidatePEHeader_SizeCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.efi")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxEFIBinarySize + 1); err != nil {
		_ = f.Close()
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()

	if err := validatePEHeader(path); err == nil {
		t.Error("expected error for oversized file, got nil")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' in error, got: %v", err)
	}
}

func TestValidatePEHeader_NonEFISubsystem(t *testing.T) {
	machine := hostPEMachineType()
	if machine == pe.IMAGE_FILE_MACHINE_UNKNOWN {
		machine = pe.IMAGE_FILE_MACHINE_AMD64
	}
	data := minimalPEWithMachineAndSubsystem(machine, pe.IMAGE_SUBSYSTEM_WINDOWS_GUI)
	path := writeTempFile(t, data, "windows.efi")
	err := validatePEHeader(path)
	if err == nil {
		t.Error("expected error for non-EFI subsystem, got nil")
	}
	if err != nil && !strings.Contains(err.Error(), "not an EFI subsystem") {
		t.Errorf("expected 'not an EFI subsystem' in error, got: %v", err)
	}
}
