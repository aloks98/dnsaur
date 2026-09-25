import { useMemo, useState, useSyncExternalStore } from "react";
import { LogOut } from "lucide-react";
import { Link, NavLink, useLocation, useSearchParams } from "react-router";
import { toast } from "sonner";
import {
  Avatar,
  AvatarFallback,
  cn,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Kbd,
  Popover,
  PopoverContent,
  PopoverTrigger,
  Sheet,
  SheetContent,
  SheetTrigger,
} from "@e412/rnui-react";
import type { SseState } from "../api/sse";
import { isAlreadyLoggedOut, useLogout, useMe, useMeInitials } from "../hooks/use-auth";
import { useResolverStatus } from "../hooks/use-settings";
import { FACT_TARGETS, factBand, statusFacts, type StatusFact } from "../lib/serving";
import { useHealth } from "../hooks/use-stats";
import {
  DASHBOARD_PATH,
  DHCP_BASE,
  DHCP_CLASSES_PATH,
  DHCP_LEASES_PATH,
  DHCP_RESERVATIONS_PATH,
  FILTERING_BASE,
  findActiveGroup,
  isNavItemActive,
  navGroups,
  QUERY_LOG_PATH,
  type NavGroup,
} from "../lib/nav";
import { useLists } from "../hooks/use-filters";
import {
  useClasses,
  useDHCPEnabled,
  useDHCPStatus,
  useLeasePollMs,
  useReservations,
  useScopes,
} from "../hooks/use-dhcp";
import { useManagedBy } from "../hooks/use-sync";
import { useLiveTailStatus } from "../lib/live-tail";
import { paletteShortcut } from "../lib/platform";
import { parseWindow, WINDOW_PARAM, WINDOWS } from "../lib/stats-window";
import { DnsaurLogo } from "./dnsaur-logo";
import { PauseControl } from "./pause-control";
import { ThemeToggle } from "./theme-toggle";

interface TopNavProps {
  onOpenCommandPalette: () => void;
}

/**
 * Shared by every cell in both rows: mono, uppercase, tracked, one type
 * step. Both strips sit at `text-xs` — the framework's smallest step, and
 * the one the whole chrome is written in. What separates row 1 from row 2
 * is weight and surface (row 2 is on `--card`), not size.
 */
const CELL =
  "flex shrink-0 items-center whitespace-nowrap px-4 font-mono text-xs font-normal " +
  "tracking-widest uppercase transition-colors " +
  // Every cell carries the active-marker border at all times, transparent
  // until it's the one you're on, so marking a cell never moves its text.
  // The colour is set per side (`border-b-*`, and `border-l-border` /
  // `border-r-border` on the dividers below) rather than with the all-sides
  // `border-border`: that would repaint this bottom edge too, drawing a
  // 2px rule under every cell in the bar.
  "border-b-2 border-b-transparent " +
  "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring";

/**
 * The DHCP section's four row-2 readouts. Not uppercased like the rest of
 * the bar, for the reason the role chip isn't (see RoleChip): these carry
 * counts and an interval rather than a label, and `every 10 s` shouted as
 * `EVERY 10 S` reads as a unit nobody uses.
 */
const DHCP_CELL =
  "ml-auto border-l border-l-border tracking-wider normal-case text-muted-foreground";

/** Every cell that can be "the one you're on" marks it the same way. */
const CELL_ACTIVE = "font-semibold text-foreground border-b-primary";
const CELL_QUIET = "text-muted-foreground hover:text-foreground";

/**
 * The app shell's chrome: two hairline-bordered rows in place of the old
 * sidebar. Row 1 is identity + the four nav *groups* + the global right-hand
 * readouts (search, blocking, resolver health); row 2 is the active group's
 * pages as tabs, plus the contextual right-hand cells — on the dashboard,
 * the stats window selector and that screen's CTA.
 *
 * The bar carries at most two filled cells and they are the two the design
 * fills: the blocking readout in row 1 (see pause-control.tsx — it is the
 * one control that changes what dnsaur *does*, so it wears the state as a
 * fill rather than as a tint) and row 2's per-screen call to action — the
 * dashboard's "view query log", the query log's tail toggle. Everything
 * else is text on the bar's own surface.
 *
 * Semantics are deliberately real, not divs-with-onClick: the groups are
 * menu buttons (base-ui Menu — arrow keys, Escape, typeahead, focus return),
 * their contents are links with `role="menuitem"`, and the row-2 tabs are
 * plain NavLinks inside a labelled <nav> so they carry `aria-current="page"`
 * for free.
 *
 * Narrow widths: both nav strips scroll horizontally (`overflow-x-auto`,
 * scrollbar suppressed — see styles/app.css) while the logo and the
 * right-hand cells stay pinned and never shrink. Four groups plus two status
 * cells cannot fit a phone at this type size, and a scrollable strip keeps
 * every destination reachable without collapsing the shell into a hamburger
 * the design does not have.
 */
