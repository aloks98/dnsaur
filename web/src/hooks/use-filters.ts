import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

// GET /filters/lists (and other filters-domain reads) land here once the
// Filtering page task needs them; the setup wizard only ever creates.
export const filterKeys = {
  lists: ["filters", "lists"] as const,
  groupLists: (groupId: number) => ["filters", "groups", groupId, "lists"] as const,
};

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
