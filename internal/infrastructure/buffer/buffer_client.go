// Package buffer implements a resilient GraphQL client for the Buffer API (https://api.buffer.com).
package buffer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/pkg/resilience"
)

const defaultEndpoint = "https://api.buffer.com"

// ShareMode defines Buffer's scheduling behavior when creating a post.
type ShareMode string

const (
	// ShareModeShareNow publishes immediately to the target channel.
	ShareModeShareNow ShareMode = "shareNow"
	// ShareModeAddToQueue places the post in the next open queue slot.
	ShareModeAddToQueue ShareMode = "addToQueue"
	// ShareModeShareNext places the post at the top of the channel queue.
	ShareModeShareNext ShareMode = "shareNext"
	// ShareModeCustomScheduled schedules for an explicit ISO 8601 UTC dueAt timestamp.
	ShareModeCustomScheduled ShareMode = "customScheduled"
)

// Config configures the Buffer GraphQL client.
type Config struct {
	APIKey         string
	Endpoint       string
	OrganizationID string
	ChannelIDs     map[domain.Platform]string
	ShareMode      ShareMode
	SchedulingType string
	RequestTimeout time.Duration
	HTTPClient     *http.Client
	DryRun         bool
	Logger         *slog.Logger
}

// AssetInput describes a publicly reachable HTTPS media attachment for Buffer createPost.
type AssetInput struct {
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
}

// CreatePostParams contains normalized parameters for scheduling or publishing via Buffer.
type CreatePostParams struct {
	Platform   domain.Platform
	Text       string
	Assets     []AssetInput
	ShareMode  ShareMode
	DueAt      string
	SaveDraft  bool
}

// Client wraps the Buffer GraphQL API with channel caching, rate-limit tracking, and circuit breaking.
type Client struct {
	apiKey         string
	endpoint       string
	organizationID string
	shareMode      ShareMode
	schedulingType string
	requestTimeout time.Duration
	httpClient     *http.Client
	dryRun         bool
	logger         *slog.Logger
	breaker        *resilience.CircuitBreaker

	mu              sync.RWMutex
	channelByServ   map[string]string
	explicitChannel map[domain.Platform]string
}

