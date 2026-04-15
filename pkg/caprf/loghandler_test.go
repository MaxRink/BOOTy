package caprf

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestRemoteHandlerShipsLogs(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Token:  "handler-token",
		LogURL: ts.server.URL + "/log",
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 100)

	logger := slog.New(handler)
	logger.Info("test message", "key", "value")

	// Close flushes the buffer.
	handler.Close()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if len(ts.logs) != 1 {
		t.Fatalf("expected 1 log shipped, got %d", len(ts.logs))
	}
}

func TestRemoteHandlerLevelFilter(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Token:  "filter-token",
		LogURL: ts.server.URL + "/log",
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	// Only ship WARN and above.
	handler := NewRemoteHandler(client, inner, slog.LevelWarn, 100)

	logger := slog.New(handler)
	logger.Info("should be filtered")
	logger.Warn("should pass")

	handler.Close()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if len(ts.logs) != 1 {
		t.Fatalf("expected 1 log (warn only), got %d", len(ts.logs))
	}
}

func TestRemoteHandlerDropsWhenFull(t *testing.T) {
	cfg := &config.MachineConfig{
		Token: "drop-token",
		// Use a non-existent URL so we can test buffer behavior without draining.
	}
	client := NewFromConfig(cfg)

	// Very small buffer to trigger overflow.
	var mu sync.Mutex
	var shipped int

	// We'll use a real handler but with a logger that counts.
	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := &RemoteHandler{
		client: client,
		inner:  inner,
		level:  slog.LevelInfo,
		buf:    make(chan string, 2), // Only 2 slots.
		done:   make(chan struct{}),
	}

	// Count messages that make it through.
	go func() {
		defer close(handler.done)
		for range handler.buf {
			mu.Lock()
			shipped++
			mu.Unlock()
		}
	}()

	logger := slog.New(handler)
	// Fire more logs than buffer can hold rapidly.
	for i := range 10 {
		logger.Info("message", "i", i)
	}

	handler.Close()

	mu.Lock()
	defer mu.Unlock()

	// At least some should have been delivered, but not necessarily all 10.
	if shipped == 0 {
		t.Fatal("expected at least some messages to be shipped")
	}
	t.Logf("shipped %d of 10 messages", shipped)
}

func TestRemoteHandlerWithAttrsAndGroups(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Token:  "attrs-token",
		LogURL: ts.server.URL + "/log",
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 100)

	logger := slog.New(handler).With("component", "test").WithGroup("sub")
	logger.Info("grouped message")

	handler.Close()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if len(ts.logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(ts.logs))
	}
}

func TestRemoteHandlerEnabled(t *testing.T) {
	handler := &RemoteHandler{level: slog.LevelWarn}

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("expected Info to be disabled at Warn level")
	}
	if !handler.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("expected Warn to be enabled at Warn level")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("expected Error to be enabled at Warn level")
	}
}

func TestRemoteHandlerCloseIdempotent(t *testing.T) {
	cfg := &config.MachineConfig{}
	client := NewFromConfig(cfg)
	inner := slog.NewTextHandler(os.Stderr, nil)
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 10)

	// Close multiple times should not panic.
	done := make(chan struct{})
	go func() {
		handler.Close()
		handler.Close()
		handler.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() deadlocked")
	}
}

func TestRemoteHandlerCloseFromDerivedHandlers(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Token:  "derived-close-token",
		LogURL: ts.server.URL + "/log",
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	root := NewRemoteHandler(client, inner, slog.LevelInfo, 10)
	derived := root.WithAttrs([]slog.Attr{slog.String("component", "test")}).(*RemoteHandler)

	logger := slog.New(derived)
	logger.Info("log from derived handler")

	// Closing both handlers should not panic or deadlock.
	root.Close()
	derived.Close()
}

func TestRedactAttr(t *testing.T) {
	tests := []struct {
		key     string
		value   string
		want    string
		redacts bool
	}{
		{"password", "s3cr3t", "[REDACTED]", true},
		{"MOKPassword", "hunter2", "[REDACTED]", true},
		{"token", "tok_abc123", "[REDACTED]", true},
		{"access_token", "tok_abc123", "[REDACTED]", true},
		{"secret", "mysecret", "[REDACTED]", true},
		{"api_key", "key123", "[REDACTED]", true},
		{"privateKey", "-----BEGIN", "[REDACTED]", true},
		{"credential", "mycred", "[REDACTED]", true},
		{"auth", "Basic xyz", "[REDACTED]", true},
		{"authorization", "Bearer xyz", "[REDACTED]", true},
		{"Authorization", "Bearer xyz", "[REDACTED]", true},
		{"authorization_header", "Bearer xyz", "[REDACTED]", true},
		{"Authorization_header", "Bearer xyz", "[REDACTED]", true},
		{"authorizationToken", "Bearer xyz", "[REDACTED]", true},
		{"oauth2Token", "tok_abc123", "[REDACTED]", true},
		{"x509Cert", "PEM", "[REDACTED]", true},
		{"pkcs12Password", "hunter2", "[REDACTED]", true},
		{"db2Password", "hunter2", "[REDACTED]", true},
		{"session", "sess_123", "[REDACTED]", true},
		{"bearer", "eyJ...", "[REDACTED]", true},
		{"cert", "PEM", "[REDACTED]", true},
		{"apikey", "abc123", "[REDACTED]", true},
		{"secretkey", "abc123", "[REDACTED]", true},
		{"privatekey", "abc123", "[REDACTED]", true},
		{"authorizationheader", "Bearer xyz", "[REDACTED]", true},
		{"x509cert", "PEM", "[REDACTED]", true},
		{"message", "hello", "hello", false},
		{"component", "provision", "provision", false},
		{"ip", "192.168.0.1", "192.168.0.1", false},
		{"count", "42", "42", false},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			attr := slog.String(tc.key, tc.value)
			got := redactAttr(attr)
			if got.Value.String() != tc.want {
				t.Errorf("redactAttr(%q, %q) = %q, want %q",
					tc.key, tc.value, got.Value.String(), tc.want)
			}
			if tc.redacts && got.Key != tc.key {
				t.Errorf("redactAttr should preserve key %q, got %q", tc.key, got.Key)
			}
		})
	}
}

