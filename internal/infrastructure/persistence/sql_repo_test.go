package persistence_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/persistence"
)

func TestSQLiteRepositoryBatchAndAtomicLocking(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, err := persistence.NewSQLiteRepository(ctx, filepath.Join(t.TempDir(), "digitalclone.db"))
	if err != nil {
		t.Fatalf("NewSQLiteRepository failed: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	batch, err := domain.NewDraftBatch("batch-100", domain.CommitEvent{
		RepoName:     "Ayobami6/digitalclone",
		Branch:       "feat/agent",
		CommitSHA:    "abcdef123456",
		Author:       "Ayobami",
		Message:      "feat: add multi-agent workflow",
		ChangedFiles: []string{"cmd/main.go"},
		DiffPatch:    "+func main() {}",
	})
	if err != nil {
		t.Fatalf("NewDraftBatch failed: %v", err)
	}

	created, err := repo.CreateBatchIfAbsent(ctx, batch)
	if err != nil || !created {
		t.Fatalf("expected first batch insert created=true, got created=%v err=%v", created, err)
	}

	createdAgain, err := repo.CreateBatchIfAbsent(ctx, batch)
	if err != nil || createdAgain {
		t.Fatalf("expected duplicate batch insert created=false, got created=%v err=%v", createdAgain, err)
	}

	now := time.Now().UTC()
	draft := &domain.ContentDraft{
		ID:        "draft-li-1",
		BatchID:   batch.ID,
		CommitSHA: batch.CommitEvent.CommitSHA,
		Platform:  domain.PlatformLinkedIn,
		Title:     "Engineering Update",
		BodyText:  "Initial LinkedIn copy",
		Tags:      []string{"go", "adk"},
		MediaAsset: &domain.MediaAsset{
			ID:        "img-1",
			MediaType: domain.MediaImage,
			MimeType:  "image/png",
			PublicURL: "https://cdn.example.com/img-1.png",
			SizeBytes: 1024,
		},
		Status:    domain.StatusDraft,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.SaveDraft(ctx, draft); err != nil {
		t.Fatalf("SaveDraft failed: %v", err)
	}

	critiqued, err := repo.UpdateDraftContent(ctx, batch.ID, domain.PlatformLinkedIn, "Refined Engineering Update", "Sharper hook + code insight", "Improved hook", []string{"golang"})
	if err != nil {
		t.Fatalf("UpdateDraftContent failed: %v", err)
	}
	if critiqued.Status != domain.StatusCritiqued {
		t.Fatalf("expected CRITIQUED, got %s", critiqued.Status)
	}

	if _, ok, err := repo.TransitionDraftStatusAtomically(ctx, batch.ID, domain.PlatformLinkedIn, domain.StatusPendingReview); err != nil || !ok {
		t.Fatalf("expected transition to PENDING_REVIEW, got ok=%v err=%v", ok, err)
	}

	// Race 10 concurrent approvals; strictly 1 must win.
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, transitioned, transErr := repo.TransitionDraftStatusAtomically(ctx, batch.ID, domain.PlatformLinkedIn, domain.StatusApproved)
			if transErr != nil {
				t.Errorf("concurrent transition error: %v", transErr)
				return
			}
			if transitioned {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if winners.Load() != 1 {
		t.Fatalf("expected exactly 1 atomic approval winner, got %d", winners.Load())
	}

	if err := repo.RecordPublishedPost(ctx, draft.ID, domain.StatusPublished, "buffer_post_123", ""); err != nil {
		t.Fatalf("RecordPublishedPost failed: %v", err)
	}
	reloadedBatch, err := repo.GetBatch(ctx, batch.ID)
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}
	if reloadedBatch.Drafts[domain.PlatformLinkedIn].ExternalPostID != "buffer_post_123" {
		t.Fatalf("expected external post id recorded, got %+v", reloadedBatch.Drafts[domain.PlatformLinkedIn])
	}
}
