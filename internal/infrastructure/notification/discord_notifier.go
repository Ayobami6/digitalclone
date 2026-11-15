// Package notification implements interactive human-in-the-loop review notifiers
// for Discord (REST API v10 embeds + ActionRow buttons) and Telegram (Bot API inline keyboards).
package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/pkg/resilience"
)

const defaultDiscordAPIBase = "https://discord.com/api/v10"

// DiscordConfig configures the Discord Bot REST v10 review notifier.
type DiscordConfig struct {
	BotToken       string
	ChannelID      string
	APIBaseURL     string
	RequestTimeout time.Duration
	HTTPClient     *http.Client
	DryRun         bool
	Logger         *slog.Logger
}

// DiscordNotifier implements domain.ReviewNotifier using Discord REST API v10.
type DiscordNotifier struct {
	botToken       string
	channelID      string
	apiBaseURL     string
	requestTimeout time.Duration
	httpClient     *http.Client
	dryRun         bool
	logger         *slog.Logger
	breaker        *resilience.CircuitBreaker
}

// NewDiscordNotifier constructs a DiscordNotifier adapter.
func NewDiscordNotifier(config DiscordConfig) *DiscordNotifier {
	baseURL := strings.TrimRight(strings.TrimSpace(config.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultDiscordAPIBase
	}
	timeout := config.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &DiscordNotifier{
		botToken:       strings.TrimSpace(config.BotToken),
		channelID:      strings.TrimSpace(config.ChannelID),
		apiBaseURL:     baseURL,
		requestTimeout: timeout,
		httpClient:     httpClient,
		dryRun:         config.DryRun || strings.TrimSpace(config.BotToken) == "" || strings.TrimSpace(config.ChannelID) == "",
		logger:         logger,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "discord-rest-api",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}
}

// ChannelName returns "discord".
func (notifier *DiscordNotifier) ChannelName() string {
	return "discord"
}

// SendForReview posts an interactive review card with embeds and Approve/Reject buttons to Discord.
func (notifier *DiscordNotifier) SendForReview(ctx context.Context, batch *domain.DraftBatch) (string, error) {
	if batch == nil {
		return "", fmt.Errorf("%w: batch cannot be nil", domain.ErrValidationFailed)
	}
	if notifier.dryRun {
		notifier.logger.Info("dry-run discord review card", "batch_id", batch.ID, "commit_sha", batch.CommitEvent.CommitSHA)
		return "dryrun-discord-" + batch.ID, nil
	}

	payload := buildDiscordMessagePayload(batch)
	endpoint := fmt.Sprintf("%s/channels/%s/messages", notifier.apiBaseURL, notifier.channelID)

	var created struct {
		ID string `json:"id"`
	}
	if err := notifier.doDiscordRequest(ctx, http.MethodPost, endpoint, payload, &created); err != nil {
		return "", fmt.Errorf("send discord review message: %w", err)
	}
	return created.ID, nil
}

// UpdateReviewStatus edits the existing Discord review message to reflect current approval/publish statuses.
func (notifier *DiscordNotifier) UpdateReviewStatus(ctx context.Context, batch *domain.DraftBatch) error {
	if batch == nil || strings.TrimSpace(batch.DiscordMessageID) == "" {
		return nil
	}
	if notifier.dryRun || strings.HasPrefix(batch.DiscordMessageID, "dryrun-") {
		notifier.logger.Info("dry-run discord review update", "batch_id", batch.ID, "message_id", batch.DiscordMessageID)
		return nil
	}

	payload := buildDiscordMessagePayload(batch)
	endpoint := fmt.Sprintf("%s/channels/%s/messages/%s", notifier.apiBaseURL, notifier.channelID, batch.DiscordMessageID)
	if err := notifier.doDiscordRequest(ctx, http.MethodPatch, endpoint, payload, nil); err != nil {
		return fmt.Errorf("update discord review message: %w", err)
	}
	return nil
}

func (notifier *DiscordNotifier) doDiscordRequest(ctx context.Context, method, endpoint string, body any, out any) error {
	rawPayload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}

	return notifier.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, notifier.requestTimeout)
		defer cancel()

		req, reqErr := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(rawPayload))
		if reqErr != nil {
			return fmt.Errorf("build discord request: %w", reqErr)
		}
		req.Header.Set("Authorization", "Bot "+notifier.botToken)
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := notifier.httpClient.Do(req)
		if doErr != nil {
			return fmt.Errorf("execute discord request: %w", doErr)
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("discord HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		if out != nil && len(respBody) > 0 {
			if decodeErr := json.Unmarshal(respBody, out); decodeErr != nil {
				return fmt.Errorf("decode discord response: %w", decodeErr)
			}
		}
		return nil
	})
}

