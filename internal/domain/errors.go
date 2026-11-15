// Package domain defines pure domain entities, value objects, state invariants,
// and outbound/inbound secondary port interfaces for DigitalClone.
package domain

import "errors"

var (
	// ErrDraftNotFound indicates that the requested platform draft does not exist.
	ErrDraftNotFound = errors.New("content draft not found")

	// ErrBatchNotFound indicates that the requested commit draft batch does not exist.
	ErrBatchNotFound = errors.New("draft batch not found")

	// ErrInvalidStateTransition indicates a violation of the draft approval or publishing lifecycle.
	ErrInvalidStateTransition = errors.New("invalid draft state transition")

	// ErrDuplicateCommitTrigger indicates that a commit push with the same idempotency key was already processed.
	ErrDuplicateCommitTrigger = errors.New("duplicate commit trigger")

	// ErrRateLimited indicates that an upstream API rejected the request due to rate limiting.
	ErrRateLimited = errors.New("upstream rate limit exceeded")

	// ErrCircuitOpen indicates that an adapter circuit breaker is currently open and rejecting calls.
	ErrCircuitOpen = errors.New("circuit breaker is open")

	// ErrInvalidSignature indicates an invalid HMAC or Ed25519 webhook signature.
	ErrInvalidSignature = errors.New("invalid webhook signature")

	// ErrValidationFailed indicates malformed or incomplete domain input data.
	ErrValidationFailed = errors.New("domain validation failed")
)