export function TopNav({ onOpenCommandPalette }: TopNavProps) {
  const { pathname } = useLocation();
  const activeGroup = findActiveGroup(pathname);
  // Row 2's right-hand cells are contextual: a stats window governs nothing
  // on Settings, "view query log" is noise on the query log itself, and a
  // tail toggle is noise everywhere else. Each screen gets its own pair, or
  // none.
  const onDashboard = pathname === DASHBOARD_PATH;
  const onQueryLog = pathname === QUERY_LOG_PATH;
  const onFilterLists = pathname === `${FILTERING_BASE}/lists`;
  const groups = navGroups(useDHCPEnabled());

  return (
    <header
      data-slot="top-nav"
      className="sticky top-0 z-20 shrink-0 bg-background text-foreground"
    >
      {/* ---- Row 1: identity, groups, global readouts ------------------- */}
      <div className="flex h-11 items-stretch border-b border-border">
        <div className={cn(CELL, "gap-2 border-r border-r-border")}>
          <DnsaurLogo size={22} />
          {/* Below `sm` the tile carries the identity on its own and the
              wordmark's ~55px go to the nav strip instead — `sr-only`, not
              `hidden`, so it stays in the accessible name and the DOM. */}
          <span className="font-heading text-sm font-bold tracking-tight normal-case max-sm:sr-only">
            dnsaur
          </span>
        </div>

        <nav aria-label="Primary" className="dnsaur-scroll-x flex min-w-0 flex-1 items-stretch">
          {groups.map((group) => (
            <NavGroupMenu key={group.id} group={group} active={group.id === activeGroup?.id} />
          ))}
        </nav>

        <div className="ml-auto flex shrink-0 items-stretch">
          <button
            type="button"
            onClick={onOpenCommandPalette}
            className={cn(CELL, "border-l border-l-border", CELL_QUIET)}
          >
            {/* Same trick as the wordmark, below desktop: at tablet
                width those ~55px are the difference between all four groups
                fitting and System being the one you have to scroll for. The
                shortcut alone is affordance enough, and the word stays in
                the accessible name rather than being dropped from it. */}
            <span className="max-lg:sr-only">search</span>
            {/* A real space rather than flex `gap`: name computation
                concatenates inline children without inserting one, so a gap
                would leave this cell announcing as "searchCtrl+K". */}{" "}
            {/* rnui's Kbd rather than a bare string: a keyboard shortcut is a
                <kbd>, and the component is what makes it look like a key
                instead of two more characters of label. The glyph follows the
                platform — the listener takes either modifier, but this names
                one, and naming ⌘ on a keyboard that has no ⌘ is just wrong. */}
            <Kbd>{paletteShortcut()}</Kbd>
          </button>
          {/* Global blocking (group 0). Still the same control — state
              readout and pause/resume menu in one — now the filled cell the
              design gives row 1. See components/pause-control.tsx. */}
          <PauseControl variant="chrome" />
          {/* Resolver liveness sits next to blocking rather than in row 2:
              they are the shell's two "is this thing working" readouts and
              read as a pair. It stays flat text while blocking is filled —
              one of them governs what dnsaur does and the other reports a
              status that is green nearly all the time, and filling both
              would make the bar shout twice. The same cell carries whatever
              else is currently wrong — see StatusCell. */}
          <StatusCell />
          <RoleChip />
        </div>
      </div>

      {/* ---- Row 2: the active group's pages ----------------------------
          Omitted entirely when the path is in no group (the not-found
          screen). A row of tabs there would offer a section the current page
          does not belong to, and an empty strip would just be a stray
          hairline under the bar. */}
      {activeGroup && (
        <div className="flex h-10 items-stretch border-b border-border bg-card">
          <nav
            aria-label={activeGroup.label}
            className="dnsaur-scroll-x flex min-w-0 items-stretch"
          >
            {activeGroup.items.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={cn(CELL, CELL_QUIET, isNavItemActive(pathname, item) && CELL_ACTIVE)}
              >
                {item.label}
              </NavLink>
            ))}
          </nav>

          {onDashboard && (
            <div className="ml-auto flex shrink-0 items-stretch">
              <WindowCells />
              {/* Row 2's filled cell, and the dashboard's one call to action:
                the numbers above are a summary, the log is where you
                actually go to look. */}
              <NavLink
                to="/queries"
                className={cn(
                  CELL,
                  "border-l border-l-border bg-primary font-semibold text-primary-foreground",
                  "hover:bg-primary/90",
                )}
              >
                View query log <span aria-hidden="true">→</span>
              </NavLink>
            </div>
          )}

          {onQueryLog && <QueryLogCells />}

          {onFilterLists && <FilterListsCell />}

          {/* DHCP's four screens each get their own readout, on the same
              terms as Filtering › Lists above: the page below already has
              the query, so this is a subscription rather than a second
              fetch, and no other screen pays for it. */}
          {pathname === DHCP_BASE && <ScopesCell />}
          {pathname === DHCP_LEASES_PATH && <LeasesPollCell />}
          {pathname === DHCP_RESERVATIONS_PATH && <ReservationsCell />}
          {pathname === DHCP_CLASSES_PATH && <ClassesCell />}
        </div>
      )}
    </header>
  );
}

