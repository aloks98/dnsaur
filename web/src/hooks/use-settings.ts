import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { ResolverStatus, Settings } from "../api/types";

// Canonical settings-domain hooks — GET /settings (Settings page, Task 12)
// and PUT /settings (both the setup wizard's starter-upstreams write and
// the Settings page's per-field saves).
export const settingsKeys = {
  all: ["settings"] as const,
  resolverStatus: ["resolver", "status"] as const,
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
// GET /resolver/status — server state the settings screen has to show and
// that is not a setting: whether the running forwarder is the plaintext
// fallback installed because an encrypted `upstreams` value would not parse
// (see internal/app/app.go's applySettings ladder). One round trip, made
// once alongside the settings themselves.
//
// Polled only while something is wrong. Saving a corrected value invalidates
// this query, but the server applies settings asynchronously — the watcher
// goroutine reacts to the write — so the refetch that immediately follows a
// save can still catch the old state. Five seconds of polling while the
// warning is up closes that window; when nothing is wrong there is nothing
// to poll for.
export function useResolverStatus() {
  return useQuery({
    queryKey: settingsKeys.resolverStatus,
    queryFn: () => api.get<ResolverStatus>("/resolver/status"),
    refetchInterval: (query) => (query.state.data?.encryption_downgraded ? 5_000 : false),
  });
}

export function useUpdateSetting() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { key: string; value: string }) => api.put<void>("/settings", v),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: settingsKeys.all });
      // A saved `upstreams` may have just ended (or begun) a downgrade.
      void qc.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
    },
  });
}
