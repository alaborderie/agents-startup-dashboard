-- Token aggregates can name a venture which never appears in ventures. Do not add
-- a foreign key: a bookkeeping mismatch must not reject a whole snapshot.
CREATE TABLE IF NOT EXISTS venture_tokens (
    slug text PRIMARY KEY,
    runs int NOT NULL,
    failed_runs int NOT NULL,
    backfilled_runs int NOT NULL,
    total bigint NOT NULL,
    fresh bigint NOT NULL,
    cost_usd numeric(12,6) NOT NULL,
    cost_measured_runs int NOT NULL,
    seconds bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS role_tokens (
    role text PRIMARY KEY,
    runs int NOT NULL,
    failed_runs int NOT NULL,
    backfilled_runs int NOT NULL,
    total bigint NOT NULL,
    fresh bigint NOT NULL,
    cost_usd numeric(12,6) NOT NULL,
    cost_measured_runs int NOT NULL,
    seconds bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS company_tokens (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    records int NOT NULL,
    first_ts timestamptz,
    last_ts timestamptz,
    runs int NOT NULL,
    failed_runs int NOT NULL,
    backfilled_runs int NOT NULL,
    live_runs int NOT NULL,
    total bigint NOT NULL,
    fresh bigint NOT NULL,
    unattributed_runs int NOT NULL,
    unattributed_total bigint NOT NULL,
    unattributed_fresh bigint NOT NULL,
    cost_usd numeric(12,6) NOT NULL,
    cost_measured_runs int NOT NULL,
    seconds bigint NOT NULL,
    snapshot_generated_at timestamptz NOT NULL
);

ALTER TABLE ingest_snapshots ADD COLUMN IF NOT EXISTS token_records int;
