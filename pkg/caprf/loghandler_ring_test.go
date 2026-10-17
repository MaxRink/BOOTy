package caprf

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

// logSink is an httptest /log server that records each body. While block
// is open, a request waits on it; a body that contains fail gets a 500.
type logSink struct {
	mu      sync.Mutex
	bodies  []string
	arrived chan string
	block   chan struct{}
	fail    string
	server  *httptest.Server
}

func newLogSink(t *testing.T) *logSink {
	t.Helper()
	s := &logSink{arrived: make(chan string, 100)}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		block, fail := s.block, s.fail
		s.mu.Unlock()
		s.arrived <- string(body) // after the read: a test may swap block now
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		if fail != "" && strings.Contains(string(body), fail) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *logSink) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// waitArrived waits until a request whose body contains want reached the
// server (it may still be blocked there).
func (s *logSink) waitArrived(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-s.arrived:
			if strings.Contains(got, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no request with %q reached the server", want)
		}
	}
}

func (s *logSink) handler(t *testing.T, maxLines, maxBytes int) *RemoteHandler {
	t.Helper()
	client := NewFromConfig(&config.MachineConfig{Transport: config.TransportConfig{LogURL: s.server.URL}})
	h := newRemoteHandler(client, slog.NewTextHandler(io.Discard, nil), slog.LevelDebug, maxLines, maxBytes)
	t.Cleanup(h.Close)
	return h
}

func ringLines(r *logRing) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := make([]string, len(r.entries))
	for i, e := range r.entries {
		lines[i] = e.line
	}
	return lines
}

func TestLogRingEvictionOrder(t *testing.T) {
	r := newLogRing(4, 1<<20)
	r.push(0, "d1")
	r.push(1, "i1")
	r.push(2, "w1")
	r.push(3, "e1")
	r.push(1, "i2") // evicts d1
	r.push(1, "i3") // evicts i1
	r.push(2, "w2") // evicts i2
	r.push(0, "d2") // nothing ranks at or below debug: d2 itself is dropped
	if got, want := strings.Join(ringLines(r), ","), "w1,e1,i3,w2"; got != want {
		t.Fatalf("ring after d2 = %s, want %s", got, want)
	}
	r.push(3, "e2") // evicts i3
	r.push(3, "e3") // evicts w1, the oldest of the lowest rank left

	if got, want := strings.Join(ringLines(r), ","), "e1,w2,e2,e3"; got != want {
		t.Fatalf("ring = %s, want %s", got, want)
	}
	if got, want := r.drops(), [4]int64{2, 3, 1, 0}; got != want {
		t.Fatalf("drops = %v, want %v", got, want)
	}
	if got := r.droppedTotal(); got != 6 {
		t.Fatalf("droppedTotal = %d, want 6", got)
	}
}

func TestLogRingByteBound(t *testing.T) {
	r := newLogRing(100, 10)
	r.push(1, "aaaa")
	r.push(3, "bbbb")
	r.push(1, "cccc") // 12 bytes > 10: evicts aaaa
	if got, want := strings.Join(ringLines(r), ","), "bbbb,cccc"; got != want {
		t.Fatalf("ring = %s, want %s", got, want)
	}
	if r.bytes != 8 {
		t.Fatalf("bytes = %d, want 8", r.bytes)
	}
	r.push(3, "0123456789abcdef") // longer than the bound: cut, evicts all
	if got, want := strings.Join(ringLines(r), ","), "0123456789"; got != want {
		t.Fatalf("ring = %s, want %s", got, want)
	}
	if got, want := r.drops(), [4]int64{0, 2, 0, 1}; got != want {
		t.Fatalf("drops = %v, want %v", got, want)
	}
	if e, ok := r.pop(); !ok || e.line != "0123456789" || r.bytes != 0 {
		t.Fatalf("pop = %q %v, bytes %d", e.line, ok, r.bytes)
	}
}

func TestLogRingCloseStopsPushAndEndsPop(t *testing.T) {
	r := newLogRing(4, 100)
	r.push(1, "kept")
	r.close()
	r.push(3, "late")
	if e, ok := r.pop(); !ok || e.line != "kept" {
		t.Fatalf("pop = %q %v, want kept", e.line, ok)
	}
	if _, ok := r.pop(); ok {
		t.Fatal("pop after close and drain returned a line")
	}
	if r.droppedTotal() != 0 {
		t.Fatalf("a line after close counted as dropped: %v", r.drops())
	}
}

