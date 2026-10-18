//go:build linux

package image

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// ErrFormatProbe wraps failures to inspect an image source before streaming.
var ErrFormatProbe = errors.New("image format probe failed")

// ProbeSourceFormat opens an image source and peeks at its magic bytes without
// consuming the full image.
func ProbeSourceFormat(ctx context.Context, source string) (Format, error) {
	_, cleanup, format, err := openAndDecompress(ctx, source, nil)
	if err != nil {
		return FormatRaw, err
	}
	defer cleanup()
	return format, nil
}

// LooksQCOW2 reports whether the image URL names a qcow2 file (possibly
// compressed), from its path alone.
func LooksQCOW2(source string) bool { return sourceLooksQCOW2(source) }

func sourceLooksQCOW2(source string) bool {
	parsed, err := url.Parse(source)
	name := source
	if err == nil && parsed.Path != "" {
		name = parsed.Path
	}
	name = strings.ToLower(name)
	for _, suffix := range []string{".gz", ".zst", ".xz", ".bz2", ".lz4"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return strings.HasSuffix(name, ".qcow2")
}

// IsTransientFormatProbeError reports whether a pre-stream format probe failed
// due to an image source that may become reachable later in provisioning.
func IsTransientFormatProbeError(err error) bool {
	if !errors.Is(err, ErrFormatProbe) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, token := range []string{
		"timeout awaiting response headers",
		"i/o timeout",
		"connection refused",
		"connection reset by peer",
		"network is unreachable",
		"no route to host",
		"server misbehaving",
		"temporary failure",
	} {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}
