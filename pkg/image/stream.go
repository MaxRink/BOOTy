package image

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/timesync"
)

const imageCopyBufferSize = 16 << 20 // 16 MiB

// imageHTTPClient is the HTTP client used for image downloads.
// It sets connect and TLS timeouts to prevent hanging on broken connections
// while leaving the response body read timeout unlimited (for large images).
var imageHTTPClient = network.NewHTTPClient(0)

// RefreshHTTPClients rebuilds the image download clients so they pick up
// network.SetSkipTLSVerify and proxy settings from the environment.
func RefreshHTTPClients() {
	imageHTTPClient = network.NewHTTPClient(0)
	gpgHTTPClient = network.NewHTTPClient(0)
}

// StreamOpts are optional parameters for Stream.
type StreamOpts struct {
	// Checksum is the expected hex-encoded checksum. ChecksumScope selects
	// the bytes it covers.
	Checksum string
	// ChecksumType is the hash algorithm: "sha256" or "sha512".
	ChecksumType string
	// ChecksumScope is ChecksumScopeDownload (default) or
	// ChecksumScopeDecompressed.
	ChecksumScope string

	// download is set by withDownloadChecksum.
	download *downloadChecksum
}

// Stream downloads an image from a URL (http/https or oci://) and writes it
// to a block device. Compression is auto-detected via magic bytes (gzip, zstd,
// lz4, xz, bzip2). qcow2 images are detected and converted via ramdisk.
// Optional checksum validation is performed after write.
func Stream(ctx context.Context, url, device string, opts ...StreamOpts) error {
	slog.Info("streaming image", "url", RedactURL(url), "device", device) //nolint:gosec // trusted config values

	var opt StreamOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt, err := withDownloadChecksum(opt)
	if err != nil {
		return err
	}

	decompressed, cleanup, format, err := openAndDecompress(ctx, url, opt.download)
	if err != nil {
		return err
	}

	// qcow2 images require download → convert → stream via ramdisk.
	if format == FormatQCOW2 {
		defer cleanup()
		return convertQCOW2Hook(ctx, decompressed, url, device, opt)
	}
	defer cleanup()

	out, err := os.OpenFile(device, os.O_WRONLY, 0) //nolint:gosec // device path from config
	if err != nil {
		return fmt.Errorf("opening device %s: %w", device, err)
	}
	defer func() { _ = out.Close() }()

	counter := &WriteCounter{}
	stopProgress := startProgressTicker(counter)

	src, h, err := wrapChecksum(decompressed, opt)
	if err != nil {
		stopProgress()
		return err
	}

	buf := make([]byte, imageCopyBufferSize)
	written, err := io.CopyBuffer(out, io.TeeReader(src, counter), buf)
	stopProgress()
	if errors.Is(err, ErrChecksumMismatch) {
		slog.Error("checksum mismatch: wiping partition table to prevent booting corrupt image",
			"device", device, "error", err)
		wipeLeadingSectors(device)
		return err //nolint:wrapcheck // ErrChecksumMismatch from verifyAtEOF, already descriptive
	}
	if err != nil {
		slog.Error("image write failed: wiping partial image", "device", device, "error", err)
		wipeLeadingSectors(device)
		return fmt.Errorf("writing to device: %w", err)
	}

	if err := verifyChecksum(h, opt); err != nil {
		slog.Error("checksum mismatch: wiping partition table to prevent booting corrupt image",
			"device", device, "error", err)
		wipeLeadingSectors(device)
		return err
	}

	if err := syncImageTarget(out, device); err != nil {
		slog.Error("image sync failed: attempting to wipe partial image", "device", device, "error", err)
		wipeLeadingSectors(device)
		return err
	}

	fmt.Println()
	slog.Info("image written", "bytes", written, "device", device) //nolint:gosec // trusted config values
	return nil
}

type fileSyncer interface {
	Sync() error
}

var syncImageTarget = func(out fileSyncer, device string) error {
	// Flush all written data to the underlying block device before returning.
	// Without this, subsequent partition table re-reads (BLKRRPART ioctl via
	// partprobe/blockdev) may not see the new partition table because dirty
	// pages haven't reached the disk backend (especially on QEMU/KVM where
	// the guest kernel's writeback queue may lag behind).
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing device %s: %w", device, err)
	}
	return nil
}