// NewClient constructs a Buffer GraphQL API client.
func NewClient(config Config) *Client {
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	shareMode := config.ShareMode
	if shareMode == "" {
		shareMode = ShareModeShareNow
	}
	schedulingType := strings.TrimSpace(config.SchedulingType)
	if schedulingType == "" {
		schedulingType = "automatic"
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		// Enforce GEMINI.md 5s max timeout boundary per outbound call.
		requestTimeout = 5 * time.Second
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	explicit := make(map[domain.Platform]string, len(config.ChannelIDs))
	for platform, channelID := range config.ChannelIDs {
		if strings.TrimSpace(channelID) != "" {
			explicit[platform] = strings.TrimSpace(channelID)
		}
	}

	return &Client{
		apiKey:          strings.TrimSpace(config.APIKey),
		endpoint:        endpoint,
		organizationID:  strings.TrimSpace(config.OrganizationID),
		shareMode:       shareMode,
		schedulingType:  schedulingType,
		requestTimeout:  requestTimeout,
		httpClient:      httpClient,
		dryRun:          config.DryRun || strings.TrimSpace(config.APIKey) == "",
		logger:          logger,
		channelByServ:   make(map[string]string),
		explicitChannel: explicit,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "buffer-graphql-api",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}
}

// CreatePost resolves the target Buffer channelId and invokes Buffer's createPost GraphQL mutation.
func (client *Client) CreatePost(ctx context.Context, params CreatePostParams) (string, error) {
	if strings.TrimSpace(params.Text) == "" {
		return "", fmt.Errorf("%w: buffer post text cannot be empty", domain.ErrValidationFailed)
	}
	if client.dryRun {
		return fmt.Sprintf("dryrun_buffer_%s_%d", params.Platform, time.Now().UTC().UnixNano()), nil
	}

	channelID, err := client.ResolveChannelID(ctx, params.Platform)
	if err != nil {
		return "", err
	}

	mode := params.ShareMode
	if mode == "" {
		mode = client.shareMode
	}

	input := map[string]any{
		"channelId":      channelID,
		"text":           params.Text,
		"schedulingType": client.schedulingType,
		"mode":           string(mode),
	}
	if params.SaveDraft {
		input["saveToDraft"] = true
	}
	if mode == ShareModeCustomScheduled && strings.TrimSpace(params.DueAt) != "" {
		input["dueAt"] = strings.TrimSpace(params.DueAt)
	}
	if len(params.Assets) > 0 {
		input["assets"] = params.Assets
	}

	const mutation = `
		mutation SchedulePost($input: CreatePostInput!) {
			createPost(input: $input) {
				__typename
				... on PostActionSuccess {
					post {
						id
						text
						dueAt
						status
					}
				}
				... on MutationError {
					message
				}
			}
		}
	`

	var responsePayload struct {
		CreatePost struct {
			Typename string `json:"__typename"`
			Message  string `json:"message"`
			Post     *struct {
				ID     string `json:"id"`
				Text   string `json:"text"`
				DueAt  string `json:"dueAt"`
				Status string `json:"status"`
			} `json:"post"`
		} `json:"createPost"`
	}

	if err := client.executeGraphQL(ctx, mutation, map[string]any{"input": input}, &responsePayload); err != nil {
		return "", err
	}

	if responsePayload.CreatePost.Typename == "MutationError" || responsePayload.CreatePost.Message != "" {
		return "", fmt.Errorf("buffer createPost mutation error: %s", responsePayload.CreatePost.Message)
	}
	if responsePayload.CreatePost.Post == nil || responsePayload.CreatePost.Post.ID == "" {
		return "", fmt.Errorf("buffer createPost returned empty post payload")
	}

	return responsePayload.CreatePost.Post.ID, nil
}

// ResolveChannelID returns an explicitly configured channelId or queries Buffer account/channels once.
func (client *Client) ResolveChannelID(ctx context.Context, platform domain.Platform) (string, error) {
	serviceName, err := bufferServiceName(platform)
	if err != nil {
		return "", err
	}

	client.mu.RLock()
	if explicitID := client.explicitChannel[platform]; explicitID != "" {
		client.mu.RUnlock()
		return explicitID, nil
	}
	if cachedID := client.channelByServ[serviceName]; cachedID != "" {
		client.mu.RUnlock()
		return cachedID, nil
	}
	client.mu.RUnlock()

	orgID := client.organizationID
	if orgID == "" {
		orgID, err = client.fetchDefaultOrganizationID(ctx)
		if err != nil {
			return "", err
		}
	}

	const channelsQuery = `
		query GetChannels($orgId: OrganizationId!) {
			channels(input: { organizationId: $orgId }) {
				id
				name
				displayName
				service
				isQueuePaused
			}
		}
	`
	var channelsResp struct {
		Channels []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			DisplayName   string `json:"displayName"`
			Service       string `json:"service"`
			IsQueuePaused bool   `json:"isQueuePaused"`
		} `json:"channels"`
	}
	if err := client.executeGraphQL(ctx, channelsQuery, map[string]any{"orgId": orgID}, &channelsResp); err != nil {
		return "", fmt.Errorf("query buffer channels: %w", err)
	}

	client.mu.Lock()
	client.organizationID = orgID
	for _, channel := range channelsResp.Channels {
		normalizedService := strings.ToLower(strings.TrimSpace(channel.Service))
		if _, exists := client.channelByServ[normalizedService]; !exists && channel.ID != "" {
			client.channelByServ[normalizedService] = channel.ID
		}
	}
	resolvedID := client.channelByServ[serviceName]
	client.mu.Unlock()

	if resolvedID == "" {
		return "", fmt.Errorf("no connected Buffer channel found for service %q (organization %s)", serviceName, orgID)
	}
	return resolvedID, nil
}

