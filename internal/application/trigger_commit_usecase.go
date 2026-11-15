package application

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"digitalclone/internal/domain"
)

// PipelineRunner defines the abstraction over ADK multi-agent content generation and critique.
type PipelineRunner interface {
	RunPipeline(ctx context.Context, batch *domain.DraftBatch) error
}

// TriggerCommitPipelineUseCase orchestrates commit validation, idempotent batch creation,
// ADK parallel generation + critique, and interactive Discord/Telegram review dispatch.
type TriggerCommitPipelineUseCase struct {
	draftRepo    domain.DraftRepository
	orchestrator PipelineRunner
	notifiers    []domain.ReviewNotifier
	logger       *slog.Logger
}

// NewTriggerCommitPipelineUseCase constructs a TriggerCommitPipelineUseCase.
func NewTriggerCommitPipelineUseCase(
	draftRepo domain.DraftRepository,
	orchestrator PipelineRunner,
	notifiers []domain.ReviewNotifier,
	logger *slog.Logger,
) *TriggerCommitPipelineUseCase {
	if logger == nil {
		logger = slog.Default()
	}
	return &TriggerCommitPipelineUseCase{
		draftRepo:    draftRepo,
		orchestrator: orchestrator,
		notifiers:    append([]domain.ReviewNotifier(nil), notifiers...),
		logger:       logger,
	}
}

// Execute runs the full content creation & review-dispatch workflow for commitEvent.
func (useCase *TriggerCommitPipelineUseCase) Execute(ctx context.Context, commitEvent domain.CommitEvent) (*domain.DraftBatch, error) {
	batch, err := domain.NewDraftBatch(uuid.NewString(), commitEvent)
	if err != nil {
		return nil, err
	}

	created, err := useCase.draftRepo.CreateBatchIfAbsent(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("create draft batch: %w", err)
	}
	if !created {
		return nil, fmt.Errorf("%w: key %s", domain.ErrDuplicateCommitTrigger, batch.CommitEvent.IdempotencyKey)
	}

	if err := useCase.orchestrator.RunPipeline(ctx, batch); err != nil {
		useCase.cleanupFailedBatch(batch.ID)
		return nil, fmt.Errorf("run multi-agent pipeline: %w", err)
	}

	loadedBatch, err := useCase.draftRepo.GetBatch(ctx, batch.ID)
	if err != nil {
		return nil, fmt.Errorf("reload batch after generation: %w", err)
	}

	for _, platform := range domain.AllPlatforms() {
		if _, exists := loadedBatch.Drafts[platform]; !exists {
			continue
		}
		updatedDraft, transitioned, transitionErr := useCase.draftRepo.TransitionDraftStatusAtomically(
			ctx,
			loadedBatch.ID,
			platform,
			domain.StatusPendingReview,
		)
		if transitionErr != nil {
			return nil, fmt.Errorf("transition %s draft to pending review: %w", platform, transitionErr)
		}
		if transitioned && updatedDraft != nil {
			loadedBatch.Drafts[platform] = updatedDraft
		}
	}

	if len(loadedBatch.Drafts) == 0 {
		useCase.cleanupFailedBatch(loadedBatch.ID)
		return nil, fmt.Errorf("%w: pipeline produced no platform drafts for batch %s", domain.ErrValidationFailed, loadedBatch.ID)
	}

	var (
		mu                sync.Mutex
		discordMessageID  string
		telegramMessageID string
	)
	notifyGroup, notifyCtx := errgroup.WithContext(ctx)
	for _, notifier := range useCase.notifiers {
		currentNotifier := notifier
		notifyGroup.Go(func() error {
			messageID, sendErr := currentNotifier.SendForReview(notifyCtx, loadedBatch)
			if sendErr != nil {
				return fmt.Errorf("send %s review notification: %w", currentNotifier.ChannelName(), sendErr)
			}
			mu.Lock()
			defer mu.Unlock()
			switch currentNotifier.ChannelName() {
			case "discord":
				discordMessageID = messageID
			case "telegram":
				telegramMessageID = messageID
			}
			return nil
		})
	}
	if err := notifyGroup.Wait(); err != nil {
		return nil, err
	}

	if discordMessageID != "" || telegramMessageID != "" {
		if err := useCase.draftRepo.UpdateReviewMessageIDs(ctx, loadedBatch.ID, discordMessageID, telegramMessageID); err != nil {
			return nil, fmt.Errorf("store review message ids: %w", err)
		}
		loadedBatch.DiscordMessageID = discordMessageID
		loadedBatch.TelegramMessageID = telegramMessageID
	}

	useCase.logger.Info(
		"dispatched batch for human review",
		"batch_id", loadedBatch.ID,
		"commit_sha", loadedBatch.CommitEvent.CommitSHA,
		"discord_message_id", discordMessageID,
		"telegram_message_id", telegramMessageID,
	)

	return loadedBatch, nil
}

// cleanupFailedBatch deletes incomplete batches after a pipeline error so retries for the same commit are not blocked.
func (useCase *TriggerCommitPipelineUseCase) cleanupFailedBatch(batchID string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if delErr := useCase.draftRepo.DeleteBatch(cleanupCtx, batchID); delErr != nil {
		useCase.logger.Warn("failed to clean up incomplete draft batch", "batch_id", batchID, "error", delErr)
	}
}
