import type { LucideIcon } from "lucide-react";
import {
  CircleUserRound,
  Globe,
  KeyRound,
  LayoutDashboard,
  ListFilter,
  Network,
  Router,
  ScrollText,
  Settings,
  ShieldBan,
  Tag,
  Users,
} from "lucide-react";

/**
 * The dashboard. Named because the chrome hangs that screen's own cells off
 * it (components/top-nav.tsx): the stats window selector belongs to this
 * route and to no other.
 */
export const DASHBOARD_PATH = "/";

/**
 * The query log. Recognised for the same reason the dashboard is: the chrome
 * hangs that screen's own cells off it (components/top-nav.tsx) — the tail's
 * readout and its LIVE/PAUSE toggle.
 */
export const QUERY_LOG_PATH = "/queries";

/** The Filtering section's base. `/filtering` itself only redirects to
 * Lists; the chrome uses this to recognise the section and its tabs. */
export const FILTERING_BASE = "/filtering";

/** The DHCP section's base, which is also its first screen (Scopes). The
 * chrome hangs each of the three screens' own right-hand readout off these
 * — how full the pools are, how fresh the lease table is, how many
 * reservations exist. */
export const DHCP_BASE = "/dhcp";
export const DHCP_LEASES_PATH = "/dhcp/leases";
export const DHCP_RESERVATIONS_PATH = "/dhcp/reservations";

interface NavItem {
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
 * design; the *contents* are only ever routes that actually exist — there is
 * no entry for encrypted DNS or anything else unbuilt, and a group with a
 * single child (Zones) still gets its row-2 tab.
 *
 * DHCP is the one group that is not always on: an instance whose
 * `kea_socket` is empty has no engine and every DHCP route but the status
 * 404s, so the section is dropped whole rather than offered and broken. Read
 * this list through `navGroups` rather than directly — see there.
 */
const ALL_NAV_GROUPS: NavGroup[] = [
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
    // one thing behind it is what the chrome says: Zones replaced the old
    // flat Local DNS override table with real authoritative zones (see
    // api/types.ts's Zone doc). The id stays `network` because it's a
    // stable handle for keys and tests, not a user-facing string, and the
    // group is still where those routes will land when they're built.
    label: "Zones",
    items: [{ to: "/zones", label: "Zones", icon: Globe }],
  },
  {
    id: "dhcp",
    label: "DHCP",
    items: [
      // `/dhcp` *is* Scopes rather than a redirecting index: the section has
      // a first screen, and a route that only forwards would put a dead
      // entry in the back stack on every visit.
      { to: DHCP_BASE, label: "Scopes", icon: Network, end: true },
      { to: DHCP_LEASES_PATH, label: "Leases", icon: Router },
      { to: DHCP_RESERVATIONS_PATH, label: "Reservations", icon: Tag },
    ],
  },
  {
    id: "system",
    label: "System",
    items: [
      { to: "/settings", label: "Settings", icon: Settings },
      // A tab of its own rather than a section inside Settings: a TSIG key
      // is a credential that another server holds a copy of, not a
      // preference of this one, and it has its own CRUD screen.
      { to: "/tsig-keys", label: "TSIG keys", icon: KeyRound },
      { to: "/account", label: "Account", icon: CircleUserRound },
    ],
  },
];

/**
 * The groups this instance actually has, which is every group except DHCP
 * on a box with no engine.
 *
 * A function rather than a second constant because the answer arrives with
 * `GET /resolver/status` and can change under a running app (a restart with
 * `kea_socket` set). Both readers of the nav — the top bar and the command
 * palette — go through this, so the palette cannot offer a jump to a
 * section the bar has hidden.
 */
export function navGroups(dhcpEnabled: boolean): NavGroup[] {
  return dhcpEnabled ? ALL_NAV_GROUPS : ALL_NAV_GROUPS.filter((group) => group.id !== "dhcp");
}

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
 *
 * Searched over *every* group, DHCP included, whatever the engine is doing:
 * an operator who reaches `/dhcp` on a box that has since lost its engine
 * gets the section's own tabs above the screen telling them so, rather than
 * the not-found chrome.
 */
export function findActiveGroup(pathname: string): NavGroup | null {
  return (
    ALL_NAV_GROUPS.find((group) => group.items.some((item) => isNavItemActive(pathname, item))) ??
    ALL_NAV_GROUPS.find(
      (group) =>
        group.basePath !== undefined &&
        (pathname === group.basePath || pathname.startsWith(`${group.basePath}/`)),
    ) ??
    null
  );
}
