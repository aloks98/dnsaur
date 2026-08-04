import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

// Same live-feel cadence as the dashboard's stats polling (see
// use-stats.ts) — cheap read, and a paused-until banner that silently goes
// stale would be actively misleading.
const POLL_MS = 30_000;

const GLOBAL_GROUP_ID = 0;

export const blockingKeys = {
  status: (groupId: number) => ["blocking", groupId] as const,
};

export interface BlockingStatus {
  /** Unix ms; `0` means blocking is active (not paused). */
  paused_until: number;
}

export function useBlockingStatus(groupId: number = GLOBAL_GROUP_ID) {
  return useQuery({
    queryKey: blockingKeys.status(groupId),
    queryFn: () => api.get<BlockingStatus>(`/blocking?group_id=${groupId}`),
    refetchInterval: POLL_MS,
  });
}

export function usePauseBlocking() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ groupId, minutes }: { groupId: number; minutes: number }) =>
      api.post<void>("/blocking/pause", { group_id: groupId, minutes }),
    onSuccess: (_data, { groupId }) =>
      qc.invalidateQueries({ queryKey: blockingKeys.status(groupId) }),
  });
}

export function useResumeBlocking() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (groupId: number) => api.del<void>(`/blocking/pause?group_id=${groupId}`),
    onSuccess: (_data, groupId) => qc.invalidateQueries({ queryKey: blockingKeys.status(groupId) }),
  });
}
