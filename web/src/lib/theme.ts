import { useCallback, useSyncExternalStore } from "react";

export type Theme = "light" | "dark";

export const THEME_STORAGE_KEY = "dnsaur-theme";

function isTheme(value: string | null): value is Theme {
  return value === "light" || value === "dark";
}

export function getStoredTheme(): Theme | null {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    return isTheme(stored) ? stored : null;
  } catch {
    // localStorage unavailable (private browsing, disabled storage, ...).
    return null;
  }
}

export function getPreferredTheme(): Theme {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
    return "light";
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function getInitialTheme(): Theme {
  return getStoredTheme() ?? getPreferredTheme();
}

export function applyTheme(theme: Theme): void {
  document.documentElement.classList.toggle("dark", theme === "dark");
}

export function persistTheme(theme: Theme): void {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // localStorage unavailable; theme just won't persist across reloads.
  }
}

// A tiny external store (rather than plain useState) so every component
// that calls useTheme() — ThemeToggle, AppShell's Toaster, etc. — observes
// the same live value instead of drifting out of sync with each other.
type Listener = () => void;
const listeners = new Set<Listener>();
let currentTheme: Theme = getInitialTheme();
applyTheme(currentTheme);

function setThemeInternal(next: Theme): void {
  currentTheme = next;
  applyTheme(next);
  persistTheme(next);
  listeners.forEach((listener) => listener());
}

function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(): Theme {
  return currentTheme;
}

export function useTheme(): {
  theme: Theme;
  setTheme: (theme: Theme) => void;
  toggleTheme: () => void;
} {
  const theme = useSyncExternalStore(subscribe, getSnapshot);

  const setTheme = useCallback((next: Theme) => {
    setThemeInternal(next);
  }, []);

  const toggleTheme = useCallback(() => {
    setThemeInternal(currentTheme === "dark" ? "light" : "dark");
  }, []);

  return { theme, setTheme, toggleTheme };
}
