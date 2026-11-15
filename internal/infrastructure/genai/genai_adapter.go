// Package genai implements domain.MediaGenerator backed by Google GenAI
// (Gemini text, Gemini/Imagen images, and Gemini Omni/Veo videos) with
// circuit-breaker protection and deterministic dry-run fallbacks.
package genai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/genai"
	"google.golang.org/genai/interactions/models/interactions"
	"google.golang.org/genai/interactions/models/operations"

	"digitalclone/pkg/resilience"
)

const (
	// minimalPNGBytes is a valid 1x1 transparent PNG used in dry-run mode.
	minimalPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

	minVideoDurationSeconds  = 180
	maxVideoDurationSeconds  = 300
	omniMaxClipDurationSecs  = 15
	maxConcurrentVideoClips  = 3
	perClipGenerationTimeout = 5 * time.Minute
)

// VideoStitcherFunc combines ordered MP4 clip payloads into a single MP4 video.
type VideoStitcherFunc func(ctx context.Context, clips [][]byte) ([]byte, error)

// Config configures models, API credentials, and timeouts for Google GenAI calls.
type Config struct {
	APIKey               string
	TextModel            string
	ImageModel           string
	VideoModel           string
	VideoDurationSeconds int
	BaseURL              string
	HTTPClient           *http.Client
	RequestTimeout       time.Duration
	VideoTimeout         time.Duration
	VideoStitcher        VideoStitcherFunc
	DryRun               bool
}

// Adapter implements domain.MediaGenerator using google.golang.org/genai.
type Adapter struct {
	client               *genai.Client
	textModel            string
	imageModel           string
	videoModel           string
	videoDurationSeconds int
	requestTimeout       time.Duration
	videoTimeout         time.Duration
	videoStitcher        VideoStitcherFunc
	dryRun               bool
	breaker              *resilience.CircuitBreaker
}

// NewAdapter initializes a Google GenAI adapter. When DryRun is true or APIKey is empty,
// deterministic synthetic assets are returned for local verification.
func NewAdapter(ctx context.Context, config Config) (*Adapter, error) {
	if strings.TrimSpace(config.TextModel) == "" {
		config.TextModel = "gemini-2.5-flash"
	}
	if strings.TrimSpace(config.ImageModel) == "" {
		config.ImageModel = "gemini-nano-banana-2.1"
	}
	if strings.TrimSpace(config.VideoModel) == "" {
		config.VideoModel = "gemini-omni-1.2-flash"
	}
	videoDuration := clampVideoDurationSeconds(config.VideoDurationSeconds)
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 90 * time.Second
	}
	if config.VideoTimeout <= 0 {
		config.VideoTimeout = 20 * time.Minute
	}
	stitcher := config.VideoStitcher
	if stitcher == nil {
		stitcher = stitchMP4ClipsWithFFmpeg
	}

	var client *genai.Client
	if !config.DryRun && strings.TrimSpace(config.APIKey) != "" {
		clientConfig := &genai.ClientConfig{
			APIKey:     config.APIKey,
			HTTPClient: config.HTTPClient,
		}
		if strings.TrimSpace(config.BaseURL) != "" {
			clientConfig.HTTPOptions = genai.HTTPOptions{
				BaseURL: strings.TrimSpace(config.BaseURL),
			}
		}
		createdClient, err := genai.NewClient(ctx, clientConfig)
		if err != nil {
			return nil, fmt.Errorf("create genai client: %w", err)
		}
		client = createdClient
	}

	return &Adapter{
		client:               client,
		textModel:            config.TextModel,
		imageModel:           config.ImageModel,
		videoModel:           config.VideoModel,
		videoDurationSeconds: videoDuration,
		requestTimeout:       config.RequestTimeout,
		videoTimeout:         config.VideoTimeout,
		videoStitcher:        stitcher,
		dryRun:               config.DryRun || client == nil,
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

// GenerateImage synthesizes an image via Gemini image models (Interactions or GenerateContent)
// or Imagen (`GenerateImages`) depending on the configured ImageModel, and returns raw image bytes.
func (adapter *Adapter) GenerateImage(ctx context.Context, prompt, aspectRatio string) ([]byte, string, error) {
	if adapter.dryRun {
		pngBytes, err := base64.StdEncoding.DecodeString(minimalPNGBase64)
		if err != nil {
			return nil, "", fmt.Errorf("decode dry-run png: %w", err)
		}
		return pngBytes, "image/png", nil
	}
	aspectRatio = NormalizeAspectRatio(aspectRatio, "16:9")

	var (
		imageBytes []byte
		mimeType   string
	)
	err := adapter.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, adapter.requestTimeout)
		defer cancel()

		if isGeminiModel(adapter.imageModel) {
			bytesOut, mimeOut, genErr := adapter.generateImageWithGemini(reqCtx, adapter.imageModel, prompt, aspectRatio)
			if genErr != nil {
				return genErr
			}
			imageBytes = bytesOut
			mimeType = mimeOut
			return nil
		}

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

// GenerateVideo synthesizes a 3-5 minute video via Gemini Omni multi-scene assembly
// (15s clips stitched with ffmpeg) or Veo (`GenerateVideosFromSource`) depending on VideoModel.
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
	aspectRatio = NormalizeAspectRatio(aspectRatio, "9:16")

	var (
		videoBytes []byte
		mimeType   string
	)
	err := adapter.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, adapter.videoTimeout)
		defer cancel()

		if isGeminiModel(adapter.videoModel) {
			bytesOut, mimeOut, genErr := adapter.generateVideoWithGeminiOmni(reqCtx, adapter.videoModel, prompt, aspectRatio)
			if genErr != nil {
				return genErr
			}
			videoBytes = bytesOut
			mimeType = mimeOut
			return nil
		}

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

func isGeminiModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gemini-")
}

// generateImageWithGemini tries Interactions first and falls back to Models.GenerateContent
// across candidate Gemini image models (supporting deprecated gemini-3.1-flash-image fallback).
func (adapter *Adapter) generateImageWithGemini(ctx context.Context, configuredModel, prompt, aspectRatio string) ([]byte, string, error) {
	candidates := []string{strings.TrimSpace(configuredModel)}
	if strings.EqualFold(candidates[0], "gemini-3.1-flash-image") {
		// gemini-3.1-flash-image was deprecated on Oct 29, 2026 in favor of gemini-nano-banana-2.1.
		candidates = append(candidates, "gemini-nano-banana-2.1", "gemini-3.1-flash-lite-image")
	}

	var errs []error
	for _, candidateModel := range candidates {
		imgBytes, mimeType, interErr := adapter.generateImageViaInteractions(ctx, candidateModel, prompt, aspectRatio)
		if interErr == nil && len(imgBytes) > 0 {
			return imgBytes, mimeType, nil
		}
		if interErr != nil {
			errs = append(errs, fmt.Errorf("%s (interactions): %w", candidateModel, interErr))
		}

		imgBytes, mimeType, contentErr := adapter.generateImageViaGenerateContent(ctx, candidateModel, prompt, aspectRatio)
		if contentErr == nil && len(imgBytes) > 0 {
			return imgBytes, mimeType, nil
		}
		if contentErr != nil {
			errs = append(errs, fmt.Errorf("%s (generateContent): %w", candidateModel, contentErr))
		}
	}

	return nil, "", fmt.Errorf("gemini image generation failed: %w", errors.Join(errs...))
}

