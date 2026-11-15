package publishers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/buffer"
	"digitalclone/internal/infrastructure/publishers"
)

func TestBufferBackedPlatformPostServices(t *testing.T) {
	t.Parallel()

	var lastChannelID string
	var lastText string
	var lastAssets []map[string]map[string]string
	bufferHTTPClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Input struct {
					ChannelID string                         `json:"channelId"`
					Text      string                         `json:"text"`
					Assets    []map[string]map[string]string `json:"assets"`
				} `json:"input"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		lastChannelID = req.Variables.Input.ChannelID
		lastText = req.Variables.Input.Text
		lastAssets = req.Variables.Input.Assets
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createPost":{"__typename":"PostActionSuccess","post":{"id":"buf_123","status":"sent"}}}}`))
	})

	client := buffer.NewClient(buffer.Config{
		APIKey:   "token",
		Endpoint: "https://buffer.test",
		ChannelIDs: map[domain.Platform]string{
			domain.PlatformLinkedIn: "ch_linkedin",
			domain.PlatformX:        "ch_x",
			domain.PlatformTikTok:   "ch_tiktok",
		},
		HTTPClient: bufferHTTPClient,
	})

	testCases := []struct {
		service       domain.PlatformPostService
		platform      domain.Platform
		wantChannelID string
		mediaType     domain.MediaType
		mediaURL      string
	}{
		{
			service:       publishers.NewLinkedInPostService(client),
			platform:      domain.PlatformLinkedIn,
			wantChannelID: "ch_linkedin",
			mediaType:     domain.MediaImage,
			mediaURL:      "https://cdn.example.com/li.png",
		},
		{
			service:       publishers.NewXPostService(client),
			platform:      domain.PlatformX,
			wantChannelID: "ch_x",
			mediaType:     domain.MediaImage,
			mediaURL:      "https://cdn.example.com/x.png",
		},
		{
			service:       publishers.NewTikTokPostService(client),
			platform:      domain.PlatformTikTok,
			wantChannelID: "ch_tiktok",
			mediaType:     domain.MediaVideo,
			mediaURL:      "https://cdn.example.com/tt.mp4",
		},
	}

	for _, testCase := range testCases {
		draft := &domain.ContentDraft{
			ID:       "draft-1",
			Platform: testCase.platform,
			BodyText: "Shipped clean commit",
			Tags:     []string{"golang"},
			MediaAsset: &domain.MediaAsset{
				MediaType: testCase.mediaType,
				PublicURL: testCase.mediaURL,
			},
		}
		postID, err := testCase.service.Publish(context.Background(), draft)
		if err != nil {
			t.Fatalf("%s Publish failed: %v", testCase.platform, err)
		}
		if postID != "buf_123" || lastChannelID != testCase.wantChannelID || !strings.Contains(lastText, "#golang") {
			t.Fatalf("unexpected publish outcome: postID=%s channel=%s text=%q", postID, lastChannelID, lastText)
		}
		if len(lastAssets) != 1 {
			t.Fatalf("%s expected 1 formatted asset, got %+v", testCase.platform, lastAssets)
		}
		expectedAssetKey := "image"
		if testCase.mediaType == domain.MediaVideo {
			expectedAssetKey = "video"
		}
		if lastAssets[0][expectedAssetKey]["url"] != testCase.mediaURL {
			t.Fatalf("%s expected assets[0][%q][\"url\"] == %q, got %+v", testCase.platform, expectedAssetKey, testCase.mediaURL, lastAssets)
		}
	}
}

func TestYouTubePostServiceUpload(t *testing.T) {
	t.Parallel()

	videoPath := filepath.Join(t.TempDir(), "short.mp4")
	if err := os.WriteFile(videoPath, []byte("fake-mp4-content-stream"), 0o600); err != nil {
		t.Fatalf("write temp mp4: %v", err)
	}

	var receivedUpload bool
	ytHTTPClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/youtube/v3/videos") {
			receivedUpload = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"yt_video_777","snippet":{"title":"Shipped ADK Agent"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	ytService := publishers.NewYouTubePostService(publishers.YouTubeConfig{
		AccessToken: "oauth-test-token",
		Endpoint:    "https://youtube.test/",
		HTTPClient:  ytHTTPClient,
	})

	videoID, err := ytService.Publish(context.Background(), &domain.ContentDraft{
		ID:       "draft-yt",
		Platform: domain.PlatformYouTube,
		Title:    "Shipped ADK Agent",
		BodyText: "Automated developer Shorts via Go ADK.",
		Tags:     []string{"golang", "adk"},
		MediaAsset: &domain.MediaAsset{
			MediaType: domain.MediaVideo,
			MimeType:  "video/mp4",
			LocalPath: videoPath,
		},
	})
	if err != nil {
		t.Fatalf("YouTube Publish failed: %v", err)
	}
	if !receivedUpload || videoID != "yt_video_777" {
		t.Fatalf("expected YouTube upload yt_video_777, got id=%s received=%v", videoID, receivedUpload)
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
