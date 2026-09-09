import { useEffect, useRef } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import { downloadBlob, filenameFromDisposition } from "../lib/download";
import { REFRESH_TICK_MS } from "../lib/zones";
import type { Zone, ZoneRecord } from "../api/types";

// Zones-domain hooks — authoritative DNS zones (Task 11) and the records
// within them (Task 12). A zone replaces the old flat local_records
// override table: it is answered or refused, never forwarded, and every
// record write bumps its SOA serial in the same transaction
// (store.ZoneStore.BumpSerial) — which is why every record mutation below
// invalidates the zone queries too, not just the records query. Skipping
// that would leave the zones list showing a serial that's already stale.
export const zoneKeys = {
  all: ["zones"] as const,
  detail: (id: number) => ["zones", id] as const,
};

// Deliberately not nested under zoneKeys: a record mutation has to
// invalidate both trees explicitly (see invalidateZoneAndRecords below)
// rather than get one of them for free through query-key prefix matching —
// the same reasoning as filterKeys.lists/groups in use-filters.ts.
export const zoneRecordKeys = {
  all: ["zoneRecords"] as const,
  list: (zoneId: number) => ["zoneRecords", zoneId] as const,
};

// Also not nested under zoneKeys, and for the same reason: editing
// notify_to (a zone-row mutation) has to invalidate this tree explicitly
// too — see useUpdateZone's onSuccess.
export const zoneNotifyKeys = {
  list: (zoneId: number) => ["zoneNotifies", zoneId] as const,
};

/**
 * How often a screen keeps a secondary's transfer state honest, and the one
 * cadence in this file — see `watchesTransfers` for when it applies at all.
 *
 * It is the scheduler's own tick (`refreshTick`, internal/zones/refresh.go),
 * deliberately rather than a number picked for feel: that tick is how often
 * the server asks which zones are due, so it is also the finest resolution at
 * which any of this can change. Polling faster cannot surface anything
 * sooner — between two ticks there is nothing new to read — and polling
 * slower would let the screen sit behind a decision the server has already
 * made. It is the same slack `transferState` already grants a zone before
 * calling it overdue (lib/zones.ts), so the two agree by construction.
 *
 * 30s also happens to be what the dashboard's stats and the pause banner
 * already use (use-stats.ts, use-blocking.ts), so the app has one live
 * cadence rather than three.
 */
const TRANSFER_POLL_MS = REFRESH_TICK_MS;

/**
 * Whether anything in this query's data can change with nobody touching it.
 *
 * Only a zone that pulls from a master can. Its records, serial and refresh
 * stamps are written by the scheduler in the background — on the SOA's
 * refresh, on a retry when a master comes back, on a secondary crossing into
 * expiry — so a screen showing one is out of date the moment it stops
 * asking. Everything else on these screens (a primary's records, a
 * forwarder's upstreams, any zone's settings) changes only when an operator
 * changes it, and the mutation that did it has already invalidated.
 *
 * The set is `secondary || stub`, which is deliberately the *server's* own
 * rule rather than a second one: internal/zones/refresh.go's
 * `pullsFromAMaster` decides which zones the scheduler wakes for, and
 * internal/api's gate on POST /refresh is the same predicate again. A stub
 * left out of it would sit on "no NS set yet" until someone reloaded the
 * page, because the first fetch that changes that is the scheduler's.
 *
 * A **forwarder** is not in it and must not be: its upstreams are typed in
 * by hand and nothing in the background ever touches its row.
 *
 * Not narrowed further to *enabled* zones. A disabled one is skipped by the
 * scheduler, but a secondary can still cross its expiry while on screen (a
 * fact about the clock, which the re-render is what surfaces) and either can
 * be re-enabled from another tab.
 */
function watchesTransfers(zones: Zone | Zone[] | undefined): boolean {
  if (zones === undefined) return false;
  const pullsFromAMaster = (zone: Zone) => zone.type === "secondary" || zone.type === "stub";
  return Array.isArray(zones) ? zones.some(pullsFromAMaster) : pullsFromAMaster(zones);
}

