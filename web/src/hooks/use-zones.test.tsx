import { http, HttpResponse } from "msw";
import { expect, test } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { makeQueryClient } from "../lib/query-client";
import { server } from "../test/msw-server";
import type { ZoneRecord } from "../api/types";
import { useCreateZoneRecord, useDeleteZone, useZoneRecords } from "./use-zones";

function wrapper() {
  const client = makeQueryClient();
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}

const ptr: ZoneRecord = {
  id: 40,
  zone_id: 2,
  name: "10",
  type: "PTR",
  ttl: 300,
  rdata: "bifrost.example.com.",
  enabled: true,
  comment: "",
};

/**
 * Zone 1 is the forward zone, zone 2 the reverse one. The reverse zone's
 * record list is empty until the forward zone takes an A record, at which
 * point the server has written the PTR into it — auto-PTR, entirely
 * server-side, in the same request (internal/api/autoptr.go).
 */
function autoPTRHandlers() {
  let ptrWritten = false;
  return [
    http.get("/api/v1/zones/2/records", () =>
      HttpResponse.json<ZoneRecord[]>(ptrWritten ? [ptr] : []),
    ),
    http.post("/api/v1/zones/1/records", () => {
      ptrWritten = true;
      return HttpResponse.json({ id: 9 }, { status: 201 });
    }),
    http.delete("/api/v1/zones/1", () => {
      ptrWritten = false;
      return new HttpResponse(null, { status: 204 });
    }),
  ];
}

/**
 * The whole point of auto-PTR is that one write changes two zones, so
 * invalidating only the zone the request named leaves anyone looking at the
 * reverse zone staring at a list that is missing the PTR their write just
 * created — until some unrelated refetch happens by.
 */
test("a record write invalidates another zone's record list", async () => {
  server.use(...autoPTRHandlers());
  const { result } = renderHook(
    () => ({ reverse: useZoneRecords(2), create: useCreateZoneRecord() }),
    { wrapper: wrapper() },
  );
  await waitFor(() => expect(result.current.reverse.isSuccess).toBe(true));
  expect(result.current.reverse.data).toEqual([]);

  await act(async () => {
    await result.current.create.mutateAsync({
      zoneId: 1,
      name: "bifrost",
      type: "A",
      ttl: 300,
      rdata: "192.168.150.10",
    });
  });

  await waitFor(() => expect(result.current.reverse.data).toEqual([ptr]));
});

// Deleting a zone retires the PTRs its records owned from the reverse zone,
// which outlives the delete — same cross-zone write, same stale list.
test("a zone delete invalidates another zone's record list", async () => {
  server.use(...autoPTRHandlers());
  const { result } = renderHook(
    () => ({ reverse: useZoneRecords(2), create: useCreateZoneRecord(), del: useDeleteZone() }),
    { wrapper: wrapper() },
  );
  await waitFor(() => expect(result.current.reverse.isSuccess).toBe(true));
  await act(async () => {
    await result.current.create.mutateAsync({
      zoneId: 1,
      name: "bifrost",
      type: "A",
      ttl: 300,
      rdata: "192.168.150.10",
    });
  });
  await waitFor(() => expect(result.current.reverse.data).toEqual([ptr]));

  await act(async () => {
    await result.current.del.mutateAsync(1);
  });

  await waitFor(() => expect(result.current.reverse.data).toEqual([]));
});
