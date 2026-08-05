import { useMemo, useState } from "react";
import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { Activity, CircleAlert, Globe, ShieldBan, ShieldCheck, Users, Zap } from "lucide-react";
import { toast } from "sonner";
import type { UseQueryResult } from "@tanstack/react-query";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  AreaChart,
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  EmptyState,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Separator,
  Skeleton,
  StatCard,
  StatusIndicator,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@e412/rnui-react";
import type { Group, StatsOverview, TimelineBucket, TopEntry } from "../api/types";
import { useAddRule, useLists } from "../hooks/use-filters";
import { useGroups } from "../hooks/use-groups";
import { useHealth, useStatsOverview, useStatsTimeline, useStatsTop } from "../hooks/use-stats";
import { relativeTime } from "../lib/format";
import { useTheme, type Theme } from "../lib/theme";
import { StaleDataAlert } from "../components/stale-data-alert";

// The group every quick rule targets unless the user picks another one.
// Group 1 is the structural default (see hooks/use-groups.ts) and the group
// unmatched clients resolve to (internal/clients/registry.go).
const DEFAULT_GROUP_ID = 1;

// --- window selector -------------------------------------------------------

const WINDOWS = [
  { value: "1h", label: "Last hour", hours: 1 },
  { value: "24h", label: "Last 24 hours", hours: 24 },
  { value: "7d", label: "Last 7 days", hours: 168 },
] as const;

type WindowValue = (typeof WINDOWS)[number]["value"];
const DEFAULT_WINDOW: WindowValue = "24h";

// `items` maps each value to its display label — without it, SelectValue
// renders the raw stored value ("24h") instead of the option's label
// (Task 12's finding; see settings.tsx/account.tsx for the same fix).
const WINDOW_ITEMS: Record<WindowValue, string> = Object.fromEntries(
  WINDOWS.map((w) => [w.value, w.label]),
) as Record<WindowValue, string>;

function hoursFor(value: WindowValue): number {
  return WINDOWS.find((w) => w.value === value)?.hours ?? 24;
}

function windowPhrase(hours: number): string {
  if (hours <= 1) return "in the last hour";
  if (hours <= 24) return "in the last 24 hours";
  return "in the last 7 days";
}

