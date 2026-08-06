import { memo, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { CircleAlert } from "lucide-react";
import { Link, useSearchParams } from "react-router";
import { toast } from "sonner";
import type { UseQueryResult } from "@tanstack/react-query";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  BarChart,
  cn,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Skeleton,
} from "@e412/rnui-react";
import type { Group, QueryEntry, StatsOverview, TimelineBucket, TopEntry } from "../api/types";
import { useAddRule } from "../hooks/use-filters";
import { useGroups } from "../hooks/use-groups";
import { useClients } from "../hooks/use-clients";
import { useLiveTail } from "../hooks/use-queries";
import { useStatsOverview, useStatsTimeline, useStatsTop } from "../hooks/use-stats";
import { durationLabel, rowKey } from "../lib/query-rows";
import { hoursFor, parseWindow, WINDOW_PARAM, windowPhrase } from "../lib/stats-window";
import { useTheme, type Theme } from "../lib/theme";
import { StaleDataAlert } from "../components/stale-data-alert";

// The group every quick rule targets unless the user picks another one.
// Group 1 is the structural default (see hooks/use-groups.ts) and the group
// unmatched clients resolve to (internal/clients/registry.go).
const DEFAULT_GROUP_ID = 1;

/** Rows of live tail the panel shows. The hook keeps 500; this is what
 * fits the split without turning the page into a second query log. */
const LIVE_ROWS = 12;

/** Sizes of the two right-rail panels, per the design. */
const TOP_BLOCKED_N = 6;
const TOP_CLIENTS_N = 5;

// --- small formatting helpers ----------------------------------------------

/** blocked/total as a whole-number percent string; "—" (never NaN) when
 * there's no data yet. */
function pct(numerator: number, denominator: number): string {
  if (denominator <= 0) return "—";
  return `${Math.round((numerator / denominator) * 100)}%`;
}

function clockTime(atMs: number): string {
  return new Date(atMs).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/**
 * Per-decision tint, shared by the live rows.
 *
 * The seven-way decision vocabulary is the resolver's (see
 * internal/dnssrv/pipeline.go); this is the design's flat, text-only reading
 * of it, not the query log's badge-and-icon treatment — twelve badges in a
 * 340px-adjacent column would be the loudest thing on the page.
 *
 * There is no `allowed` entry on purpose: the pipeline never writes that
 * decision, so a row can't carry it.
 */
const DECISION_TONE: Record<string, string> = {
  blocked: "text-destructive",
  // A failed resolve is a broken query, not a policy decision — but on a
  // one-line readout it needs the same "look at me" weight as a block.
  error: "text-destructive",
  stale: "text-warn",
  cached: "text-muted-foreground",
  forwarded: "text-foreground",
  local: "text-primary",
};

function decisionTone(decision: string): string {
  return DECISION_TONE[decision] ?? "text-muted-foreground";
}

// --- section chrome ----------------------------------------------------------
// Every section is a hairline-bordered band, never a card: one 1px rule
// between bands, one between cells, and nothing else. These three keep that
// vocabulary in one place instead of restating the same class list a dozen
// times.

function SectionTitle({ id, children }: { id?: string; children: ReactNode }) {
  return (
    <h2 id={id} className="text-sm tracking-widest uppercase">
      {children}
    </h2>
  );
}

function Note({ className, children }: { className?: string; children: ReactNode }) {
  return (
    <span className={cn("text-xs tracking-widest text-muted-foreground uppercase", className)}>
      {children}
    </span>
  );
}

/** The `ALL →` affordance on each rail panel. */
function AllLink({ to, label }: { to: string; label: string }) {
  return (
    <Link
      to={to}
      aria-label={label}
      className={cn(
        "text-xs tracking-widest text-muted-foreground uppercase transition-colors",
        "hover:text-foreground",
        "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
      )}
    >
      All <span aria-hidden="true">→</span>
    </Link>
  );
}

/** Prose (alerts, empty-state sentences) is the one thing on this page that
 * isn't mono — and it needs the page's own horizontal gutter back, since
 * the bands themselves are full-bleed. */
function Prose({ children }: { children: ReactNode }) {
  return <div className="px-4 py-3 font-sans">{children}</div>;
}

// --- 1. stat strip -----------------------------------------------------------

function StatCell({
  label,
  value,
  tone,
}: {
  label: string;
  value: string;
  /** Only BLOCKED is tinted — the design's one coloured numeral. */
  tone?: string;
}) {
  return (
    <div className="flex flex-col gap-2 border-r border-border px-4 py-3 last:border-r-0">
      <span className="text-sm tracking-widest text-muted-foreground uppercase">{label}</span>
      <span className={cn("text-3xl font-semibold tabular-nums", tone)}>{value}</span>
    </div>
  );
}

function StatStrip({
  overview,
  hours,
}: {
  overview: UseQueryResult<StatsOverview, Error>;
  hours: number;
}) {
  if (overview.isPending) {
    return (
      <div className="grid grid-cols-2 border-b border-border sm:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div
            key={i}
            className="flex flex-col gap-2 border-r border-border px-4 py-3 last:border-r-0"
          >
            <Skeleton className="h-3 w-24" />
            <Skeleton className="h-9 w-20" />
          </div>
        ))}
      </div>
    );
  }

  // isPending is already ruled out, so no data means the *first* fetch failed
  // and there are genuinely no numbers to show.
  if (overview.data === undefined) {
    return (
      <div className="border-b border-border">
        <Prose>
          <Alert variant="destructive">
            <CircleAlert />
            <AlertTitle>Couldn&apos;t load stats</AlertTitle>
            <AlertDescription>Try refreshing the page.</AlertDescription>
          </Alert>
        </Prose>
      </div>
    );
  }

  const { total, blocked, cached, clients } = overview.data;

  return (
    // The window is named once, on the region, rather than on all four
    // labels: every number in the strip answers for the same span, and
    // "TOTAL QUERIES IN THE LAST 24 HOURS" is not a KPI label.
    <section aria-label={`Query stats ${windowPhrase(hours)}`} className="border-b border-border">
      {overview.isError && (
        <Prose>
          <StaleDataAlert
            what="stats"
            onRetry={() => void overview.refetch()}
            isRetrying={overview.isFetching}
          />
        </Prose>
      )}
      <div className="grid grid-cols-2 sm:grid-cols-4">
        <StatCell label="Queries" value={total.toLocaleString()} />
        <StatCell label="Blocked" value={pct(blocked, total)} tone="text-chart-blocked" />
        <StatCell label="Cache" value={pct(cached, total)} />
        <StatCell label="Clients" value={clients.toLocaleString()} />
      </div>
    </section>
  );
}

