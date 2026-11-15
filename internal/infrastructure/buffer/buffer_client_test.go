package buffer_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/buffer"
)

func TestBufferClientHierarchyAndCreatePost(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-buffer-token" {
			t.Fatalf("unexpected Authorization header: %s", auth)
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("decode graphql request: %v", err)
		}

		callNum := requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("RateLimit", "limit=150, remaining=149, reset=600")

		switch {
		case strings.Contains(req.Query, "GetAccountHierarchy"):
			_, _ = w.Write([]byte(`{"data":{"account":{"id":"acc_1","organizations":[{"id":"org_1","name":"DigitalClone","isOwner":true}]}}}`))
		case strings.Contains(req.Query, "GetChannels"):
			_, _ = w.Write([]byte(`{"data":{"channels":[{"id":"ch_li","name":"LinkedIn","service":"linkedin"},{"id":"ch_x","name":"X","service":"twitter"},{"id":"ch_tt","name":"TikTok","service":"tiktok"}]}}`))
		case strings.Contains(req.Query, "SchedulePost"):
			// Simulate transient HTTP 429 on first createPost attempt to validate Retry-After handling.
			if callNum == 3 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"errors":[{"message":"RATE_LIMIT_EXCEEDED"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"createPost":{"__typename":"PostActionSuccess","post":{"id":"buf_post_999","text":"Hello","status":"sent","dueAt":"2026-11-14T19:00:00Z"}}}}`))
		default:
			t.Fatalf("unexpected graphql query: %s", req.Query)
		}
	})

	client := buffer.NewClient(buffer.Config{
		APIKey:         "test-buffer-token",
		Endpoint:       "https://buffer.test",
		ShareMode:      buffer.ShareModeShareNow,
		RequestTimeout: 5 * time.Second,
		HTTPClient:     httpClient,
		Logger:         slog.Default(),
	})

	postID, err := client.CreatePost(context.Background(), buffer.CreatePostParams{
		Platform: domain.PlatformLinkedIn,
		Text:     "Shipped Hexagonal Go ADK social agent!",
		Assets: []buffer.AssetInput{
			{URL: "https://cdn.example.com/banner.png", MimeType: "image/png"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePost failed: %v", err)
	}
	if postID != "buf_post_999" {
		t.Fatalf("expected buf_post_999, got %s", postID)
	}
}

func TestBufferClientHandlesMutationError(t *testing.T) {
	t.Parallel()

	httpClient := newTestHTTPClient(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"createPost":{"__typename":"MutationError","message":"Invalid media asset URL"}}}`))
	})

	client := buffer.NewClient(buffer.Config{
		APIKey:   "token",
		Endpoint: "https://buffer.test",
		ChannelIDs: map[domain.Platform]string{
			domain.PlatformX: "ch_x_1",
		},
		HTTPClient: httpClient,
	})

	_, err := client.CreatePost(context.Background(), buffer.CreatePostParams{
		Platform: domain.PlatformX,
		Text:     "Testing error",
	})
	if err == nil || !strings.Contains(err.Error(), "Invalid media asset URL") {
		t.Fatalf("expected MutationError message, got %v", err)
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
