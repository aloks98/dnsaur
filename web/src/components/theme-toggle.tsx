import { Moon, Sun } from "lucide-react";
import { DropdownMenuItem } from "@e412/rnui-react";
import { useTheme } from "../lib/theme";

/**
 * Lives inside the top bar's System menu, next to Settings, Account and log
 * out — the design's chrome has no standalone theme control, and the sidebar
 * footer that used to hold this one no longer exists.
 *
 * `closeOnClick={false}` because the point of a theme switch is comparing:
 * flipping back because the other one looked better shouldn't cost a second
 * trip through the menu.
 *
 * The label is the *action*, not the state, so it doubles as the accessible
 * name with no aria-label diverging from what's on screen.
 */
export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const isDark = theme === "dark";
  const label = isDark ? "Switch to light theme" : "Switch to dark theme";

  return (
    <DropdownMenuItem closeOnClick={false} onClick={toggleTheme} className="whitespace-nowrap">
      {isDark ? <Sun /> : <Moon />}
      {label}
    </DropdownMenuItem>
  );
}
