package httpserver_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"digitalclone/internal/application"
	"digitalclone/internal/domain"
	httpserver "digitalclone/internal/presentation/http"
)

type stubTriggerUC struct {
	lastEvent domain.CommitEvent
	err       error
}

func (stub *stubTriggerUC) Execute(_ context.Context, commitEvent domain.CommitEvent) (*domain.DraftBatch, error) {
	stub.lastEvent = commitEvent
	if stub.err != nil {
		return nil, stub.err
	}
	return domain.NewDraftBatch("batch-http-1", commitEvent)
}

type stubDecisionUC struct {
	lastCommand application.ReviewDecisionCommand
}

func (stub *stubDecisionUC) Execute(_ context.Context, command application.ReviewDecisionCommand) (*application.ReviewDecisionResult, error) {
	stub.lastCommand = command
	return &application.ReviewDecisionResult{
		BatchID:               command.BatchID,
		Action:                command.Action,
		TransitionedPlatforms: []domain.Platform{domain.PlatformLinkedIn},
	}, nil
}

func TestWebhookSignaturesAndRouting(t *testing.T) {
	t.Parallel()

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 keypair: %v", err)
	}

	assetsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(assetsDir, "demo.png"), []byte("png"), 0o600); err != nil {
		t.Fatalf("write demo asset: %v", err)
	}

	triggerStub := &stubTriggerUC{}
	decisionStub := &stubDecisionUC{}

	server, err := httpserver.NewServer(httpserver.ServerConfig{
		GitHubWebhookSecret:   "gh-secret",
		DiscordPublicKeyHex:   hex.EncodeToString(pubKey),
		TelegramWebhookSecret: "tg-secret",
		AssetsDir:             assetsDir,
		TriggerUseCase:        triggerStub,
		DecisionUseCase:       decisionStub,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	t.Run("github push webhook verifies HMAC-SHA256", func(t *testing.T) {
		payload := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"Ayobami6/digitalclone"},"head_commit":{"id":"abc123","message":"feat: ship webhook","author":{"name":"Ayobami"}}}`)

		// Bad signature must return 401 RFC 7807.
		badReq := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
		badReq.Header.Set("X-GitHub-Event", "push")
		badReq.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
		badRec := httptest.NewRecorder()
		server.Handler().ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for bad HMAC, got %d", badRec.Code)
		}

		mac := hmac.New(sha256.New, []byte("gh-secret"))
		_, _ = mac.Write(payload)
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

		goodReq := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
		goodReq.Header.Set("X-GitHub-Event", "push")
		goodReq.Header.Set("X-Hub-Signature-256", sig)
		goodRec := httptest.NewRecorder()
		server.Handler().ServeHTTP(goodRec, goodReq)
		if goodRec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted, got %d (%s)", goodRec.Code, goodRec.Body.String())
		}
		if triggerStub.lastEvent.CommitSHA != "abc123" {
			t.Fatalf("expected commit abc123, got %+v", triggerStub.lastEvent)
		}
	})

	t.Run("discord interaction verifies Ed25519 and parses button click", func(t *testing.T) {
		payload := []byte(`{"type":3,"data":{"custom_id":"review:batch-99:approve:linkedin"},"user":{"username":"ayo"}}`)
		timestamp := "1700000000"
		signed := append([]byte(timestamp), payload...)
		sig := ed25519.Sign(privKey, signed)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/discord/interactions", bytes.NewReader(payload))
		req.Header.Set("X-Signature-Timestamp", timestamp)
		req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d (%s)", rec.Code, rec.Body.String())
		}
		if decisionStub.lastCommand.BatchID != "batch-99" || decisionStub.lastCommand.Action != application.ReviewActionApprove || decisionStub.lastCommand.TargetPlatform != "linkedin" {
			t.Fatalf("unexpected discord parsed command: %+v", decisionStub.lastCommand)
		}
	})

	t.Run("telegram callback verifies secret token and parses compact payload", func(t *testing.T) {
		payload := []byte(`{"callback_query":{"id":"cb-1","data":"rv|batch-88|rj|all","from":{"username":"ayo_tg"}}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram", bytes.NewReader(payload))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d (%s)", rec.Code, rec.Body.String())
		}
		if decisionStub.lastCommand.BatchID != "batch-88" || decisionStub.lastCommand.Action != application.ReviewActionReject || decisionStub.lastCommand.Source != "telegram" {
			t.Fatalf("unexpected telegram parsed command: %+v", decisionStub.lastCommand)
		}
	})

	t.Run("manual trigger validation returns RFC 7807", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers/commit", bytes.NewReader([]byte(`{"commit_sha":""}`)))
		triggerStub.err = domain.ErrValidationFailed
		defer func() { triggerStub.err = nil }()
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
		}
		var errBody map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
		if errBody["title"] != "Validation Failed" {
			t.Fatalf("expected RFC 7807 Validation Failed, got %+v", errBody)
		}
	})

	t.Run("static asset route serves existing asset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/demo.png", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "png" {
			t.Fatalf("expected 200 png, got %d (%q)", rec.Code, rec.Body.String())
		}
	})
}
