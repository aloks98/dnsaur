import { useMutation, useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import type { HealthStatus, StatsOverview, TimelineBucket, TopEntry } from "../api/types";

export type TopMetric = "domain" | "blocked_domain" | "client";

// Live-feel polling for the dashboard's stats — cheap reads, and nobody
// wants to manually refresh an "is my resolver healthy" screen.
const LIVE_REFETCH_MS = 30_000;

export const statsKeys = {
  overview: (hours: number) => ["stats", "overview", hours] as const,
  timeline: (hours: number) => ["stats", "timeline", hours] as const,
  top: (metric: TopMetric, n: number, hours: number) => ["stats", "top", metric, n, hours] as const,
};

export function useStatsOverview(hours: number) {
  return useQuery({
    queryKey: statsKeys.overview(hours),
    queryFn: () => api.get<StatsOverview>(`/stats/overview?hours=${hours}`),
    refetchInterval: LIVE_REFETCH_MS,
  });
}

export function useStatsTimeline(hours: number) {
  return useQuery({
    queryKey: statsKeys.timeline(hours),
    queryFn: () => api.get<TimelineBucket[]>(`/stats/timeline?hours=${hours}`),
    refetchInterval: LIVE_REFETCH_MS,
  });
}

export function useStatsTop(metric: TopMetric, n: number, hours: number) {
  return useQuery({
    queryKey: statsKeys.top(metric, n, hours),
    queryFn: () => api.get<TopEntry[]>(`/stats/top?metric=${metric}&n=${n}&hours=${hours}`),
    refetchInterval: LIVE_REFETCH_MS,
  });
}

// GET /health is unauthenticated liveness/version — the cheapest possible
// "is the instance up" signal. Used by both the dashboard's health strip
// and the sidebar's resolver status LED (see sidebar-nav.tsx), so it lives
// here rather than being private to one page.
export function useHealth() {
  return useQuery({
    queryKey: ["health"] as const,
    queryFn: () => api.get<HealthStatus>("/health"),
    refetchInterval: LIVE_REFETCH_MS,
    retry: false,
  });
}

const QUICK_RULE_GROUP_ID = 1;

/**
 * Local stand-in for the dashboard's per-domain quick block/allow action.
 * Task 9 owns the real shared rule hooks (in use-filters.ts, with proper
 * rule-list query invalidation for the Filtering page) — this only calls
 * the same POST /groups/:id/rules endpoint so the dashboard has something
 * to wire its action to in the meantime. Supersede this with Task 9's hook
 * once it lands; nothing on this page reads the rules list itself, so
 * there's no cache to invalidate here.
 */
export function useAddRule() {
  return useMutation({
    mutationFn: (v: { action: "allow" | "block"; pattern: string }) =>
      api.post<{ id: number }>(`/groups/${QUICK_RULE_GROUP_ID}/rules`, {
        action: v.action,
        pattern: v.pattern,
      }),
  });
}
