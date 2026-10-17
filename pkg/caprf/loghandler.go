package caprf

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/telekom/BOOTy/pkg/logging"
)

// dropWarnEvery controls how often we emit a warning about dropped messages.
// A warning is logged after every Nth drop (or at Close).
const dropWarnEvery = 100

// sensitiveWords is the set of exact key segments that trigger redaction.
// The implementation uses segment-based matching: a key is split on separators
// (_  .  -) and camelCase boundaries, then each segment is checked against this
// set. Additionally, a substring fallback catches all-lowercase concatenated keys
// such as "apikey", "secretkey", or "privatekey".
var sensitiveWords = map[string]struct{}{
	"password":      {},
	"token":         {},
	"secret":        {},
	"key":           {},
	"credential":    {},
	"auth":          {},
	"authorization": {},
	"cert":          {},
	"private":       {},
	"bearer":        {},
	"session":       {},
}

// highSignalWords are high-confidence sensitive substrings used as a fallback
// for all-lowercase concatenated keys that splitCamel cannot split
// (e.g. "apikey", "secretkey", "privatekey", "authorizationheader").
// "key" is matched as a suffix only (via endsWithWordKey) to avoid false positives
// from words like "keyboard". Short words like "cert" are intentionally absent here
// because they appear in too many non-sensitive words (e.g. "certainty"); they are
// instead caught via segment matching once splitCamel splits digit-letter boundaries
// (e.g. "x509cert" → ["x509", "cert"]).
var highSignalWords = []string{
	"password", "token", "secret", "credential", "bearer", "private", "session",
	"authorization",
}

// splitKeySegments splits a slog attribute key on common separators (_  .  -)
// and on camelCase boundaries so that segment matching is precise.
// Examples: "privateKey" → ["private","Key"], "access_token" → ["access","token"].
func splitKeySegments(key string) []string {
	// First split on explicit separators.
	parts := strings.FieldsFunc(key, func(r rune) bool {
		return r == '_' || r == '.' || r == '-'
	})
	// Then expand each part on camelCase boundaries.
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		segs = append(segs, splitCamel(p)...)
	}
	if len(segs) == 0 {
		return []string{key}
	}
	return segs
}

func splitCamel(s string) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var segs []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			segs = append(segs, string(runes[start:i]))
			start = i
		case unicode.IsDigit(prev) && unicode.IsUpper(cur):
			segs = append(segs, string(runes[start:i]))
			start = i
		case unicode.IsDigit(prev) && unicode.IsLower(cur):
			segs = append(segs, string(runes[start:i]))
			start = i
		case i >= 2 && unicode.IsUpper(runes[i-2]) && unicode.IsUpper(prev) && unicode.IsLower(cur):
			segs = append(segs, string(runes[start:i-1]))
			start = i - 1
		}
	}
	segs = append(segs, string(runes[start:]))
	return segs
}

// sensitiveKeyPrefixes are known sensitive prefixes that form a sensitive
// compound key when directly concatenated with "key" (no separator), e.g.
// "apikey". Separator-delimited forms like "api_key" are already caught by
// the word-boundary check in endsWithWordKey.
var sensitiveKeyPrefixes = []string{"api"}

// endsWithWordKey reports whether s ends with "key" as a whole word, meaning
// the "key" suffix is preceded by a non-letter character, "key" is the entire
// string, or the prefix before "key" is a known sensitive keyword. This avoids
// false positives from common words like "monkey" or "hockey" while correctly
// matching compound forms like "apikey".
func endsWithWordKey(s string) bool {
	if !strings.HasSuffix(s, "key") {
		return false
	}
	before := s[:len(s)-3]
	if before == "" {
		return true
	}
	runes := []rune(before)
	if !unicode.IsLetter(runes[len(runes)-1]) {
		return true
	}
	// Check known sensitive prefixes that compound directly with "key".
	for _, prefix := range sensitiveKeyPrefixes {
		if strings.HasSuffix(before, prefix) {
			return true
		}
	}
	return false
}

