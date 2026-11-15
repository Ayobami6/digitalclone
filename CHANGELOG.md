# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Hexagonal Architecture core (`internal/domain`, `internal/application`, `internal/infrastructure`, `internal/presentation`) for commit-triggered social content automation.
- Google ADK (`google.golang.org/adk`) multi-agent pipeline combining `ParallelAgent` (LinkedIn, YouTube, X, TikTok `llmagent`s) and sequential `ContentCriticAgent` with typed `functiontool` tools (`load_persona_skill`, `generate_content_text`, `generate_content_image`, `generate_content_video`, `save_content_draft`, `get_content`, `update_content`).
- Google GenAI (`google.golang.org/genai`) integration for text, Imagen 3 images, and Veo 2 videos, backed by hybrid S3 SigV4 + local `/assets/{name}` static HTTP media storage.
- Buffer GraphQL API (`https://api.buffer.com`) publishing services for LinkedIn, X, and TikTok with `shareNow`/`addToQueue` support and HTTP `429 Retry-After` backoff.
- Direct YouTube Data API v3 (`Videos.Insert`) publishing service for YouTube Shorts/videos.
- Interactive human-in-the-loop review cards and Ed25519 / secret-token callback endpoints for Discord and Telegram synced via atomic row-locked state transitions in PostgreSQL / SQLite.
- Dual-mode commit trigger via GitHub HMAC-SHA256 webhook (`POST /webhooks/github`) and local CLI (`digitalclone trigger`).
- Customizable platform persona skills under `personas/{linkedin,youtube,x,tiktok}.md` and [ADR-0001](docs/adr/0001-digitalclone-hexagonal-adk-architecture.md).
