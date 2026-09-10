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
  /** Unix ms, `0` for never — which is every API token today; only sessions
   * are minted with an expiry. */
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
