import { afterEach, expect, test, vi } from "vitest";
import { api } from "./client";

type FetchFn = (url: string, init?: RequestInit) => Promise<Response>;

function mockFetch(status: number, body: unknown, ct = "application/json") {
  return vi.fn<FetchFn>().mockResolvedValue(
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

// A reverse proxy's own 502/413/504 page is HTML, and `Response.statusText`
// is empty over HTTP/2 — the protocol dropped the reason phrase. Together
// that used to produce ApiError.message === "": an empty toast, and an empty
// string in the zone import's `errors` list.
test("a non-json body with no status text still names the status", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn<FetchFn>()
      .mockResolvedValue(
        new Response("<html><body>502 Bad Gateway</body></html>", { status: 502, statusText: "" }),
      ),
  );
  await expect(api.get("/settings")).rejects.toMatchObject({
    name: "ApiError",
    status: 502,
    message: "HTTP 502",
    body: "<html><body>502 Bad Gateway</body></html>",
  });
});

test("api.file returns the blob and its Content-Disposition", async () => {
  const f = vi.fn<FetchFn>().mockResolvedValue(
    new Response("example.com. 300 IN A 10.0.0.1\n", {
      status: 200,
      headers: {
        "content-type": "text/dns",
        "content-disposition": 'attachment; filename="example.com.zone"',
      },
    }),
  );
  vi.stubGlobal("fetch", f);

  const { blob, disposition } = await api.file("/zones/1/file");
  expect(await blob.text()).toBe("example.com. 300 IN A 10.0.0.1\n");
  expect(disposition).toBe('attachment; filename="example.com.zone"');
  expect(f.mock.calls[0]![0]).toBe("/api/v1/zones/1/file");
});

// The error path is deliberately the same one `request` takes: a failed
// export answers the normal JSON envelope, not a file.
test("api.file surfaces a failure as the same ApiError as everything else", async () => {
  vi.stubGlobal("fetch", mockFetch(404, { error: "not found" }));
  await expect(api.file("/zones/9/file")).rejects.toMatchObject({
    name: "ApiError",
    status: 404,
    message: "not found",
  });
});

// fetch rejects rather than resolving when the server is unreachable. That
// is not an ApiError and must not be dressed up as one — lib/query-client's
// 401 handler and every call site's `instanceof ApiError` check depend on
// the distinction.
test("a network failure propagates as the fetch TypeError, not an ApiError", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn<FetchFn>()
      .mockRejectedValue(new TypeError("NetworkError when attempting to fetch resource.")),
  );
  await expect(api.get("/auth/me")).rejects.toBeInstanceOf(TypeError);
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
