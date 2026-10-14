// Package auth implements JWT token management for CAPRF communication.
package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/telekom/t-caas-go-library/pkg/redact"
)

// TokenResponse represents the server's token endpoint response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`            //nolint:gosec // G101: struct field for token endpoint response, not a hardcoded credential
	RefreshToken string `json:"refresh_token,omitempty"` //nolint:gosec // G101: struct field for token endpoint response, not a hardcoded credential
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// maxTokenResponseBytes caps the token endpoint response body at 64 KiB
// to prevent OOM from a malicious or misconfigured server.
const maxTokenResponseBytes = 64 << 10

// tokenRequest is the JSON body sent to the token endpoint.
type tokenRequest struct {
	MachineSerial string `json:"machineSerial"`
	BMCMAC        string `json:"bmcMAC"`
	Algorithm     string `json:"algorithm,omitempty"`
}

// TokenManager handles JWT acquisition, renewal, and failure recovery.
type TokenManager struct {
	tokenURL         string
	redactedTokenURL string
	token            string
	refreshToken     string
	expiresAt        time.Time
	mu               sync.RWMutex
	client           *http.Client
	log              *slog.Logger
	onFatal          func(error)
	backoff          func(attempt int) time.Duration
	policy           RenewalPolicy
	lifetime         time.Duration // expires_in of the current token
	algorithm        string
	acquired         bool
	renewalStarted   bool // true once StartRenewal launches renewLoop
}

// NewTokenManager creates a token manager with an initial bootstrap token.
// The tokenURL must use HTTPS unless it targets localhost for testing.
func NewTokenManager(tokenURL, bootstrapToken string, log *slog.Logger) (*TokenManager, error) {
	if log == nil {
		log = slog.Default()
	}
	u, err := url.Parse(tokenURL)
	if err != nil {
		return nil, fmt.Errorf("parse token URL: %w", newRedactedTokenURLError(tokenURL, err))
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1")) {
		return nil, fmt.Errorf("token URL must use HTTPS (http allowed only for localhost), got %q", u.Scheme)
	}
	return &TokenManager{
		tokenURL:         tokenURL,
		redactedTokenURL: redactTokenURL(tokenURL),
		token:            bootstrapToken,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		log:     log.WithGroup("auth"),
		backoff: cappedBackoff(DefaultRenewBackoff),
		policy:  RenewalPolicy{Retries: DefaultRenewRetries},
	}, nil
}

// Defaults of RenewalPolicy.
const (
	DefaultRenewRetries   = 5
	DefaultRenewBackoff   = time.Second
	MaxRenewBackoff       = time.Minute
	defaultMarginFraction = 0.2
)

// ErrTokenExpired is the error when the token expires, or reaches the
// renewal margin, and BOOTy can not renew it.
var ErrTokenExpired = errors.New("token expires without a successful renewal")

// RenewalPolicy controls when and how often the token is renewed.
type RenewalPolicy struct {
	// Margin is the time before expiry at which the renewal starts. 0 means
	// 20% of the token lifetime.
	Margin time.Duration
	// Retries is the number of renewal attempts before the renewal fails
	// for good. 0 means DefaultRenewRetries.
	Retries int
	// Backoff is the wait after the first failed attempt; it doubles after
	// each further failure, up to MaxRenewBackoff. 0 means
	// DefaultRenewBackoff.
	Backoff time.Duration
}

// SetRenewalPolicy sets the renewal margin, attempts and backoff. Must be
// called before StartRenewal.
func (tm *TokenManager) SetRenewalPolicy(p RenewalPolicy) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if p.Retries <= 0 {
		p.Retries = DefaultRenewRetries
	}
	if p.Backoff <= 0 {
		p.Backoff = DefaultRenewBackoff
	}
	tm.policy = p
	tm.backoff = cappedBackoff(p.Backoff)
}

// cappedBackoff doubles base per attempt up to MaxRenewBackoff, without a
// shift that could overflow.
func cappedBackoff(base time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		d := base
		for i := 0; i < attempt && d < MaxRenewBackoff; i++ {
			d *= 2
		}
		return min(d, MaxRenewBackoff)
	}
}

// RenewMargin returns how long before expiry the renewal starts: the
// configured margin, or 20% of the lifetime. It is at most half the
// lifetime, so a renewed token is never renewed again at once.
func RenewMargin(margin, lifetime time.Duration) time.Duration {
	if margin <= 0 {
		margin = time.Duration(float64(lifetime) * defaultMarginFraction)
	}
	return min(margin, lifetime/2)
}

// SetAlgorithm configures the token algorithm (e.g. RS256, ES256) sent in requests.
// Must be called before Acquire or StartRenewal.
func (tm *TokenManager) SetAlgorithm(alg string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.algorithm = alg
}

// SetOnFatal sets the callback invoked when token renewal fails for good or
// the token expires; it gets the reason. Must be called before StartRenewal.
func (tm *TokenManager) SetOnFatal(fn func(error)) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.onFatal = fn
}