// A slow sink holds the first line while the ring overflows: DEBUG and
// INFO are evicted, WARN and ERROR arrive in order, and one summary with
// the counts follows the first line after the drops.
func TestRemoteHandlerSlowSinkKeepsWarnAndError(t *testing.T) {
	s := newLogSink(t)
	block := make(chan struct{})
	s.block = block
	h := s.handler(t, 4, 1<<20)
	log := slog.New(h)

	log.Info("first")
	s.waitArrived(t, "first")
	log.Debug("d1")
	log.Info("i1")
	log.Warn("w1")
	log.Error("e1")
	log.Info("i2")
	log.Info("i3")
	log.Warn("w2")
	s.mu.Lock()
	s.block = nil
	s.mu.Unlock()
	close(block)

	// Second round: the sender holds "second" while two more INFO lines
	// are evicted. The next summary reports only these.
	s.waitArrived(t, "w2")
	block2 := make(chan struct{})
	s.mu.Lock()
	s.block = block2
	s.mu.Unlock()
	log.Info("second")
	s.waitArrived(t, "second")
	log.Info("i4")
	log.Warn("w3")
	log.Warn("w4")
	log.Warn("w5")
	log.Info("i5")  // evicts i4
	log.Error("e2") // evicts i5
	s.mu.Lock()
	s.block = nil
	s.mu.Unlock()
	close(block2)
	h.Close()

	want := []string{
		"[INFO] first",
		"[WARN] remote log handler dropped lines code=BOOTY-LOG-001 debug=1 info=2 warn=0 error=0",
		"[WARN] w1", "[ERROR] e1", "[INFO] i3", "[WARN] w2",
		"[INFO] second",
		"[WARN] remote log handler dropped lines code=BOOTY-LOG-001 debug=0 info=2 warn=0 error=0",
		"[WARN] w3", "[WARN] w4", "[WARN] w5", "[ERROR] e2",
	}
	if got := s.received(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("received:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := h.DroppedCount(); got != 5 {
		t.Fatalf("DroppedCount = %d, want 5", got)
	}
}

// A line that fails all retries counts as dropped; the summary follows the
// next line that arrives.
func TestRemoteHandlerFailingSinkReportsDropsOnRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the client retry backoff")
	}
	s := newLogSink(t)
	s.fail = "lost"
	h := s.handler(t, 2, 1<<20)
	log := slog.New(h)

	log.Info("lost")
	s.waitArrived(t, "lost")
	log.Debug("d1")
	log.Info("i1")
	log.Error("e1") // evicts d1
	h.Close()

	want := []string{
		"[INFO] i1",
		"[WARN] remote log handler dropped lines code=BOOTY-LOG-001 debug=1 info=1 warn=0 error=0",
		"[ERROR] e1",
	}
	if got := s.received(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("received:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A line that fails at the end of a run, with no later line, is
// reported by the flush.
func TestRemoteHandlerFlushShipsPendingSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the client retry backoff")
	}
	s := newLogSink(t)
	s.fail = "lost"
	h := s.handler(t, 10, 1<<20)
	h.sink.flushTimeout = 30 * time.Second
	slog.New(h).Error("lost")
	h.Close()

	want := "[WARN] remote log handler dropped lines code=BOOTY-LOG-001 debug=0 info=0 warn=0 error=1"
	if got := s.received(); len(got) != 1 || got[0] != want {
		t.Fatalf("received %q, want only the summary", got)
	}
}

func TestRemoteHandlerCloseDeliversLastLine(t *testing.T) {
	s := newLogSink(t)
	h := s.handler(t, 1000, 1<<20)
	log := slog.New(h).With("step", "handoff")
	for i := range 50 {
		log.Info("progress", "i", i)
	}
	log.Info("last line before kexec")
	h.Close()

	got := s.received()
	if len(got) != 51 || got[50] != "[INFO] last line before kexec step=handoff" {
		t.Fatalf("received %d lines, last %q", len(got), got[len(got)-1])
	}
}

