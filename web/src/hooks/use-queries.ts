import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { subscribeQueries, type SseState } from "../api/sse";
import type { QueryEntry } from "../api/types";

const LIVE_TAIL_CAP = 500;
const DEFAULT_SEARCH_LIMIT = 100;
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
 */
export function useLiveTail(enabled: boolean): LiveTailResult {
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

  useEffect(() => {
    if (!enabled) {
      setState("closed");
      return;
    }
    const unsubscribe = subscribeQueries((entry) => {
      pending.current.push(entry);
      flushTimer.current ??= setTimeout(flush, FLUSH_INTERVAL_MS);
    }, setState);

    return () => {
      unsubscribe();
      if (flushTimer.current !== undefined) {
        clearTimeout(flushTimer.current);
        flushTimer.current = undefined;
      }
      flush();
    };
  }, [enabled, attempt, flush]);

  const reconnect = useCallback(() => setAttempt((n) => n + 1), []);

  return useMemo(() => ({ entries, state, reconnect }), [entries, state, reconnect]);
}
