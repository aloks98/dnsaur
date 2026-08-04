import type { QueryEntry } from "./types";

export type SseState = "open" | "reconnecting" | "closed";

const TAIL_PATH = "/api/v1/queries/tail";
const INITIAL_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 30_000;

/**
 * Subscribes to the live query-log tail: `GET /api/v1/queries/tail`, a
 * `text/event-stream` where each event is `data: <QueryEntry JSON>\n\n`.
 * The session cookie rides along automatically via EventSource — no auth
 * header is possible or needed.
 *
 * Reconnects on error with a doubling, capped backoff (1s → 30s) so a
 * server restart or network blip doesn't hammer the endpoint, reporting
 * each transition through `onState`. Returns an unsubscribe function that
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
  let backoffMs = INITIAL_BACKOFF_MS;
  let unsubscribed = false;

  function connect() {
    source = new EventSource(TAIL_PATH);

    source.onopen = () => {
      backoffMs = INITIAL_BACKOFF_MS;
      onState("open");
    };

    source.onmessage = (event: MessageEvent<string>) => {
      try {
        onEntry(JSON.parse(event.data) as QueryEntry);
      } catch {
        // Malformed payload — drop it, keep the stream alive.
      }
    };

    source.onerror = () => {
      source?.close();
      source = null;
      if (unsubscribed) return;
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
    source?.close();
    onState("closed");
  };
}
