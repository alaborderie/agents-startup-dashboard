package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type Venture struct {
	Slug             string     `json:"slug"`
	Title            string     `json:"title"`
	Field            string     `json:"field"`
	Status           string     `json:"status"`
	Stage            *string    `json:"stage"`
	Kind             *string    `json:"kind"`
	Description      *string    `json:"description"`
	Monetization     *string    `json:"monetization"`
	AbandonedReason  *string    `json:"abandoned_reason"`
	AbandonedAtStage *string    `json:"abandoned_at_stage"`
	URL              *string    `json:"url"`
	RepoURL          *string    `json:"repo_url"`
	CreatedAt        *time.Time `json:"created"`
}

type Snapshot struct {
	SnapshotID  string    `json:"snapshot_id"`
	GeneratedAt time.Time `json:"generated_at"`
	Cycle       int       `json:"cycle"`
	Source      string    `json:"source"`
	Ventures    []Venture `json:"ventures"`
}

type ApplyResult struct {
	Status       string   `json:"status"`
	Applied      int      `json:"applied,omitempty"`
	SkippedStale int      `json:"skipped_stale,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

func (s *Store) ApplySnapshot(ctx context.Context, snapshot Snapshot, valid []Venture, presentSlugs []string, warnings []string) (ApplyResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin snapshot: %w", err)
	}
	defer tx.Rollback(ctx)

	// Ingests are serialized so checking the most recently accepted snapshot and
	// writing its audit row are one atomic idempotency decision.
	if _, err := tx.Exec(ctx, "LOCK TABLE ingest_snapshots IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return ApplyResult{}, fmt.Errorf("lock ingest snapshots: %w", err)
	}

	if snapshot.SnapshotID != "" {
		var lastID string
		err := tx.QueryRow(ctx,
			"SELECT snapshot_id FROM ingest_snapshots ORDER BY id DESC LIMIT 1",
		).Scan(&lastID)
		if err != nil && err != pgx.ErrNoRows {
			return ApplyResult{}, fmt.Errorf("read last snapshot: %w", err)
		}
		if err == nil && lastID == snapshot.SnapshotID {
			if err := tx.Rollback(ctx); err != nil {
				return ApplyResult{}, fmt.Errorf("finish unchanged snapshot: %w", err)
			}
			return ApplyResult{Status: "unchanged"}, nil
		}
	}

	result := ApplyResult{Status: "applied", Warnings: warnings}
	for _, venture := range valid {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ventures (
				slug, title, field, status, stage, kind, description, monetization,
				abandoned_reason, abandoned_at_stage, url, repo_url, created_at,
				last_seen_at, missing_since, snapshot_generated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
				now(), NULL, $14
			)
			ON CONFLICT (slug) DO UPDATE SET
				title = EXCLUDED.title,
				field = EXCLUDED.field,
				status = EXCLUDED.status,
				stage = EXCLUDED.stage,
				kind = EXCLUDED.kind,
				description = EXCLUDED.description,
				monetization = EXCLUDED.monetization,
				abandoned_reason = EXCLUDED.abandoned_reason,
				abandoned_at_stage = EXCLUDED.abandoned_at_stage,
				url = EXCLUDED.url,
				repo_url = EXCLUDED.repo_url,
				created_at = EXCLUDED.created_at,
				last_seen_at = now(),
				missing_since = NULL,
				snapshot_generated_at = EXCLUDED.snapshot_generated_at
			WHERE EXCLUDED.snapshot_generated_at >= ventures.snapshot_generated_at`,
			venture.Slug, venture.Title, venture.Field, venture.Status, venture.Stage,
			venture.Kind, venture.Description, venture.Monetization, venture.AbandonedReason,
			venture.AbandonedAtStage, venture.URL, venture.RepoURL, venture.CreatedAt,
			snapshot.GeneratedAt,
		)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("upsert venture %q: %w", venture.Slug, err)
		}
		if tag.RowsAffected() == 0 {
			result.SkippedStale++
		} else {
			result.Applied++
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ventures
		SET missing_since = COALESCE(missing_since, now()),
		    snapshot_generated_at = $2
		WHERE NOT (slug = ANY($1::text[]))
		  AND snapshot_generated_at <= $2`, presentSlugs, snapshot.GeneratedAt); err != nil {
		return ApplyResult{}, fmt.Errorf("mark missing ventures: %w", err)
	}

	warningsJSON, err := json.Marshal(warnings)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("encode warnings: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ingest_snapshots (
			snapshot_id, generated_at, venture_count, applied, skipped_stale, warnings, source
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		snapshot.SnapshotID, snapshot.GeneratedAt, len(snapshot.Ventures), result.Applied,
		result.SkippedStale, warningsJSON, snapshot.Source,
	); err != nil {
		return ApplyResult{}, fmt.Errorf("record snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM ingest_snapshots
		WHERE id NOT IN (
			SELECT id FROM ingest_snapshots ORDER BY id DESC LIMIT 200
		)`); err != nil {
		return ApplyResult{}, fmt.Errorf("prune snapshots: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit snapshot: %w", err)
	}
	return result, nil
}

type DashboardVenture struct {
	Slug             string
	Title            string
	Field            string
	Status           string
	Stage            string
	Kind             string
	Description      string
	Monetization     string
	AbandonedReason  string
	AbandonedAtStage string
	URL              string
	RepoURL          string
	CreatedAt        *time.Time
}

func (s *Store) ListVentures(ctx context.Context) ([]DashboardVenture, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT slug, title, field, status, stage, kind, description, monetization,
		       abandoned_reason, abandoned_at_stage, url, repo_url, created_at
		FROM ventures
		WHERE hidden = false
		ORDER BY CASE status
			WHEN 'in_progress' THEN 1
			WHEN 'deployed' THEN 2
			WHEN 'abandoned' THEN 3
			ELSE 4
		END, created_at DESC NULLS LAST, slug`)
	if err != nil {
		return nil, fmt.Errorf("query ventures: %w", err)
	}
	defer rows.Close()

	var ventures []DashboardVenture
	for rows.Next() {
		var venture DashboardVenture
		var stage, kind, description, monetization sql.NullString
		var reason, reasonStage, liveURL, repoURL sql.NullString
		var created sql.NullTime
		if err := rows.Scan(
			&venture.Slug, &venture.Title, &venture.Field, &venture.Status,
			&stage, &kind, &description, &monetization, &reason, &reasonStage,
			&liveURL, &repoURL, &created,
		); err != nil {
			return nil, fmt.Errorf("scan venture: %w", err)
		}
		venture.Stage = stage.String
		venture.Kind = kind.String
		venture.Description = description.String
		venture.Monetization = monetization.String
		venture.AbandonedReason = reason.String
		venture.AbandonedAtStage = reasonStage.String
		venture.URL = liveURL.String
		venture.RepoURL = repoURL.String
		if created.Valid {
			venture.CreatedAt = &created.Time
		}
		ventures = append(ventures, venture)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ventures: %w", err)
	}
	return ventures, nil
}

func (s *Store) LastSync(ctx context.Context) (*time.Time, error) {
	var received sql.NullTime
	err := s.pool.QueryRow(ctx,
		"SELECT received_at FROM ingest_snapshots ORDER BY id DESC LIMIT 1",
	).Scan(&received)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query last sync: %w", err)
	}
	if !received.Valid {
		return nil, nil
	}
	return &received.Time, nil
}