func (adapter *Adapter) generateImageViaInteractions(ctx context.Context, model, prompt, aspectRatio string) ([]byte, string, error) {
	modelInput := interactions.NewInteractionsInput([]interactions.Content{{
		TextContent: &interactions.TextContent{Text: prompt},
	}})
	aspect := interactions.ImageResponseFormatAspectRatio(aspectRatio)
	imgRespFmt := interactions.NewCreateModelInteractionResponseFormat(
		interactions.NewResponseFormat(interactions.ImageResponseFormat{
			AspectRatio: &aspect,
		}),
	)

	res, err := adapter.client.Interactions.Create(ctx, operations.CreateInteractionRequest{
		Body: operations.NewCreateInteractionRequestBody(interactions.CreateModelInteraction{
			Model:          interactions.Model(model),
			Input:          &modelInput,
			ResponseFormat: &imgRespFmt,
		}),
	})
	if err != nil {
		return nil, "", fmt.Errorf("interactions.Create: %w", err)
	}
	if res == nil || res.Interaction == nil {
		return nil, "", fmt.Errorf("interactions.Create returned empty interaction")
	}

	interaction, err := awaitInteractionIfPending(ctx, adapter.client, res.Interaction, func(i *interactions.Interaction) bool {
		return i.OutputImage != nil
	})
	if err != nil {
		return nil, "", err
	}

	outputImage := interaction.OutputImage
	if outputImage == nil {
		return nil, "", fmt.Errorf("interaction completed (status=%s) without output_image%s", interaction.Status, formatInteractionErrors(interaction.Errors))
	}

	mimeType := "image/png"
	if outputImage.MimeType != nil && strings.TrimSpace(string(*outputImage.MimeType)) != "" {
		mimeType = strings.TrimSpace(string(*outputImage.MimeType))
	}

	if outputImage.Data != nil && strings.TrimSpace(*outputImage.Data) != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(*outputImage.Data))
		if decErr != nil {
			return nil, "", fmt.Errorf("decode output_image base64: %w", decErr)
		}
		return decoded, mimeType, nil
	}

	if outputImage.URI != nil && strings.TrimSpace(*outputImage.URI) != "" {
		downloaded, dlErr := adapter.client.Files.Download(ctx, &genai.File{DownloadURI: strings.TrimSpace(*outputImage.URI)}, nil)
		if dlErr != nil {
			return nil, "", fmt.Errorf("download output_image URI: %w", dlErr)
		}
		return downloaded, mimeType, nil
	}

	return nil, "", fmt.Errorf("output_image contained neither base64 data nor URI")
}

func (adapter *Adapter) generateImageViaGenerateContent(ctx context.Context, model, prompt, aspectRatio string) ([]byte, string, error) {
	resp, err := adapter.client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(prompt),
		&genai.GenerateContentConfig{
			ResponseModalities: []string{"IMAGE"},
			ImageConfig: &genai.ImageConfig{
				AspectRatio: aspectRatio,
			},
		},
	)
	if err != nil {
		return nil, "", fmt.Errorf("Models.GenerateContent: %w", err)
	}
	if resp == nil {
		return nil, "", fmt.Errorf("Models.GenerateContent returned nil response")
	}

	for _, candidate := range resp.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part != nil && part.InlineData != nil && len(part.InlineData.Data) > 0 {
				mimeType := strings.TrimSpace(part.InlineData.MIMEType)
				if mimeType == "" {
					mimeType = "image/png"
				}
				return part.InlineData.Data, mimeType, nil
			}
		}
	}

	return nil, "", fmt.Errorf("Models.GenerateContent returned no inline image parts")
}

