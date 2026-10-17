package timesync

import (
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// last is the result of the last Sync that measured the clock.
var last atomic.Pointer[Result]

func lastResult() *Result { return last.Load() }

// CodeCertTime is the failure code of a TLS certificate that is not valid
// at the local clock.
const CodeCertTime = "BOOTY-TIM-002"

// ErrClockSkew matches every *ClockSkewError with errors.Is, also after a
// transport wrapper that keeps only a fixed cause and this sentinel.
var ErrClockSkew = errors.New(CodeCertTime + ": TLS certificate not valid at the local clock")

// ClockSkewError replaces the x509 error of a certificate that is not valid
// at the local clock. It names the skew that would make it valid. Unwrap
// returns the x509 error.
type ClockSkewError struct {
	// Now is the local clock at the check.
	Now time.Time
	// NotBefore and NotAfter are the validity period of the certificate.
	NotBefore, NotAfter time.Time
	// SyncedFrom is the source of the last time sync, empty when the clock
	// was not synced.
	SyncedFrom string
	err        error
}

// Error names the skew and the likely cause.
func (e *ClockSkewError) Error() string {
	if e.Now.Before(e.NotBefore) {
		behind := e.NotBefore.Sub(e.Now).Round(time.Second)
		if e.SyncedFrom != "" {
			return fmt.Sprintf("%s: TLS certificate is not valid until %s, %s after the local clock %s that was synced from %s: the certificate starts in the future or the time source is wrong",
				CodeCertTime, utc(e.NotBefore), behind, utc(e.Now), e.SyncedFrom)
		}
		return fmt.Sprintf("%s: TLS certificate is not valid until %s: the local clock %s is at least %s behind; the clock was not synced, set NTP_SERVERS or check the RTC",
			CodeCertTime, utc(e.NotBefore), utc(e.Now), behind)
	}
	ahead := e.Now.Sub(e.NotAfter).Round(time.Second)
	if e.SyncedFrom != "" {
		return fmt.Sprintf("%s: TLS certificate expired at %s, %s before the local clock %s that was synced from %s",
			CodeCertTime, utc(e.NotAfter), ahead, utc(e.Now), e.SyncedFrom)
	}
	return fmt.Sprintf("%s: TLS certificate expired at %s: the certificate is expired or the local clock %s is at least %s ahead; the clock was not synced, set NTP_SERVERS or check the RTC",
		CodeCertTime, utc(e.NotAfter), utc(e.Now), ahead)
}

// Unwrap returns the x509 error.
func (e *ClockSkewError) Unwrap() error { return e.err }

// Is reports ErrClockSkew.
func (e *ClockSkewError) Is(target error) bool { return target == ErrClockSkew } //nolint:errorlint // sentinel identity

// Skew is the smallest clock change that would make the certificate valid:
// positive when the clock is behind, negative when it is ahead.
func (e *ClockSkewError) Skew() time.Duration {
	if e.Now.Before(e.NotBefore) {
		return e.NotBefore.Sub(e.Now)
	}
	return e.NotAfter.Sub(e.Now)
}

// ExplainCertTime returns a *ClockSkewError when err contains an x509
// validity period error, and err unchanged otherwise. now is the local clock
// at the check; the zero time uses time.Now.
func ExplainCertTime(err error, now time.Time) error {
	var certErr x509.CertificateInvalidError
	if !errors.As(err, &certErr) || certErr.Reason != x509.Expired || certErr.Cert == nil {
		return err
	}
	if now.IsZero() {
		now = time.Now()
	}
	skewErr := &ClockSkewError{
		Now:       now,
		NotBefore: certErr.Cert.NotBefore,
		NotAfter:  certErr.Cert.NotAfter,
		err:       err,
	}
	if res := lastResult(); res != nil {
		skewErr.SyncedFrom = res.Source
	}
	return skewErr
}

// IsCertTimeError reports whether err contains an x509 validity period
// error.
func IsCertTimeError(err error) bool {
	var certErr x509.CertificateInvalidError
	return errors.As(err, &certErr) && certErr.Reason == x509.Expired
}

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }
