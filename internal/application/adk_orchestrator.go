package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sync/errgroup"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"digitalclone/internal/domain"
)

// OrchestratorConfig configures the Google ADK agent pipeline and Gemini model settings.
type OrchestratorConfig struct {
	AppName      string
	ModelName    string
	GeminiAPIKey string
	DryRun       bool
}

// ADKOrchestrator executes the RootParallelAgent -> ContentCriticAgent pipeline.
type ADKOrchestrator struct {
	config  OrchestratorConfig
	toolkit *AgentToolkit
	logger  *slog.Logger
	llm     model.LLM
}

// NewADKOrchestrator initializes the ADK orchestrator, connecting Gemini LLM when live API credentials are set.
func NewADKOrchestrator(ctx context.Context, config OrchestratorConfig, toolkit *AgentToolkit, logger *slog.Logger) (*ADKOrchestrator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(config.AppName) == "" {
		config.AppName = "digitalclone"
	}
	if strings.TrimSpace(config.ModelName) == "" {
		config.ModelName = "gemini-3.5-flash-lite"
	}

	var llmModel model.LLM
	if !config.DryRun && strings.TrimSpace(config.GeminiAPIKey) != "" {
		createdModel, err := gemini.NewModel(ctx, config.ModelName, &genai.ClientConfig{
			APIKey: config.GeminiAPIKey,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize ADK gemini model: %w", err)
		}
		llmModel = createdModel
	}

	return &ADKOrchestrator{
		config:  config,
		toolkit: toolkit,
		logger:  logger,
		llm:     llmModel,
	}, nil
}

// RunPipeline runs either the live Google ADK multi-agent runner or deterministic local pipeline in dry-run mode.
func (orchestrator *ADKOrchestrator) RunPipeline(ctx context.Context, batch *domain.DraftBatch) error {
	if orchestrator.llm == nil {
		orchestrator.logger.Info("running deterministic local multi-agent pipeline (dry-run / offline mode)", "batch_id", batch.ID)
		return orchestrator.RunDeterministicPipeline(ctx, batch)
	}
	return orchestrator.runLiveADKPipeline(ctx, batch)
}

// BuildAgentTree constructs the ADK SequentialAgent -> ParallelAgent + ContentCriticAgent hierarchy.
// Keeping construction exported lets tests and diagnostics inspect the full agent topology without live API calls.
func (orchestrator *ADKOrchestrator) BuildAgentTree(llmModel model.LLM) (agent.Agent, error) {
	tools, err := orchestrator.toolkit.BuildADKTools()
	if err != nil {
		return nil, err
	}

	linkedinAgent, err := buildPlatformLLMAgent(llmModel, platformAgentSpec{
		Name:        "linkedin_post_agent",
		Platform:    domain.PlatformLinkedIn,
		Description: "Creates authoritative engineering LinkedIn posts with a technical banner image.",
		MediaNote:   "Use generate_content_image with aspect_ratio 16:9.",
		Tools:       []tool.Tool{tools.LoadPersona, tools.GenerateContentText, tools.GenerateContentImage, tools.SaveContentDraft},
	})
	if err != nil {
		return nil, err
	}

	youtubeAgent, err := buildPlatformLLMAgent(llmModel, platformAgentSpec{
		Name:        "youtube_post_agent",
		Platform:    domain.PlatformYouTube,
		Description: "Creates YouTube Shorts scripts, titles, descriptions, and vertical videos.",
		MediaNote:   "Use generate_content_video with aspect_ratio 9:16.",
		Tools:       []tool.Tool{tools.LoadPersona, tools.GenerateContentText, tools.GenerateContentVideo, tools.SaveContentDraft},
	})
	if err != nil {
		return nil, err
	}

	xAgent, err := buildPlatformLLMAgent(llmModel, platformAgentSpec{
		Name:        "x_post_agent",
		Platform:    domain.PlatformX,
		Description: "Creates punchy X (Twitter) developer updates with crisp technical visuals.",
		MediaNote:   "Use generate_content_image with aspect_ratio 16:9.",
		Tools:       []tool.Tool{tools.LoadPersona, tools.GenerateContentText, tools.GenerateContentImage, tools.SaveContentDraft},
	})
	if err != nil {
		return nil, err
	}

	tiktokAgent, err := buildPlatformLLMAgent(llmModel, platformAgentSpec{
		Name:        "tiktok_post_agent",
		Platform:    domain.PlatformTikTok,
		Description: "Creates fast-paced TikTok developer demo videos and captions.",
		MediaNote:   "Use generate_content_video with aspect_ratio 9:16.",
		Tools:       []tool.Tool{tools.LoadPersona, tools.GenerateContentText, tools.GenerateContentVideo, tools.SaveContentDraft},
	})
	if err != nil {
		return nil, err
	}

	rootParallelAgent, err := parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{
			Name:        "root_parallel_agent",
			Description: "Runs LinkedIn, YouTube, X, and TikTok subagents concurrently.",
			SubAgents:   []agent.Agent{linkedinAgent, youtubeAgent, xAgent, tiktokAgent},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create root_parallel_agent: %w", err)
	}

	contentCriticAgent, err := llmagent.New(llmagent.Config{
		Name:        "content_critic_agent",
		Model:       llmModel,
		Description: "Reviews all 4 generated drafts for technical accuracy, hook strength, and platform fit.",
		Instruction: `You are the Content Critic Agent for DigitalClone.
1. Call get_content with batch_id {batch_id}.
2. Compare every draft (linkedin, youtube, x, tiktok) against the commit message and diff patch.
3. Verify each draft is technically accurate, concise, free of fabricated claims, and optimized for its target platform.
4. For each of the 4 platforms, call update_content with the refined title, body_text, tags, and concise critic_notes.`,
		Tools:                    []tool.Tool{tools.GetContent, tools.UpdateContent},
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("create content_critic_agent: %w", err)
	}

	rootSequentialPipeline, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:        "digital_clone_pipeline",
			Description: "Executes parallel platform drafting followed by the Content Critic Agent.",
			SubAgents:   []agent.Agent{rootParallelAgent, contentCriticAgent},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create sequential pipeline: %w", err)
	}

	return rootSequentialPipeline, nil
}

func (orchestrator *ADKOrchestrator) runLiveADKPipeline(ctx context.Context, batch *domain.DraftBatch) error {
	rootAgent, err := orchestrator.BuildAgentTree(orchestrator.llm)
	if err != nil {
		return err
	}

	sessionService := session.InMemoryService()
	sessionID := batch.ID
	userID := "digitalclone-system"

	_, err = sessionService.Create(ctx, &session.CreateRequest{
		AppName:   orchestrator.config.AppName,
		UserID:    userID,
		SessionID: sessionID,
		State: map[string]any{
			"batch_id":       batch.ID,
			"commit_sha":     batch.CommitEvent.CommitSHA,
			"repo_name":      batch.CommitEvent.RepoName,
			"commit_message": batch.CommitEvent.Message,
		},
	})
	if err != nil {
		return fmt.Errorf("create ADK session: %w", err)
	}

	adkRunner, err := runner.New(runner.Config{
		AppName:        orchestrator.config.AppName,
		Agent:          rootAgent,
		SessionService: sessionService,
	})
	if err != nil {
		return fmt.Errorf("create ADK runner: %w", err)
	}

	promptText := buildCommitPrompt(batch)
	userMessage := genai.NewContentFromText(promptText, genai.RoleUser)

	for event, runErr := range adkRunner.Run(ctx, userID, sessionID, userMessage, agent.RunConfig{}) {
		if runErr != nil {
			return fmt.Errorf("adk pipeline execution failed: %w", runErr)
		}
		if event != nil && event.Author != "" {
			orchestrator.logger.Debug("adk event emitted", "author", event.Author, "batch_id", batch.ID)
		}
	}

	return nil
}

// RunDeterministicPipeline executes the 4 parallel platform subagent toolchains concurrently followed by the critic pass.
// This guarantees deterministic tool execution during dry-run CLI commands and automated tests.
func (orchestrator *ADKOrchestrator) RunDeterministicPipeline(ctx context.Context, batch *domain.DraftBatch) error {
	commitSummary := buildCommitSummary(batch.CommitEvent)

	group, groupCtx := errgroup.WithContext(ctx)
	for _, platform := range domain.AllPlatforms() {
		targetPlatform := platform
		group.Go(func() error {
			personaOut, err := orchestrator.toolkit.ExecuteLoadPersona(groupCtx, LoadPersonaInput{
				Platform: string(targetPlatform),
			})
			if err != nil {
				return err
			}

			textOut, err := orchestrator.toolkit.ExecuteGenerateText(groupCtx, GenerateTextInput{
				Platform:      string(targetPlatform),
				PersonaSkill:  personaOut.Persona,
				CommitSummary: commitSummary,
			})
			if err != nil {
				return err
			}

			mediaPrompt := fmt.Sprintf("Software engineering visual for %s about commit: %s", targetPlatform, batch.CommitEvent.Message)
			var mediaOut GenerateMediaOutput
			if targetPlatform == domain.PlatformYouTube || targetPlatform == domain.PlatformTikTok {
				mediaOut, err = orchestrator.toolkit.ExecuteGenerateVideo(groupCtx, GenerateMediaInput{
					BatchID:     batch.ID,
					Platform:    string(targetPlatform),
					Prompt:      mediaPrompt,
					AspectRatio: "9:16",
				})
			} else {
				mediaOut, err = orchestrator.toolkit.ExecuteGenerateImage(groupCtx, GenerateMediaInput{
					BatchID:     batch.ID,
					Platform:    string(targetPlatform),
					Prompt:      mediaPrompt,
					AspectRatio: "16:9",
				})
			}
			if err != nil {
				return err
			}

			title := deriveHeadline(targetPlatform, batch.CommitEvent.Message)
			_, err = orchestrator.toolkit.ExecuteSaveDraft(groupCtx, SaveDraftInput{
				BatchID:        batch.ID,
				CommitSHA:      batch.CommitEvent.CommitSHA,
				Platform:       string(targetPlatform),
				Title:          title,
				BodyText:       textOut.BodyText,
				Tags:           defaultTagsForPlatform(targetPlatform),
				MediaAssetID:   mediaOut.AssetID,
				MediaType:      mediaOut.MediaType,
				MediaMimeType:  mediaOut.MimeType,
				MediaLocalPath: mediaOut.LocalPath,
				MediaPublicURL: mediaOut.PublicURL,
			})
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return fmt.Errorf("parallel platform generation failed: %w", err)
	}

	// Stage 2: Content Critic Agent pass.
	contentOut, err := orchestrator.toolkit.ExecuteGetContent(ctx, GetContentInput{BatchID: batch.ID})
	if err != nil {
		return fmt.Errorf("critic get_content failed: %w", err)
	}

	for _, draftView := range contentOut.Drafts {
		platform, parseErr := domain.ParsePlatform(draftView.Platform)
		if parseErr != nil {
			return parseErr
		}
		refinedBody := draftView.BodyText
		criticPrompt := fmt.Sprintf(
			"Critique and refine this %s post so it accurately reflects commit %q (%s) with a sharper developer hook:\n\n%s",
			platform,
			batch.CommitEvent.Message,
			batch.CommitEvent.CommitSHA,
			draftView.BodyText,
		)
		if critiquedText, genErr := orchestrator.toolkit.mediaGenerator.GenerateText(
			ctx,
			"You are a senior developer advocate critic. Return only the tightened, technically accurate post text.",
			criticPrompt,
		); genErr == nil && strings.TrimSpace(critiquedText) != "" {
			refinedBody = strings.TrimSpace(critiquedText)
		}

		_, err = orchestrator.toolkit.ExecuteUpdateContent(ctx, UpdateContentInput{
			BatchID:     batch.ID,
			Platform:    draftView.Platform,
			Title:       draftView.Title,
			BodyText:    refinedBody,
			Tags:        draftView.Tags,
			CriticNotes: fmt.Sprintf("Verified technical accuracy against commit %s and sharpened opening hook.", shortSHA(batch.CommitEvent.CommitSHA)),
		})
		if err != nil {
			return fmt.Errorf("critic update_content failed for %s: %w", platform, err)
		}
	}

	return nil
}

type platformAgentSpec struct {
	Name        string
	Platform    domain.Platform
	Description string
	MediaNote   string
	Tools       []tool.Tool
}

func buildPlatformLLMAgent(llmModel model.LLM, spec platformAgentSpec) (agent.Agent, error) {
	instruction := fmt.Sprintf(`You are the %s agent in DigitalClone.
Follow these exact steps for batch {batch_id} and commit {commit_sha}:
1. Call load_persona_skill with platform="%s".
2. Call generate_content_text using the loaded persona_skill and commit summary.
3. %s
4. Call save_content_draft with batch_id="{batch_id}", commit_sha="{commit_sha}", platform="%s", title, body_text, tags, and the generated media_asset_id, media_type, media_mime_type, media_local_path, media_public_url.`,
		spec.Name, spec.Platform, spec.MediaNote, spec.Platform)

	createdAgent, err := llmagent.New(llmagent.Config{
		Name:                     spec.Name,
		Model:                    llmModel,
		Description:              spec.Description,
		Instruction:              instruction,
		Tools:                    spec.Tools,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", spec.Name, err)
	}
	return createdAgent, nil
}

func buildCommitPrompt(batch *domain.DraftBatch) string {
	return fmt.Sprintf(
		"Process new code push for batch_id=%s.\n%s",
		batch.ID,
		buildCommitSummary(batch.CommitEvent),
	)
}

func buildCommitSummary(commit domain.CommitEvent) string {
	diffSnippet := commit.DiffPatch
	if len(diffSnippet) > 6000 {
		diffSnippet = diffSnippet[:6000] + "\n... [diff truncated]"
	}
	return fmt.Sprintf(
		"Repository: %s\nBranch: %s\nCommit SHA: %s\nAuthor: %s\nCommit Message: %s\nChanged Files: %s\n\nDiff Patch:\n%s",
		commit.RepoName,
		commit.Branch,
		commit.CommitSHA,
		commit.Author,
		commit.Message,
		strings.Join(commit.ChangedFiles, ", "),
		diffSnippet,
	)
}

func deriveHeadline(platform domain.Platform, commitMessage string) string {
	firstLine := strings.TrimSpace(strings.Split(commitMessage, "\n")[0])
	switch platform {
	case domain.PlatformYouTube:
		return fmt.Sprintf("Shipped: %s #Shorts", firstLine)
	case domain.PlatformTikTok:
		return fmt.Sprintf("Devlog: %s", firstLine)
	case domain.PlatformLinkedIn:
		return fmt.Sprintf("Engineering Update: %s", firstLine)
	default:
		return firstLine
	}
}

func defaultTagsForPlatform(platform domain.Platform) []string {
	switch platform {
	case domain.PlatformLinkedIn:
		return []string{"SoftwareEngineering", "Go", "AIAgents", "BuildInPublic"}
	case domain.PlatformYouTube:
		return []string{"Shorts", "Coding", "SoftwareEngineering", "Go"}
	case domain.PlatformX:
		return []string{"buildinpublic", "golang", "aiagents"}
	case domain.PlatformTikTok:
		return []string{"coding", "devlog", "tech", "softwareengineer"}
	default:
		return []string{"softwareengineering"}
	}
}

func shortSHA(commitSHA string) string {
	trimmed := strings.TrimSpace(commitSHA)
	if len(trimmed) > 7 {
		return trimmed[:7]
	}
	return trimmed
}
