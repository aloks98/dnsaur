import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { Client } from "../api/types";

export const clientKeys = {
  all: ["clients"] as const,
};

interface ClientInput {
  name: string;
  matcher: string;
  group_id: number;
}

export function useClients() {
  return useQuery({
    queryKey: clientKeys.all,
    queryFn: () => api.get<Client[]>("/clients"),
  });
}

export function useAddClient() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: ClientInput) => api.post<{ id: number }>("/clients", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: clientKeys.all }),
  });
}

export function useUpdateClient() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...v }: ClientInput & { id: number }) => api.put<void>(`/clients/${id}`, v),
    onSuccess: () => qc.invalidateQueries({ queryKey: clientKeys.all }),
  });
}

export function useDeleteClient() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/clients/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: clientKeys.all }),
  });
}
