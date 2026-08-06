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

export interface LocalRecord {
  id: number;
  name: string;
  type: "A" | "AAAA" | "CNAME" | "TXT";
  value: string;
  ttl: number;
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

export type Settings = Record<string, string>;
