import { ChevronDown, LogOut } from "lucide-react";
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
} from "@e412/rnui-react";
import { isAlreadyLoggedOut, useLogout, useMe, useMeInitials } from "../hooks/use-auth";
import { useHealth } from "../hooks/use-stats";
import {
  DASHBOARD_PATH,
  findActiveGroup,
  isNavItemActive,
  NAV_GROUPS,
  type NavGroup,
} from "../lib/nav";
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
 * the stats window selector and the one filled primary cell in the whole
 * shell, which the design spends on that screen's CTA.
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
  // Row 2's right-hand cells are contextual, and both of these are about the
  // dashboard: a stats window has nothing to govern on Settings, and a
  // "view query log" CTA is noise on the query log itself.
  const onDashboard = pathname === DASHBOARD_PATH;

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
            <NavGroupMenu key={group.id} group={group} active={group.id === activeGroup.id} />
          ))}
        </nav>

        <div className="ml-auto flex shrink-0 items-stretch">
          <button
            type="button"
            onClick={onOpenCommandPalette}
            className={cn(CELL, "border-l border-l-border", CELL_QUIET)}
          >
            {/* Same trick as the wordmark, below desktop: at tablet
                width those ~75px are the difference between all four groups
                fitting and System being the one you have to scroll for. The
                shortcut alone is affordance enough, and the words stay in
                the accessible name rather than being dropped from it. */}
            <span className="max-lg:sr-only">/ search · </span>⌘K
          </button>
          {/* Global blocking (group 0). Still the same control — state
              readout and pause/resume menu in one — just wearing the
              chrome's cell skin. See components/pause-control.tsx. */}
          <PauseControl variant="chrome" />
          {/* Resolver liveness sits next to blocking rather than in row 2:
              they are the shell's two "is this thing working" readouts and
              read as a pair, and row 2's terminal cell is the design's one
              filled cell — spent below on the dashboard's CTA, not on a
              status that is green nearly all the time. */}
          <ResolverStatusCell />
        </div>
      </div>

      {/* ---- Row 2: the active group's pages ---------------------------- */}
      <div className="flex h-10 items-stretch border-b border-border bg-card">
        <nav aria-label={activeGroup.label} className="dnsaur-scroll-x flex min-w-0 items-stretch">
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
            {/* The design's single filled cell, and the dashboard's one
                call to action: the numbers above are a summary, the log is
                where you actually go to look. */}
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
      </div>
    </header>
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
      <DropdownMenuTrigger
        data-active={active}
        className={cn(CELL, "gap-1.5 border-r border-r-border", CELL_QUIET, active && CELL_ACTIVE)}
      >
        {group.label}
        <ChevronDown className="size-3 opacity-60" />
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
