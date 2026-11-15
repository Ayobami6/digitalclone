package notification_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/notification"
)

func TestDiscordAndTelegramNotifiers(t *testing.T) {
	t.Parallel()

	batch, err := domain.NewDraftBatch("11111111-2222-3333-4444-555555555555", domain.CommitEvent{
		RepoName:  "Ayobami6/digitalclone",
		Branch:    "feat/agent",
		CommitSHA: "abcdef123456",
		Message:   "feat: add ADK agents & <safe> HTML escaping",
	})
	if err != nil {
		t.Fatalf("NewDraftBatch failed: %v", err)
	}
	batch.Drafts[domain.PlatformLinkedIn] = &domain.ContentDraft{
		ID:       "d1",
		BatchID:  batch.ID,
		Platform: domain.PlatformLinkedIn,
		Title:    "LinkedIn Update",
		BodyText: "Shipped Go ADK parallel social agent.",
		Status:   domain.StatusPendingReview,
		MediaAsset: &domain.MediaAsset{
			MediaType: domain.MediaImage,
			PublicURL: "https://cdn.example.com/li.png",
		},
	}

	t.Run("discord sends embeds and components and updates message", func(t *testing.T) {
		t.Parallel()
		var postMethod, patchMethod string
		httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost:
				postMethod = r.URL.Path
				_, _ = w.Write([]byte(`{"id":"disc_msg_42"}`))
			case http.MethodPatch:
				patchMethod = r.URL.Path
				_, _ = w.Write([]byte(`{"id":"disc_msg_42"}`))
			}
		})

		discord := notification.NewDiscordNotifier(notification.DiscordConfig{
			BotToken:   "bot-secret",
			ChannelID:  "chan-99",
			APIBaseURL: "https://discord.test",
			HTTPClient: httpClient,
		})
		msgID, err := discord.SendForReview(context.Background(), batch)
		if err != nil || msgID != "disc_msg_42" {
			t.Fatalf("SendForReview failed: id=%s err=%v", msgID, err)
		}
		if postMethod != "/channels/chan-99/messages" {
			t.Fatalf("unexpected POST path: %s", postMethod)
		}

		batch.DiscordMessageID = msgID
		if err := discord.UpdateReviewStatus(context.Background(), batch); err != nil {
			t.Fatalf("UpdateReviewStatus failed: %v", err)
		}
		if patchMethod != "/channels/chan-99/messages/disc_msg_42" {
			t.Fatalf("unexpected PATCH path: %s", patchMethod)
		}
	})

	t.Run("telegram sends inline keyboard under 64 bytes and escapes HTML", func(t *testing.T) {
		t.Parallel()
		var capturedBody string
		httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			capturedBody = string(raw)
			if strings.HasSuffix(r.URL.Path, "/sendMessage") {
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		})

		tg := notification.NewTelegramNotifier(notification.TelegramConfig{
			BotToken:   "123:ABC",
			ChatID:     "-100999",
			APIBaseURL: "https://telegram.test",
			HTTPClient: httpClient,
		})
		msgID, err := tg.SendForReview(context.Background(), batch)
		if err != nil || msgID != "777" {
			t.Fatalf("SendForReview failed: id=%s err=%v", msgID, err)
		}

		var decoded struct {
			Text        string `json:"text"`
			ReplyMarkup struct {
				InlineKeyboard [][]struct {
					CallbackData string `json:"callback_data"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		if err := json.Unmarshal([]byte(capturedBody), &decoded); err != nil {
			t.Fatalf("decode telegram body: %v", err)
		}
		if !strings.Contains(decoded.Text, "&lt;safe&gt;") {
			t.Fatalf("expected HTML escaping in telegram text, got: %s", decoded.Text)
		}
		for _, row := range decoded.ReplyMarkup.InlineKeyboard {
			for _, btn := range row {
				if len(btn.CallbackData) > 64 {
					t.Fatalf("callback_data exceeds Telegram 64-byte limit (%d bytes): %s", len(btn.CallbackData), btn.CallbackData)
				}
			}
		}

		batch.TelegramMessageID = msgID
		if err := tg.UpdateReviewStatus(context.Background(), batch); err != nil {
			t.Fatalf("UpdateReviewStatus failed: %v", err)
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
