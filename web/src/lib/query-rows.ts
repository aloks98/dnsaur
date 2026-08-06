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
