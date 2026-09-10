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
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  cn,
  DataGrid,
  DataGridContainer,
  DataGridScrollArea,
  DataGridTableVirtual,
  DateSelector,
  formatDateValue,
  Input,
  NativeSelect,
  NativeSelectOption,
  Popover,
  PopoverContent,
  PopoverTrigger,
  Skeleton,
  type BadgeProps,
  type DateSelectorPeriodType,
  type DateSelectorValue,
} from "@e412/rnui-react";
import type { Client, List, QueryEntry, Rule } from "../api/types";
import { StaleDataAlert } from "../components/stale-data-alert";
import { useClients } from "../hooks/use-clients";
import { useGroups } from "../hooks/use-groups";
import { useSettings } from "../hooks/use-settings";
import { useAddRule, useRules, useLists } from "../hooks/use-filters";
import {
  DEFAULT_SEARCH_LIMIT,
  LIVE_TAIL_CAP,
  useLiveTail,
  useQuerySearch,
  type QuerySearchFilter,
} from "../hooks/use-queries";
import { useLiveTailPaused, usePublishLiveTailStatus } from "../lib/live-tail";
import {
  clockTime,
  DEFAULT_GROUP_ID,
  decisionTone,
  durationLabel,
  namesByKey,
  rowKey,
} from "../lib/query-rows";
import { parseUpstreams } from "../lib/upstreams";

/** Dense row: `py-1.5` twice plus a `text-xs` line box plus the hairline. Only
 * an estimate — the virtualizer measures for real once a row is mounted. */
const ROW_HEIGHT_ESTIMATE = 29;

/** What the log renders where a value genuinely isn't knowable. Never a
 * guess, and never a blank cell that reads as a rendering bug. */
const UNKNOWN = "—";

/**
 * What to show in the Upstream column: the address the query actually went
 * to.
 *
 * Response.Upstream is the canonical entry, which for an encrypted upstream
 * is the whole thing — `tls://1.1.1.1:853#cloudflare-dns.com`, 37 characters
 * into a 128px truncating cell. Rendered whole, every DoT row read as the
 * same `tls://1.1.1.1:85…` prefix, so the two upstreams of a Cloudflare
 * preset were indistinguishable exactly when there was a second one worth
 * telling apart.
 *
 * The address is the part that differs per row (every upstream in a list
 * shares one scheme — see internal/upstream/addr.go's mixed_schemes rule),
 * and it is what this column showed before encrypted transports existed. The
 * full canonical form is on the cell's `title`, and in the inspector rail
 * unabridged.
 *
 * parseUpstreams rather than a local split: it is the grammar this string
 * was produced by. Anything it does not recognise is shown verbatim — a
 * truncated value beats an empty cell.
 */
function upstreamAddr(canonical: string): string {
  const parsed = parseUpstreams(canonical);
  return parsed.ok && parsed.entries.length === 1 ? parsed.entries[0].addr : canonical;
}

/**
 * Render counters, exported as a test seam rather than as telemetry.
 *
 * The live tail commits a batch up to ten times a second, and everything in
 * this page's subtree re-renders with it unless a `memo` boundary stops it.
 * That is not a theoretical cost: the identical problem on the dashboard ate
 * the timeline chart's first-hover tooltip, because re-applying an ECharts
 * option tears down its hover state. Here the two expensive, tail-irrelevant
 * subtrees are the filter bar (four controls plus a date-picker popover) and
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

/**
 * `client_id` → that client's name, for the table's HOSTNAME column.
 *
 * Keyed on the id rather than on the IP: `client_id` is a real foreign key
 * (internal/qlog/qlog.go writes whatever the registry matched), so it
 * resolves a name even for a client whose matcher is a CIDR and therefore
 * never equals any single `client_ip`. Rows whose `client_id` is 0, or names
 * a client deleted since the query was logged, are simply absent from the
 * map — the column renders UNKNOWN for those rather than inventing a name.
 * So is a client whose name is empty: see namesByKey.
 */
function clientNameMap(clients: Client[] | undefined): Map<number, string> {
  return namesByKey(clients, (c) => c.id);
}

// --- decisions ---------------------------------------------------------------

/**
 * The inspector chip's tone, as an rnui Badge variant.
 *
 * The rail shows one row at a time, so a chip here costs nothing and reads
 * far faster than a word in a tint. The "-light" variants are the ones whose
 * foreground tokens are tuned for a 10% wash of their own colour (see
 * styles/dnsaur-theme.css) — the solid ones hardcode `text-white`, which
 * this theme's dark mode can't carry.
 */
