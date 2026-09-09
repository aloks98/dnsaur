import type { QueryEntry } from "./types";

export type SseState = "open" | "reconnecting" | "closed" | "failed";

const TAIL_PATH = "/api/v1/queries/tail";
const INITIAL_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 30_000;
// EventSource.onerror carries no status code, so a 401 (expired session) is
// indistinguishable from a network blip — retrying forever would leave a
// dead session spinning "Reconnecting…" until the tab is closed. After this
// many consecutive failures with no *working* stream in between (see
// STABLE_OPEN_MS), give up and report a terminal state the UI can offer an
// explicit retry from.
const MAX_CONSECUTIVE_FAILURES = 6;
// How long an open connection has to hold before it counts as a working
// stream. A 401 on the tail endpoint reaches EventSource as an ordinary
// error *after* the connection opened, so "it opened" on its own is not
// evidence of anything: treating it as evidence reset the backoff on every
// attempt, which turned that loop into a flat 1s retry that never gave up.
// A delivered row is the other, better proof — a quiet resolver just may
// not have one to send.
const STABLE_OPEN_MS = 5_000;

/**
 * Subscribes to the live query-log tail: `GET /api/v1/queries/tail`, a
 * `text/event-stream` where each event is `data: <QueryEntry JSON>\n\n`.
 * The session cookie rides along automatically via EventSource — no auth
 * header is possible or needed.
 *
 * Reconnects on error with a doubling, capped backoff (1s → 30s) so a
 * server restart or network blip doesn't hammer the endpoint, reporting
 * each transition through `onState` — and stops after
 * MAX_CONSECUTIVE_FAILURES with a terminal "failed" state, since the most
 * likely cause of an endpoint that never comes back is a session that no
 * longer exists. Returns an unsubscribe function that
 * closes the connection and cancels any pending reconnect — call it on
 * unmount, or whenever the caller no longer wants live data (e.g. the
 * query log switching into filtered/paged mode).
 */
export function subscribeQueries(
  onEntry: (entry: QueryEntry) => void,
  onState: (state: SseState) => void,
): () => void {
  // Every browser dnsaur targets has EventSource; this guard only matters
  // for environments that don't (jsdom has no EventSource at all), so a
  // stray render doesn't throw — report "closed" and no-op.
  if (typeof EventSource === "undefined") {
    onState("closed");
    return () => {};
  }

  let source: EventSource | null = null;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  let stableTimer: ReturnType<typeof setTimeout> | undefined;
  let backoffMs = INITIAL_BACKOFF_MS;
  let failures = 0;
  let unsubscribed = false;

  /** This connection did something a broken one can't. */
  function markHealthy() {
    backoffMs = INITIAL_BACKOFF_MS;
    failures = 0;
  }

  function cancelStableTimer() {
    if (stableTimer !== undefined) {
      clearTimeout(stableTimer);
      stableTimer = undefined;
    }
  }

  function connect() {
    source = new EventSource(TAIL_PATH);

    source.onopen = () => {
      cancelStableTimer();
      stableTimer = setTimeout(markHealthy, STABLE_OPEN_MS);
      onState("open");
    };

    source.onmessage = (event: MessageEvent<string>) => {
      markHealthy();
      try {
        onEntry(JSON.parse(event.data) as QueryEntry);
      } catch {
        // Malformed payload — drop it, keep the stream alive.
      }
    };

    source.onerror = () => {
      source?.close();
      source = null;
      cancelStableTimer();
      if (unsubscribed) return;
      failures += 1;
      if (failures >= MAX_CONSECUTIVE_FAILURES) {
        onState("failed");
        return;
      }
      onState("reconnecting");
      reconnectTimer = setTimeout(() => {
        backoffMs = Math.min(backoffMs * 2, MAX_BACKOFF_MS);
        connect();
      }, backoffMs);
    };
  }

  connect();

  return () => {
    unsubscribed = true;
    if (reconnectTimer !== undefined) clearTimeout(reconnectTimer);
    cancelStableTimer();
    source?.close();
    onState("closed");
  };
}
