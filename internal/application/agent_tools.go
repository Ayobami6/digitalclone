// Package application implements task-oriented use cases, Google ADK agent
// orchestration, and strongly-typed agent function tools.
package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"digitalclone/internal/domain"
)

// LoadPersonaInput defines arguments for the load_persona_skill tool.
type LoadPersonaInput struct {
	Platform string `json:"platform" jsonschema:"Target social platform: linkedin, youtube, x, or tiktok"`
}

// LoadPersonaOutput returns the Markdown persona specification for the platform.
type LoadPersonaOutput struct {
	Platform string `json:"platform"`
	Persona  string `json:"persona"`
}

// GenerateTextInput defines arguments for the generate_content_text tool.
type GenerateTextInput struct {
	Platform          string `json:"platform" jsonschema:"Target social platform: linkedin, youtube, x, or tiktok"`
	PersonaSkill      string `json:"persona_skill" jsonschema:"Persona markdown guide loaded from load_persona_skill"`
	CommitSummary     string `json:"commit_summary" jsonschema:"Commit message and diff summary"`
	AdditionalContext string `json:"additional_context,omitempty" jsonschema:"Optional platform constraints or focus"`
}

// GenerateTextOutput returns generated copy for a social post.
type GenerateTextOutput struct {
	Platform string `json:"platform"`
	BodyText string `json:"body_text"`
}

// GenerateMediaInput defines arguments for generate_content_image and generate_content_video tools.
type GenerateMediaInput struct {
	BatchID     string `json:"batch_id" jsonschema:"Active draft batch identifier"`
	Platform    string `json:"platform" jsonschema:"Target social platform: linkedin, youtube, x, or tiktok"`
	Prompt      string `json:"prompt" jsonschema:"Detailed visual prompt tailored to the commit changes and platform"`
	AspectRatio string `json:"aspect_ratio,omitempty" jsonschema:"Aspect ratio such as 16:9 or 9:16"`
}

// GenerateMediaOutput returns the uploaded asset reference and URLs.
type GenerateMediaOutput struct {
	AssetID   string `json:"asset_id"`
	MediaType string `json:"media_type"`
	MimeType  string `json:"mime_type"`
	LocalPath string `json:"local_path,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
}

// SaveDraftInput defines arguments for the save_content_draft tool.
type SaveDraftInput struct {
	BatchID        string   `json:"batch_id" jsonschema:"Active draft batch identifier"`
	CommitSHA      string   `json:"commit_sha" jsonschema:"Git commit SHA"`
	Platform       string   `json:"platform" jsonschema:"Target social platform: linkedin, youtube, x, or tiktok"`
	Title          string   `json:"title" jsonschema:"Post headline or video title"`
	BodyText       string   `json:"body_text" jsonschema:"Main post text or video description"`
	Tags           []string `json:"tags,omitempty" jsonschema:"Relevant hashtags or tags without leading #"`
	MediaAssetID   string   `json:"media_asset_id,omitempty" jsonschema:"Generated asset ID"`
	MediaType      string   `json:"media_type,omitempty" jsonschema:"NONE, IMAGE, or VIDEO"`
	MediaMimeType  string   `json:"media_mime_type,omitempty" jsonschema:"Asset MIME type such as image/png or video/mp4"`
	MediaLocalPath string   `json:"media_local_path,omitempty" jsonschema:"Local filesystem path for direct uploads"`
	MediaPublicURL string   `json:"media_public_url,omitempty" jsonschema:"Public HTTPS URL for Buffer attachments"`
}

// SaveDraftOutput confirms draft persistence.
type SaveDraftOutput struct {
	DraftID  string `json:"draft_id"`
	BatchID  string `json:"batch_id"`
	Platform string `json:"platform"`
	Status   string `json:"status"`
}

// GetContentInput defines arguments for the Content Critic Agent get_content tool.
type GetContentInput struct {
	BatchID string `json:"batch_id" jsonschema:"Active draft batch identifier"`
}

// CriticDraftView exposes a lightweight draft snapshot to the Content Critic Agent.
type CriticDraftView struct {
	DraftID   string   `json:"draft_id"`
	Platform  string   `json:"platform"`
	Title     string   `json:"title"`
	BodyText  string   `json:"body_text"`
	Tags      []string `json:"tags"`
	MediaURL  string   `json:"media_url,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
	Status    string   `json:"status"`
}

// GetContentOutput returns all drafts within the active batch for critique.
type GetContentOutput struct {
	BatchID       string            `json:"batch_id"`
	CommitMessage string            `json:"commit_message"`
	DiffPatch     string            `json:"diff_patch"`
	Drafts        []CriticDraftView `json:"drafts"`
}