const DECISION_BADGE: Record<string, NonNullable<BadgeProps["variant"]>> = {
  blocked: "destructive-light",
  error: "destructive-light",
  stale: "warning-light",
  authoritative: "primary-light",
  cached: "secondary",
  forwarded: "outline",
};

/**
 * The decisions the resolver actually writes — the filter's whole
 * vocabulary. There is no `allowed` in it, on purpose: there is no such
 * decision in the Go enum (see lib/query-rows.ts's tone map). An allow rule
 * only *skips* blocking, so the row is logged with whatever the downstream
 * stage produced, and offering it advertised a query that always returns
 * zero rows.
 */
const DECISIONS = ["blocked", "forwarded", "cached", "stale", "authoritative", "error"];

/**
 * The record types worth offering as a fixed list.
 *
 * `q_type` is open-ended in the contract (whatever miekg/dns names), but a
 * free-text box here was a licence to type `a` and get nothing back — the
 * server matches exactly, uppercase as stored. These five are what a home
 * resolver actually answers in bulk; anything rarer is still reachable
 * through the domain search.
 */
const QUERY_TYPES = ["A", "AAAA", "HTTPS", "PTR", "TXT"];

function decisionBadge(decision: string): NonNullable<BadgeProps["variant"]> {
  return DECISION_BADGE[decision] ?? "outline";
}

// --- shared chrome -----------------------------------------------------------
// The same vocabulary the dashboard uses: hairline-bordered bands, never
// cards. One rule between bands, one between cells, nothing else.

