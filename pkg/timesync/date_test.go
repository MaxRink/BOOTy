package timesync

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPKI is a CA and a server certificate for 127.0.0.1.
type testPKI struct {
	roots *x509.CertPool
	cert  tls.Certificate
}

func newTestPKI(t *testing.T, notBefore, notAfter time.Time) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "timesync test CA"},
		NotBefore:             notBefore.Add(-time.Hour),
		NotAfter:              notAfter.Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "caprf"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return testPKI{roots: roots, cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}}
}

func startTLSServer(t *testing.T, pki testPKI) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("date probe sent credentials")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestDateProbeAuthenticatesSkewedClock(t *testing.T) {
	now := time.Now()
	pki := newTestPKI(t, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	srv := startTLSServer(t, pki)
	const skew = 20 * 365 * 24 * time.Hour
	p := DateProbe{Now: func() time.Time { return time.Now().Add(-skew) }, Roots: pki.roots}
	sample, err := p.Probe(context.Background(), srv.URL+"/status/init")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !sample.Authenticated {
		t.Fatal("sample is not authenticated")
	}
	if diff := (sample.Offset - skew).Abs(); diff > 2*time.Second {
		t.Fatalf("offset = %v, want %v", sample.Offset, skew)
	}
}

func TestDateProbeRejectsCertificateInvalidAtDate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		pki  testPKI
		want string
	}{
		{"expired", newTestPKI(t, now.Add(-48*time.Hour), now.Add(-24*time.Hour)), "not valid at the Date header time"},
		{"unknown CA", newTestPKI(t, now.Add(-time.Hour), now.Add(time.Hour)), "unknown authority"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startTLSServer(t, tt.pki)
			roots := tt.pki.roots
			if tt.name == "unknown CA" {
				roots = newTestPKI(t, now, now.Add(time.Hour)).roots
			}
			_, err := DateProbe{Roots: roots}.Probe(context.Background(), srv.URL)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Probe error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDateProbeUnauthenticated(t *testing.T) {
	now := time.Now()
	pki := newTestPKI(t, now.Add(-time.Hour), now.Add(time.Hour))
	tlsSrv := startTLSServer(t, pki)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(plain.Close)

	for name, p := range map[string]struct {
		probe DateProbe
		url   string
	}{
		"skip verify": {DateProbe{SkipTLSVerify: true}, tlsSrv.URL},
		"plain http":  {DateProbe{}, plain.URL},
	} {
		t.Run(name, func(t *testing.T) {
			sample, err := p.probe.Probe(context.Background(), p.url)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if sample.Authenticated || sample.Offset.Abs() > 2*time.Second {
				t.Fatalf("sample = %+v, want unauthenticated with a small offset", sample)
			}
		})
	}
}

func TestDateProbeErrors(t *testing.T) {
	noDate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Date"] = nil // suppress the automatic header
	}))
	t.Cleanup(noDate.Close)
	if _, err := (DateProbe{}).Probe(context.Background(), noDate.URL); err == nil || !strings.Contains(err.Error(), errNoDate.Error()) {
		t.Fatalf("missing Date: err = %v", err)
	}
	if _, err := (DateProbe{}).Probe(context.Background(), "ftp://user:secret@host/x"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("ftp URL: err = %v, want a redacted error", err)
	}
}

func TestVerifyAtWithoutCertificate(t *testing.T) {
	if err := verifyAt(nil, "127.0.0.1", time.Now(), nil); err == nil {
		t.Fatal("verifyAt(nil) succeeded")
	}
}

// The chain is valid at the local clock, but the server claims a time after
// the certificate expires: the Date time is not authenticated.
func TestDateProbeRejectsDateOutsideValidity(t *testing.T) {
	now := time.Now()
	pki := newTestPKI(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", now.Add(2*365*24*time.Hour).UTC().Format(http.TimeFormat))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, err := DateProbe{Roots: pki.roots}.Probe(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "not valid at the Date header time") {
		t.Fatalf("Probe error = %v, want a Date header validity error", err)
	}
}

// The probe does not follow redirects: the Date of the first response
// counts, and the redirect target gets no request.
func TestDateProbeDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Date", time.Now().Add(-48*time.Hour).UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	t.Cleanup(redirect.Close)
	sample, err := DateProbe{}.Probe(context.Background(), redirect.URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if targetHits.Load() != 0 || sample.Offset.Abs() > 2*time.Second {
		t.Fatalf("redirect followed: target hits %d, offset %v", targetHits.Load(), sample.Offset)
	}
}
