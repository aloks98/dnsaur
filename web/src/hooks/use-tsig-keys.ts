import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { TSIGKey } from "../api/types";

// TSIG keys (Milestone D1) — the shared secrets that authenticate zone
// transfers (see internal/api/tsigkeys_handlers.go). One query key,
// invalidated wholesale by every mutation, the same shape as use-tokens.ts:
// an instance has a handful of keys, never enough to warrant per-row cache
// surgery.
//
// No optimistic updates and no cache patching: the server canonicalises the
// name on every write (lowercase, trailing dot), so what a write returns to
// the list is not what was sent to it — a patched cache would show the typed
// name until the next refetch and then change it under the reader.
export const tsigKeyKeys = {
  all: ["tsigKeys"] as const,
};

/**
 * The create and replace body, which are the same shape: a TSIG key has no
 * optional or server-generated field apart from id and created_at, so PUT is
 * a full replace and needs all three (see tsigKeyWrite server-side).
 */
export interface TSIGKeyWrite {
  /** Sent as typed — the server canonicalises it. */
  name: string;
  /** The wire value, trailing dot included (see lib/tsig.ts). */
  algorithm: string;
  /** base64. */
  secret: string;
}

export function useTSIGKeys() {
  return useQuery({
    queryKey: tsigKeyKeys.all,
    queryFn: () => api.get<TSIGKey[]>("/tsig-keys"),
  });
}

export function useCreateTSIGKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: TSIGKeyWrite) => api.post<{ id: number }>("/tsig-keys", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: tsigKeyKeys.all }),
  });
}

export function useUpdateTSIGKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...v }: TSIGKeyWrite & { id: number }) =>
      api.put<void>(`/tsig-keys/${id}`, v),
    onSuccess: () => qc.invalidateQueries({ queryKey: tsigKeyKeys.all }),
  });
}

export function useDeleteTSIGKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/tsig-keys/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: tsigKeyKeys.all }),
  });
}
