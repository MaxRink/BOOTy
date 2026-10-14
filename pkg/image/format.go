package image

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// Format represents a detected compression, disk image, or container format.
type Format string

// Detected compression and image formats.
const (
	FormatRaw   Format = "raw"
	FormatGzip  Format = "gzip"
	FormatZstd  Format = "zstd"
	FormatLZ4   Format = "lz4"
	FormatXZ    Format = "xz"
	FormatBzip2 Format = "bzip2"
	FormatQCOW2 Format = "qcow2"
	FormatVMDK  Format = "vmdk"
	FormatOVA   Format = "ova"
	FormatOVF   Format = "ovf"
)

const formatHeaderSize = 512

// Magic byte signatures for auto-detection.
var magicBytes = []struct {
	format Format
	magic  []byte
}{
	{FormatGzip, []byte{0x1f, 0x8b}},
	{FormatZstd, []byte{0x28, 0xb5, 0x2f, 0xfd}},
	{FormatLZ4, []byte{0x04, 0x22, 0x4d, 0x18}},
	{FormatXZ, []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}},
	{FormatBzip2, []byte{0x42, 0x5a, 0x68}},
	{FormatQCOW2, []byte{0x51, 0x46, 0x49, 0xfb}}, // "QFI\xfb"
	{FormatVMDK, []byte{0x4b, 0x44, 0x4d, 0x56}},  // "KDMV"
	{FormatVMDK, []byte("# Disk DescriptorFile")},
}

// DetectFormat peeks at the first bytes of the reader to determine compression,
// disk image, or known unsupported container format. Returns the detected
// format and a new reader that replays the peeked bytes.
func DetectFormat(r io.Reader) (Format, io.Reader, error) {
	br := bufio.NewReaderSize(r, formatHeaderSize)
	header, err := br.Peek(formatHeaderSize)
	if err != nil && len(header) == 0 {
		return FormatRaw, br, fmt.Errorf("peek header: %w", err)
	}

	for _, m := range magicBytes {
		if len(header) >= len(m.magic) && matchBytes(header, m.magic) {
			return m.format, br, nil
		}
	}
	if isOVFHeader(header) {
		return FormatOVF, br, nil
	}
	if isOVAHeader(header) {
		return FormatOVA, br, nil
	}
	return FormatRaw, br, nil
}

// matchBytes checks if data starts with the given prefix.
func matchBytes(data, prefix []byte) bool {
	for i, b := range prefix {
		if data[i] != b {
			return false
		}
	}
	return true
}

func isOVFHeader(header []byte) bool {
	trimmed := bytes.TrimPrefix(header, []byte{0xef, 0xbb, 0xbf})
	trimmed = bytes.TrimLeft(trimmed, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("<?xml")) ||
		bytes.HasPrefix(trimmed, []byte("<Envelope")) ||
		bytes.HasPrefix(trimmed, []byte("<ovf:Envelope"))
}

func isOVAHeader(header []byte) bool {
	return len(header) >= 262 && string(header[257:262]) == "ustar"
}

func isUnsupportedContainerFormat(f Format) bool {
	switch f {
	case FormatVMDK, FormatOVA, FormatOVF:
		return true
	default:
		return false
	}
}

func unsupportedImageFormatError(f Format) error {
	return fmt.Errorf("unsupported image format %s: convert to raw or qcow2 before provisioning", f)
}

// Decompressor wraps a reader with the appropriate decompression based on format.
// The returned io.Reader streams decompressed data. The closer (if non-nil)
// must be closed when done.
func Decompressor(r io.Reader, f Format) (io.Reader, io.Closer, error) {
	if isUnsupportedContainerFormat(f) {
		return nil, nil, unsupportedImageFormatError(f)
	}

	switch f {
	case FormatGzip:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gz, gz, nil

	case FormatZstd:
		// One goroutine: the default decoder reads ahead from r in the
		// background, which races with the drain of a download-scope
		// IMAGE_CHECKSUM after the last frame.
		zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, nil, fmt.Errorf("zstd reader: %w", err)
		}
		closer := zr.IOReadCloser()
		return zr, closer, nil

	case FormatLZ4:
		return lz4.NewReader(r), nil, nil

	case FormatXZ:
		xzr, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("xz reader: %w", err)
		}
		return xzr, nil, nil

	case FormatBzip2:
		return bzip2.NewReader(r), nil, nil

	case FormatQCOW2:
		return nil, nil, fmt.Errorf("qcow2 images cannot be streamed directly; use ConvertQCOW2 first")

	case FormatRaw:
		return r, nil, nil

	default:
		return r, nil, nil
	}
}
