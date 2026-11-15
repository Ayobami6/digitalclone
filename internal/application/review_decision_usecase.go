package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"digitalclone/internal/domain"
)

// ReviewAction identifies the human decision chosen in Discord or Telegram.
type ReviewAction string

const (
	// ReviewActionApprove approves the target draft(s) and dispatches publishing.
	ReviewActionApprove ReviewAction = "approve"
	// ReviewActionReject marks the target draft(s) REJECTED without publishing.
	ReviewActionReject ReviewAction = "reject"
)

// ReviewDecisionCommand models an interactive Approve/Reject callback from Discord or Telegram.
type ReviewDecisionCommand struct {
	BatchID        string
	Action         ReviewAction
	TargetPlatform string
	Actor          string
	Source         string
}

// ReviewDecisionResult summarizes the atomic state transitions and publishing outcomes.
type ReviewDecisionResult struct {
	BatchID               string                     `json:"batch_id"`
	Action                ReviewAction               `json:"action"`
	TransitionedPlatforms []domain.Platform          `json:"transitioned_platforms"`
	SkippedPlatforms      []domain.Platform          `json:"skipped_platforms"`
	PublishedIDs          map[domain.Platform]string `json:"published_ids,omitempty"`
	FailureReasons        map[domain.Platform]string `json:"failure_reasons,omitempty"`
}

// ProcessReviewDecisionUseCase handles Approve/Reject decisions idempotently across Discord & Telegram.
type ProcessReviewDecisionUseCase struct {
	draftRepo  domain.DraftRepository
	publishers map[domain.Platform]domain.PlatformPostService
	notifiers  []domain.ReviewNotifier
	logger     *slog.Logger
}

// NewProcessReviewDecisionUseCase constructs a ProcessReviewDecisionUseCase with platform publishers and notifiers.
func NewProcessReviewDecisionUseCase(
	draftRepo domain.DraftRepository,
	services []domain.PlatformPostService,
	notifiers []domain.ReviewNotifier,
	logger *slog.Logger,
) *ProcessReviewDecisionUseCase {
	if logger == nil {
		logger = slog.Default()
	}
	serviceMap := make(map[domain.Platform]domain.PlatformPostService, len(services))
	for _, service := range services {
		if service != nil {
			serviceMap[service.Platform()] = service
		}
	}
	return &ProcessReviewDecisionUseCase{
		draftRepo:  draftRepo,
		publishers: serviceMap,
		notifiers:  append([]domain.ReviewNotifier(nil), notifiers...),
		logger:     logger,
	}
}

