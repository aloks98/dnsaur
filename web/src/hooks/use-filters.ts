import { useEffect, useRef } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { List, Rule } from "../api/types";

// Canonical filters-domain hooks — lists (used by the dashboard's health
// strip, the setup wizard's starter blocklists, and the Filtering page's
// Lists tab) and rules (used by the dashboard/query-log quick block/allow
// actions and the Filtering page's Rules tab, Task 10).
//
// Error handling deliberately lives at the call site, not here — see the
// "Data layer" section of web/README.md. These hooks stay pure data access
// so the same hook can be reused by callers that need different handling:
// deleting a list toasts the list's URL on the Filtering page, while the
// setup wizard's bulk create reports a partial-failure count instead. A
// toast in here would be wrong for one of them and duplicated for both.
// Queries surface their own failures in the component (an inline error
// state, or a stale-data banner over still-valid data); the only
// app-global handler is the 401 session recovery in lib/query-client.ts.
export const filterKeys = {
  lists: ["filters", "lists"] as const,
  /** The common prefix of every groupLists(n) key. react-query matches
   * invalidations by key *prefix*, and `lists` (["filters","lists"]) is a
   * sibling of this, not a parent — so a mutation that changes which lists
   * exist has to invalidate this too, or the Groups & Clients tab's
   * per-group assignments keep showing a list that's already gone. */
  groups: ["filters", "groups"] as const,
  groupLists: (groupId: number) => ["filters", "groups", groupId, "lists"] as const,
  rules: (groupId: number) => ["filters", "rules", groupId] as const,
};

// Adding, toggling, or deleting a list changes both the lists table and
// every group's assigned-list set (deleting a list removes it from the
// groups it was assigned to, server-side), so both key trees are
// invalidated together.
function invalidateListsEverywhere(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: filterKeys.lists });
  void qc.invalidateQueries({ queryKey: filterKeys.groups });
}

// Read by the dashboard's health strip (list count + most recent
// last_refreshed) and the Filtering page's Lists tab.
export function useLists() {
  return useQuery({
    queryKey: filterKeys.lists,
    queryFn: () => api.get<List[]>("/filters/lists"),
  });
}

/** `name` is optional — the server derives one from the URL when it's
 * omitted or blank, so a list always has a label the UI can print. */
export function useAddList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { url: string; kind: "block" | "allow"; name?: string }) =>
      api.post<{ id: number }>("/filters/lists", v),
    onSuccess: () => invalidateListsEverywhere(qc),
  });
}

/** Renames a list. A blank name resets it to the URL-derived default rather
 * than clearing it. */
export function useRenameList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, name }: { id: number; name: string }) =>
      api.patch<void>(`/filters/lists/${id}`, { name }),
    onSuccess: () => invalidateListsEverywhere(qc),
  });
}

export function useToggleList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      api.patch<void>(`/filters/lists/${id}`, { enabled }),
    onSuccess: () => invalidateListsEverywhere(qc),
  });
}

export function useDeleteList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/filters/lists/${id}`),
    onSuccess: () => invalidateListsEverywhere(qc),
  });
}

/** How long after a 202 to re-read the lists table. POST /filters/refresh
 * only *schedules* the work — internal/filter/refresh.go downloads each
 * list and writes its new last_refreshed/entry_count as it finishes, so
 * there is nothing new to read at the moment the request resolves. A
 * short ladder rather than a single delay: a couple of small, already-warm
 * lists land almost immediately, a cold multi-megabyte one doesn't. */
const REFRESH_SETTLE_MS = [1_000, 4_000, 12_000];

/**
 * POST /filters/refresh — fire-and-forget (202), so `onSuccess` fires long
 * before any list row actually changes. Nothing invalidates it for us
 * either: useLists has no refetchInterval and the query client turns
 * refetchOnWindowFocus off (see lib/query-client.ts), so without this the
 * table would keep reading "refreshed 3d ago" straight after a successful
 * manual refresh until the tab remounted and the staleTime elapsed.
 * Re-fetching on a delay is the honest approximation of "tell me when the
 * server is done" that the API doesn't otherwise offer.
 */
export function useRefreshFilters() {
  const qc = useQueryClient();
  // Cleared on unmount so a pending nudge can't outlive the page (or, in
  // tests, the render) that started it.
  const timers = useRef<ReturnType<typeof setTimeout>[]>([]);
  useEffect(
    () => () => {
      for (const t of timers.current) clearTimeout(t);
      timers.current = [];
    },
    [],
  );

  return useMutation({
    mutationFn: () => api.post<{ status: string }>("/filters/refresh"),
    onSuccess: () => {
      for (const ms of REFRESH_SETTLE_MS) {
        timers.current.push(
          setTimeout(() => void qc.invalidateQueries({ queryKey: filterKeys.lists }), ms),
        );
      }
    },
  });
}

// Rules for a single group. Used by the "why?" drawer (query log) to
// resolve a QueryEntry's rule_id, and by the Filtering page's Rules tab
// (Task 10).
// `enabled` for the query log, which only needs a group's rules once a row
// is selected — the same escape hatch useQuerySearch offers, and for the
// same reason: a screen that fetches a ruleset it has no use for pays for it
// on every mount.
export function useRules(groupId: number, options?: { enabled?: boolean }) {
  return useQuery({
    queryKey: filterKeys.rules(groupId),
    queryFn: () => api.get<Rule[]>(`/groups/${groupId}/rules`),
    enabled: options?.enabled ?? true,
  });
}

// groupId travels with each call's variables (rather than being bound at
// hook-creation time) so one hook instance works for the dashboard/query
// log's fixed-group quick block/allow actions *and* the Filtering page's
// multi-group Rules tab (Task 10) alike.
export function useAddRule() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      groupId,
      ...body
    }: {
      groupId: number;
      action: "allow" | "block";
      pattern: string;
      is_regex?: boolean;
    }) => api.post<{ id: number }>(`/groups/${groupId}/rules`, body),
    onSuccess: (_data, { groupId }) =>
      qc.invalidateQueries({ queryKey: filterKeys.rules(groupId) }),
  });
}

export function useDeleteRule() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id }: { id: number; groupId: number }) => api.del<void>(`/filters/rules/${id}`),
    onSuccess: (_data, { groupId }) =>
      qc.invalidateQueries({ queryKey: filterKeys.rules(groupId) }),
  });
}

export function useAssignGroupLists() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ groupId, listIds }: { groupId: number; listIds: number[] }) =>
      api.put<void>(`/groups/${groupId}/lists`, { list_ids: listIds }),
    onSuccess: (_data, { groupId }) =>
      qc.invalidateQueries({ queryKey: filterKeys.groupLists(groupId) }),
  });
}
