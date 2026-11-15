package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/pkg/resilience"
)

const defaultTelegramAPIBase = "https://api.telegram.org"

// TelegramConfig configures the Telegram Bot API review notifier.
type TelegramConfig struct {
	BotToken       string
	ChatID         string
	APIBaseURL     string
	RequestTimeout time.Duration
	HTTPClient     *http.Client
	DryRun         bool
	Logger         *slog.Logger
}

// TelegramNotifier implements domain.ReviewNotifier using Telegram's Bot API.
type TelegramNotifier struct {
	botToken       string
	chatID         string
	apiBaseURL     string
	requestTimeout time.Duration
	httpClient     *http.Client
	dryRun         bool
	logger         *slog.Logger
	breaker        *resilience.CircuitBreaker
}

// NewTelegramNotifier constructs a TelegramNotifier adapter.
func NewTelegramNotifier(config TelegramConfig) *TelegramNotifier {
	baseURL := strings.TrimRight(strings.TrimSpace(config.APIBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultTelegramAPIBase
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

	return &TelegramNotifier{
		botToken:       strings.TrimSpace(config.BotToken),
		chatID:         strings.TrimSpace(config.ChatID),
		apiBaseURL:     baseURL,
		requestTimeout: timeout,
		httpClient:     httpClient,
		dryRun:         config.DryRun || strings.TrimSpace(config.BotToken) == "" || strings.TrimSpace(config.ChatID) == "",
		logger:         logger,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "telegram-bot-api",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}
}

// ChannelName returns "telegram".
func (notifier *TelegramNotifier) ChannelName() string {
	return "telegram"
}

// SendForReview sends an HTML-formatted review summary with inline Approve/Reject keyboard buttons.
func (notifier *TelegramNotifier) SendForReview(ctx context.Context, batch *domain.DraftBatch) (string, error) {
	if batch == nil {
		return "", fmt.Errorf("%w: batch cannot be nil", domain.ErrValidationFailed)
	}
	if notifier.dryRun {
		notifier.logger.Info("dry-run telegram review card", "batch_id", batch.ID, "commit_sha", batch.CommitEvent.CommitSHA)
		return "dryrun-telegram-" + batch.ID, nil
	}

	payload := buildTelegramPayload(notifier.chatID, batch)
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", notifier.apiBaseURL, notifier.botToken)

	var respEnvelope struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := notifier.doTelegramRequest(ctx, endpoint, payload, &respEnvelope); err != nil {
		return "", fmt.Errorf("send telegram review message: %w", err)
	}
	return strconv.FormatInt(respEnvelope.Result.MessageID, 10), nil
}

// UpdateReviewStatus edits the existing Telegram message with updated draft statuses and buttons.
func (notifier *TelegramNotifier) UpdateReviewStatus(ctx context.Context, batch *domain.DraftBatch) error {
	if batch == nil || strings.TrimSpace(batch.TelegramMessageID) == "" {
		return nil
	}
	if notifier.dryRun || strings.HasPrefix(batch.TelegramMessageID, "dryrun-") {
		notifier.logger.Info("dry-run telegram review update", "batch_id", batch.ID, "message_id", batch.TelegramMessageID)
		return nil
	}

	messageID, err := strconv.ParseInt(batch.TelegramMessageID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid telegram message id %q: %w", batch.TelegramMessageID, err)
	}

	payload := buildTelegramPayload(notifier.chatID, batch)
	payload["message_id"] = messageID
	endpoint := fmt.Sprintf("%s/bot%s/editMessageText", notifier.apiBaseURL, notifier.botToken)
	if err := notifier.doTelegramRequest(ctx, endpoint, payload, nil); err != nil {
		return fmt.Errorf("update telegram review message: %w", err)
	}
	return nil
}

// AnswerCallbackQuery acknowledges an interactive button click to stop Telegram's loading spinner.
func (notifier *TelegramNotifier) AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string) error {
	if notifier.dryRun || strings.TrimSpace(callbackQueryID) == "" {
		return nil
	}
	endpoint := fmt.Sprintf("%s/bot%s/answerCallbackQuery", notifier.apiBaseURL, notifier.botToken)
	return notifier.doTelegramRequest(ctx, endpoint, map[string]any{
		"callback_query_id": callbackQueryID,
		"text":              truncate(text, 180),
	}, nil)
}

func (notifier *TelegramNotifier) doTelegramRequest(ctx context.Context, endpoint string, body any, out any) error {
	rawPayload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal telegram payload: %w", err)
	}

	return notifier.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, notifier.requestTimeout)
		defer cancel()

		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(rawPayload))
		if reqErr != nil {
			return fmt.Errorf("build telegram request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, doErr := notifier.httpClient.Do(req)
		if doErr != nil {
			return fmt.Errorf("execute telegram request: %w", doErr)
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		if out != nil && len(respBody) > 0 {
			if decodeErr := json.Unmarshal(respBody, out); decodeErr != nil {
				return fmt.Errorf("decode telegram response: %w", decodeErr)
			}
		}
		return nil
	})
}

