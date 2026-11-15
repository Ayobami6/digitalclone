// Package resilience provides fault-tolerance primitives including circuit breakers
// and exponential backoff with full jitter for outbound service adapters.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"digitalclone/internal/domain"
)

// CircuitState represents the current operating state of a CircuitBreaker.
type CircuitState string

const (
	// StateClosed permits all requests to flow through normally.
	StateClosed CircuitState = "CLOSED"
	// StateOpen short-circuits all requests immediately with domain.ErrCircuitOpen.
	StateOpen CircuitState = "OPEN"
	// StateHalfOpen permits a single probe request after ResetTimeout elapses.
	StateHalfOpen CircuitState = "HALF_OPEN"
)

// CircuitBreakerConfig configures failure thresholds and recovery timing.
type CircuitBreakerConfig struct {
	Name         string
	MaxFailures  int
	ResetTimeout time.Duration
	Now          func() time.Time
}

// CircuitBreaker protects outbound dependencies from cascading failure storms.
type CircuitBreaker struct {
	mu               sync.Mutex
	name             string
	maxFailures      int
	resetTimeout     time.Duration
	now              func() time.Time
	state            CircuitState
	failureCount     int
	openedAt         time.Time
	halfOpenInFlight bool
}

// NewCircuitBreaker constructs a CircuitBreaker with sensible defaults.
func NewCircuitBreaker(config CircuitBreakerConfig) *CircuitBreaker {
	maxFailures := config.MaxFailures
	if maxFailures <= 0 {
		maxFailures = 5
	}
	resetTimeout := config.ResetTimeout
	if resetTimeout <= 0 {
		resetTimeout = 30 * time.Second
	}
	nowFunc := config.Now
	if nowFunc == nil {
		nowFunc = time.Now
	}
	name := config.Name
	if name == "" {
		name = "default"
	}
	return &CircuitBreaker{
		name:         name,
		maxFailures:  maxFailures,
		resetTimeout: resetTimeout,
		now:          nowFunc,
		state:        StateClosed,
	}
}

// State returns the current circuit state, reflecting elapsed ResetTimeout transitions.
func (breaker *CircuitBreaker) State() CircuitState {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	if breaker.state == StateOpen && breaker.now().Sub(breaker.openedAt) >= breaker.resetTimeout {
		return StateHalfOpen
	}
	return breaker.state
}

// Execute invokes operation inside the circuit breaker guard.
func (breaker *CircuitBreaker) Execute(ctx context.Context, operation func(context.Context) error) error {
	if err := breaker.beforeCall(); err != nil {
		return err
	}

	err := operation(ctx)
	breaker.afterCall(err)
	return err
}

func (breaker *CircuitBreaker) beforeCall() error {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	switch breaker.state {
	case StateClosed:
		return nil
	case StateOpen:
		if breaker.now().Sub(breaker.openedAt) < breaker.resetTimeout {
			return fmt.Errorf("%w: %s", domain.ErrCircuitOpen, breaker.name)
		}
		// Allow exactly one probe call when transitioning from Open to Half-Open.
		breaker.state = StateHalfOpen
		breaker.halfOpenInFlight = true
		return nil
	case StateHalfOpen:
		if breaker.halfOpenInFlight {
			return fmt.Errorf("%w: %s probe in flight", domain.ErrCircuitOpen, breaker.name)
		}
		breaker.halfOpenInFlight = true
		return nil
	default:
		return nil
	}
}

func (breaker *CircuitBreaker) afterCall(err error) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	if breaker.state == StateHalfOpen {
		breaker.halfOpenInFlight = false
	}

	// Caller-initiated cancellations should not trip downstream outage breakers.
	if err == nil || errors.Is(err, context.Canceled) {
		if err == nil {
			breaker.state = StateClosed
			breaker.failureCount = 0
		}
		return
	}

	if breaker.state == StateHalfOpen {
		breaker.state = StateOpen
		breaker.openedAt = breaker.now()
		return
	}

	breaker.failureCount++
	if breaker.failureCount >= breaker.maxFailures {
		breaker.state = StateOpen
		breaker.openedAt = breaker.now()
	}
}