// UpdateContentInput defines arguments for the Content Critic Agent update_content tool.
type UpdateContentInput struct {
	BatchID     string   `json:"batch_id" jsonschema:"Active draft batch identifier"`
	Platform    string   `json:"platform" jsonschema:"Target social platform: linkedin, youtube, x, or tiktok"`
	Title       string   `json:"title" jsonschema:"Refined headline or video title"`
	BodyText    string   `json:"body_text" jsonschema:"Refined post body text or description"`
	Tags        []string `json:"tags,omitempty" jsonschema:"Refined hashtags or tags"`
	CriticNotes string   `json:"critic_notes" jsonschema:"Summary of improvements made by the critic agent"`
}

// UpdateContentOutput confirms that the draft has been critiqued and updated.
type UpdateContentOutput struct {
	DraftID  string `json:"draft_id"`
	Platform string `json:"platform"`
	Status   string `json:"status"`
}

// AgentToolkit encapsulates domain ports and constructs ADK function tools for all subagents.
type AgentToolkit struct {
	draftRepo      domain.DraftRepository
	personaLoader  domain.PersonaLoader
	mediaGenerator domain.MediaGenerator
	assetStorage   domain.AssetStorage
	logger         *slog.Logger
}

// NewAgentToolkit constructs an AgentToolkit wired to domain ports.
func NewAgentToolkit(
	draftRepo domain.DraftRepository,
	personaLoader domain.PersonaLoader,
	mediaGenerator domain.MediaGenerator,
	assetStorage domain.AssetStorage,
	logger *slog.Logger,
) *AgentToolkit {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentToolkit{
		draftRepo:      draftRepo,
		personaLoader:  personaLoader,
		mediaGenerator: mediaGenerator,
		assetStorage:   assetStorage,
		logger:         logger,
	}
}

// AgentToolSet exposes the ADK function tools grouped by subagent role.
type AgentToolSet struct {
	LoadPersona          tool.Tool
	GenerateContentText  tool.Tool
	GenerateContentImage tool.Tool
	GenerateContentVideo tool.Tool
	SaveContentDraft     tool.Tool
	GetContent           tool.Tool
	UpdateContent        tool.Tool
}

