import { useState } from "react";
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
import type { StatsOverview, TimelineBucket, TopEntry } from "../api/types";
import { useAddRule, useLists } from "../hooks/use-filters";
import { useHealth, useStatsOverview, useStatsTimeline, useStatsTop } from "../hooks/use-stats";
import { relativeTime } from "../lib/format";

// The one group the setup wizard ever creates today (see the query log's
// own STARTER_GROUP_ID convention in pages/queries.tsx) — the dashboard's
// per-domain quick block/allow action targets it until group selection
// exists in the UI (Task 10).
const QUICK_RULE_GROUP_ID = 1;

// --- window selector -------------------------------------------------------

const WINDOWS = [
  { value: "1h", label: "Last hour", hours: 1 },
  { value: "24h", label: "Last 24 hours", hours: 24 },
  { value: "7d", label: "Last 7 days", hours: 168 },
] as const;

type WindowValue = (typeof WINDOWS)[number]["value"];
const DEFAULT_WINDOW: WindowValue = "24h";

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
    <Select value={value} onValueChange={(v) => onChange(v as WindowValue)}>
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

  if (overview.isError) {
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
  );
}

// --- timeline chart ------------------------------------------------------------
// Stacked not-blocked (bottom, indigo) + blocked (top, destructive) area, so
// the total-height reads as query volume and the red cap reads as block
// share — one glance answers both "how busy" and "how much is being
// filtered".

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
  // "Not blocked" = every decision except blocked *and* error — a failed
  // upstream/resolve attempt isn't a query dnsaur let through, so counting
  // it as "allowed" would be misleading. (Decision kinds: allowed, blocked,
  // local, cached, stale, forwarded, error — see internal/dnssrv/pipeline.go.)
  const notBlockedSeries = sorted.map((b, i) => {
    const total = Object.entries(b.decisions)
      .filter(([decision]) => decision !== "error")
      .reduce((sum, [, n]) => sum + n, 0);
    return total - (blockedSeries[i] ?? 0);
  });
  return { categories, notBlockedSeries, blockedSeries };
}

function TimelineCard({
  timeline,
  hours,
}: {
  timeline: UseQueryResult<TimelineBucket[], Error>;
  hours: number;
}) {
  let body: ReactNode;
  if (timeline.isPending) {
    body = <Skeleton className="h-[280px] w-full" />;
  } else if (timeline.isError) {
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
    const { categories, notBlockedSeries, blockedSeries } = timelineSeries(timeline.data, hours);
    const totalNotBlocked = notBlockedSeries.reduce((s, n) => s + n, 0);
    const totalBlocked = blockedSeries.reduce((s, n) => s + n, 0);
    body = (
      <figure
        aria-label={`Query volume over time: ${totalNotBlocked.toLocaleString()} not blocked, ${totalBlocked.toLocaleString()} blocked`}
      >
        <AreaChart
          categories={categories}
          series={[
            // Indigo (the brand's own primary hue), not green, deliberately —
            // red/green is the one pairing that collapses for red-green
            // color blindness, the most common form. Red vs. indigo stays
            // distinguishable, and the legend + tooltip below back it with
            // text either way.
            { name: "Not blocked", data: notBlockedSeries, color: "var(--chart-1)" },
            { name: "Blocked", data: blockedSeries, color: "var(--destructive)" },
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
        <CardDescription>Blocked vs. not-blocked queries over time.</CardDescription>
      </CardHeader>
      <CardContent>{body}</CardContent>
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
  } else if (query.isError) {
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
                        disabled={rowStatus === "pending"}
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
      <CardContent className="px-0">{body}</CardContent>
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

  const addRule = useAddRule();
  const [ruleStatus, setRuleStatus] = useState<Record<string, "pending" | "done">>({});

  function quickRule(action: "allow" | "block", pattern: string) {
    setRuleStatus((s) => ({ ...s, [pattern]: "pending" }));
    addRule.mutate(
      { groupId: QUICK_RULE_GROUP_ID, action, pattern },
      {
        onSuccess: () => {
          setRuleStatus((s) => ({ ...s, [pattern]: "done" }));
          toast.success(action === "block" ? `Blocked ${pattern}` : `Allowed ${pattern}`);
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
