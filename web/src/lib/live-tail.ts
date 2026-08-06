import { useCallback, useSyncExternalStore } from "react";

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

const listeners = new Set<Listener>();
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

/**
 * Set the flag from outside React. The hook's setter is the normal way in;
 * this is also what tests use to put the module back to its default, since
 * a module-level store outlives the components in any one test case.
 */
export function setLiveTailPaused(next: boolean): void {
  if (next === paused) return;
  paused = next;
  listeners.forEach((listener) => listener());
}

export function useLiveTailPaused(): {
  paused: boolean;
  setPaused: (next: boolean) => void;
} {
  const value = useSyncExternalStore(subscribe, getSnapshot);
  const setPaused = useCallback((next: boolean) => setLiveTailPaused(next), []);
  return { paused: value, setPaused };
}