function SectionTitle({ id, children }: { id?: string; children: ReactNode }) {
  return (
    <h2 id={id} className="text-xs tracking-widest uppercase">
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

/** The label beside each filter control. */
const FILTER_LABEL = "shrink-0 text-xs tracking-widest text-muted-foreground uppercase";

// --- filter state ------------------------------------------------------------

interface FilterState {
  q: string;
  decision: string;
  type: string;
  client: string;
  /** Inclusive epoch-ms bounds, straight from the DateSelector — the shape
   * `GET /queries` wants, applied only when set (internal/store/search.go). */
  from?: number;
  to?: number;
}

const NO_FILTERS: FilterState = { q: "", decision: "", type: "", client: "" };

function toSearchFilter(filters: FilterState, q: string): QuerySearchFilter {
  return {
    q: q.trim() || undefined,
    decision: filters.decision || undefined,
    type: filters.type || undefined,
    client: filters.client || undefined,
    from: filters.from,
    to: filters.to,
  };
}

function isFiltered(filter: QuerySearchFilter): boolean {
  return Object.values(filter).some((value) => value !== undefined);
}

// --- the time range ----------------------------------------------------------

/**
 * Only day and month.
 *
 * `qlog.retention_days` defaults to 90 (docs/ui-contract.md §5), so a
 * quarter is already at the edge of what can still be in the table and a
 * half-year or year selects a window whose rows the pruner deleted. Offering
 * those would be the `allowed`-filter mistake again: a control that looks
 * like it narrows the search and can only ever return nothing.
 */
const RANGE_PERIODS: DateSelectorPeriodType[] = ["day", "month"];

/**
 * And only two years, for the same reason: 90 days of retention can straddle
 * a new year but nothing further back, so a year picker offering 2015 offers
 * a decade of guaranteed-empty results.
 */
const THIS_YEAR = new Date().getFullYear();

/**
 * ISO, not the component's `MM/dd/yyyy` default.
 *
 * Every other date and number on this page is mono and unambiguous, and
 * `03/04` means two different days either side of the Atlantic — which is a
 * coin flip this screen has no way to resolve for a homelab that could be
 * anywhere. `inputHint` alongside it is what makes the box typeable at all
 * (the component leaves it readonly without one), so a date can be entered
 * rather than clicked to.
 */
const DAY_FORMAT = "yyyy-MM-dd";
const DAY_HINT = "YYYY-MM-DD";

/** Midnight local on `d`, and the last millisecond of that same day. */
function dayBounds(d: Date): [number, number] {
  const start = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  return [start, new Date(d.getFullYear(), d.getMonth(), d.getDate() + 1).getTime() - 1];
}

/** Midnight local on the 1st, and the last millisecond of that month. */
function monthBounds(year: number, month: number): [number, number] {
  return [new Date(year, month, 1).getTime(), new Date(year, month + 1, 1).getTime() - 1];
}

/**
 * The bounds of the single period a DateSelectorValue names — the day it
 * points at, or the month. Null while nothing has been picked yet, which is
 * also the value the component emits on mount.
 */
function periodBounds(value: DateSelectorValue): [number, number] | null {
  if (value.period === "month") {
    if (value.year === undefined || value.month === undefined) return null;
    return monthBounds(value.year, value.month);
  }
  return value.startDate ? dayBounds(value.startDate) : null;
}

/** The bounds of the *end* of a `between` selection, which is a second day or
 * a second month rather than a second copy of the first. */
function rangeEndBounds(value: DateSelectorValue): [number, number] | null {
  if (value.period === "month") {
    return value.rangeEnd ? monthBounds(value.rangeEnd.year, value.rangeEnd.value) : null;
  }
  return value.endDate ? dayBounds(value.endDate) : null;
}

/** The start of a `between` selection — the range anchor for months, the
 * start date for days. */
function rangeStartBounds(value: DateSelectorValue): [number, number] | null {
  if (value.period === "month") {
    return value.rangeStart ? monthBounds(value.rangeStart.year, value.rangeStart.value) : null;
  }
  return value.startDate ? dayBounds(value.startDate) : null;
}

/**
 * A DateSelectorValue as the two inclusive epoch-ms bounds `GET /queries`
 * takes. The operator is what decides which end of the picked period each
 * bound comes from:
 *
 *   is       whole period          from = period start, to = period end
 *   after    that period onwards   from = period start, to unset
 *   before   up to that period     from unset,          to = period end
 *   between  two periods           from = first start,  to = second end
 *
 * `between` is emitted mid-selection too, with only the first end picked;
 * that reads as "from there onwards" (an open `to`) rather than silently
 * collapsing to a single day the user never asked for.
 *
 * Both keys are ALWAYS present, `undefined` where the operator leaves that
 * end open. That is load-bearing, not tidiness: the caller applies this over
 * the existing filters, and a key that is merely absent cannot clear a bound
 * a previous selection set. Returning `{ from }` for `after` left the `to`
 * from an earlier `is` in place, so "after the 5th" was silently still
 * capped at the end of the 5th — a filter the user could no longer see (the
 * picker clears its own selection when the operator changes, so the trigger
 * had stopped naming the range it was still applying) and could only get out
 * of via Clear filters. Same for `before` and a stale `from`, and for the
 * emitted-on-mount empty value, which used to leave the whole range applied.
 */
function toEpochRange(value: DateSelectorValue): {
  from: number | undefined;
  to: number | undefined;
} {
  const none = { from: undefined, to: undefined };
  if (value.operator === "between") {
    const start = rangeStartBounds(value);
    const end = rangeEndBounds(value);
    if (start === null) return none;
    return { from: start[0], to: end?.[1] };
  }
  const bounds = periodBounds(value);
  if (bounds === null) return none;
  if (value.operator === "after") return { from: bounds[0], to: undefined };
  if (value.operator === "before") return { from: undefined, to: bounds[1] };
  return { from: bounds[0], to: bounds[1] };
}

/** What the trigger says once something is picked: the operator plus the
 * period, because "08/06/2026" alone doesn't say whether it means that day,
 * everything before it, or everything after. */
function rangeSummary(value: DateSelectorValue | null): string {
  if (value === null) return "";
  const formatted = formatDateValue(value, undefined, DAY_FORMAT);
  return formatted === "" ? "" : `${value.operator} ${formatted}`;
}

// --- filter bar --------------------------------------------------------------

/** One labelled select in the filter strip. The `<label>` wraps the control,
 * so the visible label *is* the accessible name — no aria-label to drift. */
function FilterSelect({
  label,
  value,
  onChange,
  className,
  disabled = false,
  children,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  className?: string;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <label className="flex shrink-0 items-center gap-2">
      <span className={FILTER_LABEL}>{label}</span>
      <NativeSelect
        size="sm"
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
        className={cn("text-xs", className)}
      >
        {children}
      </NativeSelect>
    </label>
  );
}

/** An option per known client, `<ip> <name>`. */
interface ClientOption {
  ip: string;
  name: string;
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
  clients,
  clientIpsMasked,
  resetToken,
  onChange,
  onTimeRange,
}: {
  value: FilterState;
  clients: ClientOption[];
  /** `qlog.privacy` is not `full`, so a stored `client_ip` is masked or
   * absent and can never equal a client's exact-IP matcher. */
  clientIpsMasked: boolean;
  /** Bumped when the filters are cleared, so the DateSelector — which keeps
   * its own selection internally — is remounted empty rather than left
   * showing a range the page is no longer filtering on. */
  resetToken: number;
  onChange: (patch: Partial<FilterState>) => void;
  onTimeRange: (value: DateSelectorValue) => void;
}) {
  renderCounts.filterBar += 1;

  // The picked value, kept here purely to label the trigger. The page only
  // ever wants the two epoch bounds, and DateSelector is left uncontrolled:
  // it syncs from a `value` prop inside an effect, so feeding it back a
  // freshly-built object every render would loop.
  const [range, setRange] = useState<DateSelectorValue | null>(null);
  const handleRange = useCallback(
    (next: DateSelectorValue) => {
      setRange(next);
      onTimeRange(next);
    },
    [onTimeRange],
  );

  const summary = rangeSummary(range);

  return (
    <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-4 gap-y-2">
      <Input
        aria-label="Search domains"
        aria-describedby="query-search-hint"
        value={value.q}
        placeholder="search q_name…"
        onChange={(event) => onChange({ q: event.target.value })}
        className="h-7 min-w-48 flex-1 text-xs md:text-xs"
      />
      {/* Literally true, not a simplification: internal/store/search.go's
          escapeLike *strips* % and _ rather than escaping them, because the
          escape syntax isn't portable between the two dialects. A hint that
          promised wildcards would be promising a feature the server deletes
          on the way in. Only shown where there is genuinely room for it; it
          stays in the DOM as the input's description either way. */}
      <span
        id="query-search-hint"
        className="hidden shrink-0 text-xs text-muted-foreground 2xl:inline"
      >
        substring of q_name — % and _ are ignored
      </span>

      {/* A fixed list, so a fixed control: the vocabulary is the resolver's
          six, and "allowed" is not one of them (see DECISION_TONE). A
          free-text box here let people ask for rows that cannot exist. */}
      <FilterSelect
        label="Decision"
        value={value.decision}
        onChange={(decision) => onChange({ decision })}
      >
        <NativeSelectOption value="">All decisions</NativeSelectOption>
        {DECISIONS.map((decision) => (
          <NativeSelectOption key={decision} value={decision}>
            {decision}
          </NativeSelectOption>
        ))}
      </FilterSelect>

      <FilterSelect label="Type" value={value.type} onChange={(type) => onChange({ type })}>
        <NativeSelectOption value="">All types</NativeSelectOption>
        {QUERY_TYPES.map((type) => (
          <NativeSelectOption key={type} value={type}>
            {type}
          </NativeSelectOption>
        ))}
      </FilterSelect>

      {/* `client` is an exact match on the stored `client_ip`
          (ui-contract §2.7), and `qlog.privacy=anon` masks the last octet on
          the way in (internal/qlog/qlog.go's anonymize) while `none` records
          nothing at all. Under either, every option here names an address
          the log cannot hold — so the control says what is true instead of
          offering a query that always returns nothing. */}
      <FilterSelect
        label="Client"
        value={value.client}
        onChange={(client) => onChange({ client })}
        className="max-w-56"
        disabled={clientIpsMasked}
      >
        {clientIpsMasked ? (
          <NativeSelectOption value="">Client IPs masked</NativeSelectOption>
        ) : (
          <>
            <NativeSelectOption value="">Any client IP</NativeSelectOption>
            {clients.map((client) => (
              <NativeSelectOption key={client.ip} value={client.ip}>
                {client.ip} {client.name}
              </NativeSelectOption>
            ))}
          </>
        )}
      </FilterSelect>

      {/* The two `datetime-local` boxes this replaced were ~170px each and
          still couldn't express "everything before Tuesday". Behind a
          trigger, the whole vocabulary (is / before / after / between, by
          day or by month) costs one button's worth of row. */}
      <Popover>
        <PopoverTrigger render={<Button type="button" size="sm" variant="outline" />}>
          Time range
          {summary !== "" && <span className="text-muted-foreground">· {summary}</span>}
        </PopoverTrigger>
        <PopoverContent align="end" className="w-auto">
          <DateSelector
            key={resetToken}
            label="Time range"
            periodTypes={RANGE_PERIODS}
            defaultPeriodType="day"
            defaultFilterType="is"
            minYear={THIS_YEAR - 1}
            maxYear={THIS_YEAR}
            showTwoMonths={false}
            dayDateFormat={DAY_FORMAT}
            inputHint={DAY_HINT}
            onChange={handleRange}
            className="sm:w-80"
          />
        </PopoverContent>
      </Popover>
    </div>
  );
});

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
  /** `client_id` → hostname, for the HOSTNAME column. See clientNameMap. */
  hostnames: Map<number, string>;
  onSelect: (entry: QueryEntry) => void;
}

