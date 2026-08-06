import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { ApiError } from "../api/client";
import { authKeys, isSessionData } from "../hooks/use-auth";

function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}

/**
 * Global recovery from a session that dies *mid-session* — logged out in
 * another tab, token revoked, server restarted against a fresh DB.
 *
 * Without this, `me` is fetched once per page load and never revalidated
 * (retry: false, no refetchInterval), so it stays `isSuccess` forever while
 * every panel below the auth gate quietly 401s: the app looks signed in and
 * broken at the same time, and only a manual reload escapes.
 *
 * So: any 401 from any query or mutation revalidates `me` once. If `me` still
 * succeeds the 401 was specific to that one request and nothing else happens;
 * if it now fails, App's gate flips to Login on its own (no
 * window.location.assign — a reload would lose in-flight UI state and can't be
 * tested) and every other cached query is dropped so none of the previous
 * session's data survives into whoever signs in next.
 *
 * Deliberately does NOT re-enter while a revalidation is in flight, nor once
 * `me` is already pending/errored: a dead session produces a burst of 401s
 * (one per mounted panel), and all but the first have nothing left to do.
 */
function makeUnauthorizedHandler() {
  let client: QueryClient | null = null;
  let revalidating = false;

  function onError(error: unknown): void {
    const qc = client;
    if (qc === null || revalidating || !isUnauthorized(error)) return;
    if (qc.getQueryState(authKeys.me)?.status !== "success") return;

    revalidating = true;
    void qc.invalidateQueries({ queryKey: authKeys.me, exact: true }).finally(() => {
      revalidating = false;
      if (qc.getQueryState(authKeys.me)?.status !== "error") return;
      qc.removeQueries({ predicate: (query) => isSessionData(query.queryKey) });
    });
  }

  return {
    onError,
    attach(next: QueryClient) {
      client = next;
    },
  };
}

export function makeQueryClient(): QueryClient {
  const unauthorized = makeUnauthorizedHandler();
  const client = new QueryClient({
    queryCache: new QueryCache({ onError: unauthorized.onError }),
    mutationCache: new MutationCache({ onError: unauthorized.onError }),
    defaultOptions: {
      queries: {
        retry: 1,
        staleTime: 10_000,
        refetchOnWindowFocus: false,
      },
      mutations: { retry: false },
    },
  });
  unauthorized.attach(client);
  return client;
}
