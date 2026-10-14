//go:build linux && linux_e2e

package linux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/test/e2e/loopdev"
)

func TestStreamABCopiesBootAndInactiveRootPartitions(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)

	source := createPartitionedRawImage(t, []testPartition{
		{name: "EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-source", 1024*1024)},
		{name: "root", sizeMB: 24, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-source", 2*1024*1024)},
	})
	target := createPartitionedRawImage(t, []testPartition{
		{name: "BOOTY-EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-target-before", 1024*1024)},
		{name: "BOOTY-ROOT-A", sizeMB: 32, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-a-keep", 2*1024*1024)},
		{name: "BOOTY-ROOT-B", sizeMB: 32, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-b-before", 2*1024*1024)},
		{name: "BOOTY-STATE", sizeMB: 8, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("state-keep", 1024*1024)},
	})
	url := serveFile(t, source.path)

	if err := image.StreamAB(context.Background(), url, image.ABTargets{
		Disk:          target.path,
		BootPartition: target.partitions[0],
		RootPartition: target.partitions[2],
	}); err != nil {
		t.Fatalf("StreamAB() error: %v", err)
	}

	assertPartitionPrefix(t, target.partitions[0], repeatedPayload("efi-source", 1024*1024))
	assertPartitionPrefix(t, target.partitions[1], repeatedPayload("root-a-keep", 2*1024*1024))
	assertPartitionPrefix(t, target.partitions[2], repeatedPayload("root-source", 2*1024*1024))
	assertPartitionPrefix(t, target.partitions[3], repeatedPayload("state-keep", 1024*1024))
}

