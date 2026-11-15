# ADR-0001: DigitalClone Hexagonal Architecture, Google ADK Pipeline & Publishing Split

## Status
Accepted — 2026-11-14

## Context
DigitalClone automates turning git commits and code pushes into platform-tailored social posts across LinkedIn, YouTube, X, and TikTok, routing drafts through an automated Critic Agent and human reviewers on Discord and Telegram before publishing.

Key engineering requirements:
1. **Strict Domain Isolation**: Core state machine invariants (`DRAFT -> CRITIQUED -> PENDING_REVIEW -> APPROVED/REJECTED -> PUBLISHED/FAILED`) must remain independent of LLM SDKs, databases, and social APIs.
2. **Concurrent Multi-Agent Synthesis & Critique**: Platform agents must run concurrently without race conditions and trigger a sequential Critic Agent pass before human notification.
3. **Zero Double-Publishing**: Reviewers receive interactive cards simultaneously in Discord and Telegram; clicking Approve or Reject on either channel must transition draft state atomically once.
4. **Resilient Publishing**: Buffer GraphQL API handles LinkedIn, X, and TikTok with public HTTPS media URLs (`shareNow` default, `addToQueue` configurable), whereas YouTube video/Shorts uploads use YouTube Data API v3 directly (`Videos.Insert`).

## Decision
1. **Hexagonal Architecture (Ports & Adapters)**:
   - `internal/domain/` defines pure entities and secondary port interfaces (`DraftRepository`, `PersonaLoader`, `MediaGenerator`, `AssetStorage`, `ReviewNotifier`, `PlatformPostService`, `CommitInspector`).
   - `internal/application/` orchestrates `google.golang.org/adk` (`sequentialagent` wrapping `parallelagent` of 4 platform `llmagent`s + sequential `content_critic_agent`) and exposes strongly-typed `functiontool` bindings.
   - `internal/infrastructure/` implements adapters for PostgreSQL (`SELECT ... FOR UPDATE`), SQLite fallback, Google GenAI (`gemini-2.5-flash`, `imagen-3.0-generate-002`, `veo-2.0-generate-001`), S3 SigV4 storage with local static HTTP fallback, Buffer GraphQL (`https://api.buffer.com`), YouTube Data API v3, Discord REST API v10, and Telegram Bot API.
   - `internal/presentation/http/` and `cmd/main.go` provide dual-mode triggers (GitHub HMAC webhook + local git CLI) and signed Discord Ed25519 / Telegram secret-token callbacks with RFC 7807 error envelopes.
2. **Fault Tolerance**: Outbound HTTP/API adapters are protected by circuit breakers, 5s request timeouts (for webhook/GraphQL calls), and exponential backoff with full jitter honoring `Retry-After` on HTTP 429.

## Consequences
- Every port can be unit-tested deterministically offline without external credentials.
- Concurrent Discord and Telegram button clicks cannot duplicate social media posts.
- Switching storage, persona sources, or individual publishing channels requires replacing only the concrete adapter implementing the domain port.
