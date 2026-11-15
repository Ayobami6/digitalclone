// Package httpserver implements HTTP driving adapters for GitHub push webhooks,
// manual REST commit triggers, Discord/Telegram interactive review callbacks,
// and static fallback media serving.
package httpserver

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"digitalclone/internal/application"
	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/notification"
	"digitalclone/pkg/problem"
)

const maxWebhookBodyBytes = 5 << 20 // 5 MiB

// TriggerUseCase abstracts commit trigger execution for HTTP handlers.
type TriggerUseCase interface {
	Execute(ctx context.Context, commitEvent domain.CommitEvent) (*domain.DraftBatch, error)
}

// DecisionUseCase abstracts human-in-the-loop Approve/Reject execution for interaction handlers.
type DecisionUseCase interface {
	Execute(ctx context.Context, command application.ReviewDecisionCommand) (*application.ReviewDecisionResult, error)
}

// ServerConfig configures security secrets and handlers for the HTTP server.
type ServerConfig struct {
	GitHubWebhookSecret   string
	DiscordPublicKeyHex   string
	TelegramWebhookSecret string
	AssetsDir             string
	CommitInspector       domain.CommitInspector
	TelegramNotifier      *notification.TelegramNotifier
	TriggerUseCase        TriggerUseCase
	DecisionUseCase       DecisionUseCase
	Logger                *slog.Logger
}

// Server wraps net/http routing and webhook security validation.
type Server struct {
	config           ServerConfig
	discordPublicKey ed25519.PublicKey
	logger           *slog.Logger
	mux              *http.ServeMux
}

// NewServer constructs an HTTP Server with all DigitalClone endpoints registered.
func NewServer(config ServerConfig) (*Server, error) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var discordKey ed25519.PublicKey
	if strings.TrimSpace(config.DiscordPublicKeyHex) != "" {
		decodedKey, err := hex.DecodeString(strings.TrimSpace(config.DiscordPublicKeyHex))
		if err != nil || len(decodedKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: invalid Discord Ed25519 public key hex", domain.ErrValidationFailed)
		}
		discordKey = ed25519.PublicKey(decodedKey)
	}

	if strings.TrimSpace(config.AssetsDir) == "" {
		config.AssetsDir = "./assets"
	}

	server := &Server{
		config:           config,
		discordPublicKey: discordKey,
		logger:           logger,
		mux:              http.NewServeMux(),
	}
	server.registerRoutes()
	return server, nil
}

// Handler returns the root http.Handler for testing or serving.
func (server *Server) Handler() http.Handler {
	return server.mux
}

func (server *Server) registerRoutes() {
	server.mux.HandleFunc("GET /healthz", server.handleHealthz)
	server.mux.HandleFunc("POST /webhooks/github", server.handleGitHubWebhook)
	server.mux.HandleFunc("POST /api/v1/triggers/commit", server.handleManualTrigger)
	server.mux.HandleFunc("POST /webhooks/discord/interactions", server.handleDiscordInteraction)
	server.mux.HandleFunc("POST /webhooks/telegram", server.handleTelegramWebhook)
	server.mux.HandleFunc("GET /assets/{name}", server.handleGetAsset)
}

