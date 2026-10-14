package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustNewTokenManager(t *testing.T, url, token string, log *slog.Logger) *TokenManager {
	t.Helper()
	tm, err := NewTokenManager(url, token, log)
	if err != nil {
		t.Fatalf("NewTokenManager(%q): %v", url, err)
	}
	return tm
}

func TestTokenManager_Acquire(t *testing.T) {
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)

		if r.Header.Get("Authorization") != "Bearer bootstrap-token" {
			t.Errorf("expected bootstrap token, got %q", r.Header.Get("Authorization"))
		}

		resp := TokenResponse{
			AccessToken:  "jwt-token-abc",
			RefreshToken: "refresh-xyz",
			ExpiresIn:    3600,
			TokenType:    "Bearer",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "bootstrap-token", slog.Default())
	if err := tm.Acquire(context.Background(), "SN123", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	if got := tm.Token(); got != "jwt-token-abc" {
		t.Errorf("expected token 'jwt-token-abc', got %q", got)
	}

	if callCount.Load() != 1 {
		t.Errorf("expected 1 call, got %d", callCount.Load())
	}
}

func TestTokenManager_Acquire_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "bad-token", slog.Default())
	if err := tm.Acquire(context.Background(), "SN123", "aa:bb:cc:dd:ee:ff"); err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestTokenManager_Acquire_RedactsSensitiveTokenURLParts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	tokenURL := tokenURLWithSensitiveParts(t, server.URL)
	tm := mustNewTokenManager(t, tokenURL, "bad-token", slog.Default())

	err := tm.Acquire(context.Background(), "SN123", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	assertNoSensitiveTokenURLParts(t, err.Error())
	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error = %q, want redacted URL context %q", err.Error(), server.URL)
	}
}

func TestTokenManager_Acquire_RedactsSensitiveTokenURLPartsFromTransportError(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rawURL := "http://" + listener.Addr().String() + "/token"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	tokenURL := tokenURLWithSensitiveParts(t, rawURL)
	tm := mustNewTokenManager(t, tokenURL, "bootstrap-token", slog.Default())

	err = tm.Acquire(context.Background(), "SN123", "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("expected transport error")
	}
	assertNoSensitiveTokenURLParts(t, err.Error())
	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !strings.Contains(err.Error(), rawURL) {
		t.Fatalf("error = %q, want redacted URL context %q", err.Error(), rawURL)
	}
}

func TestTokenManager_Token_ThreadSafe(t *testing.T) {
	tm := mustNewTokenManager(t, "http://127.0.0.1", "initial", slog.Default())

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = tm.Token()
			}
		}()
	}
	// concurrent writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 100 {
			tm.mu.Lock()
			tm.token = fmt.Sprintf("token-%d", i)
			tm.mu.Unlock()
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Token() access timed out")
	}
}

func TestTokenManager_Renew(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		resp := TokenResponse{
			AccessToken:  "renewed-token",
			RefreshToken: "new-refresh",
			ExpiresIn:    7200,
			TokenType:    "Bearer",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "old-token", slog.Default())
	tm.refreshToken = "old-refresh"
	tm.expiresAt = time.Now().Add(time.Hour)

	if err := tm.renew(context.Background()); err != nil {
		t.Fatalf("renew failed: %v", err)
	}

	if got := tm.Token(); got != "renewed-token" {
		t.Errorf("expected 'renewed-token', got %q", got)
	}
}

func TestTokenManager_Renew_RedactsSensitiveTokenURLPartsFromTransportError(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rawURL := "http://" + listener.Addr().String() + "/token"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	tokenURL := tokenURLWithSensitiveParts(t, rawURL)
	tm := mustNewTokenManager(t, tokenURL, "token", slog.Default())
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Hour)

	err = tm.renew(context.Background())
	if err == nil {
		t.Fatal("expected transport error")
	}
	assertNoSensitiveTokenURLParts(t, err.Error())
	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !strings.Contains(err.Error(), rawURL) {
		t.Fatalf("error = %q, want redacted URL context %q", err.Error(), rawURL)
	}
}

func TestTokenManager_RenewWithRetry_EventualSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		resp := TokenResponse{
			AccessToken: "retry-token",
			ExpiresIn:   3600,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "token", slog.Default())
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Hour)
	tm.backoff = func(_ int) time.Duration { return time.Millisecond }

	if err := tm.renewWithRetry(context.Background()); err != nil {
		t.Fatalf("renewWithRetry should succeed after retries: %v", err)
	}

	if got := tm.Token(); got != "retry-token" {
		t.Errorf("expected 'retry-token', got %q", got)
	}
}

func TestTokenManager_RenewWithRetry_RedactsSensitiveTokenURLPartsFromTransportError(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rawURL := "http://" + listener.Addr().String() + "/token"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	tokenURL := tokenURLWithSensitiveParts(t, rawURL)
	tm := mustNewTokenManager(t, tokenURL, "token", slog.Default())
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Hour)
	tm.backoff = func(_ int) time.Duration { return 0 }

	err = tm.renewWithRetry(context.Background())
	if err == nil {
		t.Fatal("expected retry exhaustion error")
	}
	assertNoSensitiveTokenURLParts(t, err.Error())
	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !strings.Contains(err.Error(), rawURL) {
		t.Fatalf("error = %q, want redacted URL context %q", err.Error(), rawURL)
	}
}