func (client *Client) fetchDefaultOrganizationID(ctx context.Context) (string, error) {
	const hierarchyQuery = `
		query GetAccountHierarchy {
			account {
				id
				organizations {
					id
					name
					isOwner
				}
			}
		}
	`
	var accountResp struct {
		Account struct {
			ID            string `json:"id"`
			Organizations []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				IsOwner bool   `json:"isOwner"`
			} `json:"organizations"`
		} `json:"account"`
	}
	if err := client.executeGraphQL(ctx, hierarchyQuery, nil, &accountResp); err != nil {
		return "", fmt.Errorf("query buffer organizations: %w", err)
	}
	if len(accountResp.Account.Organizations) == 0 {
		return "", fmt.Errorf("no Buffer organizations found on account %s", accountResp.Account.ID)
	}
	for _, org := range accountResp.Account.Organizations {
		if org.IsOwner && org.ID != "" {
			return org.ID, nil
		}
	}
	return accountResp.Account.Organizations[0].ID, nil
}

func (client *Client) executeGraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	payload := map[string]any{
		"query": query,
	}
	if variables != nil {
		payload["variables"] = variables
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal buffer graphql request: %w", err)
	}

	return client.breaker.Execute(ctx, func(breakerCtx context.Context) error {
		return resilience.RetryWithBackoff(breakerCtx, resilience.RetryConfig{
			MaxAttempts: 3,
			BaseDelay:   500 * time.Millisecond,
			MaxDelay:    15 * time.Second,
			ShouldRetry: func(err error) bool {
				return errors.Is(err, domain.ErrRateLimited)
			},
		}, func(attemptCtx context.Context) error {
			reqCtx, cancel := context.WithTimeout(attemptCtx, client.requestTimeout)
			defer cancel()

			req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, client.endpoint, bytes.NewReader(bodyBytes))
			if reqErr != nil {
				return fmt.Errorf("build buffer request: %w", reqErr)
			}
			req.Header.Set("Authorization", "Bearer "+client.apiKey)
			req.Header.Set("Content-Type", "application/json")

			resp, doErr := client.httpClient.Do(req)
			if doErr != nil {
				return fmt.Errorf("execute buffer graphql request: %w", doErr)
			}
			defer func() { _ = resp.Body.Close() }()

			if rateHeader := resp.Header.Get("RateLimit"); rateHeader != "" {
				client.logger.Debug("buffer rate limit telemetry", "rate_limit", rateHeader)
			}

			rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if readErr != nil {
				return fmt.Errorf("read buffer response: %w", readErr)
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				retryDelay := parseRetryAfter(resp.Header.Get("Retry-After"))
				return &resilience.RetryAfterError{
					Delay: retryDelay,
					Err:   fmt.Errorf("%w: buffer HTTP 429: %s", domain.ErrRateLimited, strings.TrimSpace(string(rawBody))),
				}
			}

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("buffer graphql HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawBody)))
			}

			var envelope struct {
				Data   json.RawMessage `json:"data"`
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			}
			if unmarshalErr := json.Unmarshal(rawBody, &envelope); unmarshalErr != nil {
				return fmt.Errorf("decode buffer graphql envelope: %w", unmarshalErr)
			}
			if len(envelope.Errors) > 0 {
				messages := make([]string, 0, len(envelope.Errors))
				for _, gqlErr := range envelope.Errors {
					messages = append(messages, gqlErr.Message)
				}
				return fmt.Errorf("buffer graphql errors: %s", strings.Join(messages, "; "))
			}

			if out != nil && len(envelope.Data) > 0 {
				if decodeErr := json.Unmarshal(envelope.Data, out); decodeErr != nil {
					return fmt.Errorf("decode buffer graphql data: %w", decodeErr)
				}
			}
			return nil
		})
	})
}

func bufferServiceName(platform domain.Platform) (string, error) {
	switch platform {
	case domain.PlatformLinkedIn:
		return "linkedin", nil
	case domain.PlatformX:
		return "twitter", nil
	case domain.PlatformTikTok:
		return "tiktok", nil
	default:
		return "", fmt.Errorf("%w: platform %s is not routed via Buffer", domain.ErrValidationFailed, platform)
	}
}

func parseRetryAfter(rawHeader string) time.Duration {
	trimmed := strings.TrimSpace(rawHeader)
	if trimmed == "" {
		return 2 * time.Second
	}
	if seconds, err := strconv.Atoi(trimmed); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 2 * time.Second
}
