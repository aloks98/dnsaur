import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { LocalRecord } from "../api/types";

// Local DNS records CRUD (Task 11) — dnsaur answers these directly instead
// of forwarding upstream. Follows the same shape as use-clients.ts's
// client hooks: one query key, invalidated wholesale by every mutation
// (the records list is never large enough to warrant per-row cache
// surgery).
export const recordKeys = {
  all: ["records"] as const,
};

interface RecordInput {
  name: string;
  type: LocalRecord["type"];
  value: string;
  ttl: number;
}

export function useRecords() {
  return useQuery({
    queryKey: recordKeys.all,
    queryFn: () => api.get<LocalRecord[]>("/records"),
  });
}

export function useAddRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: RecordInput) => api.post<{ id: number }>("/records", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: recordKeys.all }),
  });
}

export function useUpdateRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...v }: RecordInput & { id: number }) => api.put<void>(`/records/${id}`, v),
    onSuccess: () => qc.invalidateQueries({ queryKey: recordKeys.all }),
  });
}

export function useDeleteRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/records/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: recordKeys.all }),
  });
}