function tableMeta(table: Table<QueryEntry>): QueryTableMeta {
  return table.options.meta as QueryTableMeta;
}

const QUERY_COLUMNS: ColumnDef<QueryEntry>[] = [
  {
    accessorKey: "at",
    header: "Time",
    size: 104,
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
      // `q_name` is "" when the query carried no question (ui-contract
      // §3.1). An empty cell reads as a rendering fault, and an empty
      // accessible name ("Why was  forwarded?") is worse than none.
      const named = entry.q_name !== "";
      return (
        // A real button, not just a click handler on the row: selecting a
        // row is what opens the inspector, and that has to be reachable
        // without a mouse. The row is clickable too (DataGrid's onRowClick),
        // as a convenience on top of this rather than instead of it.
        <button
          type="button"
          aria-label={
            named
              ? `Why was ${entry.q_name} ${entry.decision}?`
              : `Why was an unnamed query ${entry.decision}?`
          }
          title={entry.q_name}
          onClick={() => tableMeta(table).onSelect(entry)}
          className={cn(
            "block w-full truncate text-left transition-colors hover:text-primary",
            "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring",
          )}
        >
          {named ? entry.q_name : UNKNOWN}
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
    header: "Client IP",
    size: 120,
    meta: { headerClassName: "px-4", cellClassName: "truncate px-4 text-muted-foreground" },
    cell: ({ row }) => row.original.client_ip,
  },
  {
    // A display column: the value isn't on the row at all, it's the row's
    // `client_id` looked up in GET /clients.
    id: "hostname",
    header: "Hostname",
    size: 128,
    meta: { headerClassName: "px-4", cellClassName: "truncate px-4 text-muted-foreground" },
    cell: ({ row, table }) => tableMeta(table).hostnames.get(row.original.client_id) ?? UNKNOWN,
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
    meta: { headerClassName: "px-4", cellClassName: "px-4 text-muted-foreground" },
    cell: ({ row }) => {
      const full = row.original.upstream;
      if (!full) return UNKNOWN;
      return (
        <span title={full} className="block truncate">
          {upstreamAddr(full)}
        </span>
      );
    },
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
        headerRow: "text-xs tracking-widest text-muted-foreground uppercase",
        headerSticky: "sticky top-0 z-10 bg-background",
        // `*:` reaches the row's own cells, which is where rowBorder puts
        // the hairline — the design separates rows in the muted tone and
        // keeps full-strength --border for the bands around the table.
        bodyRow: "text-xs *:border-border-muted hover:bg-card has-data-selected:bg-card",
      }}
    >
      {/* `flex min-h-0 flex-1 flex-col` on the container, not just on the
          panes above it: DataGridContainer is a plain block `div`
          (`w-full overflow-hidden`), so a height chain that stops at its
          parent leaves everything below it sized by content. */}
      <DataGridContainer border={false} className="flex min-h-0 flex-1 flex-col">
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
          //
          // The scroll area fills the split rather than carrying a fixed
          // height — but "fills" has to be spelled out at both ends,
          // because DataGridScrollArea puts our className on base-ui's
          // ScrollArea.Root and wraps *that* in its own bare
          // `<div class="relative">`. A `flex-1` handed to the component
          // therefore lands on the child of a plain block div and does
          // nothing: the Root sizes to its content, its `size-full`
          // viewport resolves 100% of an auto height to that same content
          // height, and clientHeight ends up equal to scrollHeight. That
          // one fact is three bugs at once — nothing scrolls (the box grew
          // instead), every row mounts (the virtualizer's window is the
          // whole list), and DataGridTableVirtual sees its last row
          // mounted and calls onFetchMore forever, which is where OFFSET
          // 2300 came from with nobody touching the page.
          //
          // So: a row-flex box we own takes the height (flex-1 against a
          // column parent that has one), which stretches rnui's wrapper
          // div to match, and `h-full` gives the Root a percentage of a
          // now-definite height.
          <div className="flex min-h-0 flex-1">
            <DataGridScrollArea className="h-full min-w-0 flex-1">
              <DataGridTableVirtual
                estimateSize={ROW_HEIGHT_ESTIMATE}
                overscan={12}
                onFetchMore={onFetchMore}
                isFetchingMore={isFetchingMore}
                hasMore={hasMore}
              />
            </DataGridScrollArea>
          </div>
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
      value: entry.upstream || UNKNOWN,
      note: entry.upstream ? undefined : "never left the box",
    },
    {
      name: "r_code",
      value: entry.r_code || UNKNOWN,
      note: entry.r_code ? undefined : "no answer",
    },
    {
      name: "duration_ms",
      value: String(entry.duration_ms),
      note: entry.duration_ms === 0 ? "under 1 ms, truncated" : undefined,
    },
  ];
}

