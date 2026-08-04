import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { List, Rule } from "../api/types";

// Other filters-domain reads/writes (groups) land here once the Filtering
// page task needs them; the setup wizard only ever creates.
export const filterKeys = {
  lists: ["filters", "lists"] as const,
  groupLists: (groupId: number) => ["filters", "groups", groupId, "lists"] as const,
  rules: (groupId: number) => ["filters", "groups", groupId, "rules"] as const,
};

// Read by the dashboard's health strip (list count + most recent
// last_refreshed) and, eventually, the Filtering page's list table.
export function useLists() {
  return useQuery({
    queryKey: filterKeys.lists,
    queryFn: () => api.get<List[]>("/filters/lists"),
  });
}

// Minimal read used by the query log's "why?" drawer to resolve a
// QueryEntry's rule_id into the pattern/action that matched. The Filtering
// page (Task 9) owns the fuller CRUD story (create/update/delete + proper
// invalidation) for rules and will supersede this; this is deliberately
// just a read, shared here the same way useLists() was added ahead of the
// Filtering page for the dashboard's health strip.
export function useRules(groupId: number) {
  return useQuery({
    queryKey: filterKeys.rules(groupId),
    queryFn: () => api.get<Rule[]>(`/groups/${groupId}/rules`),
  });
}

export function useCreateList() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { url: string; kind: "block" | "allow" }) =>
      api.post<{ id: number }>("/filters/lists", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: filterKeys.lists }),
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
