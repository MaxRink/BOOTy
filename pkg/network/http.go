package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/telekom/t-caas-go-library/pkg/redact"
)

// WaitForHTTP polls target with HTTP HEAD until reachable.
func WaitForHTTP(ctx context.Context, target string, timeout time.Duration) error {
	if target == "" {
		return fmt.Errorf("empty connectivity target URL")
	}

	if timeout <= 0 {
		return fmt.Errorf("network connectivity timeout must be positive, got %s", timeout)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := NewHTTPClient(10 * time.Second)
	attempt := 0
	retryTicker := time.NewTicker(1 * time.Second)
	defer retryTicker.Stop()
	logTarget := RedactHTTPURLForLog(target)

	for {
		if err := waitCtx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("network connectivity timeout after %s (%d attempts): %w", timeout, attempt, err)
			}
			return fmt.Errorf("context canceled: %w", err)
		}

		attempt++
		req, err := http.NewRequestWithContext(waitCtx, http.MethodHead, target, http.NoBody)
		if err != nil {
			return fmt.Errorf("create request for %s: %w", logTarget, &redactedHTTPError{rawURL: target, err: err})
		}

		resp, err := client.Do(req) //nolint:gosec // target is from trusted config
		if err == nil {
			_ = resp.Body.Close()
			// Any HTTP response proves the network path works. The server
			// may return 401 (auth required) or other non-2xx codes, but
			// that still means connectivity is established.
			slog.Info("network connectivity established", "target", logTarget, "status", resp.StatusCode, "attempt", attempt)
			return nil
		}

		slog.Debug("connectivity check failed", "target", logTarget, "attempt", attempt, "error", redactHTTPErrorForLog(err, target))

		select {
		case <-waitCtx.Done():
			err := waitCtx.Err()
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("network connectivity timeout after %s (%d attempts): %w", timeout, attempt, err)
			}
			return fmt.Errorf("context canceled: %w", err)
		case <-retryTicker.C:
		}
	}
}

// RedactHTTPURLForLog strips credentials, query parameters, and fragments from
// HTTP connectivity target URLs before they are written to logs.
func RedactHTTPURLForLog(rawURL string) string {
	return redact.URL(rawURL)
}

func redactHTTPErrorForLog(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", RedactHTTPURLForLog(rawURL), redact.Wrap(err, rawURL))
}

type redactedHTTPError struct {
	rawURL string
	err    error
}

func (e *redactedHTTPError) Error() string {
	return redactHTTPErrorForLog(e.err, e.rawURL)
}

func (e *redactedHTTPError) Unwrap() error {
	return e.err
}