interface ZoneCreateInput {
  name: string;
  type?: Zone["type"];
  enabled?: boolean;
  /**
   * Where a secondary pulls the zone from, or a stub fetches its NS set
   * from: comma-separated `host[:port]`, port 53 by default. Required and
   * non-empty for both, and **refused with 400 on any other type** — a zone
   * that never pulls has no primaries, and the server will not store
   * configuration nothing reads (see checkZoneTransferConfig in
   * internal/api/zones_handlers.go). So this is omitted, not sent empty, for
   * a primary or a forwarder.
   */
  primaries?: string;
  /**
   * The key a secondary signs its transfer requests with, or a stub its
   * SOA/NS queries with. Optional (0, or omitted, means unsigned), allowed
   * on those two types alone on the same terms as `primaries`, and validated
   * to name a key that exists.
   */
  tsig_key_id?: number;
  /**
   * Where a forwarder sends the queries it claims: comma-separated
   * `host[:port]`, port 53 by default. **Forwarder-only** — the server 400s
   * "forward_to applies to forwarder zones only" on anything else — and
   * unlike `primaries` it may legitimately be empty, which claims the suffix
   * and SERVFAILs every name beneath it rather than falling through. Omitted
   * rather than sent empty in that case, on the same terms as the two above.
   */
  forward_to?: string;
  /**
   * Who may pull this zone by AXFR: a comma-separated list of address,
   * CIDR, or key:<tsig name> — see lib/acl.ts for the format. Optional;
   * omitted or "" means deny every transfer, which is the default. Unlike
   * `primaries` and `tsig_key_id`, this applies to both primary and
   * secondary zones — a secondary re-serves what it pulled.
   */
  allow_transfer?: string;
  /**
   * Who this zone tells when it changes (DNS NOTIFY, RFC 1996): a
   * comma-separated list of host[:port] with an optional key:<tsig name>
   * suffix — see lib/notify.ts for the format. Optional; omitted or ""
   * means notify nobody, which is the default. Like allow_transfer, this
   * applies to both primary and secondary zones — a secondary that
   * re-serves what it pulled has its own downstream secondaries to tell.
   */
  notify_to?: string;
  soa_ns?: string;
  soa_mbox?: string;
  soa_refresh?: number;
  soa_retry?: number;
  soa_expire?: number;
  soa_minimum?: number;
}

/**
 * Every field optional — PATCH /zones/{id} only changes what's present.
 * soa_ttl is deliberately absent: it's fixed at 900 in Milestone A and
 * there is no request field to set it (see openapi.yaml).
 */
export type ZoneUpdateInput = Partial<ZoneCreateInput>;

interface ZoneRecordInput {
  name?: string;
  type: string;
  ttl?: number;
  rdata: string;
  enabled?: boolean;
  comment?: string;
}

/**
 * Every zone, re-read while any of them is a secondary.
 *
 * The interval and the focus revalidation are both functions of the data
 * rather than constants, so the cost is paid only by a list that actually has
 * something in it that moves on its own — see `watchesTransfers`. Focus is
 * worth having alongside the timer, not instead of it: query-core does not
 * poll a hidden tab, so without this, coming back to one shows the state it
 * had when you left for up to a full interval.
 */
export function useZones() {
  return useQuery({
    queryKey: zoneKeys.all,
    queryFn: () => api.get<Zone[]>("/zones"),
    refetchInterval: (query) => (watchesTransfers(query.state.data) ? TRANSFER_POLL_MS : false),
    refetchOnWindowFocus: (query) => watchesTransfers(query.state.data),
  });
}

/** One zone, on the same terms as useZones — here the question is simply
 * whether this zone is the secondary. */
export function useZone(id: number) {
  return useQuery({
    queryKey: zoneKeys.detail(id),
    queryFn: () => api.get<Zone>(`/zones/${id}`),
    refetchInterval: (query) => (watchesTransfers(query.state.data) ? TRANSFER_POLL_MS : false),
    refetchOnWindowFocus: (query) => watchesTransfers(query.state.data),
  });
}