// isSensitiveKey reports whether any segment of key exactly matches a sensitive
// word, or the full lowercased key contains a high-signal substring (fallback for
// all-lowercase concatenated forms like "apikey", "secretkey", "privatekey").
func isSensitiveKey(key string) bool {
	for _, seg := range splitKeySegments(key) {
		if _, ok := sensitiveWords[strings.ToLower(seg)]; ok {
			return true
		}
	}
	lower := strings.ToLower(key)
	if endsWithWordKey(lower) {
		return true
	}
	for _, word := range highSignalWords {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// redactAttr replaces sensitive attribute values with [REDACTED].
// It recurses into slog groups so nested keys are also protected.
func redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		if isSensitiveKey(a.Key) {
			group := a.Value.Group()
			redacted := make([]slog.Attr, len(group))
			for i, ga := range group {
				redacted[i] = slog.String(ga.Key, "[REDACTED]")
			}
			return slog.Attr{Key: a.Key, Value: slog.GroupValue(redacted...)}
		}
		group := a.Value.Group()
		redacted := make([]slog.Attr, len(group))
		for i, ga := range group {
			redacted[i] = redactAttr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(redacted...)}
	}
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	return a
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		redacted[i] = redactAttr(attr)
	}
	return redacted
}

func redactRecord(r *slog.Record) slog.Record {
	redacted := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(attr slog.Attr) bool {
		redacted.AddAttrs(redactAttr(attr))
		return true
	})
	return redacted
}

// defaultBufferBytes bounds the bytes of the queued log lines.
const defaultBufferBytes = 4 << 20

// shipTimeout bounds one POST of a log line, retries included.
const shipTimeout = 10 * time.Second

// dropSummaryCode marks the record that reports dropped lines.
const dropSummaryCode = "BOOTY-LOG-001"

// levelNames names the drop counters by rank (see levelRank).
var levelNames = [4]string{"debug", "info", "warn", "error"}

// levelRank maps a level to its eviction rank: 0 DEBUG (and below),
// 1 INFO, 2 WARN, 3 ERROR (and above).
func levelRank(l slog.Level) int {
	switch {
	case l < slog.LevelInfo:
		return 0
	case l < slog.LevelWarn:
		return 1
	case l < slog.LevelError:
		return 2
	default:
		return 3
	}
}

type ringEntry struct {
	rank int
	line string
}

// logRing is the bounded FIFO between Handle and the sender goroutine. It
// is bounded by line count and by bytes. When it is full, it evicts the
// oldest line of the lowest rank, so DEBUG goes first, then INFO, and
// WARN and ERROR stay as long as lower lines are left. It counts every
// lost line per rank.
type logRing struct {
	mu       sync.Mutex
	cond     sync.Cond
	entries  []ringEntry
	bytes    int
	maxLines int
	maxBytes int
	closed   bool
	dropped  [4]int64
}

func newLogRing(maxLines, maxBytes int) *logRing {
	r := &logRing{maxLines: max(maxLines, 1), maxBytes: max(maxBytes, 1)}
	r.cond.L = &r.mu
	return r
}

// push queues line. A line above the byte bound is cut to it. When the
// ring is full and every queued line ranks above the new one, the new
// line is dropped instead. After close, push drops the line without a
// count: the handoff has started.
func (r *logRing) push(rank int, line string) {
	if len(line) > r.maxBytes {
		line = strings.ToValidUTF8(line[:r.maxBytes], "")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for len(r.entries) >= r.maxLines || r.bytes+len(line) > r.maxBytes {
		// ponytail: linear scan per eviction; keep one queue per rank if
		// the ring ever holds more than a few thousand lines.
		victim := 0
		for i, e := range r.entries {
			if e.rank < r.entries[victim].rank {
				victim = i
			}
		}
		if r.entries[victim].rank > rank {
			r.dropped[rank]++
			return
		}
		r.dropped[r.entries[victim].rank]++
		r.bytes -= len(r.entries[victim].line)
		r.entries = slices.Delete(r.entries, victim, victim+1)
	}
	r.entries = append(r.entries, ringEntry{rank: rank, line: line})
	r.bytes += len(line)
	r.cond.Signal()
}

// pop returns the oldest line. It waits while the ring is empty and open,
// and returns false when the ring is closed and empty.
func (r *logRing) pop() (ringEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.entries) == 0 && !r.closed {
		r.cond.Wait()
	}
	if len(r.entries) == 0 {
		return ringEntry{}, false
	}
	e := r.entries[0]
	r.entries[0] = ringEntry{}
	r.entries = r.entries[1:]
	r.bytes -= len(e.line)
	return e, true
}