func (server *Server) handleHealthz(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (server *Server) handleGitHubWebhook(writer http.ResponseWriter, request *http.Request) {
	rawBody, err := readBoundedBody(writer, request)
	if err != nil {
		problem.Write(writer, http.StatusRequestEntityTooLarge, "", "Payload Too Large", err.Error(), request.URL.Path)
		return
	}

	if strings.TrimSpace(server.config.GitHubWebhookSecret) != "" {
		signatureHeader := request.Header.Get("X-Hub-Signature-256")
		if !verifyGitHubHMAC(rawBody, server.config.GitHubWebhookSecret, signatureHeader) {
			problem.Write(writer, http.StatusUnauthorized, "", "Invalid Webhook Signature", "X-Hub-Signature-256 verification failed", request.URL.Path)
			return
		}
	}

	eventType := strings.TrimSpace(request.Header.Get("X-GitHub-Event"))
	if eventType == "ping" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	if eventType != "" && eventType != "push" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ignored", "event": eventType})
		return
	}

	var pushPayload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			ID       string   `json:"id"`
			Message  string   `json:"message"`
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
			Author   struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(rawBody, &pushPayload); err != nil {
		problem.Write(writer, http.StatusBadRequest, "", "Malformed JSON", "unable to parse GitHub push payload", request.URL.Path)
		return
	}

	commitSHA := strings.TrimSpace(pushPayload.After)
	if pushPayload.HeadCommit != nil && strings.TrimSpace(pushPayload.HeadCommit.ID) != "" {
		commitSHA = strings.TrimSpace(pushPayload.HeadCommit.ID)
	}
	if commitSHA == "" || strings.TrimSpace(pushPayload.Repository.FullName) == "" {
		problem.Write(writer, http.StatusBadRequest, "", "Validation Failed", "repository.full_name and commit SHA are required", request.URL.Path)
		return
	}

	var commitEvent *domain.CommitEvent
	if server.config.CommitInspector != nil {
		fetched, fetchErr := server.config.CommitInspector.FetchGitHubCommit(request.Context(), pushPayload.Repository.FullName, commitSHA)
		if fetchErr != nil {
			problem.Write(writer, http.StatusBadGateway, "", "GitHub Commit Inspection Failed", fetchErr.Error(), request.URL.Path)
			return
		}
		commitEvent = fetched
	} else {
		var changed []string
		message := "Push event " + commitSHA
		author := "unknown"
		if pushPayload.HeadCommit != nil {
			message = pushPayload.HeadCommit.Message
			author = pushPayload.HeadCommit.Author.Name
			changed = append(changed, pushPayload.HeadCommit.Added...)
			changed = append(changed, pushPayload.HeadCommit.Modified...)
			changed = append(changed, pushPayload.HeadCommit.Removed...)
		}
		commitEvent = &domain.CommitEvent{
			RepoName:     pushPayload.Repository.FullName,
			Branch:       strings.TrimPrefix(pushPayload.Ref, "refs/heads/"),
			CommitSHA:    commitSHA,
			Author:       author,
			Message:      message,
			ChangedFiles: changed,
		}
	}
	if strings.TrimPrefix(pushPayload.Ref, "refs/heads/") != "" {
		commitEvent.Branch = strings.TrimPrefix(pushPayload.Ref, "refs/heads/")
	}

	batch, execErr := server.config.TriggerUseCase.Execute(request.Context(), *commitEvent)
	if errors.Is(execErr, domain.ErrDuplicateCommitTrigger) {
		writeJSON(writer, http.StatusOK, map[string]string{
			"status":     "duplicate_ignored",
			"commit_sha": commitSHA,
		})
		return
	}
	if execErr != nil {
		problem.Write(writer, http.StatusInternalServerError, "", "Pipeline Execution Failed", execErr.Error(), request.URL.Path)
		return
	}

	writeJSON(writer, http.StatusAccepted, map[string]any{
		"status":     "pending_review",
		"batch_id":   batch.ID,
		"commit_sha": batch.CommitEvent.CommitSHA,
	})
}

func (server *Server) handleManualTrigger(writer http.ResponseWriter, request *http.Request) {
	rawBody, err := readBoundedBody(writer, request)
	if err != nil {
		problem.Write(writer, http.StatusRequestEntityTooLarge, "", "Payload Too Large", err.Error(), request.URL.Path)
		return
	}

	var event domain.CommitEvent
	if err := json.Unmarshal(rawBody, &event); err != nil {
		problem.Write(writer, http.StatusBadRequest, "", "Malformed JSON", "invalid commit trigger JSON payload", request.URL.Path)
		return
	}

	batch, execErr := server.config.TriggerUseCase.Execute(request.Context(), event)
	if errors.Is(execErr, domain.ErrValidationFailed) {
		problem.Write(writer, http.StatusBadRequest, "", "Validation Failed", execErr.Error(), request.URL.Path)
		return
	}
	if errors.Is(execErr, domain.ErrDuplicateCommitTrigger) {
		problem.Write(writer, http.StatusConflict, "", "Duplicate Commit Trigger", execErr.Error(), request.URL.Path)
		return
	}
	if execErr != nil {
		problem.Write(writer, http.StatusInternalServerError, "", "Pipeline Execution Failed", execErr.Error(), request.URL.Path)
		return
	}

	writeJSON(writer, http.StatusCreated, batch)
}

