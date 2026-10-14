//go:build linux

package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
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

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func compressForTest(t *testing.T, format string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch format {
	case "raw":
		return data
	case "gz":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "zst":
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "lz4":
		w := lz4.NewWriter(&buf)
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown format %s", format)
	}
	return buf.Bytes()
}

func serveBytes(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestStreamChecksumScope checks that IMAGE_CHECKSUM covers the file as
// downloaded by default, like the .sha256 files published next to .lz4
// images, and the decompressed image with ChecksumScopeDecompressed.
func TestStreamChecksumScope(t *testing.T) {
	data := bytes.Repeat([]byte("BOOTy checksum scope payload "), 4096)
	for _, format := range []string{"raw", "gz", "zst", "lz4"} {
		downloaded := compressForTest(t, format, data)
		url := serveBytes(t, downloaded) + "/image." + format
		type scopeCase struct {
			name     string
			opt      StreamOpts
			wantFail bool
		}
		tests := []scopeCase{
			{name: "default scope, digest of download", opt: StreamOpts{Checksum: sha256Hex(downloaded)}},
			{name: "download scope, digest of download", opt: StreamOpts{Checksum: sha256Hex(downloaded), ChecksumScope: "download"}},
			{name: "decompressed scope, digest of image", opt: StreamOpts{Checksum: sha256Hex(data), ChecksumScope: "Decompressed"}},
			{name: "default scope, wrong digest", opt: StreamOpts{Checksum: strings.Repeat("0", 64)}, wantFail: true},
			{name: "decompressed scope, wrong digest", opt: StreamOpts{Checksum: strings.Repeat("0", 64), ChecksumScope: "decompressed"}, wantFail: true},
		}
		if format != "raw" {
			tests = append(tests,
				scopeCase{name: "default scope, digest of image", opt: StreamOpts{Checksum: sha256Hex(data)}, wantFail: true},
				scopeCase{name: "decompressed scope, digest of download", opt: StreamOpts{Checksum: sha256Hex(downloaded), ChecksumScope: "decompressed"}, wantFail: true},
			)
		}
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				disk := filepath.Join(t.TempDir(), "disk.img")
				if err := os.WriteFile(disk, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				err := Stream(context.Background(), url, disk, tt.opt)
				if tt.wantFail {
					if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), "IMAGE_CHECKSUM_SCOPE=") {
						t.Fatalf("Stream() = %v, want a checksum mismatch that names the scope", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Stream() = %v", err)
				}
				if got := readFile(t, disk); !bytes.Equal(got, data) {
					t.Fatalf("disk has %d bytes, want the %d image bytes", len(got), len(data))
				}
			})
		}
	}
}

func TestStreamChecksumScopeRejectsUnknownScope(t *testing.T) {
	url := serveBytes(t, []byte("x")) + "/image.raw"
	err := Stream(context.Background(), url, filepath.Join(t.TempDir(), "disk"),
		StreamOpts{Checksum: sha256Hex([]byte("x")), ChecksumScope: "payload"})
	if err == nil || !strings.Contains(err.Error(), "unsupported checksum scope") {
		t.Fatalf("Stream() = %v, want unsupported checksum scope", err)
	}
}

// TestDownloadToFileVerifiesDownloadChecksum covers the ramdisk download of
// IMAGE_MODE=partition: a mismatch fails the download before the image is
// decompressed and copied.
func TestDownloadToFileVerifiesDownloadChecksum(t *testing.T) {
	downloaded := compressForTest(t, "lz4", []byte("partition mode payload"))
	url := serveBytes(t, downloaded) + "/image.raw.lz4"
	for _, tt := range []struct {
		checksum string
		wantErr  bool
	}{
		{checksum: sha256Hex(downloaded)},
		{checksum: sha256Hex([]byte("partition mode payload")), wantErr: true},
	} {
		opt, err := withDownloadChecksum(StreamOpts{Checksum: tt.checksum})
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "image.download")
		err = downloadToFile(context.Background(), url, dest, opt.download)
		if (err != nil) != tt.wantErr {
			t.Fatalf("downloadToFile(%s) = %v, want error %v", tt.checksum[:8], err, tt.wantErr)
		}
		if err == nil {
			if verr := verifyRawImageChecksum(dest, opt); verr != nil {
				t.Fatalf("verifyRawImageChecksum() = %v after a verified download", verr)
			}
		}
	}
}

