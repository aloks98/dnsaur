export interface MeResponse {
  id: number;
  username: string;
  totp_enabled: boolean;
}

export interface SetupState {
  setup_required: boolean;
}

export interface HealthStatus {
  status: string;
  version: string;
}

/**
 * One encrypted protocol's status (Go: api.ProtocolStatus) — intent (from
 * settings) alongside reality (whether the socket actually bound). The two
 * are allowed to disagree: a privileged port already taken, or a
 * certificate that will not load *at the moment a listener is (re)started*,
 * leaves `enabled` true and `listening` false. A certificate that stops
 * reading under an already-running listener does not — it keeps serving
 * from the keypair it cached, so that disagreement only surfaces at the
 * next start. `error` is the bind failure in that case, and is omitted
 * (`undefined`) whenever there's nothing wrong — mirrors the Go struct's
 * `omitempty`.
 */
export interface ProtocolStatus {
  enabled: boolean;
  listening: boolean;
  addr: string;
  error?: string;
}

/** DoT and DoH's ProtocolStatus side by side — they fail independently, so
 * this is never collapsed to one boolean. */
export interface ServingStatus {
  dot: ProtocolStatus;
  doh: ProtocolStatus;
}

/** The loaded certificate's expiry, as GET /resolver/status reports it.
 * Only present at all when a certificate has actually loaded — see
 * ResolverStatus.certificate. */
export interface CertificateStatus {
  not_after: string;
  expiring_soon: boolean;
}

/**
 * One replica registered with this main (Go: api.Replica). `last_seen` is
 * stamped by the main on registration, never sent by the replica, so a
 * replica's clock cannot decide whether it looks stale. `stale` is the
 * main's own verdict — nothing heard for three pull intervals — and is
 * never a reason to remove the row: an operator forgets a replica
 * deliberately.
 */
export interface SyncReplica {
  instance_id: string;
  /** Where this replica answers DNS (host:port) — the address an AXFR
   * arrives from and a NOTIFY goes to. */
  dns_addr: string;
  version_applied: number;
  /** Unix ms. */
  last_seen: number;
  stale: boolean;
  /** Whether that box runs a DHCP engine of its own, as its last version
   * probe reported. The main will not name a replica without one as the
   * hot-standby partner: it would answer no lease on the segment while the
   * primary waited out `max-response-delay` for it. */
  dhcp: boolean;
}

/**
 * GET /sync/status, and the same object GET /resolver/status carries as
 * `sync` (Go: api.SyncStatus). Both halves are optional because an
 * instance is only ever one of the two: a **replica** follows `peer_url`
 * and reports what it has applied, a **main** reports the key replicas
 * sign with and who is registered. An instance with no sync configured
 * reads as a main with no replicas.
 *
 * `sync.token` is deliberately absent — it is write-only (see
 * docs/api.md's Settings entry), and pairing is what writes it, so no
 * screen has a reason to read it back.
 */
export interface SyncStatus {
  role: "main" | "replica";
  /** Replica half. Omitted (`undefined`) rather than falsy when this is a
   * main — the same `omitempty` shape as ProtocolStatus.error. */
  peer_url?: string;
  peer_version?: number;
  applied_version?: number;
  applied_at?: number;
  last_pull_at?: number;
  /** Both halves. On a replica it is why the last pull failed; on a main it
   * is `sync.replicas` unreadable, which is a main that admits no transfer
   * and notifies nobody however many replicas the band lists. */
  last_error?: string;
  /** The peer is reached over plain `http://`, so the bundle — every TSIG
   * secret on the box included — travels in the clear. */
  plain_http?: boolean;
  /** Main half: the name of the TSIG key replicas sign their transfers
   * with. The main creates it on the first pairing and nobody picks it. */
  sync_key?: string;
  replicas?: SyncReplica[];
}

