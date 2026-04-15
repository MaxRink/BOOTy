package caprf

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

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

// RemoteHandler is a slog.Handler that ships log lines to the CAPRF /log endpoint.
// It wraps another handler so logs appear both in the console and remotely.
type RemoteHandler struct {
	client  *Client
	inner   slog.Handler
	level   slog.Leveler
	buf     chan string
	attrs   []slog.Attr
	groups  []string
	done    chan struct{}
	once    *sync.Once
	cancel  context.CancelFunc
	closing *atomic.Bool
}

// NewRemoteHandler creates a handler that sends logs to the CAPRF server.
// The inner handler is used for local console output. Buffer capacity controls
// how many log lines can be queued before dropping.
func NewRemoteHandler(client *Client, inner slog.Handler, level slog.Leveler, bufSize int) *RemoteHandler {
	if bufSize <= 0 {
		bufSize = 1000
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &RemoteHandler{
		client:  client,
		inner:   inner,
		level:   level,
		buf:     make(chan string, bufSize),
		done:    make(chan struct{}),
		once:    &sync.Once{},
		cancel:  cancel,
		closing: &atomic.Bool{},
	}
	go h.drain(ctx)
	return h
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

// Enabled reports whether the handler handles records at the given level.
func (h *RemoteHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle sends the log record to both the inner handler and the remote buffer.
func (h *RemoteHandler) Handle(ctx context.Context, r slog.Record) error { //nolint:gocritic // slog.Handler interface requires value receiver
	if err := h.inner.Handle(ctx, r); err != nil {
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

	r.Attrs(func(a slog.Attr) bool {
		a = redactAttr(a)
		sb.WriteString(" ")
		sb.WriteString(prefix)
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})

	// Guard against sending on a closed channel: if Close() has already been
	// called (or is in progress), skip the send to avoid a panic.
	if h.closing == nil || !h.closing.Load() {
		select {
		case h.buf <- sb.String():
		default:
		}
	}

	return nil
}

// WithAttrs returns a new handler with the given attributes.
func (h *RemoteHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RemoteHandler{
		client:  h.client,
		inner:   h.inner.WithAttrs(attrs),
		level:   h.level,
		buf:     h.buf,
		attrs:   append(append([]slog.Attr{}, h.attrs...), attrs...),
		groups:  h.groups,
		done:    h.done,
		once:    h.once,
		cancel:  h.cancel,
		closing: h.closing,
	}
}

// WithGroup returns a new handler with the given group name.
func (h *RemoteHandler) WithGroup(name string) slog.Handler {
	return &RemoteHandler{
		client:  h.client,
		inner:   h.inner.WithGroup(name),
		level:   h.level,
		buf:     h.buf,
		attrs:   h.attrs,
		groups:  append(append([]string{}, h.groups...), name),
		done:    h.done,
		once:    h.once,
		cancel:  h.cancel,
		closing: h.closing,
	}
}

// Close stops the background drain goroutine and flushes remaining logs.
// It closes the buffer to signal drain to finish, then waits up to 5s.
// If drain does not finish in time, the context is canceled to abort
// any in-flight HTTP requests.
func (h *RemoteHandler) Close() {
	if h.once == nil {
		h.once = &sync.Once{}
	}
	h.once.Do(func() {
		if h.closing != nil {
			h.closing.Store(true)
		}
		close(h.buf)
		select {
		case <-h.done:
		case <-time.After(5 * time.Second): //nolint:mnd // fixed drain timeout
			slog.Warn("log handler drain timed out after 5s")
			if h.cancel != nil {
				h.cancel()
			}
		}
	})
}

func (h *RemoteHandler) drain(ctx context.Context) {
	defer close(h.done)
	for msg := range h.buf {
		if err := h.client.ShipLog(ctx, msg); err != nil {
			slog.Warn("failed to ship log to caprf", "error", err)
		}
	}
}