// drop counts a line that left the ring but did not reach the server.
func (r *logRing) drop(rank int) {
	r.mu.Lock()
	r.dropped[rank]++
	r.mu.Unlock()
}

func (r *logRing) drops() [4]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

func (r *logRing) droppedTotal() int64 {
	d := r.drops()
	return d[0] + d[1] + d[2] + d[3]
}

func (r *logRing) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *logRing) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cond.Broadcast()
}

// remoteSink is the state that a RemoteHandler shares with the handlers
// derived from it by WithAttrs and WithGroup.
type remoteSink struct {
	client       *Client
	ring         *logRing
	flushTimeout time.Duration
	done         chan struct{}
	cancel       context.CancelFunc
	once         sync.Once
	reported     atomic.Int64 // drops already warned about on the console
}

// RemoteHandler is a slog.Handler that ships log lines to the CAPRF /log
// endpoint. It wraps another handler so logs appear both in the console
// and remotely. One sender goroutine ships the lines in order.
type RemoteHandler struct {
	sink   *remoteSink
	inner  slog.Handler
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
}

// NewRemoteHandler creates a handler that sends logs to the CAPRF server.
// The inner handler is used for local console output. bufSize bounds the
// queued lines (default 1000); the queued bytes are bounded to 4 MiB.
// Close waits up to the LOG_FLUSH_TIMEOUT_SEC of the client config.
func NewRemoteHandler(client *Client, inner slog.Handler, level slog.Leveler, bufSize int) *RemoteHandler {
	return newRemoteHandler(client, inner, level, bufSize, defaultBufferBytes)
}

func newRemoteHandler(client *Client, inner slog.Handler, level slog.Leveler, maxLines, maxBytes int) *RemoteHandler {
	if maxLines <= 0 {
		maxLines = 1000
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &RemoteHandler{
		sink: &remoteSink{
			client:       client,
			ring:         newLogRing(maxLines, maxBytes),
			flushTimeout: client.cfg.Transport.LogFlushTimeout(),
			done:         make(chan struct{}),
			cancel:       cancel,
		},
		inner: inner,
		level: level,
	}
	go h.send(ctx)
	return h
}

// Enabled reports whether the handler handles records at the given level.
func (h *RemoteHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// DroppedCount returns the number of log lines that did not reach the
// server: evicted from the full buffer, or failed to ship.
func (h *RemoteHandler) DroppedCount() int64 {
	return h.sink.ring.droppedTotal()
}

// Handle writes the record to the inner handler and queues it for the
// server. Secret values are redacted before the line is queued.
func (h *RemoteHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface requires value receiver
	redactedRecord := redactRecord(&r)
	if err := h.inner.Handle(ctx, redactedRecord); err != nil {
		return err //nolint:wrapcheck // slog.Handler.Handle must return unwrapped errors
	}

	var sb strings.Builder
	sb.WriteString("[")
	sb.WriteString(r.Level.String())
	sb.WriteString("] ")
	sb.WriteString(r.Message)

	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	for _, a := range h.attrs {
		a = redactAttr(a)
		sb.WriteString(" ")
		sb.WriteString(prefix)
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
	}

	redactedRecord.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(prefix)
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})

	h.sink.ring.push(levelRank(r.Level), logging.RedactText(sb.String()))
	h.maybeWarnDropped(h.sink.ring.droppedTotal())
	return nil
}

// maybeWarnDropped emits a console warning every dropWarnEvery drops.
// It uses h.inner directly to avoid re-entering RemoteHandler when it is
// the default slog handler.
func (h *RemoteHandler) maybeWarnDropped(total int64) {
	prev := h.sink.reported.Load()
	if total-prev >= dropWarnEvery {
		if h.sink.reported.CompareAndSwap(prev, total) {
			h.warnViaInner("remote log handler is dropping entries: buffer full",
				slog.Int64("total_dropped", total))
		}
	}
}