func TestRedactAttr_FalsePositives(t *testing.T) {
	keys := []string{"author", "keyboard", "certainty"}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			attr := slog.String(key, "value")
			got := redactAttr(attr)
			if got.Value.String() == "[REDACTED]" {
				t.Errorf("key %q should NOT be redacted (false positive)", key)
			}
		})
	}
}

func TestRedactAttr_CamelCase(t *testing.T) {
	if got := redactAttr(slog.String("privateKey", "val")); got.Value.String() != "[REDACTED]" {
		t.Errorf("privateKey should be redacted, got %q", got.Value.String())
	}
	if got := redactAttr(slog.String("authorName", "val")); got.Value.String() == "[REDACTED]" {
		t.Errorf("authorName should NOT be redacted")
	}
}

func TestRedactAttr_GroupedAttrs(t *testing.T) {
	group := slog.Group("config",
		slog.String("password", "s3cr3t"),
		slog.String("host", "example.com"),
	)
	got := redactAttr(group)
	if got.Value.Kind() != slog.KindGroup {
		t.Fatal("expected group kind back")
	}
	attrs := got.Value.Group()
	if len(attrs) != 2 {
		t.Fatalf("expected 2 attrs, got %d", len(attrs))
	}
	if attrs[0].Value.String() != "[REDACTED]" {
		t.Errorf("nested password should be redacted, got %q", attrs[0].Value.String())
	}
	if attrs[1].Value.String() != "example.com" {
		t.Errorf("nested host should pass through, got %q", attrs[1].Value.String())
	}
}

func TestRedactAttr_SensitiveGroupKeyRedactsChildren(t *testing.T) {
	group := slog.Group("authorization",
		slog.String("value", "bearer-xyz"),
		slog.String("scheme", "Bearer"),
	)

	got := redactAttr(group)
	if got.Value.Kind() != slog.KindGroup {
		t.Fatal("expected group kind back")
	}

	attrs := got.Value.Group()
	if len(attrs) != 2 {
		t.Fatalf("expected 2 attrs, got %d", len(attrs))
	}
	for _, attr := range attrs {
		if attr.Value.String() != "[REDACTED]" {
			t.Fatalf("expected all child values redacted, got %q for key %q", attr.Value.String(), attr.Key)
		}
	}
}

func TestHandleRedactsSensitiveAttrs_Grouped(t *testing.T) {
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	cfg := &config.MachineConfig{
		Token:  "redact-group-token",
		LogURL: srv.URL + "/log",
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 100)

	logger := slog.New(handler)
	logger.Info("provisioning",
		slog.Group("creds", slog.String("password", "s3cr3t"), slog.String("user", "admin")),
	)
	handler.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 1 {
		t.Fatalf("expected 1 log shipped, got %d", len(bodies))
	}
	shipped := bodies[0]
	if strings.Contains(shipped, "s3cr3t") {
		t.Errorf("nested password value must be redacted in shipped log, got: %s", shipped)
	}
}

func TestHandleRedactsSensitiveAttrs(t *testing.T) {
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	cfg := &config.MachineConfig{
		Token:  "redact-test-token",
		LogURL: srv.URL + "/log",
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 100)

	logger := slog.New(handler)
	logger.Info("provisioning", "password", "s3cr3t", "component", "provision")

	handler.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 1 {
		t.Fatalf("expected 1 log shipped, got %d", len(bodies))
	}

	shipped := bodies[0]
	if !strings.Contains(shipped, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in shipped log, got: %s", shipped)
	}
	if strings.Contains(shipped, "s3cr3t") {
		t.Errorf("expected password value to be redacted, got: %s", shipped)
	}
	if !strings.Contains(shipped, "component=provision") {
		t.Errorf("expected non-sensitive attr to pass through, got: %s", shipped)
	}
}
