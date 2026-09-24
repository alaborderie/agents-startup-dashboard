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
	DesignURL        *string    `json:"design_url"`
	CreatedAt        *time.Time `json:"created"`
}

func (v *Venture) UnmarshalJSON(data []byte) error {
	type plainVenture Venture

	var decoded plainVenture
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["created_at"]; ok {
		var createdAt *time.Time
		if err := json.Unmarshal(raw, &createdAt); err != nil {
			return fmt.Errorf("decode created_at: %w", err)
		}
		decoded.CreatedAt = createdAt
	}

	*v = Venture(decoded)
	return nil
}

type Snapshot struct {
	SnapshotID  string              `json:"snapshot_id"`
	GeneratedAt time.Time           `json:"generated_at"`
	Cycle       int                 `json:"cycle"`
	Source      string              `json:"source"`
	Ventures    []Venture           `json:"ventures"`
	Tokens      *TokenBlock         `json:"tokens"`
	LastTick    *time.Time          `json:"last_tick"`
	NextTick    *time.Time          `json:"next_tick"`
	QueuedNotes map[string][]string `json:"queued_notes"`
}

type SnapshotStatus struct {
	GeneratedAt *time.Time          `json:"generated_at"`
	LastTick    *time.Time          `json:"last_tick"`
	NextTick    *time.Time          `json:"next_tick"`
	QueuedNotes map[string][]string `json:"queued_notes"`
}

type TokenAggregate struct {
	Runs             int     `json:"runs"`
	FailedRuns       int     `json:"failed_runs"`
	BackfilledRuns   int     `json:"backfilled_runs"`
	Total            int64   `json:"total"`
	Fresh            int64   `json:"fresh"`
	CostUSD          float64 `json:"cost_usd"`
	CostMeasuredRuns int     `json:"cost_measured_runs"`
	Seconds          int64   `json:"seconds"`
}

type CompanyTokens struct {
	TokenAggregate
	LiveRuns          int   `json:"live_runs"`
	UnattributedRuns  int   `json:"unattributed_runs"`
	UnattributedTotal int64 `json:"unattributed_total"`
	UnattributedFresh int64 `json:"unattributed_fresh"`
}

type VentureTokens struct {
	Slug string `json:"slug"`
	TokenAggregate
}

type RoleTokens struct {
	Role string `json:"role"`
	TokenAggregate
}

type TokenBlock struct {
	LedgerVersion int             `json:"ledger_version"`
	Records       int             `json:"records"`
	FirstTS       *time.Time      `json:"first_ts"`
	LastTS        *time.Time      `json:"last_ts"`
	Company       *CompanyTokens  `json:"company"`
	ByVenture     []VentureTokens `json:"by_venture"`
	ByRole        []RoleTokens    `json:"by_role"`
}