export function useCreateZone() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: ZoneCreateInput) => api.post<{ id: number }>("/zones", v),
    onSuccess: () => qc.invalidateQueries({ queryKey: zoneKeys.all }),
  });
}

export function useUpdateZone() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...v }: ZoneUpdateInput & { id: number }) =>
      api.patch<void>(`/zones/${id}`, v),
    onSuccess: (_data, { id }) => {
      void qc.invalidateQueries({ queryKey: zoneKeys.all });
      void qc.invalidateQueries({ queryKey: zoneKeys.detail(id) });
      // Every PATCH, not just one that touched notify_to — the same reason
      // invalidateZoneAndRecords below refetches the whole records tree for
      // any record write rather than asking which field changed. A PATCH
      // that *did* edit notify_to changes the target set /notifies reports
      // (the notifier reconciles zone_notifies to match on its next pass,
      // woken by this same PATCH — see Notifier.Wake); one that didn't is a
      // cheap refetch of something that comes back unchanged.
      void qc.invalidateQueries({ queryKey: zoneNotifyKeys.list(id) });
    },
  });
}

export function useDeleteZone() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.del<void>(`/zones/${id}`),
    // Records too, and every zone's: deleting a zone drops its own records
    // (which no longer have a page to be stale on) but also retires the PTRs
    // they owned from a reverse zone that is still there — see
    // invalidateZoneAndRecords.
    onSuccess: (_data, id) => {
      void qc.invalidateQueries({ queryKey: zoneKeys.all });
      void qc.invalidateQueries({ queryKey: zoneKeys.detail(id) });
      void qc.invalidateQueries({ queryKey: zoneRecordKeys.all });
    },
  });
}

/**
 * A zone's records. Deliberately **not** polled, for a secondary or anything
 * else.
 *
 * A secondary's records change exactly when a transfer replaces them, and a
 * transfer that replaced them moved `refreshed_at` on the zone row — which
 * the zone queries above are already watching. A second timer here would
 * double the request rate to learn one fact, and would learn it no sooner.
 * `useRecordsFollowTransfers` is the other half of that bargain.
 */
export function useZoneRecords(zoneId: number, { enabled = true }: { enabled?: boolean } = {}) {
  return useQuery({
    queryKey: zoneRecordKeys.list(zoneId),
    queryFn: () => api.get<ZoneRecord[]>(`/zones/${zoneId}/records`),
    // The zones list asks for a count per row and there is no record_count
    // on GET /zones to read it from, so it holds this back until the row is
    // actually on screen — see RecordsCell (pages/zones/list.tsx). The detail
    // page, which is the records, never passes it.
    enabled,
  });
}

/**
 * One `notify_to` target's outbound NOTIFY delivery state (Go: notifyRow,
 * internal/api/notifies_handlers.go) — GET /zones/{id}/notifies's own row,
 * Task 11's read side of D4.
 *
 * `state` is derived server-side from `notified_at`, `notified_serial`,
 * `attempts` and `pending_serial` against the zone's current `soa_serial`,
 * deliberately never left for the client to infer from those columns — see
 * notifyStateOf's own comment: two clients could compute it differently, and
 * a status that can disagree with itself is not a status. `pending_serial`
 * is what scopes `state` and `attempts` to the round the notifier is in, so
 * neither can describe a round the zone has already moved past.
 */
export interface ZoneNotify {
  /** host:port, as written in the zone's notify_to (port always explicit).
   * Carries no key; it is the row identity. */
  target: string;
  state: "never" | "current" | "retrying" | "gave_up";
  /** The last serial this target acknowledged; meaningless (0) until
   * notified_at is non-zero. */
  notified_serial: number;
  /** Unix ms of the last round that landed; 0 = never delivered. */
  notified_at: number;
  /** How many times the current round has been tried. Reset to 0 by a
   * delivery that lands, and by the zone's serial moving on — a budget spent
   * against an older serial is not this round's. */
  attempts: number;
  /** The round budget attempts is compared against — sent alongside
   * attempts so "try 3/5" never hardcodes the denominator. */
  max_attempts: number;
  /** The most recent failure, in the sender's own words; "" when the last
   * attempt delivered or none has been tried. */
  last_error: string;
  /** Unix ms this target was first seen. Never moves after; it is what
   * dates a target whose state is still `never`. */
  created_at: number;
}

