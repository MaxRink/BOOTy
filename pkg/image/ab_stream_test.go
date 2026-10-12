//go:build linux

package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestStreamABRawCopiesGPTBootAndRootRanges(t *testing.T) {
	raw := testGPTImage(t)
	sum := sha256.Sum256(raw)

	bootTarget := filepath.Join(t.TempDir(), "boot.img")
	rootTarget := filepath.Join(t.TempDir(), "root.img")
	createTargetFile(t, bootTarget)
	createTargetFile(t, rootTarget)

	err := streamABRaw(context.Background(), bytes.NewReader(raw), ABTargets{
		BootPartition: bootTarget,
		RootPartition: rootTarget,
	}, StreamOpts{Checksum: hex.EncodeToString(sum[:]), ChecksumType: "sha256"})
	if err != nil {
		t.Fatalf("streamABRaw: %v", err)
	}

	boot := readFile(t, bootTarget)
	if !bytes.HasPrefix(boot, bytes.Repeat([]byte("B"), 32)) {
		t.Fatalf("boot target was not copied from EFI partition: %q", string(boot[:min(32, len(boot))]))
	}
	root := readFile(t, rootTarget)
	if !bytes.HasPrefix(root, bytes.Repeat([]byte("R"), 32)) {
		t.Fatalf("root target was not copied from root partition: %q", string(root[:min(32, len(root))]))
	}
}

func TestStreamABRawCopiesMBRBootAndRootRanges(t *testing.T) {
	raw := testMBRImage(t)
	sum := sha256.Sum256(raw)

	bootTarget := filepath.Join(t.TempDir(), "boot.img")
	rootTarget := filepath.Join(t.TempDir(), "root.img")
	createTargetFile(t, bootTarget)
	createTargetFile(t, rootTarget)

	err := streamABRaw(context.Background(), bytes.NewReader(raw), ABTargets{
		BootPartition:       bootTarget,
		RootPartition:       rootTarget,
		SourceRootPartition: 2,
	}, StreamOpts{Checksum: hex.EncodeToString(sum[:]), ChecksumType: "sha256"})
	if err != nil {
		t.Fatalf("streamABRaw: %v", err)
	}

	boot := readFile(t, bootTarget)
	if !bytes.HasPrefix(boot, bytes.Repeat([]byte("B"), 32)) {
		t.Fatalf("boot target was not copied from MBR EFI partition: %q", string(boot[:min(32, len(boot))]))
	}
	root := readFile(t, rootTarget)
	if !bytes.HasPrefix(root, bytes.Repeat([]byte("R"), 32)) {
		t.Fatalf("root target was not copied from MBR root partition: %q", string(root[:min(32, len(root))]))
	}
}

func TestStreamABRawChecksumErrorMarksBootAndRootDirty(t *testing.T) {
	raw := testGPTImage(t)
	bootTarget := filepath.Join(t.TempDir(), "boot.img")
	rootTarget := filepath.Join(t.TempDir(), "root.img")
	createTargetFile(t, bootTarget)
	createTargetFile(t, rootTarget)

	err := streamABRaw(context.Background(), bytes.NewReader(raw), ABTargets{
		BootPartition: bootTarget,
		RootPartition: rootTarget,
	}, StreamOpts{Checksum: strings.Repeat("0", sha256.Size*2), ChecksumType: "sha256"})
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}

	var dirty *abDirtyTargetsError
	if !errors.As(err, &dirty) {
		t.Fatalf("error = %v, want dirty targets metadata", err)
	}
	want := []string{bootTarget, rootTarget}
	if !sameStrings(dirty.targets, want) {
		t.Fatalf("dirty targets = %#v, want %#v", dirty.targets, want)
	}
}