// Acquire exchanges the bootstrap token for a JWT from the token endpoint.
func (tm *TokenManager) Acquire(ctx context.Context, serial, bmcMAC string) error {
	tm.mu.RLock()
	alg := tm.algorithm
	tm.mu.RUnlock()
	reqBody := tokenRequest{
		MachineSerial: serial,
		BMCMAC:        bmcMAC,
		Algorithm:     alg,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.tokenURL,
		bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create token request for %s: %w", tm.redactedTokenURL, newRedactedTokenURLError(tm.tokenURL, err))
	}

	tm.mu.RLock()
	req.Header.Set("Authorization", "Bearer "+tm.token)
	tm.mu.RUnlock()
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.client.Do(req) //nolint:gosec // G107: token URL comes from validated configuration, not user input
	if err != nil {
		return fmt.Errorf("acquire token from %s: %w", tm.redactedTokenURL, newRedactedTokenURLError(tm.tokenURL, err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("acquire token from %s: status %d", tm.redactedTokenURL, resp.StatusCode)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseBytes)).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("acquire token from %s: empty access_token in response", tm.redactedTokenURL)
	}
	if tokenResp.ExpiresIn <= 0 {
		return fmt.Errorf("acquire token from %s: invalid expires_in %d", tm.redactedTokenURL, tokenResp.ExpiresIn)
	}

	tm.mu.Lock()
	tm.token = tokenResp.AccessToken
	tm.refreshToken = tokenResp.RefreshToken
	tm.lifetime = time.Duration(tokenResp.ExpiresIn) * time.Second
	tm.expiresAt = time.Now().Add(tm.lifetime)
	tm.acquired = true
	tm.mu.Unlock()

	tm.log.Info("jwt acquired", "expiresIn", tokenResp.ExpiresIn)

	return nil
}

// Token returns the current token for use in Authorization headers.
func (tm *TokenManager) Token() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	return tm.token
}

// StartRenewal begins the background renewal goroutine. It renews the
// RenewalPolicy margin before expiry (20% of the lifetime by default). Must
// be called after a successful Acquire.
// Idempotent: subsequent calls after the first are a no-op.
func (tm *TokenManager) StartRenewal(ctx context.Context) error {
	tm.mu.Lock()
	if !tm.acquired {
		tm.mu.Unlock()
		return fmt.Errorf("cannot start renewal: acquire has not been called")
	}
	if tm.renewalStarted {
		tm.mu.Unlock()
		return nil
	}
	tm.renewalStarted = true
	tm.mu.Unlock()
	go tm.renewLoop(ctx)
	return nil
}

