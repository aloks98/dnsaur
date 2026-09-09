const BASE = "/api/v1";

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** The API-wide error envelope is a flat `{"error": "<message>"}` (docs/api.md,
 * "Conventions"), so the message comes off `.error` when there is one and off
 * the status line when there isn't. The raw text is kept on the ApiError
 * regardless: some endpoints put more than the summary in the body — a
 * rejected zone-file import carries an `errors` array alongside `error` — and
 * `.body` is the only place a caller can still reach it.
 *
 * `statusText` is empty over HTTP/2, which dropped the reason phrase, so it
 * cannot be the last resort: a reverse proxy answering its own HTML 502
 * otherwise produced an ApiError with an empty message — an empty toast, and
 * `[""]` out of the zone import's error list. */
function apiError(res: Response, text: string): ApiError {
  let msg = res.statusText || `HTTP ${res.status}`;
  try {
    const j = text ? JSON.parse(text) : null;
    if (j && typeof j.error === "string") msg = j.error;
  } catch {
    /* non-json error body */
  }
  return new ApiError(res.status, msg, text);
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: "include" };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    init.headers = { "Content-Type": "application/json" };
  }
  const res = await fetch(BASE + path, init);
  const text = await res.text();
  if (!res.ok) throw apiError(res, text);
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
}

/**
 * A GET whose success body is a file rather than JSON — currently only the
 * zone file export, which answers `text/dns` with a `Content-Disposition`.
 *
 * It cannot go through `request` above: that ends in `JSON.parse(text)`, and
 * a BIND master file is not JSON. The error path is deliberately identical,
 * though — a failure still answers the normal JSON envelope, so it is read
 * with the same `apiError` and surfaces as the same ApiError as everything
 * else.
 */
async function requestFile(path: string): Promise<{ blob: Blob; disposition: string | null }> {
  const res = await fetch(BASE + path, { credentials: "include" });
  if (!res.ok) throw apiError(res, await res.text());
  return { blob: await res.blob(), disposition: res.headers.get("Content-Disposition") };
}

export const api = {
  get: <T>(p: string) => request<T>("GET", p),
  post: <T>(p: string, b?: unknown) => request<T>("POST", p, b),
  put: <T>(p: string, b?: unknown) => request<T>("PUT", p, b),
  patch: <T>(p: string, b?: unknown) => request<T>("PATCH", p, b),
  del: <T>(p: string) => request<T>("DELETE", p),
  file: (p: string) => requestFile(p),
};
