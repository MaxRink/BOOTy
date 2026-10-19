//go:build linux

package image

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// testCluster is one guest cluster of a qcow2 fixture.
type testCluster struct {
	kind  string // "data", "zero", "zeroalloc", "unalloc", "compressed", "rawcompressed", "sub"
	data  []byte // guest bytes; the stored bytes for rawcompressed
	alloc uint32 // sub: allocated subclusters
	zeros uint32 // sub: zero subclusters
}

type qcow2Fixture struct {
	version     uint32
	clusterBits uint32
	extendedL2  bool
	compression byte
	size        uint64
	clusters    []testCluster
}

// build writes the fixture as a qcow2 file: header in cluster 0, L1 in
// cluster 1, the L2 tables, the data clusters, then the compressed clusters
// packed at unaligned offsets.
func (f qcow2Fixture) build(tb testing.TB) []byte {
	tb.Helper()
	cs := 1 << f.clusterBits
	width := 8
	if f.extendedL2 {
		width = 16
	}
	perL2 := cs / width
	nL2 := (len(f.clusters) + perL2 - 1) / perL2
	img := make([]byte, (2+nL2)*cs)
	be := binary.BigEndian
	copy(img, qcow2Magic)
	be.PutUint32(img[4:], f.version)
	be.PutUint32(img[20:], f.clusterBits)
	be.PutUint64(img[24:], f.size)
	be.PutUint32(img[36:], uint32(nL2))
	be.PutUint64(img[40:], uint64(cs))
	if f.version == 3 {
		var incompat uint64
		if f.extendedL2 {
			incompat |= qcow2IncompatExtendedL2
		}
		if f.compression != 0 {
			incompat |= qcow2IncompatCompressType
		}
		be.PutUint64(img[72:], incompat)
		be.PutUint32(img[96:], 4)
		be.PutUint32(img[100:], 112)
		img[104] = f.compression
	}
	for i := range nL2 {
		be.PutUint64(img[cs+i*8:], uint64((2+i)*cs)|1<<63)
	}
	var compressed [][]byte
	var compressedAt []int
	for i, c := range f.clusters {
		e := (2+i/perL2)*cs + (i%perL2)*width
		switch c.kind {
		case "data", "sub":
			off := len(img)
			img = append(img, c.data...)
			be.PutUint64(img[e:], uint64(off)|1<<63)
			switch {
			case c.kind == "sub":
				be.PutUint64(img[e+8:], uint64(c.zeros)<<32|uint64(c.alloc))
			case f.extendedL2:
				be.PutUint64(img[e+8:], 0xffffffff) // every subcluster allocated
			}
		case "zero":
			be.PutUint64(img[e:], qcow2ZeroFlag)
		case "zeroalloc":
			// The zero flag on an allocated cluster with non-zero data:
			// the guest reads zeros (QEMU write -z without unmap).
			off := len(img)
			img = append(img, c.data...)
			be.PutUint64(img[e:], uint64(off)|1<<63|qcow2ZeroFlag)
		case "compressed":
			compressed = append(compressed, f.compress(tb, c.data))
			compressedAt = append(compressedAt, e)
		case "rawcompressed":
			compressed = append(compressed, c.data)
			compressedAt = append(compressedAt, e)
		}
	}
	// Start the compressed data mid-sector, like QEMU packs them.
	img = append(img, make([]byte, 100)...)
	shift := 62 - (f.clusterBits - 8)
	for k, data := range compressed {
		off := len(img)
		sectors := (off%512+len(data)+511)/512 - 1
		be.PutUint64(img[compressedAt[k]:], 1<<62|uint64(sectors)<<shift|uint64(off))
		img = append(img, data...)
	}
	return img
}

func (f qcow2Fixture) compress(tb testing.TB, data []byte) []byte {
	tb.Helper()
	if f.compression == qcow2CompressZstd {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			tb.Fatal(err)
		}
		return enc.EncodeAll(data, nil)
	}
	var b bytes.Buffer
	w, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		tb.Fatal(err)
	}
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes()
}