/**
 * Row 1's third readout, and the only one that is not about health: this
 * box takes its configuration from another one.
 *
 * A chip beside the two health cells rather than a page-wide banner, because
 * being a replica is a *state* and not a fault — the status panel beside it
 * is kept for the sync facts that are actually wrong (lib/serving.ts's
 * statusFacts). Its square is hollow — an outline, no fill — which is the
 * bar's quietest possible marker: present, and lit up about nothing.
 *
 * The host, not the whole URL: this is an identity, and at this type size a
 * scheme and a port would push the nav strip's four groups off screen.
 * Reads the peer the shell already holds (useManagedBy), so it costs no
 * request, and links to the one screen that can change it.
 */
function RoleChip() {
  const peer = useManagedBy();
  if (peer === "") return null;

  return (
    <Link
      to="/settings#sync"
      title="Open Settings › Sync"
      // Not uppercased like the rest of the bar: this cell carries a
      // hostname, and a hostname shouted is a hostname misread.
      className={cn(CELL, "gap-2 border-l border-l-border tracking-wider normal-case", CELL_QUIET)}
    >
      <span aria-hidden className="size-[7px] shrink-0 border border-muted-foreground" />
      Replica · <span className="text-foreground">{peerHost(peer)}</span>
    </Link>
  );
}

/** The peer URL's hostname, or the URL verbatim when it will not parse —
 * the value came from a settings box, and a chip is no place to discover
 * that it is malformed. */
function peerHost(peer: string): string {
  try {
    return new URL(peer).hostname;
  } catch {
    return peer;
  }
}

/**
 * The query log's row-2 cells: one readout of what the table is doing, and
 * the filled cell that changes it.
 *
 * These live in the chrome, not on the page, because they belong to the
 * sub-nav strip — and because the alternative was worse. For one revision
 * the page owned an equivalent pair while the shell still rendered its own,
 * and `/queries` carried *four* readouts of one piece of state across two
 * rows. The fix is not to move them down; it's to have one of each, up
 * here, where every other screen's contextual cells already are.
 *
 * What the shell can't know on its own — whether a filter is active, and
 * how the subscription is faring — the page publishes through
 * lib/live-tail.ts, which already existed to carry `paused` between these
 * two siblings. One store, one live value, four states in one cell.
 */
function QueryLogCells() {
  const { paused, setPaused, filtered, streamState, reconnect } = useLiveTailStatus();
  const failed = !filtered && !paused && streamState === "failed";

  return (
    <div className="ml-auto flex shrink-0 items-stretch">
      <TailModeCell filtered={filtered} paused={paused} streamState={streamState} />
      {/* Only offered when there is something to retry. A failed stream is
          not a quieter shade of "reconnecting": api/sse.ts has given up
          after six attempts and nothing further happens without a click, so
          the click sits next to the word that says so. */}
      {failed && (
        <button
          type="button"
          onClick={reconnect}
          className={cn(CELL, "border-l border-l-border", CELL_QUIET)}
        >
          Reconnect
        </button>
      )}
      <TailToggleCell paused={paused} onToggle={setPaused} />
    </div>
  );
}

/**
 * Filtering › Lists' row-2 readout: how many lists exist, and how many
 * domains they are actually enforcing between them.
 *
 * "Enforcing" excludes disabled lists and any whose status says they are
 * contributing nothing. A total that counted those would read the same
 * whether filtering worked or not — which is precisely the failure this
 * screen exists to surface, so the number in the chrome must not paper
 * over it.
 *
 * Reads the same query the page does; react-query dedupes, so this is a
 * subscription rather than a second fetch. Rendered only on that route, so
 * no other screen pays for it.
 */
function FilterListsCell() {
  const lists = useLists();
  if (!lists.data) return null;

  const total = lists.data.length;
  const entries = lists.data
    .filter((l) => l.enabled && (l.last_status === "ok" || l.last_status === "stale"))
    .reduce((sum, l) => sum + l.entry_count, 0);

  return (
    <output
      aria-label="Filter lists"
      className={cn(CELL, "ml-auto border-l border-l-border text-muted-foreground")}
    >
      {total} {total === 1 ? "list" : "lists"} · {entries.toLocaleString()} enforcing
    </output>
  );
}

/**
 * DHCP › Scopes' row-2 readout: how many subnets this box hands out on, and
 * how much of their pools is actually in use.
 *
 * The pool numbers come from `GET /dhcp/status` rather than from the scope
 * rows, because only the engine knows them: a scope stores a pool's two
 * ends, and how many addresses inside it are leased is a fact about the
 * lease database. `leased` can exceed the pool after a pool is shrunk —
 * the engine keeps what it has already handed out — and the readout says so
 * rather than clamping, since a number over the total is exactly the
 * condition worth noticing.
 */
