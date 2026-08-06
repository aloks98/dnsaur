# dnsaur Web Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A React 19 + Vite 8 SPA in `web/`, embedded in the dnsaur Go binary via `go:embed`, that provides full-parity UI over the `/api/v1` REST API — setup, login, dashboard, live query log, filtering, records, settings, account.

**Architecture:** SPA built to `web/dist/`, embedded and served by the Go API server (`/api/*` → handlers, everything else → SPA with `index.html` fallback). Data via TanStack Query over a thin typed `fetch` client using hand-written types; auth on the HttpOnly session cookie. Components from rnui (`@e412/rnui-react`), a custom `dnsaur` rnui theme.

**Tech Stack:** React 19, Vite 8, TypeScript strict, React Router v7, TanStack Query v5, `@e412/rnui-react` + `@e412/rnui-themes`, Tailwind CSS v4, oxlint + oxfmt + Vitest + React Testing Library + MSW; Go `go:embed`.

## Global Constraints

- Package manager: **pnpm** (matches rnui). Node ≥ 20.
- **Vite 8**, TypeScript **strict**. All code oxfmt-formatted and oxlint-clean before every commit.
- Lint/format/test tools are **oxlint, oxfmt, Vitest** — never eslint/prettier/jest.
- Components come from **rnui** (`@e412/rnui-react`); do not hand-build a component that rnui already provides. rnui source for exact props: `~/projects/rnui/packages/react/src/components/`. Themes: `~/projects/rnui/packages/themes/src/`.
- Custom theme only: `[data-theme="dnsaur"]` (+ `.dark`). Do NOT import rnui presets.
- API base path `/api/v1`; all requests send `credentials: 'include'`; error envelope is `{"error": string}`; timestamps are Unix **milliseconds** (stats buckets Unix **seconds**).
- No token stored in JS — auth rides the HttpOnly `dnsaur_session` cookie the API sets.
- Dev ports (non-default): Vite `127.0.0.1:5280`, dev API `127.0.0.1:8380` (`DNSAUR_HTTP_LISTEN`). Vite proxies `/api` → `:8380`.
- **Screen/visual design is produced through the design skills** (`frontend-design` for direction, then `arrange`/`typeset`/`colorize`/`animate`/`polish`, with `critique`/`audit` gates) — not hand-styled ad hoc. Page tasks specify the functional contract (data, interactions, test assertions); the design skill realizes the visual layer.
- Conventional commits (`feat:`, `fix:`, `test:`, `chore:`, `docs:`). Branch: `feat/web-dashboard`.
- Every task: `pnpm oxlint`, `pnpm format:check`, `pnpm typecheck`, `pnpm test` (Vitest) all green before committing. Go tasks also `go test -race ./...` + `~/go/bin/golangci-lint run ./...`.

## File Structure

```
web/
  package.json, pnpm-lock.yaml, tsconfig.json, vite.config.ts
  .oxlintrc.json, .oxfmtrc.json, vitest.config.ts
  index.html
  README.md                          — dev workflow
  src/
    main.tsx                         — root render, providers
    app.tsx                          — router + auth gate
    styles/
      app.css                        — tailwind + rnui theme imports
      dnsaur-theme.css               — custom [data-theme="dnsaur"] tokens (light+dark)
    api/
      client.ts                      — typed fetch wrapper + ApiError
      types.ts                       — hand-written API types
      sse.ts                         — SSE subscription helper
    lib/
      query-client.ts                — TanStack Query config
      theme.ts                       — light/dark toggle + persistence
    hooks/                           — per-resource query/mutation hooks
      use-auth.ts, use-settings.ts, use-clients.ts, use-groups.ts,
      use-filters.ts, use-records.ts, use-queries.ts, use-stats.ts,
      use-blocking.ts, use-tokens.ts, use-totp.ts
    components/
      app-shell.tsx, sidebar-nav.tsx, header.tsx, command-palette.tsx,
      theme-toggle.tsx, error-boundary.tsx, api-unreachable-banner.tsx
    pages/
      setup.tsx, login.tsx, home.tsx, queries.tsx,
      filtering/ (lists.tsx, rules.tsx, groups-clients.tsx, index.tsx),
      dns.tsx, settings.tsx, account.tsx
    test/
      msw-handlers.ts, msw-server.ts, render.tsx (test utils)
internal/api/
  static.go                          — embed + SPA static handler (Go)
  static_test.go
.forgejo/workflows/ci.yml            — add web build+test job
```

---

### Task 1: Scaffold web app + custom theme + Go embed wiring

**Files:**
- Create: `web/package.json`, `web/tsconfig.json`, `web/vite.config.ts`, `web/vitest.config.ts`, `web/.oxlintrc.json`, `web/.oxfmtrc.json`, `web/index.html`, `web/src/main.tsx`, `web/src/app.tsx`, `web/src/styles/app.css`, `web/src/styles/dnsaur-theme.css`, `web/src/pages/home.tsx` (placeholder), `web/README.md`, `web/.gitignore`
- Create: `internal/api/static.go`, `internal/api/static_test.go`
- Modify: `internal/api/server.go` (mount static handler on non-`/api` paths), `.forgejo/workflows/ci.yml` (web job)

**Interfaces:**
- Produces: a runnable Vite app rendering a placeholder Home under the dnsaur theme; `api.StaticHandler(fsys fs.FS) http.Handler` serving an embedded SPA with `index.html` fallback; `Server` mounts it for non-`/api` paths. `web/dist` embedded via `//go:embed all:dist` in a new `web/embed.go` (Go package `web`) exposing `web.Dist fs.FS` (sub-rooted at `dist`).

- [ ] **Step 1: Scaffold the Vite app**

