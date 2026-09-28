-- Typed transcript steps for each live run (message, thinking, tool call, error…),
-- built on the laptop from the engine's JSON event stream. log_tail stays for runs
-- whose log is plain text.

ALTER TABLE live_runs ADD COLUMN IF NOT EXISTS log_events json NOT NULL DEFAULT '[]'::json;
