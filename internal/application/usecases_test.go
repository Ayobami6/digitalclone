package application_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/model"

	"digitalclone/internal/application"
	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/persistence"
)

type stubPersonaLoader struct{}

func (loader *stubPersonaLoader) LoadPersona(_ context.Context, platform domain.Platform) (string, error) {
	return fmt.Sprintf("Persona guide for %s", platform), nil
}

type stubMediaGenerator struct{}

func (gen *stubMediaGenerator) GenerateText(_ context.Context, _ string, prompt string) (string, error) {
	return "Generated post content: " + prompt[:min(32, len(prompt))], nil
}

func (gen *stubMediaGenerator) GenerateImage(_ context.Context, _ string, _ string) ([]byte, string, error) {
	return []byte("fake-png-bytes"), "image/png", nil
}

func (gen *stubMediaGenerator) GenerateVideo(_ context.Context, _ string, _ string) ([]byte, string, error) {
	return []byte("fake-mp4-bytes"), "video/mp4", nil
}

type stubAssetStorage struct{}

func (storage *stubAssetStorage) UploadAsset(
	_ context.Context,
	batchID string,
	platform domain.Platform,
	mediaType domain.MediaType,
	fileName string,
	mimeType string,
	data []byte,
) (*domain.MediaAsset, error) {
	return &domain.MediaAsset{
		ID:        fmt.Sprintf("asset-%s-%s", batchID, platform),
		MediaType: mediaType,
		MimeType:  mimeType,
		LocalPath: "/tmp/" + fileName,
		PublicURL: "https://cdn.digitalclone.test/assets/" + fileName,
		SizeBytes: int64(len(data)),
	}, nil
}

type stubNotifier struct {
	channelName string
	sendCount   atomic.Int32
	updateCount atomic.Int32
}

func (notifier *stubNotifier) ChannelName() string {
	return notifier.channelName
}

func (notifier *stubNotifier) SendForReview(_ context.Context, batch *domain.DraftBatch) (string, error) {
	notifier.sendCount.Add(1)
	return fmt.Sprintf("%s-msg-%s", notifier.channelName, batch.ID), nil
}

func (notifier *stubNotifier) UpdateReviewStatus(_ context.Context, _ *domain.DraftBatch) error {
	notifier.updateCount.Add(1)
	return nil
}

type stubPostService struct {
	platform     domain.Platform
	publishCount atomic.Int32
	failErr      error
}

func (service *stubPostService) Platform() domain.Platform {
	return service.platform
}

func (service *stubPostService) Publish(_ context.Context, draft *domain.ContentDraft) (string, error) {
	service.publishCount.Add(1)
	if service.failErr != nil {
		return "", service.failErr
	}
	return fmt.Sprintf("%s-published-%s", service.platform, draft.ID), nil
}

func newTestSQLiteRepo(t *testing.T) domain.DraftRepository {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	repo, err := persistence.NewSQLiteRepository(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("failed to create sqlite repository: %v", err)
	}
	t.Cleanup(func() {
		_ = repo.Close()
	})
	return repo
}