type ApplyResult struct {
	Status       string   `json:"status"`
	Applied      int      `json:"applied,omitempty"`
	SkippedStale int      `json:"skipped_stale,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
	TokenRecords int      `json:"token_records"`
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
	if snapshot.Tokens == nil {
		result.Warnings = append(result.Warnings, "tokens: absent from payload")
	} else {
		result.TokenRecords = snapshot.Tokens.Records
	}
	for _, venture := range valid {
		pending := snapshot.QueuedNotes[venture.Slug]
		if pending == nil {
			pending = []string{}
		}
		notes, err := json.Marshal(pending)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("encode queued notes: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO ventures (
				slug, title, field, status, stage, kind, description, monetization,
				abandoned_reason, abandoned_at_stage, url, repo_url, design_url,
				created_at, last_seen_at, missing_since, snapshot_generated_at, queued_notes
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
				$14, now(), NULL, $15, $16
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
				design_url = EXCLUDED.design_url,
				created_at = EXCLUDED.created_at,
				last_seen_at = now(),
				missing_since = NULL,
				snapshot_generated_at = EXCLUDED.snapshot_generated_at,
				queued_notes = EXCLUDED.queued_notes
			WHERE EXCLUDED.snapshot_generated_at >= ventures.snapshot_generated_at`,
			venture.Slug, venture.Title, venture.Field, venture.Status, venture.Stage,
			venture.Kind, venture.Description, venture.Monetization, venture.AbandonedReason,
			venture.AbandonedAtStage, venture.URL, venture.RepoURL, venture.DesignURL,
			venture.CreatedAt, snapshot.GeneratedAt, notes,
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

	if snapshot.Tokens != nil {
		if snapshot.Tokens.Records == 0 || snapshot.Tokens.Company == nil {
			result.Warnings = append(result.Warnings, "tokens: empty block ignored")
		} else {
			var latest time.Time
			err := tx.QueryRow(ctx, "SELECT snapshot_generated_at FROM company_tokens WHERE id = true").Scan(&latest)
			if err != nil && err != pgx.ErrNoRows {
				return ApplyResult{}, fmt.Errorf("read latest tokens: %w", err)
			}
			if err == nil && snapshot.GeneratedAt.Before(latest) {
				result.SkippedStale++
			} else {
				if !tokenIdentityMatches(*snapshot.Tokens.Company, snapshot.Tokens.ByVenture) {
					result.Warnings = append(result.Warnings, "tokens: company total != attributed + unattributed")
				}
				if snapshot.Tokens.LedgerVersion > 1 {
					result.Warnings = append(result.Warnings, fmt.Sprintf("tokens: unsupported ledger version %d", snapshot.Tokens.LedgerVersion))
				}
				if err := replaceTokens(ctx, tx, *snapshot.Tokens, snapshot.GeneratedAt); err != nil {
					return ApplyResult{}, err
				}
			}
		}
	}

	warningsJSON, err := json.Marshal(result.Warnings)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("encode warnings: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ingest_snapshots (
			 snapshot_id, generated_at, venture_count, applied, skipped_stale, warnings, source, token_records, last_tick, next_tick
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		snapshot.SnapshotID, snapshot.GeneratedAt, len(snapshot.Ventures), result.Applied,
		result.SkippedStale, warningsJSON, snapshot.Source, result.TokenRecords, snapshot.LastTick, snapshot.NextTick,
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

func (s *Store) SnapshotStatus(ctx context.Context) (SnapshotStatus, error) {
	status := SnapshotStatus{QueuedNotes: map[string][]string{}}
	err := s.pool.QueryRow(ctx, `
		SELECT generated_at, last_tick, next_tick FROM ingest_snapshots
		ORDER BY generated_at DESC NULLS LAST, id DESC LIMIT 1`,
	).Scan(&status.GeneratedAt, &status.LastTick, &status.NextTick)
	if err != nil && err != pgx.ErrNoRows {
		return SnapshotStatus{}, fmt.Errorf("read snapshot status: %w", err)
	}
	rows, err := s.pool.Query(ctx, "SELECT slug, queued_notes FROM ventures WHERE hidden = false AND missing_since IS NULL")
	if err != nil {
		return SnapshotStatus{}, fmt.Errorf("read queued notes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var notes []byte
		if err := rows.Scan(&slug, &notes); err != nil {
			return SnapshotStatus{}, fmt.Errorf("scan queued notes: %w", err)
		}
		var values []string
		if err := json.Unmarshal(notes, &values); err != nil {
			return SnapshotStatus{}, fmt.Errorf("decode queued notes for %q: %w", slug, err)
		}
		if len(values) > 0 {
			status.QueuedNotes[slug] = values
		}
	}
	return status, rows.Err()
}

func tokenIdentityMatches(company CompanyTokens, ventures []VentureTokens) bool {
	var runs int
	var total, fresh int64
	for _, venture := range ventures {
		runs += venture.Runs
		total += venture.Total
		fresh += venture.Fresh
	}
	return company.Runs == runs+company.UnattributedRuns &&
		company.Total == total+company.UnattributedTotal &&
		company.Fresh == fresh+company.UnattributedFresh
}

func replaceTokens(ctx context.Context, tx pgx.Tx, block TokenBlock, generatedAt time.Time) error {
	for _, table := range []string{"venture_tokens", "role_tokens", "company_tokens"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	for _, venture := range block.ByVenture {
		if _, err := tx.Exec(ctx, `
			INSERT INTO venture_tokens (slug, runs, failed_runs, backfilled_runs, total, fresh, cost_usd, cost_measured_runs, seconds)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			venture.Slug, venture.Runs, venture.FailedRuns, venture.BackfilledRuns, venture.Total,
			venture.Fresh, venture.CostUSD, venture.CostMeasuredRuns, venture.Seconds,
		); err != nil {
			return fmt.Errorf("insert venture tokens %q: %w", venture.Slug, err)
		}
	}
	for _, role := range block.ByRole {
		if _, err := tx.Exec(ctx, `
			INSERT INTO role_tokens (role, runs, failed_runs, backfilled_runs, total, fresh, cost_usd, cost_measured_runs, seconds)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			role.Role, role.Runs, role.FailedRuns, role.BackfilledRuns, role.Total,
			role.Fresh, role.CostUSD, role.CostMeasuredRuns, role.Seconds,
		); err != nil {
			return fmt.Errorf("insert role tokens %q: %w", role.Role, err)
		}
	}
	company := block.Company
	if _, err := tx.Exec(ctx, `
		INSERT INTO company_tokens (
			id, records, first_ts, last_ts, runs, failed_runs, backfilled_runs, live_runs,
			total, fresh, unattributed_runs, unattributed_total, unattributed_fresh,
			cost_usd, cost_measured_runs, seconds, snapshot_generated_at
		) VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		block.Records, block.FirstTS, block.LastTS, company.Runs, company.FailedRuns,
		company.BackfilledRuns, company.LiveRuns, company.Total, company.Fresh,
		company.UnattributedRuns, company.UnattributedTotal, company.UnattributedFresh,
		company.CostUSD, company.CostMeasuredRuns, company.Seconds, generatedAt,
	); err != nil {
		return fmt.Errorf("insert company tokens: %w", err)
	}
	return nil
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
	DesignURL        string
	CreatedAt        *time.Time
	Tokens           *TokenAggregate
}

func (s *Store) ListVentures(ctx context.Context) ([]DashboardVenture, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.slug, v.title, v.field, v.status, v.stage, v.kind, v.description, v.monetization,
		       v.abandoned_reason, v.abandoned_at_stage, v.url, v.repo_url, v.design_url, v.created_at,
		       t.runs, t.failed_runs, t.backfilled_runs, t.total, t.fresh,
		       t.cost_usd, t.cost_measured_runs, t.seconds
		FROM ventures v
		LEFT JOIN venture_tokens t ON v.slug = t.slug
		WHERE v.hidden = false
		ORDER BY CASE v.status
			WHEN 'in_progress' THEN 1
			WHEN 'deployed' THEN 2
			WHEN 'abandoned' THEN 3
			ELSE 4
		END, v.created_at DESC NULLS LAST, v.slug`)
	if err != nil {
		return nil, fmt.Errorf("query ventures: %w", err)
	}
	defer rows.Close()

	var ventures []DashboardVenture
	for rows.Next() {
		var venture DashboardVenture
		var stage, kind, description, monetization sql.NullString
		var reason, reasonStage, liveURL, repoURL, designURL sql.NullString
		var created sql.NullTime
		var runs, failedRuns, backfilledRuns, total, fresh sql.NullInt64
		var costUSD sql.NullFloat64
		var costMeasuredRuns, seconds sql.NullInt64
		if err := rows.Scan(
			&venture.Slug, &venture.Title, &venture.Field, &venture.Status,
			&stage, &kind, &description, &monetization, &reason, &reasonStage,
			&liveURL, &repoURL, &designURL, &created,
			&runs, &failedRuns, &backfilledRuns, &total, &fresh, &costUSD,
			&costMeasuredRuns, &seconds,
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
		venture.DesignURL = designURL.String
		if created.Valid {
			venture.CreatedAt = &created.Time
		}
		if runs.Valid {
			venture.Tokens = &TokenAggregate{
				Runs:             int(runs.Int64),
				FailedRuns:       int(failedRuns.Int64),
				BackfilledRuns:   int(backfilledRuns.Int64),
				Total:            total.Int64,
				Fresh:            fresh.Int64,
				CostUSD:          costUSD.Float64,
				CostMeasuredRuns: int(costMeasuredRuns.Int64),
				Seconds:          seconds.Int64,
			}
		}
		ventures = append(ventures, venture)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ventures: %w", err)
	}
	return ventures, nil
}

type DashboardSpend struct {
	Company   *CompanyTokens
	FirstTS   *time.Time
	LastTS    *time.Time
	Records   int
	Roles     []RoleTokens
	Unmatched []VentureTokens
}

func (s *Store) DashboardSpend(ctx context.Context) (DashboardSpend, error) {
	spend := DashboardSpend{Company: &CompanyTokens{}}
	var firstTS, lastTS sql.NullTime
	err := s.pool.QueryRow(ctx, `
		SELECT records, first_ts, last_ts, runs, failed_runs, backfilled_runs, live_runs,
		       total, fresh, unattributed_runs, unattributed_total, unattributed_fresh,
		       cost_usd, cost_measured_runs, seconds
		FROM company_tokens WHERE id = true`).Scan(
		&spend.Records, &firstTS, &lastTS,
		&spend.Company.Runs, &spend.Company.FailedRuns, &spend.Company.BackfilledRuns,
		&spend.Company.LiveRuns, &spend.Company.Total, &spend.Company.Fresh,
		&spend.Company.UnattributedRuns, &spend.Company.UnattributedTotal,
		&spend.Company.UnattributedFresh, &spend.Company.CostUSD,
		&spend.Company.CostMeasuredRuns, &spend.Company.Seconds,
	)
	if err == pgx.ErrNoRows {
		spend.Company = nil
		return spend, nil
	}
	if err != nil {
		return DashboardSpend{}, fmt.Errorf("query company tokens: %w", err)
	}
	if firstTS.Valid {
		spend.FirstTS = &firstTS.Time
	}
	if lastTS.Valid {
		spend.LastTS = &lastTS.Time
	}

	roleRows, err := s.pool.Query(ctx, `
		SELECT role, runs, failed_runs, backfilled_runs, total, fresh,
		       cost_usd, cost_measured_runs, seconds
		FROM role_tokens
		ORDER BY fresh DESC, role`)
	if err != nil {
		return DashboardSpend{}, fmt.Errorf("query role tokens: %w", err)
	}
	defer roleRows.Close()
	for roleRows.Next() {
		var role RoleTokens
		if err := roleRows.Scan(
			&role.Role, &role.Runs, &role.FailedRuns, &role.BackfilledRuns,
			&role.Total, &role.Fresh, &role.CostUSD, &role.CostMeasuredRuns, &role.Seconds,
		); err != nil {
			return DashboardSpend{}, fmt.Errorf("scan role tokens: %w", err)
		}
		spend.Roles = append(spend.Roles, role)
	}
	if err := roleRows.Err(); err != nil {
		return DashboardSpend{}, fmt.Errorf("iterate role tokens: %w", err)
	}

	// Hidden ventures remain included in the laptop-computed company aggregate.
	// They are intentionally neither visible cards nor unmatched bookkeeping rows.
	unmatchedRows, err := s.pool.Query(ctx, `
		SELECT t.slug, t.runs, t.failed_runs, t.backfilled_runs, t.total, t.fresh,
		       t.cost_usd, t.cost_measured_runs, t.seconds
		FROM venture_tokens t
		LEFT JOIN ventures v ON v.slug = t.slug
		WHERE v.slug IS NULL
		ORDER BY t.fresh DESC, t.slug`)
	if err != nil {
		return DashboardSpend{}, fmt.Errorf("query unmatched venture tokens: %w", err)
	}
	defer unmatchedRows.Close()
	for unmatchedRows.Next() {
		var venture VentureTokens
		if err := unmatchedRows.Scan(
			&venture.Slug, &venture.Runs, &venture.FailedRuns, &venture.BackfilledRuns,
			&venture.Total, &venture.Fresh, &venture.CostUSD, &venture.CostMeasuredRuns, &venture.Seconds,
		); err != nil {
			return DashboardSpend{}, fmt.Errorf("scan unmatched venture tokens: %w", err)
		}
		spend.Unmatched = append(spend.Unmatched, venture)
	}
	if err := unmatchedRows.Err(); err != nil {
		return DashboardSpend{}, fmt.Errorf("iterate unmatched venture tokens: %w", err)
	}
	return spend, nil
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

// LiveRun is one agent invocation currently in flight on the laptop. The
// laptop owns the interpretation; the server only mirrors and serves it.
type LiveRun struct {
	RunID     string     `json:"run_id"`
	Slug      string     `json:"slug"`
	Role      string     `json:"role"`
	Leg       string     `json:"leg,omitempty"`
	Engine    string     `json:"engine,omitempty"`
	Model     string     `json:"model,omitempty"`
	Status    string     `json:"status"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	LogBytes  int64      `json:"log_bytes"`
	LogTail   string     `json:"log_tail,omitempty"`
}

// LivePayload is the full running-set snapshot pushed by the laptop.
type LivePayload struct {
	GeneratedAt *time.Time `json:"generated_at"`
	Cycle       int        `json:"cycle"`
	TickRunning bool       `json:"tick_running"`
	Runs        []LiveRun  `json:"runs"`
}

// LiveState is what the public page reads back. AgeSeconds is computed
// server-side from receive time so the client can dim a stale feed.
type LiveState struct {
	GeneratedAt *time.Time `json:"generated_at"`
	Cycle       int        `json:"cycle"`
	TickRunning bool       `json:"tick_running"`
	AgeSeconds  int        `json:"age_seconds"`
	Runs        []LiveRun  `json:"runs"`
}

// ApplyLive replaces the live run set with the payload. Exact mirror: the
// caller sends the complete set of currently running agents every time.
func (s *Store) ApplyLive(ctx context.Context, payload LivePayload) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin live tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize concurrent pushes so two overlapping mirrors cannot interleave
	// into a mixed set or race on a duplicate run_id.
	if _, err := tx.Exec(ctx, "LOCK TABLE live_runs, live_state IN EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock live tables: %w", err)
	}

	if _, err := tx.Exec(ctx, "DELETE FROM live_runs"); err != nil {
		return fmt.Errorf("clear live runs: %w", err)
	}
	for _, run := range payload.Runs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO live_runs
				(run_id, slug, role, leg, engine, model, status, started_at, log_bytes, log_tail, received_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())`,
			run.RunID, run.Slug, run.Role, run.Leg, run.Engine, run.Model,
			run.Status, run.StartedAt, run.LogBytes, run.LogTail,
		); err != nil {
			return fmt.Errorf("insert live run %q: %w", run.RunID, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO live_state (id, generated_at, cycle, tick_running, received_at)
		VALUES (true, $1, $2, $3, now())
		ON CONFLICT (id) DO UPDATE SET
			generated_at = EXCLUDED.generated_at,
			cycle = EXCLUDED.cycle,
			tick_running = EXCLUDED.tick_running,
			received_at = now()`,
		payload.GeneratedAt, payload.Cycle, payload.TickRunning,
	); err != nil {
		return fmt.Errorf("upsert live state: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit live tx: %w", err)
	}
	return nil
}

// Live reads back the current live state. A zero LiveState (empty runs) is
// returned when the laptop has never pushed anything yet.
func (s *Store) Live(ctx context.Context) (LiveState, error) {
	var state LiveState
	state.Runs = []LiveRun{}

	// Read the state row and the run set from one repeatable-read snapshot so a
	// push landing between the two queries cannot mix generations.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return LiveState{}, fmt.Errorf("begin live read: %w", err)
	}
	defer tx.Rollback(ctx)

	var received sql.NullTime
	err = tx.QueryRow(ctx,
		"SELECT generated_at, cycle, tick_running, received_at FROM live_state WHERE id = true",
	).Scan(&state.GeneratedAt, &state.Cycle, &state.TickRunning, &received)
	if err == pgx.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return LiveState{}, fmt.Errorf("query live state: %w", err)
	}
	if received.Valid {
		age := int(time.Since(received.Time).Seconds())
		if age < 0 {
			age = 0
		}
		state.AgeSeconds = age
	}

	rows, err := tx.Query(ctx, `
		SELECT run_id, slug, role, leg, engine, model, status, started_at, log_bytes, log_tail
		FROM live_runs
		ORDER BY started_at ASC NULLS LAST, slug, role`)
	if err != nil {
		return LiveState{}, fmt.Errorf("query live runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var run LiveRun
		if err := rows.Scan(
			&run.RunID, &run.Slug, &run.Role, &run.Leg, &run.Engine, &run.Model,
			&run.Status, &run.StartedAt, &run.LogBytes, &run.LogTail,
		); err != nil {
			return LiveState{}, fmt.Errorf("scan live run: %w", err)
		}
		state.Runs = append(state.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return LiveState{}, fmt.Errorf("iterate live runs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return LiveState{}, fmt.Errorf("commit live read: %w", err)
	}
	return state, nil
}
