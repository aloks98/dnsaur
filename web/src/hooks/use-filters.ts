import { useEffect, useRef } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { List, Rule } from "../api/types";

// Canonical filters-domain hooks — lists (used by the dashboard's health
// strip, the setup wizard's starter blocklists, and the Filtering page's
// Lists tab) and rules (used by the dashboard/query-log quick block/allow
// actions and the Filtering page's Rules tab, Task 10).
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

export function useAddList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { url: string; kind: "block" | "allow" }) =>
      api.post<{ id: number }>("/filters/lists", v),
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
export function useRules(groupId: number) {
  return useQuery({
    queryKey: filterKeys.rules(groupId),
    queryFn: () => api.get<Rule[]>(`/groups/${groupId}/rules`),
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
