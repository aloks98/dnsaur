import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { subscribeQueries, type SseState } from "../api/sse";
import type { QueryEntry } from "../api/types";

const LIVE_TAIL_CAP = 500;
const DEFAULT_SEARCH_LIMIT = 100;

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
 */
export function useQuerySearch(filter: QuerySearchFilter, options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: queryKeys.search(filter),
    queryFn: () => api.get<QueryEntry[]>(`/queries?${buildSearchParams(filter)}`),
    enabled: options?.enabled ?? true,
  });
}

export interface LiveTailResult {
  entries: QueryEntry[];
  state: SseState;
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
 */
export function useLiveTail(enabled: boolean): LiveTailResult {
  const [entries, setEntries] = useState<QueryEntry[]>([]);
  const [state, setState] = useState<SseState>(enabled ? "reconnecting" : "closed");

  useEffect(() => {
    if (!enabled) {
      setState("closed");
      return;
    }
    const unsubscribe = subscribeQueries(
      (entry) => setEntries((prev) => [entry, ...prev].slice(0, LIVE_TAIL_CAP)),
      setState,
    );
    return unsubscribe;
  }, [enabled]);

  return { entries, state };
}