function ScopesCell() {
  const scopes = useScopes();
  const status = useDHCPStatus();
  if (!scopes.data || !status.data) return null;

  const total = scopes.data.length;
  const leased = status.data.scopes.reduce((sum, s) => sum + s.leased, 0);
  const pool = status.data.scopes.reduce((sum, s) => sum + s.pool_size, 0);

  return (
    <output aria-label="Scopes" className={cn(CELL, DHCP_CELL)}>
      {total} {total === 1 ? "scope" : "scopes"} · {leased} leased of {pool}
    </output>
  );
}

/**
 * DHCP › Leases' row-2 readout: this table is live, and on whose schedule.
 *
 * The interval is named because it is the one thing an operator cannot see
 * from the rows — a release that has not disappeared yet is a lease waiting
 * out this number, not a release that failed (see hooks/use-dhcp.ts's
 * useReleaseLease). It is the `dhcp.lease_poll_seconds` setting, so a box
 * polling every two seconds says two.
 */
function LeasesPollCell() {
  const seconds = Math.round(useLeasePollMs() / 1000);

  return (
    <output aria-label="Lease table" className={cn(CELL, DHCP_CELL, "gap-2")}>
      <span aria-hidden="true" className="size-1.5 shrink-0 bg-primary" />
      LIVE · every {seconds} s
    </output>
  );
}

/** DHCP › Reservations' row-2 readout: how many fixed addresses exist, and
 * across how many scopes — the second number being what makes the page's
 * scope filter worth reaching for. */
function ReservationsCell() {
  const reservations = useReservations();
  if (!reservations.data) return null;

  const total = reservations.data.length;
  const scopes = new Set(reservations.data.map((r) => r.scope_id)).size;

  return (
    <output aria-label="Reservations" className={cn(CELL, DHCP_CELL)}>
      {total} {total === 1 ? "reservation" : "reservations"} · {scopes}{" "}
      {scopes === 1 ? "scope" : "scopes"}
    </output>
  );
}

/** DHCP › Classes' row-2 readout: how many classes exist. */
function ClassesCell() {
  const classes = useClasses();
  if (!classes.data) return null;

  const total = classes.data.length;
  return (
    <output aria-label="Classes" className={cn(CELL, DHCP_CELL)}>
      {total} {total === 1 ? "class" : "classes"}
    </output>
  );
}

/**
 * All four states of the tail in one cell.
 *
 * The order is a precedence, not a preference: a filtered view has swapped
 * the stream for paged search, so its health is not a thing to report; a
 * paused tail likewise isn't connecting to anything. Only once neither is
 * true does the subscription's own state get to speak.
 *
 * `<output>` rather than a div with role="status": same implicit role, same
 * polite live region, and this genuinely is one — "Disconnected" arriving
 * while you're reading the table is worth being told about. The Reconnect
 * button stays *outside* it for the same reason: a control inside a live
 * region gets re-announced every time the region updates.
 */
function TailModeCell({
  filtered,
  paused,
  streamState,
}: {
  filtered: boolean;
  paused: boolean;
  streamState: SseState;
}) {
  const { tone, label } = tailMode(filtered, paused, streamState);

  return (
    // `aria-label` rather than an sr-only prefix inside the cell: `status`
    // is not a name-from-content role, so the subject has to be the name or
    // the readout announces its state with nothing to attach it to — and
    // the resolver cell beside it in row 1 is also a `status`.
    <output
      aria-label="Query log"
      className={cn(CELL, "gap-2 border-l border-l-border text-muted-foreground")}
    >
      <span aria-hidden="true" className={cn("size-1.5 shrink-0", tone)} />
      {label}
    </output>
  );
}

function tailMode(
  filtered: boolean,
  paused: boolean,
  streamState: SseState,
): { tone: string; label: string } {
  if (filtered) return { tone: "bg-muted-foreground", label: "Filtered · paged" };
  if (paused) return { tone: "bg-muted-foreground", label: "Paused" };
  if (streamState === "failed") return { tone: "bg-destructive", label: "Disconnected" };
  if (streamState === "open") return { tone: "bg-primary", label: "Live tail" };
  return { tone: "bg-warn", label: "Reconnecting…" };
}

/**
 * Row 2's filled cell on this screen, and the one control the query log is
 * actually about.
 *
 * Labelled with what it will do rather than with the state it is in, so it
 * needs no `aria-pressed` and no second half of the label to be understood
 * — the cell beside it already says which state you're in. The flag lives
 * in lib/live-tail.ts; see there for why it isn't state in the shell or in
 * the URL.
 */
function TailToggleCell({
  paused,
  onToggle,
}: {
  paused: boolean;
  onToggle: (next: boolean) => void;
}) {
  return (
    <button
      type="button"
      onClick={() => onToggle(!paused)}
      className={cn(
        CELL,
        "border-l border-l-border font-semibold",
        paused
          ? // Not `text-primary-foreground`: white on solid --warning is
            // 4.10:1 in light and 2.18:1 in dark, both under AA.
            // --warning-solid-foreground is the near-black that clears it
            // in both (see styles/dnsaur-theme.css).
            "bg-warn text-warning-solid-foreground hover:bg-warn/90"
          : "bg-primary text-primary-foreground hover:bg-primary/90",
      )}
    >
      {paused ? "Resume tail" : "Pause tail"}
    </button>
  );
}

