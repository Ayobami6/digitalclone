package domain

import "context"

// DraftRepository defines atomic persistence and row-locked approval transitions for DraftBatches and ContentDrafts.
type DraftRepository interface {
	// CreateBatchIfAbsent inserts a new DraftBatch atomically if its IdempotencyKey does not already exist.
	// It returns created=false when a duplicate trigger is detected.
	CreateBatchIfAbsent(ctx context.Context, batch *DraftBatch) (created bool, err error)

	// SaveDraft creates or updates a single platform ContentDraft inside a batch.
	SaveDraft(ctx context.Context, draft *ContentDraft) error

	// GetDraft retrieves a platform draft for a batch.
	GetDraft(ctx context.Context, batchID string, platform Platform) (*ContentDraft, error)

	// GetBatch retrieves a DraftBatch along with all associated platform drafts.
	GetBatch(ctx context.Context, batchID string) (*DraftBatch, error)

	// UpdateDraftContent updates draft copy and critic notes inside a transaction.
	UpdateDraftContent(ctx context.Context, batchID string, platform Platform, title, bodyText, criticNotes string, tags []string) (*ContentDraft, error)

	// TransitionDraftStatusAtomically locks the target draft row (SELECT ... FOR UPDATE on Postgres)
	// and applies targetStatus if allowed. Returns transitioned=false when already acted upon concurrently.
	TransitionDraftStatusAtomically(ctx context.Context, batchID string, platform Platform, targetStatus DraftStatus) (updated *ContentDraft, transitioned bool, err error)

	// UpdateReviewMessageIDs stores the Discord and Telegram message identifiers associated with a batch.
	UpdateReviewMessageIDs(ctx context.Context, batchID, discordMessageID, telegramMessageID string) error

	// RecordPublishedPost persists final publication status, external platform post ID, or failure details.
	RecordPublishedPost(ctx context.Context, draftID string, status DraftStatus, externalPostID, failureReason string) error
}

// PersonaLoader loads customizable Markdown persona instructions for a social platform.
type PersonaLoader interface {
	LoadPersona(ctx context.Context, platform Platform) (string, error)
}

// MediaGenerator defines multimodal LLM generation capabilities backed by Google GenAI.
type MediaGenerator interface {
	// GenerateText generates platform-adapted social copy using a persona system instruction and prompt.
	GenerateText(ctx context.Context, systemInstruction, prompt string) (string, error)

	// GenerateImage synthesizes a static visual graphic (PNG/JPEG) and returns raw bytes and MIME type.
	GenerateImage(ctx context.Context, prompt, aspectRatio string) (imageBytes []byte, mimeType string, err error)

	// GenerateVideo synthesizes a short-form video clip (MP4) and returns raw bytes and MIME type.
	GenerateVideo(ctx context.Context, prompt, aspectRatio string) (videoBytes []byte, mimeType string, err error)
}

// AssetStorage persists generated media assets and yields both local file paths and public HTTPS URLs.
type AssetStorage interface {
	UploadAsset(
		ctx context.Context,
		batchID string,
		platform Platform,
		mediaType MediaType,
		fileName string,
		mimeType string,
		data []byte,
	) (*MediaAsset, error)
}

// ReviewNotifier dispatches and updates human-in-the-loop review cards on notification platforms.
type ReviewNotifier interface {
	ChannelName() string
	SendForReview(ctx context.Context, batch *DraftBatch) (messageID string, err error)
	UpdateReviewStatus(ctx context.Context, batch *DraftBatch) error
}

// PlatformPostService publishes an approved ContentDraft to its target social network.
type PlatformPostService interface {
	Platform() Platform
	Publish(ctx context.Context, draft *ContentDraft) (externalPostID string, err error)
}

// CommitInspector retrieves commit metadata and diffs from local git repositories or GitHub APIs.
type CommitInspector interface {
	InspectLocalCommit(ctx context.Context, repoPath, commitRef string) (*CommitEvent, error)
	FetchGitHubCommit(ctx context.Context, repoFullName, commitSHA string) (*CommitEvent, error)
}