func TestTriggerAndConcurrentApprovalIdempotency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newTestSQLiteRepo(t)
	toolkit := application.NewAgentToolkit(repo, &stubPersonaLoader{}, &stubMediaGenerator{}, &stubAssetStorage{}, slog.Default())

	orchestrator, err := application.NewADKOrchestrator(ctx, application.OrchestratorConfig{
		DryRun: true,
	}, toolkit, slog.Default())
	if err != nil {
		t.Fatalf("NewADKOrchestrator failed: %v", err)
	}

	discordNotifier := &stubNotifier{channelName: "discord"}
	telegramNotifier := &stubNotifier{channelName: "telegram"}

	triggerUC := application.NewTriggerCommitPipelineUseCase(
		repo,
		orchestrator,
		[]domain.ReviewNotifier{discordNotifier, telegramNotifier},
		slog.Default(),
	)

	commit := domain.CommitEvent{
		RepoName:     "Ayobami6/digitalclone",
		Branch:       "feat/agent",
		CommitSHA:    "1122334455667788",
		Author:       "Ayobami",
		Message:      "feat: implement ADK parallel social agents",
		ChangedFiles: []string{"cmd/main.go", "internal/application/adk_orchestrator.go"},
		DiffPatch:    "+parallelagent.New(...)",
	}

	batch, err := triggerUC.Execute(ctx, commit)
	if err != nil {
		t.Fatalf("TriggerCommitPipelineUseCase.Execute failed: %v", err)
	}
	if len(batch.Drafts) != 4 {
		t.Fatalf("expected 4 platform drafts, got %d", len(batch.Drafts))
	}
	for _, platform := range domain.AllPlatforms() {
		draft := batch.Drafts[platform]
		if draft == nil {
			t.Fatalf("missing draft for %s", platform)
		}
		if draft.Status != domain.StatusPendingReview {
			t.Fatalf("expected PENDING_REVIEW for %s, got %s", platform, draft.Status)
		}
		if draft.CriticNotes == "" {
			t.Fatalf("expected critic notes on %s draft", platform)
		}
	}
	if discordNotifier.sendCount.Load() != 1 || telegramNotifier.sendCount.Load() != 1 {
		t.Fatalf("expected 1 Discord and 1 Telegram review send")
	}

	// Duplicate commit push must be rejected by idempotency check.
	if _, dupErr := triggerUC.Execute(ctx, commit); !errors.Is(dupErr, domain.ErrDuplicateCommitTrigger) {
		t.Fatalf("expected ErrDuplicateCommitTrigger, got %v", dupErr)
	}

	// Concurrent Approve All clicks from Discord and Telegram must publish each platform exactly once.
	liSvc := &stubPostService{platform: domain.PlatformLinkedIn}
	ytSvc := &stubPostService{platform: domain.PlatformYouTube}
	xSvc := &stubPostService{platform: domain.PlatformX}
	ttSvc := &stubPostService{platform: domain.PlatformTikTok}

	decisionUC := application.NewProcessReviewDecisionUseCase(
		repo,
		[]domain.PlatformPostService{liSvc, ytSvc, xSvc, ttSvc},
		[]domain.ReviewNotifier{discordNotifier, telegramNotifier},
		slog.Default(),
	)

	var wg sync.WaitGroup
	results := make([]*application.ReviewDecisionResult, 2)
	errs := make([]error, 2)
	sources := []string{"discord", "telegram"}

	for idx := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = decisionUC.Execute(ctx, application.ReviewDecisionCommand{
				BatchID:        batch.ID,
				Action:         application.ReviewActionApprove,
				TargetPlatform: "all",
				Actor:          "reviewer-" + sources[i],
				Source:         sources[i],
			})
		}(idx)
	}
	wg.Wait()

	for i, execErr := range errs {
		if execErr != nil {
			t.Fatalf("decision execution %d returned error: %v", i, execErr)
		}
	}

	if liSvc.publishCount.Load() != 1 || ytSvc.publishCount.Load() != 1 || xSvc.publishCount.Load() != 1 || ttSvc.publishCount.Load() != 1 {
		t.Fatalf("expected exactly 1 publish per platform, got li=%d yt=%d x=%d tt=%d",
			liSvc.publishCount.Load(), ytSvc.publishCount.Load(), xSvc.publishCount.Load(), ttSvc.publishCount.Load())
	}

	totalTransitioned := len(results[0].TransitionedPlatforms) + len(results[1].TransitionedPlatforms)
	if totalTransitioned != 4 {
		t.Fatalf("expected total 4 transitioned platforms across concurrent callers, got %d", totalTransitioned)
	}
}

func TestRejectDecisionDoesNotPublish(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newTestSQLiteRepo(t)
	toolkit := application.NewAgentToolkit(repo, &stubPersonaLoader{}, &stubMediaGenerator{}, &stubAssetStorage{}, slog.Default())
	orchestrator, err := application.NewADKOrchestrator(ctx, application.OrchestratorConfig{DryRun: true}, toolkit, slog.Default())
	if err != nil {
		t.Fatalf("NewADKOrchestrator failed: %v", err)
	}

	triggerUC := application.NewTriggerCommitPipelineUseCase(repo, orchestrator, nil, slog.Default())
	batch, err := triggerUC.Execute(ctx, domain.CommitEvent{
		RepoName:  "Ayobami6/digitalclone",
		CommitSHA: "998877665544",
		Message:   "chore: update config",
	})
	if err != nil {
		t.Fatalf("trigger failed: %v", err)
	}

	liSvc := &stubPostService{platform: domain.PlatformLinkedIn}
	decisionUC := application.NewProcessReviewDecisionUseCase(repo, []domain.PlatformPostService{liSvc}, nil, slog.Default())

	result, err := decisionUC.Execute(ctx, application.ReviewDecisionCommand{
		BatchID:        batch.ID,
		Action:         application.ReviewActionReject,
		TargetPlatform: "linkedin",
		Actor:          "ayobami",
		Source:         "telegram",
	})
	if err != nil {
		t.Fatalf("reject decision failed: %v", err)
	}
	if len(result.TransitionedPlatforms) != 1 || result.TransitionedPlatforms[0] != domain.PlatformLinkedIn {
		t.Fatalf("unexpected transitioned platforms: %+v", result.TransitionedPlatforms)
	}
	if liSvc.publishCount.Load() != 0 {
		t.Fatalf("rejected draft must never be published")
	}

	reloaded, err := repo.GetDraft(ctx, batch.ID, domain.PlatformLinkedIn)
	if err != nil {
		t.Fatalf("GetDraft failed: %v", err)
	}
	if reloaded.Status != domain.StatusRejected {
		t.Fatalf("expected REJECTED status, got %s", reloaded.Status)
	}
}

