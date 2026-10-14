package caprf

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/auth"
)

func TestParseVarsTokenRenewalOptions(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader("TOKEN_EXPIRES_AT=2026-10-14T12:00:00Z\nTOKEN_EXPIRES_IN=1800\nTOKEN_RENEW_MARGIN_SECONDS=120\n" +
		"TOKEN_RENEW_RETRIES=7\nTOKEN_RENEW_BACKOFF_SECONDS=3\n"))
	if err != nil {
		t.Fatal(err)
	}
	tr := cfg.Transport
	if tr.TokenExpiresAt != "2026-10-14T12:00:00Z" || tr.TokenExpiresInSeconds != 1800 || tr.TokenRenewMarginSeconds != 120 || tr.TokenRenewRetries != 7 || tr.TokenRenewBackoffSeconds != 3 {
		t.Fatalf("transport = %+v", tr)
	}
	p := renewalPolicy(&tr)
	if p.Margin != 2*time.Minute || p.Retries != 7 || p.Backoff != 3*time.Second {
		t.Fatalf("policy = %+v", p)
	}
	for _, bad := range []string{"TOKEN_EXPIRES_AT=tomorrow", "TOKEN_RENEW_RETRIES=-1", "TOKEN_RENEW_RETRIES=101", "TOKEN_RENEW_MARGIN_SECONDS=x", "TOKEN_EXPIRES_IN=-5"} {
		if _, err := ParseVars(strings.NewReader(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

// Without TOKEN_URL, TOKEN_EXPIRES_IN or TOKEN_EXPIRES_AT stops the run the
// margin before the expiry, while the token still works for the report.
// TOKEN_EXPIRES_IN wins.
func TestTokenExpiryWithoutRenewalCallsTheFatalHandler(t *testing.T) {
	for _, tt := range []struct {
		name     string
		vars     func() string
		margin   int
		earliest time.Duration
		latest   time.Duration
	}{
		{"expires at, margin before expiry", func() string {
			return "TOKEN_EXPIRES_AT=" + time.Now().Add(1500*time.Millisecond).Format(time.RFC3339Nano)
		}, 1, 0, 1400 * time.Millisecond},
		{"expires at, already expired", func() string {
			return "TOKEN_EXPIRES_AT=" + time.Now().Add(-time.Minute).Format(time.RFC3339)
		}, 0, 0, 500 * time.Millisecond},
		{"expires in wins over a far expires at", func() string {
			return "TOKEN_EXPIRES_IN=2\nTOKEN_EXPIRES_AT=" + time.Now().Add(time.Hour).Format(time.RFC3339)
		}, 1, 700 * time.Millisecond, 1500 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseVars(strings.NewReader("TOKEN=secret-static-token\n" + tt.vars() + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Transport.TokenRenewMarginSeconds = tt.margin
			c := NewFromConfig(cfg)
			fatal := make(chan error, 1)
			c.SetTokenRenewalFatalHandler(func(err error) { fatal <- err })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			if err := c.StartTokenRenewal(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-fatal:
				if !errors.Is(err, auth.ErrTokenExpired) || strings.Contains(err.Error(), "secret-static-token") {
					t.Fatalf("fatal = %v, want ErrTokenExpired without the token", err)
				}
				if d := time.Since(start); d < tt.earliest || d > tt.latest {
					t.Fatalf("fatal after %v, want %v-%v", d, tt.earliest, tt.latest)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no fatal call")
			}
		})
	}
}

// With TOKEN_URL the expires_in of the exchange is authoritative: a short
// TOKEN_EXPIRES_IN or TOKEN_EXPIRES_AT from the vars does not stop the run.
func TestTokenURLExchangeWinsOverTheExpiryVars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(auth.TokenResponse{AccessToken: "exchanged", ExpiresIn: 3600})
	}))
	defer srv.Close()
	cfg, err := ParseVars(strings.NewReader("HOSTNAME=m1\nTOKEN=bootstrap\nTOKEN_URL=" + srv.URL + "/agent/token\n" +
		"TOKEN_EXPIRES_IN=1\nTOKEN_EXPIRES_AT=" + time.Now().Add(-time.Minute).Format(time.RFC3339) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := NewFromConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.AcquireToken(ctx); err != nil {
		t.Fatal(err)
	}
	fatal := make(chan error, 1)
	c.SetTokenRenewalFatalHandler(func(err error) { fatal <- err })
	if err := c.StartTokenRenewal(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fatal:
		t.Fatalf("the vars expiry stopped a token with a fresh expires_in: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if c.CurrentToken() != "exchanged" {
		t.Fatalf("token = %q, want the exchanged one", c.CurrentToken())
	}
}
