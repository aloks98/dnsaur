import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import { authKeys } from "./use-auth";

// TOTP enrollment (Task 13) — a two-call dance, both against
// internal/api/tokens_handlers.go's TOTP handlers: start() mints a secret
// that is NOT persisted server-side until confirm() proves the caller can
// derive a valid code from it. Nothing here ever caches the secret in
// react-query — useTotpStart's result lives only in whichever component
// state called it (see pages/account.tsx's TotpEnableDialog), for exactly
// as long as the enable flow is open.

interface TotpStartResult {
  secret: string;
  otpauth_url: string;
}

export function useTotpStart() {
  return useMutation({
    mutationFn: () => api.post<TotpStartResult>("/auth/totp/start"),
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
  });
}

export function useTotpDisable() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { code: string }) => api.post<void>("/auth/totp/disable", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.me }),
  });
}