func buildDiscordMessagePayload(batch *domain.DraftBatch) map[string]any {
	embeds := make([]map[string]any, 0, len(batch.Drafts)+1)
	embeds = append(embeds, map[string]any{
		"title":       fmt.Sprintf("DigitalClone Review — %s", batch.CommitEvent.RepoName),
		"description": fmt.Sprintf("Commit `%s` on `%s`\n> %s", shortHash(batch.CommitEvent.CommitSHA), batch.CommitEvent.Branch, truncate(batch.CommitEvent.Message, 240)),
		"color":       0x5865F2,
		"footer":      map[string]any{"text": fmt.Sprintf("Batch ID: %s", batch.ID)},
	})

	hasPending := false
	approveRow := make([]map[string]any, 0, 4)
	rejectRow := make([]map[string]any, 0, 4)

	for _, platform := range domain.AllPlatforms() {
		draft, exists := batch.Drafts[platform]
		if !exists {
			continue
		}
		isPending := draft.Status == domain.StatusPendingReview
		if isPending {
			hasPending = true
		}

		embed := map[string]any{
			"title":       fmt.Sprintf("%s [%s]", strings.ToUpper(string(platform)), draft.Status),
			"description": truncate(draft.BodyText, 1200),
			"color":       discordColorForStatus(draft.Status),
		}
		var fields []map[string]any
		if draft.Title != "" {
			fields = append(fields, map[string]any{"name": "Headline", "value": truncate(draft.Title, 256), "inline": false})
		}
		if draft.CriticNotes != "" {
			fields = append(fields, map[string]any{"name": "Critic Notes", "value": truncate(draft.CriticNotes, 512), "inline": false})
		}
		if draft.MediaAsset != nil && draft.MediaAsset.PublicURL != "" {
			fields = append(fields, map[string]any{"name": fmt.Sprintf("Media (%s)", draft.MediaAsset.MediaType), "value": draft.MediaAsset.PublicURL, "inline": false})
			if draft.MediaAsset.MediaType == domain.MediaImage {
				embed["image"] = map[string]any{"url": draft.MediaAsset.PublicURL}
			}
		}
		if draft.ExternalPostID != "" {
			fields = append(fields, map[string]any{"name": "External Post ID", "value": draft.ExternalPostID, "inline": true})
		}
		if draft.FailureReason != "" {
			fields = append(fields, map[string]any{"name": "Failure", "value": truncate(draft.FailureReason, 256), "inline": true})
		}
		if len(fields) > 0 {
			embed["fields"] = fields
		}
		embeds = append(embeds, embed)

		approveRow = append(approveRow, map[string]any{
			"type":      2,
			"style":     1,
			"label":     "Approve " + displayPlatformName(platform),
			"custom_id": fmt.Sprintf("review:%s:approve:%s", batch.ID, platform),
			"disabled":  !isPending,
		})
		rejectRow = append(rejectRow, map[string]any{
			"type":      2,
			"style":     2,
			"label":     "Reject " + displayPlatformName(platform),
			"custom_id": fmt.Sprintf("review:%s:reject:%s", batch.ID, platform),
			"disabled":  !isPending,
		})
	}

	components := []map[string]any{
		{
			"type": 1,
			"components": []map[string]any{
				{
					"type":      2,
					"style":     3,
					"label":     "Approve All",
					"custom_id": fmt.Sprintf("review:%s:approve:all", batch.ID),
					"disabled":  !hasPending,
				},
				{
					"type":      2,
					"style":     4,
					"label":     "Reject All",
					"custom_id": fmt.Sprintf("review:%s:reject:all", batch.ID),
					"disabled":  !hasPending,
				},
			},
		},
	}
	if len(approveRow) > 0 {
		components = append(components, map[string]any{"type": 1, "components": approveRow})
	}
	if len(rejectRow) > 0 {
		components = append(components, map[string]any{"type": 1, "components": rejectRow})
	}

	return map[string]any{
		"content":    fmt.Sprintf("New commit content batch ready for review (`%s`)", shortHash(batch.CommitEvent.CommitSHA)),
		"embeds":     embeds,
		"components": components,
	}
}

func discordColorForStatus(status domain.DraftStatus) int {
	switch status {
	case domain.StatusPublished:
		return 0x57F287
	case domain.StatusRejected, domain.StatusFailed:
		return 0xED4245
	case domain.StatusApproved:
		return 0xFEE75C
	default:
		return 0x5865F2
	}
}

func displayPlatformName(platform domain.Platform) string {
	switch platform {
	case domain.PlatformLinkedIn:
		return "LinkedIn"
	case domain.PlatformYouTube:
		return "YouTube"
	case domain.PlatformX:
		return "X"
	case domain.PlatformTikTok:
		return "TikTok"
	default:
		return string(platform)
	}
}

func shortHash(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return text[:maxLen]
	}
	return text[:maxLen-3] + "..."
}