/**
 * POST /sync/pairing-code (Go: api's pairingCode) — the one-time code
 * the operator carries to the box that is to become a replica.
 *
 * Answered once and never readable again: only its hash is stored, and
 * minting a second code voids the first. The screen that asked for it is
 * the only place it exists.
 */
export interface PairingCode {
  /** Eight glyphs from an unambiguous alphabet, grouped `XXXX-XXXX` by the
   * server — shown exactly as it arrives. */
  code: string;
  /** Unix ms. */
  expires_at: number;
}

/**
 * GET /resolver/status — state of the running resolver that is not a
 * setting, and so deliberately not part of the flat GET /settings map.
 *
 * `encryption_downgraded` means the stored `upstreams` value asked for
 * tls:// or https://, would not parse, and the server fell back to its
 * hardcoded plaintext resolvers: queries are going out in the clear while
 * the settings page still shows the encrypted value. `reason` is the parse
 * failure.
 *
 * `serving` is each encrypted protocol's intent alongside reality (see
 * ProtocolStatus). `certificate` is the loaded certificate's expiry, and is
 * **omitted entirely** — not merely falsy — when no certificate has ever
 * loaded successfully: a fresh install with nothing configured yet, and a
 * configured pair that has never once read cleanly, both look like this.
 * That is a different fact from "expires soon", and conflating the two
 * would put a spurious expiry warning on every fresh install.
 */
export interface ResolverStatus {
  encryption_downgraded: boolean;
  reason: string;
  serving: ServingStatus;
  certificate?: CertificateStatus;
  /** The sync subsystem's whole visible state, carried here so every screen
   * can ask "am I a replica" without a second round trip. Optional: a
   * dnsaur older than config sync answers without it, and so does every
   * fixture written before it existed. Absent reads as a main. */
  sync?: SyncStatus;
  /** GET /dhcp/status' object, carried here for the same reason `sync` is:
   * the shell's warning strip and the nav both have to know what the engine
   * is doing on every screen, not only on the DHCP ones. Absent reads as
   * DHCP off — see DHCPStatus. */
  dhcp?: DHCPStatus;
}

export interface Group {
  id: number;
  name: string;
  enabled: boolean;
}

export interface Client {
  id: number;
  name: string;
  matcher: string;
  group_id: number;
}

/**
 * Outcome of a filter list's most recent refresh attempt (Go:
 * store.ListStatus*). These exist because `entry_count: 0` +
 * `last_refreshed: 0` used to mean four different things at once, and the UI
 * could only ever render one of them.
 *
 * - `pending` — never attempted. The *only* thing "never" is allowed to mean.
 * - `ok` — fetched (or 304'd) and parsed; entries are live.
 * - `stale` — this attempt failed, but a cached copy is still enforcing.
 *   `last_refreshed` dates that copy; `last_attempt` dates the failure.
 * - `failed` — the attempt failed and nothing is being served.
 * - `empty` — fetched and parsed fine, and still produced no usable entries
 *   (e.g. a format the parser rejects). Also blocking nothing, but the cause
 *   is the parser, not the network — `last_error` says which.
 */
export type ListStatus = "pending" | "ok" | "stale" | "failed" | "empty";

export interface List {
  id: number;
  url: string;
  /**
   * Readable label — what the UI shows instead of the raw URL in tables,
   * assignment menus, toasts and confirmations. Never empty: the server
   * derives one from the URL when the admin doesn't supply it (Go:
   * store.DeriveListName).
   */
  name: string;
  kind: "block" | "allow";
  enabled: boolean;
  /** Unix ms of the last *successful* fetch-and-parse; `0` = never. */
  last_refreshed: number;
  /** Unique domains currently compiled and enforcing. */
  entry_count: number;
  last_status: ListStatus;
  /** Short human reason; `""` when `last_status` is `pending` or `ok`. */
  last_error: string;
  /** Unix ms of the last attempt, successful or not; `0` = never tried. */
  last_attempt: number;
  /**
   * Unix ms of the moment the periodic download runs next; `0` when no
   * cadence is running. The same value on every row — the interval is
   * server-wide (`lists.refresh_hours`) and there are no per-list
   * schedules.
   *
   * Optional because only `GET /filters/lists` and the per-list refresh
   * response carry it; `GET /groups/{id}/lists` serves the stored row
   * alone.
   */
  next_refresh_at?: number;
}

