import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import type {
  DHCPClass,
  DHCPLease,
  DHCPPool,
  DHCPReservation,
  DHCPScope,
  DHCPStatus,
  ResolverStatus,
} from "../api/types";
import { settingsKeys, useResolverStatus, useSettings } from "./use-settings";

// DHCP (spec §8.3/§8.4) — scopes and reservations are synced configuration,
// leases are the engine's own table, and the status object is what every
// other screen reads to know whether any of it exists. See docs/api.md's
// DHCP entry for the routes and internal/api/dhcp_handlers.go for the
// shapes.

export const dhcpKeys = {
  status: ["dhcp", "status"] as const,
  scopes: ["dhcp", "scopes"] as const,
  classes: ["dhcp", "classes"] as const,
  reservations: ["dhcp", "reservations"] as const,
  leases: ["dhcp", "leases"] as const,
};

/**
 * The poll both live reads run at, from `dhcp.lease_poll_seconds`.
 *
 * The server rebuilds its lease table on exactly this interval and reads
 * `status-get` on the same tick (spec §7), so asking more often than the
 * setting says would only re-read numbers that cannot have moved. The
 * default mirrors internal/app/app.go's seeded value; the floor mirrors the
 * setting's own validator (`atLeast(2)` in internal/api/settings_handlers.go),
 * so a row someone emptied or set to nonsense cannot turn this into a busy
 * loop.
 */
const DEFAULT_POLL_SECONDS = 10;
const MIN_POLL_SECONDS = 2;

export function useLeasePollMs(enabled = true): number {
  const settings = useSettings({ enabled });
  const raw = Number(settings.data?.["dhcp.lease_poll_seconds"]);
  const seconds = Number.isFinite(raw) && raw > 0 ? raw : DEFAULT_POLL_SECONDS;
  return Math.max(seconds, MIN_POLL_SECONDS) * 1000;
}

/**
 * GET /dhcp/status — the engine's state, polled on the lease cadence.
 *
 * Flat polling rather than useResolverStatus' settle window, and for a
 * reason that is in the API: **every DHCP write renders synchronously** and
 * answers whatever the engine said (docs/api.md), so there is no reconcile
 * landing a second later for a settle window to catch. What does move on
 * its own is the engine's side — a partner that comes back, a table that
 * ages — and that moves on this poll, not on the operator's.
 */
export function useDHCPStatus() {
  const pollMs = useLeasePollMs();
  return useQuery({
    queryKey: dhcpKeys.status,
    queryFn: () => api.get<DHCPStatus>("/dhcp/status"),
    refetchInterval: pollMs,
  });
}

/**
 * Whether this box has an engine at all, off the status the shell already
 * holds — so the nav and the command palette can drop the whole DHCP
 * section without a request of their own.
 *
 * An unanswered status reads as "off", which is the answer that changes
 * nothing: a section that flickered in on every slow poll would be worse
 * than one that appears a beat late.
 */
export function useDHCPEnabled(): boolean {
  return useResolverStatus().data?.dhcp?.enabled ?? false;
}

export function useScopes() {
  return useQuery({
    queryKey: dhcpKeys.scopes,
    queryFn: () => api.get<DHCPScope[]>("/dhcp/scopes"),
  });
}

export function useClasses() {
  return useQuery({
    queryKey: dhcpKeys.classes,
    queryFn: () => api.get<DHCPClass[]>("/dhcp/classes"),
  });
}

export function useReservations() {
  return useQuery({
    queryKey: dhcpKeys.reservations,
    queryFn: () => api.get<DHCPReservation[]>("/dhcp/reservations"),
  });
}

/**
 * GET /dhcp/leases — the table as of the last poll, on the same cadence the
 * server refreshes it.
 */
export function useLeases() {
  const pollMs = useLeasePollMs();
  return useQuery({
    queryKey: dhcpKeys.leases,
    queryFn: () => api.get<DHCPLease[]>("/dhcp/leases"),
    refetchInterval: pollMs,
  });
}

/**
 * The lease table as a lookup, for the screens that are not about DHCP: an
 * address (or a `mac:` matcher's address) to the name the device gave
 * itself.
 *
 * A read-time join and nothing more — spec §8.2 — so a client row shows
 * whoever holds that address *now*, and shows nothing once the lease
 * lapses. Both spellings are in the one map because a client matcher is one
 * or the other and the caller should not have to know which.
 *
 * Gated on the engine existing, unlike `useLeases`: most instances have no
 * DHCP at all, and every DHCP route but the status answers 404 there. It
 * shares `useLeases`' cache entry, so a dashboard that has both open makes
 * one request.
 */
export function useLeaseHostnames(): Map<string, string> {
  const enabled = useDHCPEnabled();
  // The interval too: reading it is a `GET /settings`, and a screen that is
  // not about DHCP has no business making one on a box that has none.
  const pollMs = useLeasePollMs(enabled);
  const { data } = useQuery({
    queryKey: dhcpKeys.leases,
    queryFn: () => api.get<DHCPLease[]>("/dhcp/leases"),
    refetchInterval: pollMs,
    enabled,
  });
  const byKey = new Map<string, string>();
  for (const lease of data ?? []) {
    if (lease.hostname === "") continue;
    byKey.set(lease.ip, lease.hostname);
    byKey.set(`mac:${lease.mac}`, lease.hostname);
  }
  return byKey;
}

