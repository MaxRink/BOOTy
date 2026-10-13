//go:build linux && linux_e2e

package linux

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/test/e2e/loopdev"
)

func TestImageStreamRawToDevice(t *testing.T) {
	loopDev := createRawLoopDevice(t, 10)

	// Create test data (1MB of random data).
	data := make([]byte, 1024*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	// Serve via HTTP.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data) //nolint:errcheck
	}))
	defer ts.Close()

	// Stream to device.
	ctx := context.Background()
	if err := image.Stream(ctx, ts.URL+"/test.img", loopDev); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Read back from device and verify.
	f, err := os.Open(loopDev)
	if err != nil {
		t.Fatalf("Open device: %v", err)
	}
	defer f.Close()

	readBack := make([]byte, len(data))
	if _, err := io.ReadFull(f, readBack); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	if !bytes.Equal(data, readBack) {
		t.Error("data read from device does not match original")
	}
}

func TestImageStreamGzipToDevice(t *testing.T) {
	loopDev := createRawLoopDevice(t, 10)

	// Create test data.
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Gzip compress.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	gw.Close()

	// Serve gzipped data via HTTP.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buf.Bytes()) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	if err := image.Stream(ctx, ts.URL+"/test.img.gz", loopDev); err != nil {
		t.Fatalf("Stream gzip: %v", err)
	}

	// Read back and verify decompressed data was written.
	f, err := os.Open(loopDev)
	if err != nil {
		t.Fatalf("Open device: %v", err)
	}
	defer f.Close()

	readBack := make([]byte, len(data))
	if _, err := io.ReadFull(f, readBack); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	if !bytes.Equal(data, readBack) {
		t.Error("decompressed data from device does not match original")
	}
}

