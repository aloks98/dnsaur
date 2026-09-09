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

/**
 * A 401 — the only failure that actually means "this session is not signed
 * in". Every other one (a 5xx, a fetch TypeError because the server is
 * restarting) says nothing about the cookie, and the difference is what the
 * auth gate branches on: see app.tsx.
 */
export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}

// One retry, soon. A server being restarted answers (or refuses) for a
// second or two, which is long enough to lose the race against a page load
// and short enough that waiting once costs nothing.
const ME_RETRY_DELAY_MS = 500;

/** The signed-in user, plus everything the UI derives from them. */
export interface Me extends MeResponse {
  /** Two-letter avatar initials. */
  initials: string;
}

/** Initials shown before `me` has resolved (or once it has failed). */
export const UNKNOWN_INITIALS = "?";

// Module scope, not an inline arrow: query-core re-runs `select` whenever
// its identity changes, so a stable reference keeps this to one call per
// fetch instead of one per render.
function withDerived(me: MeResponse): Me {
  return { ...me, initials: me.username.slice(0, 2).toUpperCase() };
}

export function useMe() {
  return useQuery({
    queryKey: authKeys.me,
    queryFn: () => api.get<MeResponse>("/auth/me"),
    // Avatar initials belong to the account, not to whichever component
    // happens to draw an avatar — derived once here so every call site
    // (sidebar footer today, anything else later) agrees on them.
    select: withDerived,
    // A 401 is an answer and is never retried; anything else is the server
    // failing to answer, and retrying once keeps a restart from reading as
    // a dead session. App's gate tells the two apart the same way.
    retry: (failureCount, error) => failureCount < 1 && !isUnauthorized(error),
    retryDelay: ME_RETRY_DELAY_MS,
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

/** Avatar initials for the signed-in user, or `?` while `me` is unresolved. */
export function useMeInitials(): string {
  return useMe().data?.initials ?? UNKNOWN_INITIALS;
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

/**
 * POST /auth/logout is itself behind requireAuth (internal/api/
 * auth_handlers.go), so an already-dead session answers 401 — which is the
 * state logging out was trying to reach, i.e. success wearing an error's
 * status code. Exported so the call site's error toast can stay silent for
 * the one failure that isn't one, without re-deriving the rule.
 */
export function isAlreadyLoggedOut(error: unknown): boolean {
  return isUnauthorized(error);
}

// A hard reload, not qc.clear()/invalidateQueries(authKeys.me) — this page
// mounts more than one useMe() observer (App's own auth gate, the sidebar
// footer's account menu), and cache-clearing/invalidating the shared `me`
// query only reliably refreshed *some* of them: verified by hand
// (Playwright) that after logout the dashboard kept rendering with every
// one of its own queries silently 401ing in the background, the avatar
// correctly flipping to its logged-out "?" state while App's own gate never
// re-rendered at all — until a full page reload. Logout is rare enough that
// a reload's cost is a non-issue, and it sidesteps that cross-observer
// inconsistency entirely by starting the whole app fresh against the
// now-invalidated session.
function leaveForLoginScreen() {
  window.location.assign("/");
}

export function useLogout() {
  return useMutation({
    mutationFn: () => api.post("/auth/logout"),
    onSuccess: leaveForLoginScreen,
    // A 401 means the session was already gone, so the logout has in fact
    // happened — finish the same way rather than leaving the user stuck on
    // a shell full of 401ing panels with a "Couldn't sign out — try again"
    // toast and no way back to the login screen. Anything else (server
    // down, 500) is a real failure and falls through to the call site,
    // which owns the messaging.
    onError: (error) => {
      if (isAlreadyLoggedOut(error)) leaveForLoginScreen();
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
