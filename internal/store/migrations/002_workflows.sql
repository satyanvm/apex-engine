CREATE TABLE IF NOT EXISTS workflows (
  id            text PRIMARY KEY,
  tenant        text NOT NULL,
  name          text NOT NULL,
  description   text NOT NULL DEFAULT '',
  prompt        text NOT NULL DEFAULT '',
  nodes         jsonb NOT NULL,
  default_input jsonb NOT NULL DEFAULT '{}',
  catalog_ids   text[] NOT NULL DEFAULT '{}',
  webhook_token text UNIQUE,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workflows_tenant_updated ON workflows (tenant, updated_at DESC);

ALTER TABLE runs ADD COLUMN IF NOT EXISTS name text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS workflow_ref text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'api';
CREATE INDEX IF NOT EXISTS runs_tenant_workflow ON runs (tenant, workflow_ref, created_at DESC) WHERE workflow_ref IS NOT NULL;

ALTER TABLE schedules ADD COLUMN IF NOT EXISTS workflow_ref text;
