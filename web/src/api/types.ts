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
   * Milestone A creates and serves primary zones only — the rest are
   * accepted and stored but not yet acted on.
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
  /** secondary/stub/forwarder only; unused and empty for primary/internal. */
  primaries: string;
  tsig_key_id: number;
  /** Unix ms; secondary only, 0 otherwise. */
  expires_at: number;
  /** Unix ms of the last transfer that **succeeded**; secondary only, 0 = never. */
  refreshed_at: number;
  /**
   * Why the most recent transfer attempt failed, verbatim — `""` when it
   * succeeded. The server clears it on success, so a zone that recovered
   * stops reporting one, and it survives a restart (unlike the scheduler's
   * own in-memory view of the same thing). See lib/zones.ts, which is the one
   * place this and the three stamps are read together.
   */
  last_error: string;
  /**
   * Unix ms of that attempt, successful or not; 0 = never attempted. Only
   * meaningful beside `last_error`: an error with no date says nothing about
   * whether it is still true.
   */
  last_attempt: number;
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
  name: string;
  scope: "read" | "write";
  created_at: number;
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