// warnViaInner emits a Warn-level record directly through the inner handler,
// bypassing RemoteHandler.Handle to prevent re-entry into the buffer.
func (h *RemoteHandler) warnViaInner(msg string, attrs ...slog.Attr) {
	ctx := context.Background()
	if h.inner == nil || !h.inner.Enabled(ctx, slog.LevelWarn) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:])
	r := slog.NewRecord(time.Now(), slog.LevelWarn, msg, pcs[0])
	r.AddAttrs(attrs...)
	_ = h.inner.Handle(ctx, r)
}

// WithAttrs returns a new handler with the given attributes.
func (h *RemoteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redactedAttrs := redactAttrs(attrs)
	return &RemoteHandler{
		sink:   h.sink,
		inner:  h.inner.WithAttrs(redactedAttrs),
		level:  h.level,
		attrs:  append(append([]slog.Attr{}, h.attrs...), redactedAttrs...),
		groups: h.groups,
	}
}

// WithGroup returns a new handler with the given group name.
func (h *RemoteHandler) WithGroup(name string) slog.Handler {
	return &RemoteHandler{
		sink:   h.sink,
		inner:  h.inner.WithGroup(name),
		level:  h.level,
		attrs:  h.attrs,
		groups: append(append([]string{}, h.groups...), name),
	}
}

// Close stops new lines and flushes the queued lines, the drop summary
// last, before a handoff. It waits up to the flush timeout, then cancels
// the POST in flight and reports the lines left behind on the console.
// Later calls, also through derived handlers, do nothing.
func (h *RemoteHandler) Close() {
	s := h.sink
	s.once.Do(func() {
		if n := s.ring.droppedTotal(); n > 0 {
			h.warnViaInner("remote log handler: entries were dropped during session",
				slog.Int64("total_dropped", n))
		}
		s.ring.close()
		timer := time.NewTimer(s.flushTimeout)
		defer timer.Stop()
		select {
		case <-s.done:
		case <-timer.C:
			s.cancel()
			// The canceled line in flight counts as dropped, not
			// as undelivered.
			h.warnViaInner("remote log flush deadline reached; the canceled line in flight counts as dropped",
				slog.Duration("timeout", s.flushTimeout),
				slog.Int("undelivered", s.ring.len()))
		}
	})
}

// send ships the queued lines in order until the ring is closed and empty
// or the flush deadline cancels ctx. A failed line counts as dropped. The
// first success after drops ships one summary record with the drops per
// level since the previous summary; the end of the flush tries once more.
func (h *RemoteHandler) send(ctx context.Context) {
	defer close(h.sink.done)
	var reported [4]int64
	for ctx.Err() == nil {
		e, ok := h.sink.ring.pop()
		if !ok {
			h.shipDropSummary(ctx, &reported)
			return
		}
		if !h.ship(ctx, e.line) {
			h.sink.ring.drop(e.rank)
			continue
		}
		h.shipDropSummary(ctx, &reported)
	}
}

func (h *RemoteHandler) ship(ctx context.Context, line string) bool {
	shipCtx, cancel := context.WithTimeout(ctx, shipTimeout)
	defer cancel()
	if err := h.sink.client.ShipLog(shipCtx, line); err != nil {
		h.warnViaInner("failed to ship log to caprf", slog.Any("error", err))
		return false
	}
	return true
}

// shipDropSummary ships the drops counted since reported, if any, and
// advances reported when the summary arrived.
func (h *RemoteHandler) shipDropSummary(ctx context.Context, reported *[4]int64) {
	now := h.sink.ring.drops()
	if now == *reported || ctx.Err() != nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("[WARN] remote log handler dropped lines code=" + dropSummaryCode)
	for i, name := range levelNames {
		fmt.Fprintf(&sb, " %s=%d", name, now[i]-reported[i])
	}
	if h.ship(ctx, sb.String()) {
		*reported = now
	}
}
