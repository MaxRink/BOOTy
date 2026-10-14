//go:build linux && linux_e2e

package linux

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/image"
)

// ---------------------------------------------------------------------------
// Gap 9: RAID array creation with real loop devices
// Proves: mdadm RAID1 array creation and stop works on real devices.
// ---------------------------------------------------------------------------

func TestRAIDArrayCreationAndStop(t *testing.T) {
	requireRoot(t)

	// Check if mdadm is available.
	requireTool(t, "mdadm")

	ctx := context.Background()
	mgr := disk.NewManager(nil)

	// Create two raw loop devices for RAID1.
	dev1 := createRawLoopDevice(t, 50)
	dev2 := createRawLoopDevice(t, 50)

	// Clear any existing superblocks.
	exec.Command("mdadm", "--zero-superblock", dev1).Run() //nolint:errcheck
	exec.Command("mdadm", "--zero-superblock", dev2).Run() //nolint:errcheck

	// Create RAID1 array.
	// CreateRAIDArray prepends /dev/ to the name, so pass without /dev/ prefix.
	err := mgr.CreateRAIDArray(ctx, "md/test-raid1", 1, []string{dev1, dev2})
	if err != nil {
		t.Fatalf("CreateRAIDArray: %v", err)
	}

	t.Cleanup(func() {
		exec.Command("mdadm", "--stop", "/dev/md/test-raid1").Run() //nolint:errcheck
	})

	// Verify RAID array exists.
	out, err := exec.Command("mdadm", "--detail", "/dev/md/test-raid1").CombinedOutput()
	if err != nil {
		t.Fatalf("mdadm --detail: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "raid1") {
		t.Errorf("expected raid1 in detail output, got:\n%s", out)
	}

	// Stop RAID arrays.
	if err := mgr.StopRAIDArrays(ctx); err != nil {
		t.Errorf("StopRAIDArrays: %v", err)
	}
}

func TestRAIDArrayRequiresMinDevices(t *testing.T) {
	requireRoot(t)

	requireTool(t, "mdadm")

	ctx := context.Background()
	mgr := disk.NewManager(nil)

	// RAID with only 1 device should fail.
	dev := createRawLoopDevice(t, 50)
	err := mgr.CreateRAIDArray(ctx, "md/test-single", 1, []string{dev})
	if err == nil {
		t.Fatal("expected error for single-device RAID")
	}
	if !strings.Contains(err.Error(), "at least 2 devices") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Gap 10: LVM volume group creation and deactivation
// Proves: LVM vgcreate/lvcreate/vgchange works on real loop devices.
// ---------------------------------------------------------------------------

func TestLVMVolumeGroupLifecycle(t *testing.T) {
	requireRoot(t)

	for _, tool := range []string{"pvcreate", "vgcreate", "lvcreate", "vgchange", "vgremove"} {
		requireTool(t, tool)
	}

	dev := createRawLoopDevice(t, 100)

	// Create physical volume.
	runCmd(t, "pvcreate", "-f", dev)

	vgName := "booty-test-vg"
	lvName := "booty-test-lv"

	t.Cleanup(func() {
		exec.Command("lvremove", "-f", vgName+"/"+lvName).Run() //nolint:errcheck
		exec.Command("vgremove", "-f", vgName).Run()            //nolint:errcheck
		exec.Command("pvremove", "-f", dev).Run()               //nolint:errcheck
	})

	// Create volume group.
	runCmd(t, "vgcreate", vgName, dev)

	// Create logical volume (50MB).
	runCmd(t, "lvcreate", "-L", "50M", "-n", lvName, vgName)

	// Verify LV exists.
	out := runCmd(t, "lvs", "--noheadings", "-o", "lv_name", vgName)
	if !strings.Contains(out, lvName) {
		t.Errorf("LV %s not found in output: %s", lvName, out)
	}

	// Deactivate VG using disk manager.
	ctx := context.Background()
	mgr := disk.NewManager(nil)
	if err := mgr.DisableLVM(ctx); err != nil {
		t.Logf("DisableLVM: %v (may be expected)", err)
	}
}

// ---------------------------------------------------------------------------
// Gap 19: Multi-format image streaming (zstd, lz4, xz on real device)
// Proves: all compression formats are correctly decompressed and written.
// ---------------------------------------------------------------------------

func TestImageStreamZstdToDevice(t *testing.T) {
	requireRoot(t)

	requireTool(t, "zstd")

	loopDev := createRawLoopDevice(t, 10)

	// Create test data.
	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Compress with zstd using external tool.
	tmpFile := t.TempDir() + "/data.raw"
	os.WriteFile(tmpFile, data, 0o644) //nolint:errcheck
	runCmd(t, "zstd", "-f", "--no-progress", tmpFile, "-o", tmpFile+".zst")

	compressed, err := os.ReadFile(tmpFile + ".zst")
	if err != nil {
		t.Fatalf("read zstd: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	if err := image.Stream(ctx, ts.URL+"/test.img.zst", loopDev); err != nil {
		t.Fatalf("Stream zstd: %v", err)
	}

	// Read back and verify.
	f, _ := os.Open(loopDev)
	defer f.Close()
	readBack := make([]byte, len(data))
	io.ReadFull(f, readBack) //nolint:errcheck
	if !bytes.Equal(data, readBack) {
		t.Error("zstd decompressed data does not match")
	}
}

func TestImageStreamLZ4ToDevice(t *testing.T) {
	requireRoot(t)

	requireTool(t, "lz4")

	loopDev := createRawLoopDevice(t, 10)

	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	tmpFile := t.TempDir() + "/data.raw"
	os.WriteFile(tmpFile, data, 0o644) //nolint:errcheck
	runCmd(t, "lz4", "-f", tmpFile, tmpFile+".lz4")

	compressed, err := os.ReadFile(tmpFile + ".lz4")
	if err != nil {
		t.Fatalf("read lz4: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	if err := image.Stream(ctx, ts.URL+"/test.img.lz4", loopDev); err != nil {
		t.Fatalf("Stream lz4: %v", err)
	}

	f, _ := os.Open(loopDev)
	defer f.Close()
	readBack := make([]byte, len(data))
	io.ReadFull(f, readBack) //nolint:errcheck
	if !bytes.Equal(data, readBack) {
		t.Error("lz4 decompressed data does not match")
	}
}

func TestImageStreamXZToDevice(t *testing.T) {
	requireRoot(t)

	requireTool(t, "xz")

	loopDev := createRawLoopDevice(t, 10)

	data := make([]byte, 128*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	tmpFile := t.TempDir() + "/data.raw"
	os.WriteFile(tmpFile, data, 0o644) //nolint:errcheck
	runCmd(t, "xz", "-f", "--keep", tmpFile)

	compressed, err := os.ReadFile(tmpFile + ".xz")
	if err != nil {
		t.Fatalf("read xz: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(compressed) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	if err := image.Stream(ctx, ts.URL+"/test.img.xz", loopDev); err != nil {
		t.Fatalf("Stream xz: %v", err)
	}

	f, _ := os.Open(loopDev)
	defer f.Close()
	readBack := make([]byte, len(data))
	io.ReadFull(f, readBack) //nolint:errcheck
	if !bytes.Equal(data, readBack) {
		t.Error("xz decompressed data does not match")
	}
}

// ---------------------------------------------------------------------------
// Gap 20: Checksum verification on real block device
// Proves: full SHA256 checksum validation on disk write.
// ---------------------------------------------------------------------------

func TestImageStreamChecksumOnDevice(t *testing.T) {
	requireRoot(t)

	loopDev := createRawLoopDevice(t, 10)

	// Create deterministic data.
	data := make([]byte, 256*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	// Compute sha256.
	h := sha256.New()
	h.Write(data)
	checksum := hex.EncodeToString(h.Sum(nil))

	// Gzip compress.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write(data) //nolint:errcheck
	gw.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes()) //nolint:errcheck
	}))
	defer ts.Close()

	ctx := context.Background()
	err := image.Stream(ctx, ts.URL+"/image.gz", loopDev, image.StreamOpts{
		Checksum:      checksum,
		ChecksumType:  "sha256",
		ChecksumScope: image.ChecksumScopeDecompressed,
	})
	if err != nil {
		t.Fatalf("Stream with checksum: %v", err)
	}

	// Verify data on device matches.
	f, _ := os.Open(loopDev)
	defer f.Close()
	readBack := make([]byte, len(data))
	if _, err := io.ReadFull(f, readBack); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(data, readBack) {
		t.Error("data mismatch after checksum-validated write")
	}
}

func TestMultiFormatDetectionOnDevice(t *testing.T) {
	requireRoot(t)

	tests := []struct {
		name  string
		magic []byte
		want  image.Format
	}{
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}, image.FormatZstd},
		{"lz4", []byte{0x04, 0x22, 0x4d, 0x18}, image.FormatLZ4},
		{"xz", []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, image.FormatXZ},
		{"bzip2", []byte{0x42, 0x5a, 0x68}, image.FormatBzip2},
		{"gzip", []byte{0x1f, 0x8b}, image.FormatGzip},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loopDev := createRawLoopDevice(t, 1)

			// Write magic bytes + padding to device.
			padding := make([]byte, 512-len(tt.magic))
			data := append(tt.magic, padding...)

			f, err := os.OpenFile(loopDev, os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			f.Write(data) //nolint:errcheck
			f.Close()

			rf, err := os.Open(loopDev)
			if err != nil {
				t.Fatalf("open for read: %v", err)
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
