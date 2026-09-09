import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { QueryEntry } from "./types";
import { subscribeQueries, type SseState } from "./sse";
import { FakeEventSource } from "../test/fake-event-source";

const sampleEntry: QueryEntry = {
  id: 1,
  at: 1_700_000_000_000,
  instance_id: "i1",
  client_ip: "10.0.0.5",
  client_id: 1,
  q_name: "example.com",
  q_type: "A",
  decision: "blocked",
  rule_id: 2,
  list_id: 0,
  upstream: "",
  r_code: "NOERROR",
  duration_ms: 4,
};

beforeEach(() => {
  vi.useFakeTimers();
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

test("opens a connection to the tail endpoint and reports open state", () => {
  const onState = vi.fn<(state: SseState) => void>();
  const unsubscribe = subscribeQueries(() => {}, onState);

  expect(FakeEventSource.instances).toHaveLength(1);
  expect(FakeEventSource.instances[0]?.url).toBe("/api/v1/queries/tail");

  FakeEventSource.instances[0]?.emitOpen();
  expect(onState).toHaveBeenCalledWith("open");

  unsubscribe();
});

test("parses each message event into a QueryEntry and forwards it", () => {
  const onEntry = vi.fn<(entry: QueryEntry) => void>();
  const unsubscribe = subscribeQueries(onEntry, () => {});

  const source = FakeEventSource.instances[0];
  source?.emitOpen();
  source?.emit(sampleEntry);

  expect(onEntry).toHaveBeenCalledWith(sampleEntry);
  unsubscribe();
});

test("a malformed message payload is dropped, not thrown", () => {
  const onEntry = vi.fn<(entry: QueryEntry) => void>();
  const unsubscribe = subscribeQueries(onEntry, () => {});
  const source = FakeEventSource.instances[0];
  source?.emitOpen();

  expect(() => source?.emitRaw("not json")).not.toThrow();
  expect(onEntry).not.toHaveBeenCalled();

  unsubscribe();
});

test("reconnects with capped backoff after an error", () => {
  const onState = vi.fn<(state: SseState) => void>();
  const unsubscribe = subscribeQueries(() => {}, onState);

  const first = FakeEventSource.instances[0];
  first?.emitOpen();
  first?.emitError();

  expect(onState).toHaveBeenCalledWith("reconnecting");
  expect(first?.closed).toBe(true);
  expect(FakeEventSource.instances).toHaveLength(1); // waiting on backoff, not reconnected yet

  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(2); // reconnect attempted

  // A second consecutive failure should back off further (capped), not
  // reset to the initial delay.
  const second = FakeEventSource.instances[1];
  second?.emitError();
  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(2); // not yet — backoff doubled to 2s

  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(3);

  unsubscribe();
});

// A dead session answers the tail endpoint with a 401, which EventSource
// reports as an ordinary error — so the stream opens and immediately fails,
// over and over. Resetting the backoff on `onopen` alone made that loop
// reconnect at a flat 1s forever and never reach the terminal state.
test("a stream that opens and then fails at once keeps backing off", () => {
  const onState = vi.fn<(state: SseState) => void>();
  const unsubscribe = subscribeQueries(() => {}, onState);

  FakeEventSource.instances[0]?.emitOpen();
  FakeEventSource.instances[0]?.emitError();
  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(2);

  // Second attempt: opens, then dies before it has delivered anything or
  // stayed up long enough to count as healthy.
  FakeEventSource.instances[1]?.emitOpen();
  FakeEventSource.instances[1]?.emitError();
  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(2); // 2s now, not 1s again

  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(3);

  unsubscribe();
});

test("giving up after six consecutive failures reports failed and schedules nothing further", () => {
  const onState = vi.fn<(state: SseState) => void>();
  const unsubscribe = subscribeQueries(() => {}, onState);

  // 1s, 2s, 4s, 8s, 16s — five reconnects, and the sixth failure is the one
  // that gives up. MAX_CONSECUTIVE_FAILURES is 6.
  for (const backoff of [1_000, 2_000, 4_000, 8_000, 16_000]) {
    FakeEventSource.instances.at(-1)?.emitError();
    expect(onState).toHaveBeenLastCalledWith("reconnecting");
    vi.advanceTimersByTime(backoff);
  }
  expect(FakeEventSource.instances).toHaveLength(6);

  FakeEventSource.instances.at(-1)?.emitError();
  expect(onState).toHaveBeenLastCalledWith("failed");

  vi.advanceTimersByTime(120_000);
  expect(FakeEventSource.instances).toHaveLength(6);

  unsubscribe();
});

test("a delivered message counts the stream as healthy and resets the backoff", () => {
  const unsubscribe = subscribeQueries(() => {}, vi.fn<(state: SseState) => void>());

  FakeEventSource.instances[0]?.emitError();
  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(2);

  FakeEventSource.instances[1]?.emitOpen();
  FakeEventSource.instances[1]?.emit(sampleEntry);
  FakeEventSource.instances[1]?.emitError();

  // Back to the initial delay, because this stream actually worked.
  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(3);

  unsubscribe();
});

test("a stream that stays open counts as healthy even if it never delivers a row", () => {
  const unsubscribe = subscribeQueries(() => {}, vi.fn<(state: SseState) => void>());

  FakeEventSource.instances[0]?.emitError();
  vi.advanceTimersByTime(1_000);

  // A quiet resolver publishes nothing for minutes at a time; an open
  // connection is the only evidence available, so it counts once it holds.
  FakeEventSource.instances[1]?.emitOpen();
  vi.advanceTimersByTime(5_000);
  FakeEventSource.instances[1]?.emitError();

  vi.advanceTimersByTime(1_000);
  expect(FakeEventSource.instances).toHaveLength(3);

  unsubscribe();
});

test("unsubscribing before a scheduled reconnect fires cancels it", () => {
  const onState = vi.fn<(state: SseState) => void>();
  const unsubscribe = subscribeQueries(() => {}, onState);

  const first = FakeEventSource.instances[0];
  first?.emitOpen();
  first?.emitError();
  unsubscribe();

  expect(onState).toHaveBeenCalledWith("closed");

  vi.advanceTimersByTime(30_000);
  expect(FakeEventSource.instances).toHaveLength(1); // never reconnected after unsubscribe
});

test("reports closed immediately when EventSource is unavailable in this environment", () => {
  vi.stubGlobal("EventSource", undefined);
  const onState = vi.fn<(state: SseState) => void>();
  const unsubscribe = subscribeQueries(() => {}, onState);

  expect(onState).toHaveBeenCalledWith("closed");
  expect(() => unsubscribe()).not.toThrow();
});
