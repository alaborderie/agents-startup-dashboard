-- Live run state pushed frequently by the laptop while a tick is executing.
-- Semantics are exact-mirror: every push replaces the whole live_runs table, so
-- a run disappears the moment the laptop stops reporting it.

CREATE TABLE IF NOT EXISTS live_state (
    id           boolean PRIMARY KEY DEFAULT true CHECK (id),
    generated_at timestamptz,
    cycle        integer NOT NULL DEFAULT 0,
    tick_running boolean NOT NULL DEFAULT false,
    received_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS live_runs (
    run_id      text PRIMARY KEY,
    slug        text NOT NULL,
    role        text NOT NULL,
    leg         text NOT NULL DEFAULT '',
    engine      text NOT NULL DEFAULT '',
    model       text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'running',
    started_at  timestamptz,
    log_bytes   bigint NOT NULL DEFAULT 0,
    log_tail    text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS live_runs_slug_idx ON live_runs (slug);
