-- +goose Up
-- DHCP scopes and reservations. dnsaur is the control plane and ISC Kea is
-- the engine: these two tables are the whole of what an operator edits, and
-- the engine's config is rendered from them. Kea's own lease database is not
-- here — it is the engine's state, read back rather than written.
--
-- Both tables travel in a config bundle under the main's ids, so a
-- reservation means the same row on both boxes, exactly as rules and zones do.
-- See docs/superpowers/specs/2026-09-13-dhcp-design.md sections 4.3 and 4.4.

CREATE TABLE dhcp_scopes (
  id BIGSERIAL PRIMARY KEY,
  -- The operator's label for the subnet, and what the UI lists it by.
  name TEXT NOT NULL UNIQUE,
  -- IPv4 CIDR, e.g. '192.168.1.0/24'. No two *enabled* scopes may overlap;
  -- that rule is store.ValidateScope's, not the schema's, because it is a
  -- claim about a set of rows rather than about one.
  cidr TEXT NOT NULL,
  -- The dynamic range Kea hands out, inside cidr and excluding the network
  -- and broadcast addresses. A reservation may sit inside or outside it —
  -- Kea keeps reserved addresses out of dynamic allocation either way.
  pool_start TEXT NOT NULL,
  pool_end TEXT NOT NULL,
  -- Router option; empty means the scope hands out no router at all, which
  -- is a legal configuration for an isolated segment.
  gateway TEXT NOT NULL DEFAULT '',
  -- Comma-separated addresses, or empty for the automatic answer of section
  -- 5.3 (this box, and its peer when one is paired).
  dns_servers TEXT NOT NULL DEFAULT '',
  -- Domain suffix handed to clients; empty falls back to the dhcp.domain
  -- setting, so the common case is configured once rather than per scope.
  domain TEXT NOT NULL DEFAULT '',
  -- 0 falls back to the dhcp.lease_seconds setting, for the same reason
  -- domain does. A non-zero value is at least 300.
  lease_seconds INTEGER NOT NULL DEFAULT 0,
  -- A disabled scope is not rendered into the engine's config, and is
  -- exempt from the overlap rule: it hands out nothing to collide with.
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  -- Option 119: comma-separated suffixes a client appends to a bare name,
  -- or empty for no search list.
  domain_search TEXT NOT NULL DEFAULT '',
  -- Option 42: comma-separated IPv4 time servers, or empty.
  ntp_servers TEXT NOT NULL DEFAULT '',
  -- Option 121, as JSON: [{"destination":"10.10.0.0/16","router":"10.0.0.1"}].
  -- JSON rather than a child table because the list is written whole by one
  -- form, read whole by the renderer, and never joined against or counted.
  static_routes TEXT NOT NULL DEFAULT '[]',
  -- PXE: the boot server's address (siaddr), its name (sname, option 66) and
  -- the file to fetch (file, option 67). Each optional on its own.
  next_server TEXT NOT NULL DEFAULT '',
  server_hostname TEXT NOT NULL DEFAULT '',
  boot_file TEXT NOT NULL DEFAULT '',
  -- The escape hatch, as JSON: [{"code":150,"hex":"0A2A0005"}]. Any option
  -- with no column of its own, handed to the engine as raw bytes. Codes the
  -- renderer already emits by name are refused (store.ValidateScope), so one
  -- option never has two answers.
  options TEXT NOT NULL DEFAULT '[]',
  -- False makes Kea key a lease on the hardware address alone and ignore
  -- option 61, which is what cloned VMs sharing a client id need.
  match_client_id BOOLEAN NOT NULL DEFAULT TRUE,
  -- True renders the subnet with no pool at all: only reserved devices get
  -- an address from it.
  reservations_only BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  modified_at BIGINT NOT NULL
);

CREATE TABLE dhcp_reservations (
  id BIGSERIAL PRIMARY KEY,
  -- No ON DELETE CASCADE: DHCPStore.DeleteScope removes a scope's
  -- reservations itself, in the same transaction, so the bundle's prune can
  -- keep its one rule — children before parents — for this pair as for
  -- every other.
  scope_id BIGINT NOT NULL REFERENCES dhcp_scopes(id),
  -- Canonical lowercase 'aa:bb:cc:dd:ee:ff' (store.CanonicalMAC). Stored
  -- canonically so the unique index below actually means "this NIC",
  -- rather than meaning "this spelling of this NIC".
  mac TEXT NOT NULL,
  -- Inside the scope's cidr and not its gateway; validated in Go, since the
  -- check needs the parent row.
  ip TEXT NOT NULL,
  -- RFC 1123 label, or empty. Unique per scope when set — a rule
  -- store.ValidateReservation enforces rather than the schema, because two
  -- empty hostnames are not a collision.
  hostname TEXT NOT NULL DEFAULT '',
  comment TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  modified_at BIGINT NOT NULL,
  UNIQUE (scope_id, mac),
  UNIQUE (scope_id, ip)
);