func TestTokenManager_RenewWithRetry_Exhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "token", slog.Default())
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Hour)
	tm.backoff = func(_ int) time.Duration { return time.Millisecond }

	if err := tm.renewWithRetry(context.Background()); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

func TestTokenManager_RenewWithRetry_ContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tm := mustNewTokenManager(t, server.URL, "token", slog.Default())
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Hour)

	if err := tm.renewWithRetry(ctx); err == nil {
		t.Fatal("expected error when context is canceled")
	}
}

func TestTokenManager_StartRenewal_NotAcquired(t *testing.T) {
	tm := mustNewTokenManager(t, "http://127.0.0.1", "token", slog.Default())
	if err := tm.StartRenewal(context.Background()); err == nil {
		t.Fatal("expected error starting renewal without Acquire")
	}
}

func TestTokenManager_StartRenewal_RenewsBeforeExpiry(t *testing.T) {
	renewed := make(chan struct{})
	var renewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := TokenResponse{
			AccessToken: "renewed",
			ExpiresIn:   60,
		}
		_ = json.NewEncoder(w).Encode(resp)
		// Signal AFTER encoding the response so the client has data to read.
		if renewCalls.Add(1) == 1 {
			close(renewed)
		}
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "bootstrap", slog.Default())
	// A token inside the renewal margin makes renewLoop renew at once.
	tm.mu.Lock()
	tm.token = "initial-jwt"
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Minute)
	tm.lifetime = time.Hour // 1 min left is inside the 12 min margin: renew at once
	tm.acquired = true
	tm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := tm.StartRenewal(ctx); err != nil {
		t.Fatalf("StartRenewal failed: %v", err)
	}

	// Wait for the server to finish sending the renewal response.
	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for renewal")
	}

	// Poll briefly: the goroutine needs to read and apply the HTTP response.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case <-ticker.C:
			if tm.Token() == "renewed" {
				return
			}
		case <-timeout:
			t.Fatalf("expected token 'renewed', got %q", tm.Token())
		}
	}
}

func TestTokenManager_StartRenewal_OnFatalCalledOnExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "bootstrap", slog.Default())
	tm.mu.Lock()
	tm.token = "initial-jwt"
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Minute)
	tm.lifetime = time.Hour // 1 min left is inside the 12 min margin: renew at once
	tm.acquired = true
	tm.mu.Unlock()
	tm.backoff = func(_ int) time.Duration { return time.Millisecond }

	var fatalCalls atomic.Int32
	fatalCalled := make(chan struct{})
	tm.SetOnFatal(func(error) {
		if fatalCalls.Add(1) == 1 {
			close(fatalCalled)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := tm.StartRenewal(ctx); err != nil {
		t.Fatalf("StartRenewal failed: %v", err)
	}

	select {
	case <-fatalCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fatal callback")
	}
}

func TestTokenManager_StartRenewal_ContextCancelDoesNotCallOnFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requestStarted := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() {
			close(requestStarted)
			cancel()
		})
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tm := mustNewTokenManager(t, server.URL, "bootstrap", slog.Default())
	tm.mu.Lock()
	tm.token = "initial-jwt"
	tm.refreshToken = "refresh"
	tm.expiresAt = time.Now().Add(time.Minute)
	tm.lifetime = time.Hour // 1 min left is inside the 12 min margin: renew at once
	tm.acquired = true
	tm.mu.Unlock()
	tm.backoff = func(_ int) time.Duration { return time.Hour }

	fatalCalled := make(chan struct{}, 1)
	tm.SetOnFatal(func(error) {
		select {
		case fatalCalled <- struct{}{}:
		default:
		}
	})

	if err := tm.StartRenewal(ctx); err != nil {
		t.Fatalf("StartRenewal failed: %v", err)
	}

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for renewal request")
	}

	select {
	case <-fatalCalled:
		t.Fatal("onFatal should not be called on context cancellation")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTokenManager_SetAlgorithm_ThreadSafe(t *testing.T) {
	tm := mustNewTokenManager(t, "http://127.0.0.1", "token", slog.Default())
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.SetAlgorithm("RS256")
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent SetAlgorithm timed out")
	}
}

func TestNewTokenManager_RejectsPlainHTTP(t *testing.T) {
	_, err := NewTokenManager("http://example.com/token", "token", nil)
	if err == nil {
		t.Fatal("expected error for non-localhost HTTP URL")
	}
}