```bash
cd /home/aloks98/projects/dnsaur
pnpm create vite@8 web --template react-ts   # if the template prompt blocks, scaffold manually per files below
cd web && pnpm add react@19 react-dom@19 react-router@7 @tanstack/react-query@5 @e412/rnui-react @e412/rnui-themes && pnpm add -D vite@8 @vitejs/plugin-react typescript oxlint oxfmt vitest @testing-library/react @testing-library/user-event @testing-library/jest-dom jsdom msw @tailwindcss/vite tailwindcss
```

`web/package.json` scripts:
```json
{
  "name": "dnsaur-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "preview": "vite preview",
    "typecheck": "tsc -b --noEmit",
    "lint": "oxlint src/",
    "format": "oxfmt --write src/",
    "format:check": "oxfmt --check src/",
    "test": "vitest run",
    "test:watch": "vitest"
  }
}
```

- [ ] **Step 2: Vite config with non-default port + API proxy + Tailwind**

`web/vite.config.ts`:
```ts
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    host: '127.0.0.1',
    port: 5280,
    proxy: { '/api': 'http://127.0.0.1:8380' },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
```

`web/vitest.config.ts`:
```ts
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
  },
})
```

- [ ] **Step 3: Custom dnsaur theme + app css**

`web/src/styles/dnsaur-theme.css` — author a full token block for light and dark. Base it on the rnui token contract (every `--*` from `~/projects/rnui/packages/themes/src/ocean.css`: font-sans, font-heading, radius, background/foreground, card, popover, primary, secondary, muted, accent, destructive, success, info, warning, invert, focus, border, input, ring, chart-1..5, sidebar-*). Use a DNS/network-appropriate hue (a confident indigo-cyan primary). Light block:
```css
[data-theme="dnsaur"] {
  --font-sans: 'Inter', ui-sans-serif, system-ui, sans-serif;
  --font-heading: 'Inter', ui-sans-serif, system-ui, sans-serif;
  --radius: 0.625rem;
  --background: oklch(0.99 0.004 250);
  --foreground: oklch(0.18 0.02 260);
  /* … all remaining tokens; primary ~ oklch(0.52 0.16 265) … */
}
[data-theme="dnsaur"].dark, .dark [data-theme="dnsaur"] {
  --background: oklch(0.18 0.015 260);
  --foreground: oklch(0.95 0.008 250);
  /* … dark values for every token … */
}
```
(The exact OKLch values are finalized in the design-skills pass — Task 14's `colorize`/`polish`. For Task 1, provide a complete, valid, self-consistent token set so the app renders correctly; the design pass tunes it.)

`web/src/styles/app.css`:
```css
@import 'tailwindcss';
@import '@e412/rnui-themes';
@source "../../node_modules/@e412/rnui-react/dist";
@import './dnsaur-theme.css';
```

`web/index.html` sets `<html data-theme="dnsaur">`.

- [ ] **Step 4: Root render + placeholder Home**

`web/src/main.tsx` renders `<App/>` inside `QueryClientProvider` (import from `./lib/query-client` — created in Task 3; for Task 1 use a bare `new QueryClient()`). `web/src/app.tsx` renders `<Home/>`. `web/src/pages/home.tsx` renders an rnui `Card` with "dnsaur" heading — enough to prove the theme + rnui + Tailwind pipeline. `web/src/test/setup.ts` imports `@testing-library/jest-dom`.

- [ ] **Step 5: Verify the app builds and a smoke test passes**

`web/src/app.test.tsx`:
```tsx
import { render, screen } from '@testing-library/react'
import { App } from './app'

test('renders the dnsaur home card', () => {
  render(<App />)
  expect(screen.getByRole('heading', { name: /dnsaur/i })).toBeInTheDocument()
})
```
Run: `pnpm test && pnpm build`. Expected: test passes; `dist/` produced.

- [ ] **Step 6: Go embed + static handler (TDD)**

`web/embed.go` (new, package `web` at repo `web/`):
```go
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist is the built SPA, rooted at the dist directory. Empty when the app
// hasn't been built (backend-only dev) — callers must tolerate that.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return distFS
	}
	return sub
}
```
Ensure a committed `web/dist/.gitkeep` exists so `go:embed all:dist` always has a directory (embed fails on a missing path).

`internal/api/static_test.go`:
```go
package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticHandlerServesAssetsAndFallsBack(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>dnsaur</title>")},
		"assets/app.js":    {Data: []byte("console.log(1)")},
	}
	h := StaticHandler(fsys)

	// existing asset served
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Code != 200 || w.Body.String() != "console.log(1)" {
		t.Fatalf("asset: %d %q", w.Code, w.Body.String())
	}
	// unknown client route → index.html fallback
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") == "" {
		t.Fatalf("fallback: %d", w.Code)
	}
	if got := w.Body.String(); got == "" {
		t.Fatal("fallback body empty")
	}
}

func TestStaticHandlerEmptyFSIs404NotPanic(t *testing.T) {
	h := StaticHandler(fstest.MapFS{}) // empty build
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty fs: %d", w.Code)
	}
	_ = fs.ValidPath("index.html")
}
```

- [ ] **Step 7: Implement static.go**

`internal/api/static.go`:
```go
package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// StaticHandler serves an embedded SPA: real files by path, otherwise the
// index.html fallback so client-side routes reload correctly. An empty or
// index-less FS yields 404 (backend-only build) rather than panicking.
func StaticHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}
		f, err := fsys.Open(clean)
		if err == nil {
			_ = f.Close()
			setCacheHeaders(w, clean)
			fileServer.ServeHTTP(w, r)
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "static error", http.StatusInternalServerError)
			return
		}
		// fallback to index.html
		index, ierr := fsys.Open("index.html")
		if ierr != nil {
			http.NotFound(w, r)
			return
		}
		_ = index.Close()
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/index.html"
		fileServer.ServeHTTP(w, r2)
	})
}

func setCacheHeaders(w http.ResponseWriter, name string) {
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}
```
In `internal/api/server.go` `routes()`: replace the catch-all `/api/` 404 nuance carefully — keep `/api/` returning JSON 404, but register the SPA on `/` (root) so non-API paths hit `StaticHandler`. Wire the real FS in `internal/app/app.go` where the API server is constructed: pass `web.Dist()` into `Deps` (add `Static fs.FS` to `api.Deps`) and mount `StaticHandler(d.Static)` on `/` when `d.Static != nil`. Add `github.com/aloks98/dnsaur/web` import.

- [ ] **Step 8: CI web job**

Add to `.forgejo/workflows/ci.yml` a `web` job on `tiny-no-docker`: setup-node 20, `corepack enable`, `cd web && pnpm install --frozen-lockfile`, `pnpm oxlint src/`, `pnpm format:check`, `pnpm typecheck`, `pnpm test`, `pnpm build`. Make the existing `checks`/`test` Go jobs `needs: web` OR add a step that builds `web/dist` before the Go build so `go:embed` has assets (simplest: the Go `checks` job runs the web build first, or commits keep a built `dist` — prefer building in CI: add web build as a step in the Go job too, or upload/download dist artifact between jobs).

- [ ] **Step 9: Verify + commit**

Run: `cd web && pnpm oxlint src/ && pnpm format:check && pnpm typecheck && pnpm test && pnpm build`; then `cd .. && go test -race ./internal/api/ && ~/go/bin/golangci-lint run ./...`.
```bash
git add web internal/api/static.go internal/api/static_test.go internal/api/server.go internal/app/app.go .forgejo/workflows/ci.yml
git commit -m "feat(web): scaffold spa, custom theme, go embed + static serving"
```

---

### Task 2: Typed API client + types + ApiError

**Files:**
- Create: `web/src/api/types.ts`, `web/src/api/client.ts`, `web/src/api/client.test.ts`

**Interfaces:**
- Produces:
  - `web/src/api/types.ts` — hand-written types mirroring the API. Include (per the OpenAPI doc / handlers): `MeResponse {id:number; username:string; totp_enabled:boolean}`, `SetupState {setup_required:boolean}`, `Group {id:number; name:string; enabled:boolean}`, `Client {id:number; name:string; matcher:string; group_id:number}`, `List {id:number; url:string; kind:'block'|'allow'; enabled:boolean; last_refreshed:number; entry_count:number}`, `Rule {id:number; group_id:number; action:'allow'|'block'; pattern:string; is_regex:boolean}`, `LocalRecord {id:number; name:string; type:'A'|'AAAA'|'CNAME'|'TXT'; value:string; ttl:number}`, `QueryEntry {id:number; at:number; client_ip:string; client_id:number; q_name:string; q_type:string; decision:string; rule_id:number; list_id:number; upstream:string; r_code:string; duration_ms:number}`, `StatsOverview {total:number; blocked:number; cached:number; forwarded:number; clients:number}`, `TimelineBucket {bucket:number; decisions:Record<string,number>}`, `TopEntry {key:string; count:number}`, `ApiToken {id:number; name:string; scope:'read'|'write'; created_at:number; last_used:number}`, `Settings = Record<string,string>`.
  - `web/src/api/client.ts` — `class ApiError extends Error { constructor(public status:number, public body:string){...} }`; `api.get<T>(path)`, `api.post<T>(path, body?)`, `api.put<T>(path, body?)`, `api.patch<T>(path, body?)`, `api.del<T>(path)` — all prefix `/api/v1`, send `credentials:'include'`, set JSON content-type on bodies, parse JSON responses, and throw `ApiError(status, errorMessage)` on non-2xx (errorMessage from `{error}` when present). 204/empty → `undefined`.

- [ ] **Step 1: Write failing client tests**

`web/src/api/client.test.ts`:
```ts
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { api, ApiError } from './client'

function mockFetch(status: number, body: unknown, ct = 'application/json') {
  return vi.fn().mockResolvedValue(
    new Response(body === undefined ? null : JSON.stringify(body), {
      status,
      headers: { 'content-type': ct },
    }),
  )
}

afterEach(() => vi.restoreAllMocks())

test('get returns parsed json and hits /api/v1', async () => {
  const f = mockFetch(200, { id: 1, username: 'admin', totp_enabled: false })
  vi.stubGlobal('fetch', f)
  const me = await api.get<{ username: string }>('/auth/me')
  expect(me.username).toBe('admin')
  const [url, init] = f.mock.calls[0]
  expect(url).toBe('/api/v1/auth/me')
  expect(init.credentials).toBe('include')
})

test('non-2xx throws ApiError with the error message', async () => {
  vi.stubGlobal('fetch', mockFetch(401, { error: 'authentication required' }))
  await expect(api.get('/settings')).rejects.toMatchObject({
    name: 'ApiError',
    status: 401,
    message: 'authentication required',
  })
})

test('post sends json body and 204 yields undefined', async () => {
  const f = mockFetch(204, undefined)
  vi.stubGlobal('fetch', f)
  const r = await api.post('/blocking/pause', { group_id: 0, minutes: 5 })
  expect(r).toBeUndefined()
  const [, init] = f.mock.calls[0]
  expect(init.method).toBe('POST')
  expect(JSON.parse(init.body)).toEqual({ group_id: 0, minutes: 5 })
  expect(init.headers['Content-Type']).toBe('application/json')
})
```

- [ ] **Step 2: Run, verify failure**

Run: `pnpm test src/api/client.test.ts` — Expected: FAIL (module missing).

- [ ] **Step 3: Implement client + types**

`web/src/api/client.ts`:
```ts
const BASE = '/api/v1'

export class ApiError extends Error {
  constructor(public status: number, message: string, public body?: string) {
    super(message)
    this.name = 'ApiError'
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'include' }
  if (body !== undefined) {
    init.body = JSON.stringify(body)
    init.headers = { 'Content-Type': 'application/json' }
  }
  const res = await fetch(BASE + path, init)
  const text = await res.text()
  if (!res.ok) {
    let msg = res.statusText
    try {
      const j = text ? JSON.parse(text) : null
      if (j && typeof j.error === 'string') msg = j.error
    } catch {
      /* non-json error body */
    }
    throw new ApiError(res.status, msg, text)
  }
  if (!text) return undefined as T
  return JSON.parse(text) as T
}

export const api = {
  get: <T>(p: string) => request<T>('GET', p),
  post: <T>(p: string, b?: unknown) => request<T>('POST', p, b),
  put: <T>(p: string, b?: unknown) => request<T>('PUT', p, b),
  patch: <T>(p: string, b?: unknown) => request<T>('PATCH', p, b),
  del: <T>(p: string) => request<T>('DELETE', p),
}
```
Write `web/src/api/types.ts` with all the types from the Interfaces block.

- [ ] **Step 4: Run + lint/format, commit**

Run: `pnpm test src/api/client.test.ts && pnpm oxlint src/ && pnpm format:check && pnpm typecheck`.
```bash
git add web/src/api && git commit -m "feat(web): typed api client with ApiError"
```

---

### Task 3: Query client, MSW test harness, auth hooks

**Files:**
- Create: `web/src/lib/query-client.ts`, `web/src/test/msw-handlers.ts`, `web/src/test/msw-server.ts`, `web/src/test/render.tsx`, `web/src/hooks/use-auth.ts`, `web/src/hooks/use-auth.test.tsx`
- Modify: `web/src/test/setup.ts` (start/stop MSW)

**Interfaces:**
- Consumes: `api` (Task 2).
- Produces:
  - `makeQueryClient(): QueryClient` — retry 1, `staleTime` 10s, mutations don't retry.
  - `web/src/test/render.tsx` — `renderWithProviders(ui)` wrapping in a fresh `QueryClientProvider` + `MemoryRouter`.
  - `web/src/test/msw-server.ts` — a `setupServer(...handlers)` node MSW server; `setup.ts` calls `listen`/`resetHandlers`/`close`.
  - `web/src/test/msw-handlers.ts` — default happy-path handlers for `/api/v1/*` routes used across tests (auth/me, settings, etc.), overridable per-test via `server.use(...)`.
  - `use-auth.ts` hooks: `useMe()` (`useQuery` GET `/auth/me`, retry false so a 401 resolves fast), `useSetupState()` (GET `/setup`), `useLogin()` (`useMutation` POST `/auth/login` → on success invalidate `me`), `useLogout()` (POST `/auth/logout` → clear query cache), `useSetup()` (POST `/setup`). Each returns the standard TanStack shapes.

- [ ] **Step 1: MSW + render harness**

Install already done (Task 1). `web/src/test/msw-handlers.ts` exports `handlers` — an array of `http.get('/api/v1/auth/me', () => HttpResponse.json({id:1,username:'admin',totp_enabled:false}))` and friends for the common endpoints (setup→{setup_required:false}, settings→{}, etc.). `web/src/test/msw-server.ts`: `export const server = setupServer(...handlers)`. `web/src/test/setup.ts` adds:
```ts
import '@testing-library/jest-dom'
import { afterAll, afterEach, beforeAll } from 'vitest'
import { server } from './msw-server'
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
```
`web/src/test/render.tsx`:
```tsx
import { QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { ReactElement } from 'react'
import { makeQueryClient } from '../lib/query-client'

export function renderWithProviders(ui: ReactElement, { route = '/' } = {}) {
  const client = makeQueryClient()
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  )
}
```

- [ ] **Step 2: Write failing auth-hook test**

`web/src/hooks/use-auth.test.tsx`:
```tsx
import { http, HttpResponse } from 'msw'
import { expect, test } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClientProvider } from '@tanstack/react-query'
import { makeQueryClient } from '../lib/query-client'
import { server } from '../test/msw-server'
import { useMe } from './use-auth'

function wrapper() {
  const client = makeQueryClient()
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

test('useMe returns the authenticated user', async () => {
  const { result } = renderHook(() => useMe(), { wrapper: wrapper() })
  await waitFor(() => expect(result.current.isSuccess).toBe(true))
  expect(result.current.data?.username).toBe('admin')
})

test('useMe surfaces a 401 as an error (not a retry storm)', async () => {
  server.use(
    http.get('/api/v1/auth/me', () =>
      HttpResponse.json({ error: 'authentication required' }, { status: 401 }),
    ),
  )
  const { result } = renderHook(() => useMe(), { wrapper: wrapper() })
  await waitFor(() => expect(result.current.isError).toBe(true))
  expect((result.current.error as { status?: number })?.status).toBe(401)
})
```

- [ ] **Step 3: Run, verify failure**

Run: `pnpm test src/hooks/use-auth.test.tsx` — Expected: FAIL.

- [ ] **Step 4: Implement query-client + auth hooks**

`web/src/lib/query-client.ts`:
```ts
import { QueryClient } from '@tanstack/react-query'

export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: 1, staleTime: 10_000, refetchOnWindowFocus: false },
      mutations: { retry: false },
    },
  })
}
```
`web/src/hooks/use-auth.ts`:
```ts
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import type { MeResponse, SetupState } from '../api/types'

export const authKeys = { me: ['auth', 'me'] as const, setup: ['setup'] as const }

export function useMe() {
  return useQuery({ queryKey: authKeys.me, queryFn: () => api.get<MeResponse>('/auth/me'), retry: false })
}

export function useSetupState() {
  return useQuery({ queryKey: authKeys.setup, queryFn: () => api.get<SetupState>('/setup'), retry: false })
}

export function useLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { username: string; password: string; totp_code?: string }) =>
      api.post('/auth/login', v),
    onSuccess: () => qc.invalidateQueries({ queryKey: authKeys.me }),
  })
}

export function useLogout() {
  const qc = useQueryClient()
  return useMutation({ mutationFn: () => api.post('/auth/logout'), onSuccess: () => qc.clear() })
}

export function useSetup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { username: string; password: string }) => api.post('/setup', v),
    onSuccess: () => qc.invalidateQueries(),
  })
}
```

- [ ] **Step 5: Run + lint, commit**

Run: `pnpm test && pnpm oxlint src/ && pnpm format:check && pnpm typecheck`.
```bash
git add web/src/lib web/src/test web/src/hooks && git commit -m "feat(web): query client, msw harness, auth hooks"
```

---

### Task 4: App shell, routing, auth gate, theme toggle

**Files:**
- Create: `web/src/lib/theme.ts`, `web/src/components/app-shell.tsx`, `web/src/components/sidebar-nav.tsx`, `web/src/components/header.tsx`, `web/src/components/theme-toggle.tsx`, `web/src/components/error-boundary.tsx`, `web/src/components/api-unreachable-banner.tsx`, `web/src/components/command-palette.tsx`, `web/src/app.test.tsx` (expand)
- Modify: `web/src/app.tsx` (router + auth gate), `web/src/main.tsx` (providers)

**Interfaces:**
- Consumes: `useMe`, `useSetupState` (Task 3); rnui `Sidebar`, `Command`, `Sonner`/`Toaster`, `Button`, `Switch`.
- Produces:
  - `<App/>` mounting the router. A top-level **AuthGate**: while `useMe` is loading → full-page spinner; on success → the app shell with routed pages; on error → if `useSetupState().data.setup_required` render `<Setup/>` (placeholder ok until Task 5), else `<Login/>` (placeholder ok until Task 6).
  - `AppShell` = rnui `Sidebar` (nav items: Dashboard `/`, Query Log `/queries`, Filtering `/filtering`, Local DNS `/dns`, Settings `/settings`, Account `/account`) + `Header` (global pause button placeholder, `ThemeToggle`, account menu, ⌘K trigger) + `<Outlet/>` wrapped in `ErrorBoundary`.
  - `useTheme()` / `ThemeToggle`: toggles `document.documentElement.classList` `dark`, persists to `localStorage('dnsaur-theme')`, defaults to `matchMedia('(prefers-color-scheme: dark)')`.
  - `CommandPalette` (⌘K): rnui `Command` dialog listing page navigation entries (quick actions wired in later page tasks).

- [ ] **Step 1: Write failing gate tests**

`web/src/app.test.tsx` (replace Task 1's smoke):
```tsx
import { http, HttpResponse } from 'msw'
import { expect, test } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import { server } from './test/msw-server'
import { renderWithProviders } from './test/render'
import { App } from './app'

test('authenticated user sees the app shell nav', async () => {
  renderWithProviders(<App />)
  await waitFor(() => expect(screen.getByRole('link', { name: /query log/i })).toBeInTheDocument())
})

test('unauthenticated + setup-required shows setup', async () => {
  server.use(
    http.get('/api/v1/auth/me', () => HttpResponse.json({ error: 'x' }, { status: 401 })),
    http.get('/api/v1/setup', () => HttpResponse.json({ setup_required: true })),
  )
  renderWithProviders(<App />)
  await waitFor(() => expect(screen.getByText(/set up/i)).toBeInTheDocument())
})

test('unauthenticated + setup-done shows login', async () => {
  server.use(
    http.get('/api/v1/auth/me', () => HttpResponse.json({ error: 'x' }, { status: 401 })),
    http.get('/api/v1/setup', () => HttpResponse.json({ setup_required: false })),
  )
  renderWithProviders(<App />)
  await waitFor(() => expect(screen.getByLabelText(/password/i)).toBeInTheDocument())
})
```

- [ ] **Step 2: Run, verify failure**

Run: `pnpm test src/app.test.tsx` — Expected: FAIL.

- [ ] **Step 3: Implement shell + gate**

Build `theme.ts`, `ThemeToggle`, `ErrorBoundary` (class component rendering rnui `Alert` on error, `componentDidCatch` logs), `SidebarNav` (rnui `Sidebar` with `NavLink`s), `Header`, `CommandPalette`, `AppShell`, and the `App` gate wiring the three branches. Use rnui components; consult `~/projects/rnui/packages/react/src/components/sidebar.tsx` and `command.tsx` for exact props. Placeholder `Setup`/`Login` (a heading "Set up dnsaur" / a form with a password field) are fine here — real ones land in Tasks 5–6. Register routes with React Router v7 `createBrowserRouter` or `<Routes>`; the gate lives above the router or as the root element.

**This task's visual layer (shell layout, sidebar, header) is realized via the design skills** — invoke `frontend-design` for the shell direction and `arrange` for the layout before finalizing. Tests assert structure/roles, not pixels.

- [ ] **Step 4: Run + lint, commit**

Run: `pnpm test && pnpm oxlint src/ && pnpm format:check && pnpm typecheck && pnpm build`.
```bash
git add web/src && git commit -m "feat(web): app shell, routing, auth gate, theme toggle"
```

---

## Page tasks — common shape

Tasks 5–13 each build one page/area. Every page task follows the same
structure, so it is stated once here and each task lists only its specifics:

1. **Data hooks** (full code): resource `useQuery`/`useMutation` hooks in
   `web/src/hooks/use-<resource>.ts`, mutations invalidating the right keys.
2. **MSW interaction tests** (full code): render the page via
   `renderWithProviders`, drive it with `@testing-library/user-event`, assert
   against MSW handlers (default happy path + per-test overrides for
   errors/edge cases). Tests assert **behavior and structure/roles**, never
   pixels.
3. **Page component**: wires hooks + rnui components per the functional
   contract. **The visual layer is realized via the design skills** — invoke
   `frontend-design` once for the page's direction, then `arrange`/`typeset`/
   `polish` as needed. Do not hand-invent layout the design skill will own.
4. **Wire into routing/nav** (route already registered in Task 4; fill the
   real component).
5. **Verify** `pnpm test && pnpm oxlint src/ && pnpm format:check && pnpm typecheck`, **commit**.

Every mutation shows an rnui `sonner` toast on success/failure. Every list
page has a loading skeleton and an rnui `EmptyState`.

---

### Task 5: Setup wizard page

**Files:** Create `web/src/pages/setup.tsx`, `web/src/pages/setup.test.tsx`. Consumes `useSetup` (Task 3), `useSettings`/`useFilters` create hooks (for the starter-lists step — may POST lists + PUT settings). rnui `Stepper`, `Form`, `Input`, `Button`, `InputOTP` (not here), `Card`.

**Specifics:**
- Step 1: admin account — username + password (+ confirm) with a strength hint; `useSetup` on submit; on success the account exists and the app is now authenticated-or-login (invalidate `me`).
- Step 2 (optional, skippable): pick starter upstreams (default `1.1.1.1,1.0.0.1,9.9.9.9`) and toggle a couple of curated starter blocklists (StevenBlack, HaGeZi) — these POST `/filters/lists` + assign to group 1 + `PUT /settings upstreams`. Skippable with a "do this later" link.
- Step 3: done → navigate to `/`.
- **MSW tests:** (a) short password → inline validation error, no POST; (b) happy path POST `/setup` 201 then advances; (c) `409` (already set up) → error toast + suggests login.

**Design:** `frontend-design` for the wizard framing (a focused, centered, welcoming first-run screen), then `polish`.

---

### Task 6: Login page (+ TOTP step)

**Files:** Create `web/src/pages/login.tsx`, `web/src/pages/login.test.tsx`. Consumes `useLogin` (Task 3). rnui `Form`, `Input`, `Button`, `InputOTP`, `Card`.

**Specifics:**
- Username + password → `useLogin`. On `428` (`totp code required`) reveal a second step: 6-digit `InputOTP`, resubmit with `totp_code`. On `401` → inline "invalid credentials". On success → `me` invalidates and the gate swaps to the app.
- **MSW tests:** (a) bad creds → 401 → inline error, stays on login; (b) success → onSuccess fires (assert the login mutation resolved / `me` refetch requested); (c) TOTP: first submit 428 → OTP field appears → second submit with code succeeds.

**Design:** `frontend-design` for a clean centered auth card matching the setup screen; shared auth layout component.

---

### Task 7: Dashboard home page

**Files:** Create `web/src/hooks/use-stats.ts`, `web/src/pages/home.tsx`, `web/src/pages/home.test.tsx`. rnui `StatCard`, `charts` (line/area chart), `Card`, `Table`, `Badge`, window `Select`.

**Data hooks (`use-stats.ts`):** `useStatsOverview(hours)` GET `/stats/overview?hours=`, `useStatsTimeline(hours)` GET `/stats/timeline?hours=`, `useStatsTop(metric, n, hours)` GET `/stats/top?metric=&n=&hours=`. All `useQuery`, `refetchInterval: 30_000` for the live feel.

**Specifics:**
- Stat tiles: total, blocked % (`blocked/total`), cache hit rate (`cached/total`), active clients — from overview. Window `Select` (1h/24h/7d) drives the `hours` param across all three hooks.
- Timeline chart: transform `TimelineBucket[]` (bucket = unix **seconds** → Date) into allowed-vs-blocked series for an rnui area/line chart (see `~/projects/rnui/packages/react/src/components/charts/`).
- Three top tables (domains, blocked_domain, client) via `useStatsTop`; each domain row has a block/allow quick action (consumes the block/allow mutation from Task 9 — if Task 9 not yet done, use a local `useAddRule` stub calling POST `/groups/1/rules` and note the shared hook).
- Health strip: instance up (implied by a successful call), upstreams/lists info from settings + lists last-refreshed.
- **MSW tests:** (a) tiles render computed blocked %; (b) changing the window `Select` refetches with the new `hours`; (c) empty stats → EmptyState, no NaN in percentages.

**Design:** `frontend-design` for the dashboard composition (tile row → chart → top lists), then `arrange` for rhythm and `colorize` for the allowed/blocked chart palette.

---

### Task 8: Query log page (SSE live tail + filters + actions)

**Files:** Create `web/src/api/sse.ts`, `web/src/hooks/use-queries.ts`, `web/src/pages/queries.tsx`, `web/src/api/sse.test.ts`, `web/src/pages/queries.test.tsx`. rnui `data-grid`, `filters`, `Badge`, `Drawer`, `Button`, `Switch`.

**Interfaces / data:**
- `web/src/api/sse.ts`: `subscribeQueries(onEntry: (e: QueryEntry) => void, onState: (s:'open'|'reconnecting'|'closed')=>void): () => void` — opens `EventSource('/api/v1/queries/tail')`, parses each `data:` line as `QueryEntry`, auto-reconnects with capped backoff, returns an unsubscribe that closes the stream. (EventSource sends the cookie automatically; no auth header needed.)
- `use-queries.ts`: `useLiveTail(enabled)` — manages an in-memory ring buffer (cap ~500) fed by `subscribeQueries`, exposes `{entries, state}`; pausing stops appending without closing rendered rows. `useQuerySearch(filter)` — `useQuery` GET `/queries?…` for the filtered/paged mode. `useSearchQueries` maps a filter object to query params (from, to, client, q, decision, type, limit, offset).

**Specifics:**
- Default view: live tail streaming into a virtualized `data-grid` (time, client, domain, type, decision badge, upstream, latency), newest on top, with a **pause/play** `Switch`. A state indicator (streaming / reconnecting / paused).
- Applying any filter (rnui `filters`) or a domain search switches to paged `useQuerySearch` (stops the stream); clearing filters returns to live tail.
- Per-row: block / allow buttons (POST `/groups/1/rules`), and a "why?" that opens a `Drawer` showing the matched rule/list (from `rule_id`/`list_id`, resolved against the rules/lists queries).
- **sse.test.ts:** mock `EventSource` (a small fake dispatching `message` events); assert `onEntry` fires with a parsed entry and reconnect is attempted after an `error`. **queries.test.tsx:** (a) live rows render as the mocked stream emits; (b) pause stops new rows appending but keeps existing; (c) applying a decision filter calls GET `/queries?decision=blocked` (MSW asserts) and renders those; (d) block action POSTs a rule and toasts.

**Design:** `frontend-design` for the log's density and the decision-badge system, then `animate` for the subtle new-row insertion and `polish`. This is the showpiece — spend the design budget here.

---

### Task 9: Filtering — Lists tab

**Files:** Create `web/src/hooks/use-filters.ts`, `web/src/hooks/use-blocking.ts`, `web/src/pages/filtering/lists.tsx`, `web/src/pages/filtering/index.tsx` (tab container), `web/src/pages/filtering/lists.test.tsx`, `web/src/components/pause-control.tsx`, `web/src/components/pause-control.test.tsx`. Modify `web/src/components/header.tsx` (replace the Task 4 pause placeholder with the real control). rnui `Tabs`, `Table`/`data-grid`, `Dialog`, `Form`, `Switch`, `Badge`, `Button`, `dropdown-menu`.

**Data hooks (`use-filters.ts`):** `useLists()` GET `/filters/lists`; `useAddList()` POST `/filters/lists`; `useToggleList()` PATCH `/filters/lists/{id}`; `useDeleteList()` DELETE `/filters/lists/{id}`; `useRefreshFilters()` POST `/filters/refresh` (202); `useRules(groupId)`, `useAddRule()`, `useDeleteRule()` (used in Task 10 + Tasks 7/8's block-actions — define all here so the block/allow quick actions have a shared hook). Mutations invalidate `['filters','lists']` / `['filters','rules',groupId]`.

**Blocking hooks (`use-blocking.ts`):** `useBlockingStatus(groupId=0)` GET `/blocking?group_id=` (→ `{paused_until:number}`, poll `refetchInterval: 30_000`); `usePauseBlocking()` POST `/blocking/pause` `{group_id, minutes}`; `useResumeBlocking()` DELETE `/blocking/pause?group_id=`. Invalidate `['blocking', groupId]`.

**`pause-control.tsx`:** a `dropdown-menu`/`Button` that shows the current state (paused-until countdown from `useBlockingStatus`, or "blocking active"), with quick actions Pause 5m / 30m / 60m and Resume. Lives in the shell `Header` (global, group 0) and is reused per-group on the Groups & Clients tab (Task 10).

**Specifics:**
- Tab container (`Tabs`: Lists / Rules / Groups & Clients) — Lists tab here, others in Task 10.
- Lists table: url, kind badge, enabled `Switch`, entry_count, last_refreshed (relative time); add-list `Dialog` (url + kind, url must be http/https — mirror API validation for a friendly message); delete with confirm; a header "Refresh now" button → `useRefreshFilters` → progress toast.
- **MSW tests:** (a) add list posts + list refetches + row appears; (b) toggle enabled PATCHes; (c) bad url → inline validation, no POST; (d) refresh → 202 → toast. **pause-control.test.tsx:** (e) "Pause 5m" POSTs `{group_id:0, minutes:5}` and shows a countdown; (f) "Resume" DELETEs and returns to active.

**Design:** `frontend-design` for the filtering area's tab layout + the pause control's state affordance; shared table styling.

---

### Task 10: Filtering — Rules + Groups & Clients tabs

**Files:** Create `web/src/hooks/use-clients.ts`, `web/src/hooks/use-groups.ts`, `web/src/pages/filtering/rules.tsx`, `web/src/pages/filtering/groups-clients.tsx`, plus `.test.tsx` for each. Consumes `use-filters.ts` rule hooks (Task 9). rnui `Table`, `Dialog`, `Form`, `Select`, `Switch`, `Badge`, `combobox`/`multi-select` for list assignment.

**Data hooks:** `use-groups.ts`: `useGroups()` GET `/groups`, `useAddGroup()`, `useRenameGroup()`+`useToggleGroup()` PATCH `/groups/{id}`, `useDeleteGroup()` DELETE (409 → "in use" toast); `useGroupLists(id)` GET `/groups/{id}/lists`, `useSetGroupLists()` PUT `/groups/{id}/lists`. `use-clients.ts`: `useClients()` GET `/clients`, `useAddClient()`, `useUpdateClient()`, `useDeleteClient()` — matcher validated as IP/CIDR client-side for a friendly message.

**Specifics:**
- Rules tab: group `Select` → rules table for that group (action badge, pattern, regex flag); add-rule `Dialog` (action, pattern, is_regex; regex compiled client-side via `new RegExp` to catch errors early, pattern length ≤ 512 to match the API); delete.
- Groups & Clients tab: groups list with enable toggle + rename + delete (default group id 1 undeletable → disabled control); clients table (name, matcher, group) with add/edit/delete; a per-group "lists applied" multi-select → `useSetGroupLists`.
- **MSW tests:** (a) add rule with invalid regex → inline error, no POST; (b) delete default group control disabled/blocked; (c) client with bad matcher → inline error; (d) set group lists PUTs the id array.

**Design:** `frontend-design` + `arrange` for the two-panel groups/clients relationship.

---

### Task 11: Local DNS records page

**Files:** Create `web/src/hooks/use-records.ts`, `web/src/pages/dns.tsx`, `web/src/pages/dns.test.tsx`. rnui `Table`/`data-grid`, `Sheet`, `Form`, `Select`, `Input`, `Button`.

**Data hooks (`use-records.ts`):** `useRecords()` GET `/records`; `useAddRecord()` POST; `useUpdateRecord()` PUT `/records/{id}`; `useDeleteRecord()` DELETE. Invalidate `['records']`.

**Specifics:**
- Records table (name, type badge, value, ttl); add/edit via a `Sheet` form with a type `Select` (A/AAAA/CNAME/TXT) and **type-aware value validation mirroring the API** (A → IPv4, AAAA → IPv6, CNAME → domain, TXT → non-empty; name a domain with optional `*.` wildcard; ttl 1–86400) so a record accepted here is one the API/DNS layer accepts.
- **MSW tests:** (a) A record with an IPv6 value → inline error, no POST; (b) valid add posts + row appears; (c) edit PUTs with the path id; (d) delete removes the row.

**Design:** `frontend-design` for the records table + side-sheet editing pattern (reused mental model for other CRUD pages).

---

### Task 12: Settings page

**Files:** Create `web/src/hooks/use-settings.ts`, `web/src/pages/settings.tsx`, `web/src/pages/settings.test.tsx`. rnui `Form`, `Input`, `Select`, `Card`, `Switch`, `Badge`, `NumberField`.

**Data hooks (`use-settings.ts`):** `useSettings()` GET `/settings` (returns `Record<string,string>`); `useUpdateSetting()` PUT `/settings` `{key,value}` invalidating `['settings']` (and, since settings hot-reload server-side, nothing else client-side needed).

**Specifics:**
- Grouped `Card` sections: **Upstreams** (`upstreams` comma list + `upstream.strategy` select failover/fastest/race), **Blocking** (`blocking.mode` null-ip/nxdomain, `blocking.ttl`), **Cache** (`cache.*` — each labeled **Restart required**), **Query log** (`qlog.privacy` full/anon/none, `qlog.retention_days`), **Lists** (`lists.refresh_hours` — labeled restart-required), **Storage** (read-only info). Each field validates against the same allowlist rules the API enforces (enum membership, non-negative ints) for immediate feedback; save issues `PUT /settings` per changed key.
- **MSW tests:** (a) invalid `blocking.mode` value blocked client-side; (b) valid change PUTs `{key,value}` and toasts; (c) restart-required fields show the label.

**Design:** `frontend-design` for the settings form grouping; `typeset` for label/help hierarchy.

---

### Task 13: Account & security page

**Files:** Create `web/src/hooks/use-tokens.ts`, `web/src/hooks/use-totp.ts`, `web/src/pages/account.tsx`, `web/src/pages/account.test.tsx`. rnui `Card`, `Form`, `Dialog`, `Table`, `Badge`, `InputOTP`, `Button`, `copy-button`; a QR component (use a tiny QR lib, e.g. `qrcode` rendered to a canvas/img, or render the `otpauth_url` as text + QR).

**Data hooks:** `use-tokens.ts`: `useTokens()` GET `/tokens`, `useCreateToken()` POST `/tokens` (returns `{id, token}` — the plain token, shown once), `useRevokeToken()` DELETE `/tokens/{id}`. `use-totp.ts`: `useTotpStart()` POST `/auth/totp/start` (→ `{secret, otpauth_url}`), `useTotpConfirm()` POST `/auth/totp/confirm` `{secret,code}`, `useTotpDisable()` POST `/auth/totp/disable` `{code}`.

**Specifics:**
- Change password (if the API exposes it; otherwise omit and note — the current API has no change-password endpoint, so **this milestone shows password change as "not yet available" or is dropped**; verify against `openapi.yaml` and implement only what exists).
- TOTP: if `me.totp_enabled` show a Disable flow (enter current code → `useTotpDisable`); else an Enable flow — `useTotpStart` → show QR of `otpauth_url` + the secret → user enters a code → `useTotpConfirm` → success toast + `me` invalidates.
- API tokens: table (name, scope badge, created, last used); "New token" `Dialog` (name + read/write `Select`) → on create show the plain token **once** in a copy dialog (`copy-button`), warn it won't be shown again; revoke with confirm.
- **MSW tests:** (a) create token shows the plain token exactly once; (b) revoke DELETEs + row disappears; (c) TOTP enable: start → confirm with code → me refetches; (d) token_hash never appears in the rendered table (the API omits it — assert it's not present).

**Design:** `frontend-design` for the security page; the one-time-token reveal dialog gets `polish` (it's a high-stakes moment).

---

### Task 14: End-to-end serving, design polish gate, Playwright smoke, docs

**Files:** Modify `internal/api/app` wiring if needed; create `web/e2e/smoke.spec.ts` (Playwright), `web/playwright.config.ts`; update `.forgejo/workflows/ci.yml`; update `README.md`, `docs/architecture.md`, `docs/configuration.md`; ensure `web/dist` builds into the Go binary.

**Specifics:**
- **Full serving path:** build `web/dist`, build the Go binary embedding it, run it, confirm `GET /` serves the SPA and `GET /settings` (client route) returns `index.html`, while `/api/v1/health` still returns JSON. A Go test in `internal/app` (or extend the API e2e) asserting the embedded index is served at `/` and a deep link falls back.
- **Design polish gate (design skills):** run `critique` on the built dashboard and `audit` (accessibility + performance) across the pages; fix P0/P1 findings. This is where the dnsaur theme's OKLch values are finalized (`colorize`/`polish`) and motion is tuned (`animate`). Capture before/after where useful.
- **Playwright smoke:** one spec against the running Go binary (built with the real SPA) on a non-default port: load `/` → setup (create admin) → login → add a local record → see it in the list → toggle dark mode persists. `playwright.config.ts` starts the binary as its `webServer` (or documents launching it). Runs in CI where a browser is available; else documented as a local gate (the `tiny-no-docker`/`medium` runners may lack browsers — if so, gate it behind a `medium` job with a browser install, or mark it local-only in `web/README.md`).
- **Docs:** `web/README.md` (dev workflow: `pnpm dev` on 5280 + `DNSAUR_HTTP_LISTEN=127.0.0.1:8380 dnsaur`), README status row "Web dashboard | Shipped", architecture.md package map (+`web/`), configuration.md note that the dashboard is served on `HTTPListen`.
- **Verify:** full `pnpm test && pnpm build` in web; `go test -race ./... && ~/go/bin/golangci-lint run ./...`; the Playwright smoke where runnable.
- **Commit:** `feat(web): end-to-end serving, playwright smoke, docs; design polish pass`.