function WindowSelect({
  value,
  onChange,
}: {
  value: WindowValue;
  onChange: (value: WindowValue) => void;
}) {
  return (
    <Select items={WINDOW_ITEMS} value={value} onValueChange={(v) => onChange(v as WindowValue)}>
      <SelectTrigger aria-label="Time window" className="w-44">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {WINDOWS.map((w) => (
          <SelectItem key={w.value} value={w.value}>
            {w.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// --- quick-rule group selector ---------------------------------------------
// The top-domain tables aggregate across every client, so — unlike the query
// log, where each row names a client and therefore a group — there is
// nothing here to resolve a group *from*. Writing silently into group 1 once
// the Filtering page lets users keep several groups meant a rule that
// reported success and did nothing for anyone outside the default. So the
// target is picked explicitly, and only surfaces once there is a choice to
// make (a single-group instance sees no extra control at all).

function RuleGroupSelect({
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
    <div className="flex flex-wrap items-center gap-2">
      <span className="text-sm text-muted-foreground">Quick block/allow applies to</span>
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

// --- small formatting helpers ----------------------------------------------

/** blocked/total as a whole-number percent string; "—" (never NaN) when there's no data yet. */
function pct(numerator: number, denominator: number): string {
  if (denominator <= 0) return "—";
  return `${Math.round((numerator / denominator) * 100)}%`;
}

// --- health strip ------------------------------------------------------------
// Honest, cheap signals only: GET /health (liveness) and GET /filters/lists
// (last_refreshed). No synthetic uptime or fabricated telemetry.

function HealthStrip() {
  const health = useHealth();
  const lists = useLists();

  const instanceState = health.isPending ? "fixing" : health.isError ? "down" : "active";
  const instanceLabel = health.isPending
    ? "Checking instance…"
    : health.isError
      ? "Instance unreachable"
      : "Instance up";

  let listsLabel: string;
  if (lists.isPending) {
    listsLabel = "Checking filter lists…";
  } else if (lists.isError) {
    listsLabel = "Filter lists unavailable";
  } else if (lists.data.length === 0) {
    listsLabel = "No filter lists configured";
  } else {
    const newest = lists.data.reduce((max, l) => Math.max(max, l.last_refreshed), 0);
    listsLabel = `${lists.data.length} filter ${lists.data.length === 1 ? "list" : "lists"} · refreshed ${relativeTime(newest)}`;
  }

  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-1.5 text-sm text-muted-foreground">
      <StatusIndicator state={instanceState} label={instanceLabel} size="sm" />
      <span aria-hidden="true" className="text-border">
        ·
      </span>
      <span>{listsLabel}</span>
    </div>
  );
}

// --- stat tiles --------------------------------------------------------------

function StatTileSkeleton() {
  return (
    <Card>
      <CardContent className="flex flex-col gap-2 px-4 pt-0">
        <Skeleton className="h-4 w-24" />
        <Skeleton className="h-7 w-16" />
        <Skeleton className="h-3 w-28" />
      </CardContent>
    </Card>
  );
}

function StatTiles({
  overview,
  hours,
}: {
  overview: UseQueryResult<StatsOverview, Error>;
  hours: number;
}) {
  if (overview.isPending) {
    return (
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <StatTileSkeleton key={i} />
        ))}
      </div>
    );
  }

  // isPending is already ruled out, so no data means the *first* fetch failed
  // and there are genuinely no numbers to show.
  if (overview.data === undefined) {
    return (
      <Alert variant="destructive">
        <CircleAlert />
        <AlertTitle>Couldn&apos;t load stats</AlertTitle>
        <AlertDescription>Try refreshing the page.</AlertDescription>
      </Alert>
    );
  }

  const { total, blocked, cached, clients } = overview.data;
  const phrase = windowPhrase(hours);

  return (
    <div className="flex flex-col gap-4">
      {overview.isError && (
        <StaleDataAlert
          what="stats"
          onRetry={() => void overview.refetch()}
          isRetrying={overview.isFetching}
        />
      )}
      <div className="grid grid-cols-2 gap-4 sm:grid-cols-4 [&_.text-2xl]:tabular-nums">
        <StatCard
          title="Total queries"
          value={total.toLocaleString()}
          description={phrase}
          icon={<Activity />}
        />
        <StatCard
          title="Blocked"
          value={pct(blocked, total)}
          description={`${blocked.toLocaleString()} blocked ${phrase}`}
          icon={<ShieldBan className="text-destructive" />}
        />
        <StatCard
          title="Cache hit rate"
          value={pct(cached, total)}
          description={`${cached.toLocaleString()} served from cache`}
          icon={<Zap className="text-info" />}
        />
        <StatCard
          title="Active clients"
          value={clients.toLocaleString()}
          description="distinct client IPs"
          icon={<Users />}
        />
      </div>
    </div>
  );
}

// --- timeline chart ------------------------------------------------------------
// Stacked not-blocked (bottom, indigo) + blocked (destructive) + errors
// (amber, only when non-zero), so the total height reads as query volume —
// the same total the tiles above report — and the red band reads as block
// share: one glance answers both "how busy" and "how much is being
// filtered".

// Canvas2D cannot resolve CSS custom properties, and rnui's AreaChart feeds
// each series' `color` straight into an ECharts LinearGradient for the area
// fill (see @e412/rnui-react's area-chart), where zrender hands it to
// CanvasGradient.addColorStop. A real browser answers "var(--chart-1)" with
// `SyntaxError: ... could not be parsed as a color`, thrown from a layout
// effect — which React escalates to the nearest ErrorBoundary, replacing the
// entire dashboard with "Something went wrong". An empty timeline draws an
// EmptyState instead of a chart, so a fresh instance looked healthy and only
// started crashing once it had served its first query. The tokens' *values*
// are plain oklch(), which canvas does accept; they just have to be resolved
// before they get there.
const CHART_TOKENS = {
  notBlocked: "--chart-1",
  blocked: "--destructive",
  errors: "--warning",
} as const;

// Only reached when no stylesheet has been applied (jsdom under test, an SSR
// pass): getPropertyValue answers "" for an unknown token, and "" is no more
// paintable than var(). Mirrors styles/dnsaur-theme.css's own two blocks, so
// even the fallback path stays theme-correct.
const CHART_FALLBACKS: Record<Theme, Record<keyof typeof CHART_TOKENS, string>> = {
  light: {
    notBlocked: "oklch(0.55 0.16 265)",
    blocked: "oklch(0.577 0.245 27.325)",
    errors: "oklch(0.78 0.16 80)",
  },
  dark: {
    notBlocked: "oklch(0.68 0.16 265)",
    blocked: "oklch(0.7 0.19 22)",
    errors: "oklch(0.8 0.15 80)",
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
 * Resolved, canvas-safe series colors, re-read on every theme change: light
 * and dark define different values for all three tokens, so one read at mount
 * would leave the chart painted in the previous theme after a toggle. The
 * theme store applies the theme to <html> *before* notifying subscribers
 * (lib/theme.ts), so by the time this recomputes getComputedStyle already
 * reports the new values.
 */
function useChartColors(): Record<keyof typeof CHART_TOKENS, string> {
  const { theme } = useTheme();
  return useMemo(() => {
    const fallback = CHART_FALLBACKS[theme];
    return {
      notBlocked: readColorToken(CHART_TOKENS.notBlocked, fallback.notBlocked),
      blocked: readColorToken(CHART_TOKENS.blocked, fallback.blocked),
      errors: readColorToken(CHART_TOKENS.errors, fallback.errors),
    };
  }, [theme]);
}

function bucketLabel(bucketSec: number, hours: number): string {
  const date = new Date(bucketSec * 1000);
  if (hours <= 24) {
    return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}

function timelineSeries(buckets: TimelineBucket[], hours: number) {
  const sorted = [...buckets].sort((a, b) => a.bucket - b.bucket);
  const categories = sorted.map((b) => bucketLabel(b.bucket, hours));
  const blockedSeries = sorted.map((b) => b.decisions.blocked ?? 0);
  const errorSeries = sorted.map((b) => b.decisions.error ?? 0);
  // "Not blocked" = every decision except blocked *and* error — a failed
  // upstream/resolve attempt isn't a query dnsaur let through, so counting
  // it as "allowed" would be misleading. (Decision kinds: allowed, blocked,
  // local, cached, stale, forwarded, error — see internal/dnssrv/pipeline.go.)
  // Errors are carried as their own series rather than dropped, so the three
  // stacked series still add up to the same denominator the "Total queries"
  // tile uses (internal/api/queries_handlers.go sums *every* decision into
  // `total`) — on an instance with upstream failures the chart and the tile
  // used to disagree about the same window.
  const notBlockedSeries = sorted.map((b, i) => {
    const total = Object.entries(b.decisions)
      .filter(([decision]) => decision !== "error")
      .reduce((sum, [, n]) => sum + n, 0);
    return total - (blockedSeries[i] ?? 0);
  });
  return { categories, notBlockedSeries, blockedSeries, errorSeries };
}

function TimelineCard({
  timeline,
  hours,
}: {
  timeline: UseQueryResult<TimelineBucket[], Error>;
  hours: number;
}) {
  const colors = useChartColors();

  let body: ReactNode;
  if (timeline.isPending) {
    body = <Skeleton className="h-[280px] w-full" />;
  } else if (timeline.data === undefined) {
    // isPending is already ruled out, so no data means the *first* load
    // failed — there is genuinely nothing to draw. A background poll that
    // fails with buckets still cached takes the StaleDataAlert path below.
    body = (
      <Alert variant="destructive">
        <CircleAlert />
        <AlertTitle>Couldn&apos;t load the timeline</AlertTitle>
      </Alert>
    );
  } else if (timeline.data.length === 0) {
    body = (
      <EmptyState
        icon={<Activity />}
        title="No query activity yet"
        description="Once dnsaur resolves queries in this window, blocked vs. not-blocked volume shows up here."
      />
    );
  } else {
    const { categories, notBlockedSeries, blockedSeries, errorSeries } = timelineSeries(
      timeline.data,
      hours,
    );
    const totalNotBlocked = notBlockedSeries.reduce((s, n) => s + n, 0);
    const totalBlocked = blockedSeries.reduce((s, n) => s + n, 0);
    const totalErrors = errorSeries.reduce((s, n) => s + n, 0);
    body = (
      <figure
        aria-label={`Query volume over time: ${(totalNotBlocked + totalBlocked + totalErrors).toLocaleString()} queries — ${totalNotBlocked.toLocaleString()} not blocked, ${totalBlocked.toLocaleString()} blocked, ${totalErrors.toLocaleString()} errored`}
      >
        <AreaChart
          categories={categories}
          series={[
            // Indigo (the brand's own primary hue), not green, deliberately —
            // red/green is the one pairing that collapses for red-green
            // color blindness, the most common form. Red vs. indigo stays
            // distinguishable, and the legend + tooltip below back it with
            // text either way.
            { name: "Not blocked", data: notBlockedSeries, color: colors.notBlocked },
            { name: "Blocked", data: blockedSeries, color: colors.blocked },
            // Amber, never red: a failed resolve is a broken query, not a
            // policy decision (same convention as the query log's badges).
            // Only carried when there are any — an always-flat zero series
            // is legend noise on a healthy instance.
            ...(totalErrors > 0
              ? [{ name: "Errors", data: errorSeries, color: colors.errors }]
              : []),
          ]}
          stacked
          height={280}
          option={{ tooltip: { trigger: "axis" } }}
        />
      </figure>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Query volume</CardTitle>
        <CardDescription>
          Blocked, not-blocked, and failed queries over time — the same total as the tiles above.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {timeline.isError && timeline.data !== undefined && (
          <StaleDataAlert
            what="the timeline"
            onRetry={() => void timeline.refetch()}
            isRetrying={timeline.isFetching}
          />
        )}
        {body}
      </CardContent>
    </Card>
  );
}

// --- top lists -----------------------------------------------------------------

interface TopTableAction {
  label: string;
  icon: LucideIcon;
  doneLabel: string;
  doneVariant: BadgeProps["variant"];
  status: Record<string, "pending" | "done">;
  onAction: (pattern: string) => void;
  /** Set while the target group can't be determined — see Dashboard. */
  disabled?: boolean;
  disabledReason?: string;
}

function TopTable({
  title,
  icon: Icon,
  // Neutral by default; "Top blocked domains" passes text-destructive to
  // carry the same red = blocked thread through from the stat tile and
  // chart — restrained (only the one card that's actually about blocking
  // gets tinted), not decoration.
  iconClassName = "text-muted-foreground",
  query,
  columnLabel,
  emptyIcon: EmptyIcon,
  emptyTitle,
  emptyDescription,
  action,
}: {
  title: string;
  icon: LucideIcon;
  iconClassName?: string;
  query: UseQueryResult<TopEntry[], Error>;
  columnLabel: string;
  emptyIcon: LucideIcon;
  emptyTitle: string;
  emptyDescription: string;
  action?: TopTableAction;
}) {
  let body: ReactNode;
  if (query.isPending) {
    body = (
      <div className="flex flex-col gap-2 px-4 pb-4">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-8 w-full" />
        ))}
      </div>
    );
  } else if (query.data === undefined) {
    // First load failed outright. A poll that fails with rows still cached
    // keeps them on screen under the StaleDataAlert below instead.
    body = <p className="px-4 pb-4 text-sm text-muted-foreground">Couldn&apos;t load this list.</p>;
  } else if (query.data.length === 0) {
    body = (
      <EmptyState
        icon={<EmptyIcon />}
        title={emptyTitle}
        description={emptyDescription}
        className="py-8"
      />
    );
  } else {
    body = (
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{columnLabel}</TableHead>
            <TableHead className="text-right">Queries</TableHead>
            {action && <TableHead className="text-right">Action</TableHead>}
          </TableRow>
        </TableHeader>
        <TableBody>
          {query.data.map((entry) => {
            const rowStatus = action?.status[entry.key];
            return (
              <TableRow key={entry.key}>
                <TableCell className="max-w-48 truncate font-mono text-xs">{entry.key}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {entry.count.toLocaleString()}
                </TableCell>
                {action && (
                  <TableCell className="text-right">
                    {rowStatus === "done" ? (
                      <Badge variant={action.doneVariant}>{action.doneLabel}</Badge>
                    ) : (
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        disabled={rowStatus === "pending" || action.disabled}
                        title={action.disabled ? action.disabledReason : undefined}
                        onClick={() => action.onAction(entry.key)}
                      >
                        <action.icon />
                        {action.label}
                      </Button>
                    )}
                  </TableCell>
                )}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Icon className={`size-4 ${iconClassName}`} />
          {title}
        </CardTitle>
      </CardHeader>
      <CardContent className="px-0">
        {query.isError && query.data !== undefined && (
          <div className="px-4 pb-4">
            <StaleDataAlert
              what={title.toLowerCase()}
              onRetry={() => void query.refetch()}
              isRetrying={query.isFetching}
            />
          </div>
        )}
        {body}
      </CardContent>
    </Card>
  );
}

// --- page ------------------------------------------------------------------

export function Dashboard() {
  const [windowValue, setWindowValue] = useState<WindowValue>(DEFAULT_WINDOW);
  const hours = hoursFor(windowValue);

  const overview = useStatsOverview(hours);
  const timeline = useStatsTimeline(hours);
  const topDomains = useStatsTop("domain", 10, hours);
  const topBlocked = useStatsTop("blocked_domain", 10, hours);
  const topClients = useStatsTop("client", 10, hours);

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
  const groupsUnavailable = groups.isPending || groups.isError;
  const quickActionBlock = {
    disabled: groupsUnavailable,
    disabledReason: groups.isError
      ? "Couldn't load groups — reload to write rules from here"
      : "Loading groups…",
  };

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
    <div className="flex flex-col gap-8">
      {/* Page chrome — heading, window control, instance health — grouped
          tightly and set off from the content below with a hairline rule,
          the same "distinct structural zone" treatment the sidebar uses to
          bracket its own nav/status regions. */}
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-heading font-semibold text-foreground">Dashboard</h1>
            <p className="mt-1 text-sm text-muted-foreground">
              Resolver activity for the selected window.
            </p>
          </div>
          <WindowSelect value={windowValue} onChange={setWindowValue} />
        </div>

        <HealthStrip />
      </div>

      <Separator />

      <StatTiles overview={overview} hours={hours} />

      <TimelineCard timeline={timeline} hours={hours} />

      {availableGroups.length > 1 && (
        <RuleGroupSelect
          groups={availableGroups}
          value={effectiveGroupId}
          onChange={setRuleGroupId}
        />
      )}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        <TopTable
          title="Top domains"
          icon={Globe}
          query={topDomains}
          columnLabel="Domain"
          emptyIcon={Globe}
          emptyTitle="No domains yet"
          emptyDescription="Resolved domains will show up here once dnsaur starts serving queries."
          action={{
            label: "Block",
            icon: ShieldBan,
            doneLabel: "Blocked",
            doneVariant: "destructive-light",
            status: ruleStatus,
            onAction: (pattern) => quickRule("block", pattern),
            ...quickActionBlock,
          }}
        />
        <TopTable
          title="Top blocked domains"
          icon={ShieldBan}
          iconClassName="text-destructive"
          query={topBlocked}
          columnLabel="Domain"
          emptyIcon={ShieldBan}
          emptyTitle="No blocked domains yet"
          emptyDescription="Domains blocked by your filter lists or rules will show up here."
          action={{
            label: "Allow",
            icon: ShieldCheck,
            doneLabel: "Allowed",
            doneVariant: "success-light",
            status: ruleStatus,
            onAction: (pattern) => quickRule("allow", pattern),
            ...quickActionBlock,
          }}
        />
        <TopTable
          title="Top clients"
          icon={Users}
          query={topClients}
          columnLabel="Client"
          emptyIcon={Users}
          emptyTitle="No clients yet"
          emptyDescription="Clients making DNS queries will show up here."
        />
      </div>
    </div>
  );
}
