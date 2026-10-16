//go:build linux

package provision

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRetry_Success(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 3, InitialDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithRetry_EventualSuccess(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:   3,
		InitialDelay: time.Millisecond,
		MaxDelay:     10 * time.Millisecond,
		Transient:    true,
	}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_Exhausted(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:   2,
		InitialDelay: time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		Transient:    true,
	}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		return errors.New("always fails")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 3 { // initial + 2 retries
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_PermanentError(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:   5,
		InitialDelay: time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		Transient:    false,
	}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		return &PermanentError{Err: errors.New("disk gone")}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on permanent), got %d", calls)
	}
}

func TestWithRetry_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	policy := RetryPolicy{
		MaxRetries:   10,
		InitialDelay: 0,
		MaxDelay:     0,
		Transient:    true,
	}
	calls := 0
	err := WithRetry(ctx, "test-step", policy, func(_ context.Context) error {
		calls++
		if calls == 1 {
			cancel()
		}
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call after cancellation, got %d", calls)
	}
}

func TestWithRetry_NoRetry(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 0}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry), got %d", calls)
	}
}

func TestBackoffDelay(t *testing.T) {
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     time.Second,
		Jitter:       0.0,
	}
	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, time.Second}, // capped
	}
	for _, tc := range tests {
		got := backoffDelay(policy, tc.attempt)
		if got != tc.expected {
			t.Errorf("backoffDelay(attempt=%d) = %v, want %v", tc.attempt, got, tc.expected)
		}
	}
}

func TestBackoffDelay_WithJitter(t *testing.T) {
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     time.Second,
		Jitter:       0.5,
	}
	base := 100 * time.Millisecond
	delay := backoffDelay(policy, 1)
	maxJitter := time.Duration(float64(base) * 0.5)
	if delay < base || delay > base+maxJitter {
		t.Errorf("delay %v out of jitter range [%v, %v]", delay, base, base+maxJitter)
	}
}

func TestDefaultPolicies(t *testing.T) {
	required := []string{
		"report-init", "configure-dns", "stream-image",
		"setup-raid", "detect-disk", "partprobe", "report-success",
		"create-efi-boot-entry", "create-mirror-efi-boot-entry",
	}
	for _, name := range required {
		if _, ok := DefaultPolicies[name]; !ok {
			t.Errorf("missing default policy for step %q", name)
		}
	}
}

func TestWithRetry_NonTransientFailure(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:   5,
		InitialDelay: time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		Transient:    false, // errors NOT assumed transient
	}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		return errors.New("regular error (non-typed)")
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on non-transient), got %d", calls)
	}
}

func TestNormalizePolicy(t *testing.T) {
	tests := []struct {
		name   string
		input  RetryPolicy
		expect RetryPolicy
	}{
		{
			name:   "clamps negative MaxRetries",
			input:  RetryPolicy{MaxRetries: -5, InitialDelay: time.Second, MaxDelay: time.Second, Jitter: 0.5},
			expect: RetryPolicy{MaxRetries: 0, InitialDelay: time.Second, MaxDelay: time.Second, Jitter: 0.5},
		},
		{
			name:   "clamps negative InitialDelay",
			input:  RetryPolicy{MaxRetries: 1, InitialDelay: -time.Second, MaxDelay: time.Second, Jitter: 0.1},
			expect: RetryPolicy{MaxRetries: 1, InitialDelay: 0, MaxDelay: time.Second, Jitter: 0.1},
		},
		{
			name:   "clamps negative MaxDelay",
			input:  RetryPolicy{MaxRetries: 1, InitialDelay: time.Second, MaxDelay: -time.Second, Jitter: 0.1},
			expect: RetryPolicy{MaxRetries: 1, InitialDelay: time.Second, MaxDelay: 0, Jitter: 0.1},
		},
		{
			name:   "clamps jitter below 0",
			input:  RetryPolicy{Jitter: -0.5},
			expect: RetryPolicy{Jitter: 0},
		},
		{
			name:   "clamps jitter above 1",
			input:  RetryPolicy{Jitter: 2.0},
			expect: RetryPolicy{Jitter: 1.0},
		},
		{
			name:   "valid values unchanged",
			input:  RetryPolicy{MaxRetries: 3, InitialDelay: time.Second, MaxDelay: 10 * time.Second, Jitter: 0.3},
			expect: RetryPolicy{MaxRetries: 3, InitialDelay: time.Second, MaxDelay: 10 * time.Second, Jitter: 0.3},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePolicy(tc.input)
			if got != tc.expect {
				t.Errorf("normalizePolicy() = %+v, want %+v", got, tc.expect)
			}
		})
	}
}

func TestWithRetry_TransientErrorType(t *testing.T) {
	// When fn returns a TransientError, retry should happen even with
	// policy.Transient=false because isTransient(err) returns true.
	policy := RetryPolicy{
		MaxRetries:   3,
		InitialDelay: time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		Transient:    false,
	}
	calls := 0
	err := WithRetry(context.Background(), "test-step", policy, func(_ context.Context) error {
		calls++
		if calls < 3 {
			return &TransientError{Err: errors.New("temporary issue")}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (retried TransientError), got %d", calls)
	}
}
