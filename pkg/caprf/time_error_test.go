package caprf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/timesync"
)

// A certificate that is not valid at the local clock gets an error that
// names the skew instead of the generic "tls error".
func TestRedactedURLErrorNamesClockSkew(t *testing.T) {
	now := time.Now()
	cert := &x509.Certificate{NotBefore: now.Add(24 * time.Hour), NotAfter: now.Add(48 * time.Hour)}
	raw := "https://caprf.example/status/init?token=secret"
	err := redactedURLError(raw, &neturl.Error{Op: "Post", URL: raw, Err: &tls.CertificateVerificationError{
		Err: x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired},
	}})
	msg := err.Error()
	for _, want := range []string{timesync.CodeCertTime, "is not valid until", "behind"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("error %q leaks the query", msg)
	}
	var cause *transportCause
	if !errors.As(err, &cause) {
		t.Fatalf("error %T does not wrap a transportCause", err)
	}
}

func TestRedactedURLErrorKeepsClockSkewSentinel(t *testing.T) {
	now := time.Now()
	cert := &x509.Certificate{NotBefore: now.Add(time.Hour), NotAfter: now.Add(2 * time.Hour)}
	err := redactedURLError("https://caprf.example/x", &tls.CertificateVerificationError{
		Err: x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired},
	})
	if !errors.Is(err, timesync.ErrClockSkew) {
		t.Fatalf("errors.Is(%v, ErrClockSkew) = false", err)
	}
}
