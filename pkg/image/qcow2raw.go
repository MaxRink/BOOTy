//go:build linux

package image

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// qcow2 on-disk format, docs/interop/qcow2.rst of QEMU. BOOTy reads the
// active L1/L2 tables of a self-contained image and ignores refcounts,
// snapshots and bitmaps, like `qemu-img convert -O raw` does.
const (
	qcow2V3HeaderLen = 104 // up to header_length, the minimum of a v3 header

	qcow2IncompatDirty        = 1 << 0 // refcounts may be wrong; data is fine
	qcow2IncompatCorrupt      = 1 << 1 // metadata may be wrong; QEMU still reads it
	qcow2IncompatCompressType = 1 << 3 // compression_type field is valid
	qcow2IncompatExtendedL2   = 1 << 4 // 128-bit L2 entries with subclusters
	qcow2IncompatKnown        = qcow2IncompatDirty | qcow2IncompatCorrupt |
		qcow2IncompatExternalData | qcow2IncompatCompressType | qcow2IncompatExtendedL2

	qcow2CompressZlib = 0
	qcow2CompressZstd = 1

	qcow2OffsetMask = 0x00fffffffffffe00 // bits 9-55: L1 and L2 host offsets
	qcow2Compressed = 1 << 62
	qcow2ZeroFlag   = 1 << 0 // v3 standard cluster reads as zeros

	qcow2MinClusterBits = 9
	qcow2MaxClusterBits = 21
	qcow2MaxL1Bytes     = 32 << 20 // QCOW_MAX_L1_SIZE of QEMU
	qcow2Subclusters    = 32

	// sparseBlock is the run of zeros that the raw file leaves as a hole,
	// the default of `qemu-img convert -S`.
	sparseBlock = 4096
)

// qcow2Header holds the fields of a qcow2 header that a raw conversion needs.
type qcow2Header struct {
	version     uint32
	clusterBits uint32
	size        uint64
	l1Size      uint32
	l1Offset    uint64
	compression byte
	extendedL2  bool
}

func (h *qcow2Header) clusterSize() int64 { return 1 << h.clusterBits }

// l2Entries is the number of guest clusters that one L2 table maps.
func (h *qcow2Header) l2Entries() int64 {
	if h.extendedL2 {
		return h.clusterSize() / 16
	}
	return h.clusterSize() / 8
}

// parseQCOW2Header reads and validates a qcow2 header. It fails for an image
// that names a backing file or an external data file: QEMU would open that
// file from the initramfs or the network and copy it into the raw image.
func parseQCOW2Header(b []byte) (*qcow2Header, error) {
	if len(b) < 72 || !bytes.Equal(b[:4], qcow2Magic) {
		return nil, fmt.Errorf("not a valid qcow2 header")
	}
	be := binary.BigEndian
	h := &qcow2Header{
		version:     be.Uint32(b[4:8]),
		clusterBits: be.Uint32(b[20:24]),
		size:        be.Uint64(b[24:32]),
		l1Size:      be.Uint32(b[36:40]),
		l1Offset:    be.Uint64(b[40:48]),
	}
	if be.Uint64(b[8:16]) != 0 {
		return nil, fmt.Errorf("qcow2 image has a backing file; only self-contained images are supported")
	}
	if h.version < 2 || h.version > 3 {
		return nil, fmt.Errorf("qcow2 version %d is not supported", h.version)
	}
	if h.version == 3 {
		if err := parseQCOW2V3Fields(h, b); err != nil {
			return nil, err
		}
	}
	if crypt := be.Uint32(b[32:36]); crypt != 0 {
		return nil, fmt.Errorf("qcow2 image is encrypted (method %d); not supported", crypt)
	}
	if err := checkQCOW2Layout(h); err != nil {
		return nil, err
	}
	return h, nil
}

// checkQCOW2Layout bounds the sizes of an untrusted header, so the
// conversion allocates and seeks only within them.
func checkQCOW2Layout(h *qcow2Header) error {
	if h.clusterBits < qcow2MinClusterBits || h.clusterBits > qcow2MaxClusterBits {
		return fmt.Errorf("qcow2 cluster bits %d out of range [%d, %d]", h.clusterBits, qcow2MinClusterBits, qcow2MaxClusterBits)
	}
	if h.size > 1<<56 {
		return fmt.Errorf("qcow2 virtual size %d is too large", h.size)
	}
	need := (int64(h.size) + h.clusterSize() - 1) / h.clusterSize() //nolint:gosec // size bounded above
	needL1 := (need + h.l2Entries() - 1) / h.l2Entries()
	if int64(h.l1Size) < needL1 {
		return fmt.Errorf("qcow2 L1 table has %d entries, the virtual size needs %d", h.l1Size, needL1)
	}
	if int64(h.l1Size)*8 > qcow2MaxL1Bytes {
		return fmt.Errorf("qcow2 L1 table of %d entries is too large", h.l1Size)
	}
	if h.l1Size > 0 && h.l1Offset&uint64(h.clusterSize()-1) != 0 { //nolint:gosec // clusterSize is at most 2 MiB
		return fmt.Errorf("qcow2 L1 table offset %#x is not cluster aligned", h.l1Offset)
	}
	return nil
}

