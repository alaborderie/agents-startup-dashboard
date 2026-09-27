package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// HQState is the laptop's private HQ document as last received. Doc is stored
// and served verbatim: the laptop owns its shape, exactly like the snapshot.
type HQState struct {
	Doc         json.RawMessage
	GeneratedAt time.Time
	ReceivedAt  time.Time
}

// ApplyHQ replaces the HQ document unless the stored one is newer. It reports
// whether the document was stored.
func (s *Store) ApplyHQ(ctx context.Context, generatedAt time.Time, doc json.RawMessage) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO hq_state (id, doc, generated_at, received_at)
		VALUES (true, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET
			doc = EXCLUDED.doc,
			generated_at = EXCLUDED.generated_at,
			received_at = now()
		WHERE EXCLUDED.generated_at >= hq_state.generated_at`,
		doc, generatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("upsert hq state: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// HQ returns the stored HQ document, or nil when the laptop never pushed one.
func (s *Store) HQ(ctx context.Context) (*HQState, error) {
	var state HQState
	var doc []byte
	err := s.pool.QueryRow(ctx,
		"SELECT doc, generated_at, received_at FROM hq_state WHERE id = true",
	).Scan(&doc, &state.GeneratedAt, &state.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query hq state: %w", err)
	}
	state.Doc = doc
	return &state, nil
}

// CreateLoginLink records the hash of a one-time login token minted on the
// laptop, and prunes links nobody can use any more.
func (s *Store) CreateLoginLink(ctx context.Context, tokenSHA256 string, expiresAt time.Time) error {
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM hq_login_links WHERE expires_at < now() - interval '1 day'",
	); err != nil {
		return fmt.Errorf("prune login links: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO hq_login_links (token_sha256, expires_at) VALUES ($1, $2)
		ON CONFLICT (token_sha256) DO NOTHING`,
		tokenSHA256, expiresAt,
	); err != nil {
		return fmt.Errorf("insert login link: %w", err)
	}
	return nil
}

// ConsumeLoginLink marks an unexpired, unused link as used. Only one caller
// can ever win for a given token.
func (s *Store) ConsumeLoginLink(ctx context.Context, tokenSHA256 string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE hq_login_links SET used_at = now()
		WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > now()`,
		tokenSHA256,
	)
	if err != nil {
		return false, fmt.Errorf("consume login link: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) CreateSession(ctx context.Context, idSHA256 string, expiresAt time.Time, userAgent string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM hq_sessions WHERE expires_at < now()"); err != nil {
		return fmt.Errorf("prune sessions: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO hq_sessions (id_sha256, expires_at, user_agent) VALUES ($1, $2, $3)`,
		idSHA256, expiresAt, userAgent,
	); err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// ValidSession reports whether the session exists and has not expired, and
