import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { List, Rule } from "../api/types";

// Canonical filters-domain hooks — lists (used by the dashboard's health
// strip, the setup wizard's starter blocklists, and the Filtering page's
// Lists tab) and rules (used by the dashboard/query-log quick block/allow
// actions and the Filtering page's Rules tab, Task 10).
export const filterKeys = {
  lists: ["filters", "lists"] as const,
  groupLists: (groupId: number) => ["filters", "groups", groupId, "lists"] as const,
  rules: (groupId: number) => ["filters", "rules", groupId] as const,
};

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
    onSuccess: () => qc.invalidateQueries({ queryKey: filterKeys.lists }),
  });
}

export function useToggleList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      api.patch<void>(`/filters/lists/${id}`, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: filterKeys.lists }),
  });
}

export function useDeleteList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/filters/lists/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: filterKeys.lists }),
  });
}

// POST /filters/refresh kicks off an async background refresh (202) — no
// list data changes synchronously, so there's nothing to invalidate here;
// the lists table catches up on its own next poll/interaction.
export function useRefreshFilters() {
  return useMutation({
    mutationFn: () => api.post<{ status: string }>("/filters/refresh"),
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
