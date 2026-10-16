//go:build linux

package espsync

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCopyTreeMirrorsNestedFilesAndDeletesStaleEntries(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "EFI", "Vendor", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "EFI", "Vendor", "nested", "loader.efi"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "stale"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "EFI", "Vendor", "nested", "loader.efi"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(filepath.Join(destination, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale path remains: %v", err)
	}
}

func TestCopyTreeRejectsSameDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := CopyTree(dir, dir); err == nil {
		t.Fatal("expected identity rejection")
	}
}

func TestCopyTreePreservesSymlinks(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "loader"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loader", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "loader" {
		t.Fatalf("link target %q", target)
	}
}

func TestCopyTreeReplacesExistingLoaderWithoutDeletingItFirst(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "EFI", "BOOT", "bootx64.efi"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(destination, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "EFI", "BOOT", "bootx64.efi"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "EFI", "BOOT", "bootx64.efi"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteMarkerUsesAtomicRename(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "run", "booty-esp-sync.ready")
	if err := WriteMarker(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTreeReplacesDestinationDirectoryWithFile(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "EFI", "BOOT", "loader"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(destination, "EFI", "BOOT", "loader"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "EFI", "BOOT", "loader"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "file" {
		t.Fatalf("got %q", got)
	}
}

func TestCopyTreeReplacesDestinationDirectoryWithSymlink(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "EFI"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(source, "EFI", "loader")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(destination, "EFI", "loader"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(destination, "EFI", "loader"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "target" {
		t.Fatalf("got %q", got)
	}
}

func TestCopyTreeStagesInsideDestinationFilesystem(t *testing.T) {
	source := t.TempDir()
	destination, err := os.MkdirTemp("/dev/shm", "booty-esp-destination-")
	if err != nil {
		t.Skipf("tmpfs unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destination) })
	linkParent := t.TempDir()
	destinationPath := filepath.Join(linkParent, "mounted-esp")
	if err := os.Symlink(destination, destinationPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "EFI", "BOOT", "bootx64.efi"), []byte("cross-filesystem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destinationPath); err != nil {
		t.Fatalf("copy across parent and mounted filesystems: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "EFI", "BOOT", "bootx64.efi"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cross-filesystem" {
		t.Fatalf("got %q", got)
	}
}

func TestCopyTreeCleansOwnedStagingAndSkipsSourceLeftovers(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "EFI", "BOOT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "EFI", "BOOT", "bootx64.efi"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, stagingPrefix+"root-file"), []byte("legitimate-root-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("EFI/BOOT/bootx64.efi", filepath.Join(source, stagingPrefix+"root-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, stagingPrefix+"leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, stagingPrefix+"leftover", "ignored"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "EFI", stagingPrefix+"nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "EFI", stagingPrefix+"nested", "loader"), []byte("legitimate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(destination, stagingPrefix+"crashed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, stagingPrefix+"crashed", "ignored"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, stagingPrefix+"crashed")); !os.IsNotExist(err) {
		t.Fatalf("owned stale staging remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, stagingPrefix+"leftover")); !os.IsNotExist(err) {
		t.Fatalf("source staging leaked into destination: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "EFI", stagingPrefix+"nested", "loader"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "legitimate" {
		t.Fatalf("nested reserved-prefix data = %q", got)
	}
	got, err = os.ReadFile(filepath.Join(destination, stagingPrefix+"root-file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "legitimate-root-file" {
		t.Fatalf("root reserved-prefix file = %q", got)
	}
	link, err := os.Readlink(filepath.Join(destination, stagingPrefix+"root-link"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "EFI/BOOT/bootx64.efi" {
		t.Fatalf("root reserved-prefix symlink = %q", link)
	}
}

func TestWithLockSerializesFullCallbackLifetime(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "sync.lock")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := WithLock(lockPath, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		}); err != nil {
			t.Errorf("first lock: %v", err)
		}
	}()
	<-firstEntered
	go func() {
		defer wg.Done()
		if err := WithLock(lockPath, func() error {
			close(secondEntered)
			return nil
		}); err != nil {
			t.Errorf("second lock: %v", err)
		}
	}()
	select {
	case <-secondEntered:
		t.Fatal("second callback entered before first callback released lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	wg.Wait()
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second callback did not enter after first callback completed")
	}
}
