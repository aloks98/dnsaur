import { ChevronDown, LogOut } from "lucide-react";
import { NavLink, useLocation } from "react-router";
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
import { findActiveGroup, isNavItemActive, NAV_GROUPS, type NavGroup } from "../lib/nav";
import { DnsaurLogo } from "./dnsaur-logo";
import { PauseControl } from "./pause-control";
import { ThemeToggle } from "./theme-toggle";

interface TopNavProps {
  onOpenCommandPalette: () => void;
}

/** Shared by every cell in both rows: mono, uppercase, wide tracking. */
const CELL =
  "flex shrink-0 items-center whitespace-nowrap font-mono uppercase transition-colors " +
  "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring";

/**
 * The app shell's chrome: two hairline-bordered rows in place of the old
 * sidebar. Row 1 is identity + the four nav *groups* + the global right-hand
 * cells (search, blocking); row 2 is the active group's pages as tabs, plus
 * the contextual right-hand cells.
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

  return (
    <header
      data-slot="top-nav"
      className="sticky top-0 z-20 shrink-0 bg-background text-foreground"
    >
      {/* ---- Row 1: identity, groups, global readouts ------------------- */}
      <div className="flex h-[42px] items-stretch border-b border-border">
        <div className={cn(CELL, "gap-2 border-r border-border px-3.5")}>
          <DnsaurLogo size={22} />
          {/* Below `sm` the tile carries the identity on its own and the
              wordmark's ~55px go to the nav strip instead — `sr-only`, not
              `hidden`, so it stays in the accessible name and the DOM. */}
          <span className="font-heading text-[13px] font-bold tracking-[-0.02em] normal-case max-sm:sr-only">
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
            className={cn(
              CELL,
              "border-l border-border px-[15px] text-[11px] font-normal tracking-[0.1em]",
              "text-muted-foreground hover:text-foreground",
            )}
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
        </div>
      </div>

      {/* ---- Row 2: the active group's pages ---------------------------- */}
      <div className="flex h-[38px] items-stretch border-b border-border bg-card">
        <nav aria-label={activeGroup.label} className="dnsaur-scroll-x flex min-w-0 items-stretch">
          {activeGroup.items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={cn(
                CELL,
                "px-[15px] text-[10.5px] font-normal tracking-[0.1em]",
                "text-muted-foreground hover:text-foreground",
                isNavItemActive(pathname, item) &&
                  "font-semibold text-foreground shadow-[inset_0_-2px_0_var(--primary)]",
              )}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="ml-auto flex shrink-0 items-stretch">
          <ResolverStatusCell />
        </div>
      </div>
    </header>
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
        className={cn(
          CELL,
          "gap-1.5 border-r border-border px-[15px] text-[11px] font-normal tracking-[0.1em]",
          "text-muted-foreground hover:text-foreground",
          active && "font-semibold text-foreground shadow-[inset_0_-2px_0_var(--primary)]",
        )}
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
 * Row 2's terminal filled cell: is the *resolver* answering (GET /health)?
 *
 * This is the app's only liveness signal and it used to be the sidebar
 * footer's single status dot; the footer is gone, so it takes the design's
 * one filled cell at the end of the second row. It is deliberately not a
 * second dot next to the blocking readout — blocking's paused/active state
 * is part of the pause control itself (see pause-control.tsx).
 *
 * "Unreachable" swaps the fill to destructive: a red-on-emerald mismatch is
 * the point, a confident primary-green "UNREACHABLE" would not be.
 */
function ResolverStatusCell() {
  const health = useHealth();
  const label = health.isPending ? "Checking…" : health.isError ? "Unreachable" : "Resolving";

  return (
    // <output> rather than a div with role="status": same implicit role,
    // same polite live region, one fewer ARIA attribute to keep honest.
    <output
      className={cn(
        CELL,
        "border-l border-border px-[15px] text-[10.5px] font-semibold tracking-[0.1em]",
        health.isError
          ? "bg-destructive text-destructive-solid-foreground"
          : health.isPending
            ? "bg-muted text-muted-foreground"
            : "bg-primary text-primary-foreground",
      )}
    >
      <span className="sr-only">Resolver: </span>
      {label}
    </output>
  );
}