// BuildADKTools creates all strongly-typed Google ADK function tools.
func (toolkit *AgentToolkit) BuildADKTools() (*AgentToolSet, error) {
	loadPersonaTool, err := functiontool.New(functiontool.Config{
		Name:        "load_persona_skill",
		Description: "Loads the platform-specific persona skill guidelines in Markdown for linkedin, youtube, x, or tiktok.",
	}, func(toolCtx agent.ToolContext, input LoadPersonaInput) (LoadPersonaOutput, error) {
		return toolkit.ExecuteLoadPersona(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create load_persona_skill tool: %w", err)
	}

	generateTextTool, err := functiontool.New(functiontool.Config{
		Name:        "generate_content_text",
		Description: "Generates engaging social copy grounded in the commit diff and platform persona guidelines.",
	}, func(toolCtx agent.ToolContext, input GenerateTextInput) (GenerateTextOutput, error) {
		return toolkit.ExecuteGenerateText(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create generate_content_text tool: %w", err)
	}

	generateImageTool, err := functiontool.New(functiontool.Config{
		Name:        "generate_content_image",
		Description: "Generates a visual image via Google GenAI Gemini image or Imagen models and uploads it to storage for LinkedIn or X posts.",
	}, func(toolCtx agent.ToolContext, input GenerateMediaInput) (GenerateMediaOutput, error) {
		input.BatchID = resolveStateString(toolCtx, input.BatchID, "batch_id")
		return toolkit.ExecuteGenerateImage(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create generate_content_image tool: %w", err)
	}

	generateVideoTool, err := functiontool.New(functiontool.Config{
		Name:        "generate_content_video",
		Description: "Generates a short-form video via Google GenAI Gemini Omni or Veo models and uploads it to storage for YouTube or TikTok posts.",
	}, func(toolCtx agent.ToolContext, input GenerateMediaInput) (GenerateMediaOutput, error) {
		input.BatchID = resolveStateString(toolCtx, input.BatchID, "batch_id")
		return toolkit.ExecuteGenerateVideo(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create generate_content_video tool: %w", err)
	}

	saveDraftTool, err := functiontool.New(functiontool.Config{
		Name:        "save_content_draft",
		Description: "Saves the generated platform content draft and media asset references into persistent storage.",
	}, func(toolCtx agent.ToolContext, input SaveDraftInput) (SaveDraftOutput, error) {
		input.BatchID = resolveStateString(toolCtx, input.BatchID, "batch_id")
		input.CommitSHA = resolveStateString(toolCtx, input.CommitSHA, "commit_sha")
		return toolkit.ExecuteSaveDraft(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create save_content_draft tool: %w", err)
	}

	getContentTool, err := functiontool.New(functiontool.Config{
		Name:        "get_content",
		Description: "Retrieves all generated platform drafts in the current batch alongside the source commit diff for critic review.",
	}, func(toolCtx agent.ToolContext, input GetContentInput) (GetContentOutput, error) {
		input.BatchID = resolveStateString(toolCtx, input.BatchID, "batch_id")
		return toolkit.ExecuteGetContent(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create get_content tool: %w", err)
	}

	updateContentTool, err := functiontool.New(functiontool.Config{
		Name:        "update_content",
		Description: "Updates a platform draft with critic-refined title, body text, tags, and critic notes.",
	}, func(toolCtx agent.ToolContext, input UpdateContentInput) (UpdateContentOutput, error) {
		input.BatchID = resolveStateString(toolCtx, input.BatchID, "batch_id")
		return toolkit.ExecuteUpdateContent(toolCtx, input)
	})
	if err != nil {
		return nil, fmt.Errorf("create update_content tool: %w", err)
	}

	return &AgentToolSet{
		LoadPersona:          loadPersonaTool,
		GenerateContentText:  generateTextTool,
		GenerateContentImage: generateImageTool,
		GenerateContentVideo: generateVideoTool,
		SaveContentDraft:     saveDraftTool,
		GetContent:           getContentTool,
		UpdateContent:        updateContentTool,
	}, nil
}

// ExecuteLoadPersona loads the Markdown persona skill for the requested platform.
func (toolkit *AgentToolkit) ExecuteLoadPersona(ctx context.Context, input LoadPersonaInput) (LoadPersonaOutput, error) {
	platform, err := domain.ParsePlatform(input.Platform)
	if err != nil {
		return LoadPersonaOutput{}, err
	}
	personaText, err := toolkit.personaLoader.LoadPersona(ctx, platform)
	if err != nil {
		return LoadPersonaOutput{}, fmt.Errorf("load persona for %s: %w", platform, err)
	}
	return LoadPersonaOutput{
		Platform: string(platform),
		Persona:  personaText,
	}, nil
}

// ExecuteGenerateText calls MediaGenerator.GenerateText using the persona skill and commit summary.
func (toolkit *AgentToolkit) ExecuteGenerateText(ctx context.Context, input GenerateTextInput) (GenerateTextOutput, error) {
	platform, err := domain.ParsePlatform(input.Platform)
	if err != nil {
		return GenerateTextOutput{}, err
	}
	prompt := fmt.Sprintf(
		"Platform: %s\nCommit Summary:\n%s\n\nAdditional Context:\n%s\n\nProduce publish-ready social media copy grounded strictly in these code changes.",
		platform,
		input.CommitSummary,
		input.AdditionalContext,
	)
	generatedText, err := toolkit.mediaGenerator.GenerateText(ctx, input.PersonaSkill, prompt)
	if err != nil {
		return GenerateTextOutput{}, fmt.Errorf("generate text for %s: %w", platform, err)
	}
	return GenerateTextOutput{
		Platform: string(platform),
		BodyText: strings.TrimSpace(generatedText),
	}, nil
}

// ExecuteGenerateImage synthesizes an image via Gemini/Imagen and uploads it via AssetStorage.
func (toolkit *AgentToolkit) ExecuteGenerateImage(ctx context.Context, input GenerateMediaInput) (GenerateMediaOutput, error) {
	platform, err := domain.ParsePlatform(input.Platform)
	if err != nil {
		return GenerateMediaOutput{}, err
	}
	aspectRatio := strings.TrimSpace(input.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	imageBytes, mimeType, err := toolkit.mediaGenerator.GenerateImage(ctx, input.Prompt, aspectRatio)
	if err != nil {
		toolkit.logger.Error("media generation failed", "platform", platform, "media_type", domain.MediaImage, "error", err)
		return GenerateMediaOutput{}, fmt.Errorf("generate image for %s: %w", platform, err)
	}
	extension := extensionForMime(mimeType, "png")
	fileName := fmt.Sprintf("%s-%s-%s.%s", sanitizeFilenamePart(input.BatchID), platform, uuid.NewString()[:8], extension)
	asset, err := toolkit.assetStorage.UploadAsset(ctx, input.BatchID, platform, domain.MediaImage, fileName, mimeType, imageBytes)
	if err != nil {
		toolkit.logger.Error("media upload failed", "platform", platform, "media_type", domain.MediaImage, "error", err)
		return GenerateMediaOutput{}, fmt.Errorf("upload image asset for %s: %w", platform, err)
	}
	toolkit.logger.Info("uploaded generated media asset", "platform", platform, "media_type", asset.MediaType, "public_url", asset.PublicURL)
	return GenerateMediaOutput{
		AssetID:   asset.ID,
		MediaType: string(asset.MediaType),
		MimeType:  asset.MimeType,
		LocalPath: asset.LocalPath,
		PublicURL: asset.PublicURL,
	}, nil
}

// ExecuteGenerateVideo synthesizes a short-form video via Gemini Omni/Veo and uploads it via AssetStorage.
func (toolkit *AgentToolkit) ExecuteGenerateVideo(ctx context.Context, input GenerateMediaInput) (GenerateMediaOutput, error) {
	platform, err := domain.ParsePlatform(input.Platform)
	if err != nil {
		return GenerateMediaOutput{}, err
	}
	aspectRatio := strings.TrimSpace(input.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}
	videoBytes, mimeType, err := toolkit.mediaGenerator.GenerateVideo(ctx, input.Prompt, aspectRatio)
	if err != nil {
		toolkit.logger.Error("media generation failed", "platform", platform, "media_type", domain.MediaVideo, "error", err)
		return GenerateMediaOutput{}, fmt.Errorf("generate video for %s: %w", platform, err)
	}
	extension := extensionForMime(mimeType, "mp4")
	fileName := fmt.Sprintf("%s-%s-%s.%s", sanitizeFilenamePart(input.BatchID), platform, uuid.NewString()[:8], extension)
	asset, err := toolkit.assetStorage.UploadAsset(ctx, input.BatchID, platform, domain.MediaVideo, fileName, mimeType, videoBytes)
	if err != nil {
		toolkit.logger.Error("media upload failed", "platform", platform, "media_type", domain.MediaVideo, "error", err)
		return GenerateMediaOutput{}, fmt.Errorf("upload video asset for %s: %w", platform, err)
	}
	toolkit.logger.Info("uploaded generated media asset", "platform", platform, "media_type", asset.MediaType, "public_url", asset.PublicURL)
	return GenerateMediaOutput{
		AssetID:   asset.ID,
		MediaType: string(asset.MediaType),
		MimeType:  asset.MimeType,
		LocalPath: asset.LocalPath,
		PublicURL: asset.PublicURL,
	}, nil
}

// ExecuteSaveDraft persists a generated ContentDraft into DraftRepository.
func (toolkit *AgentToolkit) ExecuteSaveDraft(ctx context.Context, input SaveDraftInput) (SaveDraftOutput, error) {
	platform, err := domain.ParsePlatform(input.Platform)
	if err != nil {
		return SaveDraftOutput{}, err
	}
	if strings.TrimSpace(input.BatchID) == "" {
		return SaveDraftOutput{}, fmt.Errorf("%w: batch_id is required", domain.ErrValidationFailed)
	}
	if strings.TrimSpace(input.BodyText) == "" {
		return SaveDraftOutput{}, fmt.Errorf("%w: body_text is required", domain.ErrValidationFailed)
	}

	var mediaAsset *domain.MediaAsset
	mediaType := domain.MediaType(strings.ToUpper(strings.TrimSpace(input.MediaType)))
	if mediaType == "" {
		if strings.HasPrefix(strings.ToLower(input.MediaMimeType), "video/") {
			mediaType = domain.MediaVideo
		} else if input.MediaPublicURL != "" || input.MediaLocalPath != "" {
			mediaType = domain.MediaImage
		} else {
			mediaType = domain.MediaNone
		}
	}
	if mediaType != domain.MediaNone || input.MediaPublicURL != "" || input.MediaLocalPath != "" {
		mediaAsset = &domain.MediaAsset{
			ID:        input.MediaAssetID,
			MediaType: mediaType,
			MimeType:  input.MediaMimeType,
			LocalPath: input.MediaLocalPath,
			PublicURL: input.MediaPublicURL,
		}
	}

	now := time.Now().UTC()
	draft := &domain.ContentDraft{
		ID:         uuid.NewString(),
		BatchID:    strings.TrimSpace(input.BatchID),
		CommitSHA:  strings.TrimSpace(input.CommitSHA),
		Platform:   platform,
		Title:      strings.TrimSpace(input.Title),
		BodyText:   strings.TrimSpace(input.BodyText),
		Tags:       append([]string(nil), input.Tags...),
		MediaAsset: mediaAsset,
		Status:     domain.StatusDraft,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := toolkit.draftRepo.SaveDraft(ctx, draft); err != nil {
		return SaveDraftOutput{}, fmt.Errorf("save draft for %s: %w", platform, err)
	}
	toolkit.logger.Info("saved platform content draft", "batch_id", draft.BatchID, "platform", platform, "draft_id", draft.ID)

	return SaveDraftOutput{
		DraftID:  draft.ID,
		BatchID:  draft.BatchID,
		Platform: string(platform),
		Status:   string(draft.Status),
	}, nil
}

// ExecuteGetContent fetches all platform drafts for a batch so the Content Critic Agent can inspect them.
func (toolkit *AgentToolkit) ExecuteGetContent(ctx context.Context, input GetContentInput) (GetContentOutput, error) {
	if strings.TrimSpace(input.BatchID) == "" {
		return GetContentOutput{}, fmt.Errorf("%w: batch_id is required", domain.ErrValidationFailed)
	}
	batch, err := toolkit.draftRepo.GetBatch(ctx, strings.TrimSpace(input.BatchID))
	if err != nil {
		return GetContentOutput{}, fmt.Errorf("get batch %s: %w", input.BatchID, err)
	}

	draftViews := make([]CriticDraftView, 0, len(batch.Drafts))
	for _, platform := range domain.AllPlatforms() {
		draft, exists := batch.Drafts[platform]
		if !exists {
			continue
		}
		view := CriticDraftView{
			DraftID:  draft.ID,
			Platform: string(draft.Platform),
			Title:    draft.Title,
			BodyText: draft.BodyText,
			Tags:     append([]string(nil), draft.Tags...),
			Status:   string(draft.Status),
		}
		if draft.MediaAsset != nil {
			view.MediaURL = draft.MediaAsset.PublicURL
			view.MediaType = string(draft.MediaAsset.MediaType)
		}
		draftViews = append(draftViews, view)
	}

	return GetContentOutput{
		BatchID:       batch.ID,
		CommitMessage: batch.CommitEvent.Message,
		DiffPatch:     batch.CommitEvent.DiffPatch,
		Drafts:        draftViews,
	}, nil
}

// ExecuteUpdateContent applies critic refinements to a single platform draft inside the batch.
func (toolkit *AgentToolkit) ExecuteUpdateContent(ctx context.Context, input UpdateContentInput) (UpdateContentOutput, error) {
	platform, err := domain.ParsePlatform(input.Platform)
	if err != nil {
		return UpdateContentOutput{}, err
	}
	if strings.TrimSpace(input.BatchID) == "" {
		return UpdateContentOutput{}, fmt.Errorf("%w: batch_id is required", domain.ErrValidationFailed)
	}
	updated, err := toolkit.draftRepo.UpdateDraftContent(
		ctx,
		strings.TrimSpace(input.BatchID),
		platform,
		input.Title,
		input.BodyText,
		input.CriticNotes,
		input.Tags,
	)
	if err != nil {
		return UpdateContentOutput{}, fmt.Errorf("update critiqued content for %s: %w", platform, err)
	}
	toolkit.logger.Info("critic updated platform content draft", "batch_id", updated.BatchID, "platform", platform)

	return UpdateContentOutput{
		DraftID:  updated.ID,
		Platform: string(updated.Platform),
		Status:   string(updated.Status),
	}, nil
}

func resolveStateString(toolCtx agent.ToolContext, explicitValue, stateKey string) string {
	if strings.TrimSpace(explicitValue) != "" {
		return strings.TrimSpace(explicitValue)
	}
	if toolCtx == nil {
		return ""
	}
	value, err := toolCtx.ReadonlyState().Get(stateKey)
	if err != nil {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func extensionForMime(mimeType, fallback string) string {
	normalized := strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.Contains(normalized, "jpeg"), strings.Contains(normalized, "jpg"):
		return "jpg"
	case strings.Contains(normalized, "png"):
		return "png"
	case strings.Contains(normalized, "webp"):
		return "webp"
	case strings.Contains(normalized, "mp4"):
		return "mp4"
	case strings.Contains(normalized, "webm"):
		return "webm"
	default:
		return fallback
	}
}

func sanitizeFilenamePart(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "batch"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-", ":", "-")
	return replacer.Replace(trimmed)
}
