-- Sealed ai-quota READ token, bound to an HQ session and deleted with it.
CREATE TABLE IF NOT EXISTS hq_quota_tokens (
    session_sha256 text PRIMARY KEY REFERENCES hq_sessions(id_sha256) ON DELETE CASCADE,
    sealed         bytea NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
