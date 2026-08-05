import type { LucideIcon } from "lucide-react";
import {
  CircleUserRound,
  Globe,
  LayoutDashboard,
  LogOut,
  ScrollText,
  Settings,
  ShieldBan,
} from "lucide-react";
import { NavLink, useLocation } from "react-router";
import { toast } from "sonner";
import {
  Avatar,
  AvatarFallback,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarSeparator,
  StatusIndicator,
  type StatusIndicatorProps,
} from "@e412/rnui-react";
import { isAlreadyLoggedOut, useLogout, useMe, useMeInitials } from "../hooks/use-auth";
import { useHealth } from "../hooks/use-stats";
import { ThemeToggle } from "./theme-toggle";

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  /** Match the route exactly rather than as a prefix (like index routes). */
  end?: boolean;
}

export const navItems: NavItem[] = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
  { to: "/queries", label: "Query Log", icon: ScrollText },
  { to: "/filtering", label: "Filtering", icon: ShieldBan },
  { to: "/dns", label: "Local DNS", icon: Globe },
  { to: "/settings", label: "Settings", icon: Settings },
  { to: "/account", label: "Account", icon: CircleUserRound },
];

function isNavItemActive(pathname: string, item: NavItem): boolean {
  if (item.end) return pathname === item.to;
  return pathname === item.to || pathname.startsWith(`${item.to}/`);
}

// Derived from GET /health (see hooks/use-stats.ts's useHealth) — a real
// liveness signal, not decoration. "fixing" reads as an in-flight amber
// pulse rather than a hard down/up jump while the first check is pending.
function resolverStatus(health: ReturnType<typeof useHealth>): {
  state: NonNullable<StatusIndicatorProps["state"]>;
  label: string;
} {
  if (health.isPending) return { state: "fixing", label: "Checking…" };
  if (health.isError) return { state: "down", label: "Unreachable" };
  return { state: "active", label: "Resolving" };
}

/**
 * Avatar + account menu. Lives in the sidebar footer rather than the
 * header so every piece of chrome that is *about the user* (identity,
 * theme) sits together, leaving the header for what's about the current
 * view. Renders as a SidebarMenuButton so it collapses to a plain icon
 * target along with the nav in `collapsible="icon"` mode.
 */
function AccountMenu() {
  const me = useMe();
  const initials = useMeInitials();
  const logout = useLogout();

  const username = me.data?.username;
  // The visible label is the username, so it stays part of the accessible
  // name (WCAG 2.5.3) while "Account menu" keeps a stable handle for tests
  // and screen-reader users landing on an unfamiliar avatar.
  const triggerLabel = username ? `Account menu (${username})` : "Account menu";

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<SidebarMenuButton aria-label={triggerLabel} tooltip={triggerLabel} />}
      >
        <Avatar size="sm">
          <AvatarFallback>{initials}</AvatarFallback>
        </Avatar>
        <span>{username ?? "Account"}</span>
      </DropdownMenuTrigger>
      {/* Anchored to the side, not below: the trigger sits at the very
          bottom of the viewport, and when the rail is collapsed there is no
          width to align against either. */}
      <DropdownMenuContent side="right" align="end">
        {/* Menu.GroupLabel (rnui's DropdownMenuLabel) throws unless it's
            inside a Menu.Group — base-ui requires that context even for
            a single ungrouped label. */}
        <DropdownMenuGroup>
          <DropdownMenuLabel>{username ?? "Account"}</DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          variant="destructive"
          onClick={() =>
            logout.mutate(undefined, {
              // A 401 is the session already being gone, which useLogout
              // treats as a completed logout (see hooks/use-auth.ts) — it
              // navigates, so there is nothing to apologise for.
              onError: (error) => {
                if (!isAlreadyLoggedOut(error)) toast.error("Couldn't sign out — try again");
              },
            })
          }
        >
          <LogOut />
          Log out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function SidebarNav() {
  const location = useLocation();
  const health = useHealth();
  const status = resolverStatus(health);

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex items-center gap-2 px-2 py-1.5">
          <span className="font-heading text-lg font-semibold tracking-tight text-sidebar-foreground">
            <span className="text-primary">d</span>nsaur
          </span>
        </div>
      </SidebarHeader>
      <SidebarSeparator />
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupContent>
            <nav aria-label="Primary">
              <SidebarMenu>
                {navItems.map((item) => {
                  const Icon = item.icon;
                  return (
                    <SidebarMenuItem key={item.to}>
                      <SidebarMenuButton
                        isActive={isNavItemActive(location.pathname, item)}
                        render={<NavLink to={item.to} end={item.end} />}
                      >
                        <Icon />
                        <span>{item.label}</span>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  );
                })}
              </SidebarMenu>
            </nav>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarSeparator />
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <AccountMenu />
          </SidebarMenuItem>
          <SidebarMenuItem>
            <ThemeToggle />
          </SidebarMenuItem>
        </SidebarMenu>
        {/* The app's only status dot. It reports whether the *resolver* is
            answering (GET /health); blocking's paused/active state is not a
            second dot next to it but part of the header's pause button
            itself — see components/pause-control.tsx. */}
        <StatusIndicator
          state={status.state}
          label={status.label}
          size="sm"
          className="px-2 py-1.5 group-data-[collapsible=icon]:justify-center"
          labelClassName="group-data-[collapsible=icon]:hidden"
        />
      </SidebarFooter>
    </Sidebar>
  );
}
