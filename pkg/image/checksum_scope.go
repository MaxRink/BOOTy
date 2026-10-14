package image

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
)

// Values of StreamOpts.ChecksumScope.
const (
	// ChecksumScopeDownload checks the bytes as downloaded, before
	// decompression or qcow2 conversion. Checksum files published next to
	// an image (image.raw.lz4.sha256) cover these bytes. For oci:// sources
	// these are the uncompressed layer bytes (layer.Uncompressed()), not the
	// compressed blob that the layer digest covers.
	ChecksumScopeDownload = "download"
	// ChecksumScopeDecompressed checks the decompressed image. For qcow2
	// images it checks the converted raw image.
	ChecksumScopeDecompressed = "decompressed"
)

// downloadChecksum hashes the downloaded bytes of an image for a checksum
// with ChecksumScopeDownload.
type downloadChecksum struct {
	opt  StreamOpts
	h    hash.Hash
	src  io.Reader
	once sync.Once
	err  error
}

// withDownloadChecksum moves a download-scope checksum out of opt. The
// returned options carry no payload checksum, and every verifyChecksum call
// with them checks the download checksum first.
func withDownloadChecksum(opt StreamOpts) (StreamOpts, error) {
	if opt.Checksum == "" || opt.download != nil {
		return opt, nil
	}
	switch strings.ToLower(strings.TrimSpace(opt.ChecksumScope)) {
	case "", ChecksumScopeDownload:
	case ChecksumScopeDecompressed:
		return opt, nil
	default:
		return opt, fmt.Errorf("unsupported checksum scope: %s", opt.ChecksumScope)
	}
	normalized, err := normalizeChecksumOpt(opt)
	if err != nil {
		return opt, err
	}
	h, err := newHash(normalized.ChecksumType)
	if err != nil {
		return opt, err
	}
	normalized.ChecksumScope = ChecksumScopeDownload
	opt.download = &downloadChecksum{opt: normalized, h: h}
	opt.Checksum, opt.ChecksumType = "", ""
	return opt, nil
}

// source hashes everything read from the downloaded body r.
func (d *downloadChecksum) source(r io.Reader) io.Reader {
	if d == nil {
		return r
	}
	d.src = io.TeeReader(r, d.h)
	return d.src
}

// verifyAtEOF returns a reader that verifies the download checksum before
// it reports the end of r. Readers that copy the image into a file then
// fail before the file is used.
func (d *downloadChecksum) verifyAtEOF(r io.Reader) io.Reader {
	if d == nil {
		return r
	}
	return &eofVerifier{r: r, verify: d.verify}
}

// verify reads the rest of the download, for example data after the end of
// a compressed stream, and compares the digest. Only the first call reads.
func (d *downloadChecksum) verify() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		if d.src == nil {
			d.err = errors.New("download checksum: image source was not opened")
			return
		}
		if _, err := io.Copy(io.Discard, d.src); err != nil {
			d.err = fmt.Errorf("reading the rest of the download for its checksum: %w", err)
			return
		}
		d.err = verifyChecksum(d.h, d.opt)
	})
	return d.err
}

type eofVerifier struct {
	r      io.Reader
	verify func() error
}

func (e *eofVerifier) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		if verr := e.verify(); verr != nil {
			return n, verr
		}
	}
	return n, err //nolint:wrapcheck // a Reader passes io.EOF through unwrapped
}
