import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import { subscribeQueries, type SseState } from "../api/sse";
import { authKeys } from "./use-auth";
import type { QueryEntry } from "../api/types";

/** Exported because the query log's footer states both numbers to the user
 * ("500 row buffer", "limit 100") and they must not drift from the ones the
 * hooks actually use. */
export const LIVE_TAIL_CAP = 500;
export const DEFAULT_SEARCH_LIMIT = 100;
/**
 * How much history the live tail opens with.
 *
 * The stream only ever carries queries answered *after* it was subscribed,
 * so on a server with a million logged queries the page used to open on
 * "Listening — queries appear here…" and stay there until the next lookup —
 * which reads as a broken or empty install, not as a tail. One page of
 * `GET /queries` is enough to make the screen show what the box has actually
 * been doing; a fifth of the ring buffer leaves room for the stream to
 * prepend to it without immediately evicting what it seeded.
 */
export const LIVE_TAIL_SEED = 100;
// Incoming SSE rows are coalesced into one state update per frame-ish
// window. A busy resolver emits far more messages than the eye can follow,
// and every single one used to rebuild all 500 @tanstack/react-table Row
// objects (getCoreRowModel memoizes on the data array identity) —
// virtualization bounds the DOM, not that. 100ms still reads as "live".
const FLUSH_INTERVAL_MS = 100;

export interface QuerySearchFilter {
  from?: number;
  to?: number;
  client?: string;
  q?: string;
  decision?: string;
  type?: string;
  limit?: number;
  offset?: number;
}

export const queryKeys = {
  search: (filter: QuerySearchFilter) => ["queries", "search", filter] as const,
};

function buildSearchParams(filter: QuerySearchFilter): string {
  const params = new URLSearchParams();
  if (filter.from) params.set("from", String(filter.from));
  if (filter.to) params.set("to", String(filter.to));
  if (filter.client) params.set("client", filter.client);
  if (filter.q) params.set("q", filter.q);
  if (filter.decision) params.set("decision", filter.decision);
  if (filter.type) params.set("type", filter.type);
  params.set("limit", String(filter.limit ?? DEFAULT_SEARCH_LIMIT));
  if (filter.offset) params.set("offset", String(filter.offset));
  return params.toString();
}

/**
 * Paged/filtered query-log search — `GET /queries?…`. Used whenever any
 * filter or domain search is active (see pages/queries.tsx); pass
 * `enabled: false` while the live tail is showing instead, so this doesn't
 * compete with the SSE stream for the same screen real estate.
 *
 * Paged, not capped: the endpoint answers at most `limit` rows per call, so
 * a single request would silently truncate a filtered view at 100 matches
 * while the page promises "live and historical". Each page asks for the next
 * `offset`, and a short page (fewer rows than asked for) is the end of the
 * results — that's what stops `hasNextPage`, which the grid uses to drive
 * infinite scroll.
 */
export function useQuerySearch(filter: QuerySearchFilter, options?: { enabled?: boolean }) {
  const limit = filter.limit ?? DEFAULT_SEARCH_LIMIT;
  return useInfiniteQuery({
    queryKey: queryKeys.search(filter),
    queryFn: ({ pageParam }) =>
      api.get<QueryEntry[]>(`/queries?${buildSearchParams({ ...filter, offset: pageParam })}`),
    initialPageParam: 0,
    getNextPageParam: (lastPage, allPages) =>
      lastPage.length < limit ? undefined : allPages.reduce((n, page) => n + page.length, 0),
    enabled: options?.enabled ?? true,
  });
}

export interface LiveTailResult {
  entries: QueryEntry[];
  state: SseState;
  /** Reopens the stream after it has given up (`state === "failed"`). */
  reconnect: () => void;
}

/**
 * Everything about a logged query except its `id` — the one field that
 * cannot be compared across the two sources being merged.
 *
 * lib/query-rows.ts's `rowKey` is deliberately not usable for this: a stream
 * row's `id` is 0 (internal/qlog/qlog.go publishes to the SSE hub before the
 * batched insert assigns a primary key), so rowKey hands live rows a
 * synthetic `live<n>` key and seeded rows a real `q<id>` one. Those two
 * namespaces can never collide, which is exactly right for React keys and
 * exactly useless for "is this seeded row the same query as one the stream
 * already delivered?".
 *
 * It genuinely can be the same query: the seed request and the subscription
 * are started together, so any query answered in between goes out on the
 * stream (id 0) *and*, once the logger's batch lands, comes back in the seed
 * page with a real id. Comparing the fields both copies share is what stops
 * it rendering twice.
 */
function queryIdentity(e: QueryEntry): string {
  return [
    e.at,
    e.client_ip,
    e.client_id,
    e.q_name,
    e.q_type,
    e.decision,
    e.rule_id,
    e.list_id,
    e.upstream,
    e.r_code,
    e.duration_ms,
  ].join("\u001f");
}