func TestNewTokenManager_AllowsLocalhostHTTP(t *testing.T) {
	tm, err := NewTokenManager("http://localhost/token", "token", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tm == nil {
		t.Fatal("expected non-nil TokenManager")
	}
}

func TestNewTokenManager_AllowsHTTPS(t *testing.T) {
	tm, err := NewTokenManager("https://api.example.com/token", "token", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tm == nil {
		t.Fatal("expected non-nil TokenManager")
	}
}

func TestNewTokenManager_RedactsSensitiveInvalidTokenURLParseError(t *testing.T) {
	_, err := NewTokenManager("https://leaky-user:super-secret@example.com/%zz?token=abc#frag", "token", nil)
	if err == nil {
		t.Fatal("expected parse error")
	}

	assertErrorChainNoSensitiveTokenURLParts(t, err)
}

func TestRedactedTokenURLErrorDoesNotRetainSensitiveURLParts(t *testing.T) {
	rawURL := "https://leaky-user:super-secret@example.com/token?token=abc#frag"
	err := newRedactedTokenURLError(rawURL, fmt.Errorf("Get %q: %w", rawURL, context.Canceled))
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected redacted token URL error to match context.Canceled")
	}
}

func TestRedactedTokenURLErrorPreservesSanitizedURLCause(t *testing.T) {
	rawURL := "https://leaky-user:super-secret@example.com/token?token=abc#frag"
	cause := &tokenURLTestNetError{msg: "temporary transport failure", timeout: true}
	err := newRedactedTokenURLError(rawURL, &url.Error{
		Op:  "Post",
		URL: rawURL,
		Err: fmt.Errorf("dial %s: %w", rawURL, cause),
	})
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	assertErrorChainNoSensitiveTokenURLParts(t, err)

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatal("expected redacted error to preserve *url.Error matching")
	}
	if urlErr.URL != "https://example.com/token" {
		t.Fatalf("url.Error.URL = %q, want redacted URL", urlErr.URL)
	}
	assertNoSensitiveTokenURLParts(t, fmt.Sprintf("%#v", urlErr))

	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatal("expected redacted error to preserve net.Error matching")
	}
	if !netErr.Timeout() {
		t.Fatal("expected preserved net.Error timeout status")
	}
}

type tokenURLTestNetError struct {
	msg     string
	timeout bool
}

func (e *tokenURLTestNetError) Error() string {
	return e.msg
}

func (e *tokenURLTestNetError) Timeout() bool {
	return e.timeout
}

func (e *tokenURLTestNetError) Temporary() bool {
	return false
}

func tokenURLWithSensitiveParts(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword("leaky-user", "super-secret")
	parsed.RawQuery = "token=abc"
	parsed.Fragment = "frag"
	return parsed.String()
}

func sensitiveTokenURLParts() []string {
	return []string{"leaky-user", "super-secret", "token=abc", "frag"}
}

func assertNoSensitiveTokenURLParts(t *testing.T, msg string) {
	t.Helper()
	for _, leaked := range sensitiveTokenURLParts() {
		if strings.Contains(msg, leaked) {
			t.Fatalf("error leaked sensitive token URL part %q: %q", leaked, msg)
		}
	}
}

func assertErrorChainNoSensitiveTokenURLParts(t *testing.T, err error) {
	t.Helper()
	assertErrorNoSensitiveTokenURLParts(t, err, map[error]struct{}{})
}

func assertErrorNoSensitiveTokenURLParts(t *testing.T, current error, seen map[error]struct{}) {
	t.Helper()
	if current == nil {
		return
	}
	if _, ok := seen[current]; ok {
		return
	}
	seen[current] = struct{}{}

	for current != nil {
		assertNoSensitiveTokenURLParts(t, current.Error())
		formatted := fmt.Sprintf("%#v", current)
		for _, leaked := range sensitiveTokenURLParts() {
			if strings.Contains(formatted, leaked) {
				t.Fatalf("error wrapper retained sensitive token URL part %q: %#v", leaked, current)
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				assertErrorNoSensitiveTokenURLParts(t, child, seen)
			}
			return
		}
		current = errors.Unwrap(current)
	}
}

func TestRedactedTokenURLErrorRedactsStandaloneSensitiveURLParts(t *testing.T) {
	rawURL := "https://leaky-user:super-secret@example.com/token?token=abc#frag"
	err := newRedactedTokenURLError(rawURL, fmt.Errorf(
		"token request failed for user leaky-user with password super-secret, query token=abc, fragment frag",
	))
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	assertErrorChainNoSensitiveTokenURLParts(t, err)
}

func TestRedactedTokenURLErrorRedactsJoinedErrors(t *testing.T) {
	rawURL := "https://leaky-user:super-secret@example.com/token?token=abc#frag"
	err := newRedactedTokenURLError(rawURL, errors.Join(
		fmt.Errorf("token request failed for %s", rawURL),
		fmt.Errorf("transport detail leaked password super-secret"),
		context.Canceled,
	))
	if err == nil {
		t.Fatal("expected wrapped error")
	}

	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected redacted joined error to match context.Canceled")
	}
}

func TestRedactedTokenURLErrorSanitizesURLFieldComponents(t *testing.T) {
	rawURL := "https://leaky-user:super-secret@example.com/token?token=abc#frag"
	err := newRedactedTokenURLError(rawURL, &url.Error{
		Op: "Post", URL: "super-secret", Err: context.DeadlineExceeded,
	})
	assertErrorChainNoSensitiveTokenURLParts(t, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expected sanitized URL error to preserve the deadline cause")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatal("expected sanitized URL error to preserve timeout classification")
	}
}
