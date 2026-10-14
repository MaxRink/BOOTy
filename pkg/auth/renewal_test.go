package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	initialSecret = "secret-initial-token"
	renewedSecret = "secret-renewed-token"
)

// renewalServer answers renewals: the first failures calls fail with 503,
// then it issues renewedSecret tokens for an hour.
func renewalServer(t *testing.T, failures int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: renewedSecret, ExpiresIn: 3600})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// syncBuffer is a log sink for the renewal goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startRenewal starts the renewal of a token with the given lifetime that
// expires in remaining. It returns the fatal reasons and the log.
func startRenewal(t *testing.T, url string, lifetime, remaining time.Duration, p RenewalPolicy) (*TokenManager, chan error, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	tm := mustNewTokenManager(t, url, "bootstrap", slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	tm.SetRenewalPolicy(p)
	tm.mu.Lock()
	tm.token, tm.acquired = initialSecret, true
	tm.lifetime = lifetime
	tm.expiresAt = time.Now().Add(remaining)
	tm.mu.Unlock()
	fatal := make(chan error, 1)
	tm.SetOnFatal(func(err error) { fatal <- err })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := tm.StartRenewal(ctx); err != nil {
		t.Fatal(err)
	}
	return tm, fatal, logs
}

// requireNoSecret fails when a token value shows up in logs or errors.
func requireNoSecret(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, secret := range []string{initialSecret, renewedSecret} {
			if strings.Contains(s, secret) {
				t.Fatalf("token %q leaked: %s", secret, s)
			}
		}
	}
}

func waitToken(t *testing.T, tm *TokenManager, want string, fatal chan error) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for tm.Token() != want {
		select {
		case err := <-fatal:
			t.Fatalf("renewal failed: %v", err)
		case <-deadline:
			t.Fatalf("token = %q, want %q", tm.Token(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// The renewal starts the margin before expiry, replaces the token once and
// does not renew the new token at once.
func TestRenewalRenewsAtTheMargin(t *testing.T) {
	srv, calls := renewalServer(t, 0)
	tm, fatal, logs := startRenewal(t, srv.URL, 2*time.Second, 2*time.Second, RenewalPolicy{Margin: 1900 * time.Millisecond})
	waitToken(t, tm, renewedSecret, fatal)
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("renewal calls = %d, want 1", calls.Load())
	}
	requireNoSecret(t, logs.String())
}

// A margin as long as the lifetime is capped at half of it: no hot loop.
func TestRenewalMarginLongerThanTheLifetimeDoesNotSpin(t *testing.T) {
	srv, calls := renewalServer(t, 0)
	tm, fatal, _ := startRenewal(t, srv.URL, time.Minute, time.Second, RenewalPolicy{Margin: time.Hour})
	waitToken(t, tm, renewedSecret, fatal)
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("renewal calls = %d, want 1", calls.Load())
	}
}

// Transient failures are retried with backoff until a renewal succeeds.
func TestRenewalRetriesTransientFailures(t *testing.T) {
	srv, calls := renewalServer(t, 2)
	tm, fatal, logs := startRenewal(t, srv.URL, time.Minute, time.Second, RenewalPolicy{Retries: 3, Backoff: 10 * time.Millisecond})
	waitToken(t, tm, renewedSecret, fatal)
	if calls.Load() != 3 {
		t.Fatalf("renewal calls = %d, want 2 failures and 1 success", calls.Load())
	}
	requireNoSecret(t, logs.String())
}

// When every attempt fails, the fatal handler gets the reason once.
func TestRenewalFailsForGoodAfterTheRetries(t *testing.T) {
	srv, calls := renewalServer(t, 1000)
	_, fatal, logs := startRenewal(t, srv.URL, time.Minute, 5*time.Second, RenewalPolicy{Retries: 3, Backoff: 10 * time.Millisecond})
	select {
	case err := <-fatal:
		if errors.Is(err, ErrTokenExpired) || calls.Load() != 3 {
			t.Fatalf("fatal = %v after %d calls, want the retries exhausted after 3", err, calls.Load())
		}
		requireNoSecret(t, err.Error(), logs.String())
	case <-time.After(5 * time.Second):
		t.Fatal("no fatal call")
	}
}

// The attempts stop when the token expires, also with retries left: the
// backoff never sleeps past the expiry.
func TestRenewalStopsAtExpiry(t *testing.T) {
	srv, _ := renewalServer(t, 1000)
	start := time.Now()
	_, fatal, logs := startRenewal(t, srv.URL, time.Hour, 300*time.Millisecond, RenewalPolicy{Retries: 100, Backoff: time.Hour})
	select {
	case err := <-fatal:
		if !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("fatal = %v, want ErrTokenExpired", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("fatal after %v, want it at the expiry", d)
		}
		requireNoSecret(t, err.Error(), logs.String())
	case <-time.After(5 * time.Second):
		t.Fatal("no fatal call")
	}
}

func TestRenewMargin(t *testing.T) {
	for _, tt := range []struct{ margin, lifetime, want time.Duration }{
		{0, 30 * time.Minute, 6 * time.Minute},
		{time.Minute, 30 * time.Minute, time.Minute},
		{time.Hour, 30 * time.Minute, 15 * time.Minute},
	} {
		if got := RenewMargin(tt.margin, tt.lifetime); got != tt.want {
			t.Errorf("RenewMargin(%v, %v) = %v, want %v", tt.margin, tt.lifetime, got, tt.want)
		}
	}
}

// The backoff doubles up to the cap and never overflows.
func TestCappedBackoff(t *testing.T) {
	b := cappedBackoff(time.Second)
	for attempt, want := range map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 5: 32 * time.Second, 6: MaxRenewBackoff, 70: MaxRenewBackoff, 1000: MaxRenewBackoff} {
		if got := b(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}
