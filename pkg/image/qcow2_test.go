//go:build linux

package image

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactQCOW2SourceForLog(t *testing.T) {
	got := redactQCOW2SourceForLog("https://user:secret@example.com/images/node.qcow2?token=abc#frag")
	for _, leaked := range []string{"user", "secret", "token=abc", "frag"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted qcow2 source leaked %q: %s", leaked, got)
		}
	}
	if got != "https://example.com/images/node.qcow2" {
		t.Fatalf("redactQCOW2SourceForLog() = %q", got)
	}
}

func TestStreamRawToDeviceSyncsTarget(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "source.raw")
	devicePath := filepath.Join(dir, "target.img")
	payload := []byte("raw image payload")
	if err := os.WriteFile(rawPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicePath, make([]byte, len(payload)), 0o644); err != nil {
		t.Fatal(err)
	}

	origSync := syncImageTarget
	synced := ""
	syncImageTarget = func(_ fileSyncer, device string) error {
		synced = device
		return nil
	}
	t.Cleanup(func() {
		syncImageTarget = origSync
	})

	if err := streamRawToDevice(rawPath, devicePath, StreamOpts{}); err != nil {
		t.Fatalf("streamRawToDevice: %v", err)
	}
	if synced != devicePath {
		t.Fatalf("syncImageTarget called with %q, want %q", synced, devicePath)
	}
	got, err := os.ReadFile(devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:len(payload)], payload) {
		t.Fatalf("target prefix = %q, want %q", got[:len(payload)], payload)
	}
}

func TestStreamRawToDeviceChecksumMismatchDoesNotSyncTarget(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "source.raw")
	devicePath := filepath.Join(dir, "target.img")
	payload := []byte("raw image payload with wrong checksum")
	if err := os.WriteFile(rawPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicePath, make([]byte, len(payload)), 0o644); err != nil {
		t.Fatal(err)
	}

	syncCalled := false
	withSyncImageTarget(t, func(_ fileSyncer, _ string) error {
		syncCalled = true
		return nil
	})

	err := streamRawToDevice(rawPath, devicePath, StreamOpts{
		Checksum:     strings.Repeat("0", 64),
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if syncCalled {
		t.Fatal("syncImageTarget called before checksum mismatch was returned")
	}
}
