import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { MeResponse, SetupState } from "../api/types";

export const authKeys = {
  me: ["auth", "me"] as const,
  setup: ["setup"] as const,
};

export function useMe() {
  return useQuery({
    queryKey: authKeys.me,
    queryFn: () => api.get<MeResponse>("/auth/me"),
    retry: false,
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
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.me }),
  });
}

export function useLogout() {
  return useMutation({
    mutationFn: () => api.post("/auth/logout"),
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