export interface Rule {
  id: number;
  group_id: number;
  action: "allow" | "block";
  pattern: string;
  is_regex: boolean;
}

/**
 * An authoritative DNS zone (Go: store.Zone) — a claim of authority over a
 * suffix, not an override on top of an upstream forwarder. A name inside an
 * enabled zone is answered from it or refused by it; it is never forwarded,
 * which is what replaced the old flat local_records override table (the
 * Local DNS page, retired in Task 12).
 */
export interface Zone {
  id: number;
  /** Apex, lowercase, no trailing dot, e.g. "example.com". */
  name: string;
  /**
   * primary | secondary | stub | forwarder | internal; defaults to primary.
   *
   * Four are creatable through the API; `internal` is the RFC 6303 set
   * seeded at migration and is refused on create and patch alike.
   *
   * The two added in Milestone D6 both *claim a suffix and route it*
   * rather than answering from records of their own, and differ only in
   * where the addresses come from: a `forwarder`'s are typed into
   * `forward_to` by the operator, a `stub`'s are fetched from its
   * `primaries` as an SOA and an NS query with glue (not an AXFR) and
   * derived from the NS set that comes back.
   */
  type: "primary" | "secondary" | "stub" | "forwarder" | "internal";
  enabled: boolean;
  soa_ns: string;
  soa_mbox: string;
  soa_serial: number;
  soa_refresh: number;
  soa_retry: number;
  soa_expire: number;
  /** Negative-cache TTL (RFC 2308) for NXDOMAINs this zone hands out — not a floor on positive answers. */
  soa_minimum: number;
  /** The SOA record's own header TTL, distinct from soa_minimum. Fixed at 900 in Milestone A — no request field sets it. */
  soa_ttl: number;
  /**
   * Where a secondary transfers this zone from, or a stub fetches its NS
   * set from: comma-separated `host[:port]`, port 53 by default. Required
   * and non-empty on both of those, and refused with 400 on every other
   * type — a forwarder's upstreams are `forward_to`, which is a different
   * field because it is a different thing (see it below).
   */
  primaries: string;
  /** The key a secondary signs its transfer with, or a stub its SOA/NS
   * queries with; 0 is unsigned. Must be 0 on every other type. */
  tsig_key_id: number;
  /**
   * Unix ms the copy this zone holds stops being servable; **secondary
   * only, and 0 on every other type — including a stub.**
   *
   * A stub is never given one, deliberately (internal/zones/stub.go): its
   * NS set is routing information rather than data held on loan, and an
   * old-but-working nameserver beats a self-inflicted SERVFAIL. So a
   * screen that read this as "expired at" would date every stub to the
   * epoch. Nothing may render it for a zone that is not a secondary — see
   * transferState in lib/zones.ts, which is the one place it is read.
   */
  expires_at: number;
  /** Unix ms of the last transfer or stub fetch that **succeeded**;
   * secondary and stub only, 0 = never. */
  refreshed_at: number;
  /**
   * Why the most recent transfer — or, for a stub, NS fetch — failed,
   * verbatim; `""` when it succeeded. The server clears it on success, so a
   * zone that recovered stops reporting one, and it survives a restart
   * (unlike the scheduler's own in-memory view of the same thing). See
   * lib/zones.ts, which is the one place this and the three stamps are read
   * together.
   */
  last_error: string;
  /**
   * Unix ms of that attempt, successful or not; 0 = never attempted. Only
   * meaningful beside `last_error`: an error with no date says nothing about
   * whether it is still true.
   */
  last_attempt: number;
  /**
   * Who may pull this zone: a comma-separated list of address, CIDR, or
   * key:<tsig name>, in FormatACL's canonical spelling. "" means deny, and
   * that is the default. Applies to both primary and secondary zones — a
   * secondary re-serves what it pulled.
   */
  allow_transfer: string;
  /**
   * The outbound twin of last_attempt: unix ms of the last inbound transfer
   * request this zone answered, served or refused; 0 = never asked.
   */
  last_xfr_at: number;
  /** The address that asked, no port. Only meaningful beside last_xfr_at. */
  last_xfr_peer: string;
  /** Why that request was refused, verbatim; "" when it was served — the
   * outbound twin of last_error. */
  last_xfr_error: string;
  /**
   * Who this zone tells when it changes (DNS NOTIFY, RFC 1996): a
   * comma-separated list of host[:port] (port always explicit on read),
   * each with an optional key:<tsig name> suffix to sign that target's
   * NOTIFY. "" means notify nobody, which is the default. Applies to both
   * primary and secondary zones — a secondary that re-serves what it
   * pulled has its own downstream secondaries to tell — unlike primaries
   * and tsig_key_id, which are secondary-only. See lib/notify.ts for the
   * format, and GET /zones/{id}/notifies for each target's delivery state.
   */
  notify_to: string;
  /**
   * Where a forwarder zone sends the queries it claims: comma-separated
   * `host[:port]`, port always explicit on read. **Forwarder only; ""
   * on every other type**, which the server enforces on create and patch
   * alike.
   *
   * "" on a forwarder is a configuration, not a gap: the zone still claims
   * the suffix, and with no upstream to send to every query beneath it is
   * a SERVFAIL rather than a fall-through to the default resolvers. That
   * consequence is the one thing a forwarder's page has to say out loud —
   * see pages/zones/detail.tsx.
   *
   * Stored and read back in its canonical spelling (the port always
   * written, ", "-separated), which may differ from what was sent.
   */
  forward_to: string;
  /**
   * When the scheduler will next attempt this zone, unix ms — **0 whenever
   * there is no back-off pending**, which is every healthy zone, every type
   * that does not pull, and every zone in a server process that has not
   * scheduled one yet.
   *
   * This and `failures` are the scheduler's own view (Go:
   * zones.Refresher.Status), and unlike `last_error`/`last_attempt` they are
   * process-local: a restart forgets both. So they *add to* the durable pair
   * rather than replacing it — read alone they would show a zone that had
   * been failing for a week as healthy after every restart. What they add is
   * the two things no column can say: when the next attempt actually falls,
   * and how many have failed in a row.
   */
  next_attempt_at: number;
  /** Consecutive failed attempts in the running server process; 0 after a
   * success, and 0 for a zone that process has never scheduled. See
   * `next_attempt_at`. */
  failures: number;
  created_at: number;
  modified_at: number;
}

