// Command digitalclone runs the multi-agent social content generation server and CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"digitalclone/internal/application"
	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/buffer"
	"digitalclone/internal/infrastructure/genai"
	"digitalclone/internal/infrastructure/git"
	"digitalclone/internal/infrastructure/notification"
	"digitalclone/internal/infrastructure/persistence"
	"digitalclone/internal/infrastructure/persona"
	"digitalclone/internal/infrastructure/publishers"
	"digitalclone/internal/infrastructure/storage"
	httpserver "digitalclone/internal/presentation/http"
)

type appConfig struct {
	Port                  string
	DatabaseURL           string
	SQLitePath            string
	PersonasDir           string
	AssetsDir             string
	PublicBaseURL         string
	GeminiAPIKey          string
	GeminiTextModel       string
	GeminiImageModel      string
	GeminiVideoModel      string
	BufferAPIKey          string
	BufferOrgID           string
	BufferLinkedInChannel string
	BufferXChannel        string
	BufferTikTokChannel   string
	BufferShareMode       buffer.ShareMode
	YouTubeClientID       string
	YouTubeClientSecret   string
	YouTubeRefreshToken   string
	YouTubeAccessToken    string
	YouTubePrivacyStatus  string
	DiscordBotToken       string
	DiscordChannelID      string
	DiscordPublicKeyHex   string
	TelegramBotToken      string
	TelegramChatID        string
	TelegramWebhookSecret string
	GitHubToken           string
	GitHubWebhookSecret   string
	S3Bucket              string
	S3Region              string
	S3Endpoint            string
	S3AccessKeyID         string
	S3SecretKey           string
	S3PublicBaseURL       string
	DryRun                bool
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runCLI(ctx, os.Args[1:], logger); err != nil {
		logger.Error("digitalclone failed", "error", err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, args []string, logger *slog.Logger) error {
	subcommand := "serve"
	subArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcommand = strings.ToLower(strings.TrimSpace(args[0]))
		subArgs = args[1:]
	}

	switch subcommand {
	case "serve":
		return runServeCommand(ctx, subArgs, logger)
	case "trigger":
		return runTriggerCommand(ctx, subArgs, logger)
	default:
		return fmt.Errorf("unknown subcommand %q (supported: serve, trigger)", subcommand)
	}
}

func runServeCommand(ctx context.Context, args []string, logger *slog.Logger) error {
	cfg := loadConfigFromEnv(logger)
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.Port, "port", cfg.Port, "HTTP server listen port")
	fs.BoolVar(&cfg.DryRun, "dry-run", cfg.DryRun, "Run in offline dry-run mode without external API calls")
	if err := fs.Parse(args); err != nil {
		return err
	}

	deps, cleanup, err := wireDependencies(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	srv, err := httpserver.NewServer(httpserver.ServerConfig{
		GitHubWebhookSecret:   cfg.GitHubWebhookSecret,
		DiscordPublicKeyHex:   cfg.DiscordPublicKeyHex,
		TelegramWebhookSecret: cfg.TelegramWebhookSecret,
		AssetsDir:             cfg.AssetsDir,
		CommitInspector:       deps.commitInspector,
		TelegramNotifier:      deps.telegramNotifier,
		TriggerUseCase:        deps.triggerUseCase,
		DecisionUseCase:       deps.decisionUseCase,
		Logger:                logger,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting DigitalClone HTTP server", "addr", httpServer.Addr, "dry_run", cfg.DryRun)
		if listenErr := httpServer.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			errCh <- listenErr
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down DigitalClone HTTP server")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return httpServer.Shutdown(shutdownCtx)
	case listenErr := <-errCh:
		return listenErr
	}
}

func runTriggerCommand(ctx context.Context, args []string, logger *slog.Logger) error {
	cfg := loadConfigFromEnv(logger)
	repoPath := "."
	commitRef := "HEAD"

	fs := flag.NewFlagSet("trigger", flag.ContinueOnError)
	fs.StringVar(&repoPath, "repo", ".", "Path to local git repository")
	fs.StringVar(&commitRef, "commit", "HEAD", "Git commit SHA or ref to transform into social posts")
	fs.BoolVar(&cfg.DryRun, "dry-run", cfg.DryRun, "Run in offline dry-run mode without external API calls")
	if err := fs.Parse(args); err != nil {
		return err
	}

	deps, cleanup, err := wireDependencies(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	commitEvent, err := deps.commitInspector.InspectLocalCommit(ctx, repoPath, commitRef)
	if err != nil {
		return fmt.Errorf("inspect local commit: %w", err)
	}

	batch, err := deps.triggerUseCase.Execute(ctx, *commitEvent)
	if err != nil {
		return fmt.Errorf("execute trigger pipeline: %w", err)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(batch)
}

type appDependencies struct {
	commitInspector  domain.CommitInspector
	telegramNotifier *notification.TelegramNotifier
	triggerUseCase   *application.TriggerCommitPipelineUseCase
	decisionUseCase  *application.ProcessReviewDecisionUseCase
}

func wireDependencies(ctx context.Context, cfg appConfig, logger *slog.Logger) (*appDependencies, func(), error) {
	var (
		repo *persistence.SQLDraftRepository
		err  error
	)
	if cfg.DatabaseURL != "" {
		repo, err = persistence.NewPostgresRepository(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, func() {}, fmt.Errorf("initialize postgres repository: %w", err)
		}
	} else {
		repo, err = persistence.NewSQLiteRepository(ctx, cfg.SQLitePath)
		if err != nil {
			return nil, func() {}, fmt.Errorf("initialize sqlite repository: %w", err)
		}
	}
	cleanup := func() {
		if closeErr := repo.Close(); closeErr != nil {
			logger.Warn("failed to close database repository", "error", closeErr)
		}
	}

	personaLoader := persona.NewFilesystemPersonaLoader(cfg.PersonasDir)
	mediaGen, err := genai.NewAdapter(ctx, genai.Config{
		APIKey:     cfg.GeminiAPIKey,
		TextModel:  cfg.GeminiTextModel,
		ImageModel: cfg.GeminiImageModel,
		VideoModel: cfg.GeminiVideoModel,
		DryRun:     cfg.DryRun,
	})
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("initialize genai adapter: %w", err)
	}

	assetStorage := storage.NewHybridAssetStorage(storage.Config{
		LocalDir:        cfg.AssetsDir,
		LocalBaseURL:    cfg.PublicBaseURL,
		S3Bucket:        cfg.S3Bucket,
		S3Region:        cfg.S3Region,
		S3Endpoint:      cfg.S3Endpoint,
		S3AccessKeyID:   cfg.S3AccessKeyID,
		S3SecretKey:     cfg.S3SecretKey,
		S3PublicBaseURL: cfg.S3PublicBaseURL,
	})

	toolkit := application.NewAgentToolkit(repo, personaLoader, mediaGen, assetStorage, logger)
	orchestrator, err := application.NewADKOrchestrator(ctx, application.OrchestratorConfig{
		AppName:      "digitalclone",
		ModelName:    cfg.GeminiTextModel,
		GeminiAPIKey: cfg.GeminiAPIKey,
		DryRun:       cfg.DryRun,
	}, toolkit, logger)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("initialize ADK orchestrator: %w", err)
	}

	discordNotifier := notification.NewDiscordNotifier(notification.DiscordConfig{
		BotToken:  cfg.DiscordBotToken,
		ChannelID: cfg.DiscordChannelID,
		DryRun:    cfg.DryRun,
		Logger:    logger,
	})
	telegramNotifier := notification.NewTelegramNotifier(notification.TelegramConfig{
		BotToken: cfg.TelegramBotToken,
		ChatID:   cfg.TelegramChatID,
		DryRun:   cfg.DryRun,
		Logger:   logger,
	})
	notifiers := []domain.ReviewNotifier{discordNotifier, telegramNotifier}

	bufferClient := buffer.NewClient(buffer.Config{
		APIKey:         cfg.BufferAPIKey,
		OrganizationID: cfg.BufferOrgID,
		ShareMode:      cfg.BufferShareMode,
		ChannelIDs: map[domain.Platform]string{
			domain.PlatformLinkedIn: cfg.BufferLinkedInChannel,
			domain.PlatformX:        cfg.BufferXChannel,
			domain.PlatformTikTok:   cfg.BufferTikTokChannel,
		},
		DryRun: cfg.DryRun,
		Logger: logger,
	})

	services := []domain.PlatformPostService{
		publishers.NewLinkedInPostService(bufferClient),
		publishers.NewXPostService(bufferClient),
		publishers.NewTikTokPostService(bufferClient),
		publishers.NewYouTubePostService(publishers.YouTubeConfig{
			ClientID:      cfg.YouTubeClientID,
			ClientSecret:  cfg.YouTubeClientSecret,
			RefreshToken:  cfg.YouTubeRefreshToken,
			AccessToken:   cfg.YouTubeAccessToken,
			PrivacyStatus: cfg.YouTubePrivacyStatus,
			DryRun:        cfg.DryRun,
		}),
	}

	commitInspector := git.NewCommitInspector(git.InspectorConfig{
		GitHubToken: cfg.GitHubToken,
	})

	return &appDependencies{
		commitInspector:  commitInspector,
		telegramNotifier: telegramNotifier,
		triggerUseCase:   application.NewTriggerCommitPipelineUseCase(repo, orchestrator, notifiers, logger),
		decisionUseCase:  application.NewProcessReviewDecisionUseCase(repo, services, notifiers, logger),
	}, cleanup, nil
}

func loadConfigFromEnv(logger *slog.Logger) appConfig {
	if err := loadDotEnv(".env"); err != nil && logger != nil {
		logger.Warn("failed to load .env file", "error", err)
	}

	shareMode := buffer.ShareModeShareNow
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BUFFER_SHARE_MODE")), string(buffer.ShareModeAddToQueue)) {
		shareMode = buffer.ShareModeAddToQueue
	}

	geminiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if geminiKey == "" {
		geminiKey = strings.TrimSpace(os.Getenv("GOOGLE_API_KEY"))
	}

	return appConfig{
		Port:                  envOrDefault("PORT", "8080"),
		DatabaseURL:           strings.TrimSpace(os.Getenv("DATABASE_URL")),
		SQLitePath:            envOrDefault("SQLITE_PATH", "./data/digitalclone.db"),
		PersonasDir:           envOrDefault("PERSONAS_DIR", "./personas"),
		AssetsDir:             envOrDefault("ASSET_LOCAL_DIR", "./assets"),
		PublicBaseURL:         envOrDefault("PUBLIC_BASE_URL", "http://localhost:8080"),
		GeminiAPIKey:          geminiKey,
		GeminiTextModel:       envOrDefault("GEMINI_TEXT_MODEL", "gemini-2.5-flash"),
		GeminiImageModel:      envOrDefault("GEMINI_IMAGE_MODEL", "imagen-3.0-generate-002"),
		GeminiVideoModel:      envOrDefault("GEMINI_VIDEO_MODEL", "veo-2.0-generate-001"),
		BufferAPIKey:          strings.TrimSpace(os.Getenv("BUFFER_API_KEY")),
		BufferOrgID:           strings.TrimSpace(os.Getenv("BUFFER_ORGANIZATION_ID")),
		BufferLinkedInChannel: strings.TrimSpace(os.Getenv("BUFFER_LINKEDIN_CHANNEL_ID")),
		BufferXChannel:        strings.TrimSpace(os.Getenv("BUFFER_X_CHANNEL_ID")),
		BufferTikTokChannel:   strings.TrimSpace(os.Getenv("BUFFER_TIKTOK_CHANNEL_ID")),
		BufferShareMode:       shareMode,
		YouTubeClientID:       strings.TrimSpace(os.Getenv("YOUTUBE_CLIENT_ID")),
		YouTubeClientSecret:   strings.TrimSpace(os.Getenv("YOUTUBE_CLIENT_SECRET")),
		YouTubeRefreshToken:   strings.TrimSpace(os.Getenv("YOUTUBE_REFRESH_TOKEN")),
		YouTubeAccessToken:    strings.TrimSpace(os.Getenv("YOUTUBE_ACCESS_TOKEN")),
		YouTubePrivacyStatus:  envOrDefault("YOUTUBE_PRIVACY_STATUS", "unlisted"),
		DiscordBotToken:       strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN")),
		DiscordChannelID:      strings.TrimSpace(os.Getenv("DISCORD_CHANNEL_ID")),
		DiscordPublicKeyHex:   strings.TrimSpace(os.Getenv("DISCORD_PUBLIC_KEY")),
		TelegramBotToken:      strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChatID:        strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),
		TelegramWebhookSecret: strings.TrimSpace(os.Getenv("TELEGRAM_WEBHOOK_SECRET")),
		GitHubToken:           strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		GitHubWebhookSecret:   strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_SECRET")),
		S3Bucket:              strings.TrimSpace(os.Getenv("S3_BUCKET")),
		S3Region:              envOrDefault("S3_REGION", "us-east-1"),
		S3Endpoint:            strings.TrimSpace(os.Getenv("S3_ENDPOINT")),
		S3AccessKeyID:         strings.TrimSpace(os.Getenv("S3_ACCESS_KEY_ID")),
		S3SecretKey:           strings.TrimSpace(os.Getenv("S3_SECRET_ACCESS_KEY")),
		S3PublicBaseURL:       strings.TrimSpace(os.Getenv("S3_PUBLIC_BASE_URL")),
		DryRun:                strings.EqualFold(strings.TrimSpace(os.Getenv("DRY_RUN")), "true"),
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// loadDotEnv reads KEY=VALUE pairs from filePath when present and sets any
// variables not already defined in the process environment.
func loadDotEnv(filePath string) error {
	cleanedPath := filepath.Clean(filePath)
	raw, err := os.ReadFile(cleanedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", cleanedPath, err)
	}

	for lineNum, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, rawValue, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		value := strings.TrimSpace(rawValue)
		if len(value) >= 2 && ((strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) ||
			(strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`))) {
			value = value[1 : len(value)-1]
		}

		// Preserve explicit process environment overrides over .env file entries.
		if _, exists := os.LookupEnv(key); !exists {
			if setErr := os.Setenv(key, value); setErr != nil {
				return fmt.Errorf("set env %s (line %d): %w", key, lineNum+1, setErr)
			}
		}
	}

	return nil
}
