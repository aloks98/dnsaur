-- +goose Up
-- Zones replace the flat local_records table. The difference is not
-- structural but semantic: local_records was a set of overrides on a
-- forwarder, so a name it didn't hold fell through upstream. A zone is a
-- claim of authority over a suffix, so a name it doesn't hold is an
-- authoritative NXDOMAIN carrying this zone's SOA — it never leaves.
--
-- That is what makes split-horizon possible. Without it, an undefined name
-- under a publicly-registered domain leaks to the upstream resolver and any
-- public record shadows the internal one that hasn't been written yet.
--
-- See docs/superpowers/specs/2026-08-08-zones-design.md.

CREATE TABLE zones (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  -- Apex, lowercase, no trailing dot: 'e412.in', '168.192.in-addr.arpa'.
  name TEXT NOT NULL UNIQUE,
  -- primary | secondary | stub | forwarder | internal
  type TEXT NOT NULL DEFAULT 'primary',
  enabled INTEGER NOT NULL DEFAULT 1,

  -- SOA lives here rather than in zone_records because its serial needs
  -- managed increments on every write, and exactly one SOA may exist per
  -- zone. Storing it as a record would make both of those conventions the
  -- application has to defend instead of things the schema guarantees.
  soa_ns TEXT NOT NULL DEFAULT '',
  soa_mbox TEXT NOT NULL DEFAULT '',
  soa_serial INTEGER NOT NULL DEFAULT 1,
  soa_refresh INTEGER NOT NULL DEFAULT 900,
  soa_retry INTEGER NOT NULL DEFAULT 300,
  soa_expire INTEGER NOT NULL DEFAULT 604800,
  -- MINIMUM is the negative-cache TTL a resolver applies to the NXDOMAINs
  -- this zone hands out, not a floor on positive answers (RFC 2308).
  soa_minimum INTEGER NOT NULL DEFAULT 900,

  -- secondary/stub/forwarder only; unused and empty for primary/internal.
  primaries TEXT NOT NULL DEFAULT '',
  tsig_key_id INTEGER NOT NULL DEFAULT 0,
  -- Unix ms. A secondary past expires_at must stop answering rather than
  -- serve data it can no longer confirm.
  expires_at INTEGER NOT NULL DEFAULT 0,
  refreshed_at INTEGER NOT NULL DEFAULT 0,

  created_at INTEGER NOT NULL DEFAULT 0,
  modified_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE zone_records (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  zone_id INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
  -- RELATIVE to the apex: '@' for the apex itself, 'bifrost', '*',
  -- '*.nexus'. Relative names are what make a zone portable — exporting it
  -- or renaming the apex touches one row instead of all of them.
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  ttl INTEGER NOT NULL DEFAULT 3600,
  -- Presentation format, rdata portion only: '192.168.150.28' for A,
  -- '10 mail.example.com.' for MX, '0 issue "letsencrypt.org"' for CAA.
  -- An RR is rebuilt by handing miekg/dns a reassembled master-file line,
  -- so every type it can parse works without a schema change, validation
  -- is the same code path as parsing, and zone-file export is string
  -- assembly rather than a second serializer that can disagree.
  rdata TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  comment TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_zone_records_lookup ON zone_records(zone_id, name, type);
