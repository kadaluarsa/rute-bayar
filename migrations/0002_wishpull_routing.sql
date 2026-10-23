-- Wishpull v0.3: per-order execution lock/idempotency and the outbound
-- event outbox. Statements must stay idempotent: migrations run on every start.

CREATE TABLE IF NOT EXISTS route_executions (
  order_id TEXT PRIMARY KEY,
  token_id TEXT NOT NULL,
  environment TEXT NOT NULL,
  method TEXT NOT NULL,
  amount INTEGER NOT NULL,
  currency TEXT NOT NULL,
  status TEXT NOT NULL,
  provider_code TEXT NOT NULL DEFAULT '',
  result_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS outbound_events (
  id TEXT PRIMARY KEY,
  event_type TEXT NOT NULL,
  order_id TEXT NOT NULL DEFAULT '',
  provider_code TEXT NOT NULL DEFAULT '',
  webhook_event_id TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL,
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  delivered_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbound_events_due ON outbound_events(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_outbound_events_order ON outbound_events(order_id);