/**
 * What every DHCP write moves.
 *
 * The status goes with all of them because the render happens inside the
 * write: a scope the engine refuses leaves the row stored and the refusal
 * in `GET /dhcp/status`, so a screen that re-read only the rows would show
 * a saved scope and an engine line still describing the previous attempt.
 *
 * `resolver/status` too, since it carries a copy of that same object for
 * the top bar's status panel — without this the panel spent up to a poll
 * contradicting the page that had just fixed it.
 */
function invalidateDHCP(qc: QueryClient, ...keys: readonly (readonly string[])[]): void {
  for (const key of keys) void qc.invalidateQueries({ queryKey: key });
  void qc.invalidateQueries({ queryKey: dhcpKeys.status });
  void qc.invalidateQueries({ queryKey: settingsKeys.resolverStatus });
}

/** The scope fields a form owns. `id` and the timestamps are the server's,
 * and a PATCH is a merge — every key omitted keeps the value it has. */
export type ScopeInput = Partial<
  Omit<DHCPScope, "id" | "created_at" | "modified_at" | "pools"> & {
    /** Ids are the server's: a write replaces the list whole. */
    pools: PoolInput[];
  }
>;

export type PoolInput = Pick<DHCPPool, "start" | "end" | "class_id">;

export function useCreateScope() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: ScopeInput) => api.post<DHCPScope>("/dhcp/scopes", v),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.scopes),
  });
}

export function useUpdateScope() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: ScopeInput & { id: number }) =>
      api.patch<void>(`/dhcp/scopes/${id}`, body),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.scopes),
  });
}

/**
 * A scope's reservations go with it server-side, and its leases stop being
 * handed out, so both lists are re-read alongside the scopes.
 */
export function useDeleteScope() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/dhcp/scopes/${id}`),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.scopes, dhcpKeys.reservations, dhcpKeys.leases),
  });
}

/** The class fields a form owns; a PATCH replaces lists whole. */
export type ClassInput = Partial<Omit<DHCPClass, "id" | "created_at" | "modified_at">>;

export function useCreateClass() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: ClassInput) => api.post<DHCPClass>("/dhcp/classes", v),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.classes),
  });
}

export function useUpdateClass() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: ClassInput & { id: number }) =>
      api.patch<void>(`/dhcp/classes/${id}`, body),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.classes),
  });
}

/** `409` while any pool names the class, in the store's words. */
export function useDeleteClass() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/dhcp/classes/${id}`),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.classes),
  });
}

export type ReservationInput = Pick<
  DHCPReservation,
  "scope_id" | "mac" | "ip" | "hostname" | "comment"
>;

/** A reservation gives a device a name and an address before its first
 * lease, so it appears in the lease table too (`expires_at: 0`). */
export function useCreateReservation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: ReservationInput) => api.post<DHCPReservation>("/dhcp/reservations", v),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.reservations, dhcpKeys.leases),
  });
}

export function useUpdateReservation() {
  const qc = useQueryClient();
  return useMutation({
    // `scope_id` is fixed once created — the server refuses a PATCH that
    // moves one — so it is not in the editable half.
    mutationFn: ({ id, ...body }: Omit<ReservationInput, "scope_id"> & { id: number }) =>
      api.patch<void>(`/dhcp/reservations/${id}`, body),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.reservations, dhcpKeys.leases),
  });
}

export function useDeleteReservation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/dhcp/reservations/${id}`),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.reservations, dhcpKeys.leases),
  });
}

/**
 * DELETE /dhcp/leases/{ip} — `lease4-del` on this box's own engine, and the
 * one DHCP write that is **not** refused on a replica: a lease belongs to
 * the engine rather than to the configuration, and Kea's HA propagates the
 * release to the partner.
 *
 * The row goes the moment the engine accepts: the server drops it from its
 * own table rather than waiting for the next poll to rebuild one without it
 * (internal/dhcp's Manager.Release), so the invalidation below re-reads a
 * table the release is already out of.
 */
export function useReleaseLease() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (ip: string) => api.del<void>(`/dhcp/leases/${encodeURIComponent(ip)}`),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.leases),
  });
}

/** POST /dhcp/leases/{ip}/reserve — no body: the row is already on screen,
 * and the server builds the reservation from its own table entry. */
export function useReserveLease() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (ip: string) =>
      api.post<DHCPReservation>(`/dhcp/leases/${encodeURIComponent(ip)}/reserve`),
    onSuccess: () => invalidateDHCP(qc, dhcpKeys.reservations, dhcpKeys.leases),
  });
}

/**
 * POST /dhcp/apply — "Apply again". Nothing retries a refused configuration
 * on a timer, so this is the only way to re-send one after fixing what the
 * engine complained about.
 *
 * It answers with the status object that render left behind, which is
 * written straight into the cache: reading the result out of the
 * invalidated refetch instead would show the state *before* this apply
 * landed for as long as that request took.
 */
export function useApplyDHCP() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<DHCPStatus>("/dhcp/apply"),
    onSuccess: (status) => {
      qc.setQueryData(dhcpKeys.status, status);
      qc.setQueryData<ResolverStatus>(settingsKeys.resolverStatus, (prev) =>
        prev === undefined ? prev : { ...prev, dhcp: status },
      );
      void qc.invalidateQueries({ queryKey: dhcpKeys.leases });
    },
  });
}