func TestStreamABChecksumMismatchWipesWrittenTargets(t *testing.T) {
	raw := testGPTImage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	bootTarget := filepath.Join(t.TempDir(), "boot.img")
	rootTarget := filepath.Join(t.TempDir(), "root.img")
	createTargetFile(t, bootTarget)
	createTargetFile(t, rootTarget)

	err := StreamAB(context.Background(), srv.URL+"/image.raw", ABTargets{
		BootPartition: bootTarget,
		RootPartition: rootTarget,
	}, StreamOpts{Checksum: strings.Repeat("0", sha256.Size*2), ChecksumType: "sha256"})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("StreamAB() error = %v, want checksum mismatch", err)
	}

	// The copy writes 8 boot and 16 root sectors of 'B' and 'R' before the
	// checksum is known. A failed checksum must leave no payload bytes behind.
	for _, tc := range []struct {
		path    string
		written int
	}{
		{bootTarget, 8 * gptSectorSize},
		{rootTarget, 16 * gptSectorSize},
	} {
		got := readFile(t, tc.path)
		if len(got) < tc.written {
			t.Fatalf("%s has %d bytes, want at least %d", tc.path, len(got), tc.written)
		}
		if i := bytes.IndexFunc(got[:tc.written], func(r rune) bool { return r != 0 }); i >= 0 {
			t.Fatalf("%s byte %d = %q after checksum mismatch, want zeroed target", tc.path, i, got[i])
		}
	}
}

func TestDirtyABTargetsErrorDeduplicatesTargets(t *testing.T) {
	base := errors.New("copy failed")
	err := dirtyABTargetsError(base, "/dev/root", "", "/dev/root", " /dev/boot ")
	var dirty *abDirtyTargetsError
	if !errors.As(err, &dirty) {
		t.Fatalf("error = %v, want dirty target wrapper", err)
	}
	want := []string{"/dev/root", "/dev/boot"}
	if !sameStrings(dirty.targets, want) {
		t.Fatalf("dirty targets = %#v, want %#v", dirty.targets, want)
	}
	if !errors.Is(err, base) {
		t.Fatalf("wrapped error does not preserve base error: %v", err)
	}
}

func TestStreamRootRequiresRootPartition(t *testing.T) {
	err := StreamRoot(context.Background(), "http://images.local/root.raw", RootTarget{})
	if err == nil {
		t.Fatal("expected missing root partition error")
	}
	if !strings.Contains(err.Error(), "root partition is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStreamABRawFallsBackToRootFilesystemWhenNoGPT(t *testing.T) {
	raw := []byte("rootfs without partition table")
	rootTarget := filepath.Join(t.TempDir(), "root.img")
	createTargetFile(t, rootTarget)

	err := streamABRaw(context.Background(), bytes.NewReader(raw), ABTargets{RootPartition: rootTarget}, StreamOpts{})
	if err != nil {
		t.Fatalf("streamABRaw fallback: %v", err)
	}

	got := strings.TrimRight(string(readFile(t, rootTarget)), "\x00")
	if got != string(raw) {
		t.Fatalf("root fallback = %q, want %q", got, string(raw))
	}
}

func TestStreamReaderToDeviceSyncErrorMarksTargetDirty(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Fatalf("/dev/null is unavailable: %v", err)
	}

	err := streamReaderToDevice(context.Background(), strings.NewReader("payload"), "/dev/null")
	if err == nil {
		t.Fatal("expected /dev/null sync failure")
	}
	var dirty *abDirtyTargetsError
	if !errors.As(err, &dirty) {
		t.Fatalf("error = %v, want dirty targets metadata", err)
	}
	if !sameStrings(dirty.targets, []string{"/dev/null"}) {
		t.Fatalf("dirty targets = %#v, want /dev/null", dirty.targets)
	}
	if !strings.Contains(err.Error(), "syncing root target /dev/null") {
		t.Fatalf("error = %q, want sync context", err.Error())
	}
}