/** What matched, in one line. The group is named, not assumed: a rule that
 * governs this row lives in the row's *client's* group (see groupForEntry),
 * and "rule #1" means nothing without saying which group's rule #1. */
function matchTitle(entry: QueryEntry, groupName?: string, list?: List): string {
  if (entry.rule_id > 0) {
    return groupName === undefined
      ? `Matched rule #${entry.rule_id}`
      : `Matched rule #${entry.rule_id} in group ${groupName}`;
  }
  // The list is already resolved for the prose below, so name it here too:
  // "#3" is an internal id the admin has no way to look up.
  if (entry.list_id > 0) return list ? `Matched ${list.name}` : `Matched list #${entry.list_id}`;
  return "No rule or list matched";
}

function MatchProse({
  entry,
  rule,
  ruleLoading,
  list,
}: {
  entry: QueryEntry;
  rule?: Rule;
  /** The row's group's rules are still in flight — see the page's `rules`. */
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
        From the {list.kind} list <span className="font-medium text-foreground">{list.name}</span> (
        <code className="font-mono break-all">{list.url}</code>).
      </p>
    ) : (
      <p>Matched list #{entry.list_id}, which isn&apos;t available right now.</p>
    );
  }
  return <p>No rule or list matched — resolved by the default policy.</p>;
}

