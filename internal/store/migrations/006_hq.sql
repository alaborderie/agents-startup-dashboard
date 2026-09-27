-- HQ control room: the founder's private, authenticated view.
--
-- hq_state mirrors one JSON document the laptop pushes (gates, blocked
-- questions, artifacts, activity). Like live_state it is a projection: the
-- laptop owns interpretation and the next push repairs any loss. The column is
-- json, not jsonb: the document is served back verbatim and never queried, and
-- jsonb rejects valid JSON such as an escaped NUL in an artifact or log line.
CREATE TABLE IF NOT EXISTS hq_state (
    id           boolean PRIMARY KEY DEFAULT true CHECK (id),
    doc          json NOT NULL,
    generated_at timestamptz NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now()
);

-- One-time magic links minted on the laptop (Telegram /web). Only the SHA-256
-- of the token is stored; the token itself travels in the URL fragment.
CREATE TABLE IF NOT EXISTS hq_login_links (
    token_sha256 text PRIMARY KEY,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    used_at      timestamptz
);

-- Browser sessions. The cookie carries a random id; only its SHA-256 is stored.
CREATE TABLE IF NOT EXISTS hq_sessions (
    id_sha256    text PRIMARY KEY,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent   text NOT NULL DEFAULT ''
);

-- Founder actions queued for the laptop. The server can never reach the
-- laptop, so hq-bridge long-polls for these and reports each result back.
CREATE TABLE IF NOT EXISTS hq_commands (
    id           text PRIMARY KEY,
    kind         text NOT NULL,
    slug         text NOT NULL DEFAULT '',
    gate         text NOT NULL DEFAULT '',
    text         text NOT NULL DEFAULT '',
    client_id    text UNIQUE,
    status       text NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'delivered', 'done', 'failed')),
    message      text NOT NULL DEFAULT '',
    attempts     integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    finished_at  timestamptz
);

CREATE INDEX IF NOT EXISTS hq_commands_pending_idx ON hq_commands (status, created_at);
CREATE INDEX IF NOT EXISTS hq_commands_created_idx ON hq_commands (created_at DESC);
