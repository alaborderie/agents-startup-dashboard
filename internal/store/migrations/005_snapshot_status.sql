ALTER TABLE ventures ADD COLUMN queued_notes jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE ingest_snapshots ADD COLUMN last_tick timestamptz;
ALTER TABLE ingest_snapshots ADD COLUMN next_tick timestamptz;