func TestStreamABWithoutESPPreservesBootPartition(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)

	source := createPartitionedRawImage(t, []testPartition{
		{name: "root", sizeMB: 24, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-only-source", 2*1024*1024)},
	})
	target := createPartitionedRawImage(t, []testPartition{
		{name: "BOOTY-EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-must-stay", 1024*1024)},
		{name: "BOOTY-ROOT-A", sizeMB: 32, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-a-before", 2*1024*1024)},
		{name: "BOOTY-ROOT-B", sizeMB: 32, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-b-keep", 2*1024*1024)},
	})

	if err := image.StreamAB(context.Background(), serveFile(t, source.path), image.ABTargets{
		Disk:          target.path,
		BootPartition: target.partitions[0],
		RootPartition: target.partitions[1],
	}); err != nil {
		t.Fatalf("StreamAB() error: %v", err)
	}

	assertPartitionPrefix(t, target.partitions[0], repeatedPayload("efi-must-stay", 1024*1024))
	assertPartitionPrefix(t, target.partitions[1], repeatedPayload("root-only-source", 2*1024*1024))
	assertPartitionPrefix(t, target.partitions[2], repeatedPayload("root-b-keep", 2*1024*1024))
}

func TestStreamABRawRootFallbackAndChecksumMismatch(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)

	sourcePayload := repeatedPayload("raw-root-source", 2*1024*1024)
	sourcePath := filepath.Join(t.TempDir(), "rootfs.raw")
	if err := os.WriteFile(sourcePath, sourcePayload, 0o600); err != nil {
		t.Fatalf("write source rootfs: %v", err)
	}
	sourceSum := sha256.Sum256(sourcePayload)

	target := createPartitionedRawImage(t, []testPartition{
		{name: "BOOTY-EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-keep", 1024*1024)},
		{name: "BOOTY-ROOT-A", sizeMB: 16, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-before", 2*1024*1024)},
	})

	if err := image.StreamAB(context.Background(), serveFile(t, sourcePath), image.ABTargets{
		Disk:          target.path,
		BootPartition: target.partitions[0],
		RootPartition: target.partitions[1],
	}, image.StreamOpts{Checksum: hex.EncodeToString(sourceSum[:]), ChecksumType: "sha256"}); err != nil {
		t.Fatalf("StreamAB() raw fallback error: %v", err)
	}
	assertPartitionPrefix(t, target.partitions[0], repeatedPayload("efi-keep", 1024*1024))
	assertPartitionPrefix(t, target.partitions[1], sourcePayload)

	before := repeatedPayload("root-before-mismatch", 2*1024*1024)
	writePartitionPrefix(t, target.partitions[1], before)
	err := image.StreamAB(context.Background(), serveFile(t, sourcePath), image.ABTargets{
		RootPartition: target.partitions[1],
	}, image.StreamOpts{
		Checksum:     "0000000000000000000000000000000000000000000000000000000000000000",
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("StreamAB() succeeded, want checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("StreamAB() error = %q, want checksum mismatch", err.Error())
	}
	// The streaming path detects checksum mismatches after writing; it then
	// invalidates the target slot so firmware cannot boot stale corrupt data.
	assertPartitionPrefixZeroed(t, target.partitions[1], 1024*1024)
}

func TestStreamABQCOW2RootFallback(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)
	requireTool(t, "qemu-img")

	sourcePayload := repeatedPayload("qcow2-root-source", 2*1024*1024)
	sourceRaw := filepath.Join(t.TempDir(), "rootfs.raw")
	sourceQCOW2 := filepath.Join(t.TempDir(), "rootfs.qcow2")
	if err := os.WriteFile(sourceRaw, sourcePayload, 0o600); err != nil {
		t.Fatalf("write source rootfs: %v", err)
	}
	sourceSum := sha256.Sum256(sourcePayload)
	runCmd(t, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", sourceRaw, sourceQCOW2)

	target := createPartitionedRawImage(t, []testPartition{
		{name: "BOOTY-EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-keep-qcow2", 1024*1024)},
		{name: "BOOTY-ROOT-A", sizeMB: 16, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-before-qcow2", 2*1024*1024)},
	})

	if err := image.StreamAB(context.Background(), serveFile(t, sourceQCOW2), image.ABTargets{
		Disk:          target.path,
		BootPartition: target.partitions[0],
		RootPartition: target.partitions[1],
	}, image.StreamOpts{
		// The digest of the converted raw image, not of the qcow2 download.
		Checksum: hex.EncodeToString(sourceSum[:]), ChecksumType: "sha256", ChecksumScope: image.ChecksumScopeDecompressed,
	}); err != nil {
		t.Fatalf("StreamAB() qcow2 root fallback error: %v", err)
	}
	assertPartitionPrefix(t, target.partitions[0], repeatedPayload("efi-keep-qcow2", 1024*1024))
	assertPartitionPrefix(t, target.partitions[1], sourcePayload)
}

func TestStreamABRejectsQCOW2WithBackingFile(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)
	requireTool(t, "qemu-img")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, bytes.Repeat([]byte("TOKEN=supersecret "), 4096), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	sourceQCOW2 := filepath.Join(t.TempDir(), "backed.qcow2")
	runCmd(t, "qemu-img", "create", "-f", "qcow2", "-b", secret, "-F", "raw", sourceQCOW2)

	target := createPartitionedRawImage(t, []testPartition{
		{name: "BOOTY-EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-keep-backed", 1024*1024)},
		{name: "BOOTY-ROOT-A", sizeMB: 16, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-keep-backed", 2*1024*1024)},
	})

	err := image.StreamAB(context.Background(), serveFile(t, sourceQCOW2), image.ABTargets{
		Disk:          target.path,
		BootPartition: target.partitions[0],
		RootPartition: target.partitions[1],
	}, image.StreamOpts{})
	if err == nil || !strings.Contains(err.Error(), "backing file") {
		t.Fatalf("StreamAB() error = %v, want backing file rejection", err)
	}
	data, readErr := os.ReadFile(target.partitions[1])
	if readErr != nil {
		t.Fatalf("read root partition: %v", readErr)
	}
	if bytes.Contains(data, []byte("supersecret")) {
		t.Fatal("backing file content was written to the root partition")
	}
}

func TestStreamABRejectsQCOW2WithExternalDataFile(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)
	requireTool(t, "qemu-img")

	dataFile := filepath.Join(t.TempDir(), "external.raw")
	sourceQCOW2 := filepath.Join(t.TempDir(), "external-data.qcow2")
	runCmd(t, "qemu-img", "create", "-f", "qcow2", "-o", "data_file="+dataFile+",data_file_raw=on", sourceQCOW2, "64K")
	// qemu-img create truncates the data file, so write the secret after it.
	if err := os.WriteFile(dataFile, bytes.Repeat([]byte("TOKEN=supersecret "), 3640), 0o600); err != nil {
		t.Fatalf("write data file: %v", err)
	}

	target := createPartitionedRawImage(t, []testPartition{
		{name: "BOOTY-EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-keep-data", 1024*1024)},
		{name: "BOOTY-ROOT-A", sizeMB: 16, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("root-keep-data", 2*1024*1024)},
	})

	err := image.StreamAB(context.Background(), serveFile(t, sourceQCOW2), image.ABTargets{
		Disk:          target.path,
		BootPartition: target.partitions[0],
		RootPartition: target.partitions[1],
	}, image.StreamOpts{})
	if err == nil || !strings.Contains(err.Error(), "external data file") {
		t.Fatalf("StreamAB() error = %v, want external data file rejection", err)
	}
	data, readErr := os.ReadFile(target.partitions[1])
	if readErr != nil {
		t.Fatalf("read root partition: %v", readErr)
	}
	if bytes.Contains(data, []byte("supersecret")) {
		t.Fatal("external data file content was written to the root partition")
	}
}