type fakeLLM struct{}

func (llm *fakeLLM) Name() string {
	return "fake-gemini-2.5-flash"
}

func (llm *fakeLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func TestBuildADKAgentTree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newTestSQLiteRepo(t)
	toolkit := application.NewAgentToolkit(repo, &stubPersonaLoader{}, &stubMediaGenerator{}, &stubAssetStorage{}, slog.Default())
	orchestrator, err := application.NewADKOrchestrator(ctx, application.OrchestratorConfig{DryRun: true}, toolkit, slog.Default())
	if err != nil {
		t.Fatalf("NewADKOrchestrator failed: %v", err)
	}

	rootAgent, err := orchestrator.BuildAgentTree(&fakeLLM{})
	if err != nil {
		t.Fatalf("BuildAgentTree failed: %v", err)
	}
	if rootAgent.Name() != "digital_clone_pipeline" {
		t.Fatalf("expected root agent digital_clone_pipeline, got %q", rootAgent.Name())
	}

	sequentialSubAgents := rootAgent.SubAgents()
	if len(sequentialSubAgents) != 2 {
		t.Fatalf("expected 2 sequential subagents, got %d", len(sequentialSubAgents))
	}
	if sequentialSubAgents[0].Name() != "root_parallel_agent" {
		t.Fatalf("expected first sequential subagent root_parallel_agent, got %q", sequentialSubAgents[0].Name())
	}
	if sequentialSubAgents[1].Name() != "content_critic_agent" {
		t.Fatalf("expected second sequential subagent content_critic_agent, got %q", sequentialSubAgents[1].Name())
	}

	parallelSubAgents := sequentialSubAgents[0].SubAgents()
	if len(parallelSubAgents) != 4 {
		t.Fatalf("expected 4 platform subagents under root_parallel_agent, got %d", len(parallelSubAgents))
	}
}

type failOncePipelineRunner struct {
	attempts atomic.Int32
	repo     domain.DraftRepository
	delegate application.PipelineRunner
}

func (runner *failOncePipelineRunner) RunPipeline(ctx context.Context, batch *domain.DraftBatch) error {
	if runner.attempts.Add(1) == 1 {
		// Persist one partial draft before failing to verify cascade cleanup on DeleteBatch.
		_ = runner.repo.SaveDraft(ctx, &domain.ContentDraft{
			ID:        "partial-x-draft",
			BatchID:   batch.ID,
			CommitSHA: batch.CommitEvent.CommitSHA,
			Platform:  domain.PlatformX,
			BodyText:  "partial draft",
			Status:    domain.StatusDraft,
			Version:   1,
		})
		return fmt.Errorf("simulated 429 RESOURCE_EXHAUSTED")
	}
	return runner.delegate.RunPipeline(ctx, batch)
}

