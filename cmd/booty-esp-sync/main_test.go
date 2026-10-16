//go:build linux

package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairSyncDegradedPeerKeepsWatching(t *testing.T) {
	dir := t.TempDir()
	missingPeer := filepath.Join(dir, "by-partuuid", "peer")
	orig := peerDevice
	peerDevice = func(string, string, string) (string, error) { return missingPeer, nil }
	t.Cleanup(func() { peerDevice = orig })

	var logs bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	marker := filepath.Join(dir, "esp-sync.ready")
	pair := &pairSync{source: t.TempDir(), firstPartUUID: "a", secondPartUUID: "b",
		mountpoint: filepath.Join(dir, "mnt"), lockPath: filepath.Join(dir, "lock"), marker: marker}
	for range 3 {
		if err := pair.sync(); err != nil {
			t.Fatalf("sync() with a missing peer = %v, want nil", err)
		}
	}
	if n := strings.Count(logs.String(), "mirror is degraded"); n != 1 {
		t.Fatalf("degraded state logged %d times, want once:\n%s", n, logs.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker written without a synchronization: %v", err)
	}
}

func TestPairSyncCopiesToPeerDirectory(t *testing.T) {
	dir := t.TempDir()
	source, peer := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "grubx64.efi"), []byte("loader"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := peerDevice
	peerDevice = func(string, string, string) (string, error) { return peer, nil }
	t.Cleanup(func() { peerDevice = orig })

	marker := filepath.Join(dir, "esp-sync.ready")
	pair := &pairSync{source: source, firstPartUUID: "a", secondPartUUID: "b",
		mountpoint: filepath.Join(dir, "mnt"), lockPath: filepath.Join(dir, "lock"), marker: marker, degraded: true}
	if err := pair.sync(); err != nil {
		t.Fatalf("sync() = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(peer, "grubx64.efi")); err != nil || string(got) != "loader" {
		t.Fatalf("peer copy = %q, %v", got, err)
	}
	if pair.degraded {
		t.Fatal("degraded state not cleared after the peer returned")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker after synchronization: %v", err)
	}
}
