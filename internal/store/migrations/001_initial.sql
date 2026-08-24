CREATE TABLE IF NOT EXISTS ventures (
    slug text PRIMARY KEY,
    title text NOT NULL,
    field text NOT NULL,
    status text NOT NULL,
    stage text,
    kind text,
    description text,
    monetization text,
    abandoned_reason text,
    abandoned_at_stage text,
    url text,
    repo_url text,
    created_at timestamptz,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL,
    missing_since timestamptz,
    snapshot_generated_at timestamptz NOT NULL,
    hidden boolean NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS ingest_snapshots (
    id bigserial,
    snapshot_id text,
    generated_at timestamptz,
    received_at timestamptz DEFAULT now(),
    venture_count int,
    applied int,
    skipped_stale int,
    warnings jsonb,
    source text
);