// --- 2. query volume ---------------------------------------------------------

// Canvas2D cannot resolve CSS custom properties, and every colour handed to a
// chart ends up in zrender's canvas painter — for gradients, straight into
// CanvasGradient.addColorStop, where a real browser answers "var(--chart-1)"
// with `SyntaxError: ... could not be parsed as a color`. That throws from a
// layout effect, which React escalates to the nearest ErrorBoundary,
// replacing the whole dashboard with "Something went wrong". An empty
// timeline draws no chart at all, so a fresh instance looked healthy and only
// started crashing once it had served its first query. The tokens' *values*
// are plain oklch(), which canvas accepts; they just have to be resolved
// before they get there. test/setup.ts rejects the same values a browser
// does, so this stays a real guard rather than a comment.
const CHART_TOKENS = {
  blocked: "--chart-blocked",
  resolved: "--chart-resolved",
  gridline: "--border-muted",
  axis: "--muted-foreground",
} as const;

// Only reached when no stylesheet has been applied (jsdom under test, an SSR
// pass): getPropertyValue answers "" for an unknown token, and "" is no more
// paintable than var(). Mirrors styles/dnsaur-theme.css's own two blocks, so
// even the fallback path stays theme-correct.
const CHART_FALLBACKS: Record<Theme, Record<keyof typeof CHART_TOKENS, string>> = {
  light: {
    blocked: "oklch(0.2622 0.0446 157.77)",
    resolved: "oklch(0.9082 0.0983 157.22)",
    gridline: "oklch(0.9507 0.0108 158.84)",
    axis: "oklch(0.511 0.0259 155.36)",
  },
  dark: {
    blocked: "oklch(0.8014 0.1844 155.96)",
    resolved: "oklch(0.2826 0.0101 151.46)",
    gridline: "oklch(0.2138 0.006 156.68)",
    axis: "oklch(0.6441 0.0137 156.83)",
  },
};

function readColorToken(token: string, fallback: string): string {
  if (typeof document === "undefined") return fallback;
  const value = getComputedStyle(document.documentElement).getPropertyValue(token).trim();
  // A token defined in terms of another var() is exactly as unpaintable as
  // the reference we're trying to get rid of.
  return value === "" || value.includes("var(") ? fallback : value;
}

/**
 * Resolved, canvas-safe chart colours, re-read on every theme change: light
 * and dark define different values for all four tokens, so one read at mount
 * would leave the chart painted in the previous theme after a toggle. The
 * theme store applies the theme to <html> *before* notifying subscribers
 * (lib/theme.ts), so by the time this recomputes getComputedStyle already
 * reports the new values.
 */
type ChartColors = Record<keyof typeof CHART_TOKENS, string>;

function useChartColors(): ChartColors {
  const { theme } = useTheme();
  return useMemo(() => {
    const fallback = CHART_FALLBACKS[theme];
    return {
      blocked: readColorToken(CHART_TOKENS.blocked, fallback.blocked),
      resolved: readColorToken(CHART_TOKENS.resolved, fallback.resolved),
      gridline: readColorToken(CHART_TOKENS.gridline, fallback.gridline),
      axis: readColorToken(CHART_TOKENS.axis, fallback.axis),
    };
  }, [theme]);
}

