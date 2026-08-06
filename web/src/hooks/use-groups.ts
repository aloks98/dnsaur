import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { Group, List } from "../api/types";
import { filterKeys, useAssignGroupLists } from "./use-filters";

// Groups-domain hooks — group CRUD (name, enabled) and reading each group's
// assigned lists. Rules and the list catalog itself stay in use-filters.ts
// (Task 9); this file consumes that file's useAssignGroupLists rather than
// redefining the PUT /groups/{id}/lists mutation, and reuses its
// filterKeys.groupLists cache key so useGroupLists' reads and that
// mutation's invalidation always target the same cache entry.
export const groupKeys = {
  all: ["groups"] as const,
};

export function useGroups() {
  return useQuery({
    queryKey: groupKeys.all,
    queryFn: () => api.get<Group[]>("/groups"),
  });
}

export function useAddGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.post<{ id: number }>("/groups", { name }),
    onSuccess: () => qc.invalidateQueries({ queryKey: groupKeys.all }),
  });
}

export function useRenameGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, name }: { id: number; name: string }) =>
      api.patch<void>(`/groups/${id}`, { name }),
    onSuccess: () => qc.invalidateQueries({ queryKey: groupKeys.all }),
  });
}

export function useToggleGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      api.patch<void>(`/groups/${id}`, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: groupKeys.all }),
  });
}

// The default group (id 1) is structural — the server 409s deleting it
// (internal/store/crud.go's DeleteGroup) — so the Groups & Clients tab
// disables the delete control for it up front rather than relying solely on
// this round-trip to say no.
export function useDeleteGroup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/groups/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: groupKeys.all }),
  });
}

// Lists currently assigned to one group — the read side of
// use-filters.ts's useAssignGroupLists (PUT /groups/{id}/lists), which owns
// the write and the shared cache key.
export function useGroupLists(groupId: number) {
  return useQuery({
    queryKey: filterKeys.groupLists(groupId),
    queryFn: () => api.get<List[]>(`/groups/${groupId}/lists`),
  });
}

// Re-exported under the name the Groups & Clients tab reaches for — the
// mutation itself already lives in use-filters.ts (Task 9) and is consumed,
// not duplicated, here.
export { useAssignGroupLists as useSetGroupLists };
