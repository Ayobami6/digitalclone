// Package publishers implements the 4 platform-specific driven publishing services:
// LinkedInPostService, XPostService, TikTokPostService (via Buffer GraphQL API),
// and YouTubePostService (via YouTube Data API v3).
package publishers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/buffer"
	"digitalclone/pkg/resilience"
)

// BufferPostPublisher publishes social posts for a specific platform through Buffer's GraphQL API.
type BufferPostPublisher struct {
	platform     domain.Platform
	bufferClient *buffer.Client
}

// NewLinkedInPostService constructs the LinkedInPostService backed by Buffer.
func NewLinkedInPostService(bufferClient *buffer.Client) *BufferPostPublisher {
	return &BufferPostPublisher{platform: domain.PlatformLinkedIn, bufferClient: bufferClient}
}

// NewXPostService constructs the XPostService backed by Buffer.
func NewXPostService(bufferClient *buffer.Client) *BufferPostPublisher {
	return &BufferPostPublisher{platform: domain.PlatformX, bufferClient: bufferClient}
}

// NewTikTokPostService constructs the TikTokPostService backed by Buffer.
func NewTikTokPostService(bufferClient *buffer.Client) *BufferPostPublisher {
	return &BufferPostPublisher{platform: domain.PlatformTikTok, bufferClient: bufferClient}
}

// Platform identifies which social network this service publishes to.
func (service *BufferPostPublisher) Platform() domain.Platform {
	return service.platform
}

// Publish formats post text + public media asset and dispatches via Buffer's createPost mutation.
func (service *BufferPostPublisher) Publish(ctx context.Context, draft *domain.ContentDraft) (string, error) {
	if draft == nil {
		return "", fmt.Errorf("%w: draft cannot be nil", domain.ErrValidationFailed)
	}

	postText := formatSocialBody(draft)
	var assets []buffer.AssetInput
	if draft.MediaAsset != nil && strings.TrimSpace(draft.MediaAsset.PublicURL) != "" {
		mimeType := strings.TrimSpace(draft.MediaAsset.MimeType)
		if mimeType == "" {
			if draft.MediaAsset.MediaType == domain.MediaVideo {
				mimeType = "video/mp4"
			} else {
				mimeType = "image/png"
			}
		}
		assets = append(assets, buffer.AssetInput{
			URL:      strings.TrimSpace(draft.MediaAsset.PublicURL),
			MimeType: mimeType,
		})
	}

	return service.bufferClient.CreatePost(ctx, buffer.CreatePostParams{
		Platform: service.platform,
		Text:     postText,
		Assets:   assets,
	})
}

// YouTubeConfig configures direct YouTube Data API v3 video uploads.
type YouTubeConfig struct {
	ClientID       string
	ClientSecret   string
	RefreshToken   string
	AccessToken    string
	PrivacyStatus  string
	CategoryID     string
	Endpoint       string
	HTTPClient     *http.Client
	DryRun         bool
	RequestTimeout time.Duration
}

// YouTubePostService publishes video drafts directly via google.golang.org/api/youtube/v3.
type YouTubePostService struct {
	config  YouTubeConfig
	breaker *resilience.CircuitBreaker
}

// NewYouTubePostService constructs the YouTubePostService adapter.
func NewYouTubePostService(config YouTubeConfig) *YouTubePostService {
	if strings.TrimSpace(config.PrivacyStatus) == "" {
		config.PrivacyStatus = "unlisted"
	}
	if strings.TrimSpace(config.CategoryID) == "" {
		// Category "28" corresponds to Science & Technology on YouTube.
		config.CategoryID = "28"
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 2 * time.Minute
	}
	return &YouTubePostService{
		config: config,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "youtube-data-api",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}
}

// Platform identifies YouTube as the target network.
func (service *YouTubePostService) Platform() domain.Platform {
	return domain.PlatformYouTube
}

