import {
  Fragment,
  memo,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { getCoreRowModel, useReactTable, type ColumnDef, type Table } from "@tanstack/react-table";
import { toast } from "sonner";
import {
  cn,
  DataGrid,
  DataGridContainer,
  DataGridScrollArea,
  DataGridTableVirtual,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Skeleton,
} from "@e412/rnui-react";
import type { Client, List, QueryEntry, Rule } from "../api/types";
import type { SseState } from "../api/sse";
import { StaleDataAlert } from "../components/stale-data-alert";
import { useClients } from "../hooks/use-clients";
import { useAddRule, useRules, useLists } from "../hooks/use-filters";
import {
  DEFAULT_SEARCH_LIMIT,
  LIVE_TAIL_CAP,
  useLiveTail,
  useQuerySearch,
  type QuerySearchFilter,
} from "../hooks/use-queries";
import { useLiveTailPaused } from "../lib/live-tail";
import { durationLabel, rowKey } from "../lib/query-rows";

// The group a query is attributed to when its client matched no client entry
// at all — internal/clients/registry.go's Lookup falls back to
// ClientInfo{GroupID: 1} (with a zero ID) for those, so the query log must
// use the same fallback or its rules would land somewhere the resolver never
// consults for that client.
const DEFAULT_GROUP_ID = 1;

/** Dense row: `py-1.5` twice plus a `text-xs` line box plus the hairline. Only
 * an estimate — the virtualizer measures for real once a row is mounted. */
const ROW_HEIGHT_ESTIMATE = 29;

/**
 * Render counters, exported as a test seam rather than as telemetry.
 *
 * The live tail commits a batch up to ten times a second, and everything in
 * this page's subtree re-renders with it unless a `memo` boundary stops it.
 * That is not a theoretical cost: the identical problem on the dashboard ate
 * the timeline chart's first-hover tooltip, because re-applying an ECharts
 * option tears down its hover state. Here the two expensive, tail-irrelevant
 * subtrees are the filter bar (six controls, one of them a popup menu) and
 * the inspector rail.
 *
 * `memo` only pays off if every prop those two receive is referentially
 * stable across a tail commit, which is easy to break by accident — one
 * inline arrow, or a `useCallback` that closes over a react-query result
 * object instead of its stable `mutate`, and the boundary silently stops
 * working with nothing on screen to show for it. Counting renders is the
 * only way to assert on that, so the counters live here and
 * pages/queries.test.tsx watches them stay flat while rows stream.
 */
export const renderCounts = { filterBar: 0, inspector: 0 };

// --- group resolution --------------------------------------------------------

/**
 * The group whose rules actually govern this row, resolved through the row's
 * client — NOT a hardcoded group 1.
 *
 * `QueryEntry.client_id` is the *client's* id (internal/qlog/qlog.go), so the
 * group comes from that client's `group_id`. Returns null when the row names
 * a client this instance can't resolve (deleted since, or the client list
 * hasn't loaded yet): writing a rule into a guessed group would be a silent
 * no-op for that client, so callers must refuse rather than claim success.
 */
function groupForEntry(entry: QueryEntry, clientGroups: Map<number, number>): number | null {
  if (!entry.client_id) return DEFAULT_GROUP_ID;
  return clientGroups.get(entry.client_id) ?? null;
}

function clientGroupMap(clients: Client[] | undefined): Map<number, number> {
  return new Map((clients ?? []).map((c) => [c.id, c.group_id]));
}

// --- decisions ---------------------------------------------------------------

/**
 * Per-decision tint. The resolver's decision vocabulary is in
 * internal/dnssrv/pipeline.go; this is the design's flat, text-only reading
 * of it — no badges, no icons, because a column of twelve tinted pills is the
 * loudest thing on a page whose whole point is scanning a thousand rows.
 *
 * There is no `allowed` entry, here or in the filter below, on purpose:
 * DecisionAllowed exists in the Go enum but is never assigned. An allow rule
 * only *skips* blocking, so the row is logged with whatever the downstream
 * stage produced. Offering it as a filter advertised a query that always
 * returns zero rows.
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

/** The decisions the resolver actually writes — the filter's whole vocabulary. */
const DECISIONS = ["blocked", "forwarded", "cached", "stale", "local", "error"];

function decisionTone(decision: string): string {
  return DECISION_TONE[decision] ?? "text-muted-foreground";
}

// --- formatting --------------------------------------------------------------

function clockTime(atMs: number): string {
  return new Date(atMs).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// --- shared chrome -----------------------------------------------------------
// The same vocabulary the dashboard uses: hairline-bordered bands, never
// cards. One rule between bands, one between cells, nothing else.

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

/** Prose (alerts, empty sentences, explanations) is the one thing on this
 * page that isn't mono. */
function Prose({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cn("px-4 py-3 font-sans", className)}>{children}</div>;
}

/**
 * Every cell in the filter row: a hairline on the right, one gutter.
 *
 * `shrink-0` and `overflow-hidden` together are what keep the row honest.
 * Six cells plus a status readout do not fit 1280px — the two native
 * `datetime-local` controls alone are ~200px each — so the row scrolls
 * (`dnsaur-scroll-x`, the same treatment both nav strips get) rather than
 * squeezing cells to nothing. Without `shrink-0` the free-text cell
 * collapsed to 53px and its content spilled straight over the DECISION and
 * TYPE cells beside it.
 */
const FILTER_CELL =
  "flex shrink-0 items-center gap-3 overflow-hidden border-r border-border px-4 py-2";

/** The bare, cell-shaped skin every filter control wears — the row is a strip
 * of divided cells, and a bordered input dropped into it looks like something
 * that fell in from another screen. */
const FILTER_INPUT =
  "min-w-0 flex-1 bg-transparent text-xs text-foreground placeholder:text-muted-foreground " +
  "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring";

// --- filter state ------------------------------------------------------------

interface FilterState {
  q: string;
  decision: string;
  type: string;
  client: string;
  /** `datetime-local` values — local wall-clock, converted to epoch ms below. */
  from: string;
  to: string;
}

const NO_FILTERS: FilterState = { q: "", decision: "", type: "", client: "", from: "", to: "" };

/** A `datetime-local` value is local wall-clock with no zone; `new Date(v)`
 * reads it as local time, which is what the user meant. An empty or
 * half-typed value is simply no bound. */
function toEpochMs(value: string): number | undefined {
  if (value === "") return undefined;
  const ms = new Date(value).getTime();
  return Number.isNaN(ms) ? undefined : ms;
}

function toSearchFilter(filters: FilterState, q: string): QuerySearchFilter {
  return {
    q: q.trim() || undefined,
    decision: filters.decision || undefined,
    type: filters.type.trim() || undefined,
    client: filters.client.trim() || undefined,
    from: toEpochMs(filters.from),
    to: toEpochMs(filters.to),
  };
}

function isFiltered(filter: QuerySearchFilter): boolean {
  return Object.values(filter).some((value) => value !== undefined);
}

// --- filter bar --------------------------------------------------------------

function FilterTextCell({
  label,
  value,
  placeholder,
  onChange,
  className,
}: {
  label: string;
  value: string;
  placeholder: string;
  onChange: (value: string) => void;
  className?: string;
}) {
  return (
    <label className={cn(FILTER_CELL, className)}>
      <span className="shrink-0 text-xs tracking-widest text-muted-foreground uppercase">
        {label}
      </span>
      <input
        type="text"
        value={value}
        placeholder={placeholder}
        onChange={(event) => onChange(event.target.value)}
        className={FILTER_INPUT}
      />
    </label>
  );
}

/**
 * The filter row, memoised.
 *
 * Nothing in here has anything to do with arriving rows — it re-renders when
 * a filter changes and at no other time. See `renderCounts` for why that is
 * asserted on rather than assumed.
 */
const FilterBar = memo(function FilterBar({
  value,
  onChange,
}: {
  value: FilterState;
  onChange: (patch: Partial<FilterState>) => void;
}) {
  renderCounts.filterBar += 1;

  return (
    <div className="dnsaur-scroll-x flex min-w-0 flex-1 items-stretch">
      {/* The widest cell, and the only one that grows — but never below a
          usable box, since the row would rather scroll than crush it. */}
      <div className={cn(FILTER_CELL, "min-w-64 flex-1")}>
        <input
          type="text"
          aria-label="Search domains"
          aria-describedby="query-search-hint"
          value={value.q}
          placeholder="Domain contains…"
          onChange={(event) => onChange({ q: event.target.value })}
          className={cn(FILTER_INPUT, "min-w-32")}
        />
        {/* Literally true, not a simplification: internal/store/search.go's
            escapeLike *strips* % and _ rather than escaping them, because
            the escape syntax isn't portable between the two dialects. A
            hint that promised wildcards would be promising a feature the
            server deletes on the way in. */}
        {/* Only where there is genuinely room for it: at 1280 the row is
            already scrolling, and this is the one cell that can afford to
            drop content rather than push the rest off screen. It stays in
            the DOM as the input's description either way. */}
        <span
          id="query-search-hint"
          className="hidden shrink-0 text-xs text-muted-foreground 2xl:inline"
        >
          substring of q_name — % and _ are ignored
        </span>
      </div>

      <div className={FILTER_CELL}>
        <span className="shrink-0 text-xs tracking-widest text-muted-foreground uppercase">
          Decision
        </span>
        {/* A fixed list, so a fixed control: the vocabulary is the
            resolver's six, and "allowed" is not one of them (see
            DECISION_TONE). A free-text box here let people ask for rows
            that cannot exist. */}
        <Select
          items={Object.fromEntries([["any", "any"], ...DECISIONS.map((d) => [d, d])])}
          value={value.decision || "any"}
          onValueChange={(next) => onChange({ decision: next === "any" ? "" : String(next) })}
        >
          <SelectTrigger
            aria-label="Decision"
            className="h-auto w-24 border-0 bg-transparent px-0 py-0 text-xs uppercase shadow-none"
          >
            <SelectValue placeholder="any" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="any">any</SelectItem>
            {DECISIONS.map((decision) => (
              <SelectItem key={decision} value={decision}>
                {decision}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {/* Record types are open-ended (A, AAAA, HTTPS, SVCB, and whatever
          the next RFC adds), so this one stays free text. */}
      <FilterTextCell
        label="Type"
        value={value.type}
        placeholder="any"
        onChange={(type) => onChange({ type })}
        className="w-32"
      />
      <FilterTextCell
        label="Client"
        value={value.client}
        placeholder="any"
        onChange={(client) => onChange({ client })}
        className="w-48"
      />

      <label className={FILTER_CELL}>
        <span className="shrink-0 text-xs tracking-widest text-muted-foreground uppercase">
          From
        </span>
        <input
          type="datetime-local"
          value={value.from}
          onChange={(event) => onChange({ from: event.target.value })}
          className={cn(FILTER_INPUT, "w-36")}
        />
      </label>
      <label className={FILTER_CELL}>
        <span className="shrink-0 text-xs tracking-widest text-muted-foreground uppercase">To</span>
        <input
          type="datetime-local"
          value={value.to}
          onChange={(event) => onChange({ to: event.target.value })}
          className={cn(FILTER_INPUT, "w-36")}
        />
      </label>
    </div>
  );
});

/**
 * Reset, pinned outside the scrolling strip.
 *
 * Six cells is six things to empty by hand, and emptying all of them is the
 * single most common thing anyone does here — it's how you get back to the
 * live tail. Inside the strip it was the last cell, i.e. the one already
 * scrolled off screen exactly when filters are active and it is needed. Only
 * offered once there is something to clear, so the row never carries a
 * permanently inert cell.
 */
function ClearFiltersCell({ onClear }: { onClear: () => void }) {
  return (
    <button
      type="button"
      onClick={onClear}
      className={cn(
        FILTER_CELL,
        "border-r-0 border-l text-xs tracking-widest text-muted-foreground uppercase",
        "transition-colors hover:text-foreground",
        "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring",
      )}
    >
      Clear filters
    </button>
  );
}

// --- stream state ------------------------------------------------------------

/**
 * The tail's own health, at the end of the filter row — directly under the
 * chrome's LIVE · PAUSE cell, which is the control it answers for.
 *
 * The toggle can't carry this itself: it lives in the shell and the stream
 * lives in the page. And "failed" is not a state to render as a quieter
 * shade of the same dot — it means the subscription gave up after six
 * consecutive attempts (api/sse.ts) and nothing further will happen without
 * a click, so it says so and puts the click next to it.
 */
function StreamState({
  paused,
  filtered,
  state,
  onReconnect,
}: {
  paused: boolean;
  filtered: boolean;
  state: SseState;
  onReconnect: () => void;
}) {
  // A filtered view reads the database instead of the stream, so the stream
  // is closed *because you asked for something else* — not a fault.
  if (filtered) {
    return (
      <span className={cn(FILTER_CELL, "border-r-0 border-l")}>
        <Note>Filtered · stream paused</Note>
      </span>
    );
  }
  if (paused) {
    return (
      <span className={cn(FILTER_CELL, "border-r-0 border-l")}>
        <Note>Paused</Note>
      </span>
    );
  }
  if (state === "failed") {
    return (
      <div className={cn(FILTER_CELL, "border-r-0 border-l")}>
        <Note className="text-destructive">Live tail disconnected</Note>
        <button
          type="button"
          onClick={onReconnect}
          className={cn(
            "shrink-0 text-xs tracking-widest uppercase transition-colors hover:text-primary",
            "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
          )}
        >
          Reconnect
        </button>
      </div>
    );
  }
  return (
    <output className={cn(FILTER_CELL, "border-r-0 border-l")}>
      <Note>{state === "open" ? "Streaming" : "Reconnecting…"}</Note>
    </output>
  );
}

// --- table -------------------------------------------------------------------

type RowStatus = "pending" | "blocked" | "allowed";

/**
 * Everything a cell needs that changes while the table is mounted. It travels
 * through the table's `meta` rather than being closed over by the column
 * defs, because a column def array rebuilt per render is poison here: `cell`
 * functions are used as component *types*, so a fresh arrow function per
 * render makes React unmount and remount every visible row, on top of
 * rebuilding getAllColumns/getHeaderGroups — and this table re-renders on
 * every batch of arrivals. With the defs constant, only the cells' output
 * changes.
 */
interface QueryTableMeta {
  selectedKey: string | null;
  onSelect: (entry: QueryEntry) => void;
}

function tableMeta(table: Table<QueryEntry>): QueryTableMeta {
  return table.options.meta as QueryTableMeta;
}

const QUERY_COLUMNS: ColumnDef<QueryEntry>[] = [
  {
    accessorKey: "at",
    header: "Time",
    size: 112,
    meta: {
      headerClassName: "px-4",
      // `relative` so the selection marker can hang off the row's leading
      // edge without a spacer column to live in.
      cellClassName: "relative px-4 whitespace-nowrap text-muted-foreground tabular-nums",
    },
    cell: ({ row, table }) => {
      const selected = rowKey(row.original) === tableMeta(table).selectedKey;
      return (
        <>
          {selected && (
            // Also what tints the row: styles/app.css has no hand-written
            // rule for this — `has-data-selected:bg-card` on the row picks
            // this element up. One marker, both effects.
            <span
              data-selected=""
              aria-hidden="true"
              className="absolute inset-y-0 left-0 w-0.5 bg-primary"
            />
          )}
          {clockTime(row.original.at)}
        </>
      );
    },
  },
  {
    accessorKey: "q_name",
    header: "Domain",
    size: 208,
    meta: { headerClassName: "px-4", cellClassName: "px-4" },
    cell: ({ row, table }) => {
      const entry = row.original;
      return (
        // A real button, not just a click handler on the row: selecting a
        // row is what opens the inspector, and that has to be reachable
        // without a mouse. The row is clickable too (DataGrid's onRowClick),
        // as a convenience on top of this rather than instead of it.
        <button
          type="button"
          aria-label={`Why was ${entry.q_name} ${entry.decision}?`}
          title={entry.q_name}
          onClick={() => tableMeta(table).onSelect(entry)}
          className={cn(
            "block w-full truncate text-left transition-colors hover:text-primary",
            "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring",
          )}
        >
          {entry.q_name}
        </button>
      );
    },
  },
  {
    accessorKey: "q_type",
    header: "Type",
    size: 56,
    meta: { headerClassName: "px-4", cellClassName: "px-4 text-muted-foreground" },
    cell: ({ row }) => row.original.q_type,
  },
  {
    accessorKey: "client_ip",
    header: "Client",
    size: 128,
    meta: { headerClassName: "px-4", cellClassName: "truncate px-4 text-muted-foreground" },
    cell: ({ row }) => row.original.client_ip,
  },
  {
    accessorKey: "decision",
    header: "Decision",
    size: 96,
    meta: { headerClassName: "px-4", cellClassName: "px-4" },
    cell: ({ row }) => (
      <span className={decisionTone(row.original.decision)}>{row.original.decision}</span>
    ),
  },
  {
    accessorKey: "upstream",
    header: "Upstream",
    size: 128,
    meta: { headerClassName: "px-4", cellClassName: "truncate px-4 text-muted-foreground" },
    cell: ({ row }) => row.original.upstream || "—",
  },
  {
    accessorKey: "duration_ms",
    header: "ms",
    size: 56,
    meta: {
      headerClassName: "px-4 text-right",
      cellClassName: "px-4 text-right text-muted-foreground tabular-nums",
    },
    cell: ({ row }) => durationLabel(row.original.duration_ms),
  },
];

function QueryTable({
  entries,
  isLoading,
  emptyMessage,
  meta,
  onFetchMore,
  isFetchingMore,
  hasMore,
}: {
  entries: QueryEntry[];
  isLoading: boolean;
  emptyMessage: ReactNode;
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
    getRowId: rowKey,
    getCoreRowModel: getCoreRowModel(),
  });

  return (
    <DataGrid
      table={table}
      recordCount={entries.length}
      isLoading={isLoading}
      emptyMessage={emptyMessage}
      onRowClick={meta.onSelect}
      fetchingMoreMessage="Loading more matches…"
      allRowsLoadedMessage="All matching queries loaded"
      tableLayout={{
        dense: true,
        width: "fixed",
        rowBorder: true,
        headerBorder: true,
        headerBackground: false,
        headerSticky: true,
      }}
      tableClassNames={{
        headerRow: "text-sm tracking-widest text-muted-foreground uppercase",
        headerSticky: "sticky top-0 z-10 bg-background",
        // `*:` reaches the row's own cells, which is where rowBorder puts
        // the hairline — the design separates rows in the muted tone and
        // keeps full-strength --border for the bands around the table.
        bodyRow: "text-xs *:border-border-muted hover:bg-card has-data-selected:bg-card",
      }}
    >
      <DataGridContainer border={false}>
        {isLoading ? (
          <div className="flex flex-col gap-2 px-4 py-3" aria-hidden="true">
            {Array.from({ length: 12 }).map((_, i) => (
              <Skeleton key={i} className="h-5 w-full" />
            ))}
          </div>
        ) : (
          // Virtualized per the brief: an SSE batch can replace the whole
          // 500-row array, and a plain table would re-render every mounted
          // row on every batch. DataGridTableVirtual only mounts the rows
          // in (or near) the scroll window, so a full-buffer replacement
          // stays cheap however many rows are logically in the array.
          //
          // The same onFetchMore/hasMore props are what makes a filtered
          // view page past its first 100 matches instead of stopping dead
          // there, and a short final page swaps the loader for "All
          // matching queries loaded" so the end is stated, not implied.
          <DataGridScrollArea className="h-136">
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

// --- inspector ---------------------------------------------------------------

interface RawField {
  name: string;
  value: string;
  /** What a zero/empty value actually means. The contract's zero values are
   * not "missing data" — each one says something specific — and an
   * unannotated `list_id 0` reads as a bug in the log. */
  note?: string;
}

function rawRow(entry: QueryEntry): RawField[] {
  return [
    { name: "at", value: String(entry.at), note: new Date(entry.at).toLocaleString() },
    { name: "client_ip", value: entry.client_ip },
    {
      name: "client_id",
      value: String(entry.client_id),
      note: entry.client_id === 0 ? "no client entry matched" : undefined,
    },
    { name: "q_type", value: entry.q_type },
    { name: "decision", value: entry.decision },
    {
      name: "rule_id",
      value: String(entry.rule_id),
      note: entry.rule_id === 0 ? "no rule matched" : undefined,
    },
    {
      name: "list_id",
      value: String(entry.list_id),
      note: entry.list_id === 0 ? "not attributed" : undefined,
    },
    {
      name: "upstream",
      value: entry.upstream || "—",
      note: entry.upstream ? undefined : "never left the box",
    },
    { name: "r_code", value: entry.r_code || "—", note: entry.r_code ? undefined : "no answer" },
    {
      name: "duration_ms",
      value: String(entry.duration_ms),
      note: entry.duration_ms === 0 ? "under 1 ms, truncated" : undefined,
    },
  ];
}

function MatchProse({
  entry,
  rule,
  ruleLoading,
  list,
}: {
  entry: QueryEntry;
  rule?: Rule;
  /** The row's group's rules are still in flight — see whyGroupId. */
  ruleLoading?: boolean;
  list?: List;
}) {
  if (entry.rule_id > 0) {
    if (rule) {
      return (
        <p>
          Matched a {rule.action === "block" ? "block" : "allow"} rule for{" "}
          <code className="font-mono text-foreground">{rule.pattern}</code>
          {rule.is_regex ? " (regex)" : ""}.
        </p>
      );
    }
    // Never claim the rule is missing while its group's rules are still in
    // flight — the rail populates before that request comes back.
    if (ruleLoading) return <p>Looking up the matching rule…</p>;
    return (
      <p>
        Matched rule #{entry.rule_id}, which isn&apos;t available right now (it may have been
        deleted).
      </p>
    );
  }
  if (entry.list_id > 0) {
    return list ? (
      <p>
        From the {list.kind} list{" "}
        <code className="font-mono break-all text-foreground">{list.url}</code>.
      </p>
    ) : (
      <p>Matched list #{entry.list_id}, which isn&apos;t available right now.</p>
    );
  }
  return <p>No rule or list matched — resolved by the default policy.</p>;
}

/** The inspector's two-cell action strip. The primary cell is the design's
 * one filled control on this rail, and which rule it writes follows the row:
 * a blocked row needs an allow, anything else needs a block. */
function ActionStrip({
  entry,
  status,
  disabled,
  disabledReason,
  onQuickRule,
  onCopy,
}: {
  entry: QueryEntry;
  status?: RowStatus;
  disabled: boolean;
  disabledReason: string;
  onQuickRule: (action: "allow" | "block", entry: QueryEntry) => void;
  onCopy: (entry: QueryEntry) => void;
}) {
  const action = entry.decision === "blocked" ? "allow" : "block";
  const done = status === "blocked" || status === "allowed";
  const cell =
    "flex items-center justify-center px-4 py-3 text-xs tracking-widest uppercase transition-colors " +
    "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring";

  return (
    <div className="grid grid-cols-2 border-t border-border">
      {done ? (
        <span className={cn(cell, "bg-primary font-semibold text-primary-foreground")}>
          {status === "blocked" ? "Blocked" : "Allowed"}
        </span>
      ) : (
        <button
          type="button"
          disabled={status === "pending" || disabled}
          title={disabled ? disabledReason : undefined}
          onClick={() => onQuickRule(action, entry)}
          className={cn(
            cell,
            "bg-primary font-semibold text-primary-foreground hover:bg-primary/90",
            // Still visible while unavailable, just visibly inert and
            // carrying the reason — silently having no affordance is how
            // "why can't I block from here?" starts.
            "disabled:cursor-not-allowed disabled:opacity-60 disabled:hover:bg-primary",
          )}
        >
          {action === "allow" ? "Allow domain" : "Block domain"}
        </button>
      )}
      <button
        type="button"
        onClick={() => onCopy(entry)}
        className={cn(cell, "border-l border-border hover:bg-card")}
      >
        Copy row JSON
      </button>
    </div>
  );
}

/**
 * The right-hand rail: why this row got the decision it did, and the raw
 * record behind it.
 *
 * Memoised for the same reason the filter bar is — none of it is a function
 * of arriving rows, and it is the most expensive subtree on the page.
 * Replaced the slide-over drawer the log used to have: a drawer over a log
 * hides the rows you are comparing against, and every "why?" needed a click
 * to open and an Escape to get back. The rail is always there, so selecting
 * a row *is* opening it.
 */
const Inspector = memo(function Inspector({
  entry,
  rule,
  ruleLoading,
  list,
  status,
  actionsDisabled,
  actionsDisabledReason,
  onClose,
  onQuickRule,
  onCopy,
}: {
  entry: QueryEntry | null;
  rule?: Rule;
  ruleLoading?: boolean;
  list?: List;
  status?: RowStatus;
  actionsDisabled: boolean;
  actionsDisabledReason: string;
  onClose: () => void;
  onQuickRule: (action: "allow" | "block", entry: QueryEntry) => void;
  onCopy: (entry: QueryEntry) => void;
}) {
  renderCounts.inspector += 1;

  return (
    <aside
      aria-labelledby="why-title"
      className="flex shrink-0 flex-col max-lg:border-t max-lg:border-border lg:w-96 lg:border-l lg:border-border"
    >
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
        <SectionTitle id="why-title">Why this decision</SectionTitle>
        {entry && (
          <button
            type="button"
            aria-label="Clear the selected row"
            onClick={onClose}
            className={cn(
              "shrink-0 text-xs tracking-widest text-muted-foreground uppercase",
              "transition-colors hover:text-foreground",
              "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
            )}
          >
            Close <span aria-hidden="true">×</span>
          </button>
        )}
      </div>

      {entry === null ? (
        <Prose>
          <p className="text-sm text-muted-foreground">
            Pick a row to see which rule, list or default policy decided it — and the raw record
            behind it.
          </p>
        </Prose>
      ) : (
        <>
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="flex flex-col gap-2 border-b border-border px-4 py-3">
              <p className="text-base break-all">{entry.q_name}</p>
              <div className="flex flex-wrap items-center gap-3">
                <span
                  className={cn(
                    "border border-current px-2 py-0.5 text-xs tracking-widest uppercase",
                    decisionTone(entry.decision),
                  )}
                >
                  {entry.decision}
                </span>
                <Note>
                  {entry.q_type} · {entry.r_code || "no answer"} ·{" "}
                  {durationLabel(entry.duration_ms)} ms
                </Note>
              </div>
            </div>

            <Prose className="border-b border-border text-sm text-muted-foreground">
              <MatchProse entry={entry} rule={rule} ruleLoading={ruleLoading} list={list} />
            </Prose>

            <div className="px-4 py-3">
              <SectionTitle>Raw row</SectionTitle>
              <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1.5 text-xs">
                {rawRow(entry).map((field) => (
                  <Fragment key={field.name}>
                    <dt className="text-muted-foreground">{field.name}</dt>
                    <dd className="break-all">
                      {field.value}
                      {field.note !== undefined && (
                        <span className="text-muted-foreground"> {field.note}</span>
                      )}
                    </dd>
                  </Fragment>
                ))}
              </dl>
            </div>
          </div>

          <ActionStrip
            entry={entry}
            status={status}
            disabled={actionsDisabled}
            disabledReason={actionsDisabledReason}
            onQuickRule={onQuickRule}
            onCopy={onCopy}
          />
        </>
      )}
    </aside>
  );
});

// --- footer ------------------------------------------------------------------

function FooterButton({
  children,
  disabled,
  onClick,
}: {
  children: ReactNode;
  disabled?: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className={cn(
        "text-xs tracking-widest text-muted-foreground uppercase transition-colors",
        "hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50",
        "disabled:hover:text-muted-foreground",
        "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
      )}
    >
      {children}
    </button>
  );
}

function Footer({
  rows,
  filtered,
  offset,
  hasMore,
  isFetchingMore,
  onOlder,
  onNewer,
}: {
  rows: number;
  filtered: boolean;
  offset: number;
  hasMore: boolean;
  isFetchingMore: boolean;
  onOlder: () => void;
  onNewer: () => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-border px-4 py-3">
      <Note>
        {rows} rows ·{" "}
        {filtered
          ? `limit ${DEFAULT_SEARCH_LIMIT} · offset ${offset}`
          : `live tail · ${LIVE_TAIL_CAP} row buffer`}
      </Note>
      {/* The search endpoint is `ORDER BY id DESC` (internal/store/search.go),
          and id order is insertion order — which is the logger's batched
          flush order, not the order the queries were answered in. Close
          enough to always look like time, wrong often enough to say so. */}
      {filtered && <Note>ordered by id, not by time</Note>}
      <div className="ml-auto flex items-center gap-4">
        <FooterButton onClick={onNewer}>
          <span aria-hidden="true">←</span> Newer
        </FooterButton>
        <FooterButton disabled={!filtered || !hasMore || isFetchingMore} onClick={onOlder}>
          Older <span aria-hidden="true">→</span>
        </FooterButton>
      </div>
    </div>
  );
}

// --- page --------------------------------------------------------------------

export function QueryLog() {
  const [filters, setFilters] = useState<FilterState>(NO_FILTERS);
  // Light debounce so the domain box doesn't fire a LIKE query per keystroke.
  // The other five are discrete selections, so they apply immediately.
  const [debouncedQ, setDebouncedQ] = useState("");
  const [selected, setSelected] = useState<QueryEntry | null>(null);
  // The toggle itself is the chrome's one filled cell (components/top-nav.tsx);
  // this page is only ever a reader of the flag. See lib/live-tail.ts.
  const { paused } = useLiveTailPaused();

  useEffect(() => {
    const timer = setTimeout(() => setDebouncedQ(filters.q), 300);
    return () => clearTimeout(timer);
  }, [filters.q]);

  const searchFilter = useMemo(() => toSearchFilter(filters, debouncedQ), [filters, debouncedQ]);
  const filtered = isFiltered(searchFilter);

  // Live tail and paged search share one `enabled` each: the tail stops
  // consuming the stream (see use-queries.ts) the moment either a filter is
  // applied or the user pauses; the paged search only runs while filters are
  // active. Neither ever clears what's already rendered.
  const live = useLiveTail(!filtered && !paused);
  const paged = useQuerySearch(searchFilter, { enabled: filtered });

  // Every quick rule and the inspector's rule lookup are scoped to the group
  // that actually governs the row's client (see groupForEntry) — a rule
  // written into group 1 for a client that lives in group 3 is a no-op the
  // UI would otherwise report as a success.
  const clients = useClients();
  const clientGroups = useMemo(() => clientGroupMap(clients.data), [clients.data]);
  const selectedGroupId = selected ? groupForEntry(selected, clientGroups) : null;

  const rules = useRules(selectedGroupId ?? DEFAULT_GROUP_ID);
  const lists = useLists();
  const rulesById = useMemo(() => new Map((rules.data ?? []).map((r) => [r.id, r])), [rules.data]);
  const listsById = useMemo(() => new Map((lists.data ?? []).map((l) => [l.id, l])), [lists.data]);

  // `mutate`, not the mutation object: useMutation hands back a fresh result
  // object on every render, so closing over it would give `quickRule` a new
  // identity ten times a second and quietly defeat the inspector's memo.
  // `mutate` itself is stable for the life of the component.
  const { mutate: addRule } = useAddRule();
  const [rowStatus, setRowStatus] = useState<Record<string, RowStatus>>({});

  const quickRule = useCallback(
    (action: "allow" | "block", entry: QueryEntry) => {
      const verb = action === "block" ? "block" : "allow";
      const groupId = groupForEntry(entry, clientGroups);
      if (groupId === null) {
        toast.error(`Couldn't ${verb} ${entry.q_name} — this client's group is unknown`);
        return;
      }
      const key = rowKey(entry);
      setRowStatus((s) => ({ ...s, [key]: "pending" }));
      addRule(
        { groupId, action, pattern: entry.q_name },
        {
          onSuccess: () => {
            setRowStatus((s) => ({ ...s, [key]: action === "block" ? "blocked" : "allowed" }));
            toast.success(
              action === "block" ? `Blocked ${entry.q_name}` : `Allowed ${entry.q_name}`,
            );
          },
          onError: () => {
            setRowStatus((s) => {
              const next = { ...s };
              delete next[key];
              return next;
            });
            toast.error(`Couldn't ${verb} ${entry.q_name}`);
          },
        },
      );
    },
    [addRule, clientGroups],
  );

  const copyRow = useCallback((entry: QueryEntry) => {
    // Optional-chaining the call would `await undefined` and then toast a
    // success for a copy that never happened — the clipboard API is absent
    // outside secure contexts, which is exactly where a homelab instance
    // reached over plain HTTP lives.
    if (!navigator.clipboard) {
      toast.error("Couldn't copy — this browser won't allow clipboard access here");
      return;
    }
    navigator.clipboard.writeText(JSON.stringify(entry, null, 2)).then(
      () => toast.success("Row JSON copied"),
      () => toast.error("Couldn't copy the row JSON"),
    );
  }, []);

  const onSelect = useCallback((entry: QueryEntry) => setSelected(entry), []);
  const onClearSelection = useCallback(() => setSelected(null), []);
  const patchFilters = useCallback(
    (patch: Partial<FilterState>) => setFilters((f) => ({ ...f, ...patch })),
    [],
  );
  const clearFilters = useCallback(() => setFilters(NO_FILTERS), []);
  // The debounced `q` lags the box by 300ms, so "anything typed at all"
  // isn't the same question as "anything is being searched for": the Clear
  // cell has to appear the moment there is something to clear.
  const anyFilterSet = filtered || filters.q.trim() !== "";

  // Without the client list, groupForEntry can only answer null for any row
  // that names a client — which the quick rule reports as "this client's
  // group is unknown", a message about a *deleted* client. When the real
  // cause is a /clients request still in flight or failed, that's both wrong
  // and unactionable, so the action is held closed and says why instead.
  const clientsUnavailable = clients.isPending || clients.isError;
  const actionsDisabledReason = clients.isError
    ? "Couldn't load clients — reload to write rules from here"
    : "Loading clients…";

  const selectedKey = selected ? rowKey(selected) : null;
  const gridMeta: QueryTableMeta = useMemo(
    () => ({ selectedKey, onSelect }),
    [selectedKey, onSelect],
  );

  // Deduplicated by id, not a bare flat(): the search endpoint pages by
  // `ORDER BY id DESC LIMIT ? OFFSET ?` (internal/store/search.go) with the
  // running row count as the next offset, so any query logged between the
  // page-1 and page-2 fetches shifts every row down one and page 2
  // re-returns the tail of page 1. Those duplicates reach react-table and
  // the virtualizer as duplicate keys: React logs a duplicate-key warning
  // and the same domain renders twice around the boundary. Deduping here
  // rather than in the pageParam keeps hasNextPage correct — termination
  // still reads the *raw* page length, so a page that is short only because
  // it overlapped isn't mistaken for the end of the results.
  const pages = paged.data?.pages;
  const pagedEntries = useMemo(
    () => Array.from(new Map((pages?.flat() ?? []).map((e) => [e.id, e])).values()),
    [pages],
  );
  const entries = filtered ? pagedEntries : live.entries;
  const isLoading = filtered && paged.isPending;
  // The offset the last page was actually asked for: the sum of every page
  // before it, which is exactly what getNextPageParam handed the fetch.
  const offset = (pages ?? []).slice(0, -1).reduce((n, page) => n + page.length, 0);

  const tableRef = useRef<HTMLDivElement>(null);
  const scrollToNewest = useCallback(() => {
    const viewport = tableRef.current?.querySelector('[data-slot="scroll-area-viewport"]');
    if (viewport) viewport.scrollTop = 0;
  }, []);

  let emptyMessage: ReactNode;
  if (filtered && paged.isError) {
    emptyMessage = "Couldn't load queries. Try adjusting the filters, or reload the page.";
  } else if (filtered) {
    emptyMessage = "No queries match these filters.";
  } else if (paused) {
    emptyMessage = "Paused. Resume the live tail to start appending rows.";
  } else {
    emptyMessage = "Listening — queries appear here as dnsaur answers them.";
  }

  return (
    // A column that claims the whole content area (the shell drops its
    // padding for this route — see components/app-shell.tsx), so the split
    // below can take every pixel left over and its vertical rule can run to
    // the bottom of the window.
    <div className="flex flex-1 flex-col font-mono">
      {/* The chrome's own tab already says Query Log, and the design gives
          the page no visible title — but a page still needs one heading. */}
      <h1 className="sr-only">Query log</h1>

      <div className="flex items-stretch border-b border-border">
        <FilterBar value={filters} onChange={patchFilters} />
        <div className="flex shrink-0 items-stretch">
          {anyFilterSet && <ClearFiltersCell onClear={clearFilters} />}
          <StreamState
            paused={paused}
            filtered={filtered}
            state={live.state}
            onReconnect={live.reconnect}
          />
        </div>
      </div>

      <div className="flex flex-1 flex-col lg:flex-row lg:items-stretch">
        <div ref={tableRef} className="flex min-w-0 flex-1 flex-col">
          {/* The one surface this page never had: a background or next-page
              fetch that fails with rows still in hand used to render
              nothing at all, leaving a stale table that looked live. Every
              other screen shows this over still-valid data. */}
          {paged.isError && pagedEntries.length > 0 && (
            <Prose className="border-b border-border">
              <StaleDataAlert
                what="the query log"
                onRetry={() => void paged.refetch()}
                isRetrying={paged.isFetching}
              />
            </Prose>
          )}
          <QueryTable
            entries={entries}
            isLoading={isLoading}
            emptyMessage={emptyMessage}
            meta={gridMeta}
            onFetchMore={filtered ? () => void paged.fetchNextPage() : undefined}
            isFetchingMore={paged.isFetchingNextPage}
            hasMore={filtered ? paged.hasNextPage : undefined}
          />
        </div>

        <Inspector
          entry={selected}
          rule={selected && selected.rule_id > 0 ? rulesById.get(selected.rule_id) : undefined}
          ruleLoading={rules.isPending || rules.isFetching}
          list={selected && selected.list_id > 0 ? listsById.get(selected.list_id) : undefined}
          status={selectedKey === null ? undefined : rowStatus[selectedKey]}
          actionsDisabled={clientsUnavailable}
          actionsDisabledReason={actionsDisabledReason}
          onClose={onClearSelection}
          onQuickRule={quickRule}
          onCopy={copyRow}
        />
      </div>

      <Footer
        rows={entries.length}
        filtered={filtered}
        offset={offset}
        hasMore={paged.hasNextPage}
        isFetchingMore={paged.isFetchingNextPage}
        onOlder={() => void paged.fetchNextPage()}
        onNewer={scrollToNewest}
      />
    </div>
  );
}