func TestTriggerPipelineFailureCleansUpBatchForRetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newTestSQLiteRepo(t)
	toolkit := application.NewAgentToolkit(repo, &stubPersonaLoader{}, &stubMediaGenerator{}, &stubAssetStorage{}, slog.Default())
	orchestrator, err := application.NewADKOrchestrator(ctx, application.OrchestratorConfig{DryRun: true}, toolkit, slog.Default())
	if err != nil {
		t.Fatalf("NewADKOrchestrator failed: %v", err)
	}

	flakyRunner := &failOncePipelineRunner{
		repo:     repo,
		delegate: orchestrator,
	}
	triggerUC := application.NewTriggerCommitPipelineUseCase(repo, flakyRunner, nil, slog.Default())

	commit := domain.CommitEvent{
		RepoName:  "Ayobami6/digitalclone",
		CommitSHA: "deadbeef12345678",
		Message:   "fix: retry failed batch after rate limit",
	}

	if _, err := triggerUC.Execute(ctx, commit); err == nil {
		t.Fatalf("expected first trigger execution to fail on simulated 429")
	}

	// Retrying the exact same commit must succeed instead of failing with ErrDuplicateCommitTrigger.
	batch, err := triggerUC.Execute(ctx, commit)
	if err != nil {
		t.Fatalf("expected retry trigger execution to succeed, got %v", err)
	}
	if len(batch.Drafts) != 4 {
		t.Fatalf("expected 4 drafts on retry, got %d", len(batch.Drafts))
	}
}

type failOncePostService struct {
	platform domain.Platform
	attempts atomic.Int32
}

func (service *failOncePostService) Platform() domain.Platform {
	return service.platform
}

func (service *failOncePostService) Publish(_ context.Context, draft *domain.ContentDraft) (string, error) {
	if service.attempts.Add(1) == 1 {
		return "", fmt.Errorf("simulated transient buffer failure")
	}
	return fmt.Sprintf("%s-retry-published-%s", service.platform, draft.ID), nil
}

func TestRetryFailedDraftPublishesSuccessfullyWithDetachedContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newTestSQLiteRepo(t)
	toolkit := application.NewAgentToolkit(repo, &stubPersonaLoader{}, &stubMediaGenerator{}, &stubAssetStorage{}, slog.Default())
	orchestrator, err := application.NewADKOrchestrator(ctx, application.OrchestratorConfig{DryRun: true}, toolkit, slog.Default())
	if err != nil {
		t.Fatalf("NewADKOrchestrator failed: %v", err)
	}

	triggerUC := application.NewTriggerCommitPipelineUseCase(repo, orchestrator, nil, slog.Default())
	batch, err := triggerUC.Execute(ctx, domain.CommitEvent{
		RepoName:  "Ayobami6/digitalclone",
		CommitSHA: "cafebabe12345678",
		Message:   "fix: allow retrying failed post",
	})
	if err != nil {
		t.Fatalf("trigger failed: %v", err)
	}

	xSvc := &failOncePostService{platform: domain.PlatformX}
	decisionUC := application.NewProcessReviewDecisionUseCase(repo, []domain.PlatformPostService{xSvc}, nil, slog.Default())

	// First approve fails and records StatusFailed + failure reason.
	firstRes, err := decisionUC.Execute(ctx, application.ReviewDecisionCommand{
		BatchID:        batch.ID,
		Action:         application.ReviewActionApprove,
		TargetPlatform: "x",
		Actor:          "ayobami",
		Source:         "discord",
	})
	if err != nil {
		t.Fatalf("first decision execute failed: %v", err)
	}
	if len(firstRes.FailureReasons) != 1 || firstRes.FailureReasons[domain.PlatformX] == "" {
		t.Fatalf("expected failure reason for x, got %+v", firstRes.FailureReasons)
	}

	// Simulate Discord webhook request cancellation before retrying FAILED draft.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	retryRes, err := decisionUC.Execute(canceledCtx, application.ReviewDecisionCommand{
		BatchID:        batch.ID,
		Action:         application.ReviewActionApprove,
		TargetPlatform: "x",
		Actor:          "ayobami",
		Source:         "discord",
	})
	if err != nil {
		t.Fatalf("retry decision execute failed: %v", err)
	}
	if retryRes.PublishedIDs[domain.PlatformX] == "" || len(retryRes.FailureReasons) != 0 {
		t.Fatalf("expected retry publish success, got published=%+v failures=%+v", retryRes.PublishedIDs, retryRes.FailureReasons)
	}

	reloaded, err := repo.GetDraft(ctx, batch.ID, domain.PlatformX)
	if err != nil {
		t.Fatalf("GetDraft failed: %v", err)
	}
	if reloaded.Status != domain.StatusPublished || reloaded.FailureReason != "" {
		t.Fatalf("expected PUBLISHED with empty failure_reason, got status=%s reason=%q", reloaded.Status, reloaded.FailureReason)
	}
}
