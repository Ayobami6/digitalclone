// Package persistence implements durable DraftRepository adapters backed by
// PostgreSQL (with SELECT ... FOR UPDATE row locking) and SQLite fallback.
package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite"
	_ "github.com/jackc/pgx/v5/stdlib"

	"digitalclone/internal/domain"
)

type sqlDialect string

const (
	dialectPostgres sqlDialect = "postgres"
	dialectSQLite   sqlDialect = "sqlite"
)

// SQLDraftRepository implements domain.DraftRepository using raw parameterized SQL.
type SQLDraftRepository struct {
	db      *sql.DB
	dialect sqlDialect
	// SQLite writes serialize on a process-level mutex to eliminate SQLITE_BUSY under concurrent tests/webhooks.
	sqliteWriteMu sync.Mutex
}

// NewPostgresRepository connects to PostgreSQL via pgx/stdlib and runs idempotent schema migrations.
func NewPostgresRepository(ctx context.Context, databaseURL string) (*SQLDraftRepository, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("%w: postgres database URL is required", domain.ErrValidationFailed)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres connection: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	repo := &SQLDraftRepository{
		db:      db,
		dialect: dialectPostgres,
	}
	if err := repo.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

// NewSQLiteRepository opens or creates a local SQLite database for CLI and development fallback.
func NewSQLiteRepository(ctx context.Context, dbPath string) (*SQLDraftRepository, error) {
	if strings.TrimSpace(dbPath) == "" {
		dbPath = "./data/digitalclone.db"
	}
	if dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file:") {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite connection: %w", err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure sqlite pragma %q: %w", pragma, err)
		}
	}

	repo := &SQLDraftRepository{
		db:      db,
		dialect: dialectSQLite,
	}
	if err := repo.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

// Close releases underlying database connections.
func (repo *SQLDraftRepository) Close() error {
	if repo.db == nil {
		return nil
	}
	return repo.db.Close()
}

