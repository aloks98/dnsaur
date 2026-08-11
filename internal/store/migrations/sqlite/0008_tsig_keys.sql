-- +goose Up
-- A TSIG key authenticates a zone transfer (RFC 8945). Unlike an API token,
-- which is stored as a sha256 hash because it only ever needs comparing, a
-- TSIG secret must be recoverable: the server signs with it. That follows the
-- TOTP precedent in users, not the auth_tokens one -- and it means this table
-- is credential material in the clear.
CREATE TABLE tsig_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  -- A domain name, canonical form (lowercase, trailing dot): it arrives as
  -- the owner name of the TSIG RR on a signed message, and that is how the
  -- provider looks it up.
  name TEXT NOT NULL UNIQUE,
  -- 'hmac-sha256.' etc -- miekg's own constants, trailing dot included.
  algorithm TEXT NOT NULL,
  -- base64, as it is written in every other DNS server's config, so a key can
  -- be pasted between them.
  secret TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT 0
);
