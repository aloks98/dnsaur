import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type { BackupResult, ResolverStatus, Settings } from "../api/types";
import { somethingIsWrong } from "../lib/serving";
import { syncKeys } from "./use-sync";

// Canonical settings-domain hooks — GET /settings (Settings page, Task 12)
// and PUT /settings (both the setup wizard's starter-upstreams write and
// the Settings page's per-field saves).
export const settingsKeys = {
  all: ["settings"] as const,
  resolverStatus: ["resolver", "status"] as const,
  /** Not a query: a timestamp useUpdateSetting writes and
   * useResolverStatus reads, so a `serve.*` save can turn the status poll
   * on for a few seconds. It lives in the cache rather than in a module
   * variable because the cache is per-provider — one settle window per
   * rendered app, not one shared by every test in a file. Deliberately not
   * under the `resolverStatus` key: that key is invalidated on every save,
   * and a prefix match would sweep an entry that has no queryFn to refetch
   * with. */
  serveSettleUntil: ["serve-settle-until"] as const,
};

/** How long after a `serve.*` write the status is polled regardless of what
 * it currently says, and how often during that window.
 *
 * The reconcile that makes a serve.* write real runs asynchronously off the
 * settings watcher (internal/app/serve.go, driven from app.go), so the
 * single refetch useUpdateSetting triggers routinely lands before it. Five
 * seconds of one-second polling is the difference between a checkbox whose
 * reality line updates and one that reads "off" under a ticked box until
 * the operator navigates away and back. */
const SERVE_SETTLE_MS = 5_000;
const SERVE_SETTLE_POLL_MS = 1_000;

/** How often the status is polled while something is actually wrong. */
const TROUBLE_POLL_MS = 5_000;

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
// (see internal/app/app.go's applySettings ladder), what the two encrypted
// listeners are actually doing, and the certificate's expiry. One round
// trip, made once alongside the settings themselves.
//
// Polled in two situations, and otherwise not at all.
//
// While something is wrong — `somethingIsWrong`, shared with the shell
// banners so the two cannot drift. This is what clears a bind-failure
// banner on its own once the operator stops whatever was holding the port:
// the server retries the bind (internal/app/serve.go's runServingRetry) and
// this notices. The predicate used to be `encryption_downgraded` alone,
// which was every fact the endpoint carried when it was written and is now
// one of three.
//
// And briefly after any `serve.*` save, whatever the status currently says
// — because right then it says nothing is wrong, and it is about to stop
// being true. See SERVE_SETTLE_MS.
export function useResolverStatus() {
  const qc = useQueryClient();
  return useQuery({
    queryKey: settingsKeys.resolverStatus,
    queryFn: () => api.get<ResolverStatus>("/resolver/status"),
    refetchInterval: (query) => {
      const settleUntil = qc.getQueryData<number>(settingsKeys.serveSettleUntil) ?? 0;
      if (Date.now() < settleUntil) return SERVE_SETTLE_POLL_MS;
      return somethingIsWrong(query.state.data) ? TROUBLE_POLL_MS : false;
    },
  });
}

// POST /backup — writes a copy of the database on the server and answers
// with the file it wrote. Nothing to invalidate: it creates a file, not a
// row, and no query reads the backups directory.
export function useBackup() {
  return useMutation({ mutationFn: () => api.post<BackupResult>("/backup") });
}

export function useUpdateSetting() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { key: string; value: string }) => api.put<void>("/settings", v),
    onSuccess: (_data, variables) => {
      void qc.invalidateQueries({ queryKey: settingsKeys.all });
      // A saved `serve.*` key opens the settle window before the refetch
      // below, so the refetch that lands first already knows to keep
      // asking — the reconcile it is racing has not necessarily run yet.
      if (variables.key.startsWith("serve.")) {
        qc.setQueryData(settingsKeys.serveSettleUntil, Date.now() + SERVE_SETTLE_MS);
      }
      // A saved `upstreams` may have just ended (or begun) a downgrade.
      void qc.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
      // A saved `sync.*` key moves what GET /sync/status answers, and that
      // is not a setting, so the invalidation above does not reach it. The
      // Sync band renders it directly beside the boxes that were just
      // saved, and at its own 30s cadence it spent up to half a minute
      // contradicting them. Unconditional rather than keyed on a `sync.`
      // prefix: one cheap read after any save beats a prefix test that has
      // to be remembered when a key moves.
      void qc.invalidateQueries({ queryKey: syncKeys.status });
    },
  });
}