func buildTelegramPayload(chatID string, batch *domain.DraftBatch) map[string]any {
	var textBuilder strings.Builder
	fmt.Fprintf(
		&textBuilder,
		"<b>DigitalClone Content Review</b>\n<b>Repo:</b> <code>%s</code> (%s)\n<b>Commit:</b> <code>%s</code> — %s\n\n",
		html.EscapeString(batch.CommitEvent.RepoName),
		html.EscapeString(batch.CommitEvent.Branch),
		html.EscapeString(shortHash(batch.CommitEvent.CommitSHA)),
		html.EscapeString(truncate(batch.CommitEvent.Message, 140)),
	)

	var keyboard [][]map[string]any
	hasPending := false
	var platformButtons [][]map[string]any

	for _, platform := range domain.AllPlatforms() {
		draft, exists := batch.Drafts[platform]
		if !exists {
			continue
		}
		fmt.Fprintf(
			&textBuilder,
			"<b>%s</b> [<code>%s</code>]\n%s\n",
			html.EscapeString(displayPlatformName(platform)),
			html.EscapeString(string(draft.Status)),
			html.EscapeString(truncate(draft.BodyText, 500)),
		)
		if draft.MediaAsset != nil && draft.MediaAsset.PublicURL != "" {
			fmt.Fprintf(&textBuilder, "<a href=\"%s\">Media Preview (%s)</a>\n", html.EscapeString(draft.MediaAsset.PublicURL), html.EscapeString(string(draft.MediaAsset.MediaType)))
		}
		if draft.ExternalPostID != "" {
			fmt.Fprintf(&textBuilder, "<i>Post ID:</i> <code>%s</code>\n", html.EscapeString(draft.ExternalPostID))
		}
		if draft.FailureReason != "" {
			fmt.Fprintf(&textBuilder, "<i>Failure:</i> <code>%s</code>\n", html.EscapeString(truncate(draft.FailureReason, 200)))
		}
		textBuilder.WriteString("\n")

		if draft.Status == domain.StatusPendingReview || draft.Status == domain.StatusFailed {
			hasPending = true
			approveLabel := "Approve " + displayPlatformName(platform)
			if draft.Status == domain.StatusFailed {
				approveLabel = "Retry " + displayPlatformName(platform)
			}
			// Compact callback_data keeps payloads safely within Telegram's 64-byte callback limit.
			platformButtons = append(platformButtons, []map[string]any{
				{
					"text":          approveLabel,
					"callback_data": fmt.Sprintf("rv|%s|ap|%s", batch.ID, platform),
				},
				{
					"text":          "Reject " + displayPlatformName(platform),
					"callback_data": fmt.Sprintf("rv|%s|rj|%s", batch.ID, platform),
				},
			})
		}
	}

	if hasPending {
		keyboard = append(keyboard, []map[string]any{
			{
				"text":          "Approve All",
				"callback_data": fmt.Sprintf("rv|%s|ap|all", batch.ID),
			},
			{
				"text":          "Reject All",
				"callback_data": fmt.Sprintf("rv|%s|rj|all", batch.ID),
			},
		})
		keyboard = append(keyboard, platformButtons...)
	}

	payload := map[string]any{
		"chat_id":    chatID,
		"text":       truncate(textBuilder.String(), 4000),
		"parse_mode": "HTML",
	}
	if len(keyboard) > 0 {
		payload["reply_markup"] = map[string]any{
			"inline_keyboard": keyboard,
		}
	}
	return payload
}