/**
 * A zone's outbound NOTIFY delivery state, one row per `notify_to` target —
 * the NOTIFY OUT row's own data (Task 12).
 *
 * Polled on the same cadence as a secondary's transfer state
 * (`TRANSFER_POLL_MS`, the scheduler's own tick) while there is at least one
 * target to watch, and left alone otherwise: the notifier's own pass runs
 * every 5s (`notifyTick`, internal/zones/notifier.go), so this page cannot
 * promise anything fresher than the zone-refresh cadence already imported
 * here, and 30s is cheap for a row that stays on screen. A zone with no
 * targets never polls — an empty array cannot change into anything but
 * another empty array without an edit `useUpdateZone` already invalidates
 * this on.
 */
export function useZoneNotifies(zoneId: number) {
  return useQuery({
    queryKey: zoneNotifyKeys.list(zoneId),
    queryFn: () => api.get<ZoneNotify[]>(`/zones/${zoneId}/notifies`),
    refetchInterval: (query) => ((query.state.data?.length ?? 0) > 0 ? TRANSFER_POLL_MS : false),
    refetchOnWindowFocus: (query) => (query.state.data?.length ?? 0) > 0,
  });
}

/**
 * Refetches a zone's records when its last transfer moved — the single rule
 * that keeps a record list current under a secondary without polling it.
 *
 * `refreshed_at` is the signal, and it is chosen over `soa_serial` on purpose
 * even though a transfer moves both. The serial also moves on every ordinary
 * record write, which already invalidates the records query itself, so
 * watching it would fetch the list twice for every edit on a *primary*. Only
 * a transfer touches `refreshed_at`, and a primary's is 0 forever.
 *
 * A zone is only ever compared against a reading of *itself* taken earlier in
 * this component's life, so the first sight of a zone invalidates nothing —
 * a mount would otherwise refetch the list it has just fetched.
 *
 * Takes either the whole list or one zone, because both pages need it for the
 * same reason: the list shows a record count per row, the detail page shows
 * the records. The alternative — putting this inside useZoneRecords — would
 * have each of them read the zone back out of a different cache entry.
 */
export function useRecordsFollowTransfers(zones: Zone | Zone[] | undefined): void {
  const qc = useQueryClient();
  const seen = useRef<Map<number, number>>(new Map());

  useEffect(() => {
    if (zones === undefined) return;
    const previous = seen.current;
    const next = new Map<number, number>();
    for (const zone of Array.isArray(zones) ? zones : [zones]) {
      next.set(zone.id, zone.refreshed_at);
      const before = previous.get(zone.id);
      if (before !== undefined && before !== zone.refreshed_at) {
        void qc.invalidateQueries({ queryKey: zoneRecordKeys.list(zone.id) });
      }
    }
    seen.current = next;
  }, [zones, qc]);
}

/**
 * Every record write bumps the owning zone's SOA serial, so all three
 * record mutations below invalidate the zone queries alongside the records
 * query — the zones list (Task 11) reads soa_serial straight off useZones,
 * and a zone detail page (Task 12) would read it off useZone(zoneId).
 * Invalidating only zoneRecordKeys would leave both showing a stale serial.
 *
 * The records half is the whole `zoneRecords` key space, not just the zone
 * that was written to, because a record write is not confined to that zone:
 * writing an A/AAAA record makes the server write the matching PTR into
 * whichever reverse zone covers the address (auto-PTR, internal/api/autoptr.go),
 * and a zone delete retires the PTRs its records owned. The zone id in the
 * request says nothing about which reverse zone that was, and the client has
 * no way to compute it — the server picks the longest-matching enabled
 * primary zone. Refetching every mounted record list is the cheap, correct
 * answer; guessing at one reverse zone would leave a detail page showing a
 * record set that no longer exists whenever the guess is wrong. The zone
 * queries below already invalidate wholesale for the same reason (the
 * reverse zone's serial moved too).
 */
