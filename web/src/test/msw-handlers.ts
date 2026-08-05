import { http, HttpResponse } from "msw";
import type {
  Client,
  Group,
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
//
// Buckets are unix-*seconds* hour starts, exactly as the real handler emits
// them (`strftime('%s', ..., 'start of hour')` — see
// internal/api/queries_handlers.go). They used to be `now - n*3600`, which
// is only hour-*spaced*, not hour-*aligned*; that was invisible while the
// chart plotted rows in arrival order, and became a silent all-zeroes chart
// the moment the dashboard started keying missing hours by their start.
export const HOUR_SEC = 3600;

export function hourStart(hoursAgo = 0): number {
  return Math.floor(Date.now() / 1000 / HOUR_SEC) * HOUR_SEC - hoursAgo * HOUR_SEC;
}

function defaultTimeline(): TimelineBucket[] {
  return [4, 3, 2, 1, 0].map((hoursAgo) => ({
    bucket: hourStart(hoursAgo),
    decisions: { allowed: 120, blocked: 30, cached: 90, forwarded: 40, stale: 5 },
  }));
}

/**
 * Blocking-pause status, scoped per group exactly like the real handler
 * (internal/api/settings_handlers.go reads `group_id` and asks the engine
 * for *that* group's pause). The shell Header polls the global scope
 * (group 0) on every authenticated page, while the Filtering page mounts
 * one PauseControl per group — a group-blind fixture would let a control
 * that reads the wrong scope pass. Pass a map to make specific groups
 * paused: `server.use(blockingHandler({ 3: Date.now() + 60_000 }))`.
 */
export function blockingHandler(pausedUntilByGroup: Record<number, number> = {}) {
  return http.get("/api/v1/blocking", ({ request }) => {
    const groupId = Number(new URL(request.url).searchParams.get("group_id") ?? 0);
    const status: BlockingStatus = { paused_until: pausedUntilByGroup[groupId] ?? 0 };
    return HttpResponse.json(status);
  });
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
    // blocked + cached + forwarded is deliberately *less* than total: the
    // real handler (internal/api/queries_handlers.go) sums every decision
    // into total, including allowed/error/local, and reports only three of
    // them individually. A fixture where the three add up exactly would
    // green-light percentage math that can't hold on a real instance.
    const overview: StatsOverview = {
      total: 1000,
      blocked: 250,
      cached: 400,
      forwarded: 250,
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

  // Groups and clients. The query log resolves each row's group through its
  // client (see pages/queries.tsx), and the dashboard's quick rules need to
  // know whether more than one group exists — both are default-handled here
  // because onUnhandledRequest is "error" (see test/setup.ts).
  http.get("/api/v1/groups", () => {
    const groups: Group[] = [{ id: 1, name: "default", enabled: true }];
    return HttpResponse.json(groups);
  }),

  http.get("/api/v1/clients", () => {
    const clients: Client[] = [{ id: 1, name: "Laptop", matcher: "192.168.1.10", group_id: 1 }];
    return HttpResponse.json(clients);
  }),

  blockingHandler(),
];
