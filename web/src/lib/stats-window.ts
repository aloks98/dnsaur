/**
 * The dashboard's time window (1h / 24h / 7d).
 *
 * It lives in the URL rather than in component state because the control
 * and the content are no longer in the same component: the design puts the
 * selector in the shell's second chrome row (components/top-nav.tsx) and the
 * numbers it governs on the page below (pages/dashboard.tsx). A query param
 * keeps those two in step without a global store, and makes a window
 * deep-linkable and reload-stable for free.
 */

export const WINDOW_PARAM = "window";

export const WINDOWS = [
  { value: "1h", short: "1h", label: "Last hour", hours: 1 },
  { value: "24h", short: "24h", label: "Last 24 hours", hours: 24 },
  { value: "7d", short: "7d", label: "Last 7 days", hours: 168 },
] as const;

export type WindowValue = (typeof WINDOWS)[number]["value"];

export const DEFAULT_WINDOW: WindowValue = "24h";

/** Anything unrecognised (absent, hand-edited, stale bookmark) is the
 * default — never an error state, and never an unvalidated `hours=` on the
 * wire. */
export function parseWindow(raw: string | null | undefined): WindowValue {
  return WINDOWS.find((w) => w.value === raw)?.value ?? DEFAULT_WINDOW;
}

export function hoursFor(value: WindowValue): number {
  return WINDOWS.find((w) => w.value === value)?.hours ?? 24;
}

/** Prose for the window, for text that reads as a sentence. */
export function windowPhrase(hours: number): string {
  if (hours <= 1) return "in the last hour";
  if (hours <= 24) return "in the last 24 hours";
  return "in the last 7 days";
}
