package caprf

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestRemoteHandlerShipsLogs(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{Token: "handler-token", LogURL: ts.server.URL + "/log"},
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 100)

	logger := slog.New(handler)
	logger.Info("test message", "key", "value")

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
		Transport: config.TransportConfig{Token: "filter-token", LogURL: ts.server.URL + "/log"},
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
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
	cfg := &config.MachineConfig{Transport: config.TransportConfig{Token: "drop-token"}}
	client := NewFromConfig(cfg)

	var mu sync.Mutex
	var shipped int

	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := &RemoteHandler{
		client:   client,
		inner:    inner,
		level:    slog.LevelInfo,
		buf:      make(chan string, 2),
		done:     make(chan struct{}),
		once:     &sync.Once{},
		dropped:  &atomic.Int64{},
		reported: &atomic.Int64{},
		gate:     &sendGate{},
	}

	go func() {
		defer close(handler.done)
		for range handler.buf {
			mu.Lock()
			shipped++
			mu.Unlock()
		}
	}()

	logger := slog.New(handler)
	for i := range 10 {
		logger.Info("message", "i", i)
	}

	handler.Close()

	mu.Lock()
	defer mu.Unlock()

	if shipped == 0 {
		t.Fatal("expected at least some messages to be shipped")
	}
	t.Logf("shipped %d of 10 messages", shipped)
}

func TestDropCounterIncrementsOnFullBuffer(t *testing.T) {
	cfg := &config.MachineConfig{Transport: config.TransportConfig{Token: "counter-token"}}
	client := NewFromConfig(cfg)
	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})

	buf := make(chan string, 2)
	done := make(chan struct{})
	blocker := make(chan struct{})

	handler := &RemoteHandler{
		client:   client,
		inner:    inner,
		level:    slog.LevelInfo,
		buf:      buf,
		done:     done,
		once:     &sync.Once{},
		dropped:  &atomic.Int64{},
		reported: &atomic.Int64{},
		gate:     &sendGate{},
	}

	go func() {
		defer close(done)
		<-blocker
		for v := range buf {
			_ = v // drain the buffered channel so Close() can complete
		}
	}()

	logger := slog.New(handler)
	for i := range 10 {
		logger.Info("msg", "i", i)
	}

	n := handler.DroppedCount()
	if n == 0 {
		t.Fatal("expected drop counter > 0 when buffer is full and drain is blocked")
	}
	t.Logf("drop counter = %d after sending 10 messages to buffer-of-2", n)

	close(blocker)
	handler.Close()
}

func TestDropCounterAccessibleFromDerivedHandler(t *testing.T) {
	cfg := &config.MachineConfig{Transport: config.TransportConfig{Token: "derived-counter-token"}}
	client := NewFromConfig(cfg)
	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})

	root := NewRemoteHandler(client, inner, slog.LevelInfo, 1)
	derived := root.WithAttrs([]slog.Attr{slog.String("k", "v")}).(*RemoteHandler)

	if root.dropped != derived.dropped {
		t.Fatal("expected root and derived handler to share the same drop counter")
	}
	if root.reported != derived.reported {
		t.Fatal("expected root and derived handler to share the same reported counter")
	}

	root.Close()
}

func TestRemoteHandlerWithAttrsAndGroups(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{Token: "attrs-token", LogURL: ts.server.URL + "/log"},
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
	var (
		mu       sync.Mutex
		received []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Delay the response so the line is only recorded if Close waits
		// for the in-flight send.
		time.Sleep(200 * time.Millisecond)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(body))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{Token: "derived-close-token", LogURL: srv.URL + "/log"},
	}
	client := NewFromConfig(cfg)

	inner := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	root := NewRemoteHandler(client, inner, slog.LevelInfo, 10)
	derived := root.WithAttrs([]slog.Attr{slog.String("component", "test")}).(*RemoteHandler)

	logger := slog.New(derived)
	logger.Info("log from derived handler")

	// Closing the derived handler closes the shared buffer and drains it.
	derived.Close()

	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	want := "[INFO] log from derived handler component=test"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("server received %q before Close returned, want [%q]", got, want)
	}

	// A second Close through the root handler must be a no-op.
	root.Close()
}