/** One resource record within a zone (Go: store.ZoneRecord), named relative to the zone's apex. */
export interface ZoneRecord {
  id: number;
  zone_id: number;
  /** Owner name relative to the zone apex: "@" for the apex itself, "bifrost", "*", "*.nexus". */
  name: string;
  /** DNS RR type, e.g. A, AAAA, CNAME, MX, TXT, CAA, NS. */
  type: string;
  ttl: number;
  /**
   * DNS presentation format, rdata portion only — "192.168.150.28" for A,
   * "10 mail.example.com." for MX, `0 issue "letsencrypt.org"` for CAA.
   */
  rdata: string;
  enabled: boolean;
  comment: string;
}

export interface QueryEntry {
  id: number;
  at: number;
  instance_id: string;
  client_ip: string;
  client_id: number;
  q_name: string;
  q_type: string;
  decision: string;
  rule_id: number;
  list_id: number;
  upstream: string;
  r_code: string;
  duration_ms: number;
  /** The rule pattern or list entry that fired; "" unless the row was blocked. */
  matched: string;
  /**
   * The DHCP lease table's name for `client_ip`, joined on as the row is
   * read — nothing is stored, so an old row carries whoever holds that
   * address *now*.
   *
   * **Absent, not empty**, when DHCP is off, when no lease holds the
   * address, and on every row while `qlog.privacy` is `anon`.
   */
  hostname?: string;
}