func (adapter *Adapter) generateVideoWithGeminiOmni(ctx context.Context, model, prompt, aspectRatio string) ([]byte, string, error) {
	clipCount := (adapter.videoDurationSeconds + omniMaxClipDurationSecs - 1) / omniMaxClipDurationSecs
	if clipCount < 1 {
		clipCount = 1
	}

	scenePrompts := adapter.buildVideoStoryboard(ctx, prompt, clipCount)
	clips := make([][]byte, clipCount)
	mimeType := "video/mp4"

	sem := make(chan struct{}, maxConcurrentVideoClips)
	group, groupCtx := errgroup.WithContext(ctx)

	for idx, scenePrompt := range scenePrompts {
		clipIndex := idx
		clipPrompt := scenePrompt
		group.Go(func() error {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-groupCtx.Done():
				return groupCtx.Err()
			}

			var lastErr error
			for attempt := 0; attempt < 2; attempt++ {
				clipCtx, cancel := context.WithTimeout(groupCtx, perClipGenerationTimeout)
				clipBytes, clipMime, err := adapter.generateSingleOmniClip(clipCtx, model, clipPrompt, aspectRatio, fmt.Sprintf("%ds", omniMaxClipDurationSecs))
				cancel()
				if err == nil && len(clipBytes) > 0 {
					clips[clipIndex] = clipBytes
					if clipIndex == 0 && clipMime != "" {
						mimeType = clipMime
					}
					return nil
				}
				lastErr = err
				if groupCtx.Err() != nil {
					return groupCtx.Err()
				}
			}
			return fmt.Errorf("generate clip %d/%d: %w", clipIndex+1, clipCount, lastErr)
		})
	}

	if err := group.Wait(); err != nil {
		return nil, "", err
	}

	stitchedBytes, err := adapter.videoStitcher(ctx, clips)
	if err != nil {
		return nil, "", fmt.Errorf("stitch %d video clips: %w", clipCount, err)
	}
	return stitchedBytes, mimeType, nil
}

func (adapter *Adapter) generateSingleOmniClip(ctx context.Context, model, prompt, aspectRatio, duration string) ([]byte, string, error) {
	modelInput := interactions.NewInteractionsInput([]interactions.Content{{
		TextContent: &interactions.TextContent{Text: prompt},
	}})
	aspect := interactions.VideoResponseFormatAspectRatio(aspectRatio)
	delivery := interactions.VideoResponseFormatDeliveryURI
	videoRespFmt := interactions.NewCreateModelInteractionResponseFormat(
		interactions.NewResponseFormat(interactions.VideoResponseFormat{
			AspectRatio: &aspect,
			Delivery:    &delivery,
			Duration:    &duration,
		}),
	)

	res, err := adapter.client.Interactions.Create(ctx, operations.CreateInteractionRequest{
		Body: operations.NewCreateInteractionRequestBody(interactions.CreateModelInteraction{
			Model:          interactions.Model(model),
			Input:          &modelInput,
			ResponseFormat: &videoRespFmt,
		}),
	})
	if err != nil {
		return nil, "", fmt.Errorf("genai Omni interactions.Create: %w", err)
	}
	if res == nil || res.Interaction == nil {
		return nil, "", fmt.Errorf("genai Omni interactions.Create returned empty interaction")
	}

	interaction, err := awaitInteractionIfPending(ctx, adapter.client, res.Interaction, func(i *interactions.Interaction) bool {
		return i.OutputVideo != nil
	})
	if err != nil {
		return nil, "", err
	}

	outputVideo := interaction.OutputVideo
	if outputVideo == nil {
		return nil, "", fmt.Errorf("genai Omni interaction completed (status=%s) without output_video%s", interaction.Status, formatInteractionErrors(interaction.Errors))
	}

	mimeType := "video/mp4"
	if outputVideo.MimeType != nil && strings.TrimSpace(string(*outputVideo.MimeType)) != "" {
		mimeType = strings.TrimSpace(string(*outputVideo.MimeType))
	}

	if outputVideo.Data != nil && strings.TrimSpace(*outputVideo.Data) != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(*outputVideo.Data))
		if decErr != nil {
			return nil, "", fmt.Errorf("decode output_video base64: %w", decErr)
		}
		return decoded, mimeType, nil
	}

	if outputVideo.URI != nil && strings.TrimSpace(*outputVideo.URI) != "" {
		downloaded, dlErr := adapter.client.Files.Download(ctx, &genai.Video{URI: strings.TrimSpace(*outputVideo.URI)}, nil)
		if dlErr != nil {
			return nil, "", fmt.Errorf("download output_video URI: %w", dlErr)
		}
		return downloaded, mimeType, nil
	}

	return nil, "", fmt.Errorf("output_video contained neither base64 data nor URI")
}

