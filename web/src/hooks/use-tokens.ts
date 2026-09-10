import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { ApiToken } from "../api/types";

// API tokens (Task 13) — scoped bearer credentials scripts/tools can use to
// call the dnsaur API instead of the session cookie (see
// internal/api/tokens_handlers.go). GET /tokens never includes the hash —
// store.AuthToken.TokenHash is `json:"-"` server-side, so ApiToken (api/
// types.ts) has no field for it at all. One query key, invalidated
// wholesale by every mutation, the same shape as use-clients.ts: the token
// list is never large enough to warrant per-row cache surgery.
export const tokenKeys = {
  all: ["tokens"] as const,
};

interface CreateTokenInput {
  name: string;
  scope: "read" | "write";
  /** Unix ms the token stops working. Omitted means never, which is the
   * server's default too — a token a script depends on must not acquire an
   * expiry nobody asked for. A stamp in the past is a 400. */
  expires_at?: number;
}

// The plaintext `token` exists only in this one response body (see
// handleTokenCreate) — nothing here persists it anywhere beyond the
// mutation's own transient result. The caller (pages/account.tsx) is
// responsible for holding it in component state just long enough for the
// one-time reveal dialog, then letting it go.
interface CreateTokenResult {
  id: number;
  token: string;
}

export function useTokens() {
  return useQuery({
    queryKey: tokenKeys.all,
    queryFn: () => api.get<ApiToken[]>("/tokens"),
  });
}

export function useCreateToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: CreateTokenInput) => api.post<CreateTokenResult>("/tokens", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: tokenKeys.all }),
    // Drop the result the moment the dialog that shows it goes away, rather
    // than leaving the plaintext token readable through the mutation cache
    // for the default five minutes — the same treatment use-totp.ts gives
    // the TOTP secret, and for the same reason.
    gcTime: 0,
  });
}

export function useRevokeToken() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/tokens/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: tokenKeys.all }),
  });
}
