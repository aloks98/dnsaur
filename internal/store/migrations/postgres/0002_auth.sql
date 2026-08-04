-- +goose Up
CREATE TABLE users (
  id BIGSERIAL PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  totp_secret TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL
);
CREATE TABLE auth_tokens (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id),
  kind TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL UNIQUE,
  scope TEXT NOT NULL DEFAULT 'write',
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL DEFAULT 0,
  last_used BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX idx_auth_tokens_user ON auth_tokens(user_id);
