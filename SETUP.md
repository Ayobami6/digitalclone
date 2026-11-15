# DigitalClone Setup Guide

Complete configuration and onboarding guide for running **DigitalClone** in local `--dry-run` mode or live production mode. All environment variables and defaults documented here map directly to [`cmd/main.go`](./cmd/main.go) and [`internal/presentation/http/server.go`](./internal/presentation/http/server.go).

---

## Prerequisites

- **Go**: `1.27+`
- **Git**: Available on `PATH` for local commit inspection (`digitalclone trigger`)
- **Public HTTPS Endpoint** *(required for live webhooks and media hosting)*: Reverse proxy, cloud deployment, or tunnel (e.g., Cloudflare Tunnel or ngrok) forwarding to `PORT` (`8080` by default)

---

## Quick Verification (Offline Dry-Run)

Before connecting live API keys, verify the toolchain and offline multi-agent pipeline:

```bash
go test -race ./...
go vet ./...
go run ./cmd/main.go trigger --repo . --commit HEAD --dry-run
```

---

## 1. Google ADK & GenAI (Required for Live Generation)

DigitalClone uses `gemini-2.5-flash` for text/critic agents, `imagen-3.0-generate-002` for LinkedIn and X images, and `veo-2.0-generate-001` for vertical 9:16 YouTube Shorts and TikTok videos.

Create an API key in Google AI Studio or Google Cloud and export either `GEMINI_API_KEY` or `GOOGLE_API_KEY`:

```bash
export GEMINI_API_KEY="your-gemini-api-key"

# Optional model overrides (defaults shown):
export GEMINI_TEXT_MODEL="gemini-2.5-flash"
export GEMINI_IMAGE_MODEL="imagen-3.0-generate-002"
export GEMINI_VIDEO_MODEL="veo-2.0-generate-001"
```

---

## 2. Public URL & Media Asset Storage

Buffer's GraphQL `createPost` mutation requires publicly reachable HTTPS URLs for image and video attachments. DigitalClone supports two modes:

### Option A: Local Static Asset Server + Tunnel / Reverse Proxy (Default)

Expose your HTTP server (`GET /assets/{name}`) on a public HTTPS domain:

```bash
export ASSET_LOCAL_DIR="./assets"
export PUBLIC_BASE_URL="https://your-public-domain.com"
```

### Option B: S3-Compatible Object Storage

If `S3_BUCKET`, `S3_ACCESS_KEY_ID`, and `S3_SECRET_ACCESS_KEY` are configured, generated media is uploaded using AWS SigV4 `PUT` requests instead of relying solely on local disk URLs:

```bash
export S3_BUCKET="your-bucket"
export S3_REGION="us-east-1"
export S3_ENDPOINT="https://your-s3-endpoint"
export S3_ACCESS_KEY_ID="your-access-key"
export S3_SECRET_ACCESS_KEY="your-secret-key"
export S3_PUBLIC_BASE_URL="https://cdn.your-domain.com"
```

---

## 3. Buffer GraphQL API (LinkedIn, X, TikTok)

LinkedIn, X, and TikTok posts are published through the Buffer GraphQL API (`https://api.buffer.com`).

1. Connect your LinkedIn, X, and TikTok channels inside your Buffer account.
2. Generate a Buffer API token and configure:

```bash
export BUFFER_API_KEY="your-buffer-token"
export BUFFER_SHARE_MODE="shareNow" # "shareNow" (default) or "addToQueue"

# Optional: auto-discovered via GetAccountHierarchy & GetChannels if omitted
export BUFFER_ORGANIZATION_ID=""
export BUFFER_LINKEDIN_CHANNEL_ID=""
export BUFFER_X_CHANNEL_ID=""
export BUFFER_TIKTOK_CHANNEL_ID=""
```

---

## 4. YouTube Data API v3 (Direct Video / Shorts Upload)

YouTube videos are uploaded directly via Google's `youtube/v3` `Videos.Insert` API rather than Buffer.

1. In Google Cloud Console, enable **YouTube Data API v3**.
2. Create OAuth 2.0 credentials authorized with the `https://www.googleapis.com/auth/youtube.upload` scope and obtain a refresh token (or short-lived access token):

```bash
export YOUTUBE_CLIENT_ID="your-oauth-client-id"
export YOUTUBE_CLIENT_SECRET="your-oauth-client-secret"
export YOUTUBE_REFRESH_TOKEN="your-oauth-refresh-token"
export YOUTUBE_PRIVACY_STATUS="unlisted" # "unlisted" (default), "private", or "public"
```

*(For short-lived manual testing you can also supply `YOUTUBE_ACCESS_TOKEN` directly.)*

---

## 5. Human-in-the-Loop Review (Discord & Telegram)