// convertFixture converts img and returns the raw bytes and the bytes
// that the raw file allocates.
func convertFixture(t *testing.T, img []byte) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "in.qcow2")
	dst := filepath.Join(dir, "out.raw")
	if err := os.WriteFile(src, img, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := convertQCOW2ToRaw(context.Background(), src, dst); err != nil {
		if _, statErr := os.Stat(dst); statErr == nil {
			t.Errorf("failed conversion left %s", dst)
		}
		return nil, err
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return raw, nil
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

func TestQCOW2ToRawClusterKinds(t *testing.T) {
	for _, tt := range []struct {
		name string
		f    qcow2Fixture
	}{
		{"v2", qcow2Fixture{version: 2, clusterBits: 9}},
		{"v3 zlib", qcow2Fixture{version: 3, clusterBits: 9}},
		{"v3 zstd", qcow2Fixture{version: 3, clusterBits: 9, compression: qcow2CompressZstd}},
		{"v3 extended L2", qcow2Fixture{version: 3, clusterBits: 10, extendedL2: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.f
			c := 1 << f.clusterBits
			// More clusters than one L2 table maps, so two L2 tables.
			n := c/8 + 5
			var want []byte
			for i := range n {
				data := pattern(c, byte(i))
				switch {
				case i%5 == 1:
					f.clusters = append(f.clusters, testCluster{kind: "unalloc"})
					data = make([]byte, c)
				case i%5 == 2 && f.version == 3 && !f.extendedL2:
					f.clusters = append(f.clusters, testCluster{kind: "zero"})
					data = make([]byte, c)
				case i%5 == 3:
					f.clusters = append(f.clusters, testCluster{kind: "compressed", data: data})
				case i%5 == 4 && f.extendedL2:
					// Subclusters 0 and 1 allocated, 2 reads as zero, the
					// rest unallocated.
					f.clusters = append(f.clusters, testCluster{kind: "sub", data: data, alloc: 0b111, zeros: 0b100})
					data = append(append([]byte{}, data[:2*c/32]...), make([]byte, c-2*c/32)...)
				default:
					f.clusters = append(f.clusters, testCluster{kind: "data", data: data})
				}
				want = append(want, data...)
			}
			// The virtual size ends inside the last cluster.
			f.size = uint64(len(want) - c/2)
			want = want[:f.size]
			got, err := convertFixture(t, f.build(t))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("raw image differs from the guest data (len %d, want %d)", len(got), len(want))
			}
		})
	}
}

func TestQCOW2ToRawRejects(t *testing.T) {
	ok := qcow2Fixture{version: 3, clusterBits: 9, size: 512, clusters: []testCluster{{kind: "data", data: pattern(512, 1)}}}
	for _, tt := range []struct {
		name    string
		mutate  func([]byte)
		wantErr string
	}{
		{"backing file", func(b []byte) { binary.BigEndian.PutUint64(b[8:], 0x1000) }, "backing file"},
		{"external data file", func(b []byte) { b[79] |= qcow2IncompatExternalData }, "external data file"},
		{"unknown feature", func(b []byte) { b[78] |= 1 }, "unknown incompatible features"},
		{"encrypted", func(b []byte) { binary.BigEndian.PutUint32(b[32:], 2) }, "encrypted"},
		{"version 4", func(b []byte) { binary.BigEndian.PutUint32(b[4:], 4) }, "version 4"},
		{"huge cluster", func(b []byte) { binary.BigEndian.PutUint32(b[20:], 30) }, "cluster bits"},
		{"small L1", func(b []byte) { binary.BigEndian.PutUint64(b[24:], 1<<40) }, "L1 table"},
		{"compression type", func(b []byte) { b[79] |= qcow2IncompatCompressType; b[104] = 9 }, "compression type 9"},
		{"compression type without its bit", func(b []byte) { b[104] = qcow2CompressZstd }, "without the compression type feature bit"},
		// Cluster 1 is the L1 table, cluster 2 the L2 table, cluster 3 the data.
		{"L1 past EOF", func(b []byte) { binary.BigEndian.PutUint64(b[40:], 1<<20) }, "past the end"},
		{"L2 past EOF", func(b []byte) { binary.BigEndian.PutUint64(b[512:], 1<<20|1<<63) }, "past the end"},
		{"data past EOF", func(b []byte) { binary.BigEndian.PutUint64(b[1024:], 1<<20|1<<63) }, "past the end"},
		{"not qcow2", func(b []byte) { b[0] = 0 }, "not a valid qcow2 header"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			img := ok.build(t)
			tt.mutate(img)
			_, err := convertFixture(t, img)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// The zero flag wins over the host cluster that it keeps allocated.
func TestQCOW2ToRawZeroFlagHidesHostCluster(t *testing.T) {
	f := qcow2Fixture{version: 3, clusterBits: 9, size: 1024, clusters: []testCluster{
		{kind: "zeroalloc", data: pattern(512, 9)},
		{kind: "data", data: pattern(512, 4)},
	}}
	got, err := convertFixture(t, f.build(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := append(make([]byte, 512), pattern(512, 4)...); !bytes.Equal(got, want) {
		t.Fatal("a zero-flag cluster did not read as zeros")
	}
}

// A zstd frame that claims a window larger than a cluster fails before the
// decoder allocates it.
func TestQCOW2ToRawRejectsLargeZstdWindow(t *testing.T) {
	// Magic, descriptor 0x44 (2-byte content size, checksum), window
	// descriptor 0x98 (2^29 bytes), content size, then a raw last block.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x44, 0x98, 0x00, 0x01, 0x01, 0x10, 0x00, 0xaa, 0xaa}
	f := qcow2Fixture{version: 3, clusterBits: 9, compression: qcow2CompressZstd, size: 512,
		clusters: []testCluster{{kind: "rawcompressed", data: frame}}}
	if _, err := convertFixture(t, f.build(t)); err == nil || !strings.Contains(err.Error(), "window") {
		t.Fatalf("err = %v, want a zstd window error", err)
	}
}

func TestCheckQCOW2FitsDevice(t *testing.T) {
	f := qcow2Fixture{version: 3, clusterBits: 9, size: 512, clusters: []testCluster{{kind: "data", data: pattern(512, 1)}}}
	path := filepath.Join(t.TempDir(), "img.qcow2")
	if err := os.WriteFile(path, f.build(t), 0o600); err != nil {
		t.Fatal(err)
	}
	// /dev/null is a device of size 0.
	if err := checkQCOW2FitsDevice(path, "/dev/null"); err == nil || !strings.Contains(err.Error(), "larger than the target device") {
		t.Fatalf("err = %v, want larger than the target device", err)
	}
	// A regular file as target has no fixed size.
	if err := checkQCOW2FitsDevice(path, filepath.Join(t.TempDir(), "disk")); err != nil {
		t.Fatalf("regular file target: %v", err)
	}
}

func TestQCOW2ToRawRejectsUnalignedDataCluster(t *testing.T) {
	f := qcow2Fixture{version: 3, clusterBits: 10, size: 1024, clusters: []testCluster{{kind: "data", data: pattern(1024, 1)}}}
	img := f.build(t)
	// Cluster 2 is the L2 table; move its entry half a cluster.
	e := binary.BigEndian.Uint64(img[2048:])
	binary.BigEndian.PutUint64(img[2048:], e+512)
	if _, err := convertFixture(t, img); err == nil || !strings.Contains(err.Error(), "not cluster aligned") {
		t.Fatalf("err = %v, want not cluster aligned", err)
	}
}

// TestQCOW2ToRawMatchesQemuImg converts images that qemu-img wrote, with
// the cluster sizes, compression types and L2 formats that QEMU offers, and
// compares the result with the source.
func TestQCOW2ToRawMatchesQemuImg(t *testing.T) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "src.raw")
	// 9 MiB + 3 KiB: random data, a 2 MiB hole, a zero run, a partial tail.
	src := make([]byte, 9<<20+3<<10)
	_, _ = rand.Read(src[:3<<20])
	copy(src[5<<20:], bytes.Repeat([]byte("BOOTy qcow2 "), 100000))
	_, _ = rand.Read(src[len(src)-5000:])
	if err := os.WriteFile(raw, src, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"v2 64k", []string{"-o", "compat=0.10"}},
		{"v3 64k", nil},
		{"v3 512 bytes", []string{"-o", "cluster_size=512"}},
		{"v3 2M", []string{"-o", "cluster_size=2M"}},
		{"zlib", []string{"-c"}},
		{"zstd", []string{"-c", "-o", "compression_type=zstd"}},
		{"extended L2", []string{"-o", "extended_l2=on,cluster_size=128k"}},
		{"extended L2 zstd", []string{"-c", "-o", "extended_l2=on,compression_type=zstd"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := filepath.Join(t.TempDir(), "img.qcow2")
			args := append([]string{"convert", "-f", "raw", "-O", "qcow2"}, tt.args...)
			if out, err := exec.CommandContext(t.Context(), qemuImg, append(args, raw, q)...).CombinedOutput(); err != nil {
				t.Fatalf("qemu-img %v: %v: %s", tt.args, err, out)
			}
			img, err := os.ReadFile(q)
			if err != nil {
				t.Fatal(err)
			}
			got, err := convertFixture(t, img)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, src) {
				t.Fatal("converted image differs from the source")
			}
		})
	}
}

// TestQCOW2ToRawMatchesQemuIO builds images with qemu-io and qemu-img
// snapshot, which reach the zero flag on allocated clusters, discarded
// clusters and snapshots (shared clusters, a second L1 table), and compares
// the conversion with `qemu-img convert -O raw` of the same image.
func TestQCOW2ToRawMatchesQemuIO(t *testing.T) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not installed")
	}
	qemuIO, err := exec.LookPath("qemu-io")
	if err != nil {
		t.Skip("qemu-io not installed")
	}
	type step struct{ tool, arg string } // tool "io": qemu-io -c arg; "snap": qemu-img snapshot -c arg
	for _, tt := range []struct {
		name  string
		opts  string
		steps []step
	}{
		{"zero flag on allocated clusters", "", []step{
			{"io", "write -P 0xaa 0 1M"},
			{"io", "write -z 64k 128k"},
		}},
		{"snapshots", "cluster_size=4k", []step{
			{"io", "write -P 0x11 0 2M"},
			{"snap", "s1"},
			{"io", "write -P 0x22 512k 256k"},
			{"io", "write -z 1M 64k"},
			{"snap", "s2"},
			{"io", "write -P 0x33 1536k 128k"},
			{"io", "discard 0 64k"},
			{"io", "write -z -u 256k 64k"},
		}},
		{"snapshots zstd extended L2", "compression_type=zstd,extended_l2=on", []step{
			{"io", "write -c -P 0x44 0 256k"},
			{"snap", "s1"},
			{"io", "write -P 0x55 8k 4k"},
			{"io", "write -z 300k 16k"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			img := filepath.Join(dir, "img.qcow2")
			args := []string{"create", "-f", "qcow2"}
			if tt.opts != "" {
				args = append(args, "-o", tt.opts)
			}
			runTool(t, qemuImg, append(args, img, "4M")...)
			for _, s := range tt.steps {
				if s.tool == "snap" {
					runTool(t, qemuImg, "snapshot", "-c", s.arg, img)
					continue
				}
				runTool(t, qemuIO, "-f", "qcow2", "-c", s.arg, img)
			}
			ref := filepath.Join(dir, "ref.raw")
			runTool(t, qemuImg, "convert", "-f", "qcow2", "-O", "raw", img, ref)
			want, err := os.ReadFile(ref)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(img)
			if err != nil {
				t.Fatal(err)
			}
			got, err := convertFixture(t, data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("conversion differs from qemu-img convert")
			}
		})
	}
}

func runTool(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// FuzzQCOW2ToRaw converts arbitrary images: no panic, no hang. Images with
// a virtual size above 64 MiB are skipped to bound the work per input.
func FuzzQCOW2ToRaw(f *testing.F) {
	for _, fx := range []qcow2Fixture{
		{version: 2, clusterBits: 9, size: 1024, clusters: []testCluster{{kind: "data", data: pattern(512, 1)}, {kind: "compressed", data: pattern(512, 2)}}},
		{version: 3, clusterBits: 9, size: 1536, clusters: []testCluster{{kind: "zero"}, {kind: "zeroalloc", data: pattern(512, 3)}, {kind: "unalloc"}}},
		{version: 3, clusterBits: 9, compression: qcow2CompressZstd, size: 512, clusters: []testCluster{{kind: "compressed", data: pattern(512, 4)}}},
		{version: 3, clusterBits: 10, extendedL2: true, size: 2048, clusters: []testCluster{
			{kind: "sub", data: pattern(1024, 5), alloc: 0b1011, zeros: 0b0100}, {kind: "data", data: pattern(1024, 6)}}},
	} {
		f.Add(fx.build(f))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, img []byte) {
		if h, err := parseQCOW2Header(img[:min(len(img), 512)]); err == nil && h.size > 64<<20 {
			t.Skip("virtual size above 64 MiB")
		}
		src := filepath.Join(dir, "in.qcow2")
		dst := filepath.Join(dir, "out.raw")
		if err := os.WriteFile(src, img, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		_ = convertQCOW2ToRaw(ctx, src, dst)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("conversion took %s", d)
		}
	})
}
