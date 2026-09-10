import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

// Same live-feel cadence as the dashboard's stats polling (see
// use-stats.ts) — cheap read, and a paused-until banner that silently goes
// stale would be actively misleading.
const POLL_MS = 30_000;

const GLOBAL_GROUP_ID = 0;

/**
 * Which pause a control is for: one group, one device, or — with neither id
 * — every query the server answers. Groups and clients are numbered from 1,
 * so `0` is the server's spelling of "not given"; sending both is a 400.
 */
export interface BlockingScope {
  groupId?: number;
  clientId?: number;
}

function scopeParams({ groupId = GLOBAL_GROUP_ID, clientId = 0 }: BlockingScope) {
  return clientId ? `client_id=${clientId}` : `group_id=${groupId}`;
}

export const blockingKeys = {
  /** Every blocking status, for invalidating after a pause of any scope: a
   * global pause changes what a group row and a client row read too. */
  all: ["blocking"] as const,
  status: (groupId: number, clientId = 0) => ["blocking", groupId, clientId] as const,
};

export interface BlockingStatus {
  /** Unix ms; `0` means blocking is active (not paused). */
  paused_until: number;
  /**
   * Which of the three pauses is the one in force — absent when none is.
   * A control can only resume its own scope, so a client row showing a
   * countdown its group set has to be able to say so.
   */
  scope?: "global" | "group" | "client";
}

export function useBlockingStatus(scope: BlockingScope = {}) {
  return useQuery({
    queryKey: blockingKeys.status(scope.groupId ?? GLOBAL_GROUP_ID, scope.clientId ?? 0),
    queryFn: () => api.get<BlockingStatus>(`/blocking?${scopeParams(scope)}`),
    refetchInterval: POLL_MS,
  });
}

export function usePauseBlocking() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      groupId = GLOBAL_GROUP_ID,
      clientId = 0,
      minutes,
    }: BlockingScope & { minutes: number }) =>
      api.post<void>("/blocking/pause", {
        group_id: clientId ? 0 : groupId,
        client_id: clientId,
        minutes,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: blockingKeys.all }),
  });
}

export function useResumeBlocking() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (scope: BlockingScope) => api.del<void>(`/blocking/pause?${scopeParams(scope)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: blockingKeys.all }),
  });
}