func (tm *TokenManager) renewLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	// Drain the initial fire so we start with a proper sleep.
	<-timer.C

	for {
		tm.mu.RLock()
		remaining := time.Until(tm.expiresAt)
		margin := RenewMargin(tm.policy.Margin, tm.lifetime)
		tm.mu.RUnlock()

		// Renew the margin before expiry; at once when inside it.
		timer.Reset(max(remaining-margin, 0))

		select {
		case <-timer.C:
			if err := tm.renewWithRetry(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					tm.log.Debug("token renewal stopped", "reason", err)
					return
				}
				tm.log.Error("token renewal failed for good", "error", err)
				tm.mu.RLock()
				fatal := tm.onFatal
				tm.mu.RUnlock()
				if fatal != nil {
					fatal(err)
					return
				}
				tm.log.Warn("token renewal failed without fatal handler, continuing retry loop")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (tm *TokenManager) renew(ctx context.Context) error {
	tm.mu.RLock()
	refresh := tm.refreshToken
	tm.mu.RUnlock()

	type renewRequest struct {
		RefreshToken string `json:"refresh_token,omitempty"` //nolint:gosec // G101: struct field for token request body, not a hardcoded credential
	}
	data, err := json.Marshal(renewRequest{RefreshToken: refresh}) //nolint:gosec // G117: refresh token is intentionally serialized into token renewal request body
	if err != nil {
		return fmt.Errorf("marshal renewal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.tokenURL,
		bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create renewal request for %s: %w", tm.redactedTokenURL, newRedactedTokenURLError(tm.tokenURL, err))
	}

	tm.mu.RLock()
	req.Header.Set("Authorization", "Bearer "+tm.token)
	tm.mu.RUnlock()
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.client.Do(req) //nolint:gosec // G107: token URL comes from validated configuration, not user input
	if err != nil {
		return fmt.Errorf("renew token from %s: %w", tm.redactedTokenURL, newRedactedTokenURLError(tm.tokenURL, err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("renew token from %s: status %d", tm.redactedTokenURL, resp.StatusCode)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseBytes)).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decode renewal response from %s: %w", tm.redactedTokenURL, err)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("renew token from %s: empty access_token in response", tm.redactedTokenURL)
	}
	if tokenResp.ExpiresIn <= 0 {
		return fmt.Errorf("renew token from %s: invalid expires_in %d", tm.redactedTokenURL, tokenResp.ExpiresIn)
	}

	tm.mu.Lock()
	tm.token = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		tm.refreshToken = tokenResp.RefreshToken
	}
	tm.lifetime = time.Duration(tokenResp.ExpiresIn) * time.Second
	tm.expiresAt = time.Now().Add(tm.lifetime)
	tm.mu.Unlock()

	tm.log.Info("jwt renewed", "expiresIn", tokenResp.ExpiresIn)

	return nil
}

// renewWithRetry tries the renewal up to policy.Retries times with backoff.
// It stops with ErrTokenExpired once the token expired: an expired token
// can not authenticate a renewal.
func (tm *TokenManager) renewWithRetry(ctx context.Context) error {
	tm.mu.RLock()
	retries, expiresAt := tm.policy.Retries, tm.expiresAt
	tm.mu.RUnlock()
	var lastErr error

	for attempt := range retries {
		if !time.Now().Before(expiresAt) {
			return fmt.Errorf("%w: %w", ErrTokenExpired, errOrNone(lastErr))
		}
		err := tm.renew(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		tm.log.Warn("renewal attempt failed", "attempt", attempt+1, "of", retries, "error", err)
		if attempt == retries-1 {
			break
		}
		// Never sleep past the expiry.
		wait := min(tm.backoff(attempt), max(time.Until(expiresAt), 0))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return fmt.Errorf("renewal canceled: %w", ctx.Err())
		}
	}

	return fmt.Errorf("renewal failed after %d attempts: %w", retries, lastErr)
}

func errOrNone(err error) error {
	if err == nil {
		return errors.New("no renewal attempt")
	}
	return err
}

func redactTokenURL(rawURL string) string {
	safe := redact.URL(rawURL)
	if safe == "[redacted invalid URL]" {
		return "<invalid-url>"
	}
	return safe
}

func newRedactedTokenURLError(rawURL string, err error) error {
	return redactTokenError(rawURL, err)
}

type redactedWrappedError struct {
	msg string
	err error
}

func (e *redactedWrappedError) Error() string {
	return e.msg
}

func (e *redactedWrappedError) Unwrap() error {
	return e.err
}

type redactedWrappedNetError struct {
	redactedWrappedError
	timeout bool
}

func (e *redactedWrappedNetError) Timeout() bool {
	return e.timeout
}

func (e *redactedWrappedNetError) Temporary() bool {
	return false
}

type redactedJoinedError struct {
	msg  string
	errs []error
}

func (e *redactedJoinedError) Error() string {
	return e.msg
}

func (e *redactedJoinedError) Unwrap() []error {
	return e.errs
}

type redactedLeafError struct {
	msg string
}

func (e *redactedLeafError) Error() string {
	return e.msg
}

type redactedNetError struct {
	msg     string
	timeout bool
}

func (e *redactedNetError) Error() string {
	return e.msg
}

func (e *redactedNetError) Timeout() bool {
	return e.timeout
}

func (e *redactedNetError) Temporary() bool {
	return false
}

func redactTokenError(rawURL string, err error) error {
	if err == nil {
		return nil
	}
	if typed, ok := directURLError(err); ok {
		sanitized := *typed
		sanitized.URL = redact.Wrap(errors.New(redactTokenURL(typed.URL)), rawURL).Error()
		sanitized.Err = redactTokenError(rawURL, typed.Err)
		return &sanitized
	}

	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		errs := unwrapper.Unwrap()
		sanitized := make([]error, 0, len(errs))
		for _, child := range errs {
			if child != nil {
				sanitized = append(sanitized, redactTokenError(rawURL, child))
			}
		}
		return &redactedJoinedError{msg: redactTokenErrorString(rawURL, err.Error()), errs: sanitized}
	}
	if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
		wrapped := &redactedWrappedError{
			msg: redactTokenErrorString(rawURL, err.Error()),
			err: redactTokenError(rawURL, unwrapper.Unwrap()),
		}
		var netErr net.Error
		if errors.As(wrapped.err, &netErr) {
			return &redactedWrappedNetError{
				redactedWrappedError: *wrapped,
				timeout:              netErr.Timeout(),
			}
		}
		return wrapped
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return &redactedNetError{
			msg:     redactTokenErrorString(rawURL, err.Error()),
			timeout: netErr.Timeout(),
		}
	}
	return &redactedLeafError{msg: redactTokenErrorString(rawURL, err.Error())}
}

func directURLError(err error) (*url.Error, bool) {
	urlErr, ok := err.(*url.Error) //nolint:errorlint // direct match preserves outer wrappers for recursive sanitizing.
	return urlErr, ok
}

func redactTokenErrorString(rawURL, msg string) string {
	// Copy only the diagnostic: retaining Wrap's cause would expose unsanitized errors.
	return fmt.Sprintf("%s: %s", redactTokenURL(rawURL), redact.Wrap(errors.New(msg), rawURL))
}
