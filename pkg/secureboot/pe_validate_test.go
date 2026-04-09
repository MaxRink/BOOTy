package secureboot

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

const peOptionalHeader32MinSize = 96

func minimalValidPE() []byte {
	const (
		dosStubSize  = 64
		peSignature  = 4
		coffHdrSize  = 20
		optHdrOffset = dosStubSize + peSignature + coffHdrSize
		machineAMD64 = uint16(0x8664)
		magicPE32    = uint16(0x010b)
	)
	buf := make([]byte, optHdrOffset+peOptionalHeader32MinSize)

	buf[0] = 'M'
	buf[1] = 'Z'
	binary.LittleEndian.PutUint32(buf[0x3c:], dosStubSize)

	copy(buf[dosStubSize:], []byte("PE\x00\x00"))

	coffBase := dosStubSize + peSignature
	binary.LittleEndian.PutUint16(buf[coffBase:], machineAMD64)
	binary.LittleEndian.PutUint16(buf[coffBase+16:], peOptionalHeader32MinSize)
	binary.LittleEndian.PutUint16(buf[coffBase+18:], 0x0002)

	binary.LittleEndian.PutUint16(buf[optHdrOffset:], magicPE32)

	return buf
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