func (server *Server) handleDiscordInteraction(writer http.ResponseWriter, request *http.Request) {
	rawBody, err := readBoundedBody(writer, request)
	if err != nil {
		problem.Write(writer, http.StatusRequestEntityTooLarge, "", "Payload Too Large", err.Error(), request.URL.Path)
		return
	}

	if len(server.discordPublicKey) > 0 {
		sigHex := request.Header.Get("X-Signature-Ed25519")
		timestamp := request.Header.Get("X-Signature-Timestamp")
		if !verifyDiscordEd25519(server.discordPublicKey, timestamp, rawBody, sigHex) {
			problem.Write(writer, http.StatusUnauthorized, "", "Invalid Discord Signature", "Ed25519 verification failed", request.URL.Path)
			return
		}
	}

	var interaction struct {
		Type int `json:"type"`
		Data struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		Member *struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"member"`
		User *struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rawBody, &interaction); err != nil {
		problem.Write(writer, http.StatusBadRequest, "", "Malformed JSON", "invalid Discord interaction payload", request.URL.Path)
		return
	}

	// Discord PING check required during interaction endpoint verification.
	if interaction.Type == 1 {
		writeJSON(writer, http.StatusOK, map[string]int{"type": 1})
		return
	}

	if interaction.Type != 3 {
		problem.Write(writer, http.StatusBadRequest, "", "Unsupported Interaction", "expected Discord MESSAGE_COMPONENT interaction", request.URL.Path)
		return
	}

	command, err := parseDiscordCustomID(interaction.Data.CustomID)
	if err != nil {
		problem.Write(writer, http.StatusBadRequest, "", "Invalid Custom ID", err.Error(), request.URL.Path)
		return
	}
	command.Source = "discord"
	if interaction.Member != nil && interaction.Member.User.Username != "" {
		command.Actor = interaction.Member.User.Username
	} else if interaction.User != nil {
		command.Actor = interaction.User.Username
	}

	result, execErr := server.config.DecisionUseCase.Execute(request.Context(), command)
	if execErr != nil {
		problem.Write(writer, http.StatusInternalServerError, "", "Review Decision Failed", execErr.Error(), request.URL.Path)
		return
	}

	summary := fmt.Sprintf("Processed %s for %s (transitioned: %v, skipped: %v)",
		result.Action, command.TargetPlatform, result.TransitionedPlatforms, result.SkippedPlatforms)
	writeJSON(writer, http.StatusOK, map[string]any{
		"type": 4,
		"data": map[string]any{
			"content": summary,
			"flags":   64, // Ephemeral response visible to reviewer.
		},
	})
}

func (server *Server) handleTelegramWebhook(writer http.ResponseWriter, request *http.Request) {
	if strings.TrimSpace(server.config.TelegramWebhookSecret) != "" {
		provided := request.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(server.config.TelegramWebhookSecret)) != 1 {
			problem.Write(writer, http.StatusUnauthorized, "", "Invalid Telegram Secret", "X-Telegram-Bot-Api-Secret-Token verification failed", request.URL.Path)
			return
		}
	}

	rawBody, err := readBoundedBody(writer, request)
	if err != nil {
		problem.Write(writer, http.StatusRequestEntityTooLarge, "", "Payload Too Large", err.Error(), request.URL.Path)
		return
	}

	var update struct {
		CallbackQuery *struct {
			ID   string `json:"id"`
			Data string `json:"data"`
			From struct {
				Username string `json:"username"`
			} `json:"from"`
		} `json:"callback_query"`
	}
	if err := json.Unmarshal(rawBody, &update); err != nil {
		problem.Write(writer, http.StatusBadRequest, "", "Malformed JSON", "invalid Telegram update payload", request.URL.Path)
		return
	}
	if update.CallbackQuery == nil {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	command, err := parseTelegramCallbackData(update.CallbackQuery.Data)
	if err != nil {
		problem.Write(writer, http.StatusBadRequest, "", "Invalid Callback Data", err.Error(), request.URL.Path)
		return
	}
	command.Source = "telegram"
	command.Actor = update.CallbackQuery.From.Username

	result, execErr := server.config.DecisionUseCase.Execute(request.Context(), command)
	if execErr != nil {
		problem.Write(writer, http.StatusInternalServerError, "", "Review Decision Failed", execErr.Error(), request.URL.Path)
		return
	}

	if server.config.TelegramNotifier != nil {
		ackText := fmt.Sprintf("Done: %s (%d updated)", result.Action, len(result.TransitionedPlatforms))
		_ = server.config.TelegramNotifier.AnswerCallbackQuery(request.Context(), update.CallbackQuery.ID, ackText)
	}

	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleGetAsset(writer http.ResponseWriter, request *http.Request) {
	name := filepath.Base(strings.TrimSpace(request.PathValue("name")))
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		problem.Write(writer, http.StatusBadRequest, "", "Invalid Asset Name", "asset filename is invalid", request.URL.Path)
		return
	}
	fullPath := filepath.Join(server.config.AssetsDir, name)
	if _, err := os.Stat(fullPath); err != nil {
		problem.Write(writer, http.StatusNotFound, "", "Asset Not Found", "requested media asset does not exist", request.URL.Path)
		return
	}
	http.ServeFile(writer, request, fullPath)
}

func readBoundedBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(writer, request.Body, maxWebhookBodyBytes)
	defer func() { _ = limited.Close() }()
	return io.ReadAll(limited)
}

func verifyGitHubHMAC(payload []byte, secret, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sigBytes, expected)
}

func verifyDiscordEd25519(publicKey ed25519.PublicKey, timestamp string, body []byte, signatureHex string) bool {
	if strings.TrimSpace(timestamp) == "" || strings.TrimSpace(signatureHex) == "" {
		return false
	}
	sigBytes, err := hex.DecodeString(strings.TrimSpace(signatureHex))
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false
	}
	signedPayload := make([]byte, 0, len(timestamp)+len(body))
	signedPayload = append(signedPayload, []byte(timestamp)...)
	signedPayload = append(signedPayload, body...)
	return ed25519.Verify(publicKey, signedPayload, sigBytes)
}

func parseDiscordCustomID(customID string) (application.ReviewDecisionCommand, error) {
	parts := strings.Split(strings.TrimSpace(customID), ":")
	if len(parts) != 4 || parts[0] != "review" {
		return application.ReviewDecisionCommand{}, fmt.Errorf("%w: unexpected custom_id %q", domain.ErrValidationFailed, customID)
	}
	return application.ReviewDecisionCommand{
		BatchID:        parts[1],
		Action:         application.ReviewAction(parts[2]),
		TargetPlatform: parts[3],
	}, nil
}

func parseTelegramCallbackData(data string) (application.ReviewDecisionCommand, error) {
	parts := strings.Split(strings.TrimSpace(data), "|")
	if len(parts) != 4 || parts[0] != "rv" {
		return application.ReviewDecisionCommand{}, fmt.Errorf("%w: unexpected telegram callback data %q", domain.ErrValidationFailed, data)
	}
	var action application.ReviewAction
	switch parts[2] {
	case "ap":
		action = application.ReviewActionApprove
	case "rj":
		action = application.ReviewActionReject
	default:
		return application.ReviewDecisionCommand{}, fmt.Errorf("%w: unknown action code %q", domain.ErrValidationFailed, parts[2])
	}
	return application.ReviewDecisionCommand{
		BatchID:        parts[1],
		Action:         action,
		TargetPlatform: parts[3],
	}, nil
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
