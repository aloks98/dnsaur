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
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { username: string; password: string }) => api.post("/setup", v),
    onSuccess: () => qc.invalidateQueries(),
  });
}
