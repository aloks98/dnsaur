import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { Settings } from "../api/types";

// Canonical settings-domain hooks — GET /settings (Settings page, Task 12)
// and PUT /settings (both the setup wizard's starter-upstreams write and
// the Settings page's per-field saves).
export const settingsKeys = {
  all: ["settings"] as const,
};

// The server strips instance.* and stats.* internal keys before returning
// (see internal/api/settings_handlers.go's handleSettingsGet), so this is
// exactly the editable-settings surface plus nothing sensitive.
export function useSettings() {
  return useQuery({
    queryKey: settingsKeys.all,
    queryFn: () => api.get<Settings>("/settings"),
  });
}

// Settings hot-reload live server-side (except cache.* and
// lists.refresh_hours, which need a restart — see docs/configuration.md),
// so there's nothing else client-side to invalidate beyond the settings
// query itself.
export function useUpdateSetting() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { key: string; value: string }) => api.put<void>("/settings", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: settingsKeys.all }),
  });
}
