import { http, HttpResponse } from "msw";
import type {
  HealthStatus,
  List,
  MeResponse,
  QueryEntry,
  Rule,
  SetupState,
  Settings,
  StatsOverview,
  TimelineBucket,
  TopEntry,
} from "../api/types";
import type { BlockingStatus } from "../hooks/use-blocking";

// Default "happy path" fixture for /stats/timeline — a handful of hourly
// buckets ending now, each with a plausible decision mix. Real enough that
// the dashboard's chart/percentage math has something non-trivial to chew
// on by default; tests that care about specific numbers override via
// server.use().
function defaultTimeline(): TimelineBucket[] {
  const nowSec = Math.floor(Date.now() / 1000);
  const hourSec = 3600;
  return [4, 3, 2, 1, 0].map((hoursAgo) => ({
    bucket: nowSec - hoursAgo * hourSec,
    decisions: { allowed: 120, blocked: 30, cached: 90, forwarded: 40, stale: 5 },
  }));
}

export const handlers = [
  http.get("/api/v1/auth/me", () => {
    const me: MeResponse = { id: 1, username: "admin", totp_enabled: false };
    return HttpResponse.json(me);
  }),

  http.get("/api/v1/setup", () => {
    const setupState: SetupState = { setup_required: false };
    return HttpResponse.json(setupState);
  }),

  http.get("/api/v1/settings", () => {
    const settings: Settings = {};
    return HttpResponse.json(settings);
  }),

  http.get("/api/v1/health", () => {
    const health: HealthStatus = { status: "ok", version: "dev" };
    return HttpResponse.json(health);
  }),

  http.get("/api/v1/stats/overview", () => {
    const overview: StatsOverview = {
      total: 1000,
      blocked: 250,
      cached: 400,
      forwarded: 350,
      clients: 12,
    };
    return HttpResponse.json(overview);
  }),

  http.get("/api/v1/stats/timeline", () => HttpResponse.json(defaultTimeline())),

  http.get("/api/v1/stats/top", ({ request }) => {
    const metric = new URL(request.url).searchParams.get("metric");
    const byMetric: Record<string, TopEntry[]> = {
      domain: [
        { key: "example.com", count: 320 },
        { key: "api.github.com", count: 210 },
        { key: "cdn.example.net", count: 150 },
      ],
      blocked_domain: [
        { key: "ads.tracker.example", count: 140 },
        { key: "telemetry.example.io", count: 88 },
      ],
      client: [
        { key: "192.168.1.10", count: 480 },
        { key: "192.168.1.24", count: 310 },
      ],
    };
    return HttpResponse.json(byMetric[metric ?? ""] ?? []);
  }),

  http.get("/api/v1/filters/lists", () => {
    const lists: List[] = [
      {
        id: 1,
        url: "https://example.com/hosts",
        kind: "block",
        enabled: true,
        last_refreshed: Date.now() - 15 * 60 * 1000,
        entry_count: 85000,
      },
    ];
    return HttpResponse.json(lists);
  }),

  // Query log page (Task 8) — the "why?" drawer's rules lookup and the
  // paged search's default (empty, since most tests drive the page via
  // the live SSE tail instead and only need this endpoint to not 404).
  http.get("/api/v1/groups/:id/rules", () => {
    const rules: Rule[] = [];
    return HttpResponse.json(rules);
  }),

  http.get("/api/v1/queries", () => {
    const entries: QueryEntry[] = [];
    return HttpResponse.json(entries);
  }),

  // Global blocking-pause status (group 0) — the shell Header's pause
  // control (Task 9) polls this on every authenticated page, so it needs a
  // default "active" fixture even for tests that have nothing to do with
  // pausing (onUnhandledRequest is "error", see test/setup.ts).
  http.get("/api/v1/blocking", () => {
    const status: BlockingStatus = { paused_until: 0 };
    return HttpResponse.json(status);
  }),
];