func TestCopyReaderRangeToDeviceSyncErrorMarksTargetDirty(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Fatalf("/dev/null is unavailable: %v", err)
	}

	err := copyReaderRangeToDevice(context.Background(), strings.NewReader("payload"), "/dev/null", int64(len("payload")))
	if err == nil {
		t.Fatal("expected /dev/null sync failure")
	}
	var dirty *abDirtyTargetsError
	if !errors.As(err, &dirty) {
		t.Fatalf("error = %v, want dirty targets metadata", err)
	}
	if !sameStrings(dirty.targets, []string{"/dev/null"}) {
		t.Fatalf("dirty targets = %#v, want /dev/null", dirty.targets)
	}
	if !strings.Contains(err.Error(), "syncing target /dev/null") {
		t.Fatalf("error = %q, want sync context", err.Error())
	}
}

func TestParseGPTPartitionsSelectsExpectedTypes(t *testing.T) {
	parts, err := parseGPTPartitions(testGPTImage(t)[:streamABPrefixBytes])
	if err != nil {
		t.Fatalf("parseGPTPartitions: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("partitions = %d, want 2", len(parts))
	}

	boot, ok := selectSourceBootPartition(parts)
	if !ok || boot.Type != efiSystemPartitionGUID || boot.Start != 2048 {
		t.Fatalf("boot partition = %#v, ok=%v", boot, ok)
	}
	root, err := selectSourceRootPartition(parts, "", 0)
	if err != nil || root.Type != linuxFilesystemGUID || root.Start != 4096 || root.Name != "rootfs" || root.Number != 2 {
		t.Fatalf("root partition = %#v, err=%v", root, err)
	}
}

func TestParseGPTPartitionsRejectsOversizedEntryWithoutPanic(t *testing.T) {
	prefix := append([]byte(nil), testGPTImage(t)[:streamABPrefixBytes]...)
	header := prefix[gptSectorSize : 2*gptSectorSize]
	binary.LittleEndian.PutUint32(header[84:88], gptPartitionEntryMaxSize+1)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseGPTPartitions panicked: %v", r)
		}
	}()
	_, err := parseGPTPartitions(prefix)
	if err == nil {
		t.Fatal("expected oversized GPT entry size to fail")
	}
	if !strings.Contains(err.Error(), "invalid GPT partition entry size") {
		t.Fatalf("error = %q, want invalid entry size detail", err)
	}
}

func TestParseGPTPartitionsRejectsEntriesOutsidePrefixWithoutPanic(t *testing.T) {
	prefix := append([]byte(nil), testGPTImage(t)[:streamABPrefixBytes]...)
	header := prefix[gptSectorSize : 2*gptSectorSize]
	binary.LittleEndian.PutUint64(header[72:80], streamABPrefixBytes/gptSectorSize-1)
	binary.LittleEndian.PutUint32(header[80:84], 2)
	binary.LittleEndian.PutUint32(header[84:88], 512)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseGPTPartitions panicked: %v", r)
		}
	}()
	_, err := parseGPTPartitions(prefix)
	if err == nil {
		t.Fatal("expected GPT entries outside prefix to fail")
	}
	if !strings.Contains(err.Error(), "GPT partition entries exceed streaming prefix") {
		t.Fatalf("error = %q, want prefix bounds detail", err)
	}
}

