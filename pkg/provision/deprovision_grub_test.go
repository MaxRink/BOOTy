//go:build linux

package provision

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisableGRUBConfig(t *testing.T) {
	write := func(t *testing.T, root, rel string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("menuentry"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("debian path renamed", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "boot/grub/grub.cfg")
		if err := disableGRUBConfig(root); err != nil {
			t.Fatalf("disableGRUBConfig() = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "boot/grub/grub.cfg.bak")); err != nil {
			t.Fatalf("grub.cfg.bak missing: %v", err)
		}
	})
	t.Run("rhel path renamed", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "boot/grub2/grub.cfg")
		if err := disableGRUBConfig(root); err != nil {
			t.Fatalf("disableGRUBConfig() = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "boot/grub2/grub.cfg")); !os.IsNotExist(err) {
			t.Fatal("grub2/grub.cfg still present")
		}
	})
	t.Run("rerun after rename succeeds", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "boot/grub/grub.cfg.bak")
		if err := disableGRUBConfig(root); err != nil {
			t.Fatalf("disableGRUBConfig() = %v", err)
		}
	})
	t.Run("missing config fails", func(t *testing.T) {
		if err := disableGRUBConfig(t.TempDir()); err == nil {
			t.Fatal("disableGRUBConfig() = nil, want error when no grub.cfg exists")
		}
	})
	t.Run("grub dir symlink out of root fails", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		write(t, outside, "grub.cfg")
		if err := os.MkdirAll(filepath.Join(root, "boot"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "boot/grub")); err != nil {
			t.Fatal(err)
		}
		if err := disableGRUBConfig(root); err == nil {
			t.Fatal("disableGRUBConfig() = nil, want error for grub dir outside root")
		}
		if _, err := os.Stat(filepath.Join(outside, "grub.cfg")); err != nil {
			t.Fatalf("grub.cfg outside root was touched: %v", err)
		}
	})
	t.Run("stat error other than ENOENT fails", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "boot/grub")
		write(t, root, "boot/grub2/grub.cfg")
		if err := disableGRUBConfig(root); err == nil {
			t.Fatal("disableGRUBConfig() = nil, want error when boot/grub is not a directory")
		}
	})
}

func TestDisableGRUBAndUnmountReturnsUnmountError(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "boot/grub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "boot/grub/grub.cfg"), []byte("menuentry"), 0o600); err != nil {
		t.Fatal(err)
	}
	busy := errors.New("busy")
	err := disableGRUBAndUnmount(root, func(string) error { return busy })
	if !errors.Is(err, busy) {
		t.Fatalf("disableGRUBAndUnmount() = %v, want the unmount error", err)
	}

	// Both errors are kept when the rename also fails.
	err = disableGRUBAndUnmount(t.TempDir(), func(string) error { return busy })
	if !errors.Is(err, busy) || !strings.Contains(err.Error(), "no grub.cfg") {
		t.Fatalf("disableGRUBAndUnmount() = %v, want both errors", err)
	}
}
