import { afterEach, expect, test, vi } from "vitest";
import { api } from "./client";

function mockFetch(status: number, body: unknown, ct = "application/json") {
  return vi.fn<(url: string, init?: RequestInit) => Promise<Response>>().mockResolvedValue(
    new Response(body === undefined ? null : JSON.stringify(body), {
      status,
      headers: { "content-type": ct },
    }),
  );
}

afterEach(() => vi.restoreAllMocks());

test("get returns parsed json and hits /api/v1", async () => {
  const f = mockFetch(200, { id: 1, username: "admin", totp_enabled: false });
  vi.stubGlobal("fetch", f);
  const me = await api.get<{ username: string }>("/auth/me");
  expect(me.username).toBe("admin");
  const [url, init] = f.mock.calls[0]!;
  expect(url).toBe("/api/v1/auth/me");
  expect((init as RequestInit).credentials).toBe("include");
});

test("non-2xx throws ApiError with the error message", async () => {
  vi.stubGlobal("fetch", mockFetch(401, { error: "authentication required" }));
  await expect(api.get("/settings")).rejects.toMatchObject({
    name: "ApiError",
    status: 401,
    message: "authentication required",
  });
});

test("post sends json body and 204 yields undefined", async () => {
  const f = mockFetch(204, undefined);
  vi.stubGlobal("fetch", f);
  const r = await api.post("/blocking/pause", { group_id: 0, minutes: 5 });
  expect(r).toBeUndefined();
  const [, init] = f.mock.calls[0]!;
  const requestInit = init as RequestInit;
  expect(requestInit.method).toBe("POST");
  expect(JSON.parse(requestInit.body as string)).toEqual({ group_id: 0, minutes: 5 });
  const headers = requestInit.headers as Record<string, string>;
  expect(headers["Content-Type"]).toBe("application/json");
});