func TestMaybeWarnDroppedCadence(t *testing.T) {
	var warningCount atomic.Int64

	inner := &countingHandler{level: slog.LevelWarn, count: &warningCount}

	h := &RemoteHandler{
		inner:    inner,
		dropped:  &atomic.Int64{},
		reported: &atomic.Int64{},
		gate:     &sendGate{},
	}

	for i := int64(1); i <= dropWarnEvery; i++ {
		h.maybeWarnDropped(i)
	}
	if got := warningCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 warning after %d drops, got %d", dropWarnEvery, got)
	}

	for i := int64(1); i <= dropWarnEvery; i++ {
		h.maybeWarnDropped(dropWarnEvery + i)
	}
	if got := warningCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 warnings after %d drops, got %d", 2*dropWarnEvery, got)
	}
}

func TestDropCounterHighVolume(t *testing.T) {
	const messages = 1000
	const bufSize = 2

	cfg := &config.MachineConfig{Transport: config.TransportConfig{Token: "highvol-token"}}
	client := NewFromConfig(cfg)
	inner := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})

	buf := make(chan string, bufSize)
	done := make(chan struct{})
	blocker := make(chan struct{})

	h := &RemoteHandler{
		client:   client,
		inner:    inner,
		level:    slog.LevelInfo,
		buf:      buf,
		done:     done,
		once:     &sync.Once{},
		dropped:  &atomic.Int64{},
		reported: &atomic.Int64{},
		gate:     &sendGate{},
	}

	go func() {
		defer close(done)
		<-blocker
		for v := range buf {
			_ = v
		}
	}()

	logger := slog.New(h)
	for i := range messages {
		logger.Info("msg", "i", i)
	}

	dropped := h.DroppedCount()
	shipped := int64(messages) - dropped
	if dropped+shipped != messages {
		t.Fatalf("invariant: dropped(%d) + shipped(%d) != messages(%d)", dropped, shipped, messages)
	}
	if dropped < int64(messages-bufSize) {
		t.Fatalf("expected at least %d drops with blocked drain, got %d", messages-bufSize, dropped)
	}
	t.Logf("high-volume: %d messages, %d dropped, %d in buffer", messages, dropped, shipped)

	close(blocker)
	h.Close()
}

func TestWarnViaInnerNilInner(t *testing.T) {
	h := &RemoteHandler{inner: nil}
	h.warnViaInner("should be silently dropped")
}

type countingHandler struct {
	level slog.Leveler
	count *atomic.Int64
}

func (c *countingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= c.level.Level()
}

func (c *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= c.level.Level() {
		c.count.Add(1)
	}
	return nil
}

func (c *countingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *countingHandler) WithGroup(_ string) slog.Handler      { return c }

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
		Transport: config.TransportConfig{Token: "redact-group-token", LogURL: srv.URL + "/log"},
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

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{Token: "redact-test-token", LogURL: srv.URL + "/log"},
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

func TestHandleRedactsSensitiveAttrsInLocalLog(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{Token: "redact-local-token", LogURL: ts.server.URL + "/log"},
	}
	client := NewFromConfig(cfg)

	var local bytes.Buffer
	inner := slog.NewTextHandler(&local, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := NewRemoteHandler(client, inner, slog.LevelInfo, 100)
	logger := slog.New(handler).With("apiToken", "derived-secret")

	logger.Info("provisioning", "password", "s3cr3t", "component", "provision")
	handler.Close()

	got := local.String()
	for _, leaked := range []string{"s3cr3t", "derived-secret"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("local log leaked sensitive value %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("local log did not contain redaction marker: %s", got)
	}
	if !strings.Contains(got, "component=provision") {
		t.Fatalf("local log dropped non-sensitive attr: %s", got)
	}
}

func TestRemoteHandlerLogAfterCloseDoesNotPanic(t *testing.T) {
	var count atomic.Int64
	inner := &countingHandler{level: slog.LevelInfo, count: &count}
	root := NewRemoteHandler(NewFromConfig(&config.MachineConfig{}), inner, slog.LevelInfo, 10)
	logger := slog.New(root).With("tier", "overlay")

	root.Close()
	logger.Info("late log from a derived logger")

	if got := count.Load(); got != 1 {
		t.Fatalf("inner handler records = %d, want 1", got)
	}
}

func TestRemoteHandlerConcurrentLogAndClose(t *testing.T) {
	inner := slog.NewTextHandler(io.Discard, nil)
	root := NewRemoteHandler(NewFromConfig(&config.MachineConfig{}), inner, slog.LevelInfo, 10)
	logger := slog.New(root).With("tier", "underlay")

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				logger.Info("concurrent log")
			}
		}()
	}
	root.Close()
	wg.Wait()
}
