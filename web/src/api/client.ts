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

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: "include" };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
    init.headers = { "Content-Type": "application/json" };
  }
  const res = await fetch(BASE + path, init);
  const text = await res.text();
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const j = text ? JSON.parse(text) : null;
      if (j && typeof j.error === "string") msg = j.error;
    } catch {
      /* non-json error body */
    }
    throw new ApiError(res.status, msg, text);
  }
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
}

export const api = {
  get: <T>(p: string) => request<T>("GET", p),
  post: <T>(p: string, b?: unknown) => request<T>("POST", p, b),
  put: <T>(p: string, b?: unknown) => request<T>("PUT", p, b),
  patch: <T>(p: string, b?: unknown) => request<T>("PATCH", p, b),
  del: <T>(p: string) => request<T>("DELETE", p),
};