func parseQCOW2V3Fields(h *qcow2Header, b []byte) error {
	if len(b) < qcow2HeaderLen {
		return fmt.Errorf("qcow2 v3 header is truncated")
	}
	incompat := binary.BigEndian.Uint64(b[72:80])
	if incompat&qcow2IncompatExternalData != 0 {
		return fmt.Errorf("qcow2 image has an external data file; only self-contained images are supported")
	}
	if unknown := incompat &^ qcow2IncompatKnown; unknown != 0 {
		return fmt.Errorf("qcow2 image has unknown incompatible features %#x", unknown)
	}
	h.extendedL2 = incompat&qcow2IncompatExtendedL2 != 0
	hasField := len(b) > qcow2V3HeaderLen && binary.BigEndian.Uint32(b[100:104]) > qcow2V3HeaderLen
	if incompat&qcow2IncompatCompressType == 0 {
		// Like QEMU: a compression type other than zlib needs the bit.
		if hasField && b[104] != qcow2CompressZlib {
			return fmt.Errorf("qcow2 compression type %d without the compression type feature bit", b[104])
		}
		return nil
	}
	if !hasField {
		return fmt.Errorf("qcow2 header announces a compression type but has no field for it")
	}
	h.compression = b[104]
	if h.compression != qcow2CompressZlib && h.compression != qcow2CompressZstd {
		return fmt.Errorf("qcow2 compression type %d is not supported", h.compression)
	}
	return nil
}

// qcow2ToRaw writes the guest view of the qcow2 image src to the raw file
// dst. Unallocated and zero clusters, and zero runs of sparseBlock bytes,
// stay holes, so a tmpfs only holds the data, like with qemu-img convert.
func qcow2ToRaw(ctx context.Context, src, dst *os.File) error {
	h, err := readQCOW2Header(src)
	if err != nil {
		return err
	}
	fi, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat qcow2 image: %w", err)
	}
	c := &qcow2Converter{h: h, src: src, srcSize: fi.Size(), dst: dst, cluster: make([]byte, h.clusterSize())}
	defer c.close()
	l1 := make([]byte, int64(h.l1Size)*8)
	if err := c.readFullAt(l1, int64(h.l1Offset)); err != nil { //nolint:gosec // bounded by the file
		return fmt.Errorf("read qcow2 L1 table: %w", err)
	}
	l2 := make([]byte, h.clusterSize())
	perL2 := h.l2Entries()
	for i := int64(0); i*perL2*h.clusterSize() < int64(h.size); i++ { //nolint:gosec // size bounded in parse
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("qcow2 conversion: %w", err)
		}
		l2Offset := binary.BigEndian.Uint64(l1[i*8:]) & qcow2OffsetMask
		if l2Offset == 0 {
			continue // unallocated: zeros
		}
		if l2Offset&uint64(h.clusterSize()-1) != 0 { //nolint:gosec // at most 2 MiB
			return fmt.Errorf("qcow2 L2 table offset %#x is not cluster aligned", l2Offset)
		}
		if err := c.readFullAt(l2, int64(l2Offset)); err != nil { //nolint:gosec // masked to 56 bits
			return fmt.Errorf("read qcow2 L2 table %d: %w", i, err)
		}
		if err := c.convertL2(l2, i*perL2); err != nil {
			return err
		}
	}
	if err := dst.Truncate(int64(h.size)); err != nil { //nolint:gosec // size bounded in parse
		return fmt.Errorf("set raw image size: %w", err)
	}
	return nil
}

// readQCOW2Header reads and validates the header of the qcow2 file f.
func readQCOW2Header(f *os.File) (*qcow2Header, error) {
	hb := make([]byte, 512)
	n, err := f.ReadAt(hb, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read qcow2 header: %w", err)
	}
	return parseQCOW2Header(hb[:n])
}

type qcow2Converter struct {
	h       *qcow2Header
	src     *os.File
	srcSize int64
	dst     *os.File
	cluster []byte
	zstd    *zstd.Decoder
}

func (c *qcow2Converter) close() {
	if c.zstd != nil {
		c.zstd.Close()
	}
}

// convertL2 writes the guest clusters that one L2 table maps, starting at
// guest cluster first.
func (c *qcow2Converter) convertL2(l2 []byte, first int64) error {
	cs := c.h.clusterSize()
	width := 8
	if c.h.extendedL2 {
		width = 16
	}
	for j := range c.h.l2Entries() {
		guest := (first + j) * cs
		if guest >= int64(c.h.size) { //nolint:gosec // size bounded in parse
			return nil
		}
		entry := binary.BigEndian.Uint64(l2[j*int64(width):])
		var bitmap uint64
		if c.h.extendedL2 {
			bitmap = binary.BigEndian.Uint64(l2[j*int64(width)+8:])
		}
		if err := c.convertCluster(entry, bitmap, guest); err != nil {
			return fmt.Errorf("qcow2 guest offset %#x: %w", guest, err)
		}
	}
	return nil
}

