import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

// GET /settings (and other settings-domain reads) land here once the
// Settings page task needs them; the setup wizard only ever writes.
export const settingsKeys = {
  all: ["settings"] as const,
};

export function useUpdateSetting() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { key: string; value: string }) => api.put<void>("/settings", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: settingsKeys.all }),
  });
}