/**
 * Live tail of the query log via SSE (GET /queries/tail), kept as an
 * in-memory ring buffer (cap LIVE_TAIL_CAP, newest first).
 *
 * `enabled` governs both concerns the brief separates conceptually —
 * "the user paused" and "a filtered/paged view is active instead" —
 * because they have an identical requirement here: stop consuming the
 * stream, but never clear what's already rendered. `entries` lives in this
 * hook's own state and is only ever appended to, never reset, so toggling
 * `enabled` off (for either reason) never blanks the table; toggling it
 * back on just reopens the stream and prepends whatever arrives next.
 *
 * Arrivals are batched (FLUSH_INTERVAL_MS) rather than committed one at a
 * time. Anything still sitting in the batch is flushed by the effect's own
 * cleanup, so pausing mid-burst neither drops those rows nor replays ones
 * already rendered.
 *
 * The buffer is *seeded* once, from `GET /queries`, the first time this runs
 * enabled — see LIVE_TAIL_SEED. Seeding is deliberately once per mount and
 * not once per `enabled` edge: resuming from a pause, or clearing a filter,
 * must not re-fetch a page of history and re-append it under rows that are
 * already on screen.
 */
export function useLiveTail(enabled: boolean): LiveTailResult {
  const qc = useQueryClient();
  const [entries, setEntries] = useState<QueryEntry[]>([]);
  const [state, setState] = useState<SseState>(enabled ? "reconnecting" : "closed");
  // Bumped by reconnect() to re-run the effect (and so reopen the stream)
  // after the subscription has given up on a dead endpoint.
  const [attempt, setAttempt] = useState(0);

  const pending = useRef<QueryEntry[]>([]);
  const flushTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const flush = useCallback(() => {
    flushTimer.current = undefined;
    const batch = pending.current;
    if (batch.length === 0) return;
    pending.current = [];
    // Arrivals are chronological; the table is newest-first.
    batch.reverse();
    setEntries((prev) => batch.concat(prev).slice(0, LIVE_TAIL_CAP));
  }, []);

  /**
   * Drop a page of history in under whatever the stream has already
   * delivered.
   *
   * Pending arrivals are committed first: they are part of "what's already on
   * screen" as far as the user is concerned, and comparing the seed against a
   * buffer that doesn't contain them yet would let a row the stream delivered
   * 50ms ago come back a second time from the database.
   *
   * Seed rows go *below* the live ones rather than being merged by `at`. The
   * live rows are, by construction, the newest — they arrived after we
   * subscribed — so appending keeps the table newest-first without reordering
   * a tail whose order is the order the resolver actually answered in.
   */
  const seed = useCallback(
    (rows: QueryEntry[]) => {
      if (flushTimer.current !== undefined) {
        clearTimeout(flushTimer.current);
        flushTimer.current = undefined;
      }
      flush();
      setEntries((prev) => {
        const already = new Set(prev.map(queryIdentity));
        const history = rows.filter((row) => !already.has(queryIdentity(row)));
        return history.length === 0 ? prev : prev.concat(history).slice(0, LIVE_TAIL_CAP);
      });
    },
    [flush],
  );

  const seedRequested = useRef(false);
  useEffect(() => {
    if (!enabled || seedRequested.current) return;
    seedRequested.current = true;
    // Deliberately not cancelled on cleanup. StrictMode mounts effects
    // twice, and the second pass is short-circuited by the ref above — so
    // an aborted first request would mean no history at all in development.
    // Applying late is harmless: `seed` dedupes against the buffer, and
    // setState on an unmounted component is a no-op.
    api.get<QueryEntry[]>(`/queries?limit=${LIVE_TAIL_SEED}`).then(seed, () => {
      // A failed seed is not worth a toast or an error state: the tail
      // itself is unaffected and the page still fills as queries arrive.
      // The stale-data surface on this screen belongs to the paged search.
    });
  }, [enabled, seed]);

  /**
   * A stream that has given up is the one state worth asking a question
   * about. EventSource reports a 401 as an ordinary error, so an expired
   * session is indistinguishable from a blip until the retries are spent —
   * and then the likeliest explanation is a session that no longer exists.
   * Revalidating `me` lets the auth gate answer that, instead of leaving a
   * dead "Reconnect" button as the only thing on offer.
   */
  const handleState = useCallback(
    (next: SseState) => {
      setState(next);
      if (next === "failed") void qc.invalidateQueries({ queryKey: authKeys.me, exact: true });
    },
    [qc],
  );

  useEffect(() => {
    if (!enabled) {
      setState("closed");
      return;
    }
    const unsubscribe = subscribeQueries((entry) => {
      pending.current.push(entry);
      flushTimer.current ??= setTimeout(flush, FLUSH_INTERVAL_MS);
    }, handleState);

    return () => {
      unsubscribe();
      if (flushTimer.current !== undefined) {
        clearTimeout(flushTimer.current);
        flushTimer.current = undefined;
      }
      flush();
    };
  }, [enabled, attempt, flush, handleState]);

  const reconnect = useCallback(() => setAttempt((n) => n + 1), []);

  return useMemo(() => ({ entries, state, reconnect }), [entries, state, reconnect]);
}
