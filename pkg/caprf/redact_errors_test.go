package caprf

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

const leakURL = "http://127.0.0.1:1/upload?X-Amz-Signature=SECRETSIG123"

func TestTransportErrorsRedactQueryCredentials(t *testing.T) {
	c := NewFromConfig(&config.MachineConfig{})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, leakURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.doCrashUploadRequest(req, redactedURL(leakURL)); err == nil || strings.Contains(err.Error(), "SECRETSIG123") {
		t.Fatalf("crash upload error leaks or is nil: %v", err)
	}
	if err := c.doPost(context.Background(), leakURL, "{}"); err == nil || strings.Contains(err.Error(), "SECRETSIG123") {
		t.Fatalf("doPost error leaks or is nil: %v", err)
	}
}

func TestTransportErrorsKeepCancelAndTimeout(t *testing.T) {
	c := NewFromConfig(&config.MachineConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.doPost(ctx, leakURL, "{}"); !errors.Is(err, context.Canceled) {
		t.Fatalf("doPost with canceled context = %v, want context.Canceled", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	c.httpClient = &http.Client{Timeout: 50 * time.Millisecond}
	err := c.doPost(context.Background(), srv.URL+"/?X-Amz-Signature=SECRETSIG123", "{}")
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("doPost timeout = %v, want a net.Error with Timeout()", err)
	}
	if strings.Contains(err.Error(), "SECRETSIG123") {
		t.Fatalf("timeout error leaks the signature: %v", err)
	}
}

func TestTransportErrorsRedactRedirectTarget(t *testing.T) {
	for _, location := range []string{
		"http://127.0.0.1:1/target?X-Amz-Signature=REDIRECTSECRET",
		"http://127.0.0.1:1/%zz?X-Amz-Signature=REDIRECTSECRET", // malformed Location
		"/%zz?X-Amz-Signature=REDIRECTSECRET",                   // malformed relative Location
		"/%zz#token=REDIRECTSECRET",                             // malformed relative Location with fragment
		"//user:REDIRECTSECRET@127.0.0.1:1/%zz",                 // malformed protocol-relative Location
		"/%zz?note='&X-Amz-Signature=REDIRECTSECRET",            // quote inside the query
		"//us'er:REDIRECTSECRET@127.0.0.1:1/%zz",                // quote inside the userinfo
		"/%zz?a=\" b' X-Amz-Signature=REDIRECTSECRET#\"c",       // attacker-controlled delimiters
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
		}))
		c := NewFromConfig(&config.MachineConfig{})
		err := c.doPost(context.Background(), srv.URL+"/start", "{}")
		srv.Close()
		if err == nil {
			t.Fatalf("%s: expected an error from the redirect target", location)
		}
		if strings.Contains(err.Error(), "REDIRECTSECRET") {
			t.Fatalf("redirect error leaks the signature: %v", err)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		op       string
		cause    string
		timeout  bool
		sentinel error
	}{
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled), "Post", "canceled", false, context.Canceled},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), "Post", "timeout", true, context.DeadlineExceeded},
		{"net timeout", timeoutErr{}, "Post", "timeout", true, nil},
		{"dns", &net.DNSError{Err: "no such host", Name: "secret.example"}, "Post", "dns lookup failed", false, nil},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "Post", "connection refused", false, nil},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, "Post", "connection reset", false, nil},
		{"tls authority", x509.UnknownAuthorityError{}, "Get", "tls error", false, nil},
		{"tls record", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "Get", "tls error", false, nil},
		{"parse", errors.New(`invalid URL escape "%zz"`), "parse", "invalid URL", false, nil},
		{"other", errors.New("server said ?token=SECRET"), "Post", "transport error", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cause, timeout, sentinel := classifyTransportError(tt.err, tt.op)
			if cause != tt.cause || timeout != tt.timeout || !errors.Is(sentinel, tt.sentinel) || (tt.sentinel == nil && sentinel != nil) {
				t.Fatalf("classifyTransportError() = %q, %v, %v; want %q, %v, %v", cause, timeout, sentinel, tt.cause, tt.timeout, tt.sentinel)
			}
			err := redactedURLError("https://user:pw@caprf.example/x?token=SECRET", tt.err)
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "pw@") {
				t.Fatalf("error leaks credentials: %v", err)
			}
			if tt.sentinel != nil && !errors.Is(err, tt.sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tt.sentinel)
			}
		})
	}
}

func TestRedactedURLErrorDropsUnknownOperation(t *testing.T) {
	err := redactedURLError("https://caprf.example/upload", &url.Error{Op: "EVIL-SECRET", URL: "https://caprf.example/upload", Err: errors.New("x")})
	if strings.Contains(err.Error(), "EVIL-SECRET") {
		t.Fatalf("error contains a server-controlled method: %v", err)
	}
}
