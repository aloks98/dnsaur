import type { LucideIcon } from "lucide-react";
import {
  CircleUserRound,
  Globe,
  LayoutDashboard,
  ScrollText,
  Settings,
  ShieldBan,
} from "lucide-react";
import { NavLink, useLocation } from "react-router";
import {
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
} from "@e412/rnui-react";

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

export function SidebarNav() {
  const location = useLocation();

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex items-center gap-2 px-2 py-1.5">
          <span className="font-heading text-lg font-semibold tracking-tight text-sidebar-foreground">
            <span className="text-primary">d</span>nsaur
          </span>
        </div>
        <p className="px-2 text-xs text-sidebar-foreground/60 group-data-[collapsible=icon]:hidden">
          DNS · filtering · observability
        </p>
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
        <StatusIndicator
          state="active"
          label="Resolving"
          size="sm"
          className="px-2 py-1.5 group-data-[collapsible=icon]:justify-center"
          labelClassName="group-data-[collapsible=icon]:hidden"
        />
      </SidebarFooter>
    </Sidebar>
  );
}