// wipeLeadingSectors zeroes the first 1 MiB of a device to invalidate the
// partition table and filesystem superblocks after a failed checksum
// verification. This is best-effort — errors are logged but not returned
// because the real error (checksum mismatch) is already being propagated.
func wipeLeadingSectors(device string) {
	f, err := os.OpenFile(device, os.O_WRONLY, 0) //nolint:gosec // trusted config value
	if err != nil {
		slog.Warn("failed to open device for wipe", "device", device, "error", err)
		return
	}
	defer f.Close() //nolint:errcheck // best-effort close
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		slog.Warn("failed to wipe partition table", "device", device, "error", err)
		return
	}
	if err := f.Sync(); err != nil {
		slog.Warn("failed to sync wipe", "device", device, "error", err)
	}
}

// convertQCOW2Hook is set by the linux build to call ConvertQCOW2FromReader.
// On non-linux platforms, qcow2 conversion is unsupported.
var convertQCOW2Hook = func(_ context.Context, _ io.Reader, _, _ string, _ StreamOpts) error {
	return fmt.Errorf("qcow2 conversion requires linux (tmpfs ramdisk + qemu-img)")
}

// openAndDecompress opens the image source, detects compression, and returns
// the decompressed reader along with a cleanup function and detected format.
// For qcow2 images, the returned reader is the original stream with the
// detection bytes preserved so callers can continue consuming the same source.
// A non-nil download hashes the downloaded bytes and is verified before the
// returned reader reports EOF.
func openAndDecompress(ctx context.Context, url string, download *downloadChecksum) (io.Reader, func(), Format, error) {
	body, err := openSource(ctx, url)
	if err != nil {
		return nil, nil, FormatRaw, err
	}

	format, reader, err := DetectFormat(download.source(body))
	if err != nil {
		_ = body.Close()
		return nil, nil, FormatRaw, fmt.Errorf("detect format: %w", err)
	}
	slog.Info("detected image format", "format", format)

	// qcow2 cannot be decompressed inline — return early so caller can route.
	if format == FormatQCOW2 || format == FormatRaw {
		cleanup := func() { _ = body.Close() }
		return download.verifyAtEOF(reader), cleanup, format, nil
	}

	decompressed, closer, err := Decompressor(reader, format)
	if err != nil {
		_ = body.Close()
		return nil, nil, format, err
	}

	cleanup := func() {
		if closer != nil {
			_ = closer.Close()
		}
		_ = body.Close()
	}

	effectiveFormat, effectiveReader, err := DetectFormat(decompressed)
	if err != nil {
		cleanup()
		return nil, nil, format, fmt.Errorf("detect decompressed format: %w", err)
	}
	slog.Info("detected decompressed image format", "outer_format", format, "payload_format", effectiveFormat)
	return download.verifyAtEOF(effectiveReader), cleanup, effectiveFormat, nil
}

// wrapChecksum wraps the reader with a checksum hash if requested.
func wrapChecksum(r io.Reader, opt StreamOpts) (io.Reader, hash.Hash, error) {
	if opt.Checksum == "" {
		return r, nil, nil
	}
	opt, err := normalizeChecksumOpt(opt)
	if err != nil {
		return nil, nil, err
	}
	h, err := newHash(opt.ChecksumType)
	if err != nil {
		return nil, nil, err
	}
	return io.TeeReader(r, h), h, nil
}

// ErrChecksumMismatch is the error of an IMAGE_CHECKSUM that does not match.
// Its text "checksum mismatch" also marks the error as permanent.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// checksumScopeHint names the bytes that a checksum covered.
func checksumScopeHint(opt StreamOpts) string {
	if opt.ChecksumScope == ChecksumScopeDownload {
		return "IMAGE_CHECKSUM_SCOPE=download: the file as downloaded; " +
			"set IMAGE_CHECKSUM_SCOPE=decompressed for a digest of the decompressed image"
	}
	return "IMAGE_CHECKSUM_SCOPE=decompressed: the decompressed image"
}

