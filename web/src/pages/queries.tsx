import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { getCoreRowModel, useReactTable, type ColumnDef, type Table } from "@tanstack/react-table";
import {
  Activity,
  ArrowUpRight,
  CircleAlert,
  Clock,
  Filter as FilterIcon,
  HelpCircle,
  House,
  Search,
  ShieldBan,
  ShieldCheck,
  TriangleAlert,
  Zap,
  type LucideIcon,
} from "lucide-react";
import { toast } from "sonner";
import {
  Badge,
  Button,
  cn,
  DataGrid,
  DataGridContainer,
  DataGridScrollArea,
  DataGridTableVirtual,
  Drawer,
  DrawerContent,
  DrawerDescription,
  DrawerHeader,
  DrawerTitle,
  EmptyState,
  Filters,
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
  Separator,
  Skeleton,
  StatusIndicator,
  Switch,
  Tooltip,
  TooltipContent,
  TooltipTrigger,
  type BadgeProps,
  type Filter as RnuiFilter,
  type FilterFieldConfig,
  type StatusIndicatorProps,
} from "@e412/rnui-react";
import type { Client, List, QueryEntry, Rule } from "../api/types";
import type { SseState } from "../api/sse";
import { useClients } from "../hooks/use-clients";
import { useAddRule, useRules, useLists } from "../hooks/use-filters";
import { useLiveTail, useQuerySearch, type QuerySearchFilter } from "../hooks/use-queries";

// The group a query is attributed to when its client matched no client
// entry at all — internal/clients/registry.go's Lookup falls back to
// ClientInfo{GroupID: 1} (with a zero ID) for those, so the query log must
// use the same fallback or its rules would land somewhere the resolver
// never consults for that client.
const DEFAULT_GROUP_ID = 1;

/**
 * The group whose rules actually govern this row, resolved through the
 * row's client — NOT a hardcoded group 1.
 *
 * `QueryEntry.client_id` is the *client's* id (internal/qlog/qlog.go), so
 * the group comes from that client's `group_id`. Returns null when the row
 * names a client this instance can't resolve (deleted since, or the client
 * list hasn't loaded yet): writing a rule into a guessed group would be a
 * silent no-op for that client, so callers must refuse rather than claim
 * success.
 */
function groupForEntry(entry: QueryEntry, clientGroups: Map<number, number>): number | null {
  if (!entry.client_id) return DEFAULT_GROUP_ID;
  return clientGroups.get(entry.client_id) ?? null;
}

function clientGroupMap(clients: Client[] | undefined): Map<number, number> {
  return new Map((clients ?? []).map((c) => [c.id, c.group_id]));
}

// --- decision badges ---------------------------------------------------
// A seven-way vocabulary (see internal/dnssrv/pipeline.go for the decision
// kinds a query can land on) built as a deliberate hierarchy, not a flat
// rainbow — each decision gets a DIFFERENT icon silhouette (a second
// channel beyond color/text, so it still reads correctly for colorblind
// users or in a quick grayscale glance) and its color-weight signals how
// much attention it deserves:
//   - filled ("-light") badges are the two poles a sysadmin scans for
//     first — blocked (red, a stop) and allowed (green, an explicit rule
//     override) — plus error (amber), which must never share blocked's
//     red: a failed resolve is a broken query, not a policy decision.
//   - outline badges are the "expected, keep scrolling" cases: forwarded
//     (the plain default path — bare, no tint at all, so it stays quiet
//     against the two poles) and stale (amber outline — related to
//     cached but flagged, without the visual weight of a true error).
//   - cached reuses the dashboard's own cache convention (info/cyan +
//     Zap, see dashboard.tsx's "Cache hit rate" tile) rather than
//     inventing a new hue for the same concept.
//   - local sits outside the policy axis entirely (self-answered, not
//     forwarded or filtered) — neutral secondary/grey, not part of the
//     red/green/amber thread at all.
const DECISION_BADGE: Record<
  string,
  {
    label: string;
    variant: NonNullable<BadgeProps["variant"]>;
    icon: LucideIcon;
    rail: string;
  }