/** The inspector's action pair. Which rule the primary button writes follows
 * the row: a blocked row needs an allow, anything else needs a block. */
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
  // A rule needs a pattern, and a row with no question has none: the POST
  // would carry `pattern: ""` and come back 400, after the button had
  // already promised to write it.
  const unnamed = entry.q_name === "";
  const held = disabled || unnamed;
  const heldReason = unnamed ? "This query carried no name to write a rule for" : disabledReason;

  return (
    <div className="flex flex-wrap items-center gap-3 border-t border-border px-4 py-3">
      {done ? (
        <Button type="button" size="sm" disabled>
          {status === "blocked" ? "Blocked" : "Allowed"}
        </Button>
      ) : (
        <Button
          type="button"
          size="sm"
          disabled={status === "pending" || held}
          // Still visible while unavailable, just visibly inert and carrying
          // the reason — silently having no affordance is how "why can't I
          // block from here?" starts.
          title={held ? heldReason : undefined}
          onClick={() => onQuickRule(action, entry)}
        >
          {action === "allow" ? "Allow domain" : "Block domain"}
        </Button>
      )}
      <Button type="button" size="sm" variant="outline" onClick={() => onCopy(entry)}>
        Copy row JSON
      </Button>
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
 *
 * Note `groupName` is a plain string, not the group object: a react-query
 * result (or anything derived per render from one) as a prop here would give
 * this a fresh identity ten times a second and quietly defeat the memo.
 */
const Inspector = memo(function Inspector({
  entry,
  groupName,
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
  groupName?: string;
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
      {/* h-8 to the pixel: the grid's own header row measures 32px, and a
          4px difference here is enough for the two panes' rules to visibly
          miss each other across the split. */}
      <div className="flex h-8 items-center justify-between gap-2 border-b border-border px-4">
        <SectionTitle id="why-title">Why this decision</SectionTitle>
        {
          <button
            type="button"
            aria-label={
              entry ? "Close the inspector and clear the selected row" : "Close the inspector"
            }
            onClick={onClose}
            className={cn(
              "shrink-0 text-xs tracking-widest text-muted-foreground uppercase",
              "transition-colors hover:text-foreground",
              "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
            )}
          >
            Close <span aria-hidden="true">×</span>
          </button>
        }
      </div>

      {entry !== null && (
        <>
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="flex flex-col gap-2 border-b border-border px-4 py-3">
              <p className="text-base break-all">{entry.q_name}</p>
              <div className="flex flex-wrap items-center gap-3">
                <Badge variant={decisionBadge(entry.decision)} size="sm" className="uppercase">
                  {entry.decision}
                </Badge>
                <Note>
                  {entry.q_type} · {entry.r_code || "no answer"} ·{" "}
                  {durationLabel(entry.duration_ms)} ms
                </Note>
              </div>
            </div>

            <Prose className="border-b border-border">
              <Alert
                variant={
                  entry.decision === "blocked" || entry.decision === "error"
                    ? "destructive"
                    : "default"
                }
              >
                <AlertTitle>{matchTitle(entry, groupName, list)}</AlertTitle>
                <AlertDescription>
                  <MatchProse entry={entry} rule={rule} ruleLoading={ruleLoading} list={list} />
                </AlertDescription>
              </Alert>
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
          enough to always look like `at`, wrong often enough to say so. */}
      {filtered && <Note>ordered by id, not by at</Note>}
      <div className="ml-auto flex items-center gap-3">
        <Button type="button" size="sm" variant="outline" onClick={onNewer}>
          Newer
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={!filtered || !hasMore || isFetchingMore}
          onClick={onOlder}
        >
          Older
        </Button>
      </div>
    </div>
  );
}

// --- page --------------------------------------------------------------------

export function QueryLog() {
  const [filters, setFilters] = useState<FilterState>(NO_FILTERS);
  // Light debounce so the domain box doesn't fire a LIKE query per keystroke.
  // The other four are discrete selections, so they apply immediately.
  const [debouncedQ, setDebouncedQ] = useState("");
  const [selected, setSelected] = useState<QueryEntry | null>(null);
  /**
   * Is the inspector rail showing?
   *
   * Separate from `selected` because the two answer different questions:
   * closing the rail should give the table the full width even though a row
   * is still highlighted, and picking a row should bring the rail back
   * without the user hunting for a re-open control. Defaults open so the
   * screen explains itself on arrival.
   *
   * Tracks only whether the user dismissed it. Visibility is
   * `selected !== null && !railDismissed`: the rail is the detail view of a
   * row, so with nothing picked there is nothing for it to say, and on
   * arrival — nothing selected — the table gets the full width. Closing
   * hides it; picking another row brings it back.
   */
  const [railDismissed, setRailDismissed] = useState(false);
  const [resetToken, setResetToken] = useState(0);
  // The flag is shared with the chrome's own cell (components/top-nav.tsx),
  // so both toggles observe and write one value. See lib/live-tail.ts.
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

  // The tail's mode and health are rendered by the chrome, not here. Row 2
  // of the top bar owns that cell in the design, and having the page render
  // its own copy beneath produced four readouts of one fact side by side
  // (LIVE · PAUSE, LIVE TAIL, RECONNECTING…, PAUSE TAIL). `paused` flows
  // down from the chrome; these three flow up, because only the routed page
  // knows whether a filter is active or how the subscription is faring.
  usePublishLiveTailStatus({
    filtered,
    streamState: live.state,
    reconnect: live.reconnect,
  });

  // Every quick rule and the inspector's rule lookup are scoped to the group
  // that actually governs the row's client (see groupForEntry) — a rule
  // written into group 1 for a client that lives in group 3 is a no-op the
  // UI would otherwise report as a success. The same request feeds the
  // CLIENT filter's options and the table's HOSTNAME column.
  const clients = useClients();
  const clientGroups = useMemo(() => clientGroupMap(clients.data), [clients.data]);
  const hostnames = useMemo(() => clientNameMap(clients.data), [clients.data]);
  const selectedGroupId = selected ? groupForEntry(selected, clientGroups) : null;

  // Only exact-IP matchers become options. `client` is an exact match on
  // `client_ip` (docs/ui-contract.md §2.7), so a client matched by CIDR —
  // which the API accepts — can never equal any single logged address:
  // offering it would be advertising a filter that always returns nothing,
  // the same mistake the `allowed` decision used to be. Those clients still
  // resolve a HOSTNAME, which goes through `client_id`, not the matcher.
  const clientOptions = useMemo<ClientOption[]>(
    () =>
      (clients.data ?? [])
        .filter((c) => !c.matcher.includes("/"))
        .map((c) => ({ ip: c.matcher, name: c.name })),
    [clients.data],
  );

  // Only an explicit non-`full` value closes the filter. An unread or failed
  // settings request leaves it open: a filter that returns nothing is a
  // smaller harm than a control taken away for a reason that wasn't checked.
  const privacy = useSettings().data?.["qlog.privacy"];
  const clientIpsMasked = privacy !== undefined && privacy !== "full";

  const groups = useGroups();
  const groupName = useMemo(
    () => groups.data?.find((g) => g.id === selectedGroupId)?.name,
    [groups.data, selectedGroupId],
  );

  // Only once a row is selected: with nothing selected there is no group to
  // ask about, and falling back to group 1 fetched a ruleset the page had no
  // use for on every mount.
  const rules = useRules(selectedGroupId ?? DEFAULT_GROUP_ID, {
    enabled: selectedGroupId !== null,
  });
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

  const onSelect = useCallback((entry: QueryEntry) => {
    setSelected(entry);
    setRailDismissed(false);
  }, []);
  const onClearSelection = useCallback(() => {
    setSelected(null);
    setRailDismissed(true);
  }, []);
  const patchFilters = useCallback(
    (patch: Partial<FilterState>) => setFilters((f) => ({ ...f, ...patch })),
    [],
  );

  /**
   * The DateSelector's value, as the API's two bounds.
   *
   * Stable identity (no deps) because it is a prop of the memoised filter
   * bar — and it bails out when the bounds haven't actually moved, because
   * the component emits its value once on mount and again on every internal
   * state change, and a new `filters` object per emission would restart the
   * debounce and rebuild the search filter for nothing.
   */
  const applyTimeRange = useCallback((value: DateSelectorValue) => {
    const range = toEpochRange(value);
    setFilters((f) =>
      // Assigning both bounds, never spreading a partial: `range` carries an
      // explicit `undefined` for whichever end this operator leaves open, and
      // that has to overwrite whatever the last selection put there.
      f.from === range.from && f.to === range.to ? f : { ...f, from: range.from, to: range.to },
    );
  }, []);

  const clearFilters = useCallback(() => {
    setFilters(NO_FILTERS);
    // The DateSelector keeps its own selection; remount it so the trigger
    // stops claiming a range the page is no longer filtering on.
    setResetToken((n) => n + 1);
  }, []);

  // The debounced `q` lags the box by 300ms, so "anything typed at all"
  // isn't the same question as "anything is being searched for": the Clear
  // control has to appear the moment there is something to clear.
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
    () => ({ selectedKey, hostnames, onSelect }),
    [selectedKey, hostnames, onSelect],
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

  /**
   * The next-page fetch, guarded and referentially stable.
   *
   * rnui's DataGridTableVirtual asks for more from an effect whose deps
   * include both the virtual-item array (a fresh array every render) and
   * this callback, so an inline arrow here re-armed it on every commit. Its
   * own `isFetchingMore`/`hasMore` guards are the react-query flags, which
   * are a render behind the call — several requests could be issued before
   * the first one flipped `isFetchingNextPage`. A ref closes that window
   * synchronously, and the same ref means the page can never out-run the
   * end of the results even if the grid asks again.
   *
   * `pagedRef` is only ever written during render and read from callbacks —
   * never read during render — so it stays out of the rendered output.
   */
  const pagedRef = useRef(paged);
  pagedRef.current = paged;
  const fetchMoreInFlight = useRef(false);
  const fetchMore = useCallback(() => {
    const p = pagedRef.current;
    if (fetchMoreInFlight.current || p.isFetchingNextPage || !p.hasNextPage) return;
    fetchMoreInFlight.current = true;
    void p.fetchNextPage().finally(() => {
      fetchMoreInFlight.current = false;
    });
  }, []);

  const tableRef = useRef<HTMLDivElement>(null);
  const scrollToNewest = useCallback(() => {
    const viewport = tableRef.current?.querySelector('[data-slot="scroll-area-viewport"]');
    if (viewport) viewport.scrollTop = 0;
  }, []);

  let emptyMessage: ReactNode;
  if (filtered && paged.isError) {
    emptyMessage = "Couldn't load queries.";
  } else if (filtered) {
    emptyMessage = "No matching queries";
  } else if (paused) {
    emptyMessage = "Paused. Resume the live tail to start appending rows.";
  } else {
    emptyMessage = "Waiting for traffic";
  }

  return (
    // A column that claims the whole content area (the shell drops its
    // padding for this route — see components/app-shell.tsx), so the split
    // below can take every pixel left over and its vertical rule can run to
    // the bottom of the window.
    <div className="flex min-h-0 flex-1 flex-col font-mono">
      {/* The chrome's own tab already says Query Log, and the design gives
          the page no visible title — but a page still needs one heading. */}
      <h1 className="sr-only">Query log</h1>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-2">
        <FilterBar
          value={filters}
          clients={clientOptions}
          clientIpsMasked={clientIpsMasked}
          resetToken={resetToken}
          onChange={patchFilters}
          onTimeRange={applyTimeRange}
        />
        {anyFilterSet && (
          <Button type="button" size="sm" variant="ghost" onClick={clearFilters}>
            Clear filters
          </Button>
        )}
      </div>

      <div className="flex min-h-0 flex-1 flex-col lg:flex-row lg:items-stretch">
        <div ref={tableRef} className="flex min-h-0 min-w-0 flex-1 flex-col">
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
            onFetchMore={filtered ? fetchMore : undefined}
            isFetchingMore={paged.isFetchingNextPage}
            hasMore={filtered ? paged.hasNextPage : undefined}
          />
        </div>

        {selected !== null && !railDismissed && (
          <Inspector
            entry={selected}
            groupName={groupName}
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
        )}
      </div>

      <Footer
        rows={entries.length}
        filtered={filtered}
        offset={offset}
        hasMore={paged.hasNextPage}
        isFetchingMore={paged.isFetchingNextPage}
        onOlder={fetchMore}
        onNewer={scrollToNewest}
      />
    </div>
  );
}