// verifyChecksum validates the hash digest against the expected checksum.
// It verifies a download-scope checksum of opt first.
func verifyChecksum(h hash.Hash, opt StreamOpts) error {
	if err := opt.download.verify(); err != nil {
		return err
	}
	if h == nil {
		return nil
	}
	opt, err := normalizeChecksumOpt(opt)
	if err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(opt.Checksum)) != 1 {
		return fmt.Errorf("%w (%s): computed=%s want=%s", ErrChecksumMismatch, checksumScopeHint(opt),
			got[:min(8, len(got))], opt.Checksum[:min(8, len(opt.Checksum))])
	}
	slog.Info("checksum verified", "type", opt.ChecksumType)
	return nil
}

func normalizeChecksumOpt(opt StreamOpts) (StreamOpts, error) {
	opt.Checksum = strings.ToLower(strings.TrimSpace(opt.Checksum))
	opt.ChecksumType = strings.ToLower(strings.TrimSpace(opt.ChecksumType))
	if opt.Checksum == "" {
		return opt, nil
	}

	for _, alg := range []string{"sha256", "sha512"} {
		prefix := alg + ":"
		if strings.HasPrefix(opt.Checksum, prefix) {
			if opt.ChecksumType != "" && opt.ChecksumType != alg {
				return opt, fmt.Errorf("checksum prefix %s conflicts with checksum type %s", alg, opt.ChecksumType)
			}
			opt.ChecksumType = alg
			opt.Checksum = strings.TrimPrefix(opt.Checksum, prefix)
			if opt.Checksum == "" {
				return opt, fmt.Errorf("checksum prefix %s requires a non-empty digest", alg)
			}
			break
		}
	}

	if opt.ChecksumType == "" {
		switch len(opt.Checksum) {
		case sha256.Size * 2:
			opt.ChecksumType = "sha256"
		case sha512.Size * 2:
			opt.ChecksumType = "sha512"
		default:
			return opt, fmt.Errorf("cannot infer checksum type from checksum length %d", len(opt.Checksum))
		}
	}
	if err := validateChecksumDigest(opt.ChecksumType, opt.Checksum); err != nil {
		return opt, err
	}
	return opt, nil
}

// NormalizeChecksum validates and normalizes a user-provided checksum and type.
// It is exported so dry-run checks can fail before destructive provisioning.
func NormalizeChecksum(checksum, checksumType string) (StreamOpts, error) {
	return normalizeChecksumOpt(StreamOpts{Checksum: checksum, ChecksumType: checksumType})
}

func validateChecksumDigest(checksumType, checksum string) error {
	var expectedLen int
	switch checksumType {
	case "sha256":
		expectedLen = sha256.Size * 2
	case "sha512":
		expectedLen = sha512.Size * 2
	default:
		return fmt.Errorf("unsupported checksum type: %s", checksumType)
	}
	if len(checksum) != expectedLen {
		return fmt.Errorf("%s checksum length %d, want %d", checksumType, len(checksum), expectedLen)
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return fmt.Errorf("invalid %s checksum hex: %w", checksumType, err)
	}
	return nil
}

// openSource returns a ReadCloser for the given URL.
// Supports http/https and oci:// protocols.
// HTTP requests are retried up to 3 times with exponential backoff.
func openSource(ctx context.Context, url string) (io.ReadCloser, error) {
	if IsOCIReference(url) {
		ref := TrimOCIScheme(url)
		slog.Info("pulling OCI image", "ref", RedactOCIRef(ref))
		return FetchOCILayerWithRetry(ctx, ref)
	}

	return httpGetWithRetry(ctx, url)
}

// retryBackoffBase is the initial backoff duration for retry loops.
// It is a package-level variable so tests can override it for fast execution.
var retryBackoffBase = time.Second