> = {
  blocked: {
    label: "Blocked",
    variant: "destructive-light",
    icon: ShieldBan,
    rail: "bg-destructive",
  },
  allowed: {
    label: "Allowed",
    variant: "success-light",
    icon: ShieldCheck,
    rail: "bg-success",
  },
  error: {
    label: "Error",
    variant: "warning-light",
    icon: TriangleAlert,
    rail: "bg-warning",
  },
  stale: {
    label: "Stale",
    variant: "warning-outline",
    icon: Clock,
    rail: "bg-warning/50",
  },
  cached: {
    label: "Cached",
    variant: "info-light",
    icon: Zap,
    rail: "bg-info",
  },
  local: {
    label: "Local",
    variant: "secondary",
    icon: House,
    rail: "bg-muted-foreground/50",
  },
  forwarded: {
    label: "Forwarded",
    variant: "outline",
    icon: ArrowUpRight,
    rail: "bg-border",
  },
};

function decisionBadge(decision: string) {
  return (
    DECISION_BADGE[decision] ?? {
      label: decision || "Unknown",
      variant: "outline" as const,
      icon: HelpCircle,
      rail: "bg-border",
    }
  );
}

function DecisionBadge({ decision }: { decision: string }) {
  const { label, variant, icon: Icon } = decisionBadge(decision);
  return (
    <Badge variant={variant}>
      <Icon />
      {label}
    </Badge>
  );
}

/** The log's signature scan aid: a decision-colored rail down the left
 * edge of every row, the same hue as that row's decision badge — so the
 * whole log reads top-to-bottom like a colored-priority log viewer
 * (journalctl -p, lnav) without needing to read every badge individually.
 * `isNew` marks the single most-recently-arrived live row (see
 * `data-row-new` / app.css's `dnsaur-query-row-in` keyframe) so a fresh
 * row gets one brief attention flash without any JS animation loop or
 * per-row timers — never more than one row animating at once, however
 * fast the stream is. */
function DecisionRail({ decision, isNew }: { decision: string; isNew?: boolean }) {
  const { rail } = decisionBadge(decision);
  return (
    <div
      aria-hidden="true"
      className={cn("h-5 w-1 rounded-full", rail)}
      data-decision-rail={decision}
      data-row-new={isNew ? "true" : undefined}
    />
  );
}

