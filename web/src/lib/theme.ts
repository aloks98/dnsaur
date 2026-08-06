import { useCallback, useSyncExternalStore } from "react";

export type Theme = "light" | "dark";

// Key spelled as the design spec asks ("dnsaur.theme").
export const THEME_STORAGE_KEY = "dnsaur.theme";

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

function notify(): void {
  listeners.forEach((listener) => listener());
}

function setThemeInternal(next: Theme): void {
  currentTheme = next;
  applyTheme(next);
  persistTheme(next);
  notify();
}

// One MediaQueryList for the module's lifetime: window.matchMedia() hands
// back a *new* object per call, so a fresh one per subscribe would attach
// listeners to instances nothing can ever detach from.
let darkModeMedia: MediaQueryList | null | undefined;

function darkModeQuery(): MediaQueryList | null {
  if (darkModeMedia === undefined) {
    darkModeMedia =
      typeof window === "undefined" || typeof window.matchMedia !== "function"
        ? null
        : window.matchMedia("(prefers-color-scheme: dark)");
  }
  return darkModeMedia;
}

// While the user has made no explicit choice, the app *follows* the OS
// rather than sampling it once at import time and freezing: flipping the
// system to dark at sunset used to leave dnsaur bright until a reload.
// A stored theme is an explicit override and always wins.
function handleSystemThemeChange(event: MediaQueryListEvent): void {
  if (getStoredTheme() !== null) return;
  const next: Theme = event.matches ? "dark" : "light";
  if (next === currentTheme) return;
  currentTheme = next;
  applyTheme(next);
  notify();
}

function subscribe(listener: Listener): () => void {
  // Re-adding the same function reference is a no-op per the DOM spec, so
  // this stays a single registration however many components subscribe. It
  // is never removed: the store itself outlives every component, and the
  // handler is a cheap no-op once an explicit theme is stored.
  const media = darkModeQuery();
  if (media && typeof media.addEventListener === "function") {
    media.addEventListener("change", handleSystemThemeChange);
  }
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
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
