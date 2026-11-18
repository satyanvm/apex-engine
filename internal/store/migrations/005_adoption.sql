-- Preview runs: every read happens, every write is recorded instead of sent.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS preview boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS runs_finished_at ON runs (finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS audit_log_at ON audit_log (at);
