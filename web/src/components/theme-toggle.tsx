import { Moon, Sun } from "lucide-react";
import { SidebarMenuButton } from "@e412/rnui-react";
import { useTheme } from "../lib/theme";

export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const isDark = theme === "dark";
  const label = isDark ? "Switch to light theme" : "Switch to dark theme";

  // A sidebar menu button rather than a standalone icon button: this now
  // lives in the sidebar footer next to the account menu, and
  // SidebarMenuButton is what shrinks to an icon-only target — with
  // `tooltip` standing in for the clipped text — in `collapsible="icon"`
  // mode. The label is the action, not the state, so it doubles as the
  // accessible name in both the expanded and collapsed rail (no aria-label
  // diverging from what's on screen).
  return (
    <SidebarMenuButton onClick={toggleTheme} tooltip={label}>
      {isDark ? <Sun /> : <Moon />}
      <span>{label}</span>
    </SidebarMenuButton>
  );
}
