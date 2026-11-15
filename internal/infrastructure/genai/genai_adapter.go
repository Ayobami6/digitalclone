// Package genai implements domain.MediaGenerator backed by Google GenAI
// (Gemini text, Imagen images, and Veo videos) with circuit-breaker protection
// and deterministic dry-run fallbacks.
package genai

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"digitalclone/pkg/resilience"
)

// minimalPNGBytes is a valid 1x1 transparent PNG used in dry-run mode.
const minimalPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

// Config configures models, API credentials, and timeouts for Google GenAI calls.
type Config struct {
	APIKey         string
	TextModel      string
	ImageModel     string
	VideoModel     string
	RequestTimeout time.Duration
	VideoTimeout   time.Duration
	DryRun         bool
}

// Adapter implements domain.MediaGenerator using google.golang.org/genai.
type Adapter struct {
	client         *genai.Client
	textModel      string
	imageModel     string
	videoModel     string
	requestTimeout time.Duration
	videoTimeout   time.Duration
	dryRun         bool
	breaker        *resilience.CircuitBreaker
}

// NewAdapter initializes a Google GenAI adapter. When DryRun is true or APIKey is empty,
// deterministic synthetic assets are returned for local verification.
func NewAdapter(ctx context.Context, config Config) (*Adapter, error) {
	if strings.TrimSpace(config.TextModel) == "" {
		config.TextModel = "gemini-2.5-flash"
	}
	if strings.TrimSpace(config.ImageModel) == "" {
		config.ImageModel = "imagen-3.0-generate-002"
	}
	if strings.TrimSpace(config.VideoModel) == "" {
		config.VideoModel = "veo-2.0-generate-001"
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.VideoTimeout <= 0 {
		config.VideoTimeout = 3 * time.Minute
	}

	var client *genai.Client
	if !config.DryRun && strings.TrimSpace(config.APIKey) != "" {
		createdClient, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey: config.APIKey,
		})
		if err != nil {
			return nil, fmt.Errorf("create genai client: %w", err)
		}
		client = createdClient
	}

	return &Adapter{
		client:         client,
		textModel:      config.TextModel,
		imageModel:     config.ImageModel,
		videoModel:     config.VideoModel,
		requestTimeout: config.RequestTimeout,
		videoTimeout:   config.VideoTimeout,
		dryRun:         config.DryRun || client == nil,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "google-genai",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}, nil
}

// GenerateText synthesizes platform copy using Gemini Models.GenerateContent.
func (adapter *Adapter) GenerateText(ctx context.Context, systemInstruction, prompt string) (string, error) {
	if adapter.dryRun {
		return synthesizeDryRunText(prompt), nil
	}

	var output string
	err := adapter.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, adapter.requestTimeout)
		defer cancel()

		var genConfig *genai.GenerateContentConfig
		if strings.TrimSpace(systemInstruction) != "" {
			genConfig = &genai.GenerateContentConfig{
				SystemInstruction: genai.NewContentFromText(systemInstruction, genai.RoleUser),
			}
		}

		resp, genErr := adapter.client.Models.GenerateContent(
			reqCtx,
			adapter.textModel,
			[]*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
			genConfig,
		)
		if genErr != nil {
			return fmt.Errorf("genai GenerateContent: %w", genErr)
		}
		output = strings.TrimSpace(resp.Text())
		if output == "" {
			return fmt.Errorf("genai returned empty text response")
		}
		return nil
	})
	return output, err
}

