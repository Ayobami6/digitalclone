# DigitalClone Implementation Todo

- [x] Initialize Hexagonal Architecture layers (`internal/domain`, `internal/application`, `internal/infrastructure`, `internal/presentation`, `pkg/resilience`, `pkg/problem`)
- [x] Implement Domain Entities, State Machine, Sentinel Errors, and Secondary Port Interfaces
- [x] Implement Google ADK Orchestrator (`SequentialAgent` -> `ParallelAgent` [LinkedIn, YouTube, X, TikTok] -> `ContentCriticAgent`) and typed `functiontool` bindings
- [x] Implement Google GenAI adapter (Gemini text, Imagen 3 image, Veo 2 video) & hybrid S3/local static HTTP media storage
- [x] Implement PostgreSQL (`SELECT ... FOR UPDATE`) and SQLite atomic `DraftRepository` adapters
- [x] Implement Buffer GraphQL API client (`https://api.buffer.com`) with `RateLimit`/`Retry-After` backoff and `LinkedInPostService`, `XPostService`, `TikTokPostService`
- [x] Implement `YouTubePostService` using YouTube Data API v3 (`Videos.Insert`)
- [x] Implement interactive Discord REST v10 and Telegram Bot API review notifiers & callback handlers
- [x] Implement dual-mode Commit Push Triggers (GitHub HMAC-SHA256 webhook + local `git` CLI `trigger` subcommand)
- [x] Write comprehensive unit & race-detector tests across all packages and verify `go test -race ./...`, `go vet ./...`, and `golangci-lint run`