// Execute applies the human decision atomically, publishes approved drafts concurrently,
// and updates both Discord and Telegram review messages.
func (useCase *ProcessReviewDecisionUseCase) Execute(ctx context.Context, command ReviewDecisionCommand) (*ReviewDecisionResult, error) {
	if strings.TrimSpace(command.BatchID) == "" {
		return nil, fmt.Errorf("%w: batch_id is required", domain.ErrValidationFailed)
	}
	if command.Action != ReviewActionApprove && command.Action != ReviewActionReject {
		return nil, fmt.Errorf("%w: unsupported review action %q", domain.ErrValidationFailed, command.Action)
	}

	targetPlatforms, err := resolveTargetPlatforms(command.TargetPlatform)
	if err != nil {
		return nil, err
	}

	// Detach execution from incoming HTTP webhook request timeouts (e.g., Discord's 3s limit)
	// so platform publishing and review card status updates complete reliably.
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	result := &ReviewDecisionResult{
		BatchID:        command.BatchID,
		Action:         command.Action,
		PublishedIDs:   make(map[domain.Platform]string),
		FailureReasons: make(map[domain.Platform]string),
	}

	targetStatus := domain.StatusRejected
	if command.Action == ReviewActionApprove {
		targetStatus = domain.StatusApproved
	}

	approvedDrafts := make([]*domain.ContentDraft, 0, len(targetPlatforms))
	for _, platform := range targetPlatforms {
		updatedDraft, transitioned, transitionErr := useCase.draftRepo.TransitionDraftStatusAtomically(
			execCtx,
			command.BatchID,
			platform,
			targetStatus,
		)
		if transitionErr != nil {
			return nil, fmt.Errorf("transition %s draft status: %w", platform, transitionErr)
		}
		if !transitioned {
			result.SkippedPlatforms = append(result.SkippedPlatforms, platform)
			continue
		}
		result.TransitionedPlatforms = append(result.TransitionedPlatforms, platform)
		if command.Action == ReviewActionApprove && updatedDraft != nil {
			approvedDrafts = append(approvedDrafts, updatedDraft)
		}
	}

	if command.Action == ReviewActionApprove && len(approvedDrafts) > 0 {
		var resultMu sync.Mutex
		publishGroup, publishCtx := errgroup.WithContext(execCtx)

		for _, draft := range approvedDrafts {
			currentDraft := draft
			publishGroup.Go(func() error {
				publisher, ok := useCase.publishers[currentDraft.Platform]
				if !ok {
					reason := fmt.Sprintf("no post service registered for %s", currentDraft.Platform)
					useCase.logger.Error("platform publish failed",
						"batch_id", command.BatchID,
						"platform", currentDraft.Platform,
						"error", reason,
					)
					_ = useCase.draftRepo.RecordPublishedPost(publishCtx, currentDraft.ID, domain.StatusFailed, "", reason)
					resultMu.Lock()
					result.FailureReasons[currentDraft.Platform] = reason
					resultMu.Unlock()
					return nil
				}

				externalPostID, pubErr := publisher.Publish(publishCtx, currentDraft)
				if pubErr != nil {
					useCase.logger.Error("platform publish failed",
						"batch_id", command.BatchID,
						"platform", currentDraft.Platform,
						"error", pubErr,
					)
					_ = useCase.draftRepo.RecordPublishedPost(publishCtx, currentDraft.ID, domain.StatusFailed, "", pubErr.Error())
					resultMu.Lock()
					result.FailureReasons[currentDraft.Platform] = pubErr.Error()
					resultMu.Unlock()
					return nil
				}

				if recErr := useCase.draftRepo.RecordPublishedPost(publishCtx, currentDraft.ID, domain.StatusPublished, externalPostID, ""); recErr != nil {
					return fmt.Errorf("record published status for %s: %w", currentDraft.Platform, recErr)
				}
				useCase.logger.Info("platform publish succeeded",
					"batch_id", command.BatchID,
					"platform", currentDraft.Platform,
					"external_post_id", externalPostID,
				)
				resultMu.Lock()
				result.PublishedIDs[currentDraft.Platform] = externalPostID
				resultMu.Unlock()
				return nil
			})
		}

		if err := publishGroup.Wait(); err != nil {
			return result, err
		}
	}

	// Sync updated status cards back to Discord and Telegram whenever any platform transitioned.
	if len(result.TransitionedPlatforms) > 0 {
		latestBatch, getErr := useCase.draftRepo.GetBatch(execCtx, command.BatchID)
		if getErr == nil {
			syncGroup, syncCtx := errgroup.WithContext(execCtx)
			for _, notifier := range useCase.notifiers {
				currentNotifier := notifier
				syncGroup.Go(func() error {
					if updateErr := currentNotifier.UpdateReviewStatus(syncCtx, latestBatch); updateErr != nil {
						useCase.logger.Warn("failed to sync review card status",
							"channel", currentNotifier.ChannelName(),
							"batch_id", command.BatchID,
							"error", updateErr,
						)
					}
					return nil
				})
			}
			_ = syncGroup.Wait()
		}
	}

	useCase.logger.Info(
		"processed review decision",
		"batch_id", command.BatchID,
		"action", command.Action,
		"source", command.Source,
		"actor", command.Actor,
		"transitioned", result.TransitionedPlatforms,
		"skipped", result.SkippedPlatforms,
		"published_ids", result.PublishedIDs,
		"failure_reasons", result.FailureReasons,
	)

	return result, nil
}

func resolveTargetPlatforms(target string) ([]domain.Platform, error) {
	normalized := strings.ToLower(strings.TrimSpace(target))
	if normalized == "" || normalized == "all" {
		return domain.AllPlatforms(), nil
	}
	platform, err := domain.ParsePlatform(normalized)
	if err != nil {
		return nil, err
	}
	return []domain.Platform{platform}, nil
}