// buildVideoStoryboard expands a video concept into clipCount sequential 15-second scene prompts
// using Gemini text generation, falling back to deterministic scene prompts if needed.
func (adapter *Adapter) buildVideoStoryboard(ctx context.Context, basePrompt string, clipCount int) []string {
	fallback := deterministicStoryboard(basePrompt, clipCount)
	if adapter.client == nil || clipCount <= 1 {
		return fallback
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	instruction := fmt.Sprintf(
		"Break the following video concept into exactly %d sequential 15-second visual scenes forming a cohesive %d-second video. "+
			"Maintain consistent visual style, lighting, and subject continuity across scenes. "+
			"Respond ONLY with a JSON array of %d concise scene prompt strings.\n\nVideo Concept:\n%s",
		clipCount,
		clipCount*omniMaxClipDurationSecs,
		clipCount,
		basePrompt,
	)

	resp, err := adapter.client.Models.GenerateContent(
		reqCtx,
		adapter.textModel,
		genai.Text(instruction),
		&genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
		},
	)
	if err != nil || resp == nil {
		return fallback
	}

	raw := strings.TrimSpace(resp.Text())
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var scenes []string
	if unmarshalErr := json.Unmarshal([]byte(raw), &scenes); unmarshalErr != nil || len(scenes) < clipCount {
		return fallback
	}

	result := make([]string, clipCount)
	for i := 0; i < clipCount; i++ {
		trimmed := strings.TrimSpace(scenes[i])
		if trimmed == "" {
			return fallback
		}
		result[i] = trimmed
	}
	return result
}

func deterministicStoryboard(basePrompt string, clipCount int) []string {
	scenes := make([]string, clipCount)
	for i := 0; i < clipCount; i++ {
		var phase string
		switch {
		case i == 0:
			phase = "Opening hook and visual introduction"
		case i == clipCount-1:
			phase = "Closing summary and final takeaway"
		default:
			phase = fmt.Sprintf("Continuation part %d demonstrating core technical details", i)
		}
		scenes[i] = fmt.Sprintf("Scene %d of %d (%s): %s", i+1, clipCount, phase, basePrompt)
	}
	return scenes
}

func stitchMP4ClipsWithFFmpeg(ctx context.Context, clips [][]byte) ([]byte, error) {
	if len(clips) == 0 {
		return nil, fmt.Errorf("no video clips provided to stitch")
	}
	if len(clips) == 1 {
		return clips[0], nil
	}

	ffmpegPath, err := resolveFFmpegBinary()
	if err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "digitalclone-stitch-*")
	if err != nil {
		return nil, fmt.Errorf("create temp stitch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	var concatList strings.Builder
	for idx, clipBytes := range clips {
		if len(clipBytes) == 0 {
			return nil, fmt.Errorf("clip %d is empty", idx)
		}
		clipName := fmt.Sprintf("clip_%03d.mp4", idx)
		clipPath := filepath.Join(tmpDir, clipName)
		if writeErr := os.WriteFile(clipPath, clipBytes, 0o600); writeErr != nil {
			return nil, fmt.Errorf("write %s: %w", clipName, writeErr)
		}
		fmt.Fprintf(&concatList, "file '%s'\n", clipName)
	}

	concatPath := filepath.Join(tmpDir, "concat.txt")
	if writeErr := os.WriteFile(concatPath, []byte(concatList.String()), 0o600); writeErr != nil {
		return nil, fmt.Errorf("write concat list: %w", writeErr)
	}

	outputPath := filepath.Join(tmpDir, "stitched.mp4")

	// Try lossless stream-copy concatenation first; fall back to normalized H.264/AAC re-encoding
	// if clips have mismatched stream parameters or missing audio tracks.
	copyCmd := exec.CommandContext(ctx, ffmpegPath,
		"-y", "-f", "concat", "-safe", "0", "-i", concatPath,
		"-c", "copy", "-movflags", "+faststart", outputPath,
	)
	var copyStderr bytes.Buffer
	copyCmd.Stderr = &copyStderr
	if copyErr := copyCmd.Run(); copyErr != nil {
		reencodeCmd := exec.CommandContext(ctx, ffmpegPath,
			"-y", "-f", "concat", "-safe", "0", "-i", concatPath,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
			"-c:a", "aac", "-pix_fmt", "yuv420p", "-movflags", "+faststart", outputPath,
		)
		var reencodeStderr bytes.Buffer
		reencodeCmd.Stderr = &reencodeStderr
		if reencodeErr := reencodeCmd.Run(); reencodeErr != nil {
			return nil, fmt.Errorf("ffmpeg concat failed (copy: %v; reencode: %v: %s)", copyErr, reencodeErr, strings.TrimSpace(reencodeStderr.String()))
		}
	}

	stitchedBytes, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		return nil, fmt.Errorf("read stitched video: %w", readErr)
	}
	if len(stitchedBytes) == 0 {
		return nil, fmt.Errorf("ffmpeg produced an empty stitched video")
	}
	return stitchedBytes, nil
}