// records that it was seen.
func (s *Store) ValidSession(ctx context.Context, idSHA256 string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE hq_sessions SET last_seen_at = now()
		WHERE id_sha256 = $1 AND expires_at > now()`,
		idSHA256,
	)
	if err != nil {
		return false, fmt.Errorf("check session: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) DeleteSession(ctx context.Context, idSHA256 string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM hq_sessions WHERE id_sha256 = $1", idSHA256); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// Command is one founder action queued for the laptop.
type Command struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Slug        string     `json:"slug"`
	Gate        string     `json:"gate"`
	Text        string     `json:"text"`
	Status      string     `json:"status"`
	Message     string     `json:"message"`
	Attempts    int        `json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// NewCommand is a validated command about to be queued. ClientID is optional;
// a repeated one returns the command it first created.
type NewCommand struct {
	ID       string
	Kind     string
	Slug     string
	Gate     string
	Text     string
	ClientID string
}

const commandColumns = `id, kind, slug, gate, text, status, message, attempts,
	created_at, delivered_at, finished_at`

func scanCommand(row pgx.Row) (Command, error) {
	var command Command
	var delivered, finished sql.NullTime
	if err := row.Scan(
		&command.ID, &command.Kind, &command.Slug, &command.Gate, &command.Text,
		&command.Status, &command.Message, &command.Attempts, &command.CreatedAt,
		&delivered, &finished,
	); err != nil {
		return Command{}, err
	}
	if delivered.Valid {
		command.DeliveredAt = &delivered.Time
	}
	if finished.Valid {
		command.FinishedAt = &finished.Time
	}
	return command, nil
}

// CreateCommand queues a command. created is false when ClientID matched an
// existing command, which is then returned unchanged.
func (s *Store) CreateCommand(ctx context.Context, command NewCommand) (Command, bool, error) {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM hq_commands
		WHERE created_at < now() - interval '30 days' AND status IN ('done', 'failed')`,
	); err != nil {
		return Command{}, false, fmt.Errorf("prune commands: %w", err)
	}
	var clientID any
	if command.ClientID != "" {
		clientID = command.ClientID
	}
	created, err := scanCommand(s.pool.QueryRow(ctx, `
		INSERT INTO hq_commands (id, kind, slug, gate, text, client_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (client_id) DO NOTHING
		RETURNING `+commandColumns,
		command.ID, command.Kind, command.Slug, command.Gate, command.Text, clientID,
	))
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Command{}, false, fmt.Errorf("insert command: %w", err)
	}
	existing, err := scanCommand(s.pool.QueryRow(ctx,
		"SELECT "+commandColumns+" FROM hq_commands WHERE client_id = $1", command.ClientID,
	))
	if err != nil {
		return Command{}, false, fmt.Errorf("read existing command: %w", err)
	}
	return existing, false, nil
}

// ClaimCommands hands queued commands to the laptop, plus delivered ones whose
// result never arrived within redeliverAfter. The laptop dedupes by id, so a
// redelivery after a lost response is safe.
func (s *Store) ClaimCommands(ctx context.Context, redeliverAfter time.Duration, limit int) ([]Command, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE hq_commands SET status = 'delivered', delivered_at = now(), attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM hq_commands
			WHERE status = 'queued'
			   OR (status = 'delivered' AND delivered_at < now() - make_interval(secs => $1))
			ORDER BY created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+commandColumns,
		redeliverAfter.Seconds(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim commands: %w", err)
	}
	defer rows.Close()
	commands := []Command{}
	for rows.Next() {
		command, err := scanCommand(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed command: %w", err)
		}
		commands = append(commands, command)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed commands: %w", err)
	}
	sort.SliceStable(commands, func(i, j int) bool {
		return commands[i].CreatedAt.Before(commands[j].CreatedAt)
	})
	return commands, nil
}

// FinishCommand records the laptop's result. A finished command is final: a
// second result leaves it unchanged, and changed reports whether this call won.
func (s *Store) FinishCommand(ctx context.Context, id, status, message string) (command Command, found, changed bool, err error) {
	command, err = scanCommand(s.pool.QueryRow(ctx, `
		UPDATE hq_commands SET status = $2, message = $3, finished_at = now()
		WHERE id = $1 AND status IN ('queued', 'delivered')
		RETURNING `+commandColumns,
		id, status, message,
	))
	if err == nil {
		return command, true, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Command{}, false, false, fmt.Errorf("finish command: %w", err)
	}
	command, err = scanCommand(s.pool.QueryRow(ctx,
		"SELECT "+commandColumns+" FROM hq_commands WHERE id = $1", id,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Command{}, false, false, nil
	}
	if err != nil {
		return Command{}, false, false, fmt.Errorf("read command: %w", err)
	}
	return command, true, false, nil
}

// RecentCommands returns the newest commands first.
func (s *Store) RecentCommands(ctx context.Context, limit int) ([]Command, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+commandColumns+" FROM hq_commands ORDER BY created_at DESC, id LIMIT $1", limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query commands: %w", err)
	}
	defer rows.Close()
	commands := []Command{}
	for rows.Next() {
		command, err := scanCommand(rows)
		if err != nil {
			return nil, fmt.Errorf("scan command: %w", err)
		}
		commands = append(commands, command)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate commands: %w", err)
	}
	return commands, nil
}