func (repo *SQLDraftRepository) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS draft_batches (
			id TEXT PRIMARY KEY,
			idempotency_key TEXT NOT NULL UNIQUE,
			repo_name TEXT NOT NULL,
			branch TEXT NOT NULL,
			commit_sha TEXT NOT NULL,
			author TEXT NOT NULL,
			message TEXT NOT NULL,
			changed_files_json TEXT NOT NULL,
			diff_patch TEXT NOT NULL,
			commit_timestamp TEXT NOT NULL,
			discord_message_id TEXT NOT NULL DEFAULT '',
			telegram_message_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS content_drafts (
			id TEXT PRIMARY KEY,
			batch_id TEXT NOT NULL REFERENCES draft_batches(id) ON DELETE CASCADE,
			commit_sha TEXT NOT NULL,
			platform TEXT NOT NULL,
			title TEXT NOT NULL,
			body_text TEXT NOT NULL,
			tags_json TEXT NOT NULL,
			media_id TEXT NOT NULL DEFAULT '',
			media_type TEXT NOT NULL DEFAULT 'NONE',
			media_mime_type TEXT NOT NULL DEFAULT '',
			media_local_path TEXT NOT NULL DEFAULT '',
			media_public_url TEXT NOT NULL DEFAULT '',
			media_size_bytes BIGINT NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			critic_notes TEXT NOT NULL DEFAULT '',
			external_post_id TEXT NOT NULL DEFAULT '',
			failure_reason TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(batch_id, platform)
		);`,
	}

	for _, statement := range statements {
		if _, err := repo.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("run migration statement: %w", err)
		}
	}
	return nil
}

// CreateBatchIfAbsent inserts a DraftBatch if its IdempotencyKey is not already stored.
func (repo *SQLDraftRepository) CreateBatchIfAbsent(ctx context.Context, batch *domain.DraftBatch) (bool, error) {
	if batch == nil {
		return false, fmt.Errorf("%w: batch cannot be nil", domain.ErrValidationFailed)
	}
	if repo.dialect == dialectSQLite {
		repo.sqliteWriteMu.Lock()
		defer repo.sqliteWriteMu.Unlock()
	}

	changedFilesJSON, err := json.Marshal(batch.CommitEvent.ChangedFiles)
	if err != nil {
		return false, fmt.Errorf("marshal changed_files: %w", err)
	}

	query := repo.rebind(`
		INSERT INTO draft_batches (
			id, idempotency_key, repo_name, branch, commit_sha, author, message,
			changed_files_json, diff_patch, commit_timestamp, discord_message_id, telegram_message_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (idempotency_key) DO NOTHING
	`)

	result, err := repo.db.ExecContext(
		ctx,
		query,
		batch.ID,
		batch.CommitEvent.IdempotencyKey,
		batch.CommitEvent.RepoName,
		batch.CommitEvent.Branch,
		batch.CommitEvent.CommitSHA,
		batch.CommitEvent.Author,
		batch.CommitEvent.Message,
		string(changedFilesJSON),
		batch.CommitEvent.DiffPatch,
		batch.CommitEvent.Timestamp.UTC().Format(time.RFC3339Nano),
		batch.DiscordMessageID,
		batch.TelegramMessageID,
		batch.CreatedAt.UTC().Format(time.RFC3339Nano),
		batch.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("insert draft batch: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check inserted batch rows: %w", err)
	}
	return rowsAffected == 1, nil
}

// SaveDraft inserts or updates a ContentDraft keyed by (batch_id, platform).
func (repo *SQLDraftRepository) SaveDraft(ctx context.Context, draft *domain.ContentDraft) error {
	if draft == nil {
		return fmt.Errorf("%w: draft cannot be nil", domain.ErrValidationFailed)
	}
	if repo.dialect == dialectSQLite {
		repo.sqliteWriteMu.Lock()
		defer repo.sqliteWriteMu.Unlock()
	}

	tagsJSON, err := json.Marshal(draft.Tags)
	if err != nil {
		return fmt.Errorf("marshal draft tags: %w", err)
	}

	mediaID := ""
	mediaType := string(domain.MediaNone)
	mediaMimeType := ""
	mediaLocalPath := ""
	mediaPublicURL := ""
	var mediaSizeBytes int64
	if draft.MediaAsset != nil {
		mediaID = draft.MediaAsset.ID
		mediaType = string(draft.MediaAsset.MediaType)
		mediaMimeType = draft.MediaAsset.MimeType
		mediaLocalPath = draft.MediaAsset.LocalPath
		mediaPublicURL = draft.MediaAsset.PublicURL
		mediaSizeBytes = draft.MediaAsset.SizeBytes
	}

	query := repo.rebind(`
		INSERT INTO content_drafts (
			id, batch_id, commit_sha, platform, title, body_text, tags_json,
			media_id, media_type, media_mime_type, media_local_path, media_public_url, media_size_bytes,
			status, critic_notes, external_post_id, failure_reason, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (batch_id, platform) DO UPDATE SET
			title = excluded.title,
			body_text = excluded.body_text,
			tags_json = excluded.tags_json,
			media_id = excluded.media_id,
			media_type = excluded.media_type,
			media_mime_type = excluded.media_mime_type,
			media_local_path = excluded.media_local_path,
			media_public_url = excluded.media_public_url,
			media_size_bytes = excluded.media_size_bytes,
			status = excluded.status,
			version = content_drafts.version + 1,
			updated_at = excluded.updated_at
	`)

	_, err = repo.db.ExecContext(
		ctx,
		query,
		draft.ID,
		draft.BatchID,
		draft.CommitSHA,
		string(draft.Platform),
		draft.Title,
		draft.BodyText,
		string(tagsJSON),
		mediaID,
		mediaType,
		mediaMimeType,
		mediaLocalPath,
		mediaPublicURL,
		mediaSizeBytes,
		string(draft.Status),
		draft.CriticNotes,
		draft.ExternalPostID,
		draft.FailureReason,
		draft.Version,
		draft.CreatedAt.UTC().Format(time.RFC3339Nano),
		draft.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert content draft: %w", err)
	}
	return nil
}

// GetDraft retrieves a single platform ContentDraft inside a batch.
func (repo *SQLDraftRepository) GetDraft(ctx context.Context, batchID string, platform domain.Platform) (*domain.ContentDraft, error) {
	query := repo.rebind(`
		SELECT id, batch_id, commit_sha, platform, title, body_text, tags_json,
			media_id, media_type, media_mime_type, media_local_path, media_public_url, media_size_bytes,
			status, critic_notes, external_post_id, failure_reason, version, created_at, updated_at
		FROM content_drafts
		WHERE batch_id = ? AND platform = ?
	`)
	row := repo.db.QueryRowContext(ctx, query, batchID, string(platform))
	draft, err := scanDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: batch=%s platform=%s", domain.ErrDraftNotFound, batchID, platform)
	}
	if err != nil {
		return nil, fmt.Errorf("query draft: %w", err)
	}
	return draft, nil
}

// GetBatch retrieves a DraftBatch and all of its associated platform ContentDrafts.
func (repo *SQLDraftRepository) GetBatch(ctx context.Context, batchID string) (*domain.DraftBatch, error) {
	batchQuery := repo.rebind(`
		SELECT id, idempotency_key, repo_name, branch, commit_sha, author, message,
			changed_files_json, diff_patch, commit_timestamp, discord_message_id, telegram_message_id,
			created_at, updated_at
		FROM draft_batches
		WHERE id = ?
	`)

	var (
		changedFilesJSON  string
		commitTimestamp   string
		createdAtRaw      string
		updatedAtRaw      string
		batch             domain.DraftBatch
	)
	err := repo.db.QueryRowContext(ctx, batchQuery, batchID).Scan(
		&batch.ID,
		&batch.CommitEvent.IdempotencyKey,
		&batch.CommitEvent.RepoName,
		&batch.CommitEvent.Branch,
		&batch.CommitEvent.CommitSHA,
		&batch.CommitEvent.Author,
		&batch.CommitEvent.Message,
		&changedFilesJSON,
		&batch.CommitEvent.DiffPatch,
		&commitTimestamp,
		&batch.DiscordMessageID,
		&batch.TelegramMessageID,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", domain.ErrBatchNotFound, batchID)
	}
	if err != nil {
		return nil, fmt.Errorf("query batch: %w", err)
	}

	_ = json.Unmarshal([]byte(changedFilesJSON), &batch.CommitEvent.ChangedFiles)
	batch.CommitEvent.Timestamp, _ = time.Parse(time.RFC3339Nano, commitTimestamp)
	batch.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtRaw)
	batch.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAtRaw)
	batch.Drafts = make(map[domain.Platform]*domain.ContentDraft)

	draftsQuery := repo.rebind(`
		SELECT id, batch_id, commit_sha, platform, title, body_text, tags_json,
			media_id, media_type, media_mime_type, media_local_path, media_public_url, media_size_bytes,
			status, critic_notes, external_post_id, failure_reason, version, created_at, updated_at
		FROM content_drafts
		WHERE batch_id = ?
	`)
	rows, err := repo.db.QueryContext(ctx, draftsQuery, batchID)
	if err != nil {
		return nil, fmt.Errorf("query batch drafts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		draft, scanErr := scanDraft(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan draft row: %w", scanErr)
		}
		batch.Drafts[draft.Platform] = draft
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch drafts: %w", err)
	}

	return &batch, nil
}

// UpdateDraftContent atomically applies Content Critic refinements and moves the draft to CRITIQUED.
func (repo *SQLDraftRepository) UpdateDraftContent(
	ctx context.Context,
	batchID string,
	platform domain.Platform,
	title string,
	bodyText string,
	criticNotes string,
	tags []string,
) (*domain.ContentDraft, error) {
	if repo.dialect == dialectSQLite {
		repo.sqliteWriteMu.Lock()
		defer repo.sqliteWriteMu.Unlock()
	}

	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin critique update tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	selectQuery := `
		SELECT id, batch_id, commit_sha, platform, title, body_text, tags_json,
			media_id, media_type, media_mime_type, media_local_path, media_public_url, media_size_bytes,
			status, critic_notes, external_post_id, failure_reason, version, created_at, updated_at
		FROM content_drafts
		WHERE batch_id = ? AND platform = ?
	`
	if repo.dialect == dialectPostgres {
		selectQuery += " FOR UPDATE"
	}

	draft, err := scanDraft(tx.QueryRowContext(ctx, repo.rebind(selectQuery), batchID, string(platform)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: batch=%s platform=%s", domain.ErrDraftNotFound, batchID, platform)
	}
	if err != nil {
		return nil, fmt.Errorf("lock draft for critique update: %w", err)
	}

	if err := draft.ApplyCritique(title, bodyText, criticNotes, tags); err != nil {
		return nil, err
	}

	tagsJSON, err := json.Marshal(draft.Tags)
	if err != nil {
		return nil, fmt.Errorf("marshal critiqued tags: %w", err)
	}

	updateQuery := repo.rebind(`
		UPDATE content_drafts
		SET title = ?, body_text = ?, tags_json = ?, critic_notes = ?, status = ?, version = ?, updated_at = ?
		WHERE id = ?
	`)
	if _, err := tx.ExecContext(
		ctx,
		updateQuery,
		draft.Title,
		draft.BodyText,
		string(tagsJSON),
		draft.CriticNotes,
		string(draft.Status),
		draft.Version,
		draft.UpdatedAt.UTC().Format(time.RFC3339Nano),
		draft.ID,
	); err != nil {
		return nil, fmt.Errorf("update critiqued draft: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit critique update: %w", err)
	}
	return draft, nil
}

// TransitionDraftStatusAtomically locks the draft row and transitions it once, preventing concurrent double-publishing.
func (repo *SQLDraftRepository) TransitionDraftStatusAtomically(
	ctx context.Context,
	batchID string,
	platform domain.Platform,
	targetStatus domain.DraftStatus,
) (*domain.ContentDraft, bool, error) {
	if repo.dialect == dialectSQLite {
		repo.sqliteWriteMu.Lock()
		defer repo.sqliteWriteMu.Unlock()
	}

	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin status transition tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	selectQuery := `
		SELECT id, batch_id, commit_sha, platform, title, body_text, tags_json,
			media_id, media_type, media_mime_type, media_local_path, media_public_url, media_size_bytes,
			status, critic_notes, external_post_id, failure_reason, version, created_at, updated_at
		FROM content_drafts
		WHERE batch_id = ? AND platform = ?
	`
	if repo.dialect == dialectPostgres {
		// Row-level pessimistic lock prevents concurrent Discord and Telegram webhooks from claiming the same draft.
		selectQuery += " FOR UPDATE"
	}

	draft, err := scanDraft(tx.QueryRowContext(ctx, repo.rebind(selectQuery), batchID, string(platform)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: batch=%s platform=%s", domain.ErrDraftNotFound, batchID, platform)
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock draft row: %w", err)
	}

	if !draft.CanTransitionTo(targetStatus) {
		return draft, false, nil
	}

	switch targetStatus {
	case domain.StatusPendingReview:
		err = draft.MarkPendingReview()
	case domain.StatusApproved:
		err = draft.Approve()
	case domain.StatusRejected:
		err = draft.Reject()
	default:
		return draft, false, fmt.Errorf("%w: unsupported atomic target status %s", domain.ErrInvalidStateTransition, targetStatus)
	}
	if err != nil {
		return draft, false, err
	}

	updateQuery := repo.rebind(`
		UPDATE content_drafts
		SET status = ?, version = ?, updated_at = ?
		WHERE id = ? AND version = ?
	`)
	result, err := tx.ExecContext(
		ctx,
		updateQuery,
		string(draft.Status),
		draft.Version,
		draft.UpdatedAt.UTC().Format(time.RFC3339Nano),
		draft.ID,
		draft.Version-1,
	)
	if err != nil {
		return nil, false, fmt.Errorf("update draft status: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("check status update rows: %w", err)
	}
	if affected == 0 {
		return draft, false, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit status transition: %w", err)
	}
	return draft, true, nil
}

// UpdateReviewMessageIDs saves Discord and Telegram review message identifiers on the batch.
func (repo *SQLDraftRepository) UpdateReviewMessageIDs(ctx context.Context, batchID, discordMessageID, telegramMessageID string) error {
	if repo.dialect == dialectSQLite {
		repo.sqliteWriteMu.Lock()
		defer repo.sqliteWriteMu.Unlock()
	}

	query := repo.rebind(`
		UPDATE draft_batches
		SET discord_message_id = ?, telegram_message_id = ?, updated_at = ?
		WHERE id = ?
	`)
	_, err := repo.db.ExecContext(
		ctx,
		query,
		discordMessageID,
		telegramMessageID,
		time.Now().UTC().Format(time.RFC3339Nano),
		batchID,
	)
	if err != nil {
		return fmt.Errorf("update review message ids: %w", err)
	}
	return nil
}

// RecordPublishedPost updates a draft with final publishing outcome (PUBLISHED or FAILED).
func (repo *SQLDraftRepository) RecordPublishedPost(
	ctx context.Context,
	draftID string,
	status domain.DraftStatus,
	externalPostID string,
	failureReason string,
) error {
	if repo.dialect == dialectSQLite {
		repo.sqliteWriteMu.Lock()
		defer repo.sqliteWriteMu.Unlock()
	}

	query := repo.rebind(`
		UPDATE content_drafts
		SET status = ?, external_post_id = ?, failure_reason = ?, version = version + 1, updated_at = ?
		WHERE id = ?
	`)
	result, err := repo.db.ExecContext(
		ctx,
		query,
		string(status),
		strings.TrimSpace(externalPostID),
		strings.TrimSpace(failureReason),
		time.Now().UTC().Format(time.RFC3339Nano),
		draftID,
	)
	if err != nil {
		return fmt.Errorf("record published post: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check published update rows: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: draft_id=%s", domain.ErrDraftNotFound, draftID)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDraft(scanner rowScanner) (*domain.ContentDraft, error) {
	var (
		draft          domain.ContentDraft
		platformRaw    string
		tagsJSON       string
		mediaID        string
		mediaTypeRaw   string
		mediaMimeType  string
		mediaLocalPath string
		mediaPublicURL string
		mediaSizeBytes int64
		statusRaw      string
		createdAtRaw   string
		updatedAtRaw   string
	)
	err := scanner.Scan(
		&draft.ID,
		&draft.BatchID,
		&draft.CommitSHA,
		&platformRaw,
		&draft.Title,
		&draft.BodyText,
		&tagsJSON,
		&mediaID,
		&mediaTypeRaw,
		&mediaMimeType,
		&mediaLocalPath,
		&mediaPublicURL,
		&mediaSizeBytes,
		&statusRaw,
		&draft.CriticNotes,
		&draft.ExternalPostID,
		&draft.FailureReason,
		&draft.Version,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if err != nil {
		return nil, err
	}

	draft.Platform = domain.Platform(platformRaw)
	draft.Status = domain.DraftStatus(statusRaw)
	_ = json.Unmarshal([]byte(tagsJSON), &draft.Tags)
	draft.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAtRaw)
	draft.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAtRaw)

	mediaType := domain.MediaType(mediaTypeRaw)
	if mediaType != "" && mediaType != domain.MediaNone || mediaPublicURL != "" || mediaLocalPath != "" {
		draft.MediaAsset = &domain.MediaAsset{
			ID:        mediaID,
			MediaType: mediaType,
			MimeType:  mediaMimeType,
			LocalPath: mediaLocalPath,
			PublicURL: mediaPublicURL,
			SizeBytes: mediaSizeBytes,
		}
	}

	return &draft, nil
}

func (repo *SQLDraftRepository) rebind(query string) string {
	if repo.dialect != dialectPostgres {
		return query
	}
	var builder strings.Builder
	builder.Grow(len(query) + 16)
	paramIndex := 1
	for _, ch := range query {
		if ch == '?' {
			builder.WriteByte('$')
			builder.WriteString(strconv.Itoa(paramIndex))
			paramIndex++
		} else {
			builder.WriteRune(ch)
		}
	}
	return builder.String()
}
