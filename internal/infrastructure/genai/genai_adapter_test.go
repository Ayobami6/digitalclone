package genai_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"digitalclone/internal/infrastructure/genai"
)

func TestGenAIAdapterDryRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter, err := genai.NewAdapter(ctx, genai.Config{DryRun: true})
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}

	text, err := adapter.GenerateText(ctx, "System persona", "Platform: linkedin\nCommit Message: feat: add ADK agent")
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}
	if !strings.Contains(text, "LINKEDIN") || !strings.Contains(text, "feat: add ADK agent") {
		t.Fatalf("unexpected dry-run text output: %s", text)
	}

	imgBytes, imgMime, err := adapter.GenerateImage(ctx, "diagram", "16:9")
	if err != nil {
		t.Fatalf("GenerateImage failed: %v", err)
	}
	if imgMime != "image/png" || len(imgBytes) < 8 || !bytes.HasPrefix(imgBytes, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("expected valid PNG header, got mime=%s len=%d", imgMime, len(imgBytes))
	}

	vidBytes, vidMime, err := adapter.GenerateVideo(ctx, "demo clip", "9:16")
	if err != nil {
		t.Fatalf("GenerateVideo failed: %v", err)
	}
	if vidMime != "video/mp4" || !bytes.Contains(vidBytes, []byte("ftyp")) {
		t.Fatalf("expected valid MP4 ftyp header, got mime=%s len=%d", vidMime, len(vidBytes))
	}
}

func TestGenAIAdapterGeminiImageInteractions(t *testing.T) {
	t.Parallel()

	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	encodedPNG := base64.StdEncoding.EncodeToString(pngBytes)

	var seenPath string
	var seenModel string
	var seenRespType string
	httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		bodyBytes, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(bodyBytes, &payload)
		if m, ok := payload["model"].(string); ok {
			seenModel = m
		}
		if rf, ok := payload["response_format"].(map[string]any); ok {
			if typ, ok := rf["type"].(string); ok {
				seenRespType = typ
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "inter_img_1",
			"status": "completed",
			"steps": []map[string]any{
				{
					"type": "model_output",
					"content": []map[string]any{
						{
							"type":      "image",
							"data":      encodedPNG,
							"mime_type": "image/png",
						},
					},
				},
			},
		})
	})

	ctx := context.Background()
	adapter, err := genai.NewAdapter(ctx, genai.Config{
		APIKey:     "test-key",
		ImageModel: "gemini-3.1-flash-image",
		BaseURL:    "https://genai.test",
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}

	gotBytes, gotMime, err := adapter.GenerateImage(ctx, "architecture diagram", "16:9")
	if err != nil {
		t.Fatalf("GenerateImage failed: %v", err)
	}
	if seenPath != "/v1beta/interactions" {
		t.Fatalf("expected /v1beta/interactions request path, got %q", seenPath)
	}
	if seenModel != "gemini-3.1-flash-image" {
		t.Fatalf("expected model gemini-3.1-flash-image, got %q", seenModel)
	}
	if seenRespType != "image" {
		t.Fatalf("expected response_format type image, got %q", seenRespType)
	}
	if gotMime != "image/png" || !bytes.Equal(gotBytes, pngBytes) {
		t.Fatalf("unexpected image result: mime=%s bytes=%x", gotMime, gotBytes)
	}
}

func TestGenAIAdapterGeminiImageFallbackToGenerateContent(t *testing.T) {
	t.Parallel()

	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	encodedPNG := base64.StdEncoding.EncodeToString(pngBytes)

	var interactionsCalled bool
	var generateContentPath string
	httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/interactions" {
			interactionsCalled = true
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":generateContent") {
			generateContentPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"role": "model",
							"parts": []map[string]any{
								{
									"inlineData": map[string]any{
										"mimeType": "image/png",
										"data":     encodedPNG,
									},
								},
							},
						},
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	})

	ctx := context.Background()
	adapter, err := genai.NewAdapter(ctx, genai.Config{
		APIKey:     "test-key",
		ImageModel: "gemini-3.1-flash-image",
		BaseURL:    "https://genai.test",
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}

	gotBytes, gotMime, err := adapter.GenerateImage(ctx, "clean diagram", "16:9")
	if err != nil {
		t.Fatalf("GenerateImage fallback failed: %v", err)
	}
	if !interactionsCalled {
		t.Fatalf("expected Interactions endpoint to be attempted first")
	}
	if !strings.Contains(generateContentPath, "gemini-3.1-flash-image:generateContent") {
		t.Fatalf("expected GenerateContent fallback path, got %q", generateContentPath)
	}
	if gotMime != "image/png" || !bytes.Equal(gotBytes, pngBytes) {
		t.Fatalf("unexpected fallback image result: mime=%s bytes=%x", gotMime, gotBytes)
	}
}

func TestGenAIAdapterGeminiOmniVideo(t *testing.T) {
	t.Parallel()

	mp4Bytes := []byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p',
		'm', 'p', '4', '2', 0x00, 0x00, 0x00, 0x00,
	}

	var seenRespFormat map[string]any
	var downloadCalled bool

	httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/interactions":
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(bodyBytes, &payload)
			if rf, ok := payload["response_format"].(map[string]any); ok {
				seenRespFormat = rf
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "inter_vid_1",
				"status": "completed",
				"steps": []map[string]any{
					{
						"type": "model_output",
						"content": []map[string]any{
							{
								"type":      "video",
								"uri":       "https://genai.test/v1beta/files/vid123abc",
								"mime_type": "video/mp4",
							},
						},
					},
				},
			})
		case "/v1beta/files/vid123abc:download":
			downloadCalled = true
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(mp4Bytes)
		default:
			http.NotFound(w, r)
		}
	})

	ctx := context.Background()
	adapter, err := genai.NewAdapter(ctx, genai.Config{
		APIKey:     "test-key",
		VideoModel: "gemini-omni-1.2-flash",
		BaseURL:    "https://genai.test",
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}

	gotBytes, gotMime, err := adapter.GenerateVideo(ctx, "fast code walkthrough", "9:16")
	if err != nil {
		t.Fatalf("GenerateVideo failed: %v", err)
	}
	if seenRespFormat["type"] != "video" || seenRespFormat["delivery"] != "uri" || seenRespFormat["aspect_ratio"] != "9:16" {
		t.Fatalf("unexpected response_format payload: %+v", seenRespFormat)
	}
	if !downloadCalled {
		t.Fatalf("expected video file download endpoint to be called")
	}
	if gotMime != "video/mp4" || !bytes.Equal(gotBytes, mp4Bytes) {
		t.Fatalf("unexpected video result: mime=%s len=%d", gotMime, len(gotBytes))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestHTTPClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Body != nil {
				defer func() { _ = req.Body.Close() }()
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			resp := recorder.Result()
			resp.Request = req
			return resp, nil
		}),
	}
}
