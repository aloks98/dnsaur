import { Moon, Sun } from "lucide-react";
import { Button } from "@e412/rnui-react";
import { useTheme } from "../lib/theme";

export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const isDark = theme === "dark";

  // Icon button (not rnui's Switch) is deliberate: it matches the other
  // icon-only controls in the header's utility cluster (pause, account).
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon-sm"
      aria-label={isDark ? "Switch to light theme" : "Switch to dark theme"}
      aria-pressed={isDark}
      onClick={toggleTheme}
    >
      {isDark ? <Sun /> : <Moon />}
    </Button>
  );
}