func resolveFFmpegBinary() (string, error) {
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path, nil
	}
	for _, candidate := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("ffmpeg binary not found in PATH or standard locations (/opt/homebrew/bin/ffmpeg, /usr/local/bin/ffmpeg)")
}

func clampVideoDurationSeconds(seconds int) int {
	if seconds <= 0 {
		return minVideoDurationSeconds
	}
	if seconds < minVideoDurationSeconds {
		return minVideoDurationSeconds
	}
	if seconds > maxVideoDurationSeconds {
		return maxVideoDurationSeconds
	}
	return seconds
}

// NormalizeAspectRatio trims whitespace and trailing punctuation (such as "16:9,")
// and returns fallback when the normalized value is empty.
func NormalizeAspectRatio(aspectRatio, fallback string) string {
	cleaned := strings.TrimSpace(aspectRatio)
	cleaned = strings.Trim(cleaned, ".,;\"'")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func awaitInteractionIfPending(
	ctx context.Context,
	client *genai.Client,
	interaction *interactions.Interaction,
	hasMedia func(*interactions.Interaction) bool,
) (*interactions.Interaction, error) {
	for (interaction.Status == interactions.InteractionStatusQueued ||
		interaction.Status == interactions.InteractionStatusInProgress) &&
		!hasMedia(interaction) &&
		interaction.ID != nil &&
		strings.TrimSpace(*interaction.ID) != "" {
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		polled, err := client.Interactions.Get(ctx, operations.GetInteractionByIDRequest{
			ID: strings.TrimSpace(*interaction.ID),
		})
		if err != nil {
			return nil, fmt.Errorf("poll interaction %s: %w", *interaction.ID, err)
		}
		if polled == nil || polled.Interaction == nil {
			return nil, fmt.Errorf("poll interaction %s returned empty interaction", *interaction.ID)
		}
		interaction = polled.Interaction
	}

	if interaction.Status == interactions.InteractionStatusFailed ||
		interaction.Status == interactions.InteractionStatusCancelled {
		interactionID := ""
		if interaction.ID != nil {
			interactionID = *interaction.ID
		}
		return nil, fmt.Errorf("interaction %s %s%s", interactionID, interaction.Status, formatInteractionErrors(interaction.Errors))
	}
	return interaction, nil
}

func formatInteractionErrors(interactionErrors []interactions.Error) string {
	if len(interactionErrors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(interactionErrors))
	for _, item := range interactionErrors {
		msg := ""
		if item.Message != nil {
			msg = strings.TrimSpace(*item.Message)
		}
		code := ""
		if item.Code != nil {
			code = strings.TrimSpace(*item.Code)
		}
		switch {
		case code != "" && msg != "":
			parts = append(parts, fmt.Sprintf("%s: %s", code, msg))
		case msg != "":
			parts = append(parts, msg)
		case code != "":
			parts = append(parts, code)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
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
