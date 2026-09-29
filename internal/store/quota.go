package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Store) PutQuotaToken(ctx context.Context, sessionSHA string, sealed []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO hq_quota_tokens (session_sha256, sealed) VALUES ($1, $2)
		ON CONFLICT (session_sha256) DO UPDATE SET sealed = EXCLUDED.sealed`, sessionSHA, sealed)
	if err != nil {
		return fmt.Errorf("upsert quota token: %w", err)
	}
	return nil
}

func (s *Store) QuotaToken(ctx context.Context, sessionSHA string) ([]byte, bool, error) {
	var sealed []byte
	err := s.pool.QueryRow(ctx, "SELECT sealed FROM hq_quota_tokens WHERE session_sha256 = $1", sessionSHA).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read quota token: %w", err)
	}
	return sealed, true, nil
}

func (s *Store) DeleteQuotaToken(ctx context.Context, sessionSHA string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM hq_quota_tokens WHERE session_sha256 = $1", sessionSHA)
	if err != nil {
		return fmt.Errorf("delete quota token: %w", err)
	}
	return nil
}
