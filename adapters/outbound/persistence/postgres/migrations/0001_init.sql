-- Initial schema. See ARCHITECTURE.md §7 and §6a (deviation 2: no
-- compute_instances table; deployments store the opaque instance ID).

CREATE TABLE model_catalog (
  id                  TEXT PRIMARY KEY,
  name                TEXT    NOT NULL,
  artifact            TEXT    NOT NULL,
  description         TEXT    NOT NULL DEFAULT '',
  family              TEXT    NOT NULL DEFAULT '',
  param_count         BIGINT  NOT NULL DEFAULT 0,
  min_vram_gb         INT     NOT NULL,
  recommended_vram_gb INT     NOT NULL DEFAULT 0,
  min_gpu_count       INT     NOT NULL DEFAULT 1,
  features            TEXT[]  NOT NULL DEFAULT '{}',
  temperature         DOUBLE PRECISION,
  top_p               DOUBLE PRECISION,
  max_tokens          INT     NOT NULL DEFAULT 0,
  runtime             JSONB   NOT NULL DEFAULT '{}',  -- opaque; interpreted by the runtime adapter
  status              TEXT    NOT NULL CHECK (status IN ('active', 'deactivated')),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
  id               TEXT PRIMARY KEY,
  model_id         TEXT NOT NULL REFERENCES model_catalog(id),
  instance_id      TEXT,
  status           TEXT NOT NULL,
  runtime_config   JSONB NOT NULL DEFAULT '{}',
  runtime_ref      TEXT  NOT NULL DEFAULT '',
  endpoint         JSONB,
  pinned           BOOLEAN NOT NULL DEFAULT FALSE,
  health           TEXT NOT NULL DEFAULT 'unknown',
  failure_stage    TEXT,
  failure_reason   TEXT,
  failed_at        TIMESTAMPTZ,
  idempotency_key  TEXT NOT NULL UNIQUE,
  version          BIGINT NOT NULL DEFAULT 1,   -- optimistic locking
  created_at       TIMESTAMPTZ NOT NULL,
  ready_at         TIMESTAMPTZ,
  last_activity_at TIMESTAMPTZ NOT NULL,
  updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX deployments_status_idx ON deployments (status);
CREATE INDEX deployments_model_idx  ON deployments (model_id, status);

CREATE TABLE usage_records (
  id             TEXT PRIMARY KEY,
  kind           TEXT NOT NULL CHECK (kind IN ('inference', 'uptime')),
  deployment_id  TEXT REFERENCES deployments(id) ON DELETE SET NULL,
  model_id       TEXT NOT NULL DEFAULT '',
  outcome        TEXT NOT NULL DEFAULT '',
  input_tokens   BIGINT NOT NULL DEFAULT 0,
  output_tokens  BIGINT NOT NULL DEFAULT 0,
  duration_ms    BIGINT NOT NULL DEFAULT 0,
  cost_micros    BIGINT,                         -- NULL = unknown, never zero
  cost_currency  TEXT,
  cost_basis     TEXT CHECK (cost_basis IN ('estimated', 'billed')),
  occurred_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX usage_time_idx ON usage_records (occurred_at);

CREATE TABLE preferences (
  subject_id       TEXT PRIMARY KEY,
  default_model_id TEXT REFERENCES model_catalog(id),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
