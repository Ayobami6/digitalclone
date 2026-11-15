package resilience_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/pkg/resilience"
)

func TestCircuitBreakerLifecycle(t *testing.T) {
	t.Parallel()

	currentTime := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	breaker := resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
		Name:         "buffer-api",
		MaxFailures:  2,
		ResetTimeout: 10 * time.Second,
		Now:          func() time.Time { return currentTime },
	})

	downstreamErr := errors.New("upstream 503")
	ctx := context.Background()

	if err := breaker.Execute(ctx, func(context.Context) error { return downstreamErr }); !errors.Is(err, downstreamErr) {
		t.Fatalf("expected downstreamErr, got %v", err)
	}
	if breaker.State() != resilience.StateClosed {
		t.Fatalf("expected CLOSED after 1 failure, got %s", breaker.State())
	}

	if err := breaker.Execute(ctx, func(context.Context) error { return downstreamErr }); !errors.Is(err, downstreamErr) {
		t.Fatalf("expected downstreamErr, got %v", err)
	}
	if breaker.State() != resilience.StateOpen {
		t.Fatalf("expected OPEN after 2 failures, got %s", breaker.State())
	}

	// Open breaker must reject without invoking operation.
	called := false
	err := breaker.Execute(ctx, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, domain.ErrCircuitOpen) || called {
		t.Fatalf("expected ErrCircuitOpen without calling operation, got err=%v called=%v", err, called)
	}

	// Advance clock past ResetTimeout -> HalfOpen -> probe succeeds -> Closed.
	currentTime = currentTime.Add(11 * time.Second)
	if breaker.State() != resilience.StateHalfOpen {
		t.Fatalf("expected HALF_OPEN after ResetTimeout, got %s", breaker.State())
	}

	if err := breaker.Execute(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("expected probe call to succeed, got %v", err)
	}
	if breaker.State() != resilience.StateClosed {
		t.Fatalf("expected CLOSED after successful probe, got %s", breaker.State())
	}
}

func TestRetryWithBackoff(t *testing.T) {
	t.Parallel()

	t.Run("retries transient error and honors RetryAfterError", func(t *testing.T) {
		t.Parallel()
		var attempts atomic.Int32
		err := resilience.RetryWithBackoff(context.Background(), resilience.RetryConfig{
			MaxAttempts: 3,
			BaseDelay:   1 * time.Millisecond,
			MaxDelay:    20 * time.Millisecond,
			ShouldRetry: func(err error) bool {
				return errors.Is(err, domain.ErrRateLimited)
			},
		}, func(context.Context) error {
			current := attempts.Add(1)
			if current < 3 {
				return &resilience.RetryAfterError{
					Delay: 2 * time.Millisecond,
					Err:   domain.ErrRateLimited,
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("expected eventual success, got %v", err)
		}
		if attempts.Load() != 3 {
			t.Fatalf("expected 3 attempts, got %d", attempts.Load())
		}
	})

	t.Run("stops immediately on non-retryable error", func(t *testing.T) {
		t.Parallel()
		var attempts atomic.Int32
		permanentErr := errors.New("bad request")
		err := resilience.RetryWithBackoff(context.Background(), resilience.RetryConfig{
			MaxAttempts: 5,
			BaseDelay:   1 * time.Millisecond,
			MaxDelay:    5 * time.Millisecond,
			ShouldRetry: func(err error) bool { return false },
		}, func(context.Context) error {
			attempts.Add(1)
			return permanentErr
		})
		if !errors.Is(err, permanentErr) {
			t.Fatalf("expected permanentErr, got %v", err)
		}
		if attempts.Load() != 1 {
			t.Fatalf("expected 1 attempt, got %d", attempts.Load())
		}
	})
}