func TestStreamRootCopiesSelectedRootPartition(t *testing.T) {
	requireRoot(t)
	requireABStreamTools(t)

	rootA := repeatedPayload("root-a-source", 2*1024*1024)
	rootB := repeatedPayload("root-b-source", 2*1024*1024)
	source := createPartitionedRawImage(t, []testPartition{
		{name: "EFI", sizeMB: 8, typeGUID: "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", payload: repeatedPayload("efi-source", 1024*1024)},
		{name: "ROOT-A", sizeMB: 24, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: rootA},
		{name: "ROOT-B", sizeMB: 24, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: rootB},
	})
	target := createPartitionedRawImage(t, []testPartition{
		{name: "target-root", sizeMB: 32, typeGUID: "0FC63DAF-8483-4772-8E79-3D69D8477DE4", payload: repeatedPayload("target-before", 2*1024*1024)},
	})
	url := serveFile(t, source.path)

	if err := image.StreamRoot(context.Background(), url, image.RootTarget{
		RootPartition:   target.partitions[0],
		SourceRootLabel: "ROOT-B",
	}); err != nil {
		t.Fatalf("StreamRoot() label selector error: %v", err)
	}
	assertPartitionPrefix(t, target.partitions[0], rootB)

	if err := image.StreamRoot(context.Background(), url, image.RootTarget{
		RootPartition:       target.partitions[0],
		SourceRootPartition: 2,
	}); err != nil {
		t.Fatalf("StreamRoot() partition selector error: %v", err)
	}
	assertPartitionPrefix(t, target.partitions[0], rootA)
}

type testPartition struct {
	name     string
	sizeMB   int
	typeGUID string
	payload  []byte
}

type rawImage struct {
	path       string
	loop       string
	partitions []string
}

func requireABStreamTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"truncate", "sfdisk", "losetup", "partprobe", "dd"} {
		requireTool(t, tool)
	}
}

func createPartitionedRawImage(t *testing.T, partitions []testPartition) rawImage {
	t.Helper()
	totalMB := 4
	for _, part := range partitions {
		totalMB += part.sizeMB
	}
	path := filepath.Join(t.TempDir(), "disk.raw")
	runCmd(t, "truncate", "-s", fmt.Sprintf("%dM", totalMB), path)

	var spec strings.Builder
	spec.WriteString("label: gpt\n")
	for _, part := range partitions {
		fmt.Fprintf(&spec, "size=%dM, type=%s, name=%s\n", part.sizeMB, part.typeGUID, part.name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sfdisk", path)
	cmd.Stdin = strings.NewReader(spec.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("sfdisk %s timed out after 30s:\n%s", path, out)
		}
		t.Fatalf("sfdisk %s:\n%s\n%v", path, out, err)
	}

	loop, err := loopdev.Attach(path, len(partitions))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("losetup", "-d", loop).Run()
	})

	partDevs := make([]string, len(partitions))
	for i, part := range partitions {
		dev := fmt.Sprintf("%sp%d", loop, i+1)
		partDevs[i] = dev
		if len(part.payload) > 0 {
			writePartitionPrefix(t, dev, part.payload)
		}
	}
	return rawImage{path: path, loop: loop, partitions: partDevs}
}

func serveFile(t *testing.T, path string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/image.raw"
}

func repeatedPayload(seed string, size int) []byte {
	out := make([]byte, 0, size)
	pattern := []byte(seed + "\n")
	for len(out) < size {
		out = append(out, pattern...)
	}
	return out[:size]
}

func writePartitionPrefix(t *testing.T, dev string, data []byte) {
	t.Helper()
	f := openPartitionForWrite(t, dev, 5*time.Second)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write %s: %v", dev, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", dev, err)
	}
}

func openPartitionForWrite(t *testing.T, dev string, timeout time.Duration) *os.File {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(dev, os.O_WRONLY, 0)
		if err == nil {
			return f
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("open %s for write within %s: %v", dev, timeout, lastErr)
	return nil
}

func assertPartitionPrefix(t *testing.T, dev string, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	f, err := os.Open(dev)
	if err != nil {
		t.Fatalf("open %s for read: %v", dev, err)
	}
	defer f.Close()
	if _, err := io.ReadFull(f, got); err != nil {
		t.Fatalf("read %s: %v", dev, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s prefix mismatch", dev)
	}
}

func assertPartitionPrefixZeroed(t *testing.T, dev string, size int) {
	t.Helper()
	assertPartitionPrefix(t, dev, make([]byte, size))
}