func TestRemoteHandlerCloseRespectsDeadline(t *testing.T) {
	s := newLogSink(t)
	s.block = make(chan struct{}) // never released: the sink hangs
	console := &syncWriter{w: &bytes.Buffer{}}
	client := NewFromConfig(&config.MachineConfig{Transport: config.TransportConfig{LogURL: s.server.URL}})
	h := newRemoteHandler(client, slog.NewTextHandler(console, nil), slog.LevelInfo, 10, 1<<20)
	h.sink.flushTimeout = 200 * time.Millisecond
	log := slog.New(h)
	log.Info("stuck")
	s.waitArrived(t, "stuck")
	log.Info("queued-1")
	log.Info("queued-2")

	start := time.Now()
	h.Close()
	if took := time.Since(start); took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("Close took %v, want about the 200ms deadline", took)
	}
	if out := console.String(); !strings.Contains(out, "remote log flush deadline reached") ||
		!strings.Contains(out, "undelivered=2") {
		t.Fatalf("console lacks the deadline warning: %s", out)
	}
	select {
	case <-h.sink.done:
	case <-time.After(5 * time.Second):
		t.Fatal("sender did not stop after the deadline canceled it")
	}
}

func TestRemoteHandlerFlushTimeoutFromConfig(t *testing.T) {
	for _, tc := range []struct {
		value *int
		want  time.Duration
	}{
		{nil, 5 * time.Second},
		{new(0), 0},
		{new(12), 12 * time.Second},
	} {
		client := NewFromConfig(&config.MachineConfig{Transport: config.TransportConfig{LogFlushTimeoutSec: tc.value}})
		h := NewRemoteHandler(client, slog.NewTextHandler(io.Discard, nil), slog.LevelInfo, 1)
		if h.sink.flushTimeout != tc.want {
			t.Errorf("flushTimeout = %v, want %v", h.sink.flushTimeout, tc.want)
		}
		h.Close()
	}
}

func TestRemoteHandlerRedactsSecretValuesBeforeBuffering(t *testing.T) {
	s := newLogSink(t)
	block := make(chan struct{})
	s.block = block
	h := s.handler(t, 10, 1<<20)
	log := slog.New(h)
	log.Info("first")
	s.waitArrived(t, "first")

	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJib290eSJ9.c2lnbmF0dXJlLXZhbHVl"
	log.Info("calling https://robot:urlpass@caprf.example/token with Authorization: Bearer hdr-token")
	log.Info("token exchange", "response", "access_token="+jwt)
	log.Warn("cryptsetup failed", "output", "luksKey=lukssecret LUKS_PASSPHRASE=hunter2")
	log.Error("bad key", "detail", "-----BEGIN RSA PRIVATE KEY-----\nMIIEsecret\n-----END RSA PRIVATE KEY-----")

	for _, line := range ringLines(h.sink.ring) {
		for _, secret := range []string{"urlpass", "hdr-token", jwt, "lukssecret", "hunter2", "MIIEsecret"} {
			if strings.Contains(line, secret) {
				t.Errorf("buffered line leaks %q: %s", secret, line)
			}
		}
	}
	close(block)
	h.Close()
	if got := s.received(); len(got) != 5 {
		t.Fatalf("received %d lines, want 5: %q", len(got), got)
	}
}

// syncWriter serializes writes from the sender goroutine and Close.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p) //nolint:wrapcheck // test helper
}

func TestParseVarsLogFlushTimeout(t *testing.T) {
	for _, tc := range []struct {
		vars string
		want *int
	}{
		{"HOSTNAME=x\n", nil},
		{"LOG_FLUSH_TIMEOUT_SEC=\n", nil},
		{"LOG_FLUSH_TIMEOUT_SEC=0\n", new(0)},
		{"LOG_FLUSH_TIMEOUT_SEC=\"10\"\n", new(10)},
	} {
		cfg, err := ParseVars(strings.NewReader(tc.vars))
		if err != nil {
			t.Fatalf("ParseVars(%q): %v", tc.vars, err)
		}
		got := cfg.Transport.LogFlushTimeoutSec
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("ParseVars(%q) LogFlushTimeoutSec = %v, want %v", tc.vars, got, tc.want)
		}
	}
	for _, bad := range []string{"-1", "61", "5s", "x"} {
		if _, err := ParseVars(strings.NewReader("LOG_FLUSH_TIMEOUT_SEC=" + bad + "\n")); err == nil {
			t.Errorf("ParseVars accepted LOG_FLUSH_TIMEOUT_SEC=%s", bad)
		}
	}
}
