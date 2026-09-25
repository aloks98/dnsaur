-- +goose Up
-- DHCP round two: client classes, and a list of pools per scope in place of
-- the one pool_start/pool_end pair. See
-- docs/superpowers/specs/2026-09-25-dhcp-classes-design.md sections 2 and 3.

CREATE TABLE dhcp_classes (
  id BIGSERIAL PRIMARY KEY,
  -- Unique case-insensitively: "IoT" and "iot" would render as two engine
  -- classes nobody can tell apart. ValidateClass says so first; the
  -- dhcp_classes_name index below is what holds under a race.
  name TEXT NOT NULL,
  -- JSON list of matchers: "vendor:<prefix>" (option 60 starts with) or
  -- "mac:<hex>" (hardware address starts with 1-6 bytes). Any matches.
  matchers TEXT NOT NULL DEFAULT '[]',
  -- The scope's client options, same meaning and same encoding as the
  -- dhcp_scopes columns of the same names. Blank means the scope's value.
  dns_servers TEXT NOT NULL DEFAULT '',
  domain TEXT NOT NULL DEFAULT '',
  domain_search TEXT NOT NULL DEFAULT '',
  ntp_servers TEXT NOT NULL DEFAULT '',
  static_routes TEXT NOT NULL DEFAULT '[]',
  next_server TEXT NOT NULL DEFAULT '',
  server_hostname TEXT NOT NULL DEFAULT '',
  boot_file TEXT NOT NULL DEFAULT '',
  options TEXT NOT NULL DEFAULT '[]',
  created_at BIGINT NOT NULL,
  modified_at BIGINT NOT NULL
);

CREATE UNIQUE INDEX dhcp_classes_name ON dhcp_classes (lower(name));

CREATE TABLE dhcp_pools (
  id BIGSERIAL PRIMARY KEY,
  -- No ON DELETE CASCADE, for 0016's reason: DeleteScope removes a scope's
  -- pools itself, and the bundle's prune goes children before parents.
  scope_id BIGINT NOT NULL REFERENCES dhcp_scopes(id),
  -- The pool's place in the scope's list: the order the dialog shows and
  -- the renderer writes. Rewritten whole on every scope write.
  position INTEGER NOT NULL,
  start TEXT NOT NULL,
  -- Quoted everywhere: END is a keyword in both dialects.
  "end" TEXT NOT NULL,
  -- 0 = any client. Not a foreign key, since 0 names no row: DeleteClass
  -- refuses while a pool names the class, and the bundle prunes pools
  -- before classes and refuses a pool naming a class it does not carry.
  class_id BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX dhcp_pools_scope ON dhcp_pools (scope_id, position);

-- Each scope's one pool becomes pool 0. A reservations_only scope may have
-- held none (both columns empty), and it keeps none: an empty range is not
-- a pool.
INSERT INTO dhcp_pools (scope_id, position, start, "end", class_id)
  SELECT id, 0, pool_start, pool_end, 0 FROM dhcp_scopes WHERE pool_start <> '';
ALTER TABLE dhcp_scopes DROP COLUMN pool_start;
ALTER TABLE dhcp_scopes DROP COLUMN pool_end;
