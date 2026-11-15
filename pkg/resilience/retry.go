package resilience

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"
)

// RetryAfterError wraps a transient error with an explicit server-supplied backoff delay
// (such as an HTTP 429 Retry-After header).
type RetryAfterError struct {
	Delay time.Duration
	Err   error
}

// Error formats the wrapped error and retry delay.
func (retryErr *RetryAfterError) Error() string {
	if retryErr.Err == nil {
		return "rate limited"
	}
	return retryErr.Err.Error()
}

// Unwrap returns the underlying error so callers can match sentinel errors via errors.Is.
func (retryErr *RetryAfterError) Unwrap() error {
	return retryErr.Err
}

// RetryConfig configures bounded retry execution with exponential backoff and full jitter.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	ShouldRetry func(error) bool
}

// RetryWithBackoff runs operation up to MaxAttempts times, backing off with full jitter
// or honoring RetryAfterError delays while respecting context cancellation.
func RetryWithBackoff(ctx context.Context, config RetryConfig, operation func(context.Context) error) error {
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	baseDelay := config.BaseDelay
	if baseDelay <= 0 {
		baseDelay = 200 * time.Millisecond
	}
	maxDelay := config.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 10 * time.Second
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		lastErr = operation(ctx)
		if lastErr == nil {
			return nil
		}

		if config.ShouldRetry != nil && !config.ShouldRetry(lastErr) {
			return lastErr
		}
		if attempt == maxAttempts-1 {
			return lastErr
		}

		sleepDuration := computeFullJitterDelay(attempt, baseDelay, maxDelay)
		var retryAfter *RetryAfterError
		if errors.As(lastErr, &retryAfter) && retryAfter.Delay > 0 {
			sleepDuration = retryAfter.Delay
			if sleepDuration > maxDelay {
				sleepDuration = maxDelay
			}
		}

		timer := time.NewTimer(sleepDuration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func computeFullJitterDelay(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	// Cap exponent shift to prevent signed duration overflow across large attempt counts.
	shift := attempt
	if shift > 20 {
		shift = 20
	}
	ceiling := baseDelay << shift
	if ceiling <= 0 || ceiling > maxDelay {
		ceiling = maxDelay
	}
	if ceiling <= 1 {
		return ceiling
	}
	randomOffset, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)))
	if err != nil {
		return ceiling / 2
	}
	return time.Duration(randomOffset.Int64())
}
