import { useCallback, useEffect, useSyncExternalStore } from "react";
import type { SseState } from "../api/sse";

/**
 * Is the query log's live tail paused?
 *
 * The design spends the chrome's one filled cell on this screen's CTA — the
 * `LIVE · PAUSE` toggle in row 2 of the top bar (components/top-nav.tsx) —
 * while the stream that toggle governs is consumed by the routed page
 * underneath it (pages/queries.tsx). Those two are siblings under AppShell,
 * so the flag can't simply be state in either of them:
 *
 *   - lifting it into AppShell would re-render every page in the app on a
 *     pause, and give the shell a piece of one screen's state to carry
 *     around on all the others;
 *   - putting it in the URL (the trick WindowCells uses for the stats
 *     window) would make "paused" somewhere the back button can return to,
 *     which it isn't — it's a momentary hold on a stream, not a view of
 *     the data you could bookmark.
 *
 * So it's a tiny external store, the same shape lib/theme.ts uses, and every
 * subscriber observes one live value.
 *
 * Pausing only ever flips `enabled` on useLiveTail. That hook owns the ring
 * buffer and never resets it (hooks/use-queries.ts), and it flushes whatever
 * is still batched from the effect's own cleanup — so a pause keeps every
 * row already on screen, loses nothing mid-burst, and a resume prepends only
 * what arrives after it.
 */
type Listener = () => void;

/**
 * The status the chrome renders and the page owns.
 *
 * `paused` is a control the chrome writes and the page obeys. The other
 * three go the other way: only the routed page knows whether a filter is
 * active (which swaps the tail for paged search) or how the subscription is
 * faring, so it publishes them and the chrome reads them. Keeping all four
 * in one store is what lets row 2 render a single readout instead of the
 * four overlapping ones this replaced.
 */
interface TailStatus {
  paused: boolean;
  filtered: boolean;
  streamState: SseState;
  reconnect: () => void;
}

const listeners = new Set<Listener>();
let status: TailStatus = {
  paused: false,
  filtered: false,
  streamState: "closed",
  reconnect: () => {},
};
let paused = false;

function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot(): boolean {
  return paused;
}

function getStatus(): TailStatus {
  return status;
}

function emit(): void {
  listeners.forEach((listener) => listener());
}

/**
 * Set the flag from outside React. The hook's setter is the normal way in;
 * this is also what tests use to put the module back to its default, since
 * a module-level store outlives the components in any one test case.
 */
export function setLiveTailPaused(next: boolean): void {
  if (next === paused) return;
  paused = next;
  status = { ...status, paused: next };
  emit();
}

/**
 * Publish the half of the status only the page can know. Called from an
 * effect in pages/queries.tsx; a no-op when nothing actually moved, so a
 * tail commit ten times a second doesn't wake the chrome.
 */
export function setLiveTailReport(next: Omit<TailStatus, "paused">): void {
  if (
    next.filtered === status.filtered &&
    next.streamState === status.streamState &&
    next.reconnect === status.reconnect
  ) {
    return;
  }
  status = { ...next, paused };
  emit();
}

/**
 * Reset to defaults when the query log unmounts, so the chrome on another
 * screen never reports a stream that is no longer running.
 *
 * `paused` goes with it. It is a momentary hold on a stream, not a view of
 * the data (see the module comment) — and it is the one piece of this store
 * that outlives the page, while the ring buffer it governs does not. Left
 * set, coming back to the query log rendered an empty table labelled
 * "Paused": no history, because the seed only runs while enabled, and no
 * stream to fill it.
 */
export function resetLiveTailReport(): void {
  setLiveTailPaused(false);
  setLiveTailReport({ filtered: false, streamState: "closed", reconnect: () => {} });
}

/**
 * Read the status without a component. The chrome is what renders it, so a
 * page-level test asserting "the tail reported itself as failed" observes it
 * here rather than looking for text this page no longer owns.
 */
export function getLiveTailStatus(): TailStatus {
  return status;
}

/** Everything row 2 needs to render one readout and one toggle. */
export function useLiveTailStatus(): TailStatus & { setPaused: (next: boolean) => void } {
  const value = useSyncExternalStore(subscribe, getStatus);
  const setPaused = useCallback((next: boolean) => setLiveTailPaused(next), []);
  return { ...value, setPaused };
}

/**
 * The page side: publish its half of the status for as long as it is
 * mounted, and clear it on the way out.
 */
export function usePublishLiveTailStatus(report: Omit<TailStatus, "paused">): void {
  const { filtered, streamState, reconnect } = report;
  useEffect(() => {
    setLiveTailReport({ filtered, streamState, reconnect });
  }, [filtered, streamState, reconnect]);
  useEffect(() => resetLiveTailReport, []);
}

export function useLiveTailPaused(): {
  paused: boolean;
  setPaused: (next: boolean) => void;
} {
  const value = useSyncExternalStore(subscribe, getSnapshot);
  const setPaused = useCallback((next: boolean) => setLiveTailPaused(next), []);
  return { paused: value, setPaused };
}