export interface StatsOverview {
  total: number;
  blocked: number;
  cached: number;
  forwarded: number;
  clients: number;
}

export interface TimelineBucket {
  bucket: number;
  decisions: Record<string, number>;
}

export interface TopEntry {
  key: string;
  count: number;
}

export interface ApiToken {
  id: number;
  user_id: number;
  /** `"api"` on everything `GET /tokens` returns — session rows are filtered
   * out server-side (internal/api/tokens_handlers.go). Mirrored because
   * store.AuthToken sends it and this file is that struct's counterpart. */
  kind: string;
  name: string;
  scope: "read" | "write";
  created_at: number;
  /** Unix ms, `0` for never — the default, and what every token created
   * before `POST /tokens` took an `expires_at` carries. Unlike a session's,
   * this one never slides: it is a date the token's owner chose. */
  expires_at: number;
  last_used: number;
}

/**
 * A TSIG key (RFC 8945): the shared secret that authenticates a zone
 * transfer. Both ends hold the same named key and sign every transfer
 * message with it (Go: store.TSIGKey).
 *
 * The deliberate opposite of ApiToken above, in the one way that matters:
 * `secret` is returned on **every** read, not shown once at creation. It has
 * to be pasted unchanged into the matching key on the peer (BIND's `key{}`
 * clause, Technitium's transfer settings), so it is stored as plaintext and
 * handed back on list and get — see docs/api.md's TSIG keys section. That is
 * why the screen's masking (pages/tsig-keys.tsx) is a display choice about
 * what sits on screen, not a security boundary.
 */
export interface TSIGKey {
  id: number;
  /** Canonical owner name: lowercase, fully qualified — "xfer.e412.in." */
  name: string;
  /**
   * miekg/dns's own constant, **with** the trailing dot: "hmac-sha256.".
   * The screen shows these without it — see lib/tsig.ts for the one place
   * that translates between the two.
   */
  algorithm: string;
  /** base64, exactly as the peer's config wants it. */
  secret: string;
  created_at: number;
}

export type Settings = Record<string, string>;

/**
 * POST /backup's answer: the file the server wrote, on the server's own
 * filesystem. Nothing here is fetchable — no endpoint serves the file — so
 * the path is shown for an operator to copy it off the box themselves.
 */
export interface BackupResult {
  path: string;
  bytes: number;
}

/**
 * DHCP (spec §8.3, docs/api.md's DHCP entry). dnsaur does not serve the
 * protocol — ISC Kea does — but it owns everything an operator touches, so
 * a scope and a reservation are rows here like any other synced
 * configuration, and a lease is not a row at all.
 */

/** One entry of option 121. Both halves are required; the router must be
 * inside the scope's cidr, which the server checks. */
export interface DHCPStaticRoute {
  destination: string;
  router: string;
}

/** An option with no field of its own, as raw hex. The server refuses a
 * code it already emits by name, so one code never has two answers. */
export interface DHCPGenericOption {
  code: number;
  hex: string;
}

/**
 * One DHCP subnet, rendered as one Kea `subnet4`.
 *
 * `dns_servers` empty is **not** "no DNS": it is the automatic answer of
 * this box's address followed by its HA partner's. `lease_seconds` 0 falls
 * back to the `dhcp.lease_seconds` setting, and `domain` empty to
 * `dhcp.domain` — both are "use the instance default", not "none".
 *
 * `match_client_id` is the one field whose create default (`true`) is not
 * its zero value, so the form always sends it explicitly.
 */
