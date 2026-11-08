CREATE TABLE IF NOT EXISTS catalogs (
  id                 text PRIMARY KEY,
  tenant             text NOT NULL,
  name               text NOT NULL,
  slug               text NOT NULL,
  kind               text NOT NULL CHECK (kind IN ('openapi','mcp')),
  base_url           text NOT NULL,
  nodes              jsonb NOT NULL,
  credentials        bytea,
  credential_headers text[] NOT NULL DEFAULT '{}',
  created_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, slug)
);

CREATE TABLE IF NOT EXISTS runs (
  id              text PRIMARY KEY,
  tenant          text NOT NULL,
  status          text NOT NULL,
  dag             jsonb NOT NULL,
  input           jsonb NOT NULL,
  catalog_ids     text[] NOT NULL DEFAULT '{}',
  workflow_id     text NOT NULL,
  idempotency_key text,
  error           text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  finished_at     timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS runs_idempotency ON runs (tenant, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS runs_tenant_created ON runs (tenant, created_at DESC);

CREATE TABLE IF NOT EXISTS node_runs (
  run_id      text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  key         text NOT NULL,
  position    int  NOT NULL,
  type        text NOT NULL,
  name        text NOT NULL,
  status      text NOT NULL,
  started_at  timestamptz,
  finished_at timestamptz,
  output      jsonb,
  error       text,
  attempts    int NOT NULL DEFAULT 0,
  PRIMARY KEY (run_id, key)
);

CREATE TABLE IF NOT EXISTS schedules (
  id         text PRIMARY KEY,
  tenant     text NOT NULL,
  cron       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
