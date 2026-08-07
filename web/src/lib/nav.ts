import type { LucideIcon } from "lucide-react";
import {
  CircleUserRound,
  Globe,
  LayoutDashboard,
  ListFilter,
  ScrollText,
  Settings,
  ShieldBan,
  Users,
} from "lucide-react";

/**
 * The dashboard. Named because two other places need to recognise it: the
 * chrome hangs that screen's own cells off it (components/top-nav.tsx), and
 * the shell drops its page padding for it (components/app-shell.tsx) — the
 * dashboard is a full-bleed grid of hairline-separated bands, and a padded
 * <main> would leave every one of those rules floating in a 24px gutter.
 */
export const DASHBOARD_PATH = "/";

/**
 * The query log. Recognised for the same two reasons the dashboard is: the
 * chrome hangs that screen's own cells off it (components/top-nav.tsx), and
 * the shell drops its page padding for it (components/app-shell.tsx).
 */
export const QUERY_LOG_PATH = "/queries";

/**
 * Screens that own the whole content area rather than sitting inside the
 * shell's gutter: both are full-bleed grids of hairline-separated bands, and
 * their rules have to meet the viewport edges instead of floating in a 24px
 * frame.
 */
export function isFullBleedRoute(pathname: string): boolean {
  return pathname === DASHBOARD_PATH || pathname === QUERY_LOG_PATH;
}

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  /** Match the route exactly rather than as a prefix (like index routes). */
  end?: boolean;
}

export interface NavGroup {
  /** Stable handle for keys, `data-*` hooks and tests. */
  id: string;
  /**
   * Title case in the source, uppercased by CSS in the chrome. Keeping the
   * cased string here is what stops screen readers spelling out "M-O-N-I-T-O-R"
   * and keeps the accessible name of each menu button readable.
   */
  label: string;
  /**
   * Routes under this prefix belong to the group even when no leaf item
   * matches yet — `/filtering` is the redirecting index of `/filtering/lists`,
   * and the chrome must not blink back to MONITOR for that one frame.
   */
  basePath?: string;
  items: NavItem[];
}

/**
 * The shell's two-level nav. Row 1 of the top bar renders the groups; row 2
 * renders the active group's items as tabs. Grouping is fixed by the locked
 * design; the *contents* are only ever routes that actually exist — there are
 * no entries for DHCP, encrypted DNS or anything else unbuilt, and a group
 * with a single child (Local DNS) still gets its row-2 tab.
 */
export const NAV_GROUPS: NavGroup[] = [
  {
    id: "monitor",
    label: "Monitor",
    items: [
      { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
      { to: "/queries", label: "Query Log", icon: ScrollText },
    ],
  },
  {
    id: "filtering",
    label: "Filtering",
    basePath: "/filtering",
    items: [
      { to: "/filtering/lists", label: "Lists", icon: ListFilter },
      { to: "/filtering/rules", label: "Rules", icon: ShieldBan },
      { to: "/filtering/clients", label: "Groups & Clients", icon: Users },
    ],
  },
  {
    id: "network",
    // The group is named for what it actually holds. "Network" promised
    // DHCP, interfaces and encrypted DNS — none of which exist — so the
    // one thing behind it (Local DNS) is what the chrome now says. The id
    // stays `network` because it's a stable handle for keys and tests, not
    // a user-facing string, and the group is still where those routes will
    // land when they're built.
    label: "Local DNS",
    items: [{ to: "/dns", label: "Local DNS", icon: Globe }],
  },
  {
    id: "system",
    label: "System",
    items: [
      { to: "/settings", label: "Settings", icon: Settings },
      { to: "/account", label: "Account", icon: CircleUserRound },
    ],
  },
];

/** Every leaf page, flattened — what the command palette navigates to. */
export const navItems: NavItem[] = NAV_GROUPS.flatMap((group) => group.items);

export function isNavItemActive(pathname: string, item: NavItem): boolean {
  if (item.end) return pathname === item.to;
  return pathname === item.to || pathname.startsWith(`${item.to}/`);
}

/**
 * Which group's tabs row 2 shows, or `null` when the path is in no group at
 * all — which now means exactly one thing: the not-found screen.
 *
 * This used to fall back to the first group, because an unknown path was
 * only ever a transient on its way to `/` via the catch-all redirect, and an
 * empty second row for that one frame read as a broken shell. The redirect
 * is gone (pages/not-found.tsx renders instead), so the fallback would now
 * be a lie that persists: MONITOR marked, and a row of Dashboard/Query Log
 * tabs, on a page that is neither.
 */
export function findActiveGroup(pathname: string): NavGroup | null {
  return (
    NAV_GROUPS.find((group) => group.items.some((item) => isNavItemActive(pathname, item))) ??
    NAV_GROUPS.find(
      (group) =>
        group.basePath !== undefined &&
        (pathname === group.basePath || pathname.startsWith(`${group.basePath}/`)),
    ) ??
    null
  );
}
