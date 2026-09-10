import { useSyncExternalStore } from "react";

export type Theme = "light" | "dark";

// Key spelled as the design spec asks ("dnsaur.theme"). public/theme-boot.js
// reads the same one before the bundle loads, so keep the two in step.
const STORAGE_KEY = "dnsaur.theme";

// One MediaQueryList for the module's lifetime: window.matchMedia() hands
// back a *new* object per call, so a fresh one per subscriber would attach
// listeners to instances nothing can ever detach from.
const darkMedia =
  typeof window !== "undefined" && typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-color-scheme: dark)")
    : null;

function storedTheme(): Theme | null {
  try {
    const value = window.localStorage.getItem(STORAGE_KEY);
    return value === "light" || value === "dark" ? value : null;
  } catch {
    // localStorage unavailable (private browsing, disabled storage, ...).
    return null;
  }
}

// A tiny external store (rather than plain useState) so every component that
// calls useTheme() — ThemeToggle, AppShell's Toaster, etc. — observes the same
// live value instead of drifting out of sync with each other.
const listeners = new Set<() => void>();
let currentTheme: Theme = storedTheme() ?? (darkMedia?.matches ? "dark" : "light");

function apply(next: Theme): void {
  currentTheme = next;
  document.documentElement.classList.toggle("dark", next === "dark");
  listeners.forEach((listener) => listener());
}

apply(currentTheme);

// While the user has made no explicit choice, the app *follows* the OS rather
// than sampling it once at import time and freezing: flipping the system to
// dark at sunset used to leave dnsaur bright until a reload. A stored theme is
// an explicit override and always wins.
darkMedia?.addEventListener("change", (event) => {
  if (storedTheme() !== null) return;
  const next: Theme = event.matches ? "dark" : "light";
  if (next !== currentTheme) apply(next);
});

function setTheme(next: Theme): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, next);
  } catch {
    // localStorage unavailable; theme just won't persist across reloads.
  }
  apply(next);
}

function toggleTheme(): void {
  setTheme(currentTheme === "dark" ? "light" : "dark");
}

function subscribe(listener: () => void): () => void {
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
  return { theme: useSyncExternalStore(subscribe, getSnapshot), setTheme, toggleTheme };
}
