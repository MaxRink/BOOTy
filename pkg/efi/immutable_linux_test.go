//go:build linux

package efi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriteVarRewritesExistingVariable(t *testing.T) {
	dir := t.TempDir()
	r := NewEFIVarReader(dir)
	if err := r.WriteVar("V-1", 7, []byte("a longer first value")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteVar("V-1", 7, []byte("short")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "V-1"))
	if err != nil {
		t.Fatal(err)
	}
	// A regular file keeps the tail of the longer value (no O_TRUNC);
	// efivarfs replaces the whole variable per write.
	if !bytes.HasPrefix(got, []byte("\x07\x00\x00\x00short")) {
		t.Fatalf("variable = %q", got)
	}
}

// efivarfs marks variables outside the kernel allow list immutable. The
// inode flag ioctls are faked: setting the flag needs CAP_LINUX_IMMUTABLE.
func TestWriteVarClearsAndRestoresImmutableFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "V-1")
	if err := os.WriteFile(path, []byte("\x07\x00\x00\x00old"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldGet, oldSet := getInodeFlags, setInodeFlags
	t.Cleanup(func() { getInodeFlags, setInodeFlags = oldGet, oldSet })
	flags := uint32(fsImmutableFL | 0x80) // immutable and noatime
	var sets []uint32
	getInodeFlags = func(int) (uint32, error) { return flags, nil }
	setInodeFlags = func(_ int, f uint32) error {
		if f&fsImmutableFL == 0 {
			// The variable may be written now: check WriteVar wrote after this.
			if got, _ := os.ReadFile(path); string(got) != "\x07\x00\x00\x00old" {
				t.Errorf("variable written before the flag was cleared: %q", got)
			}
		}
		sets = append(sets, f)
		flags = f
		return nil
	}

	if err := NewEFIVarReader(dir).WriteVar("V-1", 7, []byte("new")); err != nil {
		t.Fatalf("WriteVar(immutable) = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "\x07\x00\x00\x00new" {
		t.Fatalf("variable = %q", got)
	}
	if len(sets) != 2 || sets[0] != 0x80 || sets[1] != fsImmutableFL|0x80 {
		t.Fatalf("flag changes = %#x, want clear (0x80) then restore (0x90)", sets)
	}

	// A plain file without the flag is written without flag changes.
	sets = nil
	flags = 0
	if err := NewEFIVarReader(dir).WriteVar("V-1", 7, []byte("2nd")); err != nil || len(sets) != 0 {
		t.Fatalf("WriteVar() = %v, flag changes %#x, want none", err, sets)
	}
	// A failed clear fails the write instead of a silent EPERM later.
	flags = fsImmutableFL
	setInodeFlags = func(int, uint32) error { return unix.EPERM }
	if err := NewEFIVarReader(dir).WriteVar("V-1", 7, []byte("3rd")); err == nil {
		t.Fatal("WriteVar() = nil when the immutable flag can not be cleared")
	}
}
