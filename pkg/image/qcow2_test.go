//go:build linux

package image

import (
	"bytes"
	"encoding/binary"
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

func TestCheckQCOW2Header(t *testing.T) {
	header := func(version uint32, backingOffset, incompat uint64) []byte {
		h := make([]byte, qcow2HeaderLen)
		copy(h, qcow2Magic)
		binary.BigEndian.PutUint32(h[4:8], version)
		binary.BigEndian.PutUint64(h[8:16], backingOffset)
		binary.BigEndian.PutUint64(h[72:80], incompat)
		return h
	}
	tests := []struct {
		name    string
		header  []byte
		wantErr string
	}{
		{"self-contained v3", header(3, 0, 0), ""},
		{"self-contained v2", header(2, 0, 0)[:72], ""},
		{"backing file", header(3, 0x1000, 0), "backing file"},
		{"external data file", header(3, 0, qcow2IncompatExternalData), "external data file"},
		{"truncated v3", header(3, 0, 0)[:72], "truncated"},
		{"not qcow2", make([]byte, qcow2HeaderLen), "not a valid qcow2 header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkQCOW2Header(tt.header)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkQCOW2Header() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkQCOW2Header() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