function invalidateZoneAndRecords(qc: ReturnType<typeof useQueryClient>, zoneId: number) {
  void qc.invalidateQueries({ queryKey: zoneRecordKeys.all });
  void qc.invalidateQueries({ queryKey: zoneKeys.all });
  void qc.invalidateQueries({ queryKey: zoneKeys.detail(zoneId) });
}

/**
 * What a completed transfer reports back (Go: zoneRefreshResult in
 * internal/api/zones_handlers.go). `primary` and `records` are why the body
 * exists at all — the zone row afterwards carries the serial and the two
 * stamps, but says nothing about *which* of its primaries answered or how
 * much arrived.
 */
export interface ZoneRefreshResult {
  /** The primary that answered, as `host:port`. */
  primary: string;
  serial: number;
  records: number;
  refreshed_at: number;
  expires_at: number;
}

/**
 * Transfers a secondary now, whatever its SOA schedule says.
 *
 * Synchronous on the wire: the request does not answer until the transfer has
 * finished, because the outcome — and especially the failure — is the whole
 * reason anyone presses the button. That failure arrives as an ApiError whose
 * message is the transfer's own text, naming every primary tried.
 *
 * Invalidates on **either** outcome, which is the part worth stating. A
 * success obviously moved things — the record set was replaced and the
 * serial and stamps came with it. A failure moves things too, and less
 * obviously: it writes `last_error` and `last_attempt` onto the zone row (see
 * lib/zones.ts). So the pages that read those refetch after a failed transfer
 * as much as after a successful one, which is also what lets them show the
 * cause without holding a copy of it in component state.
 */
export function useRefreshZone() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.post<ZoneRefreshResult>(`/zones/${id}/refresh`, {}),
    // The zone queries only, and not the records — which is the point of
    // useRecordsFollowTransfers. A transfer that actually replaced the record
    // set moved `refreshed_at` on the row this is about to refetch, so the
    // record list is refreshed by the zone arriving rather than by this
    // mutation guessing that it should be. Invalidating both here would fire
    // two fetches of the same list for one transfer, and would refetch it
    // after a *failed* refresh, which changed no record at all.
    //
    // onSettled, not onSuccess: see above.
    onSettled: (_data, _err, id) => {
      void qc.invalidateQueries({ queryKey: zoneKeys.all });
      void qc.invalidateQueries({ queryKey: zoneKeys.detail(id) });
    },
  });
}

export function useCreateZoneRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ zoneId, ...v }: ZoneRecordInput & { zoneId: number }) =>
      api.post<{ id: number }>(`/zones/${zoneId}/records`, v),
    onSuccess: (_data, { zoneId }) => invalidateZoneAndRecords(qc, zoneId),
  });
}

/**
 * A write addressed to a record the server no longer has.
 *
 * 404 is the one failure that says the *list on screen* is wrong rather than
 * the value that was sent: the record was deleted from another tab or by
 * another admin, and nothing else would refetch, so its row stayed on screen
 * looking live and every retry against it 404s again. Refetching is the
 * whole of the fix; what the screen says about it is the caller's own
 * (see detail.tsx).
 */
function refetchIfGone(qc: ReturnType<typeof useQueryClient>, err: unknown, zoneId: number): void {
  if (err instanceof ApiError && err.status === 404) invalidateZoneAndRecords(qc, zoneId);
}

export function useUpdateZoneRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ zoneId, id, ...v }: ZoneRecordInput & { zoneId: number; id: number }) =>
      api.put<void>(`/zones/${zoneId}/records/${id}`, v),
    onSuccess: (_data, { zoneId }) => invalidateZoneAndRecords(qc, zoneId),
    onError: (err, { zoneId }) => refetchIfGone(qc, err, zoneId),
  });
}

export function useDeleteZoneRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ zoneId, id }: { zoneId: number; id: number }) =>
      api.del<void>(`/zones/${zoneId}/records/${id}`),
    onSuccess: (_data, { zoneId }) => invalidateZoneAndRecords(qc, zoneId),
    onError: (err, { zoneId }) => refetchIfGone(qc, err, zoneId),
  });
}

