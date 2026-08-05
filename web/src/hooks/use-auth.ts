import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import type { MeResponse, SetupState } from "../api/types";

export const authKeys = {
  me: ["auth", "me"] as const,
  setup: ["setup"] as const,
};

/** `me`/`setup` drive the auth gate itself — everything else is session data. */
export function isSessionData(queryKey: readonly unknown[]): boolean {
  return queryKey[0] !== authKeys.me[0] && queryKey[0] !== authKeys.setup[0];
}

export function useMe() {
  return useQuery({
    queryKey: authKeys.me,
    queryFn: () => api.get<MeResponse>("/auth/me"),
    retry: false,
    // The one query that opts back into focus revalidation (the client-wide
    // default is off): coming back to a tab that has been open for days is
    // exactly when the session is most likely to have expired, and the auth
    // gate should notice before the user starts clicking. A 401 from any
    // other query revalidates this one too — see lib/query-client.ts.
    //
    // Only ever revalidate a session that actually exists. query-core sets
    // status back to "pending" on any refetch where data is undefined, which
    // is precisely the unauthenticated state — so an unconditional `true`
    // makes a plain tab-switch flip App's gate to its full-page spinner and
    // unmount whatever is on screen. On Login that discards the typed
    // credentials and the TOTP step; mid-setup it is worse, because step 1
    // has already established a session, so the refetch *succeeds* and swaps
    // the wizard for the dashboard with no blocklists or upstreams applied.
    // Scoped this way there is no flicker at all: once `me` has succeeded,
    // data is defined and the refetch stays "success" throughout.
    refetchOnWindowFocus: (query) => query.state.status === "success",
  });
}

export function useSetupState() {
  return useQuery({
    queryKey: authKeys.setup,
    queryFn: () => api.get<SetupState>("/setup"),
    retry: false,
  });
}

export function useLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { username: string; password: string; totp_code?: string }) =>
      api.post("/auth/login", v),
    // Drop the previous session's cached data before revalidating `me`.
    // lib/query-client.ts does this too, but only on the path where some
    // *other* query's 401 is what noticed the session died — when `me`'s own
    // focus refetch is the first to notice, that path never fires, and the
    // next sign-in would briefly render the last session's stats/clients/
    // tokens from cache (gcTime 5m) before revalidating.
    onSuccess: () => {
      qc.removeQueries({ predicate: (query) => isSessionData(query.queryKey) });
      return qc.invalidateQueries({ queryKey: authKeys.me });
    },
  });
}

export function useLogout() {
  return useMutation({
    // POST /auth/logout is itself behind requireAuth (internal/api/
    // auth_handlers.go), so an already-dead session answers 401 — which is
    // the state logging out was trying to reach. Treating that as failure
    // left the user stuck on a shell full of 401ing panels with a "Couldn't
    // sign out — try again" toast and no way back to the login screen.
    mutationFn: async () => {
      try {
        await api.post("/auth/logout");
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) return;
        throw err;
      }
    },
    // A hard reload, not qc.clear()/invalidateQueries(authKeys.me) — this
    // page mounts more than one useMe() observer (App's own auth gate,
    // Header's account menu), and cache-clearing/invalidating the shared
    // `me` query only reliably refreshed *some* of them: verified by hand
    // (Playwright) that after logout the dashboard kept rendering with
    // every one of its own queries silently 401ing in the background,
    // Header's avatar correctly flipping to its logged-out "?" state while
    // App's own gate never re-rendered at all — until a full page reload.
    // Logout is rare enough that a reload's cost is a non-issue, and it
    // sidesteps that cross-observer inconsistency entirely by starting the
    // whole app fresh against the now-invalidated session.
    onSuccess: () => {
      window.location.assign("/");
    },
  });
}

export function useSetup() {
  // Deliberately no onSuccess invalidation: POST /setup only creates the
  // admin account, it does not establish a session. The setup wizard stays
  // mounted through its optional starter-lists step afterwards, and would
  // be yanked out from under the user if invalidating `setup` here caused
  // the app's auth gate to swap Setup for Login mid-flow. The wizard
  // invalidates `me`/`setup` itself, once it's actually done.
  return useMutation({
    mutationFn: (v: { username: string; password: string }) => api.post("/setup", v),
  });
}

// Used internally by the setup wizard to obtain a session right after the
// admin account is created, without invalidating `me` (which would flip the
// app's auth gate to the authenticated routes before the wizard's optional
// starter-lists step gets a chance to run). Distinct from useLogin, which
// is meant to trigger that gate flip immediately — that's the whole point
// of the real login page.
export function useSetupSignIn() {
  return useMutation({
    mutationFn: (v: { username: string; password: string }) => api.post("/auth/login", v),
  });
}