func TestImageStreamWithChecksumValidation(t *testing.T) {
	loopDev := createRawLoopDevice(t, 10)

	// Create deterministic test data.
	data := make([]byte, 512*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	// Compute sha256 checksum.
	h := sha256.New()
	h.Write(data)
	checksum := hex.EncodeToString(h.Sum(nil))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	err := image.Stream(ctx, ts.URL+"/test.img", loopDev, image.StreamOpts{
		Checksum:     checksum,
		ChecksumType: "sha256",
	})
	if err != nil {
		t.Fatalf("Stream with checksum: %v", err)
	}
}

func TestImageStreamChecksumMismatch(t *testing.T) {
	loopDev := createRawLoopDevice(t, 10)

	data := []byte("test image data")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	err := image.Stream(ctx, ts.URL+"/test.img", loopDev, image.StreamOpts{
		Checksum:     "0000000000000000000000000000000000000000000000000000000000000000",
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !contains(err.Error(), "checksum mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestImageStreamToFormattedPartition(t *testing.T) {
	loopDev := createLoopDevice(t, 1)
	ctx := context.Background()

	// createLoopDevice waited for the partition nodes.
	rootPart := loopDev + "p2"

	runCmd(t, "mkfs.ext4", "-F", rootPart)

	// Create raw image data (write smaller than partition).
	data := make([]byte, 1024*64) // 64KB
	for i := range data {
		data[i] = byte(i % 200)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) //nolint:errcheck
	}))
	defer ts.Close()

	// Stream to the partition (overwrites ext4, that's expected).
	if err := image.Stream(ctx, ts.URL+"/image.raw", rootPart); err != nil {
		t.Fatalf("Stream to partition: %v", err)
	}

	// Verify written data.
	f, err := os.Open(rootPart)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	readBack := make([]byte, len(data))
	if _, err := io.ReadFull(f, readBack); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(data, readBack) {
		t.Error("data written to partition does not match")
	}
}

func TestFormatDetectionOnDevice(t *testing.T) {
	loopDev := createRawLoopDevice(t, 10)

	tests := []struct {
		name   string
		writer func([]byte) []byte
		want   image.Format
	}{
		{
			name:   "raw",
			writer: func(d []byte) []byte { return d },
			want:   image.FormatRaw,
		},
		{
			name: "gzip",
			writer: func(d []byte) []byte {
				var buf bytes.Buffer
				gw := gzip.NewWriter(&buf)
				gw.Write(d) //nolint:errcheck
				gw.Close()
				return buf.Bytes()
			},
			want: image.FormatGzip,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.writer([]byte("test data for format detection"))

			// Write to device.
			f, err := os.OpenFile(loopDev, os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			f.Write(data) //nolint:errcheck
			f.Close()

			// Read back and detect format.
			rf, err := os.Open(loopDev)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer rf.Close()

			format, _, err := image.DetectFormat(rf)
			if err != nil {
				t.Fatalf("DetectFormat: %v", err)
			}
			if format != tt.want {
				t.Errorf("DetectFormat = %s, want %s", format, tt.want)
			}
		})
	}
}

func TestImageStream404Error(t *testing.T) {
	loopDev := createRawLoopDevice(t, 10)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	ctx := context.Background()
	err := image.Stream(ctx, ts.URL+"/missing.img", loopDev)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestEndToEndDiskProvisioningFlow(t *testing.T) {
	requireRoot(t)
	ctx := context.Background()

	loopDev := createLoopDevice(t, 1)

	// Create a small OS image (just random data to simulate disk image).
	imgData := make([]byte, 512*1024)
	for i := range imgData {
		imgData[i] = byte(i % 128)
	}

	// Gzip the image.
	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	gw.Write(imgData) //nolint:errcheck
	gw.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed.Bytes()) //nolint:errcheck
	}))
	defer ts.Close()

	// Step 1: Stream image to the whole disk.
	if err := image.Stream(ctx, ts.URL+"/os.img.gz", loopDev); err != nil {
		t.Fatalf("Stream image: %v", err)
	}

	// Step 2: Re-partition (streaming overwrites partition table).
	sfdisk := exec.CommandContext(ctx, "sfdisk", loopDev)
	sfdisk.Stdin = bytes.NewReader([]byte(
		"label: gpt\n" +
			"1: size=50M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B\n" +
			"2: type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n",
	))
	if out, err := sfdisk.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk re-partition: %s: %v", out, err)
	}

	// Re-read partitions and wait for the new nodes.
	exec.CommandContext(ctx, "partprobe", loopDev).Run() //nolint:errcheck
	if err := loopdev.WaitPartitions(loopDev, 2, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	mgr := disk.NewManager(nil)
	parts, err := mgr.ParsePartitions(ctx, loopDev)
	if err != nil {
		t.Fatalf("ParsePartitions: %v", err)
	}

	root, err := mgr.FindRootPartition(parts)
	if err != nil {
		t.Fatalf("FindRootPartition: %v", err)
	}

	// Step 3: Format root.
	runCmd(t, "mkfs.ext4", "-F", root.Node)

	// Step 4: Mount root.
	mountpoint, err := os.MkdirTemp("", "booty-provision-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(mountpoint)

	if err := mgr.MountPartition(ctx, root.Node, mountpoint); err != nil {
		t.Fatalf("MountPartition: %v", err)
	}
	defer mgr.Unmount(mountpoint) //nolint:errcheck

	// Step 5: Write config files (simulating configurator).
	os.MkdirAll(mountpoint+"/etc", 0o755) //nolint:errcheck
	if err := os.WriteFile(mountpoint+"/etc/hostname", []byte("test-node\n"), 0o644); err != nil {
		t.Fatalf("WriteFile hostname: %v", err)
	}

	os.MkdirAll(mountpoint+"/etc/default", 0o755) //nolint:errcheck
	kubeletConf := "KUBELET_EXTRA_ARGS=\"--provider-id=test://node-1\"\n"
	if err := os.WriteFile(
		mountpoint+"/etc/default/kubelet",
		[]byte(kubeletConf),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile kubelet config: %v", err)
	}

	// Verify files.
	hostname, err := os.ReadFile(mountpoint + "/etc/hostname")
	if err != nil || string(hostname) != "test-node\n" {
		t.Errorf("hostname file: got %q, err %v", hostname, err)
	}

	t.Log("End-to-end provisioning flow completed successfully")
}