/** One record an import would replace in place. Both sides are carried
 * because a dry run's whole job is making a destructive replace legible
 * before it happens — "3 changed" with no values is not that.
 *
 * The server pairs the two by the RR itself (name, type and rdata together:
 * `recordIdentity` in internal/api/zonefile_handlers.go), so `from` and `to`
 * always agree on all three. What differs is the TTL or the enabled flag —
 * anything else is a delete plus an add, not a change. */
export interface ZoneRecordChange {
  from: ZoneRecord;
  to: ZoneRecord;
}

/** The three-way diff between a zone as it stands and the file that would
 * replace it — the answer to both the dry run and the commit. `errors` is
 * empty on success; on a rejection it carries one message per problem and
 * the three lists are empty. */
export interface ZoneFileDiff {
  add: ZoneRecord[];
  change: ZoneRecordChange[];
  delete: ZoneRecord[];
  errors: string[];
  /** One-line summary, present only on a rejection. */
  error?: string;
}

/**
 * Downloads a zone as a BIND master file.
 *
 * Allowed on every zone, built-in ones included: the server refuses *writes*
 * to an RFC 6303 zone, not reads, and there is no "internal" check on the
 * export handler (internal/api/zonefile_handlers.go).
 *
 * The zone's own name is the fallback filename, used only if the response
 * arrives without a Content-Disposition — the server always sends one.
 */
export function useExportZoneFile() {
  return useMutation({
    mutationFn: async ({ id, name }: { id: number; name: string }) => {
      const { blob, disposition } = await api.file(`/zones/${id}/file`);
      downloadBlob(blob, filenameFromDisposition(disposition, `${name}.zone`));
    },
  });
}

/**
 * Replaces a zone's contents with a master file — or, with `dryRun`, reports
 * what that would do and touches nothing.
 *
 * `dryRun` is a required parameter rather than an optional one with a
 * default, mirroring the server: `dry_run` is a required field there and an
 * omitted one is a 400, deliberately, because Go's zero value for the field
 * is the committing branch (see zoneFileImport's own comment). Making it
 * required here means the destructive choice has to be written at every call
 * site instead of being what happens when nobody thought about it.
 *
 * Only a committed import invalidates: a dry run changed nothing, so
 * refetching after one would be pure noise. A committed one goes through
 * invalidateZoneAndRecords for the usual reason — it rewrote the record set
 * and bumped the zone's serial.
 */
export function useImportZoneFile() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, content, dryRun }: { id: number; content: string; dryRun: boolean }) =>
      api.post<ZoneFileDiff>(`/zones/${id}/file`, { content, dry_run: dryRun }),
    onSuccess: (_data, { id, dryRun }) => {
      if (!dryRun) invalidateZoneAndRecords(qc, id);
    },
  });
}

/**
 * Every problem the server named when it refused a zone file.
 *
 * These live on the ApiError's `body`, not its `message`: the client reads
 * only `.error` — the one-line summary — into the message (api/client.ts),
 * and spec §8 requires naming every offending line, which one string cannot
 * do. Falling back to the message keeps the dialog from rendering blank when
 * the failure is not a 422 at all (a 409 on a built-in zone, say, or a 500).
 *
 * The strings are returned exactly as they arrived. Most name a line or the
 * offending record, but a file-wide problem — a missing SOA — names neither,
 * so there is no prefix here worth parsing and any attempt to would quietly
 * drop that whole class of message.
 */
export function zoneFileErrors(err: unknown): string[] {
  if (!(err instanceof ApiError)) return ["Couldn't read the zone file."];
  try {
    const parsed = JSON.parse(err.body ?? "") as { errors?: unknown };
    if (Array.isArray(parsed.errors)) {
      const messages = parsed.errors.filter((e): e is string => typeof e === "string");
      if (messages.length > 0) return messages;
    }
  } catch {
    /* not the JSON envelope — fall through to the summary below */
  }
  return [err.message];
}
