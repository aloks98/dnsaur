import { http, HttpResponse } from "msw";
import { afterEach, expect, test, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import type { List } from "../api/types";
import { makeQueryClient } from "../lib/query-client";
import { server } from "../test/msw-server";
import { filterKeys, useRefreshFilters } from "./use-filters";

function wrapper(client: QueryClient) {
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

afterEach(() => vi.useRealTimers());

/**
 * `POST /filters/refresh` answers 202 and the download happens afterwards,
 * so the mutation resolving says nothing about the table. Nothing else
 * invalidates it either — useLists has no refetchInterval and the client
 * turns focus refetching off — so this ladder is the only thing that makes
 * "refreshed 3d ago" stop saying that after a successful manual refresh.
 */
function listHandlers(refreshes: () => void = () => {}) {
  let lastRefreshed = 0;
  const lists = (): List[] => [
    {
      id: 1,
      name: "StevenBlack",
      url: "https://example.test/hosts",
      kind: "block",
      enabled: true,
      entry_count: 100,
      last_refreshed: lastRefreshed,
      last_status: "ok",
      last_error: "",
      last_attempt: lastRefreshed,
    },
  ];
  return [
    http.get("/api/v1/filters/lists", () => HttpResponse.json(lists())),
    http.post("/api/v1/filters/refresh", () => {
      refreshes();
      lastRefreshed = 1_700_000_000_000;
      return HttpResponse.json({ status: "accepted" }, { status: 202 });
    }),
  ];
}

test("a successful refresh re-reads the list table on the 1s / 4s / 12s ladder", async () => {
  server.use(...listHandlers());
  const client = makeQueryClient();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  // shouldAdvanceTime keeps msw's own async work moving; the ladder has to be
  // scheduled on the fake clock, so the clock goes in before the mutation.
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const { result } = renderHook(() => useRefreshFilters(), { wrapper: wrapper(client) });

  await act(async () => {
    await result.current.mutateAsync();
  });

  const listInvalidations = () =>
    invalidate.mock.calls.filter(
      (call) => JSON.stringify(call[0]?.queryKey) === JSON.stringify(filterKeys.lists),
    ).length;

  // Nothing fires on the 202 itself — there is nothing new to read yet.
  expect(listInvalidations()).toBe(0);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(1_000);
  });
  expect(listInvalidations()).toBe(1);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(3_000);
  });
  expect(listInvalidations()).toBe(2);

  await act(async () => {
    await vi.advanceTimersByTimeAsync(8_000);
  });
  expect(listInvalidations()).toBe(3);

  // ...and that is the whole ladder.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(listInvalidations()).toBe(3);
});

test("unmounting cancels a pending nudge instead of letting it outlive the page", async () => {
  server.use(...listHandlers());
  const client = makeQueryClient();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const { result, unmount } = renderHook(() => useRefreshFilters(), { wrapper: wrapper(client) });

  await act(async () => {
    await result.current.mutateAsync();
  });

  unmount();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });

  expect(invalidate).not.toHaveBeenCalled();
});

test("a failed refresh schedules no re-reads at all", async () => {
  const refreshes = vi.fn<() => void>();
  server.use(
    http.post("/api/v1/filters/refresh", () =>
      HttpResponse.json({ error: "boom" }, { status: 500 }),
    ),
    ...listHandlers(refreshes),
  );
  const client = makeQueryClient();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const { result } = renderHook(() => useRefreshFilters(), { wrapper: wrapper(client) });

  await act(async () => {
    await result.current.mutateAsync().catch(() => {});
  });
  await waitFor(() => expect(result.current.isError).toBe(true));

  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(invalidate).not.toHaveBeenCalled();
  expect(refreshes).not.toHaveBeenCalled();
});
