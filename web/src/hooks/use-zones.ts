import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
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
