package storage_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/storage"
)

func TestHybridAssetStorage(t *testing.T) {
	t.Parallel()

	t.Run("local static HTTP fallback", func(t *testing.T) {
		t.Parallel()
		localDir := t.TempDir()
		store := storage.NewHybridAssetStorage(storage.Config{
			LocalDir:     localDir,
			LocalBaseURL: "https://digitalclone.example.com",
		})

		asset, err := store.UploadAsset(
			context.Background(),
			"batch-1",
			domain.PlatformLinkedIn,
			domain.MediaImage,
			"banner.png",
			"image/png",
			[]byte("png-bytes"),
		)
		if err != nil {
			t.Fatalf("UploadAsset failed: %v", err)
		}
		if asset.PublicURL != "https://digitalclone.example.com/assets/banner.png" {
			t.Fatalf("unexpected public URL: %s", asset.PublicURL)
		}
		saved, err := os.ReadFile(asset.LocalPath)
		if err != nil || string(saved) != "png-bytes" {
			t.Fatalf("unexpected local file content: %q err=%v", string(saved), err)
		}
	})

	t.Run("S3 SigV4 upload", func(t *testing.T) {
		t.Parallel()
		var gotAuth string
		var gotBody string
		httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				t.Fatalf("expected PUT, got %s", r.Method)
			}
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			gotBody = string(raw)
			w.WriteHeader(http.StatusOK)
		})

		store := storage.NewHybridAssetStorage(storage.Config{
			LocalDir:        t.TempDir(),
			S3Bucket:        "my-bucket",
			S3Region:        "us-east-1",
			S3Endpoint:      "https://s3.test",
			S3AccessKeyID:   "AKIAEXAMPLE",
			S3SecretKey:     "SECRETEXAMPLE",
			S3PublicBaseURL: "https://cdn.example.com",
			HTTPClient:      httpClient,
		})

		asset, err := store.UploadAsset(
			context.Background(),
			"batch-2",
			domain.PlatformTikTok,
			domain.MediaVideo,
			"clip.mp4",
			"video/mp4",
			[]byte("mp4-data"),
		)
		if err != nil {
			t.Fatalf("S3 UploadAsset failed: %v", err)
		}
		if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") {
			t.Fatalf("expected SigV4 Authorization header, got %q", gotAuth)
		}
		if gotBody != "mp4-data" {
			t.Fatalf("expected body mp4-data, got %q", gotBody)
		}
		if asset.PublicURL != "https://cdn.example.com/digitalclone/batch-2/tiktok/clip.mp4" {
			t.Fatalf("unexpected CDN public URL: %s", asset.PublicURL)
		}
	})
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