// GenerateImage synthesizes an image via Google GenAI Imagen and returns PNG bytes.
func (adapter *Adapter) GenerateImage(ctx context.Context, prompt, aspectRatio string) ([]byte, string, error) {
	if adapter.dryRun {
		pngBytes, err := base64.StdEncoding.DecodeString(minimalPNGBase64)
		if err != nil {
			return nil, "", fmt.Errorf("decode dry-run png: %w", err)
		}
		return pngBytes, "image/png", nil
	}
	if strings.TrimSpace(aspectRatio) == "" {
		aspectRatio = "16:9"
	}

	var (
		imageBytes []byte
		mimeType   string
	)
	err := adapter.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, adapter.requestTimeout)
		defer cancel()

		resp, genErr := adapter.client.Models.GenerateImages(
			reqCtx,
			adapter.imageModel,
			prompt,
			&genai.GenerateImagesConfig{
				NumberOfImages: 1,
				AspectRatio:    aspectRatio,
				OutputMIMEType: "image/png",
			},
		)
		if genErr != nil {
			return fmt.Errorf("genai GenerateImages: %w", genErr)
		}
		if len(resp.GeneratedImages) == 0 || resp.GeneratedImages[0].Image == nil || len(resp.GeneratedImages[0].Image.ImageBytes) == 0 {
			return fmt.Errorf("genai GenerateImages returned no image bytes")
		}
		imageBytes = resp.GeneratedImages[0].Image.ImageBytes
		mimeType = resp.GeneratedImages[0].Image.MIMEType
		if mimeType == "" {
			mimeType = "image/png"
		}
		return nil
	})
	return imageBytes, mimeType, err
}

// GenerateVideo synthesizes a short-form video via Google GenAI Veo and polls until completion.
func (adapter *Adapter) GenerateVideo(ctx context.Context, prompt, aspectRatio string) ([]byte, string, error) {
	if adapter.dryRun {
		// Minimal valid ISO Base Media File Format header for dry-run pipelines and local tests.
		dryRunMP4 := []byte{
			0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p',
			'm', 'p', '4', '2', 0x00, 0x00, 0x00, 0x00,
			'm', 'p', '4', '2', 'i', 's', 'o', 'm',
		}
		return dryRunMP4, "video/mp4", nil
	}
	if strings.TrimSpace(aspectRatio) == "" {
		aspectRatio = "9:16"
	}

	var (
		videoBytes []byte
		mimeType   string
	)
	err := adapter.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, adapter.videoTimeout)
		defer cancel()

		operation, genErr := adapter.client.Models.GenerateVideosFromSource(
			reqCtx,
			adapter.videoModel,
			&genai.GenerateVideosSource{Prompt: prompt},
			&genai.GenerateVideosConfig{
				NumberOfVideos: 1,
				AspectRatio:    aspectRatio,
			},
		)
		if genErr != nil {
			return fmt.Errorf("genai GenerateVideosFromSource: %w", genErr)
		}

		for !operation.Done {
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-reqCtx.Done():
				timer.Stop()
				return reqCtx.Err()
			case <-timer.C:
			}
			polledOp, pollErr := adapter.client.Operations.GetVideosOperation(reqCtx, operation, nil)
			if pollErr != nil {
				return fmt.Errorf("poll video generation operation: %w", pollErr)
			}
			operation = polledOp
		}

		if len(operation.Error) > 0 {
			return fmt.Errorf("video generation failed: %v", operation.Error)
		}
		if operation.Response == nil || len(operation.Response.GeneratedVideos) == 0 || operation.Response.GeneratedVideos[0].Video == nil {
			return fmt.Errorf("genai video operation completed without video payload")
		}

		generatedVideo := operation.Response.GeneratedVideos[0]
		mimeType = generatedVideo.Video.MIMEType
		if mimeType == "" {
			mimeType = "video/mp4"
		}
		if len(generatedVideo.Video.VideoBytes) > 0 {
			videoBytes = generatedVideo.Video.VideoBytes
			return nil
		}

		downloaded, dlErr := adapter.client.Files.Download(reqCtx, generatedVideo, nil)
		if dlErr != nil {
			return fmt.Errorf("download generated video: %w", dlErr)
		}
		videoBytes = downloaded
		return nil
	})
	return videoBytes, mimeType, err
}

func synthesizeDryRunText(prompt string) string {
	lines := strings.Split(strings.TrimSpace(prompt), "\n")
	platform := "social"
	commitLine := "Latest engineering milestone"
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Platform:") {
			platform = strings.TrimSpace(strings.TrimPrefix(trimmed, "Platform:"))
		}
		if strings.HasPrefix(trimmed, "Commit Message:") {
			commitLine = strings.TrimSpace(strings.TrimPrefix(trimmed, "Commit Message:"))
		}
	}
	return fmt.Sprintf("[%s] Just shipped: %s. Built with Go, Google ADK parallel agents, and clean Hexagonal ports.", strings.ToUpper(platform), commitLine)
}
