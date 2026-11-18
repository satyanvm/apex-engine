-- Resume, per-item records, OAuth, pieces, API keys, audit log and counters.

ALTER TABLE runs ADD COLUMN IF NOT EXISTS resume_of  text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS idem_root  text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS started_by text NOT NULL DEFAULT '';
ALTER TABLE node_runs ADD COLUMN IF NOT EXISTS reused boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS runs_unfinished ON runs (tenant, status) WHERE finished_at IS NULL;

-- One row per loop item per body node, so a loop of 200 leaves 200 records
-- instead of whatever the last item did.
CREATE TABLE IF NOT EXISTS node_item_runs (
  run_id      text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  key         text NOT NULL,
  idx         int  NOT NULL,
  status      text NOT NULL,
  started_at  timestamptz,
  finished_at timestamptz,
  output      jsonb,
  error       text,
  attempts    int NOT NULL DEFAULT 0,
  PRIMARY KEY (run_id, key, idx)
);

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'catalogs_kind_check_v2') THEN
    ALTER TABLE catalogs DROP CONSTRAINT IF EXISTS catalogs_kind_check;
    ALTER TABLE catalogs ADD CONSTRAINT catalogs_kind_check_v2 CHECK (kind IN ('openapi','mcp','piece'));
  END IF;
END $$;
ALTER TABLE catalogs ADD COLUMN IF NOT EXISTS auth_kind text NOT NULL DEFAULT '';
UPDATE catalogs SET auth_kind = 'headers' WHERE auth_kind = '' AND credentials IS NOT NULL;

-- A pending OAuth consent. The state value is the only credential the callback has.
CREATE TABLE IF NOT EXISTS oauth_states (
  state      text PRIMARY KEY,
  tenant     text NOT NULL,
  catalog_id text NOT NULL REFERENCES catalogs(id) ON DELETE CASCADE,
  verifier   text NOT NULL,
  expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  id           text PRIMARY KEY,
  tenant       text NOT NULL,
  name         text NOT NULL,
  role         text NOT NULL CHECK (role IN ('viewer','approver','editor','operator','admin')),
  hash         bytea NOT NULL UNIQUE,
  prefix       text NOT NULL,
  created_by   text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at   timestamptz,
  UNIQUE (tenant, name)
);

CREATE TABLE IF NOT EXISTS audit_log (
  id     bigserial PRIMARY KEY,
  tenant text NOT NULL,
  at     timestamptz NOT NULL DEFAULT now(),
  actor  text NOT NULL,
  action text NOT NULL,
  target text NOT NULL DEFAULT '',
  detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS audit_tenant_id ON audit_log (tenant, id DESC);

-- Monotonic counters behind /metrics. Kept in Postgres so the API and worker
-- processes add to the same numbers.
CREATE TABLE IF NOT EXISTS counters (
  name   text NOT NULL,
  tenant text NOT NULL,
  label  text NOT NULL DEFAULT '',
  value  bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (name, tenant, label)
);
