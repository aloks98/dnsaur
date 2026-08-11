import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api/client";
import { downloadBlob, filenameFromDisposition } from "../lib/download";
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

interface ZoneCreateInput {
  name: string;
  type?: Zone["type"];
  enabled?: boolean;
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
type ZoneUpdateInput = Partial<ZoneCreateInput>;

interface ZoneRecordInput {
  name?: string;
  type: string;
  ttl?: number;
  rdata: string;
  enabled?: boolean;
  comment?: string;
}

export function useZones() {
  return useQuery({
    queryKey: zoneKeys.all,
    queryFn: () => api.get<Zone[]>("/zones"),
  });
}

export function useZone(id: number) {
  return useQuery({
    queryKey: zoneKeys.detail(id),
    queryFn: () => api.get<Zone>(`/zones/${id}`),
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

export function useZoneRecords(zoneId: number) {
  return useQuery({
    queryKey: zoneRecordKeys.list(zoneId),
    queryFn: () => api.get<ZoneRecord[]>(`/zones/${zoneId}/records`),
  });
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

export function useCreateZoneRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ zoneId, ...v }: ZoneRecordInput & { zoneId: number }) =>
      api.post<{ id: number }>(`/zones/${zoneId}/records`, v),
    onSuccess: (_data, { zoneId }) => invalidateZoneAndRecords(qc, zoneId),
  });
}

export function useUpdateZoneRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ zoneId, id, ...v }: ZoneRecordInput & { zoneId: number; id: number }) =>
      api.put<void>(`/zones/${zoneId}/records/${id}`, v),
    onSuccess: (_data, { zoneId }) => invalidateZoneAndRecords(qc, zoneId),
  });
}

export function useDeleteZoneRecord() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ zoneId, id }: { zoneId: number; id: number }) =>
      api.del<void>(`/zones/${zoneId}/records/${id}`),
    onSuccess: (_data, { zoneId }) => invalidateZoneAndRecords(qc, zoneId),
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
