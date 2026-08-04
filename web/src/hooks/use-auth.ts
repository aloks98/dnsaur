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
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post("/auth/logout"),
    onSuccess: () => qc.clear(),
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
