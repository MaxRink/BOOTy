package timesync

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/telekom/t-caas-go-library/pkg/redact"
)

// DateProbe reads the time from the HTTP Date header of a server, the CAPRF
// connectivity target. It sends a HEAD request without credentials.
//
// The TLS handshake checks the certificate like every other client. When
// the local clock is outside the validity period of the server certificate,
// the probe connects again with the certificate check at a time inside that
// period, and then checks the chain, the host name and the key usage again
// at the time of the Date header: the server must hold the key of a
// certificate that is valid at the time it claims. The time is then
// authenticated.
type DateProbe struct {
	// Now returns the local wall clock. Tests fake it to simulate skew.
	Now func() time.Time
	// Roots is the trust anchor set. Nil uses the system roots, like the
	// CAPRF and image clients.
	Roots *x509.CertPool
	// SkipTLSVerify (SKIP_TLS_VERIFY) skips the certificate check, like the
	// CAPRF and image clients. The time is then unauthenticated.
	SkipTLSVerify bool
	// Timeout bounds each request.
	Timeout time.Duration
}

// dateSource is the Sample.Source of the Date probe.
const dateSource = "caprf-date"

// errNoDate is returned when the response has no usable Date header.
var errNoDate = errors.New("response has no valid Date header")

// Probe returns the offset of the local clock to the Date header of rawURL.
func (p DateProbe) Probe(ctx context.Context, rawURL string) (Sample, error) {
	safe := redact.URL(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return Sample{}, fmt.Errorf("date probe %s: not an HTTP(S) URL", safe)
	}
	verify := u.Scheme == "https" && !p.SkipTLSVerify

	sample, date, state, err := p.head(ctx, rawURL, nil)
	if err != nil && verify && IsCertTimeError(err) {
		// The local clock is outside the certificate validity: check the
		// handshake at a time inside it, and the chain at the Date time
		// below.
		at := validityMiddle(err)
		sample, date, state, err = p.head(ctx, rawURL, func() time.Time { return at })
	}
	if err != nil {
		return Sample{}, fmt.Errorf("date probe %s: %w", safe, redact.Wrap(err, rawURL))
	}
	if verify {
		if err := verifyAt(state, u.Hostname(), date, p.Roots); err != nil {
			return Sample{}, fmt.Errorf("date probe %s: %w", safe, err)
		}
		sample.Authenticated = true
	}
	return sample, nil
}

// head sends one HEAD request. tlsTime, when set, is the time of the TLS
// certificate check instead of the local clock.
func (p DateProbe) head(ctx context.Context, rawURL string, tlsTime func() time.Time) (Sample, time.Time, *tls.ConnectionState, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if tlsTime == nil {
		tlsTime = p.Now // nil: the TLS check uses the local clock
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				RootCAs:            p.Roots,
				Time:               tlsTime,
				InsecureSkipVerify: p.SkipTLSVerify, //nolint:gosec // opt-in legacy CAPRF parity (curl --insecure)
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, http.NoBody)
	if err != nil {
		return Sample{}, time.Time{}, nil, fmt.Errorf("create request: %w", err)
	}
	t1 := p.now()()
	start := time.Now()
	resp, err := client.Do(req)
	rtt := time.Since(start)
	if err != nil {
		return Sample{}, time.Time{}, nil, err //nolint:wrapcheck // Probe adds the redacted URL
	}
	_ = resp.Body.Close()
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return Sample{}, time.Time{}, nil, errNoDate
	}
	// The header has a resolution of one second: take the middle of the
	// second, at the middle of the round trip.
	offset := date.Add(500 * time.Millisecond).Sub(t1.Add(rtt / 2))
	return Sample{Source: dateSource, Offset: offset, RTT: rtt, Resolution: 2 * time.Second}, date, resp.TLS, nil
}

func (p DateProbe) now() func() time.Time {
	if p.Now != nil {
		return p.Now
	}
	return time.Now
}

// validityMiddle returns the middle of the validity period of the
// certificate in a validity error.
func validityMiddle(err error) time.Time {
	var certErr x509.CertificateInvalidError
	if !errors.As(err, &certErr) || certErr.Cert == nil {
		return time.Time{}
	}
	c := certErr.Cert
	return c.NotBefore.Add(c.NotAfter.Sub(c.NotBefore) / 2)
}

// verifyAt checks the server chain like crypto/tls does for a client, at the
// time at instead of the local clock.
func verifyAt(cs *tls.ConnectionState, host string, at time.Time, roots *x509.CertPool) error {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return errors.New("no server certificate")
	}
	opts := x509.VerifyOptions{
		Roots:         roots,
		DNSName:       host,
		CurrentTime:   at,
		Intermediates: x509.NewCertPool(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, cert := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(cert)
	}
	if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
		return fmt.Errorf("server certificate is not valid at the Date header time %s: %w", at.UTC().Format(time.RFC3339), err)
	}
	return nil
}
