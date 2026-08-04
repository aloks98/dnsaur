-- +goose Up
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE config_version (id BIGINT PRIMARY KEY CHECK (id = 1), version BIGINT NOT NULL);
INSERT INTO config_version (id, version) VALUES (1, 1);
CREATE TABLE groups (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  enabled BOOLEAN NOT NULL DEFAULT true
);
CREATE TABLE clients (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  matcher TEXT NOT NULL UNIQUE,
  group_id BIGINT NOT NULL REFERENCES groups(id)
);
CREATE TABLE lists (
  id BIGSERIAL PRIMARY KEY,
  url TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL DEFAULT 'block',
  enabled BOOLEAN NOT NULL DEFAULT true,
  last_refreshed BIGINT NOT NULL DEFAULT 0,
  entry_count BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE group_lists (
  group_id BIGINT NOT NULL REFERENCES groups(id),
  list_id BIGINT NOT NULL REFERENCES lists(id),
  PRIMARY KEY (group_id, list_id)
);
CREATE TABLE rules (
  id BIGSERIAL PRIMARY KEY,
  group_id BIGINT NOT NULL REFERENCES groups(id),
  action TEXT NOT NULL,
  pattern TEXT NOT NULL,
  is_regex BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE local_records (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  value TEXT NOT NULL,
  ttl BIGINT NOT NULL DEFAULT 300
);
CREATE TABLE query_log (
  id BIGSERIAL PRIMARY KEY,
  at BIGINT NOT NULL,
  instance_id TEXT NOT NULL,
  client_ip TEXT NOT NULL,
  client_id BIGINT NOT NULL DEFAULT 0,
  qname TEXT NOT NULL,
  qtype TEXT NOT NULL,
  decision TEXT NOT NULL,
  rule_id BIGINT NOT NULL DEFAULT 0,
  list_id BIGINT NOT NULL DEFAULT 0,
  upstream TEXT NOT NULL DEFAULT '',
  rcode TEXT NOT NULL,
  duration_ms BIGINT NOT NULL
);
CREATE INDEX idx_qlog_at ON query_log(at);
CREATE INDEX idx_qlog_qname ON query_log(qname);
CREATE TABLE stats_hourly (
  bucket BIGINT NOT NULL,
  metric TEXT NOT NULL,
  key TEXT NOT NULL,
  value BIGINT NOT NULL,
  PRIMARY KEY (bucket, metric, key)
);