// randomRootGPTImage is a GPT image with a rootSectors-sector root
// partition of random data and a random tail after the partition table end.
// Random data does not compress, so a decoder is still reading the download
// when the root partition is done.
func randomRootGPTImage(t *testing.T, rootSectors uint64) []byte {
	t.Helper()
	const rootStart = 4096
	sectors := rootStart + rootSectors + 34
	const tailSize = 4 << 20
	raw := make([]byte, sectors*gptSectorSize, sectors*gptSectorSize+tailSize)
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
		0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47,
		0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4,
	}, rootStart, rootStart+rootSectors-1, "rootfs")
	if _, err := rand.Read(raw[rootStart*gptSectorSize : (rootStart+rootSectors)*gptSectorSize]); err != nil {
		t.Fatal(err)
	}
	tailData := make([]byte, tailSize)
	if _, err := rand.Read(tailData); err != nil {
		t.Fatal(err)
	}
	return append(raw, tailData...)
}

// TestStreamRootVerifiesDownloadChecksum covers the root and A/B stream
// path, which stops reading the payload after the root partition, with a
// multi-MiB random root and a random tail (run it with -race).
func TestStreamRootVerifiesDownloadChecksum(t *testing.T) {
	raw := randomRootGPTImage(t, 8192) // 4 MiB root
	for _, format := range []string{"gz", "zst"} {
		downloaded := compressForTest(t, format, raw)
		url := serveBytes(t, downloaded) + "/image.raw." + format
		for _, tt := range []struct {
			checksum string
			wantErr  bool
		}{
			{checksum: sha256Hex(downloaded)},
			{checksum: sha256Hex(raw), wantErr: true},
		} {
			for range 3 {
				root := filepath.Join(t.TempDir(), "root.img")
				createTargetFile(t, root)
				err := StreamRoot(context.Background(), url, RootTarget{RootPartition: root}, StreamOpts{Checksum: tt.checksum})
				if (err != nil) != tt.wantErr {
					t.Fatalf("%s: StreamRoot(%s) = %v, want error %v", format, tt.checksum[:8], err, tt.wantErr)
				}
			}
		}
	}
}

// TestDownloadChecksumVerifyWithOpenDecoder reads part of a zstd payload
// and verifies the download checksum while the decoder is still open, like
// streamABRaw after the root partition. Nothing may read the body
// concurrently (fails under -race with a read-ahead decoder).
func TestDownloadChecksumVerifyWithOpenDecoder(t *testing.T) {
	payload := make([]byte, 16<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	downloaded := compressForTest(t, "zst", payload)
	for range 3 {
		opt, err := withDownloadChecksum(StreamOpts{Checksum: sha256Hex(downloaded)})
		if err != nil {
			t.Fatal(err)
		}
		dec, closer, err := Decompressor(opt.download.source(bytes.NewReader(downloaded)), FormatZstd)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(dec, make([]byte, 1<<20)); err != nil {
			t.Fatal(err)
		}
		if err := opt.download.verify(); err != nil {
			t.Fatalf("verify() = %v", err)
		}
		if closer != nil {
			_ = closer.Close()
		}
	}
}

// TestStreamChecksumScopeTrailingData: data after the last compressed frame
// (a zstd skippable frame, as padding) belongs to the download, so the
// download-scope digest covers it.
func TestStreamChecksumScopeTrailingData(t *testing.T) {
	data := bytes.Repeat([]byte("payload "), 8192)
	skippable := append([]byte{0x50, 0x2a, 0x4d, 0x18, 8, 0, 0, 0}, []byte("PADDING!")...)
	downloaded := append(compressForTest(t, "zst", data), skippable...)
	url := serveBytes(t, downloaded) + "/image.raw.zst"
	for _, tt := range []struct {
		checksum string
		wantErr  bool
	}{
		{checksum: sha256Hex(downloaded)},
		{checksum: sha256Hex(downloaded[:len(downloaded)-len(skippable)]), wantErr: true},
	} {
		disk := filepath.Join(t.TempDir(), "disk.img")
		if err := os.WriteFile(disk, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := Stream(context.Background(), url, disk, StreamOpts{Checksum: tt.checksum})
		if (err != nil) != tt.wantErr {
			t.Fatalf("Stream(%s) = %v, want error %v", tt.checksum[:8], err, tt.wantErr)
		}
		if tt.wantErr && (!errors.Is(err, ErrChecksumMismatch) || !strings.Contains(err.Error(), "IMAGE_CHECKSUM_SCOPE=download")) {
			t.Fatalf("Stream() = %v, want a checksum mismatch that names IMAGE_CHECKSUM_SCOPE", err)
		}
	}
}