func TestParseMBRPartitionsSelectsExpectedTypes(t *testing.T) {
	parts, err := parseMBRPartitions(testMBRImage(t)[:streamABPrefixBytes])
	if err != nil {
		t.Fatalf("parseMBRPartitions: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("partitions = %d, want 2", len(parts))
	}

	boot, ok := selectSourceBootPartition(parts)
	if !ok || boot.Type != efiSystemMBRType || boot.Start != 2048 {
		t.Fatalf("boot partition = %#v, ok=%v", boot, ok)
	}
	root, err := selectSourceRootPartition(parts, "", 0)
	if err != nil || root.Type != linuxFilesystemMBRType || root.Start != 4096 || root.Number != 2 {
		t.Fatalf("root partition = %#v, err=%v", root, err)
	}
}

func TestCopyReaderFailsOnShortWrite(t *testing.T) {
	written, err := copyReader(context.Background(), shortWriter{}, strings.NewReader("payload"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("copyReader() error = %v, want io.ErrShortWrite", err)
	}
	if err != nil && !strings.Contains(err.Error(), "writing stream chunk") {
		t.Fatalf("copyReader() error = %q, want stream write context", err.Error())
	}
	if written != int64(len("payload")-1) {
		t.Fatalf("copyReader() written = %d, want %d", written, len("payload")-1)
	}
}

func TestCopyReaderFailsOnNoProgress(t *testing.T) {
	written, err := copyReader(context.Background(), io.Discard, stuckReader{})
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("copyReader() error = %v, want io.ErrNoProgress", err)
	}
	if err != nil && !strings.Contains(err.Error(), "reading stream chunk") {
		t.Fatalf("copyReader() error = %q, want stream read context", err.Error())
	}
	if written != 0 {
		t.Fatalf("copyReader() written = %d, want 0", written)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

type stuckReader struct{}

func (stuckReader) Read(_ []byte) (int, error) {
	return 0, nil
}

func testGPTImage(t *testing.T) []byte {
	t.Helper()

	const sectors = 8192
	raw := make([]byte, sectors*gptSectorSize)
	header := raw[gptSectorSize : 2*gptSectorSize]
	copy(header[:8], "EFI PART")
	binary.LittleEndian.PutUint32(header[8:12], 0x00010000)
	binary.LittleEndian.PutUint32(header[12:16], 92)
	binary.LittleEndian.PutUint64(header[24:32], 1)
	binary.LittleEndian.PutUint64(header[32:40], sectors-1)
	binary.LittleEndian.PutUint64(header[40:48], 2048)
	binary.LittleEndian.PutUint64(header[48:56], sectors-34)
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], 128)
	binary.LittleEndian.PutUint32(header[84:88], 128)

	writeGPTEntry(raw, 0, []byte{
		0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11,
		0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b,
	}, 2048, 2055, "ESP")
	writeGPTEntry(raw, 1, []byte{
		0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47,
		0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4,
	}, 4096, 4111, "rootfs")

	fillPartition(raw, 2048, 2055, 'B')
	fillPartition(raw, 4096, 4111, 'R')
	return raw
}

func testMBRImage(t *testing.T) []byte {
	t.Helper()

	const sectors = 8192
	raw := make([]byte, sectors*gptSectorSize)
	raw[510] = 0x55
	raw[511] = 0xaa

	writeMBREntry(raw, 0, 0xef, 2048, 8)
	writeMBREntry(raw, 1, 0x83, 4096, 16)

	fillPartition(raw, 2048, 2055, 'B')
	fillPartition(raw, 4096, 4111, 'R')
	return raw
}

func writeGPTEntry(raw []byte, index int, typeGUID []byte, firstLBA, lastLBA uint64, name string) {
	entry := raw[2*gptSectorSize+index*128 : 2*gptSectorSize+(index+1)*128]
	copy(entry[:16], typeGUID)
	for i := 16; i < 32; i++ {
		entry[i] = byte(index + 1)
	}
	binary.LittleEndian.PutUint64(entry[32:40], firstLBA)
	binary.LittleEndian.PutUint64(entry[40:48], lastLBA)
	encodedName := utf16.Encode([]rune(name))
	for i, r := range encodedName {
		if 56+i*2+1 >= len(entry) {
			break
		}
		binary.LittleEndian.PutUint16(entry[56+i*2:58+i*2], r)
	}
}

func writeMBREntry(raw []byte, index int, partType byte, firstLBA, sectors uint32) {
	entry := raw[mbrPartitionTableOffset+index*mbrPartitionEntrySize : mbrPartitionTableOffset+(index+1)*mbrPartitionEntrySize]
	entry[4] = partType
	binary.LittleEndian.PutUint32(entry[8:12], firstLBA)
	binary.LittleEndian.PutUint32(entry[12:16], sectors)
}

func fillPartition(raw []byte, firstLBA, lastLBA uint64, value byte) {
	start := int(firstLBA) * gptSectorSize
	end := int(lastLBA+1) * gptSectorSize
	for i := start; i < end; i++ {
		raw[i] = value
	}
}

func createTargetFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, 128*1024), 0o600); err != nil {
		t.Fatalf("create target file: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
