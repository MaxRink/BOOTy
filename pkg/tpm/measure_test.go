package tpm

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestHashBytes(t *testing.T) {
	data := []byte("hello")
	expected := sha256.Sum256(data)
	got := HashBytes(data)
	if !bytes.Equal(got, expected[:]) {
		t.Errorf("HashBytes mismatch")
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	data := []byte("test content")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(data)
	got, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected[:]) {
		t.Errorf("HashFile mismatch")
	}
}

func TestHashFile_NotFound(t *testing.T) {
	_, err := HashFile("/nonexistent/file")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestExtendPCRSoft(t *testing.T) {
	digest := HashBytes([]byte("measurement"))
	result := ExtendPCRSoft(nil, digest)
	if len(result) != sha256.Size {
		t.Errorf("expected %d bytes, got %d", sha256.Size, len(result))
	}
	// Extend again should produce different result
	result2 := ExtendPCRSoft(result, digest)
	if bytes.Equal(result, result2) {
		t.Error("extending twice should produce different values")
	}
}

func TestMeasurementLog(t *testing.T) {
	log := NewMeasurementLog()
	digest1 := HashBytes([]byte("event1"))
	digest2 := HashBytes([]byte("event2"))

	log.Add(PCRBooty, "first event", digest1)
	log.Add(PCRBooty, "second event", digest2)
	log.Add(PCRSecureBoot, "secure boot event", digest1)

	entries := log.Entries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Label != "first event" {
		t.Errorf("first entry label = %q", entries[0].Label)
	}

	digests := log.DigestsForPCR(PCRBooty)
	if len(digests) != 2 {
		t.Errorf("expected 2 digests for PCR %d, got %d", PCRBooty, len(digests))
	}
}

func TestReplayMeasurementLog(t *testing.T) {
	log := NewMeasurementLog()
	d1 := HashBytes([]byte("boot"))
	d2 := HashBytes([]byte("config"))

	log.Add(PCRBooty, "boot measurement", d1)
	log.Add(PCRSecureBoot, "secure boot measurement", d2)

	pcrs := ReplayMeasurementLog(log)
	if _, ok := pcrs[PCRBooty]; !ok {
		t.Error("missing PCR BOOTy in replayed log")
	}
	if _, ok := pcrs[PCRSecureBoot]; !ok {
		t.Error("missing PCR secure boot in replayed log")
	}
}

func TestVerifyPCRs_Match(t *testing.T) {
	expected := map[int][]byte{0: HashBytes([]byte("test"))}
	actual := map[int][]byte{0: HashBytes([]byte("test"))}
	ok, mismatches := VerifyPCRs(expected, actual)
	if !ok {
		t.Errorf("expected match, got mismatches: %v", mismatches)
	}
}

func TestVerifyPCRs_Mismatch(t *testing.T) {
	expected := map[int][]byte{0: HashBytes([]byte("a"))}
	actual := map[int][]byte{0: HashBytes([]byte("b"))}
	ok, mismatches := VerifyPCRs(expected, actual)
	if ok {
		t.Error("expected mismatch")
	}
	if len(mismatches) != 1 {
		t.Errorf("expected 1 mismatch, got %d", len(mismatches))
	}
}