function formatTime(atMs: number): string {
  return new Date(atMs).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// --- table columns -------------------------------------------------------

type RowStatus = "pending" | "blocked" | "allowed";

/**
 * Everything a cell needs that changes while the table is mounted. It
 * travels through the table's `meta` rather than being closed over by the
 * column defs, because a column def array built per render is poison here:
 * `cell` functions are used as component *types*, so a fresh arrow function
 * per render makes React unmount and remount every visible row (losing focus
 * and restarting the row-flash animation), on top of rebuilding
 * getAllColumns/getHeaderGroups — and the live tail re-renders on every
 * batch of arrivals. With the defs constant, only the cells' output changes.
 */
interface QueryTableMeta {
  rowStatus: Record<number, RowStatus>;
  newRowId: number | null;
  onBlock: (entry: QueryEntry) => void;
  onAllow: (entry: QueryEntry) => void;
  onWhy: (entry: QueryEntry) => void;
  /**
   * Set while GET /clients hasn't landed. Every rule is scoped to the group
   * that governs the row's client (see groupForEntry), so with no client list
   * there is no group to write into — the actions would fail on click with
   * "this client's group is unknown", which describes a deleted client, not a
   * sibling request that is still in flight or has failed.
   */
  actionsDisabled: boolean;
  actionsDisabledReason?: string;
}

function tableMeta(table: Table<QueryEntry>): QueryTableMeta {
  return table.options.meta as QueryTableMeta;
}

const QUERY_COLUMNS: ColumnDef<QueryEntry>[] = [
  {
    id: "rail",
    header: "",
    size: 10,
    cell: ({ row, table }) => (
      <DecisionRail
        decision={row.original.decision}
        isNew={row.original.id === tableMeta(table).newRowId}
      />
    ),
  },
  {
    accessorKey: "at",
    header: "Time",
    size: 88,
    cell: ({ row }) => (
      <span className="font-mono text-xs tabular-nums text-muted-foreground">
        {formatTime(row.original.at)}
      </span>
    ),
  },
  {
    accessorKey: "client_ip",
    header: "Client",
    size: 118,
    cell: ({ row }) => <span className="font-mono text-xs">{row.original.client_ip}</span>,
  },
  {
    accessorKey: "q_name",
    header: "Domain",
    cell: ({ row }) => (
      <span className="font-mono text-xs" title={row.original.q_name}>
        {row.original.q_name}
      </span>
    ),
  },
  {
    accessorKey: "q_type",
    header: "Type",
    size: 60,
    cell: ({ row }) => (
      <span className="font-mono text-xs text-muted-foreground">{row.original.q_type}</span>
    ),
  },
  {
    accessorKey: "decision",
    header: "Decision",
    size: 108,
    cell: ({ row }) => <DecisionBadge decision={row.original.decision} />,
  },
  {
    accessorKey: "upstream",
    header: "Upstream",
    size: 124,
    cell: ({ row }) => (
      <span className="font-mono text-xs text-muted-foreground">
        {row.original.upstream || "—"}
      </span>
    ),
  },
  {
    accessorKey: "duration_ms",
    header: "Latency",
    size: 76,
    meta: { headerClassName: "text-right", cellClassName: "text-right" },
    cell: ({ row }) => (
      <span className="font-mono text-xs tabular-nums text-muted-foreground">
        {row.original.duration_ms}ms
      </span>
    ),
  },
  {
    id: "actions",
    header: "",
    size: 216,
    cell: ({ row, table }) => {
      const entry = row.original;
      const { rowStatus, onBlock, onAllow, onWhy, actionsDisabled, actionsDisabledReason } =
        tableMeta(table);
      const status = rowStatus[entry.id];
      const disabled = status === "pending" || actionsDisabled;
      const title = actionsDisabled ? actionsDisabledReason : undefined;
      return (
        <div className="flex items-center justify-end gap-1">
          {status === "blocked" ? (
            <Badge variant="destructive-light">Blocked</Badge>
          ) : status === "allowed" ? (
            <Badge variant="success-light">Allowed</Badge>
          ) : (
            <>
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={disabled}
                title={title}
                onClick={() => onBlock(entry)}
              >
                <ShieldBan />
                Block
              </Button>
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={disabled}
                title={title}
                onClick={() => onAllow(entry)}
              >
                <ShieldCheck />
                Allow
              </Button>
            </>
          )}
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  aria-label={`Why was ${entry.q_name} ${entry.decision}?`}
                  onClick={() => onWhy(entry)}
                />
              }
            >
              <HelpCircle />
            </TooltipTrigger>
            <TooltipContent>Why this decision?</TooltipContent>
          </Tooltip>
        </div>
      );
    },
  },
];

// --- shared grid -----------------------------------------------------------
// Virtualized per the brief: every SSE message can replace the whole
// 500-row live-tail array, and a plain (non-virtualized) table would
// re-render every richly-celled row on every single message on a busy
// resolver. DataGridTableVirtual only mounts the rows within (or near) the
// visible scroll window, so a full-buffer replacement stays cheap
// regardless of how many rows are logically in the array.
//
// DataGridTableVirtual has no built-in skeleton/isLoading handling (unlike
// the plain DataGridTable) — its virtual body only branches on
// isFetchingMore/hasMore for infinite-scroll status rows, not an initial
// loading state. So the loading skeleton is rendered here explicitly,
// swapped in for the grid entirely while paged search is still in flight,
// rather than relying on DataGrid's (virtual-body-unaware) isLoading prop.
//
// Those same isFetchingMore/hasMore props are what makes filtered search
// page: scrolling to the end asks useQuerySearch for the next offset
// instead of stopping dead at the first 100 matches, and a short final page
// swaps the loader for "All matching queries loaded" so the end of the
// results is stated rather than implied.

const ROW_HEIGHT_ESTIMATE = 34; // dense row: ~12px vertical padding + text-xs line height + 1px border

