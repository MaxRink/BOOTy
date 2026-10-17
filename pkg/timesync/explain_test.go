package timesync

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// handshakeAt connects to srv with the client clock at now and returns the
// request error.
func handshakeAt(t *testing.T, url string, roots *x509.CertPool, now time.Time) error {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    roots,
		Time:       func() time.Time { return now },
		MinVersion: tls.VersionTLS12,
	}}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestExplainCertTimeWithSkewedClock(t *testing.T) {
	now := time.Now()
	pki := newTestPKI(t, now.Add(-time.Hour), now.Add(30*24*time.Hour))
	srv := startTLSServer(t, pki)

	tests := []struct {
		name       string
		clock      time.Time
		syncedFrom string
		wantSkew   time.Duration
		want       []string
	}{
		{"clock years behind", time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), "", now.Add(-time.Hour).Sub(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)),
			[]string{CodeCertTime, "is not valid until", "is at least", "behind", "the clock was not synced", "NTP_SERVERS"}},
		{"clock ahead", now.Add(40 * 24 * time.Hour), "", -10 * 24 * time.Hour,
			[]string{CodeCertTime, "expired at", "is at least", "ahead"}},
		{"synced clock, expired certificate", now.Add(40 * 24 * time.Hour), "ntp://192.0.2.1:123", -10 * 24 * time.Hour,
			[]string{CodeCertTime, "expired at", "that was synced from ntp://192.0.2.1:123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			last.Store(nil)
			if tt.syncedFrom != "" {
				last.Store(&Result{Source: tt.syncedFrom})
			}
			t.Cleanup(func() { last.Store(nil) })

			err := handshakeAt(t, srv.URL, pki.roots, tt.clock)
			if !IsCertTimeError(err) {
				t.Fatalf("handshake error = %v, want a certificate validity error", err)
			}
			explained := ExplainCertTime(fmt.Errorf("post init: %w", err), tt.clock)
			var skewErr *ClockSkewError
			if !errors.As(explained, &skewErr) {
				t.Fatalf("ExplainCertTime = %T %v", explained, explained)
			}
			if diff := (skewErr.Skew() - tt.wantSkew).Abs(); diff > time.Second {
				t.Fatalf("Skew() = %v, want %v", skewErr.Skew(), tt.wantSkew)
			}
			for _, want := range tt.want {
				if !strings.Contains(explained.Error(), want) {
					t.Errorf("message %q does not contain %q", explained.Error(), want)
				}
			}
			var certErr x509.CertificateInvalidError
			if !errors.As(explained, &certErr) {
				t.Error("explained error does not unwrap to the x509 error")
			}
		})
	}
}

func TestExplainCertTimeKeepsOtherErrors(t *testing.T) {
	now := time.Now()
	pki := newTestPKI(t, now.Add(-time.Hour), now.Add(time.Hour))
	srv := startTLSServer(t, pki)
	other := newTestPKI(t, now.Add(-time.Hour), now.Add(time.Hour))
	err := handshakeAt(t, srv.URL, other.roots, now)
	if err == nil || IsCertTimeError(err) {
		t.Fatalf("handshake error = %v, want an unknown authority error", err)
	}
	if got := ExplainCertTime(err, now); got != err { //nolint:errorlint // identity is the contract
		t.Fatalf("ExplainCertTime changed %v to %v", err, got)
	}
	// Another x509 validity reason keeps its error too.
	otherReason := fmt.Errorf("post: %w", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.NotAuthorizedToSign})
	if got := ExplainCertTime(otherReason, now); got != otherReason || IsCertTimeError(otherReason) { //nolint:errorlint // identity is the contract
		t.Fatalf("ExplainCertTime changed a NotAuthorizedToSign error to %v", got)
	}
	if ExplainCertTime(nil, now) != nil {
		t.Fatal("ExplainCertTime(nil) != nil")
	}
	// A valid clock gives no error at all.
	if err := handshakeAt(t, srv.URL, pki.roots, now); err != nil {
		t.Fatalf("handshake with a correct clock: %v", err)
	}
}