export interface DHCPScope {
  id: number;
  name: string;
  /** An IPv4 prefix in masked form. Host bits set are refused rather than
   * quietly masked — "10.0.0.5/24" and "10.0.0.0/24" look alike in a form
   * and hand out different subnets. */
  cidr: string;
  pool_start: string;
  pool_end: string;
  gateway: string;
  dns_servers: string;
  domain: string;
  lease_seconds: number;
  enabled: boolean;
  domain_search: string;
  ntp_servers: string;
  static_routes: DHCPStaticRoute[];
  next_server: string;
  server_hostname: string;
  boot_file: string;
  options: DHCPGenericOption[];
  match_client_id: boolean;
  reservations_only: boolean;
  created_at: number;
  modified_at: number;
}

/**
 * A fixed address for one MAC inside one scope.
 *
 * `scope_id` is fixed once created — an address validated against one
 * subnet must not be carried into another — so the edit form offers no
 * scope select and the API refuses a PATCH that moves one.
 */
export interface DHCPReservation {
  id: number;
  scope_id: number;
  /** Canonical lowercase `aa:bb:cc:dd:ee:ff`; the server takes any
   * notation `net.ParseMAC` accepts and stores it in this one. */
  mac: string;
  ip: string;
  /** One RFC 1123 label — the suffix comes from the scope — or empty. */
  hostname: string;
  comment: string;
  created_at: number;
  modified_at: number;
}

/**
 * One row of the lease table as of the last poll. Never stored by dnsaur:
 * it is read from the engine every `dhcp.lease_poll_seconds` and replaced
 * whole, which is also why releasing one does not empty the row until the
 * next poll lands.
 */
export interface DHCPLease {
  scope_id: number;
  ip: string;
  mac: string;
  /** The client's own name, or the reservation's when the client sent
   * none. `""` for a device that offered nothing usable. */
  hostname: string;
  /** Unix ms, and **0 for a reservation nothing has leased yet** — the one
   * field that tells such a row from a live lease. */
  expires_at: number;
  reserved: boolean;
}

/** What `status-get` reports about the pair. Absent on a single box, which
 * is a different fact from a pair that is not talking. */
export interface DHCPHAStatus {
  mode: string;
  local_state: string;
  /** What the partner calls itself, as the engine talking to it reports the
   * name. **Absent on an engine whose `status-get` does not carry it**, which
   * the status line has to handle anyway for a box with no partner. */
  peer?: string;
  remote_state: string;
  communication_interrupted: boolean;
  unacked_clients: number;
}

/** How full one scope's pool is. `leased` may exceed `pool_size` after a
 * pool is shrunk: the engine keeps what it has already handed out. */
export interface DHCPScopeUsage {
  id: number;
  pool_size: number;
  leased: number;
}

/**
 * GET /dhcp/status, and the same object GET /resolver/status carries as
 * `dhcp`.
 *
 * `enabled: false` — the `kea_socket` bootstrap key is empty — is the whole
 * answer on a box with no engine, and the only DHCP answer it gives: every
 * other route 404s. Read `enabled` first; everything else is `omitempty`.
 *
 * A configuration the engine would not take (`config rejected`) outranks an
 * engine that is not there: it is the one an operator has to act on, and it
 * is still true when the engine comes back.
 */
export interface DHCPStatus {
  enabled: boolean;
  engine?: "ok" | "unreachable" | "config rejected";
  /** What `version-get` reported, e.g. "2.6.3". */
  engine_version?: string;
  /** The engine's own words: its refusal of the last `config-set`, or why
   * it could not be reached. Cleared by the next render it accepts. */
  message?: string;
  /** How stale the lease table is. An engine that stops answering keeps
   * the table it last gave — still the truth about the segment. */
  table_age_seconds: number;
  ha?: DHCPHAStatus;
  scopes: DHCPScopeUsage[];
}
