//go:build linux

package provision

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// RetryPolicy defines how a provisioning step handles failures.
type RetryPolicy struct {
	MaxRetries   int           // 0 = no retry, total attempts = MaxRetries+1
	InitialDelay time.Duration // base delay before first retry
	MaxDelay     time.Duration // cap on exponential backoff
	Jitter       float64       // 0.0-1.0, random delay fraction
	Transient    bool          // if true, errors are assumed transient
}

// DefaultPolicies maps step names to their retry policies.
var DefaultPolicies = map[string]RetryPolicy{
	"report-init":           {MaxRetries: 5, InitialDelay: 2 * time.Second, MaxDelay: 30 * time.Second, Jitter: 0.2, Transient: true},
	"configure-dns":         {MaxRetries: 5, InitialDelay: 1 * time.Second, MaxDelay: 15 * time.Second, Jitter: 0.1, Transient: true},
	"stream-image":          {MaxRetries: 3, InitialDelay: 5 * time.Second, MaxDelay: 60 * time.Second, Jitter: 0.3, Transient: true},
	"setup-raid":            {MaxRetries: 2, InitialDelay: 2 * time.Second, MaxDelay: 10 * time.Second, Jitter: 0.1, Transient: true},
	"detect-disk":           {MaxRetries: 3, InitialDelay: 2 * time.Second, MaxDelay: 10 * time.Second, Jitter: 0.1, Transient: true},
	"partprobe":             {MaxRetries: 3, InitialDelay: 1 * time.Second, MaxDelay: 5 * time.Second, Jitter: 0.0, Transient: true},
	"report-success":        {MaxRetries: 5, InitialDelay: 2 * time.Second, MaxDelay: 30 * time.Second, Jitter: 0.2, Transient: true},
	"create-efi-boot-entry": {MaxRetries: 2, InitialDelay: 1 * time.Second, MaxDelay: 5 * time.Second, Jitter: 0.0, Transient: true},
	// The second disk boot entry uses efibootmgr like create-efi-boot-entry.
	"create-mirror-efi-boot-entry": {MaxRetries: 2, InitialDelay: 1 * time.Second, MaxDelay: 5 * time.Second, Jitter: 0.0, Transient: true},
}

// WithRetry executes fn with the given retry policy.
func WithRetry(ctx context.Context, name string, policy RetryPolicy, fn func(ctx context.Context) error) error {
	policy = normalizePolicy(policy)

	var lastErr error
	for attempt := range policy.MaxRetries + 1 {
		if err := retryCanceledErr(ctx); err != nil {
			return err
		}
		if err := waitRetryDelay(ctx, name, policy, attempt); err != nil {
			return err
		}
		if err := retryCanceledErr(ctx); err != nil {
			return err
		}

		if err := fn(ctx); err != nil {
			lastErr = err
			if shouldRetry, classifyErr := classifyRetryError(err, policy); !shouldRetry {
				return classifyErr
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("exhausted %d retries: %w", policy.MaxRetries, lastErr)
}

func normalizePolicy(policy RetryPolicy) RetryPolicy {
	if policy.MaxRetries < 0 {
		policy.MaxRetries = 0
	}
	if policy.InitialDelay < 0 {
		policy.InitialDelay = 0
	}
	if policy.MaxDelay < 0 {
		policy.MaxDelay = 0
	}
	if policy.Jitter < 0 {
		policy.Jitter = 0
	}
	if policy.Jitter > 1 {
		policy.Jitter = 1
	}
	return policy
}

func retryCanceledErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("retry canceled: %w", err)
	}
	return nil
}

func waitRetryDelay(ctx context.Context, name string, policy RetryPolicy, attempt int) error {
	if attempt == 0 {
		return nil
	}

	delay := backoffDelay(policy, attempt)
	if delay <= 0 {
		slog.Debug("retrying step immediately", "step", name, "attempt", attempt)
		return nil
	}
	slog.Warn("retrying step", "step", name, "attempt", attempt, "delay", delay)

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("retry canceled: %w", ctx.Err())
	}
}

func classifyRetryError(err error, policy RetryPolicy) (bool, error) {
	if isPermanent(err) {
		return false, fmt.Errorf("permanent failure: %w", err)
	}
	if !isTransient(err) && !policy.Transient {
		return false, fmt.Errorf("non-transient failure: %w", err)
	}
	return true, nil
}

func backoffDelay(policy RetryPolicy, attempt int) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)

	delay := policy.InitialDelay
	if delay < 0 {
		delay = 0
	}

	for i := 1; i < attempt; i++ {
		if delay > maxDuration/2 {
			delay = maxDuration
		} else {
			delay *= 2
		}
		if policy.MaxDelay > 0 && delay >= policy.MaxDelay {
			delay = policy.MaxDelay
			break
		}
	}

	if policy.MaxDelay > 0 && delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	if policy.Jitter > 0 {
		jitter := time.Duration(float64(delay) * policy.Jitter * rand.Float64()) //nolint:gosec // jitter does not need crypto-grade randomness
		delay += jitter
	}
	return delay
}