const HOUR_SEC = 3600;

/** Safety valve on the synthesised axis, nothing more: a bucket from a
 * wildly wrong clock must not stretch it to thousands of empty hours. The
 * widest window the UI offers is 7d — 169 hourly buckets — so this never
 * trips on data the API can actually return. */
const MAX_BUCKETS = 24 * 8 + 1;

function bucketLabel(bucketSec: number, hours: number): string {
  const date = new Date(bucketSec * 1000);
  if (hours <= 24) {
    return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}

/**
 * Timeline buckets → two continuous stacked series.
 *
 * `GET /stats/timeline` returns one entry per hour that had traffic, keyed by
 * a unix-**seconds** hour start, with only the non-zero decisions present —
 * there is no zero-filling on the server. Plotting the rows as they arrive
 * therefore silently *compresses* quiet hours out of the axis: a resolver
 * that served nothing overnight drew 23:00 immediately next to 07:00, at
 * equal width, which reads as continuous traffic. So the missing hours are
 * synthesised here as real zeroes across the whole window, and the section
 * says so in as many words under the chart.
 *
 * "Resolved" is every non-`blocked` decision — including `error`. A failed
 * resolve isn't a query dnsaur let through, but it is one it *didn't block*,
 * and the two bands have to add up to the same `total` the strip above
 * reports (internal/api/queries_handlers.go sums every decision into it).
 */
function timelineSeries(buckets: TimelineBucket[], hours: number) {
  const byBucket = new Map<number, Record<string, number>>();
  for (const b of buckets) byBucket.set(b.bucket, b.decisions);
  const present = [...byBucket.keys()];

  const nowSec = Math.floor(Date.now() / 1000);
  const lastHour = Math.floor(nowSec / HOUR_SEC) * HOUR_SEC;
  const firstHour = Math.floor((nowSec - hours * HOUR_SEC) / HOUR_SEC) * HOUR_SEC;

  // The axis spans the requested window *and* every bucket the API actually
  // handed over — dropping a returned bucket to keep a tidy axis would be
  // losing real traffic to make a picture neater.
  const end = Math.max(lastHour, ...present);
  const start = Math.max(Math.min(firstHour, ...present), end - (MAX_BUCKETS - 1) * HOUR_SEC);

  const categories: string[] = [];
  const resolved: number[] = [];
  const blocked: number[] = [];
  for (let t = start; t <= end; t += HOUR_SEC) {
    const decisions = byBucket.get(t);
    const total = decisions ? Object.values(decisions).reduce((sum, n) => sum + n, 0) : 0;
    const blockedCount = decisions?.blocked ?? 0;
    categories.push(bucketLabel(t, hours));
    blocked.push(blockedCount);
    resolved.push(total - blockedCount);
  }
  return { categories, resolved, blocked };
}

function Swatch({ tone, label }: { tone: string; label: string }) {
  return (
    <span className="flex items-center gap-1.5">
      <span aria-hidden="true" className={cn("size-2 shrink-0", tone)} />
      <Note>{label}</Note>
    </span>
  );
}

/**
 * The plot itself, isolated and memoised.
 *
 * The dashboard around this re-renders often and for reasons that have
 * nothing to do with the chart — the live tail commits a batch up to ten
 * times a second, the stats poll lands every 30s. rnui's EChart re-applies
 * its option whenever the object identity changes, and re-applying an
 * option tears down ECharts' hover state: the first hover after a re-render
 * was being eaten, so the tooltip only appeared if you hovered again in the
 * gap between two commits.
 *
 * So: `memo` on value-equal props (two number arrays and a string array, all
 * memoised by the caller; `colors` is memoised on the theme), and `useMemo`
 * on `series` and `option` so their identities survive any re-render that
 * doesn't actually change the picture. The live tail also owns its own state
 * now (see LiveQueries) rather than pushing it through this component's
 * parent, so idle traffic doesn't reach here at all.
 */
const VolumeChart = memo(function VolumeChart({
  categories,
  resolved,
  blocked,
  colors,
}: {
  categories: string[];
  resolved: number[];
  blocked: number[];
  colors: ChartColors;
}) {
  // Passed explicitly rather than left to BarChart's default series, which
  // sets itemStyle.borderRadius to [4,4,0,0]. The design has no rounded
  // anything (--radius is 0 app-wide), and a rounded bar cap is especially
  // wrong on the *lower* half of a stack, where it notches the segment above.
  const series = useMemo(
    () => [
      {
        name: "Resolved",
        type: "bar" as const,
        stack: "volume",
        data: resolved,
        barMaxWidth: 28,
        itemStyle: { borderRadius: 0, color: colors.resolved },
      },
      {
        name: "Blocked",
        type: "bar" as const,
        stack: "volume",
        data: blocked,
        barMaxWidth: 28,
        itemStyle: { borderRadius: 0, color: colors.blocked },
      },
    ],
    [resolved, blocked, colors],
  );

  const option = useMemo(
    () => ({
      grid: { left: 0, right: 0, top: 8, bottom: 0, containLabel: true },
      xAxis: {
        type: "category" as const,
        data: categories,
        axisTick: { show: false },
        axisLine: { lineStyle: { color: colors.gridline } },
        axisLabel: { color: colors.axis, fontSize: 10, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: {
        type: "value" as const,
        axisTick: { show: false },
        axisLine: { show: false },
        axisLabel: { color: colors.axis, fontSize: 10 },
        // The design's chrome: hairline gridlines only, in the muted border
        // tone, so the bars are the only ink with any weight.
        splitLine: { lineStyle: { color: colors.gridline, type: "solid" as const } },
      },
      tooltip: { trigger: "axis" as const },
    }),
    [categories, colors],
  );

  return (
    // The plot needs a real pixel height — ECharts measures its container —
    // and PlotFrame is where that height is decided, so the chart fills it.
    <div className="h-full">
      <BarChart
        // `categories` + `series` supersede it; BarChart's prop type still
        // requires the field.
        data={[]}
        categories={categories}
        stacked
        height="100%"
        series={series}
        option={option}
      />
    </div>
  );
});

/**
 * The box every state of this section renders into.
 *
 * A fresh instance sits with no traffic in the window for its first hour,
 * and the stats tables lag the query log by up to a minute — so the
 * loading → empty → populated sequence is *guaranteed* to be watched, not a
 * corner case. Letting the empty message collapse to one line of text
 * shunted LIVE QUERIES, the whole right rail and the bottom rule ~200px up
 * the page and then dropped them back the moment the first bucket landed.
 * So the height is the section's, not the chart's: skeleton, error, empty
 * and plot all fill the same 16rem.
 */
function PlotFrame({ children }: { children: ReactNode }) {
  return (
    <div className="px-4 pb-3">
      {/* Named so the height reservation is assertable: under jsdom there is
          no layout to measure, so the test asserts that every state really
          does render into this one box. */}
      <div data-slot="query-volume-plot" className="h-64">
        {children}
      </div>
    </div>
  );
}

/** Turns buckets into series once, and wraps the plot in its text
 * alternative. Split out so the memoised chart's array props are computed
 * in a hook rather than inline in a branch. */
function VolumeBody({ buckets, hours }: { buckets: TimelineBucket[]; hours: number }) {
  const colors = useChartColors();
  const { categories, resolved, blocked } = useMemo(
    () => timelineSeries(buckets, hours),
    [buckets, hours],
  );
  const totalResolved = resolved.reduce((s, n) => s + n, 0);
  const totalBlocked = blocked.reduce((s, n) => s + n, 0);

  return (
    <figure
      className="h-full"
      aria-label={`Query volume over ${categories.length} hourly buckets: ${(totalResolved + totalBlocked).toLocaleString()} queries — ${totalResolved.toLocaleString()} resolved, ${totalBlocked.toLocaleString()} blocked`}
    >
      <VolumeChart categories={categories} resolved={resolved} blocked={blocked} colors={colors} />
    </figure>
  );
}

function QueryVolume({
  timeline,
  hours,
}: {
  timeline: UseQueryResult<TimelineBucket[], Error>;
  hours: number;
}) {
  let body: ReactNode;
  if (timeline.isPending) {
    body = <Skeleton className="h-full w-full" />;
  } else if (timeline.data === undefined) {
    // isPending is already ruled out, so no data means the *first* load
    // failed — there is genuinely nothing to draw. A background poll that
    // fails with buckets still cached takes the StaleDataAlert path below.
    body = (
      <div className="flex h-full flex-col justify-center font-sans">
        <Alert variant="destructive">
          <CircleAlert />
          <AlertTitle>Couldn&apos;t load the timeline</AlertTitle>
        </Alert>
      </div>
    );
  } else if (timeline.data.length === 0) {
    // Deliberately not a zero-filled flat chart: an axis of empty hours
    // claims dnsaur was up and quiet, which on a fresh instance is a guess.
    body = (
      <div className="flex h-full items-center justify-center font-sans">
        <p className="text-sm text-muted-foreground">
          No query activity yet. Once dnsaur resolves queries in this window, blocked and resolved
          volume shows up here.
        </p>
      </div>
    );
  } else {
    body = <VolumeBody buckets={timeline.data} hours={hours} />;
  }

  return (
    <section aria-labelledby="query-volume-title" className="border-b border-border">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3">
        <SectionTitle id="query-volume-title">Query volume</SectionTitle>
        {/* The design says "15-min"; /stats/timeline only ever produces hour
            buckets, so the label says what the data actually is. */}
        <Note>hourly buckets</Note>
        <div className="ml-auto flex items-center gap-4">
          <Swatch tone="bg-chart-blocked" label="Blocked" />
          <Swatch tone="bg-chart-resolved" label="Resolved" />
        </div>
      </div>
      {timeline.isError && timeline.data !== undefined && (
        <Prose>
          <StaleDataAlert
            what="the timeline"
            onRetry={() => void timeline.refetch()}
            isRetrying={timeline.isFetching}
          />
        </Prose>
      )}
      <PlotFrame>{body}</PlotFrame>
    </section>
  );
}

// --- 3a. live queries --------------------------------------------------------

const RATE_WINDOW_MS = 30_000;
// Nothing is shown before this much of the window has actually been
// observed: "0.3 q/s" computed over 900ms of uptime is a number, not a
// measurement, and it would be the twitchiest thing on the page.
const RATE_MIN_OBSERVED_MS = 10_000;
const RATE_TICK_MS = 1_000;

/**
 * Arrivals per second, measured — there is no endpoint for this.
 *
 * The live tail is the only honest source: count how many rows actually
 * showed up, over how long we've actually been listening. Server-side
 * timestamps can't be used instead, because the buffer is capped at 500 rows
 * and a busy resolver would make "the oldest row I still hold" an ever
 * shorter, ever wronger window.
 *
 * The 1s tick is what lets the number fall back to zero when the stream goes
 * quiet — without it the readout would freeze at whatever the last burst
 * measured. It only re-renders this readout, not the rows beside it.
 */
function useArrivalRate(entries: QueryEntry[]): number | undefined {
  const observedFrom = useRef(Date.now());
  const lastSeenId = useRef<number | undefined>(undefined);
  const arrivals = useRef<{ at: number; n: number }[]>([]);
  const [rate, setRate] = useState<number | undefined>(undefined);

  useEffect(() => {
    // `entries` is newest-first and only ever grows at the front, so the run
    // of ids above the last one we counted is exactly what's new. Length
    // can't be used: it saturates at the buffer cap.
    let added = 0;
    for (const entry of entries) {
      if (lastSeenId.current !== undefined && entry.id <= lastSeenId.current) break;
      added += 1;
    }
    const newest = entries[0];
    if (newest !== undefined) lastSeenId.current = newest.id;
    if (added > 0) arrivals.current.push({ at: Date.now(), n: added });
  }, [entries]);

  useEffect(() => {
    const timer = setInterval(() => {
      const now = Date.now();
      arrivals.current = arrivals.current.filter((a) => a.at >= now - RATE_WINDOW_MS);
      const observed = now - observedFrom.current;
      if (observed < RATE_MIN_OBSERVED_MS) {
        setRate(undefined);
        return;
      }
      const counted = arrivals.current.reduce((sum, a) => sum + a.n, 0);
      setRate(counted / (Math.min(observed, RATE_WINDOW_MS) / 1000));
    }, RATE_TICK_MS);
    return () => clearInterval(timer);
  }, []);

  return rate;
}

function RateReadout({ entries }: { entries: QueryEntry[] }) {
  const rate = useArrivalRate(entries);
  if (rate === undefined) return null;
  return <Note>{rate.toFixed(1)} q/s</Note>;
}

function LiveQueries({
  quickRule,
  ruleStatus,
  quickRuleDisabled,
  quickRuleDisabledReason,
}: {
  quickRule: (action: "allow" | "block", pattern: string) => void;
  ruleStatus: Record<string, "pending" | "done">;
  quickRuleDisabled: boolean;
  quickRuleDisabledReason: string;
}) {
  // The same stream the query log uses — one SSE client for the app, with
  // its coalescing, its 500-row buffer and its backoff (hooks/use-queries.ts).
  //
  // Subscribed *here* rather than in the page: the tail commits a batch up to
  // ten times a second, and holding it a level higher re-rendered the stat
  // strip and the chart at that rate for no reason — which is what was eating
  // the chart's first-hover tooltip. Now idle traffic only touches this panel.
  const { entries, state } = useLiveTail(true);
  const isStreaming = state === "open" || state === "reconnecting";
  const rows = entries.slice(0, LIVE_ROWS);

  return (
    <section
      aria-labelledby="live-queries-title"
      className="min-w-0 flex-1 lg:border-r lg:border-border"
    >
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-3">
        <span
          aria-hidden="true"
          className={cn(
            "size-1.5 shrink-0 rounded-full",
            isStreaming ? "bg-primary" : "bg-muted-foreground",
          )}
        />
        <SectionTitle id="live-queries-title">Live queries</SectionTitle>
        <RateReadout entries={entries} />
        {/* No filters exist on this panel — that's what the query log is
            for — so the summary says what the stream actually is rather
            than pretending to be a control. */}
        <Note className="ml-auto">All clients · all decisions</Note>
      </div>

      {rows.length === 0 ? (
        <Prose>
          <p className="text-sm text-muted-foreground">
            {isStreaming
              ? "Listening — queries appear here as dnsaur answers them."
              : "The live stream isn't connected. The query log has the full history."}
          </p>
        </Prose>
      ) : (
        <table className="w-full table-fixed">
          <caption className="sr-only">
            The most recent {rows.length} queries dnsaur answered
          </caption>
          <thead>
            <tr className="border-b border-border text-sm tracking-widest text-muted-foreground uppercase">
              <th scope="col" className="w-24 px-4 py-2 text-left font-normal">
                Time
              </th>
              <th scope="col" className="px-4 py-2 text-left font-normal">
                Domain
              </th>
              <th scope="col" className="w-36 px-4 py-2 text-left font-normal">
                Client
              </th>
              <th scope="col" className="w-28 px-4 py-2 text-left font-normal">
                Decision
              </th>
              <th scope="col" className="w-24 px-4 py-2 text-right font-normal">
                Duration
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((entry) => (
              <tr
                key={rowKey(entry)}
                className="group/row border-b border-border-muted text-xs last:border-b-0"
              >
                <td className="px-4 py-2 whitespace-nowrap text-muted-foreground tabular-nums">
                  {clockTime(entry.at)}
                </td>
                <td className="truncate px-4 py-2">
                  <span className="flex items-center gap-2">
                    <span className="truncate">{entry.q_name}</span>
                    {/* The design has no per-row buttons; the capability
                        that used to live on a "top domains" table this
                        layout no longer has still has to go somewhere, so
                        it is revealed on hover and on keyboard focus rather
                        than deleted. */}
                    <QuickRuleAction
                      pattern={entry.q_name}
                      action={entry.decision === "blocked" ? "allow" : "block"}
                      status={ruleStatus[entry.q_name]}
                      disabled={quickRuleDisabled}
                      disabledReason={quickRuleDisabledReason}
                      onAction={quickRule}
                    />
                  </span>
                </td>
                <td className="truncate px-4 py-2 text-muted-foreground">{entry.client_ip}</td>
                <td className={cn("truncate px-4 py-2", decisionTone(entry.decision))}>
                  {entry.decision}
                </td>
                <td className="px-4 py-2 text-right text-muted-foreground tabular-nums">
                  {durationLabel(entry.duration_ms)}ms
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

/**
 * Quick block/allow, as a hover- and focus-revealed affordance.
 *
 * `opacity-0` rather than `hidden`/`invisible` on purpose: both of those
 * take the control out of the tab order, which would make this
 * mouse-only. Hidden-but-focusable plus `group-focus-within` means it
 * appears the moment a keyboard reaches it.
 */
function QuickRuleAction({
  pattern,
  action,
  status,
  disabled,
  disabledReason,
  onAction,
}: {
  pattern: string;
  action: "allow" | "block";
  status?: "pending" | "done";
  disabled: boolean;
  disabledReason: string;
  onAction: (action: "allow" | "block", pattern: string) => void;
}) {
  // Confirmation is never hover-gated: the user needs to see that it worked.
  if (status === "done") {
    return (
      <span className="shrink-0 text-xs tracking-widest text-primary uppercase">
        {action === "block" ? "Blocked" : "Allowed"}
      </span>
    );
  }

  return (
    <button
      type="button"
      disabled={status === "pending" || disabled}
      title={disabled ? disabledReason : undefined}
      onClick={() => onAction(action, pattern)}
      className={cn(
        "shrink-0 text-xs tracking-widest text-muted-foreground uppercase opacity-0",
        "transition-opacity hover:text-foreground",
        "group-hover/row:opacity-100 group-focus-within/row:opacity-100 focus-visible:opacity-100",
        // Still revealed while unavailable, just visibly inert and carrying
        // the reason in its title — silently having no affordance at all is
        // how "why can't I block from here?" starts.
        "disabled:cursor-not-allowed disabled:text-muted-foreground/50 disabled:hover:text-muted-foreground/50",
        "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
      )}
    >
      {action === "block" ? "Block" : "Allow"}
    </button>
  );
}

// --- 3b. the right rail ------------------------------------------------------

function RailPanel({
  title,
  allTo,
  allLabel,
  children,
  className,
}: {
  title: string;
  allTo: string;
  allLabel: string;
  children: ReactNode;
  className?: string;
}) {
  const titleId = `rail-${title.toLowerCase().replaceAll(" ", "-")}`;
  return (
    <section aria-labelledby={titleId} className={className}>
      <div className="flex items-center justify-between gap-2 border-b border-border px-3.5 py-3">
        <SectionTitle id={titleId}>{title}</SectionTitle>
        <AllLink to={allTo} label={allLabel} />
      </div>
      {children}
    </section>
  );
}

/**
 * One rail row: the label and count sit over an inline proportional bar
 * measuring that row's share of the *largest* value in its own panel.
 *
 * Absolutely positioned behind the text rather than beside it, because the
 * rail is 340px wide and a separate bar column would cost a third of it —
 * and because the ranking is the point: the shape falls away down the panel
 * whether or not you read a single number.
 */
function RailRow({
  label,
  count,
  share,
  countTone,
  action,
}: {
  label: string;
  count: number;
  share: number;
  countTone?: string;
  action?: ReactNode;
}) {
  return (
    <li className="group/row relative flex items-center gap-2 px-3.5 py-2 text-xs">
      <span
        aria-hidden="true"
        className="absolute inset-y-0 left-0 bg-muted/55"
        style={{ width: `${share}%` }}
      />
      <span className="relative min-w-0 flex-1 truncate">{label}</span>
      {action !== undefined && <span className="relative">{action}</span>}
      <span className={cn("relative shrink-0 tabular-nums", countTone)}>
        {count.toLocaleString()}
      </span>
    </li>
  );
}

/** Largest count in the panel — the bar denominator. Never 0 (a panel with
 * rows always has a positive maximum, but a defensive 1 keeps the width
 * finite if the API ever reports a zero count). */
function maxCount(entries: TopEntry[]): number {
  return Math.max(1, ...entries.map((e) => e.count));
}

function RailBody({
  query,
  emptyText,
  what,
  rows,
  reserve,
  children,
}: {
  query: UseQueryResult<TopEntry[], Error>;
  emptyText: string;
  what: string;
  /** How many rows this panel can hold — the skeleton draws that many. */
  rows: number;
  /**
   * The height that many rows occupy, so loading → empty → populated never
   * moves the panel below (or the rule under the rail). A RailRow is exactly
   * `py-2` plus a `text-xs` line box = 32px, i.e. 8 spacing units per row;
   * the class has to be a literal for Tailwind to see it, so the caller
   * spells it out next to its row count.
   */
  reserve: string;
  children: (entries: TopEntry[]) => ReactNode;
}) {
  if (query.isPending) {
    return (
      <div data-slot="rail-body" className={reserve} aria-hidden="true">
        {Array.from({ length: rows }).map((_, i) => (
          <div key={i} className="px-3.5 py-2">
            <Skeleton className="h-4 w-full" />
          </div>
        ))}
      </div>
    );
  }
  if (query.data === undefined) {
    // First load failed outright. A poll that fails with rows still cached
    // keeps them on screen under the StaleDataAlert below instead.
    return (
      <div
        data-slot="rail-body"
        className={cn(reserve, "px-3.5 py-3 font-sans text-sm text-muted-foreground")}
      >
        Couldn&apos;t load this list.
      </div>
    );
  }
  return (
    <>
      {query.isError && (
        <div className="px-3.5 py-3 font-sans">
          <StaleDataAlert
            what={what}
            onRetry={() => void query.refetch()}
            isRetrying={query.isFetching}
          />
        </div>
      )}
      <div data-slot="rail-body" className={reserve}>
        {query.data.length === 0 ? (
          <p className="px-3.5 py-3 font-sans text-sm text-muted-foreground">{emptyText}</p>
        ) : (
          <ul>{children(query.data)}</ul>
        )}
      </div>
    </>
  );
}

/**
 * `GET /stats/top?metric=client` answers with an IP and nothing else, so a
 * name has to come from the client registry — and only where the registry
 * can actually answer for that IP. A CIDR matcher ("192.168.1.0/24") covers
 * the address but does not identify the device, and labelling one host with
 * a subnet's name is worse than showing the address it really was.
 */
function useClientNames(): Map<string, string> {
  const clients = useClients();
  return useMemo(() => {
    const byIp = new Map<string, string>();
    for (const client of clients.data ?? []) {
      if (!client.matcher.includes("/")) byIp.set(client.matcher, client.name);
    }
    return byIp;
  }, [clients.data]);
}

// --- quick-rule group selector ----------------------------------------------
// The rail aggregates across every client, so — unlike the query log, where
// each row names a client and therefore a group — there is nothing here to
// resolve a group *from*. Writing silently into group 1 once the Filtering
// page lets users keep several groups meant a rule that reported success and
// did nothing for anyone outside the default. So the target is picked
// explicitly, and only surfaces once there is a choice to make (a
// single-group instance sees no extra control at all).

function RuleGroupStrip({
  groups,
  value,
  onChange,
}: {
  groups: Group[];
  value: number;
  onChange: (groupId: number) => void;
}) {
  const items = Object.fromEntries(groups.map((g) => [String(g.id), g.name]));
  return (
    <div className="flex flex-wrap items-center gap-3 border-t border-border px-4 py-3">
      <Note>Quick block/allow applies to</Note>
      <Select items={items} value={String(value)} onValueChange={(v) => onChange(Number(v))}>
        <SelectTrigger aria-label="Rule group" className="w-44">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {groups.map((g) => (
            <SelectItem key={g.id} value={String(g.id)}>
              {g.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}

// --- page --------------------------------------------------------------------

export function Dashboard() {
  const [params] = useSearchParams();
  const hours = hoursFor(parseWindow(params.get(WINDOW_PARAM)));

  const overview = useStatsOverview(hours);
  const timeline = useStatsTimeline(hours);
  const topBlocked = useStatsTop("blocked_domain", TOP_BLOCKED_N, hours);
  const topClients = useStatsTop("client", TOP_CLIENTS_N, hours);
  const clientNames = useClientNames();

  const groups = useGroups();
  const availableGroups = groups.data ?? [];
  const [ruleGroupId, setRuleGroupId] = useState(DEFAULT_GROUP_ID);
  // A group can be deleted from the Filtering page while this is mounted.
  const selectedGroup = availableGroups.find((g) => g.id === ruleGroupId);
  const effectiveGroupId = selectedGroup?.id ?? DEFAULT_GROUP_ID;
  // Without the group list there is no picker (it only renders once there's a
  // choice) and `effectiveGroupId` collapses to DEFAULT_GROUP_ID — so a click
  // in that window would write into group 1 and toast an unqualified "Blocked
  // example.com", reinstating exactly the silent default the picker exists to
  // remove. On a multi-group instance that rule lands somewhere the clicked
  // domain's clients may never be governed by. Refuse instead of guessing.
  const quickRuleDisabled = groups.isPending || groups.isError;
  const quickRuleDisabledReason = groups.isError
    ? "Couldn't load groups — reload to write rules from here"
    : "Loading groups…";

  const addRule = useAddRule();
  const [ruleStatus, setRuleStatus] = useState<Record<string, "pending" | "done">>({});

  function quickRule(action: "allow" | "block", pattern: string) {
    // Named in the toast only when there was a choice — on a single-group
    // instance "in default" is noise, not information.
    const scope = availableGroups.length > 1 && selectedGroup ? ` in ${selectedGroup.name}` : "";
    setRuleStatus((s) => ({ ...s, [pattern]: "pending" }));
    addRule.mutate(
      { groupId: effectiveGroupId, action, pattern },
      {
        onSuccess: () => {
          setRuleStatus((s) => ({ ...s, [pattern]: "done" }));
          toast.success(
            action === "block" ? `Blocked ${pattern}${scope}` : `Allowed ${pattern}${scope}`,
          );
        },
        onError: () => {
          setRuleStatus((s) => {
            const next = { ...s };
            delete next[pattern];
            return next;
          });
          toast.error(
            action === "block" ? `Couldn't block ${pattern}` : `Couldn't allow ${pattern}`,
          );
        },
      },
    );
  }

  return (
    // A column that claims the whole content area (the shell drops its
    // padding for this route — see components/app-shell.tsx), so the split
    // below can take every pixel left over and its vertical rule can run to
    // the bottom of the window however few rows there are.
    <div className="flex flex-1 flex-col font-mono">
      {/* The design gives the page no visible title — the chrome's own tab
          already says Dashboard — but a page still needs one heading. */}
      <h1 className="sr-only">Dashboard</h1>

      <StatStrip overview={overview} hours={hours} />
      <QueryVolume timeline={timeline} hours={hours} />

      <div className="flex flex-1 flex-col lg:flex-row lg:items-stretch">
        <LiveQueries
          quickRule={quickRule}
          ruleStatus={ruleStatus}
          quickRuleDisabled={quickRuleDisabled}
          quickRuleDisabledReason={quickRuleDisabledReason}
        />

        <div className="shrink-0 max-lg:border-t max-lg:border-border lg:w-80">
          <RailPanel
            title="Top blocked"
            allTo="/queries"
            allLabel="All blocked domains in the query log"
            className="border-b border-border"
          >
            <RailBody
              query={topBlocked}
              what="top blocked"
              emptyText="Nothing blocked in this window yet."
              rows={TOP_BLOCKED_N}
              reserve="min-h-48"
            >
              {(entries) => {
                const max = maxCount(entries);
                return entries.map((entry) => (
                  <RailRow
                    key={entry.key}
                    label={entry.key}
                    count={entry.count}
                    share={(entry.count / max) * 100}
                    countTone="text-chart-blocked"
                    action={
                      <QuickRuleAction
                        pattern={entry.key}
                        action="allow"
                        status={ruleStatus[entry.key]}
                        disabled={quickRuleDisabled}
                        disabledReason={quickRuleDisabledReason}
                        onAction={quickRule}
                      />
                    }
                  />
                ));
              }}
            </RailBody>
          </RailPanel>

          <RailPanel
            title="Top clients"
            allTo="/filtering/clients"
            allLabel="All clients in Groups & Clients"
          >
            <RailBody
              query={topClients}
              what="top clients"
              emptyText="No clients have queried in this window yet."
              rows={TOP_CLIENTS_N}
              reserve="min-h-40"
            >
              {(entries) => {
                const max = maxCount(entries);
                return entries.map((entry) => (
                  <RailRow
                    key={entry.key}
                    label={clientNames.get(entry.key) ?? entry.key}
                    count={entry.count}
                    share={(entry.count / max) * 100}
                  />
                ));
              }}
            </RailBody>
          </RailPanel>
        </div>
      </div>

      {availableGroups.length > 1 && (
        <RuleGroupStrip
          groups={availableGroups}
          value={effectiveGroupId}
          onChange={setRuleGroupId}
        />
      )}
    </div>
  );
}