function QueryDataGridPanel({
  entries,
  isLoading,
  emptyState,
  meta,
  onFetchMore,
  isFetchingMore,
  hasMore,
}: {
  entries: QueryEntry[];
  isLoading: boolean;
  emptyState: ReactNode;
  meta: QueryTableMeta;
  /** Paged mode only — omitted for the live tail, which has no "more". */
  onFetchMore?: () => void;
  isFetchingMore?: boolean;
  hasMore?: boolean;
}) {
  const table = useReactTable({
    data: entries,
    columns: QUERY_COLUMNS,
    meta,
    getRowId: (row) => String(row.id),
    getCoreRowModel: getCoreRowModel(),
  });

  return (
    <DataGrid
      table={table}
      recordCount={entries.length}
      isLoading={isLoading}
      emptyMessage={emptyState}
      fetchingMoreMessage="Loading more matches…"
      allRowsLoadedMessage="All matching queries loaded"
      tableLayout={{ dense: true, width: "auto" }}
    >
      <DataGridContainer>
        {isLoading ? (
          <div className="flex flex-col gap-2 p-3" aria-hidden="true">
            {Array.from({ length: 8 }).map((_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : (
          <DataGridScrollArea className="h-[34rem]">
            <DataGridTableVirtual
              estimateSize={ROW_HEIGHT_ESTIMATE}
              overscan={12}
              onFetchMore={onFetchMore}
              isFetchingMore={isFetchingMore}
              hasMore={hasMore}
            />
          </DataGridScrollArea>
        )}
      </DataGridContainer>
    </DataGrid>
  );
}

// --- "why?" drawer -----------------------------------------------------------

function WhyContent({
  entry,
  rule,
  ruleLoading,
  list,
}: {
  entry: QueryEntry;
  rule?: Rule;
  /** The row's group's rules are still being fetched — see whyGroupId. */
  ruleLoading?: boolean;
  list?: List;
}) {
  const { label, variant, icon: Icon } = decisionBadge(entry.decision);

  let matchBody: ReactNode;
  if (entry.rule_id > 0) {
    matchBody = rule ? (
      <p className="text-sm text-muted-foreground">
        Matched a {rule.action === "block" ? "block" : "allow"} rule for{" "}
        <code className="font-mono text-foreground">{rule.pattern}</code>
        {rule.is_regex ? " (regex)" : ""}.
      </p>
    ) : ruleLoading ? (
      // Never claim the rule is missing while its group's rules are still
      // in flight — the drawer opens before that request comes back.
      <p className="text-sm text-muted-foreground">Looking up the matching rule…</p>
    ) : (
      <p className="text-sm text-muted-foreground">
        Matched rule #{entry.rule_id}, which isn&apos;t available right now (it may have been
        deleted).
      </p>
    );
  } else if (entry.list_id > 0) {
    matchBody = list ? (
      <p className="text-sm text-muted-foreground">
        From the {list.kind} list{" "}
        <code className="font-mono text-foreground break-all">{list.url}</code>.
      </p>
    ) : (
      <p className="text-sm text-muted-foreground">
        Matched list #{entry.list_id}, which isn&apos;t available right now.
      </p>
    );
  } else {
    matchBody = (
      <p className="text-sm text-muted-foreground">
        No rule or list matched — resolved by the default policy.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <Badge variant={variant} className="w-fit">
        <Icon />
        {label}
      </Badge>
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 text-sm">
        <dt className="text-muted-foreground">Time</dt>
        <dd className="tabular-nums">{new Date(entry.at).toLocaleString()}</dd>
        <dt className="text-muted-foreground">Client</dt>
        <dd className="font-mono">{entry.client_ip}</dd>
        <dt className="text-muted-foreground">Query type</dt>
        <dd className="font-mono">{entry.q_type}</dd>
        <dt className="text-muted-foreground">Upstream</dt>
        <dd className="font-mono">{entry.upstream || "—"}</dd>
        <dt className="text-muted-foreground">Response code</dt>
        <dd className="font-mono">{entry.r_code || "—"}</dd>
        <dt className="text-muted-foreground">Latency</dt>
        <dd className="tabular-nums">{entry.duration_ms}ms</dd>
      </dl>
      <Separator />
      <div className="flex flex-col gap-1.5">
        <h4 className="text-sm font-medium text-foreground">Match</h4>
        {matchBody}
      </div>
    </div>
  );
}

// --- filters -----------------------------------------------------------

const FILTER_FIELDS: FilterFieldConfig<string>[] = [
  { key: "decision", label: "Decision", type: "text", placeholder: "blocked, allowed, cached…" },
  { key: "type", label: "Record type", type: "text", placeholder: "A, AAAA, CNAME…" },
  { key: "client", label: "Client IP", type: "text", placeholder: "192.168.1.10" },
];

function toSearchFilter(rnuiFilters: RnuiFilter<string>[], search: string): QuerySearchFilter {
  const byField = new Map(rnuiFilters.map((f) => [f.field, f.values[0]]));
  const decision = byField.get("decision")?.trim();
  const type = byField.get("type")?.trim();
  const client = byField.get("client")?.trim();
  return {
    decision: decision || undefined,
    type: type || undefined,
    client: client || undefined,
    q: search.trim() || undefined,
  };
}

// --- live state indicator -----------------------------------------------

function liveStateBadge(
  paused: boolean,
  state: SseState,
): { dotState: NonNullable<StatusIndicatorProps["state"]>; label: string } {
  if (paused) return { dotState: "idle", label: "Paused" };
  if (state === "open") return { dotState: "active", label: "Streaming" };
  if (state === "reconnecting") return { dotState: "fixing", label: "Reconnecting…" };
  // "failed" = the stream gave up retrying (see api/sse.ts). Said plainly,
  // with a manual retry beside it, rather than pretending a reconnect is
  // still coming — the usual cause is a session that no longer exists.
  if (state === "failed") return { dotState: "down", label: "Live tail disconnected" };
  return { dotState: "down", label: "Disconnected" };
}

// --- page ------------------------------------------------------------------

export function QueryLog() {
  const [rnuiFilters, setRnuiFilters] = useState<RnuiFilter<string>[]>([]);
  const [searchDraft, setSearchDraft] = useState("");
  const [search, setSearch] = useState("");
  const [paused, setPaused] = useState(false);
  // Open state is deliberately separate from the target rather than derived
  // from `whyEntry !== null`: the drawer stays mounted through vaul's exit
  // transition, so nulling the entry on close would blank the still-visible
  // panel and slide an empty sheet off screen. The entry is replaced on the
  // next open instead of cleared on close (same treatment as dns.tsx's edit
  // sheet and groups-clients.tsx's client dialog).
  const [whyEntry, setWhyEntry] = useState<QueryEntry | null>(null);
  const [whyOpen, setWhyOpen] = useState(false);

  const openWhy = useCallback((entry: QueryEntry) => {
    setWhyEntry(entry);
    setWhyOpen(true);
  }, []);

  // Light debounce so the domain search doesn't fire a LIKE query per
  // keystroke — the rnui Filters chips (decision/type/client) are
  // discrete selections, not per-keystroke, so they're left undebounced.
  useEffect(() => {
    const t = setTimeout(() => setSearch(searchDraft), 300);
    return () => clearTimeout(t);
  }, [searchDraft]);

  const hasActiveFilters = rnuiFilters.length > 0 || search.trim() !== "";
  const searchFilter = useMemo(() => toSearchFilter(rnuiFilters, search), [rnuiFilters, search]);

  // Live tail and paged search share one `enabled` toggle each: the live
  // tail stops consuming the stream (see use-queries.ts) the moment either
  // a filter is applied OR the user pauses; the paged search only ever
  // runs while filters are active.
  const live = useLiveTail(!hasActiveFilters && !paused);
  const paged = useQuerySearch(searchFilter, { enabled: hasActiveFilters });

  // Row-insertion flash (see DecisionRail / app.css's dnsaur-query-row-in
  // keyframe): track only the single newest live row at a time, so a
  // bursty stream never has more than one row animating at once. A ref
  // (not state) tracks "have we already flashed this id" across renders
  // without re-triggering the effect on every unrelated re-render.
  const [newRowId, setNewRowId] = useState<number | null>(null);
  const latestFlashedId = useRef<number | undefined>(undefined);
  const latestLiveId = live.entries[0]?.id;
  useEffect(() => {
    if (hasActiveFilters || paused) return;
    if (latestLiveId === undefined || latestLiveId === latestFlashedId.current) return;
    latestFlashedId.current = latestLiveId;
    setNewRowId(latestLiveId);
    const t = setTimeout(() => {
      setNewRowId((id) => (id === latestLiveId ? null : id));
    }, 700);
    return () => clearTimeout(t);
  }, [latestLiveId, hasActiveFilters, paused]);

  // Every quick rule and the "why?" drawer are scoped to the group that
  // actually governs the row's client (see groupForEntry) — a rule written
  // into group 1 for a client that lives in group 3 is a no-op the UI would
  // otherwise report as a success.
  const clients = useClients();
  const clientGroups = useMemo(() => clientGroupMap(clients.data), [clients.data]);
  const whyGroupId = whyEntry ? groupForEntry(whyEntry, clientGroups) : null;

  const rules = useRules(whyGroupId ?? DEFAULT_GROUP_ID);
  const lists = useLists();
  const rulesById = useMemo(() => new Map((rules.data ?? []).map((r) => [r.id, r])), [rules.data]);
  const listsById = useMemo(() => new Map((lists.data ?? []).map((l) => [l.id, l])), [lists.data]);

  const addRule = useAddRule();
  const [rowStatus, setRowStatus] = useState<Record<number, RowStatus>>({});

  const quickRule = useCallback(
    (action: "allow" | "block", entry: QueryEntry) => {
      const verb = action === "block" ? "block" : "allow";
      const groupId = groupForEntry(entry, clientGroups);
      if (groupId === null) {
        toast.error(`Couldn't ${verb} ${entry.q_name} — this client's group is unknown`);
        return;
      }
      setRowStatus((s) => ({ ...s, [entry.id]: "pending" }));
      addRule.mutate(
        { groupId, action, pattern: entry.q_name },
        {
          onSuccess: () => {
            setRowStatus((s) => ({ ...s, [entry.id]: action === "block" ? "blocked" : "allowed" }));
            toast.success(
              action === "block" ? `Blocked ${entry.q_name}` : `Allowed ${entry.q_name}`,
            );
          },
          onError: () => {
            setRowStatus((s) => {
              const next = { ...s };
              delete next[entry.id];
              return next;
            });
            toast.error(`Couldn't ${verb} ${entry.q_name}`);
          },
        },
      );
    },
    [addRule, clientGroups],
  );

  const onBlock = useCallback((e: QueryEntry) => quickRule("block", e), [quickRule]);
  const onAllow = useCallback((e: QueryEntry) => quickRule("allow", e), [quickRule]);
  // Without the client list, groupForEntry can only answer null for any row
  // that names a client — which the quick rules report as "this client's group
  // is unknown", a message about a *deleted* client. When the real cause is a
  // /clients request that is still in flight or has failed, that's both wrong
  // and unactionable, so the actions are held closed and say why instead.
  const clientsUnavailable = clients.isPending || clients.isError;
  const gridMeta: QueryTableMeta = useMemo(
    () => ({
      rowStatus,
      newRowId,
      onBlock,
      onAllow,
      onWhy: openWhy,
      actionsDisabled: clientsUnavailable,
      actionsDisabledReason: clients.isError
        ? "Couldn't load clients — reload to write rules from here"
        : "Loading clients…",
    }),
    [rowStatus, newRowId, onBlock, onAllow, openWhy, clientsUnavailable, clients.isError],
  );

  // Deduplicated by id, not a bare flat(): the search endpoint pages by
  // `ORDER BY id DESC LIMIT ? OFFSET ?` (internal/store/search.go) with the
  // running row count as the next offset, so any query logged between the
  // page-1 and page-2 fetches shifts every row down one and page 2 re-returns
  // the tail of page 1. getRowId is String(row.id), so those duplicates reach
  // react-table and the virtualizer as duplicate keys — React logs a
  // duplicate-key warning and the same domain renders twice around the page
  // boundary. Deduping here rather than in the pageParam keeps hasNextPage
  // correct: termination still reads the *raw* page length, so a page that is
  // short only because it overlapped isn't mistaken for the end of results.
  const pagedEntries = useMemo(
    () => Array.from(new Map((paged.data?.pages.flat() ?? []).map((e) => [e.id, e])).values()),
    [paged.data],
  );
  const entries = hasActiveFilters ? pagedEntries : live.entries;
  const isLoading = hasActiveFilters && paged.isPending;
  const liveState = liveStateBadge(paused, live.state);

  let emptyState: ReactNode;
  if (hasActiveFilters && paged.isError) {
    emptyState = (
      <EmptyState
        icon={<CircleAlert />}
        title="Couldn't load queries"
        description="Try adjusting filters or refreshing the page."
      />
    );
  } else if (hasActiveFilters) {
    emptyState = (
      <EmptyState
        icon={<Search />}
        title="No matching queries"
        description="Try a different domain, decision, or clearing filters."
      />
    );
  } else {
    emptyState = (
      <EmptyState
        icon={<Activity />}
        title="Waiting for traffic"
        description="Live queries will appear here as dnsaur resolves them."
      />
    );
  }

  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-3">
        <div>
          <h1 className="text-2xl font-heading font-semibold text-foreground">Query Log</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Live and historical DNS queries, newest first.
          </p>
        </div>

        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <InputGroup className="w-56">
              <InputGroupAddon>
                <Search className="size-4" />
              </InputGroupAddon>
              <InputGroupInput
                aria-label="Search domains"
                placeholder="Search domains…"
                value={searchDraft}
                onChange={(e) => setSearchDraft(e.target.value)}
              />
            </InputGroup>
            <Filters
              filters={rnuiFilters}
              fields={FILTER_FIELDS}
              onChange={setRnuiFilters}
              size="sm"
              trigger={
                <Button type="button" variant="outline" size="sm">
                  <FilterIcon />
                  Filter
                </Button>
              }
            />
          </div>

          {hasActiveFilters ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => {
                setRnuiFilters([]);
                setSearchDraft("");
                setSearch("");
              }}
            >
              Clear filters
            </Button>
          ) : (
            <div className="flex items-center gap-3">
              <StatusIndicator state={liveState.dotState} label={liveState.label} size="sm" />
              {!paused && live.state === "failed" && (
                <Button type="button" variant="outline" size="sm" onClick={live.reconnect}>
                  Reconnect
                </Button>
              )}
              <Switch
                checked={!paused}
                onCheckedChange={(checked) => setPaused(!checked)}
                aria-label="Live tail"
              />
            </div>
          )}
        </div>
      </div>

      <Separator />

      <QueryDataGridPanel
        entries={entries}
        isLoading={isLoading}
        emptyState={emptyState}
        meta={gridMeta}
        onFetchMore={hasActiveFilters ? () => void paged.fetchNextPage() : undefined}
        isFetchingMore={paged.isFetchingNextPage}
        hasMore={hasActiveFilters ? paged.hasNextPage : undefined}
      />

      <Drawer open={whyOpen} onOpenChange={setWhyOpen} direction="right">
        <DrawerContent>
          {whyEntry && (
            <>
              <DrawerHeader>
                <DrawerTitle>Why this decision?</DrawerTitle>
                <DrawerDescription className="font-mono">{whyEntry.q_name}</DrawerDescription>
              </DrawerHeader>
              <div className="flex flex-col gap-4 overflow-y-auto px-4 pb-4">
                <WhyContent
                  entry={whyEntry}
                  rule={whyEntry.rule_id > 0 ? rulesById.get(whyEntry.rule_id) : undefined}
                  ruleLoading={rules.isPending || rules.isFetching}
                  list={whyEntry.list_id > 0 ? listsById.get(whyEntry.list_id) : undefined}
                />
              </div>
            </>
          )}
        </DrawerContent>
      </Drawer>
    </div>
  );
}
