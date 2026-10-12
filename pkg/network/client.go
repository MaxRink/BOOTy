package network

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

var skipTLSVerify atomic.Bool

// SetSkipTLSVerify controls TLS certificate verification for clients that
// NewHTTPClient creates afterwards. Only CAPRF legacy compatibility enables
// it, to match `curl --insecure` in the legacy provisioner.
func SetSkipTLSVerify(skip bool) {
	skipTLSVerify.Store(skip)
}

// NewHTTPClient returns a pre-configured HTTP client suitable for downloads
// or general API usage. It sets connect and TLS timeouts to prevent hanging
// on broken connections while leaving the response body read timeout unlimited
// unless a specific client-level timeout is provided.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: skipTLSVerify.Load(), //nolint:gosec // opt-in legacy CAPRF parity (curl --insecure)
			},
			DialContext: (&net.Dialer{
				Timeout: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}
