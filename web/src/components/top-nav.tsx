import { LogOut } from "lucide-react";
import { NavLink, useLocation, useSearchParams } from "react-router";
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
} from "@e412/rnui-react";
import type { SseState } from "../api/sse";
import { isAlreadyLoggedOut, useLogout, useMe, useMeInitials } from "../hooks/use-auth";
import { useHealth } from "../hooks/use-stats";
import {
  DASHBOARD_PATH,
  FILTERING_BASE,
  findActiveGroup,
  isNavItemActive,
  NAV_GROUPS,
  QUERY_LOG_PATH,
  type NavGroup,
} from "../lib/nav";
import { useLists } from "../hooks/use-filters";
import { useLiveTailStatus } from "../lib/live-tail";
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
          {NAV_GROUPS.map((group) => (
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
                would leave this cell announcing as "search⌘K". */}{" "}
            {/* rnui's Kbd rather than a bare "⌘K": a keyboard shortcut is a
                <kbd>, and the component is what makes it look like a key
                instead of two more characters of label. */}
            <Kbd>⌘K</Kbd>
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
              would make the bar shout twice. */}
          <ResolverStatusCell />
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
        </div>
      )}
    </header>
  );
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
 * Row 1's second readout: is the *resolver* answering (GET /health)?
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
 */
function ResolverStatusCell() {
  const health = useHealth();
  const label = health.isPending ? "Checking DNS…" : health.isError ? "DNS down" : "DNS OK";

  return (
    // <output> rather than a div with role="status": same implicit role,
    // same polite live region, one fewer ARIA attribute to keep honest.
    <output
      className={cn(
        CELL,
        "border-l border-l-border text-xs font-medium tracking-wider",
        health.isError
          ? "text-destructive"
          : health.isPending
            ? "text-muted-foreground"
            : "text-primary",
      )}
    >
      <span className="sr-only">Resolver: </span>
      {label}
    </output>
  );
}