DigitalClone broadcasts interactive review cards with **Approve All**, **Reject All**, and per-platform **Approve** / **Reject** buttons. Concurrent or duplicate clicks across Discord and Telegram are synchronized atomically at the database layer so each approved draft publishes at most once.

### Discord Bot & Interactions

1. In the **Discord Developer Portal**, create an application and bot.
2. Copy the application's **Public Key** (64-character hex string) and **Bot Token**, and invite the bot to your review server/channel.
3. Set **Interactions Endpoint URL** to:
   `https://your-public-domain.com/webhooks/discord/interactions`
4. Export:

```bash
export DISCORD_BOT_TOKEN="your-discord-bot-token"
export DISCORD_CHANNEL_ID="your-channel-id"
export DISCORD_PUBLIC_KEY="your-64-char-hex-public-key"
```

### Telegram Bot & Webhook

1. Create a bot via `@BotFather` to obtain your `TELEGRAM_BOT_TOKEN`, and retrieve the target `TELEGRAM_CHAT_ID`.
2. Choose a random `TELEGRAM_WEBHOOK_SECRET` string and export:

```bash
export TELEGRAM_BOT_TOKEN="123456:ABC-DEF..."
export TELEGRAM_CHAT_ID="-1001234567890"
export TELEGRAM_WEBHOOK_SECRET="your-random-secret-token"
```

3. Register the webhook with Telegram so callback button clicks reach `POST /webhooks/telegram`:

```bash
curl -X POST "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/setWebhook" \
  -d "url=https://your-public-domain.com/webhooks/telegram&secret_token=${TELEGRAM_WEBHOOK_SECRET}"
```

---

## 6. Commit Push Triggers (GitHub Webhook or Local CLI)

### Option A: GitHub Push Webhook (`POST /webhooks/github`)

1. Export a GitHub personal access token (used to fetch commit metadata and diff patches) and a shared HMAC-SHA256 webhook secret:

```bash
export GITHUB_TOKEN="ghp_your_token"
export GITHUB_WEBHOOK_SECRET="your-hmac-webhook-secret"
```

2. In your GitHub repository under **Settings → Webhooks → Add webhook**:
   - **Payload URL**: `https://your-public-domain.com/webhooks/github`
   - **Content type**: `application/json`
   - **Secret**: matches `GITHUB_WEBHOOK_SECRET`
   - **Events**: select **Just the push event**

### Option B: Manual HTTP API Trigger (`POST /api/v1/triggers/commit`)

You can trigger the pipeline programmatically with JSON commit metadata (`repo_name`, `branch`, `commit_sha`, `author`, `message`, optional `diff_patch` and `changed_files`).

### Option C: Local CLI Trigger (`digitalclone trigger`)

Inspect any local git commit directly (useful for local development or a git `post-commit` hook):

```bash
go run ./cmd/main.go trigger --repo /path/to/repo --commit HEAD
```

Add `--dry-run` (or set `DRY_RUN=true`) to test offline without external API calls.

---

## 7. Persistence & Persona Customization

### Database

- **SQLite (Default)**: No extra setup required; stores state in `SQLITE_PATH` (`./data/digitalclone.db` by default).
- **PostgreSQL (Recommended for Production)**: Set `DATABASE_URL` to use PostgreSQL with `SELECT ... FOR UPDATE` row-level locking:

```bash
export DATABASE_URL="postgres://user:password@localhost:5432/digitalclone?sslmode=disable"
```

### Platform Personas

Each ADK subagent loads its voice, formatting constraints, and CTA guidelines from `PERSONAS_DIR` (`./personas` by default):

- [`personas/linkedin.md`](./personas/linkedin.md)
- [`personas/youtube.md`](./personas/youtube.md)
- [`personas/x.md`](./personas/x.md)
- [`personas/tiktok.md`](./personas/tiktok.md)

---

## 8. Running the Service

```bash
# Build binary
make build

# Start HTTP server (default port 8080)
make run
# or directly:
go run ./cmd/main.go serve --port 8080
```

### HTTP Endpoints Overview

| Method & Path | Purpose |
| :--- | :--- |
| `GET /healthz` | Liveness/health status check |
| `POST /webhooks/github` | GitHub `push` webhook receiver (`X-Hub-Signature-256` HMAC-SHA256 verified) |
| `POST /api/v1/triggers/commit` | Manual commit trigger endpoint |
| `POST /webhooks/discord/interactions` | Discord component interaction handler (`Ed25519` signature verified) |
| `POST /webhooks/telegram` | Telegram `callback_query` webhook handler (`X-Telegram-Bot-Api-Secret-Token` verified) |
| `GET /assets/{name}` | Static local media asset endpoint |
