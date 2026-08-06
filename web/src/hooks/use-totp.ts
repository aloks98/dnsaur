import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import { authKeys } from "./use-auth";

// TOTP enrollment (Task 13) — a two-call dance, both against
// internal/api/tokens_handlers.go's TOTP handlers: start() mints a secret
// that is NOT persisted server-side until confirm() proves the caller can
// derive a valid code from it.
//
// The shared secret reaches react-query twice, so both paths are pinned to
// gcTime 0: useTotpStart holds it in `data`, and useTotpConfirm holds it in
// `variables` (query-core carries variables through the success action
// untouched). pages/account.tsx reset()s these when the enable dialog
// closes, which drops the observer; gcTime 0 then evicts the Mutation
// object immediately instead of leaving the secret readable via
// queryClient.getMutationCache().getAll() until the default 5-minute GC
// timer fires. Same treatment for useTotpDisable's single-use code.

interface TotpStartResult {
  secret: string;
  otpauth_url: string;
}

export function useTotpStart() {
  return useMutation({
    mutationFn: () => api.post<TotpStartResult>("/auth/totp/start"),
    gcTime: 0,
  });
}

// Confirms enrollment and flips the account into 2FA-enabled — `me`
// (totp_enabled) is the only client state that reflects this, so that's
// what gets invalidated.
export function useTotpConfirm() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { secret: string; code: string }) => api.post<void>("/auth/totp/confirm", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.me }),
    gcTime: 0,
  });
}

export function useTotpDisable() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { code: string }) => api.post<void>("/auth/totp/disable", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.me }),
    gcTime: 0,
  });
}