// httpGetWithRetry performs an HTTP GET with retry and exponential backoff.
func httpGetWithRetry(ctx context.Context, url string) (io.ReadCloser, error) {
	const maxRetries = 3
	backoff := retryBackoffBase
	redactedURL := RedactURL(url)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		// The request context is canceled by the idle timer of the body.
		reqCtx, cancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("creating request for %s: %w", redactedURL, &redactedSourceError{rawSource: url, err: err})
		}

		resp, err := imageHTTPClient.Do(req) //nolint:gosec // URL from trusted config
		if err != nil {
			cancel()
			redactedErr := &redactedSourceError{rawSource: url, err: timesync.ExplainCertTime(err, time.Time{})}
			lastErr = fmt.Errorf("fetching image %s (attempt %d/%d): %w", redactedURL, attempt+1, maxRetries, redactedErr)
			slog.Warn("HTTP request failed, retrying", "attempt", attempt+1, "error", redactedErr, "backoff", backoff)
			if attempt < maxRetries-1 {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("context canceled: %w", ctx.Err())
				case <-time.After(backoff):
				}
				backoff *= 2
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return newIdleTimeoutBody(resp.Body, cancel, imageIdleReadTimeout), nil
		}
		_ = resp.Body.Close()
		cancel()

		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("image not found: %s", redactedURL)
		}
		// Retry on 5xx server errors.
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error %d for %s (attempt %d/%d)", resp.StatusCode, redactedURL, attempt+1, maxRetries)
			slog.Warn("HTTP server error, retrying", "attempt", attempt+1, "status", resp.StatusCode, "backoff", backoff)
			if attempt < maxRetries-1 {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("context canceled: %w", ctx.Err())
				case <-time.After(backoff):
				}
				backoff *= 2
			}
			continue
		}
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, redactedURL)
	}
	return nil, lastErr
}

// imageIdleReadTimeout is how long an image download may deliver no data
// before it is aborted. The total download time stays unlimited.
var imageIdleReadTimeout = 2 * time.Minute

// idleTimeoutBody cancels the request when one Read of the body blocks for
// longer than idle, so a server that stops sending without closing the
// connection cannot hang provisioning. The timer only runs while a Read is
// in progress, so a slow consumer (device write, decompression) does not
// count as a stall.
type idleTimeoutBody struct {
	io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	stalled atomic.Bool
}

func newIdleTimeoutBody(body io.ReadCloser, cancel context.CancelFunc, idle time.Duration) *idleTimeoutBody {
	b := &idleTimeoutBody{ReadCloser: body, idle: idle, cancel: cancel}
	b.timer = time.AfterFunc(idle, func() {
		b.stalled.Store(true)
		cancel()
	})
	b.timer.Stop()
	return b
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.ReadCloser.Read(p)
	b.timer.Stop()
	if err != nil && b.stalled.Load() {
		return n, fmt.Errorf("image download stalled: no data for %s", b.idle)
	}
	return n, err //nolint:wrapcheck // io.EOF must stay unwrapped
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel()
	return err //nolint:wrapcheck // plain body close error
}

// FetchOCILayerWithRetry retries OCI layer fetch with exponential backoff.
func FetchOCILayerWithRetry(ctx context.Context, ref string) (io.ReadCloser, error) {
	const maxRetries = 3
	backoff := retryBackoffBase
	redactedRef := RedactOCIRef(ref)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		rc, err := FetchOCILayer(ctx, ref)
		if err == nil {
			return rc, nil
		}
		redactedErr := wrapRedactedOCIRefError(err, ref)
		lastErr = fmt.Errorf("OCI pull %s (attempt %d/%d): %w", redactedRef, attempt+1, maxRetries, redactedErr)
		slog.Warn("OCI pull failed, retrying", "attempt", attempt+1, "ref", redactedRef, "error", redactedErr, "backoff", backoff)
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context canceled: %w", ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, lastErr
}

// FetchOCILayerByMediaTypeWithRetry retries OCI layer fetch by explicit media
// type with exponential backoff.
func FetchOCILayerByMediaTypeWithRetry(ctx context.Context, ref string, mediaTypes ...types.MediaType) (io.ReadCloser, error) {
	const maxRetries = 3
	backoff := retryBackoffBase
	redactedRef := RedactOCIRef(ref)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		rc, err := FetchOCILayerByMediaType(ctx, ref, mediaTypes...)
		if err == nil {
			return rc, nil
		}
		redactedErr := wrapRedactedOCIRefError(err, ref)
		lastErr = fmt.Errorf("OCI pull %s (attempt %d/%d): %w", redactedRef, attempt+1, maxRetries, redactedErr)
		slog.Warn("OCI pull failed, retrying", "attempt", attempt+1, "ref", redactedRef, "error", redactedErr, "backoff", backoff)
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context canceled: %w", ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, lastErr
}

func newHash(checksumType string) (hash.Hash, error) {
	switch strings.ToLower(strings.TrimSpace(checksumType)) {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported checksum type: %s", checksumType)
	}
}