func (c *qcow2Converter) convertCluster(entry, bitmap uint64, guest int64) error {
	if entry&qcow2Compressed != 0 {
		if err := c.readCompressed(entry); err != nil {
			return err
		}
		return writeSparse(c.dst, c.cluster, guest)
	}
	if !c.h.extendedL2 && c.h.version >= 3 && entry&qcow2ZeroFlag != 0 {
		return nil
	}
	host := entry & qcow2OffsetMask
	if host == 0 {
		return nil // unallocated, or all subclusters unallocated or zero
	}
	cs := c.h.clusterSize()
	if host&uint64(cs-1) != 0 { //nolint:gosec // at most 2 MiB
		return fmt.Errorf("data cluster offset %#x is not cluster aligned", host)
	}
	if err := c.readFullAt(c.cluster, int64(host)); err != nil { //nolint:gosec // masked to 56 bits
		return fmt.Errorf("read data cluster: %w", err)
	}
	if c.h.extendedL2 {
		// Low 32 bits: subcluster allocated; high 32 bits: reads as zero.
		// An unallocated subcluster reads as zero without a backing file.
		sub := cs / qcow2Subclusters
		for k := range int64(qcow2Subclusters) {
			allocated := bitmap&(1<<k) != 0
			zero := bitmap&(1<<(32+k)) != 0
			if zero || !allocated {
				clear(c.cluster[k*sub : (k+1)*sub])
			}
		}
	}
	return writeSparse(c.dst, c.cluster, guest)
}

// readCompressed decompresses the cluster of a compressed L2 entry into
// c.cluster.
func (c *qcow2Converter) readCompressed(entry uint64) error {
	shift := 62 - (c.h.clusterBits - 8)
	offset := entry & (1<<shift - 1)
	sectors := (entry>>shift)&(1<<(c.h.clusterBits-8)-1) + 1
	size := int64(sectors)*512 - int64(offset&511) //nolint:gosec // at most 2 * cluster size
	if int64(offset) >= c.srcSize {                //nolint:gosec // masked to 62 bits
		return fmt.Errorf("compressed cluster at %#x starts past the end of the image (%d bytes): truncated download?", offset, c.srcSize)
	}
	buf := make([]byte, size)
	n, err := c.src.ReadAt(buf, int64(offset)) //nolint:gosec // masked to 62 bits
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read compressed cluster: %w", err)
	}
	// The last compressed cluster may end before its last sector.
	buf = buf[:n]
	var r io.Reader
	switch c.h.compression {
	case qcow2CompressZstd:
		if c.zstd == nil {
			// A cluster never needs a window larger than itself; a frame
			// that claims one is refused before any allocation.
			window := uint64(max(c.h.clusterSize(), zstd.MinWindowSize)) //nolint:gosec // at most 2 MiB
			c.zstd, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(window))
			if err != nil {
				return fmt.Errorf("zstd decoder: %w", err)
			}
		}
		if err := c.zstd.Reset(bytes.NewReader(buf)); err != nil {
			return fmt.Errorf("zstd cluster: %w", err)
		}
		r = c.zstd
	default:
		// QEMU writes raw deflate without a zlib header.
		r = flate.NewReader(bytes.NewReader(buf))
	}
	if _, err := io.ReadFull(r, c.cluster); err != nil {
		return fmt.Errorf("decompress cluster: %w", err)
	}
	return nil
}

// writeSparse writes b at off and skips sparseBlock runs of zeros.
func writeSparse(dst *os.File, b []byte, off int64) error {
	for start := 0; start < len(b); start += sparseBlock {
		end := min(start+sparseBlock, len(b))
		block := b[start:end]
		if allZero(block) {
			continue
		}
		if _, err := dst.WriteAt(block, off+int64(start)); err != nil {
			return fmt.Errorf("write raw image: %w", err)
		}
	}
	return nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// readFullAt reads len(b) bytes at off. A table or cluster that starts past
// the end of the file fails (a truncated download); bytes past the end of a
// table or cluster that starts inside the file read as zeros, like QEMU's
// file driver: the last data cluster may end the image file early.
func (c *qcow2Converter) readFullAt(b []byte, off int64) error {
	if len(b) == 0 {
		return nil
	}
	if off >= c.srcSize {
		return fmt.Errorf("offset %#x is past the end of the image (%d bytes): truncated download?", off, c.srcSize)
	}
	n, err := c.src.ReadAt(b, off)
	if errors.Is(err, io.EOF) {
		clear(b[n:])
		return nil
	}
	return err //nolint:wrapcheck // callers wrap
}