// Publish uploads the draft's local video file to YouTube using Videos.Insert.
func (service *YouTubePostService) Publish(ctx context.Context, draft *domain.ContentDraft) (string, error) {
	if draft == nil {
		return "", fmt.Errorf("%w: youtube draft cannot be nil", domain.ErrValidationFailed)
	}
	if service.config.DryRun || (strings.TrimSpace(service.config.RefreshToken) == "" && strings.TrimSpace(service.config.AccessToken) == "") {
		return fmt.Sprintf("dryrun_youtube_%d", time.Now().UTC().UnixNano()), nil
	}
	if draft.MediaAsset == nil || strings.TrimSpace(draft.MediaAsset.LocalPath) == "" {
		return "", fmt.Errorf("%w: youtube publishing requires a local video file path", domain.ErrValidationFailed)
	}

	var uploadedVideoID string
	err := service.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, service.config.RequestTimeout)
		defer cancel()

		mediaReader, mimeType, cleanup, openErr := openVideoStream(draft.MediaAsset.LocalPath, draft.MediaAsset.MimeType)
		if openErr != nil {
			return openErr
		}
		defer cleanup()

		clientOptions, clientErr := service.buildClientOptions(reqCtx)
		if clientErr != nil {
			return clientErr
		}

		ytService, svcErr := youtube.NewService(reqCtx, clientOptions...)
		if svcErr != nil {
			return fmt.Errorf("create youtube service: %w", svcErr)
		}

		title := strings.TrimSpace(draft.Title)
		if title == "" {
			title = "DigitalClone Developer Update"
		}
		if len(title) > 100 {
			title = title[:100]
		}

		videoMetadata := &youtube.Video{
			Snippet: &youtube.VideoSnippet{
				Title:       title,
				Description: formatSocialBody(draft),
				Tags:        append([]string(nil), draft.Tags...),
				CategoryId:  service.config.CategoryID,
			},
			Status: &youtube.VideoStatus{
				PrivacyStatus: service.config.PrivacyStatus,
			},
		}

		resp, insertErr := ytService.Videos.
			Insert([]string{"snippet", "status"}, videoMetadata).
			Media(mediaReader, googleapi.ContentType(mimeType)).
			Context(reqCtx).
			Do()
		if insertErr != nil {
			return fmt.Errorf("youtube Videos.Insert failed: %w", insertErr)
		}
		if resp == nil || resp.Id == "" {
			return fmt.Errorf("youtube Videos.Insert returned empty video id")
		}
		uploadedVideoID = resp.Id
		return nil
	})
	return uploadedVideoID, err
}

func (service *YouTubePostService) buildClientOptions(ctx context.Context) ([]option.ClientOption, error) {
	if service.config.HTTPClient != nil && strings.TrimSpace(service.config.Endpoint) != "" {
		return []option.ClientOption{
			option.WithHTTPClient(service.config.HTTPClient),
			option.WithEndpoint(service.config.Endpoint),
		}, nil
	}

	var tokenSource oauth2.TokenSource
	if strings.TrimSpace(service.config.RefreshToken) != "" {
		oauthCfg := &oauth2.Config{
			ClientID:     service.config.ClientID,
			ClientSecret: service.config.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://accounts.google.com/o/oauth2/auth",
				TokenURL: "https://oauth2.googleapis.com/token",
			},
			Scopes: []string{youtube.YoutubeUploadScope},
		}
		tokenSource = oauthCfg.TokenSource(ctx, &oauth2.Token{
			RefreshToken: service.config.RefreshToken,
			AccessToken:  service.config.AccessToken,
		})
	} else {
		tokenSource = oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: service.config.AccessToken,
		})
	}

	options := []option.ClientOption{option.WithTokenSource(tokenSource)}
	if strings.TrimSpace(service.config.Endpoint) != "" {
		options = append(options, option.WithEndpoint(service.config.Endpoint))
	}
	return options, nil
}

func openVideoStream(localPath, mimeType string) (io.Reader, string, func(), error) {
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "video/mp4"
	}
	file, err := os.Open(localPath)
	if err != nil {
		return nil, "", func() {}, fmt.Errorf("open youtube video file %s: %w", localPath, err)
	}
	// Verify file is non-empty before streaming to YouTube upload endpoint.
	info, statErr := file.Stat()
	if statErr != nil || info.Size() == 0 {
		_ = file.Close()
		return nil, "", func() {}, fmt.Errorf("%w: youtube video file %s is empty or unreadable", domain.ErrValidationFailed, localPath)
	}
	// Reset cursor defensively for small streams or buffered tests.
	if info.Size() < 1024 {
		data, readErr := io.ReadAll(file)
		_ = file.Close()
		if readErr != nil {
			return nil, "", func() {}, fmt.Errorf("read video file: %w", readErr)
		}
		return bytes.NewReader(data), mimeType, func() {}, nil
	}
	return file, mimeType, func() { _ = file.Close() }, nil
}

func formatSocialBody(draft *domain.ContentDraft) string {
	body := strings.TrimSpace(draft.BodyText)
	if len(draft.Tags) == 0 {
		return body
	}
	hashtags := make([]string, 0, len(draft.Tags))
	for _, tag := range draft.Tags {
		cleaned := strings.TrimLeft(strings.TrimSpace(tag), "#")
		if cleaned != "" && !strings.Contains(body, "#"+cleaned) {
			hashtags = append(hashtags, "#"+cleaned)
		}
	}
	if len(hashtags) == 0 {
		return body
	}
	return fmt.Sprintf("%s\n\n%s", body, strings.Join(hashtags, " "))
}
