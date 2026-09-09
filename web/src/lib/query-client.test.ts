import { http, HttpResponse } from "msw";
import { expect, test, vi } from "vitest";
import { waitFor } from "@testing-library/react";
import { QueryObserver, type QueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import { authKeys } from "../hooks/use-auth";
import { server } from "../test/msw-server";
import { makeQueryClient } from "./query-client";

/**
 * The app-global 401 handler, driven directly rather than through a screen.
 * It is the only cross-cutting piece of error handling in the client (see
 * web/README.md), and every path through it is a session-recovery decision:
 * whose 401 counts, how many, and what survives it.
 */

const ME = { id: 1, username: "admin", totp_enabled: false };

function unauthorized() {
  return HttpResponse.json({ error: "authentication required" }, { status: 401 });
}

/** `GET /auth/me`, counted, answering whatever `signedIn` currently says. */
function meAnswers(calls: () => void, signedIn: () => boolean) {
  server.use(
    http.get("/api/v1/auth/me", () => {
      calls();
      return signedIn() ? HttpResponse.json(ME) : unauthorized();
    }),
  );
}

/**
 * A signed-in client with a live `me` observer — App's own `useMe()`, which
 * is what makes the handler's `invalidateQueries` actually refetch. Without
 * an observer `me` is inactive and an invalidation only marks it stale, so a
 * test without one would exercise nothing.
 */
async function signedInClient(): Promise<{ client: QueryClient; stop: () => void }> {
  const client = makeQueryClient();
  const observer = new QueryObserver(client, {
    queryKey: authKeys.me,
    queryFn: () => api.get("/auth/me"),
    retry: false,
    staleTime: 0,
  });
  const stop = observer.subscribe(() => {});
  await waitFor(() => expect(client.getQueryState(authKeys.me)?.status).toBe("success"));
  return { client, stop };
}

/** One 401'ing query, run to completion. */
async function fetch401(client: QueryClient, key: readonly unknown[], path: string) {
  await client
    .fetchQuery({ queryKey: key, queryFn: () => api.get(path), retry: false })
    .catch(() => {});
}

test("a burst of 401s revalidates me exactly once", async () => {
  const meCalls = vi.fn<() => void>();
  let signedIn = true;
  meAnswers(meCalls, () => signedIn);
  server.use(http.get("/api/v1/stats/overview", () => unauthorized()));

  const { client, stop } = await signedInClient();
  meCalls.mockClear();
  signedIn = false;

  // One 401 per mounted panel is the real shape of a dead session; all but
  // the first have nothing left to do, and re-entering would mean one
  // /auth/me per panel.
  await Promise.allSettled(
    [1, 2, 3, 4, 5].map((n) => fetch401(client, ["stats", "overview", n], "/stats/overview")),
  );

  await waitFor(() => expect(client.getQueryState(authKeys.me)?.status).toBe("error"));
  expect(meCalls).toHaveBeenCalledTimes(1);
  stop();
});

test("a 401 that turns out to be specific to one request leaves everything else alone", async () => {
  const meCalls = vi.fn<() => void>();
  meAnswers(meCalls, () => true);
  server.use(http.get("/api/v1/tokens", () => unauthorized()));

  const { client, stop } = await signedInClient();
  client.setQueryData(["clients"], [{ id: 1 }]);
  meCalls.mockClear();

  await fetch401(client, ["tokens"], "/tokens");

  await waitFor(() => expect(meCalls).toHaveBeenCalledTimes(1));
  // `me` still succeeds, so the session is fine and nothing is dropped.
  expect(client.getQueryState(authKeys.me)?.status).toBe("success");
  expect(client.getQueryData(["clients"])).toEqual([{ id: 1 }]);
  stop();
});

test("a dead session drops every other cached query, and only those", async () => {
  const meCalls = vi.fn<() => void>();
  let signedIn = true;
  meAnswers(meCalls, () => signedIn);
  server.use(http.get("/api/v1/tokens", () => unauthorized()));

  const { client, stop } = await signedInClient();
  client.setQueryData(["clients"], [{ id: 1 }]);
  client.setQueryData(authKeys.setup, { setup_required: false });
  signedIn = false;

  await fetch401(client, ["tokens"], "/tokens");

  await waitFor(() => expect(client.getQueryData(["clients"])).toBeUndefined());
  // `me` and `setup` drive the gate itself and are not session data.
  expect(client.getQueryState(authKeys.me)).toBeDefined();
  expect(client.getQueryData(authKeys.setup)).toEqual({ setup_required: false });
  stop();
});

test("a mutation's 401 goes through the same recovery as a query's", async () => {
  const meCalls = vi.fn<() => void>();
  let signedIn = true;
  meAnswers(meCalls, () => signedIn);
  server.use(http.post("/api/v1/tokens", () => unauthorized()));

  const { client, stop } = await signedInClient();
  client.setQueryData(["clients"], [{ id: 1 }]);
  meCalls.mockClear();
  signedIn = false;

  await client
    .getMutationCache()
    .build(client, { mutationFn: () => api.post("/tokens", { name: "x", scope: "read" }) })
    .execute(undefined)
    .catch(() => {});

  await waitFor(() => expect(meCalls).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(client.getQueryData(["clients"])).toBeUndefined());
  stop();
});

test("a 500 is not a session problem and is left to the call site", async () => {
  const meCalls = vi.fn<() => void>();
  meAnswers(meCalls, () => true);
  server.use(
    http.get("/api/v1/tokens", () => HttpResponse.json({ error: "boom" }, { status: 500 })),
  );

  const { client, stop } = await signedInClient();
  client.setQueryData(["clients"], [{ id: 1 }]);
  meCalls.mockClear();

  await fetch401(client, ["tokens"], "/tokens");

  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(meCalls).not.toHaveBeenCalled();
  expect(client.getQueryData(["clients"])).toEqual([{ id: 1 }]);
  stop();
});

// A server that is down produces a fetch TypeError, not an ApiError with a
// status — which is exactly why the handler tests `instanceof ApiError`
// rather than reading a status off whatever it was handed.
test("an unreachable server is not treated as a dead session", async () => {
  const meCalls = vi.fn<() => void>();
  meAnswers(meCalls, () => true);
  server.use(http.get("/api/v1/tokens", () => HttpResponse.error()));

  const { client, stop } = await signedInClient();
  client.setQueryData(["clients"], [{ id: 1 }]);
  meCalls.mockClear();

  await fetch401(client, ["tokens"], "/tokens");

  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(meCalls).not.toHaveBeenCalled();
  expect(client.getQueryData(["clients"])).toEqual([{ id: 1 }]);
  stop();
});

test("a 401 before me has ever succeeded is ignored", async () => {
  const meCalls = vi.fn<() => void>();
  meAnswers(meCalls, () => false);
  server.use(http.get("/api/v1/tokens", () => unauthorized()));

  // No `me` in the cache at all: the gate is already showing Login, and
  // there is nothing to revalidate.
  const client = makeQueryClient();
  await fetch401(client, ["tokens"], "/tokens");

  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(meCalls).not.toHaveBeenCalled();
});