/**
 * The 1h / 24h / 7d selector, as three chrome cells rather than a Select:
 * three options with two-character labels is a segmented control, and a
 * dropdown inside a 38px strip of divided cells reads as a foreign control.
 *
 * The value lives in the URL (see lib/stats-window.ts) — this row is in the
 * shell and the numbers it governs are in the routed page below it.
 * `replace` keeps a window switch out of the back stack: it is a view
 * setting, not a navigation.
 */
function WindowCells() {
  const [params, setParams] = useSearchParams();
  const active = parseWindow(params.get(WINDOW_PARAM));

  return (
    // A real <fieldset> rather than role="group": same grouping semantics,
    // one fewer ARIA attribute. `min-w-0` undoes the UA's
    // `min-inline-size: min-content`, which preflight doesn't touch and
    // which would stop this shrinking with the rest of the row.
    <fieldset aria-label="Time window" className="flex min-w-0 items-stretch">
      {WINDOWS.map((w) => (
        <button
          key={w.value}
          type="button"
          // The cell shows "24h"; the accessible name says "Last 24 hours",
          // because "24h" read aloud on its own says nothing about what it
          // does to the page.
          aria-label={w.label}
          aria-pressed={w.value === active}
          onClick={() => {
            const next = new URLSearchParams(params);
            next.set(WINDOW_PARAM, w.value);
            setParams(next, { replace: true });
          }}
          className={cn(
            CELL,
            "border-l border-l-border",
            w.value === active ? CELL_ACTIVE : CELL_QUIET,
          )}
        >
          {w.short}
        </button>
      ))}
    </fieldset>
  );
}

/**
 * One group in row 1. The trigger shows the group; the menu lists its pages
 * (as real links), and — for System — the account identity, theme and log
 * out that used to live in the sidebar footer. The design's top bar has no
 * avatar and no theme control of its own, and the footer that held them is
 * gone, so they belong with Settings and Account rather than nowhere.
 */
