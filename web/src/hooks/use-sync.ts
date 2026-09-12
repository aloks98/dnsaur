import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { SyncStatus } from "../api/types";
import { settingsKeys, syncKeys, useResolverStatus } from "./use-settings";

// Config sync (spec §7, §8) — two dnsaur instances serving one network, one
// of which takes its configuration from the other. See docs/api.md's Sync
// entry for the endpoints and internal/api/sync_handlers.go for the shapes.

/** Same cadence as the blocking-pause poll (use-blocking.ts): a replica
 * checks its peer every `sync.interval_seconds` (30 by default), so asking
 * more often than that would only re-read the same numbers. */
const POLL_MS = 30_000;

/**
 * GET /sync/status — the only place the main half (the sync key, the
 * registered replicas) is readable, and the Settings Sync band is its only
 * reader.
 *
 * Polled flatly rather than through useResolverStatus' settle/trouble
 * pattern: there is no write on this screen whose effect lands
 * asynchronously a second or two later, and the facts here move on the
 * replica's schedule, not the operator's.
 */
export function useSyncStatus() {
  return useQuery({
    queryKey: syncKeys.status,
    queryFn: () => api.get<SyncStatus>("/sync/status"),
    refetchInterval: POLL_MS,
  });
}

/**
 * The peer this instance takes its configuration from, `""` when it takes
 * it from nobody.
 *
 * Read off `GET /resolver/status` rather than `/sync/status` because the
 * shell already holds that query for its banners (see serving-banners.tsx),
 * so every screen that has to know whether its write controls are live gets
 * the answer without a round trip of its own.
 *
 * An unanswered status reads as "not managed": the server refuses a synced
 * write with 409 regardless, so guessing wrong here costs one error toast,
 * while guessing the other way would grey out a main's whole screen every
 * time the endpoint was slow.
 */
export function useManagedBy(): string {
  const status = useResolverStatus();
  const sync = status.data?.sync;
  return sync?.role === "replica" ? (sync.peer_url ?? "") : "";
}

/**
 * DELETE /sync/replicas/{instance_id} — the operator's action, since a
 * replica that stopped pulling is shown as stale and never removed
 * automatically. It takes the registered address back out of the implicit
 * transfer allow, so both the sync status and the resolver status that
 * carries a copy of it have to be re-read.
 */
export function useForgetReplica() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) =>
      api.del<void>(`/sync/replicas/${encodeURIComponent(instanceId)}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: syncKeys.status });
      void qc.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
    },
  });
}

/**
 * Promotion: clear `sync.peer_url` and `sync.token` together, which is the
 * one write that turns a replica back into a main (spec §7).
 *
 * A settings *map*, not the two per-key writes settings.tsx makes
 * everywhere else, and deliberately so: the server refuses a token cleared
 * under a configured peer and a peer set with no token, so the pair is only
 * valid as one all-or-nothing request — which is also what makes the guard
 * lift exactly once instead of leaving a window where half of it has.
 *
 * Invalidates the settings and both statuses — the three reads that decide
 * what this page shows and whether every other screen's write controls are
 * live.
 */
export function useStopFollowing() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.put<void>("/settings", { "sync.peer_url": "", "sync.token": "" }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: syncKeys.status });
      void qc.invalidateQueries({ queryKey: settingsKeys.all });
      void qc.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
    },
  });
}
