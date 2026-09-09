import { delay, http, HttpResponse } from "msw";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import type { QueryEntry } from "../api/types";
import { makeQueryClient } from "../lib/query-client";
import {
  getLiveTailStatus,
  setLiveTailPaused,
  usePublishLiveTailStatus,
  useLiveTailPaused,
} from "../lib/live-tail";
import { FakeEventSource } from "../test/fake-event-source";
import { server } from "../test/msw-server";
import { authKeys } from "./use-auth";
import { LIVE_TAIL_CAP, useLiveTail } from "./use-queries";

function wrapper(client: QueryClient) {
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

function entry(patch: Partial<QueryEntry> = {}): QueryEntry {
  return {
    id: 0,
    at: 1_700_000_000_000,
    instance_id: "i1",
    client_ip: "10.0.0.5",
    client_id: 1,
    q_name: "example.com",
    q_type: "A",
    decision: "forwarded",
    rule_id: 0,
    list_id: 0,
    upstream: "1.1.1.1:53",
    r_code: "NOERROR",
    duration_ms: 4,
    ...patch,
  };
}

/** The seed page GET /queries answers, after `delayMs` — the ordering the
 * dedupe exists for is "the stream got there first". */
function seedWith(rows: QueryEntry[], delayMs = 0) {
  server.use(
    http.get("/api/v1/queries", async () => {
      if (delayMs > 0) await delay(delayMs);
      return HttpResponse.json(rows);
    }),
  );
}

function stream(): FakeEventSource {
  const source = FakeEventSource.instances.at(-1);
  if (!source) throw new Error("no EventSource was opened");
  return source;
}

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  setLiveTailPaused(false);
});

afterEach(() => {
  vi.unstubAllGlobals();
  setLiveTailPaused(false);
});

// The seed request and the subscription start together, so a query answered
// in between goes out on the stream with id 0 and comes back in the seed page
// with a real one. Only the fields both copies share can tell they are the
// same row — lib/query-rows.ts's rowKey deliberately cannot, since it keys
// the two sources into separate namespaces.
test("a row the stream already delivered is not re-added by the seed", async () => {
  const shared = entry({ q_name: "seeded-and-streamed.example" });
  seedWith([{ ...shared, id: 41 }, entry({ id: 40, q_name: "history-only.example" })], 400);

  const { result } = renderHook(() => useLiveTail(true), { wrapper: wrapper(makeQueryClient()) });

  act(() => stream().emit(shared));
  await waitFor(() => expect(result.current.entries).toHaveLength(1));
  await waitFor(() => expect(result.current.entries).toHaveLength(2));

  expect(result.current.entries.map((e) => e.q_name)).toEqual([
    "seeded-and-streamed.example",
    "history-only.example",
  ]);
});

test("the ring buffer keeps the newest LIVE_TAIL_CAP rows and drops the rest", async () => {
  seedWith([]);
  const { result } = renderHook(() => useLiveTail(true), { wrapper: wrapper(makeQueryClient()) });

  const total = LIVE_TAIL_CAP + 100;
  act(() => {
    for (let i = 0; i < total; i += 1) stream().emit(entry({ q_name: `q${i}.example` }));
  });

  await waitFor(() => expect(result.current.entries).toHaveLength(LIVE_TAIL_CAP));
  // Newest first, and the oldest 100 are gone rather than the newest.
  expect(result.current.entries[0].q_name).toBe(`q${total - 1}.example`);
  expect(result.current.entries.at(-1)?.q_name).toBe(`q${total - LIVE_TAIL_CAP}.example`);
});

// EventSource.onerror carries no status, so an expired session looks exactly
// like a blip until the stream gives up. At that point the likeliest cause is
// a session that no longer exists — so ask, and let the auth gate decide,
// instead of leaving a dead "Disconnected · Reconnect" on screen.
test("a stream that gives up revalidates me", async () => {
  seedWith([]);
  const client = makeQueryClient();
  client.setQueryData(authKeys.me, { id: 1, username: "admin", totp_enabled: false });

  const { result } = renderHook(() => useLiveTail(true), { wrapper: wrapper(client) });
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

  // Six consecutive failures is the terminal state; the five reconnects in
  // between are on the 1s → 30s doubling backoff (api/sse.ts).
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    for (const backoff of [1_000, 2_000, 4_000, 8_000, 16_000]) {
      act(() => stream().emitError());
      await act(async () => {
        await vi.advanceTimersByTimeAsync(backoff);
      });
    }
    act(() => stream().emitError());
  } finally {
    vi.useRealTimers();
  }

  await waitFor(() => expect(result.current.state).toBe("failed"));
  await waitFor(() => expect(client.getQueryState(authKeys.me)?.isInvalidated).toBe(true));
});

/** What pages/queries.tsx does: pause governs `enabled`, and the page
 * publishes its half of the chrome's readout for as long as it is mounted. */
function TailHarness() {
  const { paused } = useLiveTailPaused();
  const tail = useLiveTail(!paused);
  usePublishLiveTailStatus({
    filtered: false,
    streamState: tail.state,
    reconnect: tail.reconnect,
  });
  return null;
}

// `paused` is module state (the chrome's toggle and the page are siblings),
// while the ring buffer is per-mount and the seed only runs while enabled.
// Leaving the flag set across a route change therefore gave the query log an
// empty table labelled "Paused", with no history and no stream — a screen
// that looks broken and whose only cue is a toggle in the row above.
test("pausing the tail does not survive leaving the query log", async () => {
  seedWith([entry({ id: 40, q_name: "history.example" })]);
  const client = makeQueryClient();

  const first = render(<TailHarness />, { wrapper: wrapper(client) });
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

  act(() => setLiveTailPaused(true));
  expect(getLiveTailStatus().paused).toBe(true);

  first.unmount();
  expect(getLiveTailStatus().paused).toBe(false);

  // ...and coming back opens a stream and re-seeds, rather than sitting on an
  // empty table.
  render(<TailHarness />, { wrapper: wrapper(client) });
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
});