function NavGroupMenu({ group, active }: { group: NavGroup; active: boolean }) {
  const { pathname } = useLocation();
  const isSystem = group.id === "system";

  return (
    <DropdownMenu>
      {/* No disclosure chevron. Four of them in a row, at 12px and 60%
          opacity, added a column of visual noise to say something the row
          below already answers — whichever group is marked, its pages are
          on screen. The one chevron the bar keeps is on the blocking cell,
          where the menu is the only way to reach the actions. */}
      <DropdownMenuTrigger
        data-active={active}
        className={cn(CELL, "border-r border-r-border", CELL_QUIET, active && CELL_ACTIVE)}
      >
        {group.label}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" sideOffset={0}>
        {isSystem && <AccountIdentity />}
        <DropdownMenuGroup>
          {group.items.map((item) => {
            const Icon = item.icon;
            return (
              <DropdownMenuItem
                key={item.to}
                // The active page is still listed (jumping to where you
                // already are is harmless) but marked, so the menu agrees
                // with the row-2 tabs about where you are.
                render={<NavLink to={item.to} end={item.end} />}
              >
                <Icon />
                {item.label}
                {isNavItemActive(pathname, item) && <span className="sr-only">, current page</span>}
              </DropdownMenuItem>
            );
          })}
        </DropdownMenuGroup>
        {isSystem && (
          <>
            <DropdownMenuSeparator />
            <ThemeToggle />
            <DropdownMenuSeparator />
            <LogOutItem />
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * Who you're signed in as. The design's chrome carries no avatar, so the
 * initials (useMeInitials) surface here instead — the one place in the app
 * that still answers "which account is this?".
 */
function AccountIdentity() {
  const me = useMe();
  const initials = useMeInitials();
  const username = me.data?.username;

  return (
    <>
      <DropdownMenuGroup>
        <DropdownMenuLabel className="flex items-center gap-2">
          <Avatar size="sm">
            <AvatarFallback>{initials}</AvatarFallback>
          </Avatar>
          <span className="truncate">{username ?? "Account"}</span>
        </DropdownMenuLabel>
      </DropdownMenuGroup>
      <DropdownMenuSeparator />
    </>
  );
}

function LogOutItem() {
  const logout = useLogout();

  return (
    <DropdownMenuItem
      variant="destructive"
      onClick={() =>
        logout.mutate(undefined, {
          // A 401 is the session already being gone, which useLogout treats
          // as a completed logout (see hooks/use-auth.ts) — it navigates, so
          // there is nothing to apologise for.
          onError: (error) => {
            if (!isAlreadyLoggedOut(error)) toast.error("Couldn't sign out — try again");
          },
        })
      }
    >
      <LogOut />
      Log out
    </DropdownMenuItem>
  );
}

/**
 * Row 1's second readout, and the shell's one place for everything that is
 * currently wrong with this server.
 *
 * It used to be two things in two places: this cell said whether the
 * resolver was answering, and the shell stacked a page-wide warning bar
 * under the top bar for every other fact that was true. There are ten such
 * facts now, most of them persistent for hours, so three bars regularly ate
 * the top of every screen and the operator stopped reading them. They are
 * one count here, and a panel of rows under it.
 *
 * With nothing open the cell is exactly the liveness readout it has always
 * been (ResolverReadout). With facts it is a button — `3 ISSUES ▾`, tinted
 * amber, or red when any fact is red — opening a panel whose every row
 * links to the band that fixes that fact.
 *
 * **A resolver that is not answering outranks all of it.** `DNS down` keeps
 * the cell whatever the count: it is the one thing on this bar that nothing
 * else can be more urgent than, and a cell reading `4 ISSUES` while DNS is
 * off the air buries the only one that stops the network working. The panel
 * still opens from it — the facts have not gone anywhere.
 *
 * No dismiss and no toast: rows leave when the fact clears, and at zero the
 * cell goes back to `DNS OK`. The panel never opens itself. How a screen
 * reader learns about an arrival is app-shell.tsx's StatusAnnouncer.
 */
function StatusCell() {
  const health = useHealth();
  const { facts, markSeen } = useStatusFacts();
  const [open, setOpen] = useState(false);
  const phone = useIsPhone();

  function onOpenChange(next: boolean) {
    setOpen(next);
    // On the way *out*, not on the way in: the tag's whole job is to say
    // which rows the reader has not seen yet, and clearing it as the panel
    // opens would clear it before they had looked. Closing is the moment
    // every row that was on screen stops being new — including one that
    // arrived while it was open, and one the reader left by clicking it.
    if (!next) markSeen();
  }

  if (facts.length === 0) {
    // The panel unmounts with its last row, and `open` would outlive it:
    // the next fact to arrive would mount the popover *already open*, which
    // is the one thing the boards say never happens. Closing here marks the
    // rows seen as any close does. Focus is not returned to the cell — the
    // trigger unmounts with the popover, so there is nothing to return it
    // to; the readout that replaces it is not focusable.
    if (open) onOpenChange(false);
    return <ResolverReadout />;
  }

  const issues = `${facts.length} ${facts.length === 1 ? "issue" : "issues"}`;
  const down = health.isError;
  const tone = down || facts.some((fact) => fact.tone === "red") ? TONE.red : TONE.amber;

  // Same button either way — only what it opens changes with the width.
  // `children` as a prop because both triggers render the element for us.
  const cell = {
    // Uppercased by CSS like every other cell, so the accessible name stays
    // "3 issues" rather than being spelled out letter by letter.
    "aria-label": down ? `DNS down, ${issues}` : issues,
    className: cn(CELL, "gap-2 border-l border-l-border font-medium tracking-wider", tone.cell),
    children: (
      <>
        <span aria-hidden className={cn("size-[7px] shrink-0", tone.dot)} />
        {down ? "DNS down" : issues}
        <span aria-hidden className="opacity-70">
          ▾
        </span>
      </>
    ),
  };

  // Below `sm` the bar is mostly this cell, so a 460px card anchored to it
  // would be the width of the screen with nowhere to hang — the boards put
  // the panel across the bottom instead. A real media query rather than a
  // CSS-only swap: the two are different components, not one in two skins.
  if (phone) {
    return (
      <Sheet open={open} onOpenChange={onOpenChange}>
        <SheetTrigger {...cell} />
        <SheetContent
          side="bottom"
          // The panel's own header carries the ESC hint at the right, which
          // is exactly where rnui floats its close button.
          showCloseButton={false}
          aria-labelledby={PANEL_TITLE_ID}
          className="max-h-[80svh] gap-0 overflow-y-auto rounded-none p-0"
        >
          <StatusPanel facts={facts} onNavigate={() => onOpenChange(false)} />
        </SheetContent>
      </Sheet>
    );
  }

  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      {/* base-ui's trigger carries aria-haspopup="dialog", aria-expanded and
          aria-controls itself, and returns focus here when the panel closes
          — including on Esc. */}
      <PopoverTrigger {...cell} />
      <PopoverContent
        align="end"
        sideOffset={0}
        aria-labelledby={PANEL_TITLE_ID}
        className={cn(
          "w-[460px] max-w-[calc(100vw-0.5rem)] gap-0 rounded-none border border-border p-0",
          "max-h-[70vh] overflow-y-auto shadow-[0_14px_40px_rgba(0,0,0,0.18)] ring-0",
        )}
      >
        <StatusPanel facts={facts} onNavigate={() => onOpenChange(false)} />
      </PopoverContent>
    </Popover>
  );
}

const PANEL_TITLE_ID = "status-panel-title";

/**
 * The panel: a header that counts what is open, then one row per fact.
 *
 * Every row is a link, because every fact has a place it is fixed and the
 * point of collapsing the bars was that the fix was a screen away with no
 * way to get there. The destination is named on the row rather than left to
 * the operator to infer from the wording.
 */
function StatusPanel({ facts, onNavigate }: { facts: PanelFact[]; onNavigate: () => void }) {
  const red = facts.filter((fact) => fact.tone === "red").length;

  return (
    <>
      <div className="flex items-center gap-2.5 border-b border-border px-3.5 py-2.5">
        <span
          id={PANEL_TITLE_ID}
          className="font-mono text-[9.5px] leading-none font-semibold tracking-[0.14em] text-muted-foreground uppercase"
        >
          Status
        </span>
        <span className="font-mono text-[11px] leading-none text-muted-foreground">
          {facts.length} open{red > 0 && ` · ${red} red`}
        </span>
        <span className="ml-auto font-mono text-[9.5px] leading-none tracking-[0.1em] text-muted-foreground">
          ESC
        </span>
      </div>
      {facts.map((fact) => {
        const tone = TONE[fact.tone];
        return (
          <Link
            key={fact.id}
            to={fact.target}
            onClick={onNavigate}
            className={cn(
              "grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-4 gap-y-1",
              "border-b border-border-muted py-2.5 pr-3.5 pl-[17px] no-underline",
              tone.row,
            )}
          >
            <span className="flex min-w-0 items-center gap-2">
              <span className="font-mono text-[12.5px] leading-[1.35] break-words">
                {fact.text}
              </span>
              {fact.isNew && (
                <span
                  className={cn(
                    "shrink-0 border px-1 py-0.5 font-mono text-[9px] leading-none font-semibold",
                    "tracking-[0.12em]",
                    tone.tag,
                  )}
                >
                  NEW
                </span>
              )}
            </span>
            <span className="pt-0.5 font-mono text-[9.5px] leading-[1.35] tracking-[0.1em] whitespace-nowrap text-muted-foreground uppercase">
              {FACT_TARGETS[fact.target]} <span aria-hidden>→</span>
            </span>
            {/* The listener's error, under the fact it belongs to rather
                than appended to it: the fact is one sentence an operator
                recognises, and the bind error is three lines of Go. */}
            {fact.sub !== undefined && fact.sub !== "" && (
              <span className="col-span-full font-mono text-[11.5px] leading-[1.35] break-words text-muted-foreground">
                {fact.sub}
              </span>
            )}
          </Link>
        );
      })}
    </>
  );
}

/**
 * The two tones, exactly as the boards give them: the tint is `--warning` /
 * `--destructive` at the board's own light and dark opacities, the text is
 * the matching `-foreground`, and the solid token draws the rule — 3px down
 * the left of a panel row, and the bar's own active-cell bottom border on
 * the cell, which is how every other marked cell up here does it.
 */
const TONE = {
  amber: {
    cell: "border-b-warning bg-warning/7 text-warning-foreground dark:bg-warning/9",
    row: "bg-warning/7 text-warning-foreground shadow-[inset_3px_0_0_var(--warning)] dark:bg-warning/9",
    dot: "bg-warning",
    tag: "border-warning",
  },
  red: {
    cell: "border-b-destructive bg-destructive/5 text-destructive-foreground dark:bg-destructive/7",
    row: "bg-destructive/5 text-destructive-foreground shadow-[inset_3px_0_0_var(--destructive)] dark:bg-destructive/7",
    dot: "bg-destructive",
    tag: "border-destructive",
  },
} as const;

type PanelFact = StatusFact & { isNew: boolean };

/**
 * The facts in the order the panel lists them, each knowing whether it
 * arrived since the panel was last opened.
 *
 * Ordering is red first, then amber that waits on the operator, then amber
 * that clears on its own; within a group, oldest first. The grouping is a
 * claim about what the reader can do next, and first-seen inside it is what
 * stops a row the reader has already placed from moving under their eyes
 * when the poll comes back with the same facts in a different arithmetic.
 */
function useStatusFacts(): { facts: PanelFact[]; markSeen: () => void } {
  const status = useResolverStatus();
  // Memoised on the query's own data so the facts move only when the
  // server's answer does. One of them reads the clock (how long a replica
  // has been quiet), and the bar re-renders for reasons that have nothing
  // to do with this — a route change, a stats window — none of which should
  // rewrite a row.
  const facts = useMemo(() => statusFacts(status.data), [status.data]);

  // First-seen order, so a row never moves once it is on screen: the sort
  // below only ever groups, and inside a group this number is what keeps the
  // third row third while the poll comes back with the same facts in a
  // different arithmetic.
  //
  // Set during render rather than from an effect — React's own shape for
  // state derived from what the last render saw. The new numbering is used
  // *this* render, so a row is never painted in one place and moved to
  // another a frame later, and React re-runs the component before the
  // browser sees either version.
  const [order, setOrder] = useState(NO_ORDER);
  const numbered = reorder(order, facts);
  if (numbered !== order) setOrder(numbered);

  // What the panel has already shown. Empty until it is first closed, so
  // everything waiting on a fresh load carries NEW — which is what "arrived
  // since the panel was last opened" means when it never has been.
  //
  // Pruned in the same breath as the numbering above, and for the same
  // reason: a fact that cleared and came back is a new arrival. Without
  // this it kept its place in the "already shown" set while `reorder` gave
  // it a fresh position — a row that had moved to the bottom of its band
  // with nothing on it saying why.
  const [seen, setSeen] = useState(NOTHING_SEEN);
  const remembered = prune(seen, facts);
  if (remembered !== seen) setSeen(remembered);

  return {
    facts: facts
      .slice()
      .sort((a, b) => factBand(a) - factBand(b) || num(numbered, a) - num(numbered, b))
      .map((fact) => ({ ...fact, isNew: !remembered.has(fact.id) })),
    markSeen: () => setSeen(new Set(facts.map((fact) => fact.id))),
  };
}

const NO_ORDER: ReadonlyMap<string, number> = new Map();
const NOTHING_SEEN: ReadonlySet<string> = new Set();

/** `reorder` numbers every fact that is currently true, so this cannot
 * miss; the fallback says what an unnumbered fact would be — the newest
 * one there is, which is where it would sort anyway. */
function num(order: ReadonlyMap<string, number>, fact: StatusFact): number {
  return order.get(fact.id) ?? Number.MAX_SAFE_INTEGER;
}

/**
 * Numbers the facts that are new to `order` and forgets the ones that have
 * cleared — a fact that comes back is a new arrival, not the old row
 * returning to the place it used to hold.
 *
 * Returns `order` itself when neither happened, so a poll that reports the
 * same facts costs no render.
 */
function reorder(
  order: ReadonlyMap<string, number>,
  facts: StatusFact[],
): ReadonlyMap<string, number> {
  const live = new Set(facts.map((fact) => fact.id));
  const kept = [...order].filter(([id]) => live.has(id));
  const arrived = facts.filter((fact) => !order.has(fact.id));
  if (kept.length === order.size && arrived.length === 0) return order;
  let next = kept.length === 0 ? 0 : Math.max(...kept.map(([, n]) => n)) + 1;
  return new Map([...kept, ...arrived.map((fact) => [fact.id, next++] as const)]);
}

/** The boards' phone breakpoint, which is also Tailwind's `sm`. */
const PHONE = "(max-width: 640px)";

function subscribePhone(onChange: () => void): () => void {
  const query = window.matchMedia(PHONE);
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}

function isPhone(): boolean {
  return window.matchMedia(PHONE).matches;
}

/** Which panel the cell opens. The rest of the bar meets narrow widths with
 * CSS alone (see TopNav's own comment), but a popover and a bottom sheet are
 * two components rather than one restyled, so this one needs the answer in
 * JavaScript. */
function useIsPhone(): boolean {
  return useSyncExternalStore(subscribePhone, isPhone);
}

/**
 * Is the *resolver* answering (GET /health)?
 *
 * This is the app's only liveness signal and it used to be the sidebar
 * footer's single status dot. It sits beside the blocking readout, in the
 * same flat text-only vocabulary that control uses (see pause-control.tsx's
 * `chromeTone`) — the two together answer "is dnsaur working right now",
 * and giving one of them a solid fill would make it shout over the other.
 *
 * "DNS down" goes destructive rather than merely grey: the whole point of
 * the readout is that it must be impossible to skim past when it's bad.
 *
 * The wording names the subject, and it used to not: a bare "RESOLVING"
 * reads as an action in progress ("…resolving what? is it stuck?") rather
 * than as a state. "DNS OK" / "DNS down" parallel the blocking readout
 * beside it, so the pair scans as two answers to the same question.
 *
 * This is the cell with nothing else to report; StatusCell above takes the
 * same slot over as a button when there is.
 */
function ResolverReadout() {
  const health = useHealth();
  const label = health.isPending ? "Checking DNS…" : health.isError ? "DNS down" : "DNS OK";

  return (
    // <output> rather than a div with role="status": same implicit role,
    // same polite live region, one fewer ARIA attribute to keep honest.
    <output
      // The build that is answering, from the same /health poll — no second
      // request, and no second fact in a label the eye skims for one word.
      // A native `title` rather than a Tooltip: this cell is a live region,
      // and a hover card inside one gets re-announced on every update.
      title={health.data ? `dnsaur ${health.data.version}` : undefined}
      className={cn(
        CELL,
        "gap-2 border-l border-l-border text-xs font-medium tracking-wider",
        health.isError
          ? "text-destructive"
          : health.isPending
            ? "text-muted-foreground"
            : "text-primary",
      )}
    >
      {/* The boards' 7px square. `bg-current` rather than a tone of its own:
          the cell already picks a colour per state, and a dot that had to
          be told the same thing twice is a second place to get it wrong. */}
      <span aria-hidden className="size-[7px] shrink-0 bg-current" />
      <span className="sr-only">Resolver: </span>
      {label}
    </output>
  );
}

/** Forgets what the panel has shown about facts that have since cleared, so
 * one that comes back is new again — the rule `reorder` applies to a row's
 * position, applied to its tag. Returns `seen` itself when nothing went. */
function prune(seen: ReadonlySet<string>, facts: StatusFact[]): ReadonlySet<string> {
  const live = facts.filter((fact) => seen.has(fact.id)).map((fact) => fact.id);
  if (live.length === seen.size) return seen;
  return new Set(live);
}
