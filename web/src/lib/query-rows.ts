import type { QueryEntry } from "../api/types";

/**
 * Shared rendering rules for a query-log row.
 *
 * Both the Query Log's table and the Dashboard's live feed render the same
 * `QueryEntry` objects off the same SSE stream, so the two ways of getting a
 * row wrong belong in one place rather than being rediscovered per screen —
 * the dashboard originally keyed on `entry.id` and printed `0ms`, both of
 * which this module exists to prevent.
 */

/**
 * A stable key per row — deliberately NOT `entry.id`.
 *
 * A live row's `id` is 0. internal/qlog/qlog.go publishes the entry to the
 * SSE hub in the same breath as it queues it for the batched database write,
 * so it goes out before the insert assigns a primary key: every row on the
 * stream carries the zero value. Keying on it (react-table's `getRowId`,
 * React's list keys, the virtualizer's item keys, the "which row is
 * selected" comparison, the per-row action status map) collapses the entire
 * live tail onto one identity.
 *
 * Paged rows come from the database and do have real ids, so they use them.
 * Live rows get a monotonic client-side key instead, remembered per entry
 * object in a WeakMap — each SSE message is its own JSON.parse result, so
 * the object identity is unique and the map costs nothing once the ring
 * buffer drops the row.
 */
const LIVE_ROW_KEYS = new WeakMap<QueryEntry, string>();
let liveRowSeq = 0;

export function rowKey(entry: QueryEntry): string {
  if (entry.id > 0) return `q${entry.id}`;
  let key = LIVE_ROW_KEYS.get(entry);
  if (key === undefined) {
    liveRowSeq += 1;
    key = `live${liveRowSeq}`;
    LIVE_ROW_KEYS.set(entry, key);
  }
  return key;
}

/**
 * `duration_ms` is `time.Duration.Milliseconds()` — truncated whole
 * milliseconds (internal/qlog/qlog.go). A cache hit answered in 180µs is
 * therefore logged as 0, and printing "0 ms" claims an instantaneous
 * resolve; "<1" says what was actually measured.
 */
export function durationLabel(ms: number): string {
  return ms > 0 ? String(ms) : "<1";
}

/**
 * The group a rule is written into when nothing else names one.
 *
 * Id 1 is structural: the server seeds it, refuses to delete it
 * (internal/store/crud.go's DeleteGroup) and resolves every client that
 * matched no client row to it (internal/clients/registry.go's Lookup). Five
 * screens needed the same constant, and five copies of a number that has to
 * agree with two Go files is four too many.
 */
export const DEFAULT_GROUP_ID = 1;

/**
 * Per-decision tint, shared by the dashboard's live feed and the query
 * log's table.
 *
 * The six-way decision vocabulary is the resolver's (see
 * internal/dnssrv/pipeline.go); this is the flat, text-only reading of it —
 * no badges, no icons, because a column of a thousand tinted pills is the
 * loudest thing on a page whose whole point is scanning a thousand rows.
 *
 * There is no `allowed` entry on purpose: there is no such decision in the
 * Go enum, so a row can't carry one. Both screens render the same enum, so
 * one map tracks it rather than two that have to be kept in step.
 */
const DECISION_TONE: Record<string, string> = {
  blocked: "text-destructive",
  // A failed resolve is a broken query, not a policy decision — but on a
  // one-line readout it needs the same "look at me" weight as a block.
  error: "text-destructive",
  stale: "text-warn",
  cached: "text-muted-foreground",
  forwarded: "text-foreground",
  authoritative: "text-primary",
};

export function decisionTone(decision: string): string {
  return DECISION_TONE[decision] ?? "text-muted-foreground";
}

/** `at` is unix ms; both screens print the wall clock and nothing else —
 * the date is the window's job, not a row's. */
export function clockTime(atMs: number): string {
  return new Date(atMs).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/**
 * `client_id` (or an exact-IP matcher) → the client's name, for whichever
 * column is showing a hostname.
 *
 * Names are **not validated server-side and may be empty** (ui-contract
 * §3.4 — the Add-client form allows it too). An empty name in the map makes
 * every `?? UNKNOWN` fallback at the call site fire on a value that exists,
 * so the cell renders blank instead of the em dash or the address the page
 * promised. Skipping the empty ones is what keeps that fallback reachable.
 */
export function namesByKey<T extends { name: string }, K>(
  clients: readonly T[] | undefined,
  key: (client: T) => K,
): Map<K, string> {
  const byKey = new Map<K, string>();
  for (const client of clients ?? []) {
    if (client.name !== "") byKey.set(key(client), client.name);
  }
  return byKey;
}
