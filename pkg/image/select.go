package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"
)

// SelectBestSource probes multiple image URLs with ranged GET requests and returns
// the one with the lowest response time. If only one URL is provided, it is
// returned directly. OCI references (oci://) are returned as-is without probing.
func SelectBestSource(ctx context.Context, urls []string) (string, error) {
	if len(urls) == 0 {
		return "", fmt.Errorf("no image URLs provided")
	}
	if len(urls) == 1 {
		return urls[0], nil
	}

	// OCI references are not probeable via HEAD.
	ociURLs := make([]string, 0, len(urls))
	httpURLs := make([]string, 0, len(urls))
	for _, u := range urls {
		if IsOCIReference(u) {
			ociURLs = append(ociURLs, u)
		} else {
			httpURLs = append(httpURLs, u)
		}
	}

	// If all URLs are OCI, return the first one.
	if len(httpURLs) == 0 {
		return ociURLs[0], nil
	}

	var bestURL string
	var bestTime = time.Duration(math.MaxInt64)

	slog.Info("selecting best image source", "candidates", len(httpURLs))
	for _, rawURL := range httpURLs {
		elapsed, err := probeURL(ctx, rawURL)
		if err != nil {
			slog.Warn("image source probe failed", "url", redactHost(rawURL), "error", err)
			continue
		}
		slog.Info("image source probe result", "url", redactHost(rawURL), "response_time", elapsed)
		if elapsed < bestTime {
			bestTime = elapsed
			bestURL = rawURL
		}
	}

	if bestURL != "" {
		slog.Info("selected best image source", "url", redactHost(bestURL), "response_time", bestTime)
		return bestURL, nil
	}

	// All HTTP probes failed; try OCI URLs first, then fall back to first HTTP URL.
	if len(ociURLs) > 0 {
		return ociURLs[0], nil
	}
	return httpURLs[0], nil
}

// probeURL measures connectivity latency with a GET for the first byte,
// like the `curl --range 0-0` probe of the CAPRF legacy provisioner.
// Presigned URLs are often bound to GET and reject HEAD.
func probeURL(ctx context.Context, rawURL string) (time.Duration, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("creating probe request: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")

	start := time.Now()
	resp, err := gpgHTTPClient.Do(req) //nolint:gosec // URL from trusted config
	if err != nil {
		return 0, fmt.Errorf("probe request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The download uses the same GET, so any error status rules the source
	// out, like `curl --fail` in the legacy probe.
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("probe returned status %d", resp.StatusCode)
	}
	// Measure until the first image byte arrives, like curl's time_total
	// for --range 0-0, so a source that sends headers and then stalls loses.
	// An empty body ends with io.EOF, which curl accepts as well.
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("probe read: %w", err)
	}
	return time.Since(start), nil
}

// redactHost extracts only the hostname from a URL for safe logging.
func redactHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid-url>"
	}
	return u.Hostname()
}
