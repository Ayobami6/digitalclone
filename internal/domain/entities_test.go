package domain_test

import (
	"errors"
	"testing"
	"time"

	"digitalclone/internal/domain"
)

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		input     string
		want      domain.Platform
		expectErr bool
	}{
		{name: "linkedin lowercase", input: "linkedin", want: domain.PlatformLinkedIn},
		{name: "youtube mixed case trimmed", input: "  YouTube ", want: domain.PlatformYouTube},
		{name: "x platform", input: "X", want: domain.PlatformX},
		{name: "tiktok platform", input: "tiktok", want: domain.PlatformTikTok},
		{name: "unsupported platform", input: "mastodon", expectErr: true},
		{name: "empty platform", input: "", expectErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.ParsePlatform(testCase.input)
			if testCase.expectErr {
				if !errors.Is(err, domain.ErrValidationFailed) {
					t.Fatalf("expected ErrValidationFailed, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("got %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestCommitEventValidate(t *testing.T) {
	t.Parallel()

	t.Run("derives deterministic idempotency key and defaults", func(t *testing.T) {
		t.Parallel()
		event := domain.CommitEvent{
			RepoName:  "Ayobami6/digitalclone",
			CommitSHA: "abc1234",
			Message:   "feat: add hexagonal social agent",
		}
		if err := event.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.IdempotencyKey != "Ayobami6/digitalclone:abc1234" {
			t.Fatalf("unexpected idempotency key: %s", event.IdempotencyKey)
		}
		if event.Branch != "main" {
			t.Fatalf("expected default branch main, got %s", event.Branch)
		}
		if event.Timestamp.IsZero() {
			t.Fatalf("expected non-zero timestamp")
		}
	})

	t.Run("rejects missing commit sha", func(t *testing.T) {
		t.Parallel()
		event := domain.CommitEvent{Message: "feat: something"}
		if err := event.Validate(); !errors.Is(err, domain.ErrValidationFailed) {
			t.Fatalf("expected ErrValidationFailed, got %v", err)
		}
	})

	t.Run("rejects missing commit message", func(t *testing.T) {
		t.Parallel()
		event := domain.CommitEvent{CommitSHA: "abc1234"}
		if err := event.Validate(); !errors.Is(err, domain.ErrValidationFailed) {
			t.Fatalf("expected ErrValidationFailed, got %v", err)
		}
	})
}

func TestContentDraftStateMachine(t *testing.T) {
	t.Parallel()

	newDraft := func(status domain.DraftStatus) *domain.ContentDraft {
		now := time.Now().UTC()
		return &domain.ContentDraft{
			ID:        "draft-1",
			BatchID:   "batch-1",
			CommitSHA: "deadbeef",
			Platform:  domain.PlatformLinkedIn,
			Title:     "Initial",
			BodyText:  "Initial body",
			Status:    status,
			Version:   1,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	t.Run("happy path draft -> critiqued -> pending_review -> approved -> published", func(t *testing.T) {
		t.Parallel()
		draft := newDraft(domain.StatusDraft)

		if err := draft.ApplyCritique("Refined Title", "Refined Body", "Tightened hook", []string{"golang", "adk"}); err != nil {
			t.Fatalf("ApplyCritique failed: %v", err)
		}
		if draft.Status != domain.StatusCritiqued || draft.Version != 2 {
			t.Fatalf("unexpected status/version after critique: %s/%d", draft.Status, draft.Version)
		}

		if err := draft.MarkPendingReview(); err != nil {
			t.Fatalf("MarkPendingReview failed: %v", err)
		}
		if draft.Status != domain.StatusPendingReview {
			t.Fatalf("expected PENDING_REVIEW, got %s", draft.Status)
		}

		if err := draft.Approve(); err != nil {
			t.Fatalf("Approve failed: %v", err)
		}
		if draft.Status != domain.StatusApproved {
			t.Fatalf("expected APPROVED, got %s", draft.Status)
		}

		if err := draft.MarkPublished("buf_post_99"); err != nil {
			t.Fatalf("MarkPublished failed: %v", err)
		}
		if draft.Status != domain.StatusPublished || draft.ExternalPostID != "buf_post_99" {
			t.Fatalf("unexpected published draft state: %+v", draft)
		}
		if !draft.Status.IsTerminal() {
			t.Fatalf("expected PUBLISHED to be terminal")
		}
	})

	t.Run("reject transitions from pending_review and blocks double approve", func(t *testing.T) {
		t.Parallel()
		draft := newDraft(domain.StatusPendingReview)

		if err := draft.Reject(); err != nil {
			t.Fatalf("Reject failed: %v", err)
		}
		if draft.Status != domain.StatusRejected || !draft.Status.IsTerminal() {
			t.Fatalf("expected terminal REJECTED status, got %s", draft.Status)
		}
		if err := draft.Approve(); !errors.Is(err, domain.ErrInvalidStateTransition) {
			t.Fatalf("expected ErrInvalidStateTransition on approve after reject, got %v", err)
		}
	})

	t.Run("blocks publishing unapproved draft", func(t *testing.T) {
		t.Parallel()
		draft := newDraft(domain.StatusPendingReview)
		if err := draft.MarkPublished("post-1"); !errors.Is(err, domain.ErrInvalidStateTransition) {
			t.Fatalf("expected ErrInvalidStateTransition, got %v", err)
		}
	})

	t.Run("allows retrying failed draft via approve and clears failure reason", func(t *testing.T) {
		t.Parallel()
		draft := newDraft(domain.StatusPendingReview)
		if err := draft.Approve(); err != nil {
			t.Fatalf("Approve failed: %v", err)
		}
		if err := draft.MarkFailed("buffer timeout"); err != nil {
			t.Fatalf("MarkFailed failed: %v", err)
		}
		if !draft.CanTransitionTo(domain.StatusApproved) {
			t.Fatalf("expected StatusFailed to allow transition to StatusApproved")
		}
		if err := draft.Approve(); err != nil {
			t.Fatalf("Retry Approve from FAILED failed: %v", err)
		}
		if draft.Status != domain.StatusApproved || draft.FailureReason != "" {
			t.Fatalf("expected APPROVED with cleared failure reason, got status=%s reason=%q", draft.Status, draft.FailureReason)
		}
	})
}
